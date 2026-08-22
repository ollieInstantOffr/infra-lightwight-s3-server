package s3api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// S3 clients branch on the error Code, not on the HTTP status or the message.
// Getting a code wrong makes SDK retry logic misbehave in ways that look like
// network flakiness: the AWS SDKs retry on some codes, refuse to retry on
// others, and re-fetch credentials on a third set. A generic 500 for everything
// therefore produces far worse client behaviour than a correct 404.

// APIError is an S3 protocol error: a code, a human-readable message, and the
// status to return with them.
type APIError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s (%d): %s", e.Code, e.HTTPStatus, e.Message)
}

// The error set this server can actually produce. Messages match AWS's wording
// where it exists, because operators search for these strings.
var (
	ErrNoSuchBucket = &APIError{
		Code:       "NoSuchBucket",
		Message:    "The specified bucket does not exist.",
		HTTPStatus: http.StatusNotFound,
	}
	ErrNoSuchKey = &APIError{
		Code:       "NoSuchKey",
		Message:    "The specified key does not exist.",
		HTTPStatus: http.StatusNotFound,
	}
	ErrNoSuchUpload = &APIError{
		Code:       "NoSuchUpload",
		Message:    "The specified multipart upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.",
		HTTPStatus: http.StatusNotFound,
	}
	ErrBucketAlreadyOwnedByYou = &APIError{
		Code:       "BucketAlreadyOwnedByYou",
		Message:    "Your previous request to create the named bucket succeeded and you already own it.",
		HTTPStatus: http.StatusConflict,
	}
	ErrBucketAlreadyExists = &APIError{
		Code:       "BucketAlreadyExists",
		Message:    "The requested bucket name is not available.",
		HTTPStatus: http.StatusConflict,
	}
	ErrBucketNotEmpty = &APIError{
		Code:       "BucketNotEmpty",
		Message:    "The bucket you tried to delete is not empty.",
		HTTPStatus: http.StatusConflict,
	}
	ErrInvalidBucketName = &APIError{
		Code:       "InvalidBucketName",
		Message:    "The specified bucket is not valid.",
		HTTPStatus: http.StatusBadRequest,
	}
	ErrAccessDenied = &APIError{
		Code:       "AccessDenied",
		Message:    "Access Denied.",
		HTTPStatus: http.StatusForbidden,
	}
	// SignatureDoesNotMatch and InvalidAccessKeyId are deliberately distinct
	// from AccessDenied: the SDKs treat the latter as a reason to refresh
	// credentials, which is the right behaviour for an expired role but not for
	// a mis-signed request.
	ErrSignatureDoesNotMatch = &APIError{
		Code:       "SignatureDoesNotMatch",
		Message:    "The request signature we calculated does not match the signature you provided. Check your key and signing method.",
		HTTPStatus: http.StatusForbidden,
	}
	ErrInvalidAccessKeyIDCode = &APIError{
		Code:       "InvalidAccessKeyId",
		Message:    "The AWS Access Key Id you provided does not exist in our records.",
		HTTPStatus: http.StatusForbidden,
	}
	ErrRequestTimeTooSkewed = &APIError{
		Code:       "RequestTimeTooSkewed",
		Message:    "The difference between the request time and the current time is too large.",
		HTTPStatus: http.StatusForbidden,
	}
	ErrMissingSecurityHeader = &APIError{
		Code:       "MissingSecurityHeader",
		Message:    "Your request is missing a required header.",
		HTTPStatus: http.StatusBadRequest,
	}
	ErrEntityTooLarge = &APIError{
		Code:       "EntityTooLarge",
		Message:    "Your proposed upload exceeds the maximum allowed object size.",
		HTTPStatus: http.StatusBadRequest,
	}
	ErrEntityTooSmall = &APIError{
		Code:       "EntityTooSmall",
		Message:    "Your proposed upload is smaller than the minimum allowed object size.",
		HTTPStatus: http.StatusBadRequest,
	}
	ErrInvalidPart = &APIError{
		Code:       "InvalidPart",
		Message:    "One or more of the specified parts could not be found. The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.",
		HTTPStatus: http.StatusBadRequest,
	}
	ErrInvalidPartOrder = &APIError{
		Code:       "InvalidPartOrder",
		Message:    "The list of parts was not in ascending order. Parts must be ordered by part number.",
		HTTPStatus: http.StatusBadRequest,
	}
	ErrInvalidArgument = &APIError{
		Code:       "InvalidArgument",
		Message:    "Invalid Argument.",
		HTTPStatus: http.StatusBadRequest,
	}
	ErrInvalidDigest = &APIError{
		Code:       "InvalidDigest",
		Message:    "The Content-MD5 or checksum value you specified is not valid.",
		HTTPStatus: http.StatusBadRequest,
	}
	ErrBadDigest = &APIError{
		Code:       "BadDigest",
		Message:    "The Content-MD5 or checksum value you specified did not match what the server received.",
		HTTPStatus: http.StatusBadRequest,
	}
	ErrInvalidRange = &APIError{
		Code:       "InvalidRange",
		Message:    "The requested range is not satisfiable.",
		HTTPStatus: http.StatusRequestedRangeNotSatisfiable,
	}
	ErrPreconditionFailed = &APIError{
		Code:       "PreconditionFailed",
		Message:    "At least one of the preconditions you specified did not hold.",
		HTTPStatus: http.StatusPreconditionFailed,
	}
	ErrMethodNotAllowed = &APIError{
		Code:       "MethodNotAllowed",
		Message:    "The specified method is not allowed against this resource.",
		HTTPStatus: http.StatusMethodNotAllowed,
	}
	ErrNotImplemented = &APIError{
		Code:       "NotImplemented",
		Message:    "A header or parameter you provided implies functionality that is not implemented.",
		HTTPStatus: http.StatusNotImplemented,
	}
	ErrInternalError = &APIError{
		Code:       "InternalError",
		Message:    "We encountered an internal error. Please try again.",
		HTTPStatus: http.StatusInternalServerError,
	}
	ErrMalformedXML = &APIError{
		Code:       "MalformedXML",
		Message:    "The XML you provided was not well-formed or did not validate against our published schema.",
		HTTPStatus: http.StatusBadRequest,
	}
	ErrIncompleteBody = &APIError{
		Code:       "IncompleteBody",
		Message:    "You did not provide the number of bytes specified by the Content-Length HTTP header.",
		HTTPStatus: http.StatusBadRequest,
	}
	ErrKeyTooLong = &APIError{
		Code:       "KeyTooLongError",
		Message:    "Your key is too long.",
		HTTPStatus: http.StatusBadRequest,
	}
)

// WithMessage returns a copy carrying a more specific message. The code and
// status are preserved, so client branching is unaffected while the operator
// gets something actionable.
func (e *APIError) WithMessage(format string, args ...any) *APIError {
	return &APIError{
		Code:       e.Code,
		Message:    fmt.Sprintf(format, args...),
		HTTPStatus: e.HTTPStatus,
	}
}

// errorResponse is the XML envelope S3 clients parse.
type errorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId"`
}

// WriteError renders err as an S3 error response.
//
// A HEAD response carries no body, so clients rely entirely on the status code
// and headers. The code is echoed in a header for exactly that reason — without
// it, a HEAD failure is indistinguishable from any other 404.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	apiErr := AsAPIError(err)
	requestID := RequestIDFrom(r.Context())

	w.Header().Set("x-amz-request-id", requestID)
	w.Header().Set("x-amz-error-code", apiErr.Code)
	w.Header().Set("Content-Type", "application/xml")

	if r.Method == http.MethodHead {
		w.WriteHeader(apiErr.HTTPStatus)
		return
	}

	body, marshalErr := xml.Marshal(errorResponse{
		Code:      apiErr.Code,
		Message:   apiErr.Message,
		Resource:  r.URL.Path,
		RequestID: requestID,
	})
	if marshalErr != nil {
		// Falling back to a bare status is better than emitting XML the client
		// cannot parse, which would surface as a confusing parse error rather
		// than the actual problem.
		w.WriteHeader(apiErr.HTTPStatus)
		return
	}

	w.WriteHeader(apiErr.HTTPStatus)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}

// AsAPIError maps any error to the S3 error a client should see.
//
// Anything unrecognised becomes InternalError, so an unexpected failure leaks
// an implementation detail into a log rather than into a response body.
func AsAPIError(err error) *APIError {
	if err == nil {
		return ErrInternalError
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}

	switch {
	case errors.Is(err, ErrMissingAuthorization):
		return ErrAccessDenied.WithMessage("The request is not signed. This server requires AWS Signature Version 4.")
	case errors.Is(err, ErrMalformedAuthorization),
		errors.Is(err, ErrMissingDate),
		errors.Is(err, ErrMissingSignedHeader):
		return ErrMissingSecurityHeader.WithMessage("%s", err.Error())
	case errors.Is(err, ErrUnsupportedAlgorithm):
		return ErrInvalidArgument.WithMessage("%s", err.Error())
	// A revoked key and a fictional one are indistinguishable to the client, so
	// revocation cannot be used to probe which keys once existed.
	case errors.Is(err, ErrInvalidAccessKeyID):
		return ErrInvalidAccessKeyIDCode
	case errors.Is(err, ErrRequestExpired):
		return ErrRequestTimeTooSkewed
	case errors.Is(err, ErrSignatureMismatch), errors.Is(err, ErrChunkSignature):
		return ErrSignatureDoesNotMatch
	case errors.Is(err, ErrContentHashMismatch):
		return ErrBadDigest.WithMessage("%s", err.Error())
	case errors.Is(err, ErrMalformedChunk):
		return ErrIncompleteBody.WithMessage("%s", err.Error())
	}
	return ErrInternalError
}

// requestIDKey is the context key for the per-request identifier.
type requestIDKey struct{}

// NewRequestID returns an AWS-shaped request identifier: 16 uppercase hex
// characters. It appears on every response and in every log line, which is what
// makes a client-reported failure traceable to a server-side log entry.
func NewRequestID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// A degraded identifier is far better than failing the request; the
		// only consequence is that this one line is harder to correlate.
		return "00000000FFFFFFFF"
	}
	return strings.ToUpper(hex.EncodeToString(buf))
}

// RequestIDFrom returns the request id stored in ctx, or a placeholder if the
// middleware did not run.
func RequestIDFrom(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey{}).(string); ok {
		return id
	}
	return "-"
}
