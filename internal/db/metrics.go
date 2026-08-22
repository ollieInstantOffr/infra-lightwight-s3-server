package db

import (
	"context"
	"fmt"
	"time"
)

// Request counts are rolled up per hour rather than stored per request.
//
// A row per request would put a database write on every object GET — the
// hottest path in the system, and the one that most needs to stay close to a
// plain file read. The server accumulates counts in memory and flushes them
// periodically; losing a partial minute of counts to a crash is an acceptable
// price for not making every read transactional.

// MetricSample is one flushed bucket of counts.
type MetricSample struct {
	Hour        time.Time
	StatusClass int
	Requests    int64
	BytesIn     int64
	BytesOut    int64
}

// FlushMetrics adds counts to the rollup table.
func FlushMetrics(ctx context.Context, q Querier, samples []MetricSample) error {
	for _, sample := range samples {
		_, err := q.Exec(ctx, `
			INSERT INTO request_metrics (hour, status_class, requests, bytes_in, bytes_out)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (hour, status_class) DO UPDATE SET
				requests  = request_metrics.requests + EXCLUDED.requests,
				bytes_in  = request_metrics.bytes_in + EXCLUDED.bytes_in,
				bytes_out = request_metrics.bytes_out + EXCLUDED.bytes_out`,
			sample.Hour, sample.StatusClass, sample.Requests, sample.BytesIn, sample.BytesOut)
		if err != nil {
			return fmt.Errorf("flush metrics: %w", err)
		}
	}
	return nil
}

// TrafficSummary is what the overview shows.
type TrafficSummary struct {
	Requests24h  int64
	ClientErrors int64
	ServerErrors int64
	BytesIn24h   int64
	BytesOut24h  int64
	// Daily is oldest-first, one entry per day, so the chart can be drawn
	// without the client having to fill gaps.
	Daily []DailyTraffic
}

// DailyTraffic is one bar of the chart.
type DailyTraffic struct {
	Day      time.Time
	Requests int64
	Errors   int64
}

// ErrorRate is the fraction of requests that failed, as a proportion.
func (s TrafficSummary) ErrorRate() float64 {
	if s.Requests24h == 0 {
		return 0
	}
	return float64(s.ClientErrors+s.ServerErrors) / float64(s.Requests24h)
}

// Traffic reports recent request activity.
func Traffic(ctx context.Context, q Querier, days int) (*TrafficSummary, error) {
	summary := &TrafficSummary{}

	err := q.QueryRow(ctx, `
		SELECT
			coalesce(sum(requests), 0),
			coalesce(sum(requests) FILTER (WHERE status_class = 4), 0),
			coalesce(sum(requests) FILTER (WHERE status_class = 5), 0),
			coalesce(sum(bytes_in), 0),
			coalesce(sum(bytes_out), 0)
		FROM request_metrics
		WHERE hour > now() - interval '24 hours'`,
	).Scan(&summary.Requests24h, &summary.ClientErrors, &summary.ServerErrors,
		&summary.BytesIn24h, &summary.BytesOut24h)
	if err != nil {
		return nil, fmt.Errorf("summarise traffic: %w", err)
	}

	// generate_series fills days with no traffic, so the chart has a bar for
	// every day rather than silently compressing quiet periods.
	rows, err := q.Query(ctx, `
		SELECT d.day,
		       coalesce(sum(m.requests), 0),
		       coalesce(sum(m.requests) FILTER (WHERE m.status_class IN (4, 5)), 0)
		FROM generate_series(
			date_trunc('day', now()) - make_interval(days => $1 - 1),
			date_trunc('day', now()),
			interval '1 day'
		) AS d(day)
		LEFT JOIN request_metrics m ON date_trunc('day', m.hour) = d.day
		GROUP BY d.day
		ORDER BY d.day`, days)
	if err != nil {
		return nil, fmt.Errorf("read daily traffic: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var daily DailyTraffic
		if err := rows.Scan(&daily.Day, &daily.Requests, &daily.Errors); err != nil {
			return nil, fmt.Errorf("scan daily traffic: %w", err)
		}
		summary.Daily = append(summary.Daily, daily)
	}
	return summary, rows.Err()
}

// PurgeMetrics drops rollups past the retention window.
func PurgeMetrics(ctx context.Context, q Querier, retain time.Duration) error {
	if _, err := q.Exec(ctx,
		`DELETE FROM request_metrics WHERE hour < now() - $1::interval`, retain.String()); err != nil {
		return fmt.Errorf("purge metrics: %w", err)
	}
	return nil
}
