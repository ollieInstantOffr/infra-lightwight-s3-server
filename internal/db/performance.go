package db

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// The model behind the Performance page: traffic and error rate for an
// arbitrary window, latency percentiles, and which operations are slow.
//
// Traffic comes from request_metrics — the durable hourly rollup, 90 days of
// retention, exact because every request is counted. Latency and slowest
// operations come from request_logs — the sampled log, which keeps every
// failure and every slow request but only a thin slice (the configured
// SampleRate) of ordinary fast successes. A naive percentile or count over
// that table is wrong in a specific and misleading direction: slow and failed
// requests are ~100x over-represented relative to their true frequency, so an
// unweighted p95 reads as far worse than reality.
//
// The queries below correct for that with the standard fix for Bernoulli
// sampling: a kept-deterministically row (failed or slow) counts once, and a
// kept-by-chance row counts as 1/SampleRate — an unbiased estimate of how many
// requests it stands in for. Percentiles are found by walking the weighted
// empirical distribution rather than the raw one. This assumes the sample rate
// was constant across the window, which is documented rather than silently
// assumed: a rate changed mid-window makes the estimate approximate, not wrong
// in a way that compounds.

// WindowedTraffic is request volume and errors for an arbitrary window, with a
// series suitable for a bar chart.
type WindowedTraffic struct {
	Requests     int64
	ClientErrors int64
	ServerErrors int64
	BytesIn      int64
	BytesOut     int64
	// Series is oldest first. Hourly for a window of two days or less, daily
	// beyond that — the same threshold the Performance page's own range
	// selector offers, so the chart is never asked to draw more bars than fit.
	Series []TrafficPoint
}

// TrafficPoint is one bar.
type TrafficPoint struct {
	At       time.Time
	Requests int64
	Errors   int64
}

// ErrorRate is the fraction of requests that failed.
func (t WindowedTraffic) ErrorRate() float64 {
	if t.Requests == 0 {
		return 0
	}
	return float64(t.ClientErrors+t.ServerErrors) / float64(t.Requests)
}

// hourlySeriesLimit is the window length above which the series buckets by
// day instead of by hour. Beyond it hourly bars would outrun what a chart at
// console width can usefully draw.
const hourlySeriesLimit = 48 * time.Hour

// RequestsInWindow reports traffic and errors between since and until,
// inclusive of since and exclusive of until, from the durable rollup — exact
// for any window inside its 90-day retention, unlike the sampled-log queries
// below.
func RequestsInWindow(ctx context.Context, q Querier, since, until time.Time) (*WindowedTraffic, error) {
	result := &WindowedTraffic{}

	err := q.QueryRow(ctx, `
		SELECT
			coalesce(sum(requests), 0),
			coalesce(sum(requests) FILTER (WHERE status_class = 4), 0),
			coalesce(sum(requests) FILTER (WHERE status_class = 5), 0),
			coalesce(sum(bytes_in), 0),
			coalesce(sum(bytes_out), 0)
		FROM request_metrics
		WHERE hour >= $1 AND hour < $2`, since, until,
	).Scan(&result.Requests, &result.ClientErrors, &result.ServerErrors,
		&result.BytesIn, &result.BytesOut)
	if err != nil {
		return nil, fmt.Errorf("summarise windowed traffic: %w", err)
	}

	step := "hour"
	if until.Sub(since) > hourlySeriesLimit {
		step = "day"
	}

	// generate_series fills gaps with zero rather than compressing them, so a
	// quiet stretch stays visible as a quiet stretch rather than vanishing.
	rows, err := q.Query(ctx, fmt.Sprintf(`
		SELECT b.at,
		       coalesce(sum(m.requests), 0),
		       coalesce(sum(m.requests) FILTER (WHERE m.status_class IN (4, 5)), 0)
		FROM generate_series(date_trunc('%s', $1::timestamptz), date_trunc('%s', $2::timestamptz - interval '1 microsecond'), interval '1 %s') AS b(at)
		LEFT JOIN request_metrics m ON date_trunc('%s', m.hour) = b.at
		GROUP BY b.at
		ORDER BY b.at`, step, step, step, step), since, until)
	if err != nil {
		return nil, fmt.Errorf("read windowed traffic series: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var point TrafficPoint
		if err := rows.Scan(&point.At, &point.Requests, &point.Errors); err != nil {
			return nil, fmt.Errorf("scan traffic point: %w", err)
		}
		result.Series = append(result.Series, point)
	}
	return result, rows.Err()
}

// EarliestSample reports the oldest surviving row in the sampled log, which is
// what bounds how far back latency and slowest-operations can honestly reach —
// retention purges older rows regardless of what window is asked for.
func EarliestSample(ctx context.Context, q Querier) (time.Time, error) {
	var earliest *time.Time
	err := q.QueryRow(ctx,
		`SELECT min(occurred_at) FROM request_logs WHERE surface = 's3'`).Scan(&earliest)
	if err != nil {
		return time.Time{}, fmt.Errorf("read earliest sample: %w", err)
	}
	if earliest == nil {
		return time.Time{}, nil
	}
	return *earliest, nil
}

// LatencySummary is the weighted percentile estimate for a window.
type LatencySummary struct {
	P50MS, P90MS, P99MS, MaxMS int
	// OverThreshold is exact, not estimated: every request at or above the
	// configured slow threshold is always kept, never sampled away, so this is
	// a real count rather than a scaled-up guess.
	OverThreshold int64
	// SampleRows is how many log rows the estimate was built from — the raw
	// count, not the weighted one, so a thin window (few slow requests, low
	// sample rate) is visibly thin rather than reported with false confidence.
	SampleRows int64
}

// Latency estimates percentiles for a window from the sampled log.
//
// sampleRate is the policy's current SampleRate, used to weight the ordinary
// successes that were kept by chance. A row kept because it failed or was slow
// carries weight 1: it was retained deterministically, so it already
// represents exactly one request.
func Latency(ctx context.Context, q Querier, since, until time.Time, sampleRate float64, slowThresholdMS int) (*LatencySummary, error) {
	summary := &LatencySummary{}

	err := q.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE duration_ms >= $3)
		FROM request_logs
		WHERE surface = 's3' AND occurred_at >= $1 AND occurred_at < $2`,
		since, until, slowThresholdMS,
	).Scan(&summary.OverThreshold)
	if err != nil {
		return nil, fmt.Errorf("count over threshold: %w", err)
	}

	rate := sampleRate
	if rate <= 0 {
		// Weighting is undefined at a zero rate — no ordinary success was ever
		// kept to weight. Falling back to 1 does not fabricate data it does
		// not have; it just stops dividing by zero. The result in this case is
		// effectively "percentiles of failures and slow requests only", which
		// is what the data actually contains.
		rate = 1
	}

	rows, err := q.Query(ctx, `
		SELECT duration_ms, sampled FROM request_logs
		WHERE surface = 's3' AND occurred_at >= $1 AND occurred_at < $2
		ORDER BY duration_ms`, since, until)
	if err != nil {
		return nil, fmt.Errorf("read latency samples: %w", err)
	}
	defer rows.Close()

	type point struct {
		ms int
		w  float64
	}
	var points []point
	var total float64
	for rows.Next() {
		var p point
		var sampled bool
		if err := rows.Scan(&p.ms, &sampled); err != nil {
			return nil, fmt.Errorf("scan latency sample: %w", err)
		}
		p.w = 1
		if sampled {
			p.w = 1 / rate
		}
		total += p.w
		if p.ms > summary.MaxMS {
			summary.MaxMS = p.ms
		}
		points = append(points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	summary.SampleRows = int64(len(points))

	// Weighted nearest-rank: walk the sorted, already-ordered rows and take the
	// first one whose cumulative weight reaches the target quantile of the
	// total. Postgres's percentile_cont has no weighted form, and this is the
	// standard fix for it.
	quantile := func(q float64) int {
		if total == 0 {
			return 0
		}
		var cum float64
		target := total * q
		for _, p := range points {
			cum += p.w
			if cum >= target {
				return p.ms
			}
		}
		return summary.MaxMS
	}
	summary.P50MS = quantile(0.50)
	summary.P90MS = quantile(0.90)
	summary.P99MS = quantile(0.99)

	return summary, nil
}

// OperationStat is one row of the slowest-operations table.
type OperationStat struct {
	Operation string
	Bucket    string
	// CallsEstimate and BytesEstimate are weighted the same way as Latency's
	// percentiles: an unbiased estimate of the true total, not the raw number
	// of log rows this operation happened to leave behind.
	CallsEstimate int64
	P95MS         int
	BytesEstimate int64
}

// SlowestOperations ranks operations by estimated p95 latency over a window.
//
// Grouped by operation and bucket together, because "ListObjectsV2 is slow" is
// less actionable than "ListObjectsV2 on user-uploads is slow" — the design
// this was built from draws exactly that distinction, and it is what makes a
// row's drill-through into the log filter specific enough to be useful.
func SlowestOperations(ctx context.Context, q Querier, since, until time.Time, sampleRate float64, limit int) ([]OperationStat, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rate := sampleRate
	if rate <= 0 {
		rate = 1
	}

	rows, err := q.Query(ctx, `
		SELECT operation, bucket, duration_ms, sampled, bytes_in, bytes_out
		FROM request_logs
		WHERE surface = 's3' AND operation <> '' AND occurred_at >= $1 AND occurred_at < $2
		ORDER BY operation, bucket, duration_ms`, since, until)
	if err != nil {
		return nil, fmt.Errorf("read operation samples: %w", err)
	}
	defer rows.Close()

	type row struct {
		ms         int
		w          float64
		bytes      int64
		op, bucket string
	}
	groups := map[[2]string][]row{}
	var order [][2]string
	for rows.Next() {
		var r row
		var sampled bool
		var bytesIn, bytesOut int64
		if err := rows.Scan(&r.op, &r.bucket, &r.ms, &sampled, &bytesIn, &bytesOut); err != nil {
			return nil, fmt.Errorf("scan operation sample: %w", err)
		}
		r.w = 1
		if sampled {
			r.w = 1 / rate
		}
		r.bytes = bytesIn + bytesOut
		key := [2]string{r.op, r.bucket}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	stats := make([]OperationStat, 0, len(order))
	for _, key := range order {
		rs := groups[key]
		var total, calls, bytesEst float64
		for _, r := range rs {
			total += r.w
			bytesEst += r.w * float64(r.bytes)
		}
		calls = total

		target := total * 0.95
		var cum float64
		p95 := rs[len(rs)-1].ms
		for _, r := range rs {
			cum += r.w
			if cum >= target {
				p95 = r.ms
				break
			}
		}

		stats = append(stats, OperationStat{
			Operation: key[0], Bucket: key[1],
			CallsEstimate: int64(calls + 0.5),
			P95MS:         p95,
			BytesEstimate: int64(bytesEst + 0.5),
		})
	}

	sortOperationStatsByP95Desc(stats)
	if len(stats) > limit {
		stats = stats[:limit]
	}
	return stats, nil
}

func sortOperationStatsByP95Desc(stats []OperationStat) {
	sort.Slice(stats, func(i, j int) bool { return stats[i].P95MS > stats[j].P95MS })
}
