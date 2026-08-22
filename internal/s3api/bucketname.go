package s3api

import (
	"net"
	"strings"
)

// S3 bucket names are constrained far more tightly than a database column can
// express, largely because they have to be valid DNS labels: a bucket is
// addressable as bucket.s3.example.com. The rules are enforced here rather than
// in SQL so the error can name the exact rule that was broken.

const (
	minBucketNameLength = 3
	maxBucketNameLength = 63
)

// reservedBucketPrefixes and reservedBucketSuffixes are reserved by AWS. They
// are rejected here too: a bucket named for one of these would be unusable
// against real S3, and this server exists to be a drop-in for it.
var (
	reservedBucketPrefixes = []string{"xn--", "sthree-", "amzn-s3-demo-"}
	reservedBucketSuffixes = []string{"-s3alias", "--ol-s3", ".mrap", "--x-s3"}
)

// ValidateBucketName checks a name against S3's rules, returning an
// InvalidBucketName error naming the specific problem.
func ValidateBucketName(name string) error {
	invalid := func(format string, args ...any) error {
		return ErrInvalidBucketName.WithMessage(format, args...)
	}

	if len(name) < minBucketNameLength || len(name) > maxBucketNameLength {
		return invalid("Bucket name must be between %d and %d characters long, got %d.",
			minBucketNameLength, maxBucketNameLength, len(name))
	}

	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			if c >= 'A' && c <= 'Z' {
				return invalid("Bucket name must not contain uppercase characters.")
			}
			return invalid("Bucket name must contain only lowercase letters, numbers, hyphens and periods.")
		}
	}

	if !isBucketNameAlnum(name[0]) {
		return invalid("Bucket name must begin with a letter or number.")
	}
	if !isBucketNameAlnum(name[len(name)-1]) {
		return invalid("Bucket name must end with a letter or number.")
	}

	if strings.Contains(name, "..") {
		return invalid("Bucket name must not contain two adjacent periods.")
	}
	// A label boundary next to a hyphen produces a name that is legal as a
	// string but not as a DNS label.
	if strings.Contains(name, ".-") || strings.Contains(name, "-.") {
		return invalid("Bucket name must not contain a period adjacent to a hyphen.")
	}

	// An IP-shaped name is ambiguous with a literal address in a URL.
	if ip := net.ParseIP(name); ip != nil {
		return invalid("Bucket name must not be formatted as an IP address.")
	}

	for _, prefix := range reservedBucketPrefixes {
		if strings.HasPrefix(name, prefix) {
			return invalid("Bucket name must not start with the reserved prefix %q.", prefix)
		}
	}
	for _, suffix := range reservedBucketSuffixes {
		if strings.HasSuffix(name, suffix) {
			return invalid("Bucket name must not end with the reserved suffix %q.", suffix)
		}
	}
	return nil
}

func isBucketNameAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

// maxObjectKeyLength is S3's limit, measured in UTF-8 bytes rather than runes.
const maxObjectKeyLength = 1024

// ValidateObjectKey checks a key is usable.
//
// S3 keys are deliberately permissive — almost any UTF-8 sequence is legal, and
// clients rely on that. Only length, emptiness and NUL are rejected; a NUL byte
// cannot survive a round trip through a filesystem path or a Postgres text
// column, so accepting it would mean silently altering the key.
func ValidateObjectKey(key string) error {
	if key == "" {
		return ErrInvalidArgument.WithMessage("Object key must not be empty.")
	}
	if len(key) > maxObjectKeyLength {
		return ErrKeyTooLong.WithMessage(
			"Object key must be at most %d bytes, got %d.", maxObjectKeyLength, len(key))
	}
	if strings.ContainsRune(key, 0) {
		return ErrInvalidArgument.WithMessage("Object key must not contain null bytes.")
	}
	return nil
}
