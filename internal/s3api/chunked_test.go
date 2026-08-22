package s3api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// AWS's published chunked-upload example: a 66,560 byte object of 'a'
// characters sent as a 64 KiB chunk, a 1 KiB chunk, and the terminating chunk.
// The signatures below are the ones AWS documents, so a pass here means the
// chunk chaining matches the real service and not merely itself.
const (
	exampleSeedSignature   = "4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9"
	exampleChunk1Signature = "ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648"
	exampleChunk2Signature = "0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497"
	exampleChunk3Signature = "b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9"

	exampleTimestamp = "20130524T000000Z"
)

func signedChunk(size int, signature string, data []byte) string {
	return fmt.Sprintf("%x;chunk-signature=%s\r\n%s\r\n", size, signature, data)
}

func TestSignedChunkedReaderMatchesAWSVector(t *testing.T) {
	chunk1 := bytes.Repeat([]byte("a"), 65536)
	chunk2 := bytes.Repeat([]byte("a"), 1024)

	body := signedChunk(len(chunk1), exampleChunk1Signature, chunk1) +
		signedChunk(len(chunk2), exampleChunk2Signature, chunk2) +
		signedChunk(0, exampleChunk3Signature, nil)

	r := newSignedChunkedReader(
		strings.NewReader(body),
		SigningKey(exampleSecretKey, exampleScope),
		exampleScope,
		exampleTimestamp,
		exampleSeedSignature,
	)

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := append(append([]byte{}, chunk1...), chunk2...)
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded %d bytes, want %d", len(got), len(want))
	}
}

// Tampering with a chunk's contents must break its signature, and because each
// chunk signs the previous signature, everything after it too.
func TestSignedChunkedReaderRejectsTamperedChunk(t *testing.T) {
	chunk1 := bytes.Repeat([]byte("a"), 65536)
	chunk1[0] = 'b'

	body := signedChunk(len(chunk1), exampleChunk1Signature, chunk1) +
		signedChunk(0, exampleChunk3Signature, nil)

	r := newSignedChunkedReader(
		strings.NewReader(body),
		SigningKey(exampleSecretKey, exampleScope),
		exampleScope,
		exampleTimestamp,
		exampleSeedSignature,
	)

	if _, err := io.ReadAll(r); !errors.Is(err, ErrChunkSignature) {
		t.Fatalf("ReadAll error = %v, want ErrChunkSignature", err)
	}
}

// A tampered chunk must not have delivered any of its bytes before the
// signature was checked.
func TestSignedChunkedReaderWithholdsUnverifiedBytes(t *testing.T) {
	chunk1 := bytes.Repeat([]byte("a"), 65536)
	chunk1[65535] = 'b' // corrupt the very last byte

	body := signedChunk(len(chunk1), exampleChunk1Signature, chunk1)

	r := newSignedChunkedReader(
		strings.NewReader(body),
		SigningKey(exampleSecretKey, exampleScope),
		exampleScope,
		exampleTimestamp,
		exampleSeedSignature,
	)

	buf := make([]byte, 1024)
	n, err := r.Read(buf)
	if err == nil {
		t.Fatalf("first Read returned %d bytes and no error; unverified data reached the caller", n)
	}
	if n != 0 {
		t.Errorf("first Read returned %d bytes before verification failed, want 0", n)
	}
}

// STREAMING-UNSIGNED-PAYLOAD-TRAILER, which the current AWS SDKs send by
// default. The trailing checksum header must be consumed rather than delivered
// as object data.
func TestUnsignedChunkedReaderWithTrailer(t *testing.T) {
	payload := bytes.Repeat([]byte("hello world "), 4096)

	body := fmt.Sprintf("%x\r\n%s\r\n", len(payload), payload) +
		"0\r\n" +
		"x-amz-checksum-crc32:hK5nBg==\r\n" +
		"\r\n"

	got, err := io.ReadAll(newChunkedReader(strings.NewReader(body)))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("decoded %d bytes, want %d; trailer may have leaked into the body", len(got), len(payload))
	}
}

func TestUnsignedChunkedReaderMultipleChunks(t *testing.T) {
	parts := [][]byte{
		[]byte("first chunk "),
		[]byte("second chunk "),
		[]byte("third chunk"),
	}
	var body strings.Builder
	var want []byte
	for _, p := range parts {
		fmt.Fprintf(&body, "%x\r\n%s\r\n", len(p), p)
		want = append(want, p...)
	}
	body.WriteString("0\r\n\r\n")

	got, err := io.ReadAll(newChunkedReader(strings.NewReader(body.String())))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestChunkedReaderRejectsMalformedFraming(t *testing.T) {
	cases := map[string]string{
		"non-hex size":        "zz\r\ndata\r\n0\r\n\r\n",
		"missing CRLF":        "4\r\ndataXX0\r\n\r\n",
		"truncated chunk":     "100\r\nshort\r\n",
		"oversized chunk":     "FFFFFFFF\r\n",
		"header without CRLF": strings.Repeat("A", maxChunkHeaderSize+10),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := io.ReadAll(newChunkedReader(strings.NewReader(body))); err == nil {
				t.Error("ReadAll accepted malformed framing")
			}
		})
	}
}

// An unsigned reader must not silently accept a stream whose chunks carry
// signatures it never checks; conversely a signed reader must refuse chunks
// that carry none.
func TestSignedChunkedReaderRequiresSignatures(t *testing.T) {
	body := "4\r\ndata\r\n0\r\n\r\n"

	r := newSignedChunkedReader(
		strings.NewReader(body),
		SigningKey(exampleSecretKey, exampleScope),
		exampleScope,
		exampleTimestamp,
		exampleSeedSignature,
	)
	if _, err := io.ReadAll(r); !errors.Is(err, ErrMalformedChunk) {
		t.Fatalf("ReadAll error = %v, want ErrMalformedChunk", err)
	}
}
