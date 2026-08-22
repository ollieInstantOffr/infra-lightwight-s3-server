// Package metrics counts S3 API requests without putting a database write on
// the request path.
//
// The overview screen shows a 24-hour request count, an error rate and a
// fourteen-day chart. Recording a row per request would make every object GET
// — the hottest path in the system, and the one that should stay closest to a
// plain file read — into a database transaction. Counts are therefore
// accumulated in memory under a mutex and flushed periodically. A crash loses
// at most one flush interval of counts, which is the right trade for a graph.
package metrics

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// FlushInterval is how often accumulated counts reach the database.
const FlushInterval = time.Minute

// bucketKey identifies one rollup cell: an hour and a status class.
type bucketKey struct {
	hour        time.Time
	statusClass int
}

type counts struct {
	requests int64
	bytesIn  int64
	bytesOut int64
}

// Collector accumulates request counts.
type Collector struct {
	mu      sync.Mutex
	pending map[bucketKey]*counts

	// now is injectable so tests do not have to wait for the clock.
	now func() time.Time
}

// New returns an empty collector.
func New() *Collector {
	return &Collector{pending: make(map[bucketKey]*counts), now: time.Now}
}

// Record adds one request to the counts. It is called from the request path, so
// it does nothing but take a mutex and increment.
func (c *Collector) Record(status int, bytesIn, bytesOut int64) {
	class := status / 100
	if class < 1 || class > 5 {
		class = 5
	}
	key := bucketKey{hour: c.now().UTC().Truncate(time.Hour), statusClass: class}

	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.pending[key]
	if !ok {
		entry = &counts{}
		c.pending[key] = entry
	}
	entry.requests++
	entry.bytesIn += bytesIn
	entry.bytesOut += bytesOut
}

// drain removes and returns the accumulated counts.
func (c *Collector) drain() []db.MetricSample {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[bucketKey]*counts)
	c.mu.Unlock()

	samples := make([]db.MetricSample, 0, len(pending))
	for key, entry := range pending {
		samples = append(samples, db.MetricSample{
			Hour:        key.hour,
			StatusClass: key.statusClass,
			Requests:    entry.requests,
			BytesIn:     entry.bytesIn,
			BytesOut:    entry.bytesOut,
		})
	}
	return samples
}

// restore puts counts back after a failed flush, so a database blip loses
// nothing more than it has to.
func (c *Collector) restore(samples []db.MetricSample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, sample := range samples {
		key := bucketKey{hour: sample.Hour, statusClass: sample.StatusClass}
		entry, ok := c.pending[key]
		if !ok {
			entry = &counts{}
			c.pending[key] = entry
		}
		entry.requests += sample.Requests
		entry.bytesIn += sample.BytesIn
		entry.bytesOut += sample.BytesOut
	}
}

// Flush writes accumulated counts to the database.
func (c *Collector) Flush(ctx context.Context, pool *db.Pool) error {
	samples := c.drain()
	if len(samples) == 0 {
		return nil
	}
	if err := db.FlushMetrics(ctx, pool, samples); err != nil {
		c.restore(samples)
		return err
	}
	return nil
}

// Run flushes on a ticker until ctx is cancelled, then flushes once more so a
// clean shutdown does not discard the final minute.
func (c *Collector) Run(ctx context.Context, pool *db.Pool, log *slog.Logger) {
	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// A fresh context: the request context is already cancelled, and
			// the final flush still needs to reach the database.
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := c.Flush(final, pool); err != nil {
				log.Warn("could not flush final request metrics", "error", err)
			}
			cancel()
			return
		case <-ticker.C:
			if err := c.Flush(ctx, pool); err != nil {
				log.Warn("could not flush request metrics", "error", err)
			}
		}
	}
}
