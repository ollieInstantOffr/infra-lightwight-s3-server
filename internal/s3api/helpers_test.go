package s3api

import (
	"context"
	"errors"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"testing"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/httpx"
)

var errNoSuchKey = errors.New("no such access key")

// testVerifier builds a Verifier with a frozen clock and a lookup that returns
// AWS's documented example secret.
func testVerifier(t *testing.T, now time.Time) *Verifier {
	t.Helper()
	trust, err := httpx.NewProxyTrust(nil)
	if err != nil {
		t.Fatalf("NewProxyTrust: %v", err)
	}
	return &Verifier{
		Region:  "us-east-1",
		Proxies: trust,
		Now:     func() time.Time { return now },
		Lookup: func(_ context.Context, accessKeyID string) (KeyMaterial, error) {
			if accessKeyID != exampleAccessKeyID {
				return KeyMaterial{}, errNoSuchKey
			}
			return KeyMaterial{SecretKey: exampleSecretKey, Grant: db.UnrestrictedGrant()}, nil
		},
	}
}
