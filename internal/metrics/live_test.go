package metrics

import (
	"testing"
	"time"
)

// clockAt returns a func() time.Time frozen (or advanceable) for tests, so the
// ring's second-bucketing can be exercised deterministically rather than
// racing the real clock.
func clockAt(start time.Time) (now func() time.Time, advance func(time.Duration)) {
	current := start
	return func() time.Time { return current },
		func(d time.Duration) { current = current.Add(d) }
}

func TestLiveWindowRecordsIntoTheCurrentSecond(t *testing.T) {
	window := NewLiveWindow()
	now, _ := clockAt(time.Unix(1_700_000_000, 0))
	window.now = now

	window.Record(200, 100, 200)
	window.Record(200, 50, 60)

	points := window.Snapshot()
	if len(points) != windowSeconds {
		t.Fatalf("got %d points, want %d — a fixed-width chart cannot handle a variable length", len(points), windowSeconds)
	}
	last := points[len(points)-1]
	if last.Requests != 2 || last.BytesIn != 150 || last.BytesOut != 260 {
		t.Errorf("current second = %+v, want 2 requests, 150 in, 260 out", last)
	}
}

func TestLiveWindowFillsQuietSecondsWithZero(t *testing.T) {
	window := NewLiveWindow()
	now, _ := clockAt(time.Unix(1_700_000_000, 0))
	window.now = now

	window.Record(200, 10, 10)

	points := window.Snapshot()
	for i := 0; i < len(points)-1; i++ {
		if points[i].Requests != 0 {
			t.Errorf("point %d has %d requests, want 0 (only the current second has traffic)", i, points[i].Requests)
		}
	}
}

func TestLiveWindowRingReuseDoesNotLeakAMinuteOldValue(t *testing.T) {
	// The bug this guards: the ring reuses slots by second % windowSeconds, so
	// slot 5 holds second 5, then second 65, then second 125. Without the
	// stored second-identity check, reading slot 5 at second 125 would show
	// whatever second 65 left behind — a value from a minute ago presented as
	// current.
	window := NewLiveWindow()
	now, advance := clockAt(time.Unix(1_700_000_005, 0)) // lands in slot 5
	window.now = now
	window.Record(200, 999, 999)

	advance(windowSeconds * time.Second) // back to slot 5, one full lap later
	points := window.Snapshot()

	for i, p := range points {
		if p.BytesIn == 999 {
			t.Fatalf("point %d still shows the value from a minute ago: %+v", i, p)
		}
	}
}

func TestLiveWindowSnapshotIsOldestFirst(t *testing.T) {
	window := NewLiveWindow()
	now, advance := clockAt(time.Unix(1_700_000_000, 0))
	window.now = now

	window.Record(200, 1, 0) // second 0
	advance(10 * time.Second)
	window.Record(200, 2, 0) // second 10

	points := window.Snapshot()
	var firstNonZero, lastNonZero time.Time
	for _, p := range points {
		if p.BytesIn != 0 {
			if firstNonZero.IsZero() {
				firstNonZero = p.At
			}
			lastNonZero = p.At
		}
	}
	if !firstNonZero.Before(lastNonZero) {
		t.Errorf("the earlier recording (second 0) did not sort before the later one (second 10)")
	}
	if points[len(points)-1].At.Unix() != 1_700_000_010 {
		t.Errorf("last point is %v, want the current second (1_700_000_010)", points[len(points)-1].At)
	}
}

func TestLiveWindowIsConcurrencySafe(t *testing.T) {
	window := NewLiveWindow()
	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func() {
			window.Record(200, 1, 1)
			window.Snapshot()
			done <- struct{}{}
		}()
	}
	for i := 0; i < 50; i++ {
		<-done
	}
}
