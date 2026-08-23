package metrics

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// render returns the exposition text for a registry.
func render(t *testing.T, r *Registry, snap Snapshot) string {
	t.Helper()
	var out strings.Builder
	if err := r.WriteTo(&out, snap); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return out.String()
}

// The exposition format is unforgiving and a scraper's error message is
// usually just "parse error". These check the shape a scraper actually needs.

func TestExpositionFormatIsWellFormed(t *testing.T) {
	registry := NewRegistry("1.2.3")
	registry.Observe("s3", "GetObject", 200, 5*time.Millisecond, 0, 1024)

	text := render(t, registry, Snapshot{DatabaseUp: true})

	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" {
			t.Error("blank line in the exposition output")
			continue
		}
		if strings.HasPrefix(line, "#") {
			// HELP and TYPE lines name a metric that must carry the namespace.
			fields := strings.Fields(line)
			if len(fields) < 3 {
				t.Errorf("malformed comment line: %q", line)
				continue
			}
			if !strings.HasPrefix(fields[2], Namespace+"_") {
				t.Errorf("metric %q is missing the %q namespace", fields[2], Namespace)
			}
			continue
		}
		if !strings.HasPrefix(line, Namespace+"_") {
			t.Errorf("sample line is missing the namespace: %q", line)
		}
		// Every sample must end in a parseable value.
		fields := strings.Fields(line)
		value := fields[len(fields)-1]
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			t.Errorf("line %q does not end in a number: %v", line, err)
		}
	}
}

func TestCountersAreMonotonicAcrossScrapes(t *testing.T) {
	// The property that separates this from the console's hourly rollup. A
	// counter that fell back would be read as a process restart, and every
	// rate() over it would be wrong.
	registry := NewRegistry("test")
	registry.Observe("s3", "PutObject", 200, time.Millisecond, 10, 0)

	first := valueOf(t, render(t, registry, Snapshot{}), `pail_requests_total{surface="s3",operation="PutObject",status="2xx"}`)
	if first != 1 {
		t.Fatalf("first scrape = %v, want 1", first)
	}

	registry.Observe("s3", "PutObject", 200, time.Millisecond, 10, 0)
	second := valueOf(t, render(t, registry, Snapshot{}), `pail_requests_total{surface="s3",operation="PutObject",status="2xx"}`)
	if second != 2 {
		t.Fatalf("second scrape = %v, want 2; a scrape must not reset the counter", second)
	}
}

func TestStatusIsGroupedIntoClasses(t *testing.T) {
	// Per-code labels would multiply the series count for no benefit — nobody
	// alerts on 404 versus 403 in aggregate, they read the log for that.
	registry := NewRegistry("test")
	for _, status := range []int{200, 204, 301, 403, 404, 500, 503} {
		registry.Observe("s3", "GetObject", status, time.Millisecond, 0, 0)
	}

	text := render(t, registry, Snapshot{})
	for class, want := range map[string]float64{"2xx": 2, "3xx": 1, "4xx": 2, "5xx": 2} {
		got := valueOf(t, text, `pail_requests_total{surface="s3",operation="GetObject",status="`+class+`"}`)
		if got != want {
			t.Errorf("status %s = %v, want %v", class, got, want)
		}
	}
}

func TestHistogramBucketsAreCumulativeAndEndInInf(t *testing.T) {
	// A histogram whose buckets are not cumulative parses fine and produces
	// quantiles that are silently wrong, which is the worst kind of broken.
	registry := NewRegistry("test")
	for _, d := range []time.Duration{time.Millisecond, 20 * time.Millisecond, 2 * time.Second} {
		registry.Observe("s3", "GetObject", 200, d, 0, 0)
	}

	text := render(t, registry, Snapshot{})

	var previous float64
	var sawInf bool
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "pail_request_duration_seconds_bucket") {
			continue
		}
		value := valueOfLine(t, line)
		if strings.Contains(line, `le="+Inf"`) {
			sawInf = true
			if value != 3 {
				t.Errorf("+Inf bucket = %v, want every observation (3)", value)
			}
		}
		if value < previous {
			t.Errorf("buckets are not cumulative: %q fell to %v from %v", line, value, previous)
		}
		previous = value
	}
	if !sawInf {
		t.Error("no +Inf bucket; a histogram without one is not a histogram")
	}

	if got := valueOf(t, text, `pail_request_duration_seconds_count{surface="s3",operation="GetObject"}`); got != 3 {
		t.Errorf("count = %v, want 3", got)
	}
}

func TestBucketGaugesAreLabelledByName(t *testing.T) {
	registry := NewRegistry("test")
	text := render(t, registry, Snapshot{
		DatabaseUp: true,
		Buckets: []BucketUsage{
			{Name: "assets", Objects: 12, Bytes: 3456},
		},
		AlertsFiring: map[string]int{"error_rate": 1},
		LogsDropped:  7,
	})

	if got := valueOf(t, text, `pail_bucket_objects{bucket="assets"}`); got != 12 {
		t.Errorf("objects = %v, want 12", got)
	}
	if got := valueOf(t, text, `pail_bucket_bytes{bucket="assets"}`); got != 3456 {
		t.Errorf("bytes = %v, want 3456", got)
	}
	if got := valueOf(t, text, `pail_alerts_firing{rule="error_rate"}`); got != 1 {
		t.Errorf("alerts = %v, want 1", got)
	}
	if got := valueOf(t, text, "pail_log_entries_dropped_total"); got != 7 {
		t.Errorf("dropped = %v, want 7", got)
	}
	if got := valueOf(t, text, "pail_up_database"); got != 1 {
		t.Errorf("up_database = %v, want 1", got)
	}
}

func TestDatabaseDownIsReportedRatherThanFailing(t *testing.T) {
	// The moment someone most wants their monitoring to say something is the
	// moment the database is unreachable, so a scrape then must still answer.
	registry := NewRegistry("test")
	registry.Observe("s3", "GetObject", 500, time.Millisecond, 0, 0)

	text := render(t, registry, Snapshot{DatabaseUp: false})

	if got := valueOf(t, text, "pail_up_database"); got != 0 {
		t.Errorf("up_database = %v, want 0", got)
	}
	if got := valueOf(t, text, `pail_requests_total{surface="s3",operation="GetObject",status="5xx"}`); got != 1 {
		t.Errorf("request counters were lost when the database was down: %v", got)
	}
}

func TestUnroutedRequestsAreCountedNotDropped(t *testing.T) {
	// A request rejected before routing — a bad signature, a denied scope — has
	// no operation. Those are exactly the requests someone watching an error
	// rate wants to see, so they must not vanish.
	registry := NewRegistry("test")
	registry.Observe("s3", "", 403, time.Millisecond, 0, 0)

	text := render(t, registry, Snapshot{})
	if got := valueOf(t, text, `pail_requests_total{surface="s3",operation="Unknown",status="4xx"}`); got != 1 {
		t.Errorf("an unrouted request was not counted: %v", got)
	}
}

// valueOf finds a sample by its full name-and-labels prefix.
func valueOf(t *testing.T, text, sample string) float64 {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, sample+" ") {
			return valueOfLine(t, line)
		}
	}
	t.Fatalf("no sample %q in:\n%s", sample, text)
	return 0
}

func valueOfLine(t *testing.T, line string) float64 {
	t.Helper()
	fields := strings.Fields(line)
	value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
	if err != nil {
		t.Fatalf("line %q: %v", line, err)
	}
	return value
}
