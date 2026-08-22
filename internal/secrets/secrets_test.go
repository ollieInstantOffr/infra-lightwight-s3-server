package secrets

import (
	"errors"
	"strings"
	"testing"
)

const testKey = "a-credentials-key-of-at-least-32-characters"

func TestEncryptDecryptRoundTrip(t *testing.T) {
	c, err := NewCipher(testKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	secret := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	accessKeyID := "AKIAIOSFODNN7EXAMPLE"

	ciphertext, nonce, err := c.Encrypt(secret, accessKeyID)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if strings.Contains(string(ciphertext), secret) {
		t.Fatal("plaintext secret is visible in the ciphertext")
	}

	got, err := c.Decrypt(ciphertext, nonce, accessKeyID)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != secret {
		t.Errorf("decrypted %q, want %q", got, secret)
	}
}

// Encrypting the same secret twice must produce different ciphertexts, or an
// observer could tell which credentials share a secret.
func TestEncryptUsesFreshNonce(t *testing.T) {
	c, _ := NewCipher(testKey)
	first, nonce1, _ := c.Encrypt("same-secret", "AKIA1")
	second, nonce2, _ := c.Encrypt("same-secret", "AKIA1")

	if string(nonce1) == string(nonce2) {
		t.Error("nonce was reused, which is catastrophic for GCM")
	}
	if string(first) == string(second) {
		t.Error("identical plaintexts produced identical ciphertexts")
	}
}

// The access key id is bound in as additional authenticated data, so an
// encrypted secret moved to a different row will not decrypt.
func TestDecryptRejectsSwappedAccessKey(t *testing.T) {
	c, _ := NewCipher(testKey)
	ciphertext, nonce, _ := c.Encrypt("secret-value", "AKIAORIGINAL")

	if _, err := c.Decrypt(ciphertext, nonce, "AKIASWAPPED"); !errors.Is(err, ErrUndecryptable) {
		t.Errorf("Decrypt with a different access key id = %v, want ErrUndecryptable", err)
	}
}

// Changing CREDENTIALS_KEY must produce a clear, recognisable failure rather
// than garbage that later surfaces as an inscrutable signature mismatch.
func TestDecryptWithWrongKey(t *testing.T) {
	original, _ := NewCipher(testKey)
	ciphertext, nonce, _ := original.Encrypt("secret-value", "AKIA1")

	rotated, err := NewCipher("a-completely-different-key-of-sufficient-length")
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	if _, err := rotated.Decrypt(ciphertext, nonce, "AKIA1"); !errors.Is(err, ErrUndecryptable) {
		t.Errorf("Decrypt with a rotated key = %v, want ErrUndecryptable", err)
	}
}

func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	c, _ := NewCipher(testKey)
	ciphertext, nonce, _ := c.Encrypt("secret-value", "AKIA1")
	ciphertext[0] ^= 0xff

	if _, err := c.Decrypt(ciphertext, nonce, "AKIA1"); !errors.Is(err, ErrUndecryptable) {
		t.Errorf("Decrypt of tampered ciphertext = %v, want ErrUndecryptable", err)
	}
}

func TestNewCipherRejectsShortKey(t *testing.T) {
	if _, err := NewCipher("too-short"); err == nil {
		t.Error("NewCipher accepted a key under 32 characters")
	}
}

func TestGeneratedKeysHaveTheRightShape(t *testing.T) {
	id, err := GenerateAccessKeyID()
	if err != nil {
		t.Fatalf("GenerateAccessKeyID: %v", err)
	}
	if !strings.HasPrefix(id, "AKIA") || len(id) != 4+accessKeyIDBodyLength {
		t.Errorf("access key id %q does not have the expected shape", id)
	}

	secret, err := GenerateSecretKey()
	if err != nil {
		t.Fatalf("GenerateSecretKey: %v", err)
	}
	if len(secret) != secretKeyLength {
		t.Errorf("secret key length = %d, want %d", len(secret), secretKeyLength)
	}
}

// Rejection sampling should draw uniformly. A modulo-biased generator would
// over-represent the start of the alphabet; this is a coarse check that it does
// not.
func TestGeneratedKeysAreNotObviouslyBiased(t *testing.T) {
	seen := make(map[rune]int)
	for range 500 {
		s, err := GenerateSecretKey()
		if err != nil {
			t.Fatalf("GenerateSecretKey: %v", err)
		}
		for _, r := range s {
			seen[r]++
		}
	}
	if len(seen) < len(secretKeyAlphabet)*3/4 {
		t.Errorf("only %d of %d alphabet characters appeared across 20,000 draws",
			len(seen), len(secretKeyAlphabet))
	}
}

func TestGeneratedKeysAreUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		id, err := GenerateAccessKeyID()
		if err != nil {
			t.Fatalf("GenerateAccessKeyID: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate access key id generated: %s", id)
		}
		seen[id] = true
	}
}
