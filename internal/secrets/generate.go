package secrets

import (
	"crypto/rand"
	"fmt"
)

const (
	// accessKeyIDLength matches AWS: "AKIA" plus 16 characters.
	accessKeyIDBodyLength = 16
	// secretKeyLength matches AWS's 40-character secret.
	secretKeyLength = 40
)

// AWS access key ids are uppercase alphanumeric. Secrets are base64-ish in
// practice, but any printable set works as long as clients can carry it
// verbatim; this alphabet avoids characters that get mangled in shell config
// files and .env parsing.
const (
	accessKeyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	secretKeyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
)

// GenerateAccessKeyID returns an AWS-shaped access key id such as
// AKIA7NKQ2WVMX4HJ6PZR. The shape matters only for familiarity; nothing parses
// it beyond looking it up.
func GenerateAccessKeyID() (string, error) {
	body, err := randomString(accessKeyIDBodyLength, accessKeyAlphabet)
	if err != nil {
		return "", fmt.Errorf("generate access key id: %w", err)
	}
	return "AKIA" + body, nil
}

// GenerateSecretKey returns a 40-character secret with roughly 240 bits of
// entropy.
func GenerateSecretKey() (string, error) {
	s, err := randomString(secretKeyLength, secretKeyAlphabet)
	if err != nil {
		return "", fmt.Errorf("generate secret key: %w", err)
	}
	return s, nil
}

// randomString draws uniformly from alphabet using rejection sampling, so no
// character is more likely than another. A modulo of raw random bytes would
// skew toward the start of the alphabet.
func randomString(n int, alphabet string) (string, error) {
	size := len(alphabet)
	// The largest multiple of size that fits in a byte; values at or above this
	// are discarded rather than folded, which is what would introduce the bias.
	//
	// Kept as an int deliberately. When size divides 256 exactly — as both
	// alphabets here do — the limit is 256, which truncates to 0 as a byte and
	// would reject every sample, looping forever.
	limit := 256 - (256 % size)

	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, alphabet[int(b)%size])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}
