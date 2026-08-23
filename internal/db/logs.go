package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// RequestLog is one recorded request.
//
// It deliberately holds no Authorization header, signature, secret or presigned
// query parameter. A presigned URL in a log file is a working credential, and
// log files are copied into support tickets.
type RequestLog struct {
	ID        int64
	At        time.Time
	RequestID string
	Node      string
	Surface   string
	Method    string
	Bucket    string
	Key       string
	Path      string
	Status    int
	ErrorCode string
	// Reason is the server's own explanation, which is not what the client was
	// told. A prober gets InvalidAccessKeyId; the operator needs "revoked".
	Reason      string
	BytesIn     int64
	BytesOut    int64
	DurationMS  int
	AccessKeyID string
	Actor       string
	ClientIP    string
	UserAgent   string
	Sampled     bool
}

// ServerEvent is a warning or error the server raised about itself.
type ServerEvent struct {
	ID         int64
	At         time.Time
	Node       string
	Level      string
	Message    string
	Attributes map[string]any
}

// InsertRequestLogs writes a batch.
//
// CopyFrom rather than multi-row INSERT: this runs every couple of seconds with
// potentially thousands of rows, and the binary copy protocol is substantially
// cheaper than parsing a statement that large.
func InsertRequestLogs(ctx context.Context, pool *Pool, entries []RequestLog) error {
	if len(entries) == 0 {
		return nil
	}

	columns := []string{
		"occurred_at", "request_id", "node", "surface", "method", "bucket",
		"object_key", "path", "status", "error_code", "reason", "bytes_in",
		"bytes_out", "duration_ms", "access_key_id", "actor", "client_ip",
		"user_agent", "sampled",
	}

	rows := make([][]any, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, []any{
			at(e.At), e.RequestID, e.Node, surfaceOrDefault(e.Surface), e.Method,
			e.Bucket, truncate(e.Key, 1024), truncate(e.Path, 2048), e.Status,
			e.ErrorCode, truncate(e.Reason, 2048), e.BytesIn, e.BytesOut,
			e.DurationMS, e.AccessKeyID, e.Actor, nullableIP(e.ClientIP),
			truncate(e.UserAgent, 512), e.Sampled,
		})
	}

	if _, err := pool.CopyFrom(ctx, pgx.Identifier{"request_logs"}, columns,
		pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("insert request logs: %w", err)
	}
	return nil
}

// InsertServerEvents writes a batch of warnings and errors.
func InsertServerEvents(ctx context.Context, pool *Pool, events []ServerEvent) error {
	if len(events) == 0 {
		return nil
	}

	rows := make([][]any, 0, len(events))
	for _, e := range events {
		attributes, err := json.Marshal(e.Attributes)
		if err != nil {
			attributes = []byte(`{}`)
		}
		rows = append(rows, []any{
			at(e.At), e.Node, e.Level, truncate(e.Message, 2048), attributes,
		})
	}

	if _, err := pool.CopyFrom(ctx, pgx.Identifier{"server_events"},
		[]string{"occurred_at", "node", "level", "message", "attributes"},
		pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("insert server events: %w", err)
	}
	return nil
}

// LogFilter narrows a log query.
type LogFilter struct {
	Since      time.Time
	Until      time.Time
	Surface    string
	StatusFrom int
	StatusTo   int
	// OnlyErrors is a shorthand for the common case, which is the reason
	// anyone opens the log at all.
	OnlyErrors  bool
	ErrorCode   string
	Bucket      string
	KeyPrefix   string
	Method      string
	AccessKeyID string
	Search      string
	// Before paginates by id rather than offset, so a busy log does not shift
	// rows between pages while they are being read.
	Before int64
	Limit  int
}

// ListRequestLogs returns a page, newest first.
func ListRequestLogs(ctx context.Context, q Querier, filter LogFilter) ([]RequestLog, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	conditions := []string{"1 = 1"}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(clause, len(args)))
	}

	if !filter.Since.IsZero() {
		add("occurred_at >= $%d", filter.Since)
	}
	if !filter.Until.IsZero() {
		add("occurred_at <= $%d", filter.Until)
	}
	if filter.Surface != "" {
		add("surface = $%d", filter.Surface)
	}
	if filter.OnlyErrors {
		conditions = append(conditions, "status >= 400")
	}
	if filter.StatusFrom > 0 {
		add("status >= $%d", filter.StatusFrom)
	}
	if filter.StatusTo > 0 {
		add("status <= $%d", filter.StatusTo)
	}
	if filter.ErrorCode != "" {
		add("error_code = $%d", filter.ErrorCode)
	}
	if filter.Bucket != "" {
		add("bucket = $%d", filter.Bucket)
	}
	if filter.KeyPrefix != "" {
		add("object_key LIKE $%d || '%%'", filter.KeyPrefix)
	}
	if filter.Method != "" {
		add("method = $%d", filter.Method)
	}
	if filter.AccessKeyID != "" {
		add("access_key_id = $%d", filter.AccessKeyID)
	}
	if filter.Search != "" {
		// Matches the parts a person would search: the key, the reason, or a
		// request id copied from a support report.
		//
		// Added directly rather than through the helper, which substitutes a
		// single placeholder number — this clause needs the same argument in
		// three positions.
		args = append(args, filter.Search)
		conditions = append(conditions, fmt.Sprintf(
			"(object_key ILIKE '%%' || $%d || '%%' OR reason ILIKE '%%' || $%d || '%%' OR request_id = $%d)",
			len(args), len(args), len(args)))
	}
	if filter.Before > 0 {
		add("id < $%d", filter.Before)
	}

	args = append(args, limit)
	query := fmt.Sprintf(`
		SELECT id, occurred_at, request_id, node, surface, method, bucket, object_key,
		       path, status, error_code, reason, bytes_in, bytes_out, duration_ms,
		       access_key_id, actor, host(client_ip), user_agent, sampled
		FROM request_logs
		WHERE %s
		ORDER BY id DESC
		LIMIT $%d`, strings.Join(conditions, " AND "), len(args))

	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list request logs: %w", err)
	}
	defer rows.Close()

	var out []RequestLog
	for rows.Next() {
		var e RequestLog
		var ip *string
		if err := rows.Scan(&e.ID, &e.At, &e.RequestID, &e.Node, &e.Surface, &e.Method,
			&e.Bucket, &e.Key, &e.Path, &e.Status, &e.ErrorCode, &e.Reason,
			&e.BytesIn, &e.BytesOut, &e.DurationMS, &e.AccessKeyID, &e.Actor,
			&ip, &e.UserAgent, &e.Sampled); err != nil {
			return nil, fmt.Errorf("scan request log: %w", err)
		}
		if ip != nil {
			e.ClientIP = *ip
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListServerEvents returns recent warnings and errors.
func ListServerEvents(ctx context.Context, q Querier, level string, before int64, limit int) ([]ServerEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.Query(ctx, `
		SELECT id, occurred_at, node, level, message, attributes
		FROM server_events
		WHERE ($1 = '' OR level = $1) AND ($2 = 0 OR id < $2)
		ORDER BY id DESC
		LIMIT $3`, level, before, limit)
	if err != nil {
		return nil, fmt.Errorf("list server events: %w", err)
	}
	defer rows.Close()

	var out []ServerEvent
	for rows.Next() {
		var e ServerEvent
		var attributes []byte
		if err := rows.Scan(&e.ID, &e.At, &e.Node, &e.Level, &e.Message, &attributes); err != nil {
			return nil, fmt.Errorf("scan server event: %w", err)
		}
		if len(attributes) > 0 {
			_ = json.Unmarshal(attributes, &e.Attributes)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeLogs drops entries past their retention window.
//
// Sampled successes and retained failures expire on different schedules: the
// successes exist to show traffic shape and are worthless after a few days,
// while a failure from last month may be exactly what someone is looking for.
func PurgeLogs(ctx context.Context, q Querier, failures, samples, events time.Duration) (int64, error) {
	var total int64

	tag, err := q.Exec(ctx,
		`DELETE FROM request_logs WHERE sampled AND occurred_at < now() - $1::interval`, samples.String())
	if err != nil {
		return 0, fmt.Errorf("purge sampled logs: %w", err)
	}
	total += tag.RowsAffected()

	tag, err = q.Exec(ctx,
		`DELETE FROM request_logs WHERE NOT sampled AND occurred_at < now() - $1::interval`, failures.String())
	if err != nil {
		return total, fmt.Errorf("purge retained logs: %w", err)
	}
	total += tag.RowsAffected()

	tag, err = q.Exec(ctx,
		`DELETE FROM server_events WHERE occurred_at < now() - $1::interval`, events.String())
	if err != nil {
		return total, fmt.Errorf("purge server events: %w", err)
	}
	return total + tag.RowsAffected(), nil
}

// LogStorage reports how much space the logs occupy, so it is visible before
// it becomes a problem on a server whose job is storing other things.
func LogStorage(ctx context.Context, q Querier) (requestRows, eventRows, bytes int64, err error) {
	err = q.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM request_logs),
		       (SELECT count(*) FROM server_events),
		       pg_total_relation_size('request_logs') + pg_total_relation_size('server_events')`).
		Scan(&requestRows, &eventRows, &bytes)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("measure log storage: %w", err)
	}
	return requestRows, eventRows, bytes, nil
}

func at(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

func surfaceOrDefault(s string) string {
	if s == "console" {
		return "console"
	}
	return "s3"
}
