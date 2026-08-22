// Package s3api implements the S3 wire protocol: request authentication,
// routing and the XML dialect AWS clients expect.
//
// Signature Version 4 is the load-bearing piece. If the canonical request this
// server builds differs from the one the client built by a single byte, the
// signatures disagree and every SDK call fails with an error that says nothing
// about which byte. Each construction step below therefore mirrors the AWS
// specification exactly rather than approximately.
package s3api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Signature verification failures. They are distinct so logs can say what
// actually happened; what the client is told is decided in the error mapping,
// where several of these deliberately collapse to the same S3 code.
var (
	ErrMissingAuthorization   = errors.New("request is not signed")
	ErrMalformedAuthorization = errors.New("authorization header is malformed")
	ErrUnsupportedAlgorithm   = errors.New("unsupported signing algorithm")
	ErrInvalidAccessKeyID     = errors.New("unknown access key id")
	ErrSignatureMismatch      = errors.New("signature does not match")
	ErrRequestExpired         = errors.New("request timestamp is outside the permitted window")
	ErrMissingDate            = errors.New("request has no usable date")
	ErrMissingSignedHeader    = errors.New("a required header was not signed")
	ErrContentHashMismatch    = errors.New("body does not match x-amz-content-sha256")
)

const (
	// Algorithm is the only signing algorithm S3 uses.
	Algorithm       = "AWS4-HMAC-SHA256"
	service         = "s3"
	scopeTerminator = "aws4_request"

	// iso8601 is the x-amz-date format: 20060102T150405Z.
	iso8601  = "20060102T150405Z"
	dateOnly = "20060102"

	// maxClockSkew matches AWS. A request signed further than this from the
	// server's clock is rejected, which is what stops a captured signature
	// being replayed indefinitely.
	maxClockSkew = 15 * time.Minute
)

// Payload hash sentinels a client may send in x-amz-content-sha256 instead of a
// real digest. Each changes how the body must be read, so they are matched
// explicitly rather than treated as opaque.
const (
	// UnsignedPayload: the body is not covered by the signature at all.
	UnsignedPayload = "UNSIGNED-PAYLOAD"
	// StreamingSigned: aws-chunked framing where each chunk carries its own
	// signature, chained from the seed signature of the request itself.
	StreamingSigned = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	// StreamingSignedTrailer: as above, with trailing headers after the final
	// chunk (typically a checksum).
	StreamingSignedTrailer = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	// StreamingUnsignedTrailer: aws-chunked framing with unsigned chunks and
	// trailing headers. This is what the current AWS SDKs send by default over
	// HTTPS, so support for it is not optional.
	StreamingUnsignedTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"

	// emptyStringSHA256 is the hash of zero bytes, sent for bodyless requests.
	emptyStringSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// IsStreaming reports whether a payload hash sentinel means the body arrives in
// aws-chunked framing and must be decoded rather than read directly.
func IsStreaming(payloadHash string) bool {
	switch payloadHash {
	case StreamingSigned, StreamingSignedTrailer, StreamingUnsignedTrailer:
		return true
	}
	return false
}

// Scope is the credential scope: date, region, service, terminator.
type Scope struct {
	Date    string // yyyymmdd
	Region  string
	Service string
}

func (s Scope) String() string {
	return strings.Join([]string{s.Date, s.Region, s.Service, scopeTerminator}, "/")
}

// AuthHeader is a parsed Authorization header.
type AuthHeader struct {
	AccessKeyID   string
	Scope         Scope
	SignedHeaders []string
	Signature     string
}

// ParseAuthorization parses the Authorization header.
//
// The format is:
//
//	AWS4-HMAC-SHA256 Credential=AKIA.../20260822/us-east-1/s3/aws4_request,
//	SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=abc123
//
// Whitespace after the commas is optional in practice, so it is tolerated.
func ParseAuthorization(header string) (*AuthHeader, error) {
	algorithm, rest, found := strings.Cut(header, " ")
	if !found {
		return nil, ErrMalformedAuthorization
	}
	if algorithm != Algorithm {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedAlgorithm, algorithm)
	}

	var auth AuthHeader
	var sawCredential, sawSignedHeaders, sawSignature bool

	for _, part := range strings.Split(rest, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			return nil, ErrMalformedAuthorization
		}
		switch key {
		case "Credential":
			scope, err := parseCredential(value)
			if err != nil {
				return nil, err
			}
			auth.AccessKeyID, auth.Scope = scope.accessKeyID, scope.scope
			sawCredential = true
		case "SignedHeaders":
			if value == "" {
				return nil, ErrMalformedAuthorization
			}
			auth.SignedHeaders = strings.Split(value, ";")
			sawSignedHeaders = true
		case "Signature":
			if value == "" {
				return nil, ErrMalformedAuthorization
			}
			auth.Signature = value
			sawSignature = true
		default:
			// Unknown components are ignored rather than rejected, so a future
			// AWS addition does not break every request.
		}
	}

	if !sawCredential || !sawSignedHeaders || !sawSignature {
		return nil, ErrMalformedAuthorization
	}
	return &auth, nil
}

type parsedCredential struct {
	accessKeyID string
	scope       Scope
}

// parseCredential splits AKIA.../20260822/us-east-1/s3/aws4_request.
//
// The access key id is everything before the first slash. Splitting from the
// left rather than by a fixed count matters: the scope always has exactly four
// components, but an access key id is opaque and could in principle be any
// length.
func parseCredential(value string) (parsedCredential, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 5 {
		return parsedCredential{}, ErrMalformedAuthorization
	}
	if parts[4] != scopeTerminator {
		return parsedCredential{}, ErrMalformedAuthorization
	}
	if parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return parsedCredential{}, ErrMalformedAuthorization
	}
	return parsedCredential{
		accessKeyID: parts[0],
		scope:       Scope{Date: parts[1], Region: parts[2], Service: parts[3]},
	}, nil
}

// CanonicalRequest builds the canonical form of a request, whose hash is what
// actually gets signed.
//
// The structure is fixed:
//
//	METHOD \n URI \n QUERY \n CANONICAL_HEADERS \n SIGNED_HEADERS \n PAYLOAD_HASH
//
// canonicalURI is passed in rather than derived here because S3 signs the path
// exactly as transmitted, and recovering that faithfully from net/http needs
// care — see canonicalURIs.
func CanonicalRequest(
	method, canonicalURI, canonicalQuery string,
	headers http.Header,
	host string,
	signedHeaders []string,
	payloadHash string,
) string {
	var b strings.Builder
	b.WriteString(method)
	b.WriteByte('\n')
	b.WriteString(canonicalURI)
	b.WriteByte('\n')
	b.WriteString(canonicalQuery)
	b.WriteByte('\n')
	b.WriteString(canonicalHeaders(headers, host, signedHeaders))
	b.WriteByte('\n')
	b.WriteString(strings.Join(signedHeaders, ";"))
	b.WriteByte('\n')
	b.WriteString(payloadHash)
	return b.String()
}

// canonicalHeaders renders each signed header as "name:value\n", names
// lowercased and already sorted by the caller.
//
// Host is special: net/http strips it from Header into Request.Host, so it is
// substituted from there. Missing it is the single most common reason a
// hand-rolled implementation disagrees with the SDKs.
func canonicalHeaders(headers http.Header, host string, signedHeaders []string) string {
	var b strings.Builder
	for _, name := range signedHeaders {
		b.WriteString(name)
		b.WriteByte(':')
		if name == "host" {
			b.WriteString(trimHeaderValue(host))
		} else {
			values := headers.Values(http.CanonicalHeaderKey(name))
			for i, v := range values {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(trimHeaderValue(v))
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// trimHeaderValue strips surrounding whitespace and collapses internal runs of
// spaces to one, as the specification requires. Values inside double quotes are
// left alone.
func trimHeaderValue(value string) string {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "  ") {
		return value
	}

	var b strings.Builder
	b.Grow(len(value))
	inQuotes := false
	prevSpace := false
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '"' {
			inQuotes = !inQuotes
		}
		if c == ' ' && !inQuotes {
			if prevSpace {
				continue
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
		b.WriteByte(c)
	}
	return b.String()
}

// StringToSign wraps the canonical request hash with the algorithm, timestamp
// and scope.
func StringToSign(t time.Time, scope Scope, canonicalRequest string) string {
	sum := sha256.Sum256([]byte(canonicalRequest))
	return strings.Join([]string{
		Algorithm,
		t.UTC().Format(iso8601),
		scope.String(),
		hex.EncodeToString(sum[:]),
	}, "\n")
}

// SigningKey derives the request-specific key by HMAC chaining the secret
// through date, region, service and terminator. Because the key is scoped to a
// single day and region, a leaked signing key is far less damaging than a
// leaked secret.
func SigningKey(secretKey string, scope Scope) []byte {
	key := hmacSHA256([]byte("AWS4"+secretKey), scope.Date)
	key = hmacSHA256(key, scope.Region)
	key = hmacSHA256(key, scope.Service)
	return hmacSHA256(key, scopeTerminator)
}

// Sign produces the hex signature for a string to sign.
func Sign(signingKey []byte, stringToSign string) string {
	return hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// signaturesEqual compares in constant time. A byte-by-byte comparison that
// returns early would leak, through timing, how much of a guessed signature was
// correct — enough to forge one a byte at a time.
func signaturesEqual(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

// sortedSignedHeaders returns the signed header names lowercased and sorted, as
// the canonical form requires. Clients normally send them that way already, but
// the specification requires the order and it is cheap to guarantee.
func sortedSignedHeaders(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strings.ToLower(strings.TrimSpace(n))
	}
	sort.Strings(out)
	return out
}
