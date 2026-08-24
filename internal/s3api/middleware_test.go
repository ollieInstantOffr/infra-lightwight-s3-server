package s3api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// GetObject is served by http.ServeContent, which copies the blob with
// io.Copy — and io.Copy's one real optimization is checking whether the
// destination implements io.ReaderFrom, which is how Go's own
// http.ResponseWriter turns that copy into a kernel-side sendfile(2). A
// wrapper that only implements Write hides that from io.Copy without
// anything failing: the response is still correct, just copied through
// userspace instead of staying in the kernel. These tests hold the fix to
// account, not just to existing.

func TestResponseRecorderReadFromDelegatesToTheRealWriter(t *testing.T) {
	inner := &countingReadFrom{}
	rec := &responseRecorder{ResponseWriter: inner}

	body := strings.NewReader("the quick brown fox")
	n, err := rec.ReadFrom(body)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if n != int64(body.Size()) {
		t.Errorf("ReadFrom returned %d, want %d", n, body.Size())
	}
	if inner.calls != 1 {
		t.Fatalf("the underlying ReadFrom was called %d times, want exactly 1 — "+
			"anything else means the fast path was not actually reached", inner.calls)
	}
	if rec.written != int64(body.Size()) {
		t.Errorf("recorder counted %d bytes, want %d — ReadFrom bypasses Write, "+
			"so the byte count has to be recorded separately or the access log lies", rec.written, body.Size())
	}
	if rec.status != http.StatusOK {
		t.Errorf("status = %d, want 200 to be defaulted, same as Write would", rec.status)
	}
}

// countingReadFrom is a minimal io.Writer + io.ReaderFrom, standing in for
// Go's real http.ResponseWriter without pulling in the rest of net/http's
// response machinery.
type countingReadFrom struct {
	calls   int
	written int64
	header  http.Header
	status  int
}

func (c *countingReadFrom) Header() http.Header {
	if c.header == nil {
		c.header = http.Header{}
	}
	return c.header
}

func (c *countingReadFrom) Write(p []byte) (int, error) {
	c.written += int64(len(p))
	return len(p), nil
}

func (c *countingReadFrom) WriteHeader(status int) { c.status = status }

func (c *countingReadFrom) ReadFrom(r io.Reader) (int64, error) {
	c.calls++
	n, err := io.Copy(discardWriter{c}, r)
	return n, err
}

// discardWriter forwards to countingReadFrom.Write without exposing its
// ReadFrom, the same writerOnly trick the production code uses — otherwise
// io.Copy inside ReadFrom would see ReadFrom again and recurse.
type discardWriter struct{ w *countingReadFrom }

func (d discardWriter) Write(p []byte) (int, error) { return d.w.Write(p) }

// readOnly hides a Reader's own WriteTo (if it has one) so io.Copy is forced
// to look at the destination instead — matching an *os.File, which has no
// WriteTo of its own and is what a real blob read actually copies from.
type readOnly struct{ io.Reader }

func TestResponseRecorderReadFromFallsBackWhenTheRealWriterCannot(t *testing.T) {
	// httptest.NewRecorder does not implement io.ReaderFrom, which is exactly
	// the shape of a test double or anything else that only offers Write. The
	// byte count still has to come out right.
	plain := httptest.NewRecorder()
	rec := &responseRecorder{ResponseWriter: plain}

	body := strings.NewReader("no fast path here")
	n, err := rec.ReadFrom(body)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if n != int64(body.Size()) || rec.written != int64(body.Size()) {
		t.Errorf("wrote %d, recorded %d, want %d for both", n, rec.written, body.Size())
	}
	if plain.Body.String() != "no fast path here" {
		t.Errorf("body = %q, want the fallback copy to have actually reached the writer", plain.Body.String())
	}
}

func TestRecorderForReusesAnExistingRecorderRatherThanWrapping(t *testing.T) {
	// The bug this whole fix responds to: four middlewares in this package
	// each used to allocate their own wrapper around whatever they received.
	// Reused per this helper, the second and third callers must get back the
	// exact same pointer the first one created — anything else means a chain
	// of these middlewares is back to stacking wrappers, and ReadFrom
	// visibility breaks again the moment two of them are wired together.
	base := httptest.NewRecorder()

	first := recorderFor(base)
	second := recorderFor(first)
	third := recorderFor(second)

	if second != first {
		t.Error("recorderFor wrapped an existing recorder instead of reusing it")
	}
	if third != first {
		t.Error("recorderFor wrapped an existing recorder a second time")
	}
}

func TestReadFromSurvivesTheRealFourMiddlewareChain(t *testing.T) {
	// The real chain, in the real order Server.Handler assembles it, ending in
	// a handler that writes via io.Copy the way http.ServeContent does for
	// GetObject. The fake ResponseWriter at the bottom implements
	// io.ReaderFrom; the assertion is that its ReadFrom is what actually gets
	// called — proving the interface survives being passed through all four
	// middlewares, not just through one recorder tested in isolation.
	//
	// This does not by itself prove there is only one recorder in the chain —
	// every layer now forwards ReadFrom, so delegation reaches the bottom
	// whether there is one shared recorder or four nested ones each passing
	// it along. That narrower property — no redundant wrapping — is what
	// TestRecorderForReusesAnExistingRecorderRatherThanWrapping checks
	// directly. This test is about the behaviour that actually matters:
	// whichever way it happens, does the fast path reach the real writer.
	fake := &countingReadFrom{}

	const payload = "everything below the s3 API's four logging and metrics middlewares"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// readOnly hides strings.Reader's own WriteTo method. io.Copy checks
		// the SOURCE for io.WriterTo before it ever checks the destination for
		// io.ReaderFrom, and strings.Reader — unlike the *os.File a real blob
		// read actually uses — implements one. Left exposed, this test would
		// take that path instead and prove nothing about the fix.
		if _, err := io.Copy(w, readOnly{strings.NewReader(payload)}); err != nil {
			t.Fatalf("handler copy: %v", err)
		}
	})

	var chain http.Handler = handler
	chain = WithAccessLog(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, chain)
	chain = WithMetrics(&countingCounter{}, chain)
	chain = WithLiveMetrics(&countingCounter{}, chain)
	chain = WithScrapeMetrics(&countingObserver{}, chain)

	request := httptest.NewRequest(http.MethodGet, "/b/k", nil)
	chain.ServeHTTP(fake, request)

	if fake.calls != 1 {
		t.Fatalf("the real writer's ReadFrom was called %d times through the full chain, want exactly 1 — "+
			"0 means sendfile visibility is broken again somewhere in the chain", fake.calls)
	}
	if fake.written != int64(len(payload)) {
		t.Errorf("the real writer received %d bytes, want %d", fake.written, len(payload))
	}
}

// countingCounter and countingObserver satisfy RequestCounter and
// RequestObserver without pulling in the real metrics package, and exist only
// so TestFourStackedMiddlewaresShareOneRecorder can wire up the real chain.
type countingCounter struct {
	status            int
	bytesIn, bytesOut int64
}

func (c *countingCounter) Record(status int, bytesIn, bytesOut int64) {
	c.status, c.bytesIn, c.bytesOut = status, bytesIn, bytesOut
}

type countingObserver struct{}

func (countingObserver) Observe(string, string, int, time.Duration, int64, int64) {}
