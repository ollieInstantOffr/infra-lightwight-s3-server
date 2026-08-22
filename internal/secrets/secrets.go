// Package secrets encrypts S3 secret keys at rest.
//
// Unlike a console password, an S3 secret cannot be stored as a one-way hash:
// SigV4 verification re-derives the signing key from the secret on every
// request, so the server must be able to recover the original value. The next
// best thing is authenticated encryption under a key held outside the database,
// so a database leak alone does not yield working credentials.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

// ErrUndecryptable means the ciphertext did not authenticate. In practice this
// almost always means CREDENTIALS_KEY has changed rather than that the data is
// corrupt, and the caller should say so rather than reporting a signature
// failure the operator cannot act on.
var ErrUndecryptable = errors.New("credential could not be decrypted; CREDENTIALS_KEY may have changed since it was created")

// Cipher encrypts and decrypts credential secrets with AES-256-GCM.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher derives an AES-256 key from the configured passphrase.
//
// SHA-256 is used as the derivation rather than a password hash such as Argon2
// because the input is a high-entropy generated key from `openssl rand`, not a
// human-chosen password. There is nothing to slow down an attacker guessing:
// the work factor would cost startup time and buy no security.
func NewCipher(key string) (*Cipher, error) {
	if len(key) < 32 {
		return nil, fmt.Errorf("credentials key must be at least 32 characters, got %d", len(key))
	}
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt returns the ciphertext and the nonce it was sealed under. They are
// stored in separate columns, so the nonce is returned separately rather than
// prefixed onto the ciphertext.
//
// accessKeyID is bound in as additional authenticated data: the ciphertext will
// only decrypt alongside the same access key id, so swapping encrypted secrets
// between rows in the database is detected rather than silently accepted.
func (c *Cipher) Encrypt(plaintext, accessKeyID string) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext = c.aead.Seal(nil, nonce, []byte(plaintext), []byte(accessKeyID))
	return ciphertext, nonce, nil
}

// Decrypt recovers a secret. A failure here is reported as ErrUndecryptable
// regardless of cause, since distinguishing corruption from a wrong key tells
// an attacker more than it tells the operator.
func (c *Cipher) Decrypt(ciphertext, nonce []byte, accessKeyID string) (string, error) {
	if len(nonce) != c.aead.NonceSize() {
		return "", ErrUndecryptable
	}
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, []byte(accessKeyID))
	if err != nil {
		return "", ErrUndecryptable
	}
	return string(plaintext), nil
}
