package admin

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:ui/out
var uiFS embed.FS

// UIHandler returns an http.Handler that serves the embedded admin UI.
// The UI is expected to be built as a static export in ui/out/.
func UIHandler() http.Handler {
	subFS, err := fs.Sub(uiFS, "ui/out")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "Admin UI not built. Run: make build-admin-ui", http.StatusServiceUnavailable)
		})
	}

	// Check if UI was built by looking for index.html
	if _, err := subFS.Open("index.html"); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "Admin UI not built. Run: make build-admin-ui", http.StatusServiceUnavailable)
		})
	}

	fileServer := http.FileServer(http.FS(subFS))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Strip /admin prefix if present
		if strings.HasPrefix(path, "/admin") {
			path = strings.TrimPrefix(path, "/admin")
			if path == "" {
				path = "/"
			}
		}

		// Try to serve the exact file
		if path != "/" && !strings.HasSuffix(path, "/") {
			// Check if file exists
			if f, err := subFS.Open(strings.TrimPrefix(path, "/")); err == nil {
				_ = f.Close()
				r.URL.Path = path
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		// For directory paths, try index.html
		indexPath := strings.TrimPrefix(path, "/")
		if indexPath == "" {
			indexPath = "index.html"
		} else {
			indexPath = strings.TrimSuffix(indexPath, "/") + "/index.html"
		}

		if f, err := subFS.Open(indexPath); err == nil {
			_ = f.Close()
			r.URL.Path = "/" + indexPath
			fileServer.ServeHTTP(w, r)
			return
		}

		// Fallback to root index.html for SPA routing
		r.URL.Path = "/index.html"
		fileServer.ServeHTTP(w, r)
	})
}
