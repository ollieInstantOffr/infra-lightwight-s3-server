package db

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Permission is one kind of thing a key may do.
//
// Three rather than one verb per S3 operation, because an operation-level
// permission list would have to grow every time an operation is added, and the
// person granting access would have to know which S3 calls their tool makes.
// Read, write and delete are what someone actually means.
type Permission string

const (
	// PermissionRead covers getting an object and listing a bucket. Listing is
	// a read even though it returns no object bytes: knowing what is in a
	// bucket is exactly the thing a read-only key is being trusted with.
	PermissionRead Permission = "read"
	// PermissionWrite covers creating and overwriting objects, and every step
	// of a multipart upload. Overwriting destroys the previous bytes, but it
	// is what every uploader does routinely, so it belongs with write rather
	// than delete — a key that may upload but not overwrite would be an
	// unusable thing to hand to a backup tool.
	PermissionWrite Permission = "write"
	// PermissionDelete covers removing objects, and aborting a multipart upload
	// belonging to someone else. Separate from write so the common case — a
	// key that uploads and never removes — is expressible.
	PermissionDelete Permission = "delete"
)

// AllPermissions is every permission, in a stable order for display.
var AllPermissions = []Permission{PermissionRead, PermissionWrite, PermissionDelete}

// Valid reports whether p is a permission this server understands.
func (p Permission) Valid() bool { return slices.Contains(AllPermissions, p) }

// GrantRule permits a set of actions on one bucket, optionally narrowed to keys
// under a prefix.
type GrantRule struct {
	Bucket string `json:"bucket"`
	// Prefix narrows the rule to keys beginning with it. Empty means the whole
	// bucket. It is a byte prefix, not a path segment: a prefix of "logs"
	// matches "logs/a" and also "logsomething", which is how S3 prefixes work
	// everywhere else in this server and is worth being consistent about even
	// though it occasionally surprises people.
	Prefix      string       `json:"prefix"`
	Permissions []Permission `json:"permissions"`
}

// grants reports whether this rule permits perm.
func (r GrantRule) grants(perm Permission) bool {
	return slices.Contains(r.Permissions, perm)
}

// covers reports whether key falls inside this rule's prefix.
func (r GrantRule) covers(key string) bool {
	return strings.HasPrefix(key, r.Prefix)
}

// Grant is everything one access key is allowed to do.
type Grant struct {
	// Unrestricted keys may do anything to any bucket. Every key that existed
	// before scoping was introduced is unrestricted, so enabling this feature
	// changed nobody's access. It is a separate flag rather than a wildcard
	// rule so that "this key is not scoped at all" is visibly different from
	// "this key happens to be scoped to everything", which matters when someone
	// is auditing a list of keys.
	Unrestricted bool `json:"unrestricted"`
	// Rules are consulted only when Unrestricted is false. An empty list means
	// the key can do nothing at all — a valid state, and the way to disable a
	// key without destroying the record of it.
	Rules []GrantRule `json:"rules,omitempty"`
}

// UnrestrictedGrant is the grant every key had before scoping existed, and the
// default for a key created without one.
func UnrestrictedGrant() Grant { return Grant{Unrestricted: true} }

// Allows reports whether the key may perform perm on this exact object.
//
// The zero Grant — neither unrestricted nor carrying any rule — allows nothing.
// That is deliberate: an authorizer that fails open is worse than no authorizer,
// because it reads as protection that is not there.
func (g Grant) Allows(bucket, key string, perm Permission) bool {
	if g.Unrestricted {
		return true
	}
	for _, rule := range g.Rules {
		if rule.Bucket == bucket && rule.covers(key) && rule.grants(perm) {
			return true
		}
	}
	return false
}

// AllowsAnywhereIn reports whether the key may perform perm somewhere in the
// bucket, without saying where.
//
// This is the check for operations that address a bucket rather than an object:
// listing it, or asking whether it exists. A key scoped to one prefix may still
// list the bucket — it just sees only its own prefix, which ConstrainListing
// arranges.
func (g Grant) AllowsAnywhereIn(bucket string, perm Permission) bool {
	if g.Unrestricted {
		return true
	}
	for _, rule := range g.Rules {
		if rule.Bucket == bucket && rule.grants(perm) {
			return true
		}
	}
	return false
}

// AllowsWholeBucket reports whether the key may perform perm across the entire
// bucket, with no prefix narrowing it.
//
// Creating and deleting a bucket affect everything in it, so a key confined to
// a prefix must not be able to do either. Requiring an unprefixed rule is what
// separates "may write under tenant-a/" from "may create and destroy the
// bucket those keys live in".
func (g Grant) AllowsWholeBucket(bucket string, perm Permission) bool {
	if g.Unrestricted {
		return true
	}
	for _, rule := range g.Rules {
		if rule.Bucket == bucket && rule.Prefix == "" && rule.grants(perm) {
			return true
		}
	}
	return false
}

// Visible reports whether the key may know the bucket exists at all.
//
// Used to filter ListBuckets. A key that cannot see a bucket should not learn
// its name from a listing: bucket names leak what a deployment is for, and a
// listing that shows everything makes a scoped key look wider than it is to
// whoever is holding it.
func (g Grant) Visible(bucket string) bool {
	if g.Unrestricted {
		return true
	}
	for _, rule := range g.Rules {
		if rule.Bucket == bucket && len(rule.Permissions) > 0 {
			return true
		}
	}
	return false
}

// ReadablePrefixes returns the prefixes the key may read within a bucket.
//
// A nil result means unrestricted within this bucket — every key is readable.
// An empty non-nil result means nothing is. Callers listing a bucket use this
// to narrow what they return; see ConstrainListing, which is where the awkward
// cases actually live.
func (g Grant) ReadablePrefixes(bucket string) []string {
	if g.Unrestricted {
		return nil
	}
	prefixes := []string{}
	for _, rule := range g.Rules {
		if rule.Bucket != bucket || !rule.grants(PermissionRead) {
			continue
		}
		if rule.Prefix == "" {
			// The whole bucket is readable, so any other prefix rule on it is
			// redundant and would only complicate the listing constraint.
			return nil
		}
		if !slices.Contains(prefixes, rule.Prefix) {
			prefixes = append(prefixes, rule.Prefix)
		}
	}
	return prefixes
}

// Validate reports what is wrong with a grant, for the console to show before
// it is stored rather than for the authorizer to discover afterwards.
func (g Grant) Validate() error {
	if g.Unrestricted {
		if len(g.Rules) > 0 {
			return fmt.Errorf("an unrestricted key cannot also carry rules")
		}
		return nil
	}
	for i, rule := range g.Rules {
		// Only the shape is checked here. Full S3 bucket-name validation lives
		// in the s3api package, which imports this one, and the console
		// validates the name against the buckets that actually exist — which
		// is the more useful check anyway, since a rule naming a bucket that
		// was never created grants nothing to nobody.
		if rule.Bucket == "" {
			return fmt.Errorf("rule %d names no bucket", i+1)
		}
		if len(rule.Bucket) > 63 {
			return fmt.Errorf("rule %d: bucket name is too long", i+1)
		}
		if len(rule.Permissions) == 0 {
			return fmt.Errorf("rule %d for %q grants no permissions", i+1, rule.Bucket)
		}
		for _, perm := range rule.Permissions {
			if !perm.Valid() {
				return fmt.Errorf("rule %d for %q: %q is not a permission", i+1, rule.Bucket, perm)
			}
		}
	}
	return nil
}

// Describe renders a grant as a short human sentence, for logs and for the
// audit trail. The console draws its own richer version.
func (g Grant) Describe() string {
	if g.Unrestricted {
		return "unrestricted"
	}
	if len(g.Rules) == 0 {
		return "no access"
	}
	parts := make([]string, 0, len(g.Rules))
	for _, rule := range g.Rules {
		target := rule.Bucket
		if rule.Prefix != "" {
			target += "/" + rule.Prefix
		}
		perms := make([]string, 0, len(rule.Permissions))
		for _, perm := range rule.Permissions {
			perms = append(perms, string(perm))
		}
		parts = append(parts, target+" ("+strings.Join(perms, ", ")+")")
	}
	return strings.Join(parts, "; ")
}

// marshalGrant encodes a grant for storage.
func marshalGrant(g Grant) ([]byte, error) {
	encoded, err := json.Marshal(g)
	if err != nil {
		return nil, fmt.Errorf("encode access scope: %w", err)
	}
	return encoded, nil
}

// unmarshalGrant decodes a stored grant.
//
// A document that cannot be decoded produces an error rather than a permissive
// default, so a corrupted or hand-edited row denies access instead of granting
// it. The caller turns that into a refused request.
func unmarshalGrant(raw []byte) (Grant, error) {
	if len(raw) == 0 {
		return Grant{}, fmt.Errorf("access scope is empty")
	}

	// Decoded as a map first, to catch the documents that would otherwise
	// decode into a zero Grant without complaint. JSON null is the one that
	// matters: unmarshalling it into a struct succeeds and leaves every field
	// at its zero value, which happens to mean "no access". That fails closed,
	// which is the right direction, but silently — and a key that denies
	// everything for a reason nobody can see is a bad afternoon.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Grant{}, fmt.Errorf("decode access scope: %w", err)
	}
	if fields == nil {
		return Grant{}, fmt.Errorf("access scope is null")
	}
	if _, ok := fields["unrestricted"]; !ok {
		return Grant{}, fmt.Errorf("access scope does not say whether the key is unrestricted")
	}

	var g Grant
	if err := json.Unmarshal(raw, &g); err != nil {
		return Grant{}, fmt.Errorf("decode access scope: %w", err)
	}
	return g, nil
}
