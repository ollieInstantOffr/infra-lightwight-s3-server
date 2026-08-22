// Package web carries the built console interface inside the binary.
//
// The Vite build writes into dist/, which is embedded at compile time so the
// shipped artefact stays a single file with no Node runtime and no static
// directory to deploy alongside it.
package web

import (
	"embed"
	"errors"
	"io/fs"
)

// The build output is embedded if present. The `all:` prefix is required
// because Vite emits files beginning with an underscore, which the default
// embed rules skip — and a silently missing asset is far harder to diagnose
// than a missing directory.
//
//go:embed all:dist
var dist embed.FS

// ErrNotBuilt means the frontend was not built before compiling.
var ErrNotBuilt = errors.New("the web interface was not built (run the Vite build first)")

// Dist returns the built app rooted at dist/.
func Dist() (fs.FS, error) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, ErrNotBuilt
	}
	// A directory with no index.html is a build that did not run, which is
	// worth reporting as such rather than serving 404s.
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, ErrNotBuilt
	}
	return sub, nil
}
