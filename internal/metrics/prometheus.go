package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The scrapeable view of what this server is doing.
//
// Separate from Collector, which rolls counts into hourly cells for the
// console's chart. Prometheus wants the opposite shape: monotonic counters and
// current gauges, read whenever it happens to ask. Deriving one from the other
// would give the scraper a number that resets every hour, which is exactly the
// shape a counter must not have.
//
// The text format is emitted directly rather than through a client library.
// It is a few lines of printf, and the alternative is a dependency tree in a
// project that has otherwise stayed close to the standard library.

// Namespace prefixes every metric.
const Namespace = "pail"

// durationBuckets are the histogram boundaries, in seconds.
//
// Chosen around what this server actually does: most requests are a metadata
// lookup and a file read and land in single-digit milliseconds, while a large
// multipart completion is seconds. An average would hide both ends; the point
// of a histogram here is the tail.
var durationBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// requestKey labels one counted request.
//
// Cardinality is the trap with a metric like this, and it is bounded here by
// construction: surface is one of two, operation is drawn from a fixed list the
// router knows, and status is a class rather than a code. Roughly a hundred and
// fifty series at the very most, and it cannot grow with traffic. A bucket or
// key label would have been more useful and would eventually take the
// monitoring down rather than the storage.
type requestKey struct {
	surface   string
	operation string
	status    string
}

// Registry holds the counters a scrape reads.
type Registry struct {
	mu sync.Mutex

	requests  map[requestKey]int64
	durations map[requestKey]*histogram
	bytesIn   map[string]int64 // by surface
	bytesOut  map[string]int64

	startedAt time.Time
	version   string
}

type histogram struct {
	counts []uint64
	sum    float64
	total  uint64
}

func newHistogram() *histogram {
	return &histogram{counts: make([]uint64, len(durationBuckets))}
}

func (h *histogram) observe(seconds float64) {
	h.total++
	h.sum += seconds
	for i, boundary := range durationBuckets {
		if seconds <= boundary {
			h.counts[i]++
		}
	}
}

// NewRegistry returns an empty registry.
func NewRegistry(version string) *Registry {
	return &Registry{
		requests:  make(map[requestKey]int64),
		durations: make(map[requestKey]*histogram),
		bytesIn:   make(map[string]int64),
		bytesOut:  make(map[string]int64),
		startedAt: time.Now(),
		version:   version,
	}
}

// statusClass renders a status code as the class Prometheus should see.
func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}

// Observe records one completed request. Called from the request path, so it
// takes a mutex and increments and does nothing else.
func (r *Registry) Observe(surface, operation string, status int, duration time.Duration, bytesIn, bytesOut int64) {
	if operation == "" {
		operation = "Unknown"
	}
	key := requestKey{surface: surface, operation: operation, status: statusClass(status)}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.requests[key]++
	hist, ok := r.durations[key]
	if !ok {
		hist = newHistogram()
		r.durations[key] = hist
	}
	hist.observe(duration.Seconds())
	r.bytesIn[surface] += bytesIn
	r.bytesOut[surface] += bytesOut
}

// Snapshot is the state a scrape reads that this process does not own: storage
// totals, alert state, and anything else that lives in the database.
//
// Gathered per scrape rather than kept current, because the alternative is a
// background job maintaining numbers nobody may ever read.
type Snapshot struct {
	DiskFreeBytes  uint64
	DiskTotalBytes uint64
	Buckets        []BucketUsage
	AlertsFiring   map[string]int
	LogsDropped    int64
	DatabaseUp     bool
}

// BucketUsage is one bucket's stored footprint.
type BucketUsage struct {
	Name    string
	Objects int64
	Bytes   int64
}

// WriteTo renders the Prometheus text exposition format.
func (r *Registry) WriteTo(w io.Writer, snap Snapshot) error {
	r.mu.Lock()
	keys := make([]requestKey, 0, len(r.requests))
	for key := range r.requests {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].surface != keys[j].surface {
			return keys[i].surface < keys[j].surface
		}
		if keys[i].operation != keys[j].operation {
			return keys[i].operation < keys[j].operation
		}
		return keys[i].status < keys[j].status
	})
	requests := make(map[requestKey]int64, len(r.requests))
	durations := make(map[requestKey]histogram, len(r.durations))
	for key, count := range r.requests {
		requests[key] = count
	}
	for key, hist := range r.durations {
		durations[key] = histogram{counts: append([]uint64(nil), hist.counts...), sum: hist.sum, total: hist.total}
	}
	bytesIn := maps(r.bytesIn)
	bytesOut := maps(r.bytesOut)
	started := r.startedAt
	version := r.version
	r.mu.Unlock()

	out := &writer{w: w}

	out.help("build_info", "Build information, as a constant 1 with the version in a label.", "gauge")
	out.line(`build_info{version=%q} 1`, version)

	out.help("uptime_seconds", "How long this process has been running.", "gauge")
	out.line("uptime_seconds %g", time.Since(started).Seconds())

	out.help("up_database", "Whether the metadata store answered on this scrape.", "gauge")
	out.line("up_database %d", boolValue(snap.DatabaseUp))

	out.help("requests_total", "Requests served, by surface, operation and status class.", "counter")
	for _, key := range keys {
		out.line(`requests_total{surface=%q,operation=%q,status=%q} %d`,
			key.surface, key.operation, key.status, requests[key])
	}

	out.help("request_duration_seconds", "Request duration, by surface and operation.", "histogram")
	for _, key := range keys {
		hist := durations[key]
		cumulative := uint64(0)
		for i, boundary := range durationBuckets {
			cumulative = hist.counts[i]
			out.line(`request_duration_seconds_bucket{surface=%q,operation=%q,le="%s"} %d`,
				key.surface, key.operation, formatBoundary(boundary), cumulative)
		}
		out.line(`request_duration_seconds_bucket{surface=%q,operation=%q,le="+Inf"} %d`,
			key.surface, key.operation, hist.total)
		out.line(`request_duration_seconds_sum{surface=%q,operation=%q} %g`,
			key.surface, key.operation, hist.sum)
		out.line(`request_duration_seconds_count{surface=%q,operation=%q} %d`,
			key.surface, key.operation, hist.total)
	}

	out.help("received_bytes_total", "Bytes read from clients.", "counter")
	for _, surface := range sortedKeys(bytesIn) {
		out.line(`received_bytes_total{surface=%q} %d`, surface, bytesIn[surface])
	}
	out.help("sent_bytes_total", "Bytes written to clients.", "counter")
	for _, surface := range sortedKeys(bytesOut) {
		out.line(`sent_bytes_total{surface=%q} %d`, surface, bytesOut[surface])
	}

	out.help("disk_free_bytes", "Free space on the volume holding object data.", "gauge")
	out.line("disk_free_bytes %d", snap.DiskFreeBytes)
	out.help("disk_total_bytes", "Total size of the volume holding object data.", "gauge")
	out.line("disk_total_bytes %d", snap.DiskTotalBytes)

	// Per bucket rather than per object: one series per bucket is a number an
	// operator chooses, while one per object is a number their clients choose.
	out.help("bucket_objects", "Objects stored, by bucket.", "gauge")
	for _, bucket := range snap.Buckets {
		out.line(`bucket_objects{bucket=%q} %d`, bucket.Name, bucket.Objects)
	}
	out.help("bucket_bytes", "Bytes stored, by bucket.", "gauge")
	for _, bucket := range snap.Buckets {
		out.line(`bucket_bytes{bucket=%q} %d`, bucket.Name, bucket.Bytes)
	}

	out.help("alerts_firing", "Alerts currently firing, by rule.", "gauge")
	for _, rule := range sortedIntKeys(snap.AlertsFiring) {
		out.line(`alerts_firing{rule=%q} %d`, rule, snap.AlertsFiring[rule])
	}

	// Surfaced because a log with holes in it has to say so, and until now the
	// only sign was a warning on stdout that nothing scraped.
	out.help("log_entries_dropped_total", "Log entries discarded because the buffer was full.", "counter")
	out.line("log_entries_dropped_total %d", snap.LogsDropped)

	return out.err
}

// writer accumulates the first error rather than checking every line.
type writer struct {
	w   io.Writer
	err error
}

func (o *writer) help(name, help, kind string) {
	o.line("# HELP %s_%s %s", Namespace, name, help)
	o.line("# TYPE %s_%s %s", Namespace, name, kind)
}

func (o *writer) line(format string, args ...any) {
	if o.err != nil {
		return
	}
	text := fmt.Sprintf(format, args...)
	if !strings.HasPrefix(text, "#") {
		text = Namespace + "_" + text
	}
	_, o.err = io.WriteString(o.w, text+"\n")
}

// formatBoundary renders a bucket boundary the way Prometheus expects: no
// trailing zeroes, and never in exponent form.
func formatBoundary(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func boolValue(b bool) int {
	if b {
		return 1
	}
	return 0
}

func maps(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func sortedKeys(in map[string]int64) []string {
	out := make([]string, 0, len(in))
	for k := range in {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedIntKeys(in map[string]int) []string {
	out := make([]string, 0, len(in))
	for k := range in {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
