package s3api

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// aws-chunked framing wraps the body as a sequence of length-prefixed chunks:
//
//	<hex-size>;chunk-signature=<hex>\r\n <data> \r\n
//	...
//	0;chunk-signature=<hex>\r\n
//	<optional trailing headers>\r\n
//	\r\n
//
// The chunk-signature parameter is absent in the unsigned variants. The current
// AWS SDKs default to STREAMING-UNSIGNED-PAYLOAD-TRAILER, so a server that
// cannot decode this framing hands callers a body with size headers embedded in
// it — silently corrupting every upload rather than failing outright.

var (
	// ErrMalformedChunk means the framing itself is broken.
	ErrMalformedChunk = errors.New("malformed aws-chunked encoding")
	// ErrChunkSignature means a chunk's signature did not verify, which breaks
	// the chain and invalidates everything after it.
	ErrChunkSignature = errors.New("chunk signature does not match")
)

const (
	// maxChunkHeaderSize bounds one chunk header line. Real headers are well
	// under 100 bytes; the limit stops a hostile client streaming an unbounded
	// "header" and exhausting memory.
	maxChunkHeaderSize = 1024

	// maxChunkSize bounds one chunk's payload. Signed chunks must be buffered
	// whole — a chunk's bytes cannot be trusted until its signature verifies —
	// so this doubles as the per-request memory ceiling on that path. The AWS
	// SDKs use 64 KiB to 8 MiB, so 16 MiB is generous without being reckless
	// under concurrency.
	maxChunkSize = 16 << 20

	// maxTrailerBytes bounds the trailing header section.
	maxTrailerBytes = 8 << 10

	readBufferSize = 64 << 10

	chunkStringToSignPrefix = "AWS4-HMAC-SHA256-PAYLOAD"
)

// chunkedReader decodes aws-chunked framing, optionally verifying the chunk
// signature chain.
//
// Two delivery paths exist deliberately. Unsigned chunks stream straight
// through, holding no more than the read buffer. Signed chunks are buffered
// whole, because their bytes must not reach the caller before the signature
// covering them has been checked.
type chunkedReader struct {
	src *bufio.Reader

	// verify and the chain state are unused for unsigned streams.
	verify        bool
	signingKey    []byte
	scope         Scope
	timestamp     string
	prevSignature string

	// pending holds a verified chunk awaiting delivery (signed path).
	pending []byte
	// remaining counts undelivered bytes of the current chunk (unsigned path).
	remaining int64
	// atChunkEnd records that the trailing CRLF still needs consuming.
	atChunkEnd bool

	finished bool
	err      error
}

// newChunkedReader decodes framing without verifying chunk signatures, for
// STREAMING-UNSIGNED-PAYLOAD-TRAILER.
func newChunkedReader(r io.Reader) *chunkedReader {
	return &chunkedReader{src: bufio.NewReaderSize(r, readBufferSize)}
}

// newSignedChunkedReader decodes framing and verifies each chunk against the
// running signature chain, seeded by the request's own signature.
func newSignedChunkedReader(r io.Reader, signingKey []byte, scope Scope, timestamp, seedSignature string) *chunkedReader {
	return &chunkedReader{
		src:           bufio.NewReaderSize(r, readBufferSize),
		verify:        true,
		signingKey:    signingKey,
		scope:         scope,
		timestamp:     timestamp,
		prevSignature: seedSignature,
	}
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if len(p) == 0 {
		return 0, nil
	}

	for {
		switch {
		case len(c.pending) > 0:
			n := copy(p, c.pending)
			c.pending = c.pending[n:]
			if len(c.pending) == 0 {
				if err := c.endOfChunk(); err != nil {
					return n, err
				}
			}
			return n, nil

		case c.remaining > 0:
			want := p
			if int64(len(want)) > c.remaining {
				want = want[:c.remaining]
			}
			n, err := c.src.Read(want)
			c.remaining -= int64(n)
			if err != nil {
				c.err = fmt.Errorf("%w: truncated chunk data: %v", ErrMalformedChunk, err)
				return n, c.err
			}
			if c.remaining == 0 {
				if err := c.endOfChunk(); err != nil {
					return n, err
				}
			}
			return n, nil

		case c.finished:
			return 0, io.EOF

		default:
			if err := c.nextChunk(); err != nil {
				c.err = err
				return 0, err
			}
		}
	}
}

// endOfChunk consumes the CRLF that follows a chunk's data.
func (c *chunkedReader) endOfChunk() error {
	if !c.atChunkEnd {
		return nil
	}
	c.atChunkEnd = false
	if err := c.expectCRLF(); err != nil {
		c.err = err
		return err
	}
	return nil
}

// nextChunk reads the next chunk header and prepares its body for delivery.
func (c *chunkedReader) nextChunk() error {
	line, err := c.readLine()
	if err != nil {
		return err
	}

	sizeField, signatureParam, hasSignature := strings.Cut(line, ";")
	size, err := strconv.ParseInt(strings.TrimSpace(sizeField), 16, 64)
	if err != nil || size < 0 {
		return fmt.Errorf("%w: bad chunk size %q", ErrMalformedChunk, sizeField)
	}
	if size > maxChunkSize {
		return fmt.Errorf("%w: chunk of %d bytes exceeds the %d byte limit",
			ErrMalformedChunk, size, maxChunkSize)
	}

	if c.verify {
		signature, err := chunkSignatureFrom(signatureParam, hasSignature)
		if err != nil {
			return err
		}
		// Buffer and verify before any of these bytes become readable.
		data := make([]byte, size)
		if _, err := io.ReadFull(c.src, data); err != nil {
			return fmt.Errorf("%w: truncated chunk: %v", ErrMalformedChunk, err)
		}
		if err := c.verifyChunk(data, signature); err != nil {
			return err
		}
		c.pending = data
	} else {
		c.remaining = size
	}

	if size == 0 {
		// The terminating chunk. Its CRLF is consumed here rather than through
		// the delivery path, which never runs for a zero-length chunk.
		c.pending = nil
		c.remaining = 0
		c.finished = true
		return c.consumeTrailers()
	}

	c.atChunkEnd = true
	return nil
}

// verifyChunk checks one chunk against the running signature chain. Each chunk
// signs the previous signature, so a tampered chunk invalidates everything
// after it as well as itself.
func (c *chunkedReader) verifyChunk(data []byte, signature string) error {
	sum := sha256.Sum256(data)
	empty := sha256.Sum256(nil)
	stringToSign := strings.Join([]string{
		chunkStringToSignPrefix,
		c.timestamp,
		c.scope.String(),
		c.prevSignature,
		hex.EncodeToString(empty[:]),
		hex.EncodeToString(sum[:]),
	}, "\n")

	if !signaturesEqual(Sign(c.signingKey, stringToSign), signature) {
		return ErrChunkSignature
	}
	c.prevSignature = signature
	return nil
}

func chunkSignatureFrom(param string, present bool) (string, error) {
	if !present {
		return "", fmt.Errorf("%w: chunk has no signature", ErrMalformedChunk)
	}
	name, value, found := strings.Cut(strings.TrimSpace(param), "=")
	if !found || name != "chunk-signature" || value == "" {
		return "", fmt.Errorf("%w: bad chunk-signature parameter %q", ErrMalformedChunk, param)
	}
	return value, nil
}

// consumeTrailers reads the trailing header section after the final chunk.
//
// Trailers carry checksums the client computed. They are read and discarded:
// validating them belongs with the checksum work rather than here, and leaving
// them unread would desynchronise a reused connection.
func (c *chunkedReader) consumeTrailers() error {
	read := 0
	for {
		line, err := c.readLine()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if line == "" {
			return nil
		}
		read += len(line)
		if read > maxTrailerBytes {
			return fmt.Errorf("%w: trailing headers exceed %d bytes", ErrMalformedChunk, maxTrailerBytes)
		}
	}
}

// readLine reads one CRLF-terminated line, without its terminator.
func (c *chunkedReader) readLine() (string, error) {
	line, err := c.src.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			if line == "" {
				return "", io.EOF
			}
			// A final line without its terminator is tolerated; the framing is
			// already complete by the time trailers are being read.
		} else {
			return "", fmt.Errorf("%w: %v", ErrMalformedChunk, err)
		}
	}
	if len(line) > maxChunkHeaderSize {
		return "", fmt.Errorf("%w: chunk header exceeds %d bytes", ErrMalformedChunk, maxChunkHeaderSize)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *chunkedReader) expectCRLF() error {
	crlf := make([]byte, 2)
	if _, err := io.ReadFull(c.src, crlf); err != nil {
		return fmt.Errorf("%w: expected CRLF after chunk: %v", ErrMalformedChunk, err)
	}
	if crlf[0] != '\r' || crlf[1] != '\n' {
		return fmt.Errorf("%w: expected CRLF after chunk, got %q", ErrMalformedChunk, crlf)
	}
	return nil
}
