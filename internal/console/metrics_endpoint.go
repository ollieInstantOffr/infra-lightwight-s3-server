package console

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/metrics"
)

// The scrape endpoint.
//
// It lives on the console port rather than a third listener. The console is
// already the control-plane surface, already behind its own hostname and proxy,
// and already the thing an operator has firewalled where they want it — a new
// port would be another thing to bind, expose and forget.

// handleMetrics renders the Prometheus exposition format.
//
// Metrics are not neutral. Bucket names, object counts, traffic volume and
// error patterns together describe who is using this system and how much, so
// this is authenticated like everything else on the console — the difference
// being that a scraper is not a browser and cannot hold a session cookie.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.metricsAuthorized(r) {
		// The realm tells a human who lands here what to configure, since the
		// most likely reader of a 401 from this endpoint is whoever is trying
		// to set the scrape up.
		w.Header().Set("WWW-Authenticate", `Bearer realm="pail metrics"`)
		writeError(w, http.StatusUnauthorized,
			"Set METRICS_TOKEN and send it as a bearer token, or sign in to the console.")
		return
	}

	snapshot := s.metricsSnapshot(r)

	// Version 0.0.4 of the text format, which is what every scraper still
	// speaks and what the content type has to say for them to parse it.
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.Registry.WriteTo(w, snapshot); err != nil {
		// The response is already partly written, so there is no status left to
		// change. Logged rather than swallowed.
		s.Log.Warn("could not finish writing metrics", "error", err)
	}
}

// metricsAuthorized decides whether a scrape may proceed.
//
// Two ways in, for two different callers. A scraper presents a bearer token; a
// person following a link from the console already has a session. The token is
// the one that matters, and when none is configured the endpoint is reachable
// only by a signed-in administrator — which is the safe default, because
// somebody will deploy this without reading the documentation and the failure
// they get should be a 401 rather than an open endpoint.
func (s *Server) metricsAuthorized(r *http.Request) bool {
	if token := bearerToken(r); token != "" && s.MetricsToken != "" {
		// Constant time, because this is a secret compared on every scrape and
		// a scrape is something an attacker can cause at will.
		return subtle.ConstantTimeCompare([]byte(token), []byte(s.MetricsToken)) == 1
	}
	user, ok := UserFrom(r.Context())
	return ok && user.IsAdmin()
}

// bearerToken extracts an Authorization: Bearer value.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// metricsSnapshot gathers the numbers this process does not hold itself.
//
// Read per scrape rather than kept current by a background job. A scrape is a
// handful of indexed queries against small tables, and the alternative is
// maintaining numbers nobody may ever ask for.
//
// A failure here degrades rather than fails: a scrape that returns request
// counters and an up_database of 0 is far more useful than a 500, since the
// database being down is exactly the moment someone wants their monitoring to
// still say something.
func (s *Server) metricsSnapshot(r *http.Request) metrics.Snapshot {
	snapshot := metrics.Snapshot{DatabaseUp: true}

	if usage, err := s.Blobs.Usage(); err == nil {
		snapshot.DiskFreeBytes = usage.FreeBytes
		snapshot.DiskTotalBytes = usage.TotalBytes
	}

	buckets, err := db.ListBucketsWithStats(r.Context(), s.DB)
	if err != nil {
		s.Log.Warn("could not read bucket usage for metrics", "error", err)
		snapshot.DatabaseUp = false
		return snapshot
	}
	for _, bucket := range buckets {
		snapshot.Buckets = append(snapshot.Buckets, metrics.BucketUsage{
			Name:    bucket.Name,
			Objects: bucket.ObjectCount,
			Bytes:   bucket.TotalBytes,
		})
	}

	firing, err := db.CountFiringAlerts(r.Context(), s.DB)
	if err != nil {
		s.Log.Warn("could not read alert state for metrics", "error", err)
	} else {
		snapshot.AlertsFiring = firing
	}

	if s.Sink != nil {
		snapshot.LogsDropped = s.Sink.DroppedTotal()
	}
	return snapshot
}
