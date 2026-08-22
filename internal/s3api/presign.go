package s3api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Presigned URLs move the signature out of the Authorization header and into
// the query string, so a plain browser — which cannot sign anything — can be
// handed a link that works for a bounded window without ever seeing the secret.
//
// This is how the console offers share links and direct browser uploads.

// Query parameters carrying a presigned signature.
const (
	paramAlgorithm     = "X-Amz-Algorithm"
	paramCredential    = "X-Amz-Credential"
	paramDate          = "X-Amz-Date"
	paramExpires       = "X-Amz-Expires"
	paramSignedHeaders = "X-Amz-SignedHeaders"
	paramSignature     = "X-Amz-Signature"
)

// maxPresignExpiry matches S3's ceiling. A link that never expires is a
// credential, and one handed out in an email or a chat message outlives every
// assumption made when it was created.
const maxPresignExpiry = 7 * 24 * time.Hour

// IsPresigned reports whether a request carries a query-string signature.
func IsPresigned(r *http.Request) bool {
	return r.URL.Query().Get(paramSignature) != ""
}

// Legacy Signature Version 2 query parameters. This server does not implement
// SigV2 — AWS deprecated it and it is materially weaker — but botocore still
// falls back to it when presigning against a custom endpoint unless
// signature_version is set explicitly.
//
// Detecting it is worth the few lines: the alternative is a bare "request is
// not signed", which sends the reader hunting for a credentials problem that
// does not exist. The fix is a one-line client change, so the error says so.
const (
	paramV2AccessKeyID = "AWSAccessKeyId"
	paramV2Signature   = "Signature"
	paramV2Expires     = "Expires"
)

// isSignatureV2 reports whether a request carries a legacy SigV2 query
// signature.
func isSignatureV2(r *http.Request) bool {
	query := r.URL.Query()
	return query.Get(paramV2AccessKeyID) != "" && query.Get(paramV2Signature) != ""
}

// errSignatureV2 explains the fix rather than merely refusing.
var errSignatureV2 = ErrInvalidArgument.WithMessage(
	"This request is signed with the deprecated AWS Signature Version 2. " +
		"This server requires Signature Version 4. " +
		"In boto3, pass Config(signature_version=\"s3v4\"); " +
		"in the AWS CLI, set signature_version = s3v4 in your profile.")

// verifyPresigned authenticates a request signed through the query string.
func (v *Verifier) verifyPresigned(ctx context.Context, r *http.Request) (*Identity, error) {
	query := r.URL.Query()

	if algorithm := query.Get(paramAlgorithm); algorithm != Algorithm {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedAlgorithm, algorithm)
	}

	credential, err := parseCredential(query.Get(paramCredential))
	if err != nil {
		return nil, err
	}

	signedAt, err := time.Parse(iso8601, query.Get(paramDate))
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not ISO8601 basic format", ErrMissingDate, paramDate)
	}

	expiresSeconds, err := strconv.Atoi(query.Get(paramExpires))
	if err != nil || expiresSeconds <= 0 {
		return nil, ErrInvalidArgument.WithMessage(
			"%s must be a positive number of seconds.", paramExpires)
	}
	expiry := time.Duration(expiresSeconds) * time.Second
	if expiry > maxPresignExpiry {
		return nil, ErrInvalidArgument.WithMessage(
			"%s must be at most %d seconds (7 days).", paramExpires, int(maxPresignExpiry.Seconds()))
	}

	now := v.now()
	// A link signed in the future is either a clock problem or an attempt to
	// extend the window, so the same skew allowance applies at both ends.
	if now.Before(signedAt.Add(-maxClockSkew)) {
		return nil, fmt.Errorf("%w: signed at %s, which is in the future",
			ErrRequestExpired, signedAt.UTC().Format(time.RFC3339))
	}
	if now.After(signedAt.Add(expiry)) {
		return nil, fmt.Errorf("%w: the link expired at %s",
			ErrRequestExpired, signedAt.Add(expiry).UTC().Format(time.RFC3339))
	}

	signedHeaders := sortedSignedHeaders(strings.Split(query.Get(paramSignedHeaders), ";"))
	if !slices.Contains(signedHeaders, "host") {
		return nil, fmt.Errorf("%w: host", ErrMissingSignedHeader)
	}

	secretKey, err := v.Lookup(ctx, credential.accessKeyID)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidAccessKeyID, credential.accessKeyID)
	}

	// The signature cannot cover itself, so it is excluded from the canonical
	// query. Everything else in the URL is signed, which is what stops a
	// recipient editing the key or the bucket out of a share link.
	canonicalQueryString := canonicalQuery(r.URL.RawQuery, paramSignature)
	host := v.Proxies.Host(r)
	signingKey := SigningKey(secretKey, credential.scope)

	// A browser sends no payload hash, so presigned requests are always
	// unsigned-payload.
	payloadHash := UnsignedPayload
	if declared := r.Header.Get("X-Amz-Content-Sha256"); declared != "" {
		payloadHash = declared
	}

	providedSignature := query.Get(paramSignature)
	for _, uri := range canonicalURIs(r) {
		canonical := CanonicalRequest(r.Method, uri, canonicalQueryString, r.Header, host, signedHeaders, payloadHash)
		expected := Sign(signingKey, StringToSign(signedAt, credential.scope, canonical))
		if signaturesEqual(expected, providedSignature) {
			return &Identity{
				AccessKeyID: credential.accessKeyID,
				PayloadHash: payloadHash,
				signingKey:  signingKey,
				scope:       credential.scope,
				timestamp:   signedAt.UTC().Format(iso8601),
				signature:   providedSignature,
			}, nil
		}
	}
	return nil, ErrSignatureMismatch
}

// Presign builds a signed URL for a request the recipient can make without
// credentials.
//
// The console uses this for share links and for browser uploads that go
// straight to the S3 port rather than through the console process.
func Presign(
	method, publicURL, bucket, key string,
	extraQuery map[string]string,
	accessKeyID, secretKey, region string,
	now time.Time,
	expiry time.Duration,
) (string, error) {
	if expiry <= 0 || expiry > maxPresignExpiry {
		return "", fmt.Errorf("expiry must be between 1 second and 7 days, got %s", expiry)
	}

	endpoint, err := parseEndpoint(publicURL)
	if err != nil {
		return "", err
	}

	scope := Scope{Date: now.UTC().Format(dateOnly), Region: region, Service: service}
	path := "/" + uriEncode(bucket, true) + "/" + uriEncode(key, false)

	values := map[string]string{
		paramAlgorithm:     Algorithm,
		paramCredential:    accessKeyID + "/" + scope.String(),
		paramDate:          now.UTC().Format(iso8601),
		paramExpires:       strconv.Itoa(int(expiry.Seconds())),
		paramSignedHeaders: "host",
	}
	for name, value := range extraQuery {
		values[name] = value
	}

	// Built by hand rather than with url.Values so the encoding matches SigV4's
	// rules exactly; url.Values would encode a space as "+".
	var pairs []string
	for name, value := range values {
		pairs = append(pairs, uriEncode(name, true)+"="+uriEncode(value, true))
	}
	slices.Sort(pairs)
	rawQuery := strings.Join(pairs, "&")

	canonical := CanonicalRequest(method, path, rawQuery,
		http.Header{}, endpoint.host, []string{"host"}, UnsignedPayload)
	signature := Sign(SigningKey(secretKey, scope), StringToSign(now, scope, canonical))

	return fmt.Sprintf("%s://%s%s?%s&%s=%s",
		endpoint.scheme, endpoint.host, path, rawQuery, paramSignature, signature), nil
}

type endpoint struct{ scheme, host string }

func parseEndpoint(publicURL string) (endpoint, error) {
	scheme, rest, found := strings.Cut(publicURL, "://")
	if !found || rest == "" {
		return endpoint{}, fmt.Errorf("public URL %q must be absolute, such as https://s3.example.com", publicURL)
	}
	host, _, _ := strings.Cut(rest, "/")
	if host == "" {
		return endpoint{}, fmt.Errorf("public URL %q has no host", publicURL)
	}
	return endpoint{scheme: scheme, host: host}, nil
}
