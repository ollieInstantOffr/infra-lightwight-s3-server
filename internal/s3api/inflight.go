package s3api

import (
	"sort"
	"sync"
	"time"
)

// InFlight tracks requests currently being handled — a count, and enough
// about each one to say what it is and how long it has been running.
//
// Deliberately not tracking bytes moved or a live rate: that needs a counting
// wrapper on every request body reader and response writer, a materially
// bigger change for a number that is nice-to-have rather than actionable
// (ILS-112). What this gives — "23 in flight, oldest is a 41-second
// UploadPart" — is cheap and still tells an operator whether something is
// stuck.
//
// Keyed by the request's own *requestInfo rather than a separate struct: that
// holder already carries operation, bucket and key, filled in live as routing
// and handling progress, so a snapshot here just reads whatever it currently
// knows rather than duplicating the bookkeeping.
//
// Process state, not the rollups: it disappears on restart and was never
// meant to be charted historically. Purely a live view.
type InFlight struct {
	mu    sync.Mutex
	items map[*requestInfo]time.Time // value: when the request started
}

// NewInFlight returns an empty tracker.
func NewInFlight() *InFlight {
	return &InFlight{items: make(map[*requestInfo]time.Time)}
}

// start registers a request as begun. The caller must call the returned func
// exactly once, when the request finishes — deferred immediately, the same
// pattern as an unlock, so an early return or a panic cannot leak an entry
// that then sits "in flight" forever.
func (f *InFlight) start(info *requestInfo) func() {
	f.mu.Lock()
	f.items[info] = time.Now()
	f.mu.Unlock()

	return func() {
		f.mu.Lock()
		delete(f.items, info)
		f.mu.Unlock()
	}
}

// RegisterForTest registers a synthetic in-flight entry, for tests in other
// packages (the console SSE endpoint's tests, notably) that need something to
// report without driving a real request through WithRequestInfo. Not used by
// any production code path — every real registration goes through start,
// reached only from the middleware.
func (f *InFlight) RegisterForTest(operation, bucket, key string) func() {
	return f.start(&requestInfo{operation: operation, bucket: bucket, key: key})
}

// InFlightEntry is a snapshot of one open request, ready for display.
type InFlightEntry struct {
	Operation string
	Bucket    string
	Key       string
	Age       time.Duration
}

// Snapshot returns every open request, oldest first — the ones most worth
// looking at come first, without the caller having to sort them again.
func (f *InFlight) Snapshot() []InFlightEntry {
	f.mu.Lock()
	// Copied out under the lock rather than read from while iterating, since
	// info.target() takes a second lock (the requestInfo's own) and holding
	// both at once for the whole iteration is how two independent locks
	// become a deadlock the day someone adds a third.
	items := make(map[*requestInfo]time.Time, len(f.items))
	for info, startedAt := range f.items {
		items[info] = startedAt
	}
	f.mu.Unlock()

	now := time.Now()
	out := make([]InFlightEntry, 0, len(items))
	for info, startedAt := range items {
		operation, bucket, key := info.target()
		out = append(out, InFlightEntry{
			Operation: operation, Bucket: bucket, Key: key, Age: now.Sub(startedAt),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Age > out[j].Age })
	return out
}

// Count is the same number Snapshot's length would give, without building the
// slice or taking the second round of per-item locks — the Live stream's
// per-second payload wants only the count on most ticks.
func (f *InFlight) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.items)
}
