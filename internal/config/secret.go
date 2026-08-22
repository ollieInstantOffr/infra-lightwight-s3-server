package config

import (
	"crypto/rand"
	"encoding/base64"
)

// randomSecret produces an ephemeral session secret for development. Because it
// changes on every restart, sessions do not survive a reload — which is the
// correct trade-off locally, and the reason production insists on a real value.
func randomSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
