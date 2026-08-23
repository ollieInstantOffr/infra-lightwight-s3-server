package console

import (
	"net/http"
	"runtime"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// The system screen is the on-prem one: what this node is, how it is
// configured, and what is about to go wrong. It reports settings but never
// their secret values — an operator needs to know whether a Resend key is
// configured, not what it is.

// SystemInfo is the static half, supplied at startup.
type SystemInfo struct {
	Version   string
	NodeName  string
	StartedAt time.Time
	DataDir   string
	S3Domain  string
	// TrustedProxyCount rather than the list: the CIDRs are not secret, but a
	// count answers "is forwarding configured" without turning a status page
	// into a network map.
	TrustedProxyCount int
	ResendConfigured  bool
	Environment       string
}

// handleSystem reports the node's state.
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	usage, usageErr := s.Blobs.Usage()
	databaseUp := s.DB.Ping(ctx) == nil
	stat := s.DB.Stat()

	var credentialCount, activeCredentials int
	_ = s.DB.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE revoked_at IS NULL) FROM credentials`).
		Scan(&credentialCount, &activeCredentials)

	var userCount int
	_ = s.DB.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&userCount)

	// Warnings are the point of this screen. Each one is a specific thing an
	// operator can act on, in the order it would bite them.
	warnings := []map[string]string{}
	if !databaseUp {
		warnings = append(warnings, warning("database",
			"The database is unreachable. The server cannot serve any request until it returns."))
	}
	if usageErr != nil {
		warnings = append(warnings, warning("storage",
			"The data directory could not be read: "+usageErr.Error()))
	} else if usage.TotalBytes > 0 {
		free := float64(usage.FreeBytes) / float64(usage.TotalBytes)
		if free < 0.05 {
			warnings = append(warnings, warning("disk",
				"Less than 5% of the volume is free. Uploads will start failing."))
		} else if free < 0.15 {
			warnings = append(warnings, warning("disk",
				"Less than 15% of the volume is free."))
		}
	}
	if !s.System.ResendConfigured {
		warnings = append(warnings, warning("email",
			"RESEND_API_KEY is not set, so sign-in links are written to the server log instead of emailed. Nobody else can sign in."))
	}
	if activeCredentials == 0 {
		warnings = append(warnings, warning("credentials",
			"No active S3 credentials exist, so the S3 API cannot be used yet."))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"node": map[string]any{
			"name":        s.System.NodeName,
			"version":     s.System.Version,
			"go":          runtime.Version(),
			"environment": s.System.Environment,
			"startedAt":   s.System.StartedAt,
			"uptime":      int64(time.Since(s.System.StartedAt).Seconds()),
		},
		"storage": map[string]any{
			"dataDir":    s.System.DataDir,
			"diskTotal":  usage.TotalBytes,
			"diskFree":   usage.FreeBytes,
			"readable":   usageErr == nil,
			"singleCopy": true,
		},
		"database": map[string]any{
			"reachable":       databaseUp,
			"connections":     stat.TotalConns(),
			"idleConnections": stat.IdleConns(),
			"maxConnections":  stat.MaxConns(),
			"acquiredConns":   stat.AcquiredConns(),
		},
		"endpoints": map[string]any{
			"s3":       s.PublicS3URL,
			"console":  s.PublicURL,
			"region":   s.Region,
			"s3Domain": s.System.S3Domain,
			// Path style always works; virtual-host style needs a domain and a
			// wildcard certificate, so whether it is on is worth stating.
			"virtualHostStyle": s.System.S3Domain != "",
		},
		"config": map[string]any{
			"resendConfigured":  s.System.ResendConfigured,
			"trustedProxyCount": s.System.TrustedProxyCount,
		},
		"counts": map[string]any{
			"users":             userCount,
			"credentials":       credentialCount,
			"activeCredentials": activeCredentials,
		},
		"warnings": warnings,
	})
}

func warning(area, message string) map[string]string {
	return map[string]string{"area": area, "message": message}
}

// handleTraffic reports request volume for the overview chart.
func (s *Server) handleTraffic(w http.ResponseWriter, r *http.Request) {
	days := intParam(r.URL.Query().Get("days"), 14)
	if days > 90 {
		days = 90
	}

	traffic, err := db.Traffic(r.Context(), s.DB, days)
	if err != nil {
		s.internalError(w, r, "read traffic", err)
		return
	}

	daily := make([]map[string]any, 0, len(traffic.Daily))
	for _, day := range traffic.Daily {
		daily = append(daily, map[string]any{
			"day":      day.Day,
			"requests": day.Requests,
			"errors":   day.Errors,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"requests24h":  traffic.Requests24h,
		"clientErrors": traffic.ClientErrors,
		"serverErrors": traffic.ServerErrors,
		"errorRate":    traffic.ErrorRate(),
		"bytesIn24h":   traffic.BytesIn24h,
		"bytesOut24h":  traffic.BytesOut24h,
		"daily":        daily,
	})
}

// handleSearch finds objects by key across every bucket.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	result, err := db.SearchObjects(r.Context(), s.DB, query, intParam(r.URL.Query().Get("limit"), 30))
	if err != nil {
		s.internalError(w, r, "search objects", err)
		return
	}

	hits := make([]map[string]any, 0, len(result.Hits))
	for _, hit := range result.Hits {
		hits = append(hits, map[string]any{
			"bucket":       hit.Bucket,
			"key":          hit.Key,
			"size":         hit.Size,
			"contentType":  hit.ContentType,
			"lastModified": hit.LastModified,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"hits": hits,
		// Reported rather than hidden: a silently truncated search is worse
		// than an empty one, because the user concludes the object is absent.
		"truncated": result.Truncated,
		"byPrefix":  result.ScannedAsPrefix,
	})
}

// handleVersion reports what is running, without needing a session.
//
// Unauthenticated deliberately, and it is the only thing here that is. The
// question it answers — did my rollout land, and on what — is asked by
// deployment tooling that holds no session, and the answer is a number that
// appears in the repository, the image tag and the release notes. Guarding it
// would inconvenience the people who need it and inconvenience nobody else.
//
// The schema versions come with it because the interesting failure is a
// mismatch: a build that will not start says the database is ahead of it, and
// this is where someone checks that claim from outside the container.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	payload := map[string]any{"version": s.System.Version}

	if build, err := db.SchemaVersion(); err == nil {
		payload["schemaVersion"] = build
	}
	ctx, cancel := contextWithTimeout(r, 3*time.Second)
	defer cancel()
	if applied, err := db.AppliedSchemaVersion(ctx, s.DB); err == nil {
		payload["appliedSchemaVersion"] = applied
	}

	writeJSON(w, http.StatusOK, payload)
}
