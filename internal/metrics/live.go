package metrics

import (
	"sync"
	"time"
)

// LiveWindow is a per-second view of the last minute, for the Performance
// page's Live mode.
//
// Separate from both Collector (hourly cells, flushed to the durable rollup)
// and Registry (the Prometheus scrape, which accumulates since the process
// started and is never meant to be read as "right now"). A second's
// resolution has no business anywhere near either: it would either pollute an
// hourly rollup with a bucketing scheme nothing else uses, or make the scrape
// registry's counters jitter in a way a monotonic counter should never do.
// This is deliberately throwaway state — a ring of 60 buckets that overwrites
// itself, gone on restart, which is exactly right for a view whose only claim
// is "what is happening in the last minute".
type LiveWindow struct {
	mu      sync.Mutex
	buckets [windowSeconds]secondBucket
	// now is injectable so tests do not depend on wall-clock timing.
	now func() time.Time
}

// windowSeconds is how many seconds of history the ring keeps. Sixty matches
// the "last 60 seconds" the design draws; there is no reason it needs to be
// exactly that, but it is what the page asks for.
const windowSeconds = 60

type secondBucket struct {
	// second identifies which wall-clock second this bucket holds, as a Unix
	// timestamp. Used to tell "this bucket is this second's data" from "this
	// bucket is 60+ seconds stale and needs clearing before use" — the ring
	// reuses slots by index, and without this a bucket from a minute ago would
	// silently be read as current.
	second            int64
	requests          int64
	bytesIn, bytesOut int64
}

// NewLiveWindow returns an empty window.
func NewLiveWindow() *LiveWindow {
	return &LiveWindow{now: time.Now}
}

// Record adds one completed request to the current second's bucket. Called
// from the request path, so it does the least possible: a mutex and an
// increment, same as the hourly Collector it sits alongside.
func (w *LiveWindow) Record(status int, bytesIn, bytesOut int64) {
	now := w.now().Unix()
	index := now % windowSeconds

	w.mu.Lock()
	defer w.mu.Unlock()
	bucket := &w.buckets[index]
	if bucket.second != now {
		// Either genuinely empty, or a slot from a minute-plus ago being
		// reused — either way, this second starts fresh.
		*bucket = secondBucket{second: now}
	}
	bucket.requests++
	bucket.bytesIn += bytesIn
	bucket.bytesOut += bytesOut
}

// Point is one second of the live series.
type Point struct {
	At       time.Time
	Requests int64
	BytesIn  int64
	BytesOut int64
}

// Snapshot returns the last windowSeconds of history, oldest first. A second
// with no traffic is reported as zero rather than omitted, so a client
// drawing a fixed-width chart never has to fill a gap itself.
func (w *LiveWindow) Snapshot() []Point {
	now := w.now().Unix()

	w.mu.Lock()
	defer w.mu.Unlock()

	points := make([]Point, windowSeconds)
	for i := range points {
		// Walking backward from now rather than forward through the ring by
		// index, so "oldest first" holds regardless of where in the ring the
		// current second happens to land.
		second := now - int64(windowSeconds-1-i)
		bucket := w.buckets[second%windowSeconds]
		points[i].At = time.Unix(second, 0).UTC()
		if bucket.second == second {
			points[i].Requests = bucket.requests
			points[i].BytesIn = bucket.bytesIn
			points[i].BytesOut = bucket.bytesOut
		}
	}
	return points
}
