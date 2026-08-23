// Package logs captures request logs and server events for the console,
// without putting a database write on the request path.
//
// The server has always produced this information; it went to stdout and was
// lost. What it lacked was somewhere queryable, which is why the console could
// report that a percentage of requests failed while being unable to say which
// ones or why.
//
// The same discipline as the metrics collector applies: entries accumulate in
// memory behind a mutex and are flushed in batches. A crash loses at most one
// flush interval, which is the right trade for a log that exists to be read by
// a person.
package logs

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// FlushInterval is how often buffered entries reach the database. Short enough
// that someone reproducing a problem sees it almost immediately.
const FlushInterval = 2 * time.Second

// maxBuffer bounds memory if the database is unreachable or a burst outpaces
// the flush. Beyond it entries are dropped and counted, because a logging
// subsystem that exhausts memory has failed far worse than one that loses
// lines.
const maxBuffer = 20000

// Policy decides which requests are worth keeping.
type Policy struct {
	// SampleRate is the fraction of successful requests retained, 0 to 1.
	// Failures ignore it entirely.
	SampleRate float64
	// SlowThreshold retains any request taking at least this long, whatever
	// its status. A slow success is often the more interesting event.
	SlowThreshold time.Duration
}

// DefaultPolicy keeps every failure and a thin slice of successes.
func DefaultPolicy() Policy {
	return Policy{SampleRate: 0.01, SlowThreshold: 3 * time.Second}
}

// Sink buffers entries and flushes them in batches.
type Sink struct {
	mu       sync.Mutex
	requests []db.RequestLog
	events   []db.ServerEvent
	policy   Policy
	node     string

	// dropped counts entries discarded because the buffer was full. Reported
	// rather than silently lost: a log with holes in it must say so.
	dropped int64
}

// New returns a sink for a node.
func New(node string, policy Policy) *Sink {
	if policy.SampleRate < 0 {
		policy.SampleRate = 0
	}
	if policy.SampleRate > 1 {
		policy.SampleRate = 1
	}
	return &Sink{policy: policy, node: node}
}

// SetPolicy adjusts sampling at runtime, so an operator can turn detail up
// while diagnosing and back down afterwards without a restart.
func (s *Sink) SetPolicy(policy Policy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policy = policy
}

// Policy returns the current sampling policy.
func (s *Sink) Policy() Policy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy
}

// RecordRequest offers a request to the log. Whether it is kept is decided
// here, so callers do not have to know the policy.
func (s *Sink) RecordRequest(entry db.RequestLog) {
	s.mu.Lock()
	defer s.mu.Unlock()

	failed := entry.Status >= 400
	slow := s.policy.SlowThreshold > 0 &&
		time.Duration(entry.DurationMS)*time.Millisecond >= s.policy.SlowThreshold

	switch {
	case failed, slow:
		// Always kept. These are what anyone is looking for.
		entry.Sampled = false
	case rand.Float64() < s.policy.SampleRate:
		// Kept as a sample of ordinary traffic, and marked as such so
		// retention can treat it differently.
		entry.Sampled = true
	default:
		return
	}

	if len(s.requests) >= maxBuffer {
		s.dropped++
		return
	}
	entry.Node = s.node
	s.requests = append(s.requests, entry)
}

// RecordEvent stores a server warning or error.
func (s *Sink) RecordEvent(event db.ServerEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.events) >= maxBuffer {
		s.dropped++
		return
	}
	event.Node = s.node
	s.events = append(s.events, event)
}

// drain removes and returns everything buffered.
func (s *Sink) drain() ([]db.RequestLog, []db.ServerEvent, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	requests, events, dropped := s.requests, s.events, s.dropped
	s.requests, s.events, s.dropped = nil, nil, 0
	return requests, events, dropped
}

// Flush writes buffered entries to the database.
//
// A failure discards rather than retries. Unlike metrics, where a lost flush
// leaves a visible hole in a graph, log lines are individually dispensable —
// and retrying would risk a growing backlog of stale entries competing with
// live ones for the same buffer.
func (s *Sink) Flush(ctx context.Context, pool *db.Pool, log *slog.Logger) {
	requests, events, dropped := s.drain()

	if dropped > 0 {
		// Reported to stdout rather than into the sink, which would recurse.
		log.Warn("log buffer full; entries dropped", "count", dropped)
	}
	if len(requests) > 0 {
		if err := db.InsertRequestLogs(ctx, pool, requests); err != nil {
			log.Warn("could not persist request logs", "count", len(requests), "error", err)
		}
	}
	if len(events) > 0 {
		if err := db.InsertServerEvents(ctx, pool, events); err != nil {
			log.Warn("could not persist server events", "count", len(events), "error", err)
		}
	}
}

// Run flushes on a ticker until ctx is cancelled, then flushes once more so a
// clean shutdown keeps the last few seconds.
func (s *Sink) Run(ctx context.Context, pool *db.Pool, log *slog.Logger) {
	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			s.Flush(final, pool, log)
			cancel()
			return
		case <-ticker.C:
			s.Flush(ctx, pool, log)
		}
	}
}
