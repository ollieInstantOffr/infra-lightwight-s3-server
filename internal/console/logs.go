package console

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/logs"
)

// LogSink is the sampling policy the console can adjust at runtime.
type LogSink interface {
	Policy() logs.Policy
	SetPolicy(policy logs.Policy)
	// DroppedTotal is exported for the metrics endpoint. A log with holes in
	// it has to say so, and until this was scrapeable the only sign was a
	// warning on stdout that nothing was watching.
	DroppedTotal() int64
}

// handleListLogs returns a page of request logs.
func (s *Server) handleListLogs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	filter := db.LogFilter{
		Surface:     query.Get("surface"),
		OnlyErrors:  query.Get("errors") == "1",
		ErrorCode:   query.Get("code"),
		Bucket:      query.Get("bucket"),
		KeyPrefix:   query.Get("prefix"),
		Method:      query.Get("method"),
		Operation:   query.Get("operation"),
		AccessKeyID: query.Get("accessKeyId"),
		Search:      query.Get("q"),
		Before:      int64Param(query.Get("before")),
		Limit:       intParam(query.Get("limit"), 100),
	}

	// slow=1 means "at least as slow as the configured threshold" — the same
	// number the Latency panel and the sink's own retention already use, so
	// the log screen's idea of slow never disagrees with the one that decided
	// which rows survived to be searched at all.
	if query.Get("slow") == "1" {
		policy := logs.DefaultPolicy()
		if s.Sink != nil {
			policy = s.Sink.Policy()
		}
		filter.MinDurationMS = int(policy.SlowThreshold.Milliseconds())
	}

	// A status class is friendlier than a range: people think in "4xx", not
	// "400 to 499".
	if class := query.Get("class"); class != "" {
		if n, err := strconv.Atoi(class); err == nil && n >= 1 && n <= 5 {
			filter.StatusFrom, filter.StatusTo = n*100, n*100+99
		}
	}
	if since := query.Get("since"); since != "" {
		if minutes, err := strconv.Atoi(since); err == nil && minutes > 0 {
			filter.Since = s.now().Add(-time.Duration(minutes) * time.Minute)
		}
	}

	entries, err := db.ListRequestLogs(r.Context(), s.DB, filter)
	if err != nil {
		s.internalError(w, r, "list request logs", err)
		return
	}

	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, logResponse(e))
	}

	var nextBefore any
	if len(entries) == filter.Limit && len(entries) > 0 {
		nextBefore = entries[len(entries)-1].ID
	}

	writeJSON(w, http.StatusOK, map[string]any{"logs": out, "nextBefore": nextBefore})
}

func logResponse(e db.RequestLog) map[string]any {
	return map[string]any{
		"id":          e.ID,
		"at":          e.At,
		"requestId":   e.RequestID,
		"surface":     e.Surface,
		"method":      e.Method,
		"operation":   e.Operation,
		"bucket":      e.Bucket,
		"key":         e.Key,
		"path":        e.Path,
		"status":      e.Status,
		"errorCode":   e.ErrorCode,
		"reason":      e.Reason,
		"bytesIn":     e.BytesIn,
		"bytesOut":    e.BytesOut,
		"durationMs":  e.DurationMS,
		"accessKeyId": e.AccessKeyID,
		"actor":       e.Actor,
		"clientIp":    e.ClientIP,
		"userAgent":   e.UserAgent,
		"sampled":     e.Sampled,
	}
}

// handleLogSummary groups recent failures into something that names a cause.
//
// A list of a thousand 403s is not a diagnosis. "842 SignatureDoesNotMatch, all
// from one access key, all in the last hour" is.
func (s *Server) handleLogSummary(w http.ResponseWriter, r *http.Request) {
	window := time.Duration(intParam(r.URL.Query().Get("minutes"), 60)) * time.Minute

	groups, err := db.GroupErrors(r.Context(), s.DB, window, 20)
	if err != nil {
		s.internalError(w, r, "group errors", err)
		return
	}

	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		out = append(out, map[string]any{
			"errorCode":   g.ErrorCode,
			"reason":      g.Reason,
			"bucket":      g.Bucket,
			"accessKeyId": g.AccessKeyID,
			"clientIp":    g.ClientIP,
			"count":       g.Count,
			"lastSeen":    g.LastSeen,
			// The likely cause, where the pattern is unambiguous. Naming it
			// saves the operator inferring it, and these four account for most
			// real failures against this server.
			"likelyCause": likelyCause(g),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out, "windowMinutes": int(window.Minutes())})
}

// likelyCause names the diagnosis when the failure pattern only has one
// sensible reading. Empty when it does not — a confident wrong guess is worse
// than none.
func likelyCause(g db.ErrorBreakdown) string {
	switch g.ErrorCode {
	case "RequestTimeTooSkewed":
		return "The client's clock is outside the 15 minute window. Fix the time on the machine making these requests."
	case "InvalidAccessKeyId":
		return "The access key does not exist or has been revoked. Check the Access keys screen, and reissue if needed."
	case "SignatureDoesNotMatch":
		return "Usually a wrong secret key, or a reverse proxy not forwarding the original host — SigV4 signs the hostname, so a mismatch there fails every request."
	case "AccessDenied":
		// AccessDenied now has two quite different causes, and telling them
		// apart matters more than either message. Sending someone to check
		// their client's credentials when the real answer is that they scoped
		// the key too narrowly wastes exactly the time this screen exists to
		// save. The recorded reason is what separates them.
		if strings.Contains(g.Reason, "not permitted") {
			return "The key is valid, but its access does not cover this. Open Access keys, find the key, and widen it — or point the client at somewhere inside its scope. " +
				"The reason column names the bucket and the permission it wanted."
		}
		return "The request carried no signature at all. Either the client is not configured with credentials, or it is addressing a bucket that is not public."
	case "NoSuchBucket":
		return "The bucket does not exist. A client using virtual-host addressing against a server without wildcard DNS produces this, as the bucket name never reaches the server."
	case "NoSuchKey":
		return "The object does not exist. Ordinary if clients probe for optional files; worth investigating if it is sudden."
	case "EntityTooLarge":
		return "The upload exceeded a limit. Check the reverse proxy's client_max_body_size, which defaults to 1 MB and rejects most real uploads."
	}
	return ""
}

// handleServerEvents returns the warnings and errors the server raised about
// itself, which explain the things that are not requests.
func (s *Server) handleServerEvents(w http.ResponseWriter, r *http.Request) {
	events, err := db.ListServerEvents(r.Context(), s.DB,
		r.URL.Query().Get("level"), int64Param(r.URL.Query().Get("before")),
		intParam(r.URL.Query().Get("limit"), 100))
	if err != nil {
		s.internalError(w, r, "list server events", err)
		return
	}

	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, map[string]any{
			"id": e.ID, "at": e.At, "level": e.Level,
			"message": e.Message, "attributes": e.Attributes, "node": e.Node,
		})
	}

	var nextBefore any
	if len(events) > 0 {
		nextBefore = events[len(events)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out, "nextBefore": nextBefore})
}

// handleLogStream pushes new log entries as they arrive.
//
// Server-sent events rather than polling from the browser: one connection,
// server-pushed, and it degrades to a plain HTTP response through any proxy.
// It does require response buffering to be off at the proxy, which the reverse
// proxy guide already asks for.
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	// Through the response controller rather than a type assertion on w: the
	// access log wraps every console response to record its status, and that
	// wrapper is not itself a Flusher. Asserting directly fails on the wrapper
	// instead of reaching the real connection underneath it.
	control := http.NewResponseController(w)

	// The stream is open-ended, so the server's write deadline must not apply.
	// Left in place it would kill the connection mid-tail; the browser would
	// silently reconnect in a loop, and each reconnect starts from the newest
	// entry, so whatever arrived in between would never be shown.
	if err := control.SetWriteDeadline(time.Time{}); err != nil {
		s.Log.Warn("could not clear the write deadline for the log stream", "error", err)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Tells nginx not to buffer this response even if it is configured to
	// buffer everything else, which would otherwise hold the stream until it
	// filled a buffer.
	w.Header().Set("X-Accel-Buffering", "no")

	// Headers first, then flush. Flushing earlier would commit a bare 200 with
	// none of these set, and every header above would be silently discarded.
	w.WriteHeader(http.StatusOK)
	if err := control.Flush(); err != nil {
		// The status is already sent, so there is no way to report this to the
		// client as an error. It cannot happen with the handlers this server
		// installs; it is logged rather than ignored in case that changes.
		s.Log.Error("the log stream cannot flush; live tail is unavailable", "error", err)
		return
	}

	filter := db.LogFilter{
		OnlyErrors: r.URL.Query().Get("errors") == "1",
		Surface:    r.URL.Query().Get("surface"),
		Bucket:     r.URL.Query().Get("bucket"),
		Search:     r.URL.Query().Get("q"),
		Limit:      100,
	}

	// Start from the newest entry rather than replaying history: the tail is
	// for watching what happens next.
	var lastID int64
	if recent, err := db.ListRequestLogs(r.Context(), s.DB, db.LogFilter{Limit: 1}); err == nil && len(recent) > 0 {
		lastID = recent[0].ID
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	// A heartbeat keeps intermediaries from closing an idle connection, and
	// tells the browser the stream is alive on a quiet server.
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case <-heartbeat.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			control.Flush()

		case <-ticker.C:
			entries, err := db.ListRequestLogsSince(r.Context(), s.DB, filter, lastID)
			if err != nil {
				return
			}
			// Oldest first, so the client appends in the order things happened.
			for i := len(entries) - 1; i >= 0; i-- {
				payload, err := jsonBytes(logResponse(entries[i]))
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "data: %s\n\n", payload)
				if entries[i].ID > lastID {
					lastID = entries[i].ID
				}
			}
			if len(entries) > 0 {
				control.Flush()
			}
		}
	}
}

// handleLogSettings reports and adjusts sampling, so detail can be turned up
// while diagnosing and back down afterwards without a restart.
func (s *Server) handleLogSettings(w http.ResponseWriter, r *http.Request) {
	requests, events, bytes, err := db.LogStorage(r.Context(), s.DB)
	if err != nil {
		s.internalError(w, r, "measure log storage", err)
		return
	}

	policy := logs.DefaultPolicy()
	if s.Sink != nil {
		policy = s.Sink.Policy()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"sampleRate":      policy.SampleRate,
		"slowThresholdMs": policy.SlowThreshold.Milliseconds(),
		"requestRows":     requests,
		"eventRows":       events,
		"bytes":           bytes,
		"note":            "Failures and slow requests are always kept. The sample rate applies only to successful requests.",
	})
}

type logSettingsRequest struct {
	SampleRate      float64 `json:"sampleRate"`
	SlowThresholdMs int64   `json:"slowThresholdMs"`
}

func (s *Server) handleUpdateLogSettings(w http.ResponseWriter, r *http.Request) {
	var request logSettingsRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "Send a JSON body with a sample rate.")
		return
	}
	if request.SampleRate < 0 || request.SampleRate > 1 {
		writeError(w, http.StatusBadRequest, "The sample rate must be between 0 and 1.")
		return
	}
	if s.Sink == nil {
		writeError(w, http.StatusConflict, "Logging is not enabled on this server.")
		return
	}

	slow := time.Duration(request.SlowThresholdMs) * time.Millisecond
	if slow <= 0 {
		slow = logs.DefaultPolicy().SlowThreshold
	}
	s.Sink.SetPolicy(logs.Policy{SampleRate: request.SampleRate, SlowThreshold: slow})

	user, _ := UserFrom(r.Context())
	s.Log.Info("log sampling changed",
		"sample_rate", request.SampleRate, "slow_ms", slow.Milliseconds(), "by", user.Email)

	writeJSON(w, http.StatusOK, map[string]any{"message": "Sampling updated."})
}
