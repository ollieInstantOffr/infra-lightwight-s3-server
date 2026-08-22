package s3api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The credentials AWS uses throughout its SigV4 documentation examples.
const (
	exampleAccessKeyID = "AKIAIOSFODNN7EXAMPLE"
	exampleSecretKey   = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

var exampleScope = Scope{Date: "20130524", Region: "us-east-1", Service: "s3"}

func exampleTime(t *testing.T) time.Time {
	t.Helper()
	ts, err := time.Parse(iso8601, "20130524T000000Z")
	if err != nil {
		t.Fatalf("parse example timestamp: %v", err)
	}
	return ts
}

// awsVector is one of AWS's published worked examples. Asserting on the
// canonical request as well as the signature is what makes a failure
// diagnosable: if the canonical request matches and the signature does not, the
// fault is in the HMAC chain rather than in request construction.
type awsVector struct {
	name             string
	method           string
	target           string
	host             string
	headers          map[string]string
	payloadHash      string
	signedHeaders    []string
	canonicalRequest string
	signature        string
}

func awsVectors() []awsVector {
	const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	return []awsVector{
		{
			name:   "GET Object with Range",
			method: http.MethodGet,
			target: "/test.txt",
			host:   "examplebucket.s3.amazonaws.com",
			headers: map[string]string{
				"Range":                "bytes=0-9",
				"x-amz-content-sha256": emptyHash,
				"x-amz-date":           "20130524T000000Z",
			},
			payloadHash:   emptyHash,
			signedHeaders: []string{"host", "range", "x-amz-content-sha256", "x-amz-date"},
			canonicalRequest: "GET\n" +
				"/test.txt\n" +
				"\n" +
				"host:examplebucket.s3.amazonaws.com\n" +
				"range:bytes=0-9\n" +
				"x-amz-content-sha256:" + emptyHash + "\n" +
				"x-amz-date:20130524T000000Z\n" +
				"\n" +
				"host;range;x-amz-content-sha256;x-amz-date\n" +
				emptyHash,
			signature: "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			// The "$" in the key is the point of this vector: it must appear
			// percent-encoded in the canonical URI even though it is a legal
			// path character that Go's url package leaves alone.
			name:   "PUT Object with an encoded character in the key",
			method: http.MethodPut,
			target: "/test%24file.text",
			host:   "examplebucket.s3.amazonaws.com",
			headers: map[string]string{
				"Date":                 "Fri, 24 May 2013 00:00:00 GMT",
				"x-amz-date":           "20130524T000000Z",
				"x-amz-storage-class":  "REDUCED_REDUNDANCY",
				"x-amz-content-sha256": "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			},
			payloadHash:   "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			signedHeaders: []string{"date", "host", "x-amz-content-sha256", "x-amz-date", "x-amz-storage-class"},
			canonicalRequest: "PUT\n" +
				"/test%24file.text\n" +
				"\n" +
				"date:Fri, 24 May 2013 00:00:00 GMT\n" +
				"host:examplebucket.s3.amazonaws.com\n" +
				"x-amz-content-sha256:44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072\n" +
				"x-amz-date:20130524T000000Z\n" +
				"x-amz-storage-class:REDUCED_REDUNDANCY\n" +
				"\n" +
				"date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class\n" +
				"44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			signature: "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
		},
		{
			// A valueless subresource must canonicalise to "lifecycle=", with
			// the trailing equals sign.
			name:   "GET Bucket Lifecycle",
			method: http.MethodGet,
			target: "/?lifecycle",
			host:   "examplebucket.s3.amazonaws.com",
			headers: map[string]string{
				"x-amz-content-sha256": emptyHash,
				"x-amz-date":           "20130524T000000Z",
			},
			payloadHash:   emptyHash,
			signedHeaders: []string{"host", "x-amz-content-sha256", "x-amz-date"},
			canonicalRequest: "GET\n" +
				"/\n" +
				"lifecycle=\n" +
				"host:examplebucket.s3.amazonaws.com\n" +
				"x-amz-content-sha256:" + emptyHash + "\n" +
				"x-amz-date:20130524T000000Z\n" +
				"\n" +
				"host;x-amz-content-sha256;x-amz-date\n" +
				emptyHash,
			signature: "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
		{
			name:   "GET Bucket with prefix and max-keys",
			method: http.MethodGet,
			target: "/?max-keys=2&prefix=J",
			host:   "examplebucket.s3.amazonaws.com",
			headers: map[string]string{
				"x-amz-content-sha256": emptyHash,
				"x-amz-date":           "20130524T000000Z",
			},
			payloadHash:   emptyHash,
			signedHeaders: []string{"host", "x-amz-content-sha256", "x-amz-date"},
			canonicalRequest: "GET\n" +
				"/\n" +
				"max-keys=2&prefix=J\n" +
				"host:examplebucket.s3.amazonaws.com\n" +
				"x-amz-content-sha256:" + emptyHash + "\n" +
				"x-amz-date:20130524T000000Z\n" +
				"\n" +
				"host;x-amz-content-sha256;x-amz-date\n" +
				emptyHash,
			signature: "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	}
}

func TestCanonicalRequestMatchesAWSVectors(t *testing.T) {
	for _, v := range awsVectors() {
		t.Run(v.name, func(t *testing.T) {
			r := httptest.NewRequest(v.method, v.target, nil)
			r.Host = v.host
			for k, val := range v.headers {
				r.Header.Set(k, val)
			}

			uris := canonicalURIs(r)
			query := canonicalQuery(r.URL.RawQuery, "")

			var got string
			var matched bool
			for _, uri := range uris {
				got = CanonicalRequest(v.method, uri, query, r.Header, v.host, v.signedHeaders, v.payloadHash)
				if got == v.canonicalRequest {
					matched = true
					break
				}
			}
			if !matched {
				t.Errorf("canonical request mismatch.\n got:\n%s\nwant:\n%s", got, v.canonicalRequest)
			}
		})
	}
}

func TestSignatureMatchesAWSVectors(t *testing.T) {
	ts := exampleTime(t)
	key := SigningKey(exampleSecretKey, exampleScope)

	for _, v := range awsVectors() {
		t.Run(v.name, func(t *testing.T) {
			got := Sign(key, StringToSign(ts, exampleScope, v.canonicalRequest))
			if got != v.signature {
				t.Errorf("signature = %s, want %s", got, v.signature)
			}
		})
	}
}

// End to end through the verifier, with a real Authorization header, proving
// the pieces compose as well as computing correctly in isolation.
func TestVerifyAcceptsAWSVector(t *testing.T) {
	v := awsVectors()[0]
	verifier := testVerifier(t, exampleTime(t))

	r := httptest.NewRequest(v.method, v.target, nil)
	r.Host = v.host
	for k, val := range v.headers {
		r.Header.Set(k, val)
	}
	r.Header.Set("Authorization", authorizationHeader(v.signedHeaders, v.signature))

	id, err := verifier.Verify(t.Context(), r)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.AccessKeyID != exampleAccessKeyID {
		t.Errorf("access key id = %s, want %s", id.AccessKeyID, exampleAccessKeyID)
	}
}

func TestVerifyRejectsTamperedRequest(t *testing.T) {
	v := awsVectors()[0]
	verifier := testVerifier(t, exampleTime(t))

	// Same signature, different Range header: the signature covers it, so this
	// must not verify.
	r := httptest.NewRequest(v.method, v.target, nil)
	r.Host = v.host
	for k, val := range v.headers {
		r.Header.Set(k, val)
	}
	r.Header.Set("Range", "bytes=0-99999")
	r.Header.Set("Authorization", authorizationHeader(v.signedHeaders, v.signature))

	if _, err := verifier.Verify(t.Context(), r); err == nil {
		t.Fatal("Verify accepted a request whose signed Range header had been altered")
	}
}

// A signature captured for one hostname must not be replayable against another,
// which is why host is required to be signed.
func TestVerifyRejectsSubstitutedHost(t *testing.T) {
	v := awsVectors()[0]
	verifier := testVerifier(t, exampleTime(t))

	r := httptest.NewRequest(v.method, v.target, nil)
	r.Host = "attacker.example.com"
	for k, val := range v.headers {
		r.Header.Set(k, val)
	}
	r.Header.Set("Authorization", authorizationHeader(v.signedHeaders, v.signature))

	if _, err := verifier.Verify(t.Context(), r); err == nil {
		t.Fatal("Verify accepted a request signed for a different host")
	}
}

func TestVerifyRejectsUnsignedHost(t *testing.T) {
	v := awsVectors()[0]
	verifier := testVerifier(t, exampleTime(t))

	r := httptest.NewRequest(v.method, v.target, nil)
	r.Host = v.host
	for k, val := range v.headers {
		r.Header.Set(k, val)
	}
	r.Header.Set("Authorization", authorizationHeader(
		[]string{"range", "x-amz-content-sha256", "x-amz-date"}, v.signature))

	_, err := verifier.Verify(t.Context(), r)
	if err == nil || !strings.Contains(err.Error(), "host") {
		t.Fatalf("Verify error = %v, want a missing-signed-header error naming host", err)
	}
}

func TestVerifyRejectsClockSkew(t *testing.T) {
	v := awsVectors()[0]

	for _, skew := range []time.Duration{20 * time.Minute, -20 * time.Minute} {
		verifier := testVerifier(t, exampleTime(t).Add(skew))
		r := httptest.NewRequest(v.method, v.target, nil)
		r.Host = v.host
		for k, val := range v.headers {
			r.Header.Set(k, val)
		}
		r.Header.Set("Authorization", authorizationHeader(v.signedHeaders, v.signature))

		if _, err := verifier.Verify(t.Context(), r); err == nil {
			t.Errorf("Verify accepted a request %v out of date", skew)
		}
	}

	// Inside the window it must still work, so the check is not simply always
	// rejecting.
	verifier := testVerifier(t, exampleTime(t).Add(10*time.Minute))
	r := httptest.NewRequest(v.method, v.target, nil)
	r.Host = v.host
	for k, val := range v.headers {
		r.Header.Set(k, val)
	}
	r.Header.Set("Authorization", authorizationHeader(v.signedHeaders, v.signature))
	if _, err := verifier.Verify(t.Context(), r); err != nil {
		t.Errorf("Verify rejected a request only 10 minutes old: %v", err)
	}
}

func TestVerifyRejectsUnknownAccessKey(t *testing.T) {
	v := awsVectors()[0]
	verifier := testVerifier(t, exampleTime(t))
	verifier.Lookup = func(_ context.Context, _ string) (string, error) {
		return "", errNoSuchKey
	}

	r := httptest.NewRequest(v.method, v.target, nil)
	r.Host = v.host
	for k, val := range v.headers {
		r.Header.Set(k, val)
	}
	r.Header.Set("Authorization", authorizationHeader(v.signedHeaders, v.signature))

	if _, err := verifier.Verify(t.Context(), r); !errors.Is(err, ErrInvalidAccessKeyID) {
		t.Fatalf("Verify error = %v, want ErrInvalidAccessKeyID", err)
	}
}

func authorizationHeader(signedHeaders []string, signature string) string {
	return Algorithm +
		" Credential=" + exampleAccessKeyID + "/" + exampleScope.String() +
		",SignedHeaders=" + strings.Join(signedHeaders, ";") +
		",Signature=" + signature
}
