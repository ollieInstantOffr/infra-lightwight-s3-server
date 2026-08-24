package console

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The log stream once answered 500 because the response writer it needed to
// flush through was wrapped by middleware that is not itself a Flusher — a
// bug invisible to any test that did not open the stream through the real
// handler chain. This stream uses the identical mechanics, so it gets the
// identical test: open it for real and check it actually streams.

func TestPerformanceLiveOpens(t *testing.T) {
	console := newConsole(t)
	console.signIn(t, "admin@example.com")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, console.url+"/api/performance/live", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	response, err := console.client.Do(request)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := response.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
}

func TestPerformanceLiveDeliversASnapshotEverySecond(t *testing.T) {
	console := newConsole(t)
	console.signIn(t, "admin@example.com")

	console.server.Live.Record(200, 500, 1500)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, console.url+"/api/performance/live", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	response, err := console.client.Do(request)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer response.Body.Close()

	found := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				found <- line
				return
			}
		}
	}()

	select {
	case line := <-found:
		if !strings.Contains(line, `"series"`) || !strings.Contains(line, `"inFlightCount"`) {
			t.Errorf("payload is missing expected fields: %s", line)
		}
	case <-ctx.Done():
		t.Fatal("no snapshot arrived on the stream")
	}
}

func TestPerformanceLiveReportsInFlightRequests(t *testing.T) {
	console := newConsole(t)
	console.signIn(t, "admin@example.com")

	// A held-open S3 request, so the console's InFlight tracker — the same one
	// the s3api.Server would register into in production — has something to
	// report.
	stop := console.server.InFlight.RegisterForTest("GetObject", "assets-prod", "big.bin")
	defer stop()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, console.url+"/api/performance/live", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	response, err := console.client.Do(request)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer response.Body.Close()

	found := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "GetObject") {
				found <- line
				return
			}
		}
	}()

	select {
	case line := <-found:
		if !strings.Contains(line, `"inFlightCount":1`) {
			t.Errorf("payload did not report the open request as in flight: %s", line)
		}
	case <-ctx.Done():
		t.Fatal("the in-flight request never appeared on the stream")
	}
}

func TestPerformanceLiveWithoutAWindowReportsNotImplemented(t *testing.T) {
	console := newConsole(t)
	console.signIn(t, "admin@example.com")
	console.server.Live = nil

	status, body := console.do(t, "GET", "/api/performance/live", nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; a build with no live window should say so rather than error opaquely", status)
	}
	if body["error"] == nil {
		t.Error("no error message explaining why live mode is unavailable")
	}
}
