package config

import (
	"fmt"
	"sort"
	"strings"
)

// Role selects which parts of the server this process runs.
//
// One binary rather than several: the build, the image and the release stay
// single, the roles cannot drift apart in the libraries they carry, and the
// split stays reversible — RoleAll is still there if this shape turns out to be
// wrong. See docs/services.md.
type Role string

const (
	// RoleAll runs everything in one process. The default, so an existing
	// deployment upgrading to this version keeps working untouched.
	RoleAll Role = "all"
	// RoleS3 serves only the S3 API.
	RoleS3 Role = "s3"
	// RoleConsole serves only the admin console.
	RoleConsole Role = "console"
	// RoleWorker runs the background sweeps, the metrics and log flushers and
	// the alert engine, and listens on no port at all.
	RoleWorker Role = "worker"
)

// roles is the set, in the order help text should list them.
var roles = []Role{RoleAll, RoleS3, RoleConsole, RoleWorker}

// ParseRole reads a role name, defaulting to RoleAll when empty.
func ParseRole(raw string) (Role, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	if name == "" {
		return RoleAll, nil
	}
	for _, role := range roles {
		if Role(name) == role {
			return role, nil
		}
	}
	names := make([]string, 0, len(roles))
	for _, role := range roles {
		names = append(names, string(role))
	}
	sort.Strings(names)
	return "", fmt.Errorf("unknown ROLE %q: expected one of %s", raw, strings.Join(names, ", "))
}

// ServesS3 reports whether this role listens on the S3 port.
func (r Role) ServesS3() bool { return r == RoleAll || r == RoleS3 }

// ServesConsole reports whether this role listens on the console port.
func (r Role) ServesConsole() bool { return r == RoleAll || r == RoleConsole }

// RunsWorkers reports whether this role runs the background workers.
func (r Role) RunsWorkers() bool { return r == RoleAll || r == RoleWorker }

// NeedsCredentialsKey reports whether this role must be able to decrypt stored
// S3 secrets.
//
// The S3 API needs it to check a signature; the console needs it to show a
// newly created key once. The worker never does, which is the one real
// reduction in blast radius the split buys.
func (r Role) NeedsCredentialsKey() bool { return r.ServesS3() || r.ServesConsole() }

// String makes Role printable in logs and errors.
func (r Role) String() string { return string(r) }
