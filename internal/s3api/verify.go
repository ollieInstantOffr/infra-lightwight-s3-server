package s3api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/httpx"
)

// SecretLookup resolves an access key id to its secret. It returns
// db.ErrCredentialNotFound or db.ErrCredentialRevoked for unusable keys; the
// verifier treats both as ErrInvalidAccessKeyID so a client cannot tell a
// revoked key from one that never existed.
type SecretLookup func(ctx context.Context, accessKeyID string) (secretKey string, err error)

// Verifier authenticates S3 requests.
type Verifier struct {
	Region  string
	Lookup  SecretLookup
	Proxies *httpx.ProxyTrust

	// Now is injectable so clock-skew behaviour can be tested without waiting.
	Now func() time.Time
}

// Identity is the outcome of a successful verification. It carries the chain
// state that signed streaming bodies need, so Body can pick up where Verify
// left off.
type Identity struct {
	AccessKeyID string
	// PayloadHash is the literal x-amz-content-sha256 value, which may be a
	// real digest or one of the streaming sentinels.
	PayloadHash string

	signingKey []byte
	scope      Scope
	timestamp  string
	signature  string
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// Verify authenticates a request and returns the caller's identity.
//
// The body is deliberately not touched here. Object uploads are unbounded, and
// reading them to verify a hash before deciding whether the request is even
// authorised would let an unauthenticated caller stream arbitrary data at the
// server. Body handles payload integrity afterwards, as the handler reads.
func (v *Verifier) Verify(ctx context.Context, r *http.Request) (*Identity, error) {
	// A query-string signature means a presigned URL, which a browser can use
	// without ever holding a credential.
	if IsPresigned(r) {
		return v.verifyPresigned(ctx, r)
	}
	// Named explicitly so a client using the deprecated scheme is told what to
	// change, rather than being told its request is unsigned.
	if isSignatureV2(r) {
		return nil, errSignatureV2
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return nil, ErrMissingAuthorization
	}
	auth, err := ParseAuthorization(authHeader)
	if err != nil {
		return nil, err
	}

	timestamp, err := requestTime(r)
	if err != nil {
		return nil, err
	}
	if skew := v.now().Sub(timestamp); skew > maxClockSkew || skew < -maxClockSkew {
		return nil, fmt.Errorf("%w: signed at %s, server clock reads %s",
			ErrRequestExpired, timestamp.UTC().Format(time.RFC3339), v.now().UTC().Format(time.RFC3339))
	}
	// The scope's date must agree with the request timestamp, otherwise a
	// signature could be replayed under a different day's signing key.
	if scopeDate := timestamp.UTC().Format(dateOnly); scopeDate != auth.Scope.Date {
		return nil, fmt.Errorf("%w: credential scope date %q does not match request date %q",
			ErrSignatureMismatch, auth.Scope.Date, scopeDate)
	}

	signedHeaders := sortedSignedHeaders(auth.SignedHeaders)
	// Host must be signed. Without it a signature captured for one hostname
	// would be replayable against another.
	if !slices.Contains(signedHeaders, "host") {
		return nil, fmt.Errorf("%w: host", ErrMissingSignedHeader)
	}

	secretKey, err := v.Lookup(ctx, auth.AccessKeyID)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidAccessKeyID, auth.AccessKeyID)
	}

	payloadHash := payloadHashOf(r)
	host := v.Proxies.Host(r)
	query := canonicalQuery(r.URL.RawQuery, "")
	signingKey := SigningKey(secretKey, auth.Scope)

	// S3 signs the path as transmitted and does not normalize it. canonicalURIs
	// returns the faithful reading first and a re-encoded fallback second, for
	// the cases where net/http had to synthesise an encoding.
	for _, uri := range canonicalURIs(r) {
		canonical := CanonicalRequest(r.Method, uri, query, r.Header, host, signedHeaders, payloadHash)
		expected := Sign(signingKey, StringToSign(timestamp, auth.Scope, canonical))
		if signaturesEqual(expected, auth.Signature) {
			return &Identity{
				AccessKeyID: auth.AccessKeyID,
				PayloadHash: payloadHash,
				signingKey:  signingKey,
				scope:       auth.Scope,
				timestamp:   timestamp.UTC().Format(iso8601),
				signature:   auth.Signature,
			}, nil
		}
	}
	return nil, ErrSignatureMismatch
}

// Body returns the request's payload with transport framing removed and
// integrity enforced.
//
// Which of those applies depends on x-amz-content-sha256: a real digest is
// checked as the body is read, aws-chunked framing is decoded (and for the
// signed variants, verified chunk by chunk), and UNSIGNED-PAYLOAD passes
// through untouched.
func (v *Verifier) Body(r *http.Request, id *Identity) io.ReadCloser {
	switch id.PayloadHash {
	case StreamingSigned, StreamingSignedTrailer:
		return readCloser{
			Reader: newSignedChunkedReader(r.Body, id.signingKey, id.scope, id.timestamp, id.signature),
			Closer: r.Body,
		}
	case StreamingUnsignedTrailer:
		return readCloser{Reader: newChunkedReader(r.Body), Closer: r.Body}
	case UnsignedPayload, "":
		return r.Body
	default:
		return readCloser{Reader: newVerifyingReader(r.Body, id.PayloadHash), Closer: r.Body}
	}
}

// requestTime reads the signing timestamp from x-amz-date, falling back to
// Date. Clients that send neither cannot be verified.
func requestTime(r *http.Request) (time.Time, error) {
	if raw := r.Header.Get("X-Amz-Date"); raw != "" {
		t, err := time.Parse(iso8601, raw)
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: x-amz-date %q is not ISO8601 basic format", ErrMissingDate, raw)
		}
		return t, nil
	}
	if raw := r.Header.Get("Date"); raw != "" {
		if t, err := http.ParseTime(raw); err == nil {
			return t, nil
		}
	}
	return time.Time{}, ErrMissingDate
}

// payloadHashOf reads the declared payload hash.
//
// A missing header is treated as the hash of an empty body, which is what
// bodyless requests from older clients rely on. It is not defaulted to
// UNSIGNED-PAYLOAD: that would let a caller strip integrity checking from a
// request the client had actually signed with a real digest.
func payloadHashOf(r *http.Request) string {
	if h := r.Header.Get("X-Amz-Content-Sha256"); h != "" {
		return h
	}
	return emptyStringSHA256
}

// verifyingReader checks the body against its declared SHA-256 as it streams.
//
// The mismatch surfaces at EOF rather than up front, because detecting it up
// front would mean buffering the whole object. Handlers must therefore treat a
// read error as fatal to the request and discard what they have written.
type verifyingReader struct {
	src      io.Reader
	hash     hash.Hash
	expected string
	done     bool
}

func newVerifyingReader(src io.Reader, expectedHex string) *verifyingReader {
	return &verifyingReader{src: src, hash: sha256.New(), expected: strings.ToLower(expectedHex)}
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	n, err := v.src.Read(p)
	if n > 0 {
		v.hash.Write(p[:n])
	}
	if errors.Is(err, io.EOF) && !v.done {
		v.done = true
		if got := hex.EncodeToString(v.hash.Sum(nil)); got != v.expected {
			return n, fmt.Errorf("%w: declared %s, received %s", ErrContentHashMismatch, v.expected, got)
		}
	}
	return n, err
}

// readCloser pairs a decoded stream with the underlying body, so closing still
// releases the connection.
type readCloser struct {
	io.Reader
	io.Closer
}
