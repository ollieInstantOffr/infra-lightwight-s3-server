package console

import (
	"errors"
	"net/http"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/logs"
)

// handlePerformance answers the Performance page's non-live view: traffic,
// error rate, latency and the slowest operations for a chosen window.
func (s *Server) handlePerformance(w http.ResponseWriter, r *http.Request) {
	since, until, err := performanceWindow(r.URL.Query().Get("range"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()

	traffic, err := db.RequestsInWindow(ctx, s.DB, since, until)
	if err != nil {
		s.internalError(w, r, "read windowed traffic", err)
		return
	}

	policy := logs.DefaultPolicy()
	if s.Sink != nil {
		policy = s.Sink.Policy()
	}

	latency, err := db.Latency(ctx, s.DB, since, until, policy.SampleRate, int(policy.SlowThreshold.Milliseconds()))
	if err != nil {
		s.internalError(w, r, "read latency", err)
		return
	}

	slowest, err := db.SlowestOperations(ctx, s.DB, since, until, policy.SampleRate, 20)
	if err != nil {
		s.internalError(w, r, "read slowest operations", err)
		return
	}

	// How much of the requested window the sampled log can actually speak to.
	// request_metrics (traffic, above) outlives request_logs by a wide margin —
	// 90 days against roughly 3 for an ordinary success — so a 7-day window's
	// request count is exact while its latency panel may only be describing
	// the last day or two of it. Reported rather than silently shown as if it
	// covered the whole window.
	earliest, err := db.EarliestSample(ctx, s.DB)
	if err != nil {
		s.internalError(w, r, "read earliest sample", err)
		return
	}
	coveredSince := since
	partialCoverage := false
	if !earliest.IsZero() && earliest.After(since) {
		coveredSince = earliest
		partialCoverage = true
	}

	series := make([]map[string]any, 0, len(traffic.Series))
	for _, point := range traffic.Series {
		series = append(series, map[string]any{
			"at": point.At, "requests": point.Requests, "errors": point.Errors,
		})
	}

	operations := make([]map[string]any, 0, len(slowest))
	for _, op := range slowest {
		operations = append(operations, map[string]any{
			"operation":     op.Operation,
			"bucket":        op.Bucket,
			"callsEstimate": op.CallsEstimate,
			"p95Ms":         op.P95MS,
			"bytesEstimate": op.BytesEstimate,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"since": since, "until": until,
		"requests": traffic.Requests, "clientErrors": traffic.ClientErrors,
		"serverErrors": traffic.ServerErrors, "errorRate": traffic.ErrorRate(),
		"bytesIn": traffic.BytesIn, "bytesOut": traffic.BytesOut,
		"series": series,
		"latency": map[string]any{
			"p50Ms": latency.P50MS, "p90Ms": latency.P90MS, "p99Ms": latency.P99MS,
			"maxMs": latency.MaxMS, "overThreshold": latency.OverThreshold,
			"sampleRows": latency.SampleRows,
		},
		"slowThresholdMs":   policy.SlowThreshold.Milliseconds(),
		"sampleRate":        policy.SampleRate,
		"slowestOperations": operations,
		"coverage": map[string]any{
			"partial":      partialCoverage,
			"coveredSince": coveredSince,
		},
	})
}

// performanceWindow turns a range keyword into a since/until pair, capped at
// what the durable rollup can honestly answer.
func performanceWindow(rangeParam string) (since, until time.Time, err error) {
	until = time.Now()
	switch rangeParam {
	case "", "1h":
		since = until.Add(-time.Hour)
	case "24h":
		since = until.Add(-24 * time.Hour)
	case "7d":
		since = until.Add(-7 * 24 * time.Hour)
	default:
		return time.Time{}, time.Time{}, errors.New("range must be 1h, 24h or 7d.")
	}
	return since, until, nil
}
