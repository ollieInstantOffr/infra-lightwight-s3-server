package logs

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// buffered reports what the sink is currently holding, without flushing.
func buffered(t *testing.T, s *Sink) ([]db.RequestLog, []db.ServerEvent) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests, s.events
}

func TestRecordRequestKeepsEveryFailure(t *testing.T) {
	// SampleRate 0 means no successful request is kept, so anything in the
	// buffer afterwards got there by being a failure.
	sink := New("node-a", Policy{SampleRate: 0})

	for _, status := range []int{200, 204, 301, 399, 400, 403, 404, 500, 503} {
		sink.RecordRequest(db.RequestLog{Status: status})
	}

	requests, _ := buffered(t, sink)
	var kept []int
	for _, entry := range requests {
		kept = append(kept, entry.Status)
	}

	want := []int{400, 403, 404, 500, 503}
	if len(kept) != len(want) {
		t.Fatalf("kept %v, want %v", kept, want)
	}
	for i := range want {
		if kept[i] != want[i] {
			t.Fatalf("kept %v, want %v", kept, want)
		}
	}
	for _, entry := range requests {
		if entry.Sampled {
			t.Errorf("status %d marked sampled; failures are kept in full, not sampled", entry.Status)
		}
		if entry.Node != "node-a" {
			t.Errorf("node = %q, want node-a", entry.Node)
		}
	}
}

func TestRecordRequestKeepsSlowSuccesses(t *testing.T) {
	sink := New("node-a", Policy{SampleRate: 0, SlowThreshold: 3 * time.Second})

	sink.RecordRequest(db.RequestLog{Status: 200, DurationMS: 2999})
	sink.RecordRequest(db.RequestLog{Status: 200, DurationMS: 3000})
	sink.RecordRequest(db.RequestLog{Status: 200, DurationMS: 9000})

	requests, _ := buffered(t, sink)
	if len(requests) != 2 {
		t.Fatalf("kept %d slow requests, want 2 (3000ms and 9000ms)", len(requests))
	}
	if requests[0].DurationMS != 3000 || requests[1].DurationMS != 9000 {
		t.Fatalf("kept %dms and %dms, want 3000ms and 9000ms",
			requests[0].DurationMS, requests[1].DurationMS)
	}
	for _, entry := range requests {
		if entry.Sampled {
			t.Error("slow request marked sampled; it is kept on merit, not by sampling")
		}
	}
}

func TestRecordRequestZeroSlowThresholdDoesNotKeepEverything(t *testing.T) {
	// A zero threshold must mean "no slow rule", not "every request is slow" —
	// which is what a naive >= comparison against zero would produce.
	sink := New("node-a", Policy{SampleRate: 0, SlowThreshold: 0})

	sink.RecordRequest(db.RequestLog{Status: 200, DurationMS: 0})
	sink.RecordRequest(db.RequestLog{Status: 200, DurationMS: 5000})

	if requests, _ := buffered(t, sink); len(requests) != 0 {
		t.Fatalf("kept %d requests with sampling off and no slow threshold, want 0", len(requests))
	}
}

func TestRecordRequestSamplesSuccessesAndMarksThem(t *testing.T) {
	// SampleRate 1 keeps every success, which makes the marking observable
	// without depending on the random draw.
	sink := New("node-a", Policy{SampleRate: 1})

	sink.RecordRequest(db.RequestLog{Status: 200})
	sink.RecordRequest(db.RequestLog{Status: 500})

	requests, _ := buffered(t, sink)
	if len(requests) != 2 {
		t.Fatalf("kept %d, want 2", len(requests))
	}
	if !requests[0].Sampled {
		t.Error("success kept by sampling should be marked sampled, so retention can expire it early")
	}
	if requests[1].Sampled {
		t.Error("failure should never be marked sampled")
	}
}

func TestNewClampsSampleRate(t *testing.T) {
	if got := New("n", Policy{SampleRate: -0.5}).Policy().SampleRate; got != 0 {
		t.Errorf("negative rate clamped to %v, want 0", got)
	}
	if got := New("n", Policy{SampleRate: 7}).Policy().SampleRate; got != 1 {
		t.Errorf("rate above 1 clamped to %v, want 1", got)
	}
}

func TestBufferCapDropsAndCounts(t *testing.T) {
	sink := New("node-a", Policy{SampleRate: 1})

	for i := 0; i < maxBuffer+50; i++ {
		sink.RecordRequest(db.RequestLog{Status: 500})
	}

	requests, _ := buffered(t, sink)
	if len(requests) != maxBuffer {
		t.Fatalf("buffered %d, want the cap of %d", len(requests), maxBuffer)
	}

	_, _, dropped := sink.drain()
	if dropped != 50 {
		t.Errorf("dropped = %d, want 50; a log with holes has to be able to say so", dropped)
	}
}

func TestDrainEmptiesTheBuffer(t *testing.T) {
	sink := New("node-a", Policy{SampleRate: 1})
	sink.RecordRequest(db.RequestLog{Status: 500})
	sink.RecordEvent(db.ServerEvent{Level: "ERROR", Message: "boom"})

	requests, events, _ := sink.drain()
	if len(requests) != 1 || len(events) != 1 {
		t.Fatalf("drained %d requests and %d events, want 1 and 1", len(requests), len(events))
	}

	// A second drain must come back empty, or a failed flush would resend.
	requests, events, dropped := sink.drain()
	if len(requests) != 0 || len(events) != 0 || dropped != 0 {
		t.Fatalf("second drain returned %d requests, %d events, %d dropped; want all zero",
			len(requests), len(events), dropped)
	}
}

func TestSetPolicyTakesEffectWithoutRestart(t *testing.T) {
	sink := New("node-a", Policy{SampleRate: 0})
	sink.RecordRequest(db.RequestLog{Status: 200})
	if requests, _ := buffered(t, sink); len(requests) != 0 {
		t.Fatal("success kept while sampling was off")
	}

	sink.SetPolicy(Policy{SampleRate: 1})
	sink.RecordRequest(db.RequestLog{Status: 200})
	if requests, _ := buffered(t, sink); len(requests) != 1 {
		t.Fatal("turning sampling up did not take effect")
	}
}

func TestRecordEventStampsNode(t *testing.T) {
	sink := New("node-b", DefaultPolicy())
	sink.RecordEvent(db.ServerEvent{Level: "WARN", Message: "hello"})

	_, events := buffered(t, sink)
	if len(events) != 1 || events[0].Node != "node-b" {
		t.Fatalf("events = %+v, want one stamped node-b", events)
	}
}

func TestHandlerCapturesWarnAndAboveOnly(t *testing.T) {
	sink := New("node-a", DefaultPolicy())
	logger := slog.New(NewHandler(
		slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}),
		sink,
	))

	logger.Debug("debug")
	logger.Info("info")
	logger.Warn("warn", "key", "value")
	logger.Error("error")

	_, events := buffered(t, sink)
	if len(events) != 2 {
		t.Fatalf("captured %d events, want 2 (warn and error only)", len(events))
	}
	if events[0].Level != "WARN" || events[0].Message != "warn" {
		t.Errorf("first event = %s/%s, want WARN/warn", events[0].Level, events[0].Message)
	}
	if events[0].Attributes["key"] != "value" {
		t.Errorf("attributes = %v, want key=value carried through", events[0].Attributes)
	}
	if events[1].Level != "ERROR" {
		t.Errorf("second event level = %s, want ERROR", events[1].Level)
	}
	if events[0].At.IsZero() {
		t.Error("event timestamp is zero")
	}
}

func TestHandlerSkipsRequestScopedRecords(t *testing.T) {
	sink := New("node-a", DefaultPolicy())
	logger := slog.New(NewHandler(slog.NewTextHandler(io.Discard, nil), sink))

	// Passed on the record, as the access log and authenticator do.
	logger.Warn("request rejected", "status", 403, Skip())
	// Bound through With, as a per-request logger would.
	logger.With(Skip()).Error("request failed")
	// Bound through With, then grouped.
	logger.With(Skip()).WithGroup("req").Warn("still request scoped")

	if _, events := buffered(t, sink); len(events) != 0 {
		t.Fatalf("captured %d request-scoped events, want 0; they are already in the request log", len(events))
	}

	// The marker must not leak into unrelated loggers built from the same root.
	logger.Warn("genuine server problem")
	if _, events := buffered(t, sink); len(events) != 1 {
		t.Fatalf("captured %d unmarked events, want 1", len(events))
	}
}

func TestHandlerWithAttrsKeepsCapturing(t *testing.T) {
	sink := New("node-a", DefaultPolicy())
	logger := slog.New(NewHandler(slog.NewTextHandler(io.Discard, nil), sink))

	logger.With("component", "sweeper").Warn("could not reclaim blob")

	_, events := buffered(t, sink)
	if len(events) != 1 || events[0].Message != "could not reclaim blob" {
		t.Fatalf("events = %+v, want the sweeper warning captured", events)
	}
}

func TestHandlerEnabledDefersToInner(t *testing.T) {
	sink := New("node-a", DefaultPolicy())
	handler := NewHandler(
		slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}),
		sink,
	)

	if handler.Enabled(t.Context(), slog.LevelWarn) {
		t.Error("warn reported enabled while the inner handler is set to error")
	}
	if !handler.Enabled(t.Context(), slog.LevelError) {
		t.Error("error reported disabled while the inner handler is set to error")
	}
}
