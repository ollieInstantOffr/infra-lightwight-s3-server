package s3api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// Public buckets and CORS.
//
// A public bucket permits anonymous GET and HEAD, and nothing else. Anonymous
// writes are never allowed: a publicly writable bucket is an open file drop,
// and nobody sets one up on purpose. Making that a rule in code rather than a
// setting means it cannot be turned on by accident.

// anonymousMethodAllowed reports whether an unauthenticated request may proceed
// on a public bucket.
func anonymousMethodAllowed(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// allowAnonymous decides whether an unsigned request should be served.
//
// It runs only after signature verification has already failed or found no
// signature, so the cost falls on anonymous traffic rather than on every
// request.
func (s *Server) allowAnonymous(r *http.Request) bool {
	if !anonymousMethodAllowed(r.Method) {
		return false
	}
	bucket, _, err := s.resolveAddressing(r)
	if err != nil || bucket == "" {
		return false
	}
	public, err := db.PublicBucket(r.Context(), s.DB, bucket)
	if err != nil {
		return false
	}
	return public
}

// applyCORS writes the CORS response headers a bucket's rules permit.
//
// Returns true if the request was a preflight and has been fully answered, in
// which case the caller must not continue: a preflight carries no body and
// must not reach a handler that would try to read one.
func (s *Server) applyCORS(w http.ResponseWriter, r *http.Request) (handled bool) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}

	bucketName, _, err := s.resolveAddressing(r)
	if err != nil || bucketName == "" {
		return false
	}
	bucket, err := db.GetBucket(r.Context(), s.DB, bucketName)
	if err != nil {
		return false
	}
	settings, err := db.GetBucketSettings(r.Context(), s.DB, bucket.ID)
	if err != nil || len(settings.CORSRules) == 0 {
		return false
	}

	// A preflight asks about the method it intends to use, not the OPTIONS it
	// is sending, so the rule is matched against that.
	requested := r.Method
	if r.Method == http.MethodOptions {
		if asked := r.Header.Get("Access-Control-Request-Method"); asked != "" {
			requested = asked
		}
	}

	rule := matchCORSRule(settings.CORSRules, origin, requested)
	if rule == nil {
		// An unmatched preflight is still answered, without the permissive
		// headers. The browser then blocks the request itself, which produces
		// a clearer console message than a bare 403.
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusForbidden)
			return true
		}
		return false
	}

	// Echoing the specific origin rather than "*" is what allows credentialed
	// requests, and Vary keeps a cache from serving one origin's response to
	// another.
	w.Header().Set("Access-Control-Allow-Origin", allowedOriginValue(rule, origin))
	w.Header().Add("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", strings.Join(rule.AllowedMethods, ", "))

	if len(rule.ExposeHeaders) > 0 {
		w.Header().Set("Access-Control-Expose-Headers", strings.Join(rule.ExposeHeaders, ", "))
	}

	if r.Method == http.MethodOptions {
		if requestedHeaders := r.Header.Get("Access-Control-Request-Headers"); requestedHeaders != "" {
			w.Header().Set("Access-Control-Allow-Headers", allowedHeadersValue(rule, requestedHeaders))
		}
		if rule.MaxAgeSeconds > 0 {
			w.Header().Set("Access-Control-Max-Age", strconv.Itoa(rule.MaxAgeSeconds))
		}
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// matchCORSRule finds the first rule permitting this origin and method.
func matchCORSRule(rules []db.CORSRule, origin, method string) *db.CORSRule {
	for i := range rules {
		rule := &rules[i]
		if !originMatches(rule.AllowedOrigins, origin) {
			continue
		}
		if !methodMatches(rule.AllowedMethods, method) {
			continue
		}
		return rule
	}
	return nil
}

func originMatches(allowed []string, origin string) bool {
	for _, candidate := range allowed {
		if candidate == "*" || strings.EqualFold(candidate, origin) {
			return true
		}
	}
	return false
}

func methodMatches(allowed []string, method string) bool {
	for _, candidate := range allowed {
		if strings.EqualFold(candidate, method) {
			return true
		}
	}
	return false
}

func allowedOriginValue(rule *db.CORSRule, origin string) string {
	for _, candidate := range rule.AllowedOrigins {
		if candidate == "*" {
			return "*"
		}
	}
	return origin
}

// allowedHeadersValue answers a preflight's header list.
//
// The requested headers are echoed when the rule allows any, which is what a
// wildcard means in practice; otherwise the rule's own list is returned.
func allowedHeadersValue(rule *db.CORSRule, requested string) string {
	for _, candidate := range rule.AllowedHeaders {
		if candidate == "*" {
			return requested
		}
	}
	if len(rule.AllowedHeaders) == 0 {
		return requested
	}
	return strings.Join(rule.AllowedHeaders, ", ")
}
