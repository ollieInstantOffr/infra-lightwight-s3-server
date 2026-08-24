package s3api

import (
	"strings"
	"testing"
)

// TrustedContentSHA256 is what lets Store.Put skip hashing a body a second
// time when Body has already arranged to verify it as the bytes stream past.
// It must say yes only for the one case where that is actually true — a
// literal digest — and no for every sentinel, including ones that still carry
// a per-chunk or trailer-based check, since those never produce a single
// whole-body SHA-256 to hand off.
func TestTrustedContentSHA256(t *testing.T) {
	realDigest := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

	cases := map[string]struct {
		payloadHash string
		want        string
	}{
		"a real digest is trusted":               {payloadHash: realDigest, want: realDigest},
		"a real digest is lowercased":            {payloadHash: strings.ToUpper(realDigest), want: realDigest},
		"streaming signed carries no whole hash": {payloadHash: StreamingSigned, want: ""},
		"streaming signed trailer likewise":      {payloadHash: StreamingSignedTrailer, want: ""},
		"streaming unsigned trailer likewise":    {payloadHash: StreamingUnsignedTrailer, want: ""},
		"unsigned payload has nothing to check":  {payloadHash: UnsignedPayload, want: ""},
		"a bodyless request has no digest":       {payloadHash: "", want: ""},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			id := &Identity{PayloadHash: c.payloadHash}
			if got := id.TrustedContentSHA256(); got != c.want {
				t.Errorf("TrustedContentSHA256() = %q, want %q", got, c.want)
			}
		})
	}
}
