package console

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// The log stream goes through the same middleware as every other console
// route, and that middleware wraps the response writer to record its status.
// A wrapper that is not a Flusher fails a direct type assertion, so the stream
// answered 500 and the live tail silently showed nothing while the log itself
// filled up normally. These tests hold the stream to the wrapped handler.

func TestLogStreamOpens(t *testing.T) {
	console := newConsole(t)
	console.signIn(t, "admin@example.com")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, console.url+"/api/logs/stream", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	response, err := console.client.Do(request)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200; the live tail is dead if this fails", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	// Asked for explicitly so a buffering reverse proxy does not hold the
	// stream until a buffer fills.
	if got := response.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
}

func TestLogStreamDeliversNewEntries(t *testing.T) {
	console := newConsole(t)
	console.signIn(t, "admin@example.com")

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, console.url+"/api/logs/stream", nil)
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

	// Written after the stream is open, so it can only arrive by being pushed.
	entry := db.RequestLog{
		At:        time.Now(),
		RequestID: "stream-probe",
		Surface:   "s3",
		Method:    http.MethodGet,
		Bucket:    "stream-probe-bucket",
		Key:       "object.txt",
		Path:      "/stream-probe-bucket/object.txt",
		Status:    403,
		ErrorCode: "AccessDenied",
		Reason:    "request is not signed",
	}
	// A moment's grace so the handler has recorded the current newest id
	// before the row it must notice exists.
	time.Sleep(500 * time.Millisecond)
	if err := db.InsertRequestLogs(ctx, console.pool, []db.RequestLog{entry}); err != nil {
		t.Fatalf("InsertRequestLogs: %v", err)
	}

	found := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") && strings.Contains(line, "stream-probe-bucket") {
				found <- line
				return
			}
		}
	}()

	select {
	case line := <-found:
		for _, want := range []string{"stream-probe", "AccessDenied", "request is not signed"} {
			if !strings.Contains(line, want) {
				t.Errorf("streamed entry is missing %q: %s", want, line)
			}
		}
	case <-ctx.Done():
		t.Fatal("the entry never arrived on the stream")
	}
}
