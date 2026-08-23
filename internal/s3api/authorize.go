package s3api

import (
	"net/http"
	"strings"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// Authorization for scoped access keys.
//
// Every check happens in the router, before a handler runs, rather than inside
// the handlers. That is the whole design: a scope checked on GetObject and
// missed on multipart is worse than no scope, because it reads as protection
// that is not there. One place means one place to audit, and routeTable_test
// walks every route to prove none is unguarded.
//
// Three exceptions handle their own checks, because one decision per request is
// not enough for them: batch delete decides per key, listing narrows what it
// returns rather than refusing, and ListBuckets filters. Each is marked below
// and each has its own test.

// breadth says how much of a bucket an operation reaches.
type breadth int

const (
	// reachesObject is the ordinary case: one key, which a prefix rule can
	// permit or refuse.
	reachesObject breadth = iota
	// reachesSomewhere is for operations addressing a bucket without naming a
	// key — listing it, or asking whether it exists. A prefix-scoped key may
	// do these; it simply sees less.
	reachesSomewhere
	// reachesEverything is for operations affecting the whole bucket at once,
	// which a prefix-scoped key must never be able to do.
	reachesEverything
)

// access is what a route requires of the caller.
type access struct {
	permission db.Permission
	breadth    breadth
}

var (
	// Multipart is entirely a write lifecycle: initiating, uploading a part,
	// completing, listing the parts of an upload in progress, and aborting.
	//
	// Abort is deliberately a write and not a delete, which is a departure from
	// how ILS-84 first described it. Aborting removes an incomplete upload,
	// never a stored object, and a client that retries a failed multipart must
	// be able to clean up after itself. Requiring delete would mean a
	// write-only backup key leaks parts forever, which is worse for the system
	// than the permission is tight.
	writeObject = access{db.PermissionWrite, reachesObject}
	readObject  = access{db.PermissionRead, reachesObject}
	// Deleting one key. A prefix rule can permit or refuse it.
	deleteObject = access{db.PermissionDelete, reachesObject}
	// Listing a bucket, or asking whether it exists.
	readBucket = access{db.PermissionRead, reachesSomewhere}
	// Creating and deleting a bucket both affect everything in it.
	createBucket = access{db.PermissionWrite, reachesEverything}
	deleteBucket = access{db.PermissionDelete, reachesEverything}
)

// grantFor returns the caller's grant, and whether the caller is authenticated
// at all.
//
// An unauthenticated request reaching a handler has already been approved by
// allowAnonymous, which serves reads from public buckets only. Public read wins
// over any scope, because a scope describes what a key may do and an anonymous
// request presents no key — see the note on ILS-82. Nothing here can widen
// that: allowAnonymous still restricts anonymous traffic to GET, HEAD and
// OPTIONS on a bucket explicitly marked public.
func grantFor(r *http.Request) (db.Grant, bool) {
	id, ok := IdentityFrom(r.Context())
	if !ok {
		return db.Grant{}, false
	}
	return id.Grant, true
}

// permit reports whether the request may proceed, and writes AccessDenied if
// not.
//
// The client is told only that access was denied. The reason — which key, which
// bucket, which permission — goes to the request log, because it is exactly
// what an operator needs and exactly what someone probing the server would like
// to have.
func (s *Server) permit(w http.ResponseWriter, r *http.Request, bucket, key string, want access) bool {
	grant, authenticated := grantFor(r)
	if !authenticated {
		return true
	}
	if allowed(grant, bucket, key, want) {
		return true
	}
	s.denyScope(w, r, bucket, key, want.permission)
	return false
}

// allowed applies a grant to one requirement.
func allowed(grant db.Grant, bucket, key string, want access) bool {
	switch want.breadth {
	case reachesEverything:
		return grant.AllowsWholeBucket(bucket, want.permission)
	case reachesSomewhere:
		return grant.AllowsAnywhereIn(bucket, want.permission)
	default:
		return grant.Allows(bucket, key, want.permission)
	}
}

// denyScope refuses a request and records why.
func (s *Server) denyScope(w http.ResponseWriter, r *http.Request, bucket, key string, permission db.Permission) {
	target := bucket
	if key != "" {
		target += "/" + key
	}
	reason := "the access key is not permitted to " + string(permission) + " " + target

	// Recorded on the request so the log viewer can group and explain it. The
	// first week after a key is narrowed is otherwise a guessing game about
	// which application was scoped too tightly.
	noteFailure(r.Context(), ErrAccessDenied.Code, reason)

	WriteError(w, r, ErrAccessDenied.WithMessage(
		"The access key is not permitted to perform this operation on %s.", target))
}

// Listing is the one operation that narrows rather than refuses.
//
// A key scoped to a prefix is allowed to list the bucket; it simply must not
// see outside its own prefix. Refusing instead would make the obvious per-tenant
// setup — one bucket, a prefix per tenant — unusable with the tools people
// actually have, since none of them know to ask for a prefix they were never
// told about.

// listingLimit describes how a listing must be narrowed for the caller.
type listingLimit struct {
	// unrestricted means show everything; the caller may read the whole bucket.
	unrestricted bool
	// prefixes are the only prefixes whose contents may be shown. Empty with
	// unrestricted false means the caller may read nothing here.
	prefixes []string
}

// listingLimitFor works out what the caller may see in a bucket.
func listingLimitFor(r *http.Request, bucket string) listingLimit {
	grant, authenticated := grantFor(r)
	if !authenticated {
		// Anonymous, so this is a public bucket and everything in it is public.
		return listingLimit{unrestricted: true}
	}
	prefixes := grant.ReadablePrefixes(bucket)
	if prefixes == nil {
		return listingLimit{unrestricted: true}
	}
	return listingLimit{prefixes: prefixes}
}

// showsNothing reports whether the caller may see no keys at all here.
func (l listingLimit) showsNothing() bool {
	return !l.unrestricted && len(l.prefixes) == 0
}

// allowsKey reports whether one key may appear in a listing.
func (l listingLimit) allowsKey(key string) bool {
	if l.unrestricted {
		return true
	}
	for _, prefix := range l.prefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// allowsCommonPrefix reports whether a rolled-up prefix may appear.
//
// Overlap in either direction is enough. A caller scoped to "tenant-a/logs/"
// listing with a delimiter sees the common prefix "tenant-a/", which is a real
// disclosure but a trivial one — they already know the path they were given —
// and hiding it would make a delimiter listing return nothing at all, which
// looks like the bucket is empty rather than like the key is scoped.
func (l listingLimit) allowsCommonPrefix(commonPrefix string) bool {
	if l.unrestricted {
		return true
	}
	for _, prefix := range l.prefixes {
		if strings.HasPrefix(commonPrefix, prefix) || strings.HasPrefix(prefix, commonPrefix) {
			return true
		}
	}
	return false
}

// narrow returns the prefix the database should scan, given what the caller
// asked for.
//
// Purely an optimisation for the common case: one prefix rule, and a client
// that asked for the whole bucket. Scanning only the caller's own prefix
// returns full pages instead of pages mostly filtered away. Everything else
// falls back to scanning what was asked for and filtering the result, which is
// correct either way — a page may come back short, which S3 permits, and the
// truncation state is left exactly as the database reported it so pagination
// still lands on the right key.
func (l listingLimit) narrow(requested string) string {
	if l.unrestricted || len(l.prefixes) != 1 {
		return requested
	}
	only := l.prefixes[0]
	if strings.HasPrefix(only, requested) {
		return only
	}
	return requested
}
