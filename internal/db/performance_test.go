package db

import (
	"context"
	"testing"
	"time"
)

// The whole point of the weighted approach: request_logs keeps every failure
// and every slow request but only a thin slice of ordinary fast successes, so
// an unweighted percentile over it is biased toward "everything is slow" in a
// way that would actively mislead whoever is looking at it. These tests build
// a population with a known true distribution, sample it the way the sink
// actually does, and check the estimate lands close to the truth rather than
// close to the biased raw sample.

func seedLatencyPopulation(t *testing.T, pool *Pool, fastCount int, fastMS int, slowCount int, slowMS int, sampleRate float64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()

	var entries []RequestLog
	kept := 0
	// Every fast request is offered to the log; only a sampleRate fraction is
	// actually kept — deterministically here (every Nth one) rather than with
	// real randomness, so the test is not flaky.
	every := int(1 / sampleRate)
	for i := 0; i < fastCount; i++ {
		if i%every != 0 {
			continue
		}
		kept++
		entries = append(entries, RequestLog{
			At: now, RequestID: fmtID("fast", kept), Method: "GET", Bucket: "b",
			Path: "/b/k", Operation: "GetObject", Status: 200,
			DurationMS: fastMS, Surface: "s3", Sampled: true,
		})
	}
	// Every slow request is kept, none sampled away — this is what the sink
	// actually guarantees.
	for i := 0; i < slowCount; i++ {
		entries = append(entries, RequestLog{
			At: now, RequestID: fmtID("slow", i), Method: "GET", Bucket: "b",
			Path: "/b/k", Operation: "GetObject", Status: 200,
			DurationMS: slowMS, Surface: "s3", Sampled: false,
		})
	}
	if err := InsertRequestLogs(ctx, pool, entries); err != nil {
		t.Fatalf("InsertRequestLogs: %v", err)
	}
}

func fmtID(prefix string, n int) string {
	return prefix + "-" + time.Now().Format("150405.000000") + "-" + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func TestLatencyCorrectsForSamplingBias(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	sampleRate := 0.01

	// True population: 99,000 requests at 10ms, 1,000 at 2,000ms. True p50 is
	// 10ms (comfortably inside the fast 99%), true p99 sits right at the
	// boundary into the slow 1%.
	//
	// The sink's own rule kept in mind: every slow one survives, only 1% of the
	// fast ones do. So the raw table holds roughly 990 fast rows and 1,000 slow
	// rows — an unweighted percentile over that raw table would report a p50
	// near 2000ms, not 10ms.
	seedLatencyPopulation(t, pool, 99000, 10, 1000, 2000, sampleRate)

	since := time.Now().Add(-time.Hour)
	until := time.Now().Add(time.Hour)
	summary, err := Latency(ctx, pool, since, until, sampleRate, 500)
	if err != nil {
		t.Fatalf("Latency: %v", err)
	}

	if summary.P50MS != 10 {
		t.Errorf("p50 = %dms, want 10ms (99%% of true traffic is fast) — "+
			"an unweighted percentile over the raw sample would report ~2000ms instead", summary.P50MS)
	}
	if summary.P99MS < 10 {
		t.Errorf("p99 = %dms, want it to fall in the slow tail", summary.P99MS)
	}
	// Exact, because every slow request is always kept — this is not an
	// estimate and should not drift with the sample rate.
	if summary.OverThreshold != 1000 {
		t.Errorf("OverThreshold = %d, want exactly 1000 (never sampled away)", summary.OverThreshold)
	}
}

func TestSlowestOperationsCallsEstimateIsCloseToTruth(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	sampleRate := 0.01

	seedLatencyPopulation(t, pool, 50000, 5, 200, 900, sampleRate)

	since := time.Now().Add(-time.Hour)
	until := time.Now().Add(time.Hour)
	stats, err := SlowestOperations(ctx, pool, since, until, sampleRate, 20)
	if err != nil {
		t.Fatalf("SlowestOperations: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("got %d groups, want 1 (one operation, one bucket)", len(stats))
	}

	// True total is 50200. The raw row count kept is ~700 (500 sampled fast +
	// 200 always-kept slow); the weighted estimate should land near the truth,
	// not near the raw count.
	got := stats[0].CallsEstimate
	if got < 45000 || got > 55000 {
		t.Errorf("CallsEstimate = %d, want close to the true 50200 — "+
			"a raw unweighted count would be roughly 700, off by two orders of magnitude", got)
	}
	if stats[0].P95MS < 5 {
		t.Errorf("p95 = %dms, want it inside the slow tail (95th percentile of 50200 requests, 200 of which are 900ms)", stats[0].P95MS)
	}
}

func TestLatencyHandlesAZeroSampleRateWithoutDividingByZero(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if err := InsertRequestLogs(ctx, pool, []RequestLog{
		{At: time.Now(), RequestID: "z1", Method: "GET", Bucket: "b", Path: "/b/k",
			Operation: "GetObject", Status: 500, DurationMS: 30, Surface: "s3", Sampled: false},
	}); err != nil {
		t.Fatalf("InsertRequestLogs: %v", err)
	}

	summary, err := Latency(ctx, pool, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), 0, 500)
	if err != nil {
		t.Fatalf("Latency with a zero sample rate: %v", err)
	}
	if summary.P50MS != 30 {
		t.Errorf("p50 = %d, want 30 (the one deterministically-kept row)", summary.P50MS)
	}
}

func TestEarliestSampleReportsTheOldestS3Row(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if _, err := EarliestSample(ctx, pool); err != nil {
		t.Fatalf("EarliestSample on an empty table: %v", err)
	}

	old := time.Now().Add(-48 * time.Hour)
	if err := InsertRequestLogs(ctx, pool, []RequestLog{
		{At: old, RequestID: "old", Method: "GET", Path: "/b/k", Status: 200, Surface: "s3"},
		{At: time.Now(), RequestID: "new", Method: "GET", Path: "/b/k", Status: 200, Surface: "s3"},
		// Console traffic must not count toward the S3 sample horizon.
		{At: old.Add(-24 * time.Hour), RequestID: "console-old", Method: "GET", Path: "/api/x", Status: 200, Surface: "console"},
	}); err != nil {
		t.Fatalf("InsertRequestLogs: %v", err)
	}

	earliest, err := EarliestSample(ctx, pool)
	if err != nil {
		t.Fatalf("EarliestSample: %v", err)
	}
	if earliest.Sub(old).Abs() > time.Second {
		t.Errorf("earliest = %v, want close to %v (the oldest s3 row, not the older console one)", earliest, old)
	}
}

func TestRequestsInWindowIsExactUnlikeTheSampledQueries(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	hour := time.Now().UTC().Truncate(time.Hour)
	if err := FlushMetrics(ctx, pool, []MetricSample{
		{Hour: hour, StatusClass: 2, Requests: 100, BytesIn: 1000, BytesOut: 2000},
		{Hour: hour, StatusClass: 5, Requests: 3, BytesIn: 10, BytesOut: 0},
		{Hour: hour.Add(-3 * time.Hour), StatusClass: 2, Requests: 50, BytesIn: 500, BytesOut: 500},
	}); err != nil {
		t.Fatalf("FlushMetrics: %v", err)
	}

	result, err := RequestsInWindow(ctx, pool, hour.Add(-time.Hour), hour.Add(time.Hour))
	if err != nil {
		t.Fatalf("RequestsInWindow: %v", err)
	}
	if result.Requests != 103 {
		t.Errorf("Requests = %d, want 103 (the 3-hours-ago bucket is outside this window)", result.Requests)
	}
	if result.ServerErrors != 3 {
		t.Errorf("ServerErrors = %d, want 3", result.ServerErrors)
	}
	if len(result.Series) == 0 {
		t.Error("no series points; the chart would render nothing")
	}
}

func TestRequestsInWindowStepsDailyBeyondTheHourlyLimit(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	result, err := RequestsInWindow(ctx, pool, time.Now().Add(-7*24*time.Hour), time.Now())
	if err != nil {
		t.Fatalf("RequestsInWindow over 7 days: %v", err)
	}
	if len(result.Series) > 10 {
		t.Errorf("got %d series points for a 7-day window, want roughly 7-8 (daily buckets), not hourly", len(result.Series))
	}
}
