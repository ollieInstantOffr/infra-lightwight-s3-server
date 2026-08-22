package s3api

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// parseErrorBody decodes a response the way an SDK would.
func parseErrorBody(t *testing.T, body string) errorResponse {
	t.Helper()
	var parsed errorResponse
	if err := xml.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("SDKs would fail to parse this error body: %v\nbody: %s", err, body)
	}
	return parsed
}

func serveError(t *testing.T, method string, err error) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	handler := WithRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, err)
	}))
	handler.ServeHTTP(w, httptest.NewRequest(method, "/bucket/key.txt", nil))
	return w
}

func TestWriteErrorProducesParseableXML(t *testing.T) {
	w := serveError(t, http.MethodGet, ErrNoSuchKey)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/xml" {
		t.Errorf("Content-Type = %q, want application/xml", ct)
	}
	if !strings.HasPrefix(w.Body.String(), xml.Header) {
		t.Error("body does not start with an XML declaration")
	}

	parsed := parseErrorBody(t, w.Body.String())
	if parsed.Code != "NoSuchKey" {
		t.Errorf("Code = %q, want NoSuchKey", parsed.Code)
	}
	if parsed.Resource != "/bucket/key.txt" {
		t.Errorf("Resource = %q, want /bucket/key.txt", parsed.Resource)
	}
	if parsed.RequestID == "" || parsed.RequestID == "-" {
		t.Errorf("RequestId = %q, want a generated id", parsed.RequestID)
	}
	if parsed.RequestID != w.Header().Get("x-amz-request-id") {
		t.Errorf("RequestId in body (%s) differs from the header (%s); they must match for a report to be traceable",
			parsed.RequestID, w.Header().Get("x-amz-request-id"))
	}
}

// A HEAD response must carry no body, so the error code has to survive in a
// header or the client learns nothing beyond the status.
func TestWriteErrorOmitsBodyForHEAD(t *testing.T) {
	w := serveError(t, http.MethodHead, ErrNoSuchKey)

	if w.Body.Len() != 0 {
		t.Errorf("HEAD error response has a %d byte body, want none", w.Body.Len())
	}
	if got := w.Header().Get("x-amz-error-code"); got != "NoSuchKey" {
		t.Errorf("x-amz-error-code = %q, want NoSuchKey", got)
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// Every error the server can produce must carry the status AWS uses, since SDK
// retry behaviour keys off both code and status.
func TestErrorStatusCodes(t *testing.T) {
	cases := []struct {
		err    *APIError
		status int
	}{
		{ErrNoSuchBucket, http.StatusNotFound},
		{ErrNoSuchKey, http.StatusNotFound},
		{ErrNoSuchUpload, http.StatusNotFound},
		{ErrBucketAlreadyOwnedByYou, http.StatusConflict},
		{ErrBucketNotEmpty, http.StatusConflict},
		{ErrInvalidBucketName, http.StatusBadRequest},
		{ErrAccessDenied, http.StatusForbidden},
		{ErrSignatureDoesNotMatch, http.StatusForbidden},
		{ErrInvalidAccessKeyIDCode, http.StatusForbidden},
		{ErrRequestTimeTooSkewed, http.StatusForbidden},
		{ErrEntityTooLarge, http.StatusBadRequest},
		{ErrInvalidPart, http.StatusBadRequest},
		{ErrInvalidRange, http.StatusRequestedRangeNotSatisfiable},
		{ErrPreconditionFailed, http.StatusPreconditionFailed},
		{ErrNotImplemented, http.StatusNotImplemented},
		{ErrInternalError, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.err.Code, func(t *testing.T) {
			if tc.err.HTTPStatus != tc.status {
				t.Errorf("%s status = %d, want %d", tc.err.Code, tc.err.HTTPStatus, tc.status)
			}
		})
	}
}

// The mapping from internal failures to client-visible codes is what makes SDK
// behaviour correct, and what decides how much a prober can learn.
func TestAsAPIErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"unsigned request", ErrMissingAuthorization, "AccessDenied"},
		{"malformed header", ErrMalformedAuthorization, "MissingSecurityHeader"},
		{"unknown key", fmt.Errorf("%w: AKIA1", ErrInvalidAccessKeyID), "InvalidAccessKeyId"},
		{"bad signature", ErrSignatureMismatch, "SignatureDoesNotMatch"},
		{"bad chunk signature", ErrChunkSignature, "SignatureDoesNotMatch"},
		{"clock skew", fmt.Errorf("%w: too old", ErrRequestExpired), "RequestTimeTooSkewed"},
		{"content hash mismatch", ErrContentHashMismatch, "BadDigest"},
		{"malformed chunk", ErrMalformedChunk, "IncompleteBody"},
		{"already an APIError", ErrBucketNotEmpty, "BucketNotEmpty"},
		{"wrapped APIError", fmt.Errorf("context: %w", ErrNoSuchKey), "NoSuchKey"},
		{"unrecognised", errors.New("something went wrong internally"), "InternalError"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AsAPIError(tc.err).Code; got != tc.want {
				t.Errorf("AsAPIError(%v).Code = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

// An unexpected internal failure must never put its message in the response,
// where it could disclose paths, queries or configuration.
func TestInternalErrorsDoNotLeakDetail(t *testing.T) {
	leaky := errors.New("pq: relation \"objects\" does not exist at /var/lib/postgresql")
	w := serveError(t, http.MethodGet, leaky)

	body := w.Body.String()
	if strings.Contains(body, "postgresql") || strings.Contains(body, "relation") {
		t.Errorf("internal error detail leaked into the response body:\n%s", body)
	}
	if parsed := parseErrorBody(t, body); parsed.Code != "InternalError" {
		t.Errorf("Code = %s, want InternalError", parsed.Code)
	}
}

// A revoked credential must be indistinguishable from one that never existed,
// or revocation becomes a way to enumerate which keys were once valid.
func TestRevokedAndUnknownKeysLookIdentical(t *testing.T) {
	unknown := AsAPIError(fmt.Errorf("%w: AKIANEVEREXISTED", ErrInvalidAccessKeyID))
	revoked := AsAPIError(fmt.Errorf("%w: AKIAWASREVOKED", ErrInvalidAccessKeyID))

	if unknown.Code != revoked.Code || unknown.Message != revoked.Message ||
		unknown.HTTPStatus != revoked.HTTPStatus {
		t.Error("a revoked key produces a different response from an unknown one")
	}
}

func TestWithMessagePreservesCodeAndStatus(t *testing.T) {
	specific := ErrInvalidBucketName.WithMessage("Bucket name %q is too short.", "ab")

	if specific.Code != ErrInvalidBucketName.Code {
		t.Errorf("Code = %s, want %s", specific.Code, ErrInvalidBucketName.Code)
	}
	if specific.HTTPStatus != ErrInvalidBucketName.HTTPStatus {
		t.Errorf("HTTPStatus = %d, want %d", specific.HTTPStatus, ErrInvalidBucketName.HTTPStatus)
	}
	if !strings.Contains(specific.Message, "too short") {
		t.Errorf("Message = %q, want the specific wording", specific.Message)
	}
	// The shared value must not have been mutated.
	if strings.Contains(ErrInvalidBucketName.Message, "too short") {
		t.Error("WithMessage mutated the shared error value")
	}
}

func TestRequestIDsAreUniqueAndWellShaped(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		id := NewRequestID()
		if len(id) != 16 {
			t.Fatalf("request id %q has length %d, want 16", id, len(id))
		}
		if id != strings.ToUpper(id) {
			t.Fatalf("request id %q is not uppercase", id)
		}
		if seen[id] {
			t.Fatalf("duplicate request id: %s", id)
		}
		seen[id] = true
	}
}

func TestRequestIDMiddlewareSetsHeaderAndContext(t *testing.T) {
	var fromContext string
	handler := WithRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fromContext = RequestIDFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	header := w.Header().Get("x-amz-request-id")
	if header == "" {
		t.Fatal("x-amz-request-id header was not set")
	}
	if fromContext != header {
		t.Errorf("context id %q differs from header %q", fromContext, header)
	}
}
