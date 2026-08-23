package logs

import (
	"context"
	"log/slog"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// Handler tees warnings and errors into the sink while passing everything
// through to the underlying handler.
//
// Request logs explain requests. They do not explain a failed email send, a
// blob the sweeper could not reclaim, or settings that could not be read — all
// logged at warn or above and otherwise lost to stdout.
//
// Info and debug are deliberately not captured: on a busy server that is
// volume without value, and the console has request logs for traffic.
//
// Per-request warnings are excluded too. Every rejected request logs at warn
// twice — once from the authenticator and once from the access log — and both
// already appear in the request log with more detail. Capturing them here as
// well would triple the volume and bury the events this table exists for.
type Handler struct {
	inner slog.Handler
	sink  *Sink

	// skip is set when Skip() was bound through With rather than passed on
	// the record. Without it a logger built once per request and reused would
	// look unmarked at Handle time, and every record it wrote would be
	// persisted as a server event — the exact flooding Skip exists to stop.
	skip bool
}

// NewHandler wraps a handler so warn-and-above is also persisted.
func NewHandler(inner slog.Handler, sink *Sink) *Handler {
	return &Handler{inner: inner, sink: sink}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// SkipKey marks a log record as belonging to the request log rather than the
// server event log. Records carrying it pass through to stdout untouched and
// are not persisted as events.
const SkipKey = "__request_scoped"

// Skip marks a record as request-scoped.
func Skip() slog.Attr { return slog.Bool(SkipKey, true) }

func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level >= slog.LevelWarn && !h.skip {
		attributes := make(map[string]any, record.NumAttrs())
		requestScoped := false
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == SkipKey {
				requestScoped = true
				return true
			}
			attributes[attr.Key] = attr.Value.String()
			return true
		})

		if requestScoped {
			return h.inner.Handle(ctx, record)
		}

		level := "WARN"
		if record.Level >= slog.LevelError {
			level = "ERROR"
		}

		// The sink only appends under a mutex, so this cannot block on the
		// database — which matters because it runs inside every log call.
		h.sink.RecordEvent(db.ServerEvent{
			At:         nonZeroTime(record.Time),
			Level:      level,
			Message:    record.Message,
			Attributes: attributes,
		})
	}
	return h.inner.Handle(ctx, record)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	skip := h.skip
	for _, attr := range attrs {
		if attr.Key == SkipKey {
			skip = true
		}
	}
	return &Handler{inner: h.inner.WithAttrs(attrs), sink: h.sink, skip: skip}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name), sink: h.sink, skip: h.skip}
}

func nonZeroTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}
