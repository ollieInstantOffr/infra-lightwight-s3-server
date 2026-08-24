package s3api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestInFlightTracksStartAndStop(t *testing.T) {
	tracker := NewInFlight()
	if tracker.Count() != 0 {
		t.Fatalf("Count = %d on an empty tracker, want 0", tracker.Count())
	}

	info := &requestInfo{}
	stop := tracker.start(info)
	if tracker.Count() != 1 {
		t.Fatalf("Count = %d after start, want 1", tracker.Count())
	}

	stop()
	if tracker.Count() != 0 {
		t.Fatalf("Count = %d after stop, want 0 — an entry that outlives its request would sit \"in flight\" forever", tracker.Count())
	}
}

func TestInFlightSnapshotReadsLiveState(t *testing.T) {
	// The whole reason InFlight is keyed by *requestInfo rather than a copy:
	// a request registers before routing knows what it is, and the operation
	// and bucket fill in afterward on the same object. The snapshot has to see
	// that update, not the blank state from registration time.
	tracker := NewInFlight()
	info := &requestInfo{}
	stop := tracker.start(info)
	defer stop()

	before := tracker.Snapshot()
	if len(before) != 1 || before[0].Operation != "" {
		t.Fatalf("snapshot before routing = %+v, want one blank entry", before)
	}

	ctx := context.WithValue(context.Background(), requestInfoKey{}, info)
	noteOperation(ctx, "GetObject")
	noteTarget(ctx, "assets-prod", "logo.png")

	after := tracker.Snapshot()
	if len(after) != 1 {
		t.Fatalf("got %d entries, want 1", len(after))
	}
	if after[0].Operation != "GetObject" || after[0].Bucket != "assets-prod" || after[0].Key != "logo.png" {
		t.Errorf("snapshot did not see the live update: %+v", after[0])
	}
}

func TestInFlightSnapshotOrdersOldestFirst(t *testing.T) {
	tracker := NewInFlight()

	oldest := &requestInfo{}
	stopOldest := tracker.start(oldest)
	defer stopOldest()
	time.Sleep(5 * time.Millisecond)

	newest := &requestInfo{}
	stopNewest := tracker.start(newest)
	defer stopNewest()

	entries := tracker.Snapshot()
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Age < entries[1].Age {
		t.Errorf("entries not oldest-first: ages %v then %v", entries[0].Age, entries[1].Age)
	}
}

func TestWithRequestInfoRegistersAndDeregisters(t *testing.T) {
	tracker := NewInFlight()

	handler := WithRequestInfo(tracker, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tracker.Count() != 1 {
			t.Errorf("Count during the request = %d, want 1", tracker.Count())
		}
	}))

	request := httptest.NewRequest(http.MethodGet, "/b/k", nil)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	if tracker.Count() != 0 {
		t.Errorf("Count after the request completed = %d, want 0", tracker.Count())
	}
}

func TestWithRequestInfoToleratesANilTracker(t *testing.T) {
	// Every other optional dependency on Server is nil in most tests; this one
	// must be exactly as tolerant, or every fixture in the package needs
	// updating for a feature most of them do not exercise.
	handler := WithRequestInfo(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	request := httptest.NewRequest(http.MethodGet, "/b/k", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}
