package main

import (
	"io/fs"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/web"
)

// consoleAssets serves the built single-page app from inside the binary.
//
// Returns nil when the frontend was not built into this binary, in which case
// the API still works and the console root explains itself. That keeps a
// Go-only build usable rather than failing at startup.
func consoleAssets(log *slog.Logger) http.Handler {
	dist, err := web.Dist()
	if err != nil {
		log.Warn("web interface not built into this binary; serving the API only", "reason", err)
		return nil
	}
	return spaHandler{files: http.FS(dist), fsys: dist}
}

// spaHandler serves static files, falling back to index.html.
//
// The fallback is what makes client-side routing survive a hard refresh: a
// browser asking for /buckets/photos must receive the app, not a 404, and let
// the router work out what to render.
type spaHandler struct {
	files http.FileSystem
	fsys  fs.FS
}

func (h spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requested := strings.TrimPrefix(r.URL.Path, "/")
	if requested == "" {
		requested = "index.html"
	}

	if file, err := h.files.Open(requested); err == nil {
		defer file.Close()
		if info, err := file.Stat(); err == nil && !info.IsDir() {
			// Hashed asset filenames change whenever their contents do, so they
			// are safe to cache indefinitely. index.html never is.
			if strings.HasPrefix(requested, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			http.ServeContent(w, r, requested, info.ModTime(), file)
			return
		}
	}

	index, err := h.files.Open("index.html")
	if err != nil {
		http.Error(w, "the web interface is not available", http.StatusNotFound)
		return
	}
	defer index.Close()
	info, err := index.Stat()
	if err != nil {
		http.Error(w, "the web interface is not available", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", info.ModTime(), index)
}
