package admin

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
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
		reqPath := r.URL.Path

		if strings.HasPrefix(reqPath, "/admin") {
			reqPath = strings.TrimPrefix(reqPath, "/admin")
			if reqPath == "" {
				reqPath = "/"
			}
		}

		if reqPath != "/" && !strings.HasSuffix(reqPath, "/") {
			if f, err := subFS.Open(strings.TrimPrefix(reqPath, "/")); err == nil {
				_ = f.Close()
				r.URL.Path = reqPath
				fileServer.ServeHTTP(w, r)
				return
			}
			if isStaticAsset(reqPath) {
				http.NotFound(w, r)
				return
			}
		}

		// For directory paths, serve index.html directly to avoid
		// http.FileServer's redirect from /index.html -> ./
		indexPath := strings.TrimPrefix(reqPath, "/")
		if indexPath == "" {
			indexPath = "index.html"
		} else {
			indexPath = strings.TrimSuffix(indexPath, "/") + "/index.html"
		}

		if err := serveFile(w, subFS, indexPath); err == nil {
			return
		}

		// Fallback to root index.html for SPA routing
		_ = serveFile(w, subFS, "index.html")
	})
}

// isStaticAsset reports whether a path addresses a build artifact rather than
// a client-side route. A missing artifact must 404 instead of falling through
// to the SPA shell: chunk filenames change every build, so a request left over
// from an older build would otherwise receive HTML under a .js/.css URL, which
// the browser refuses as a stylesheet and cannot parse as a script -- rendering
// an unstyled page with no visible error.
func isStaticAsset(reqPath string) bool {
	return strings.HasPrefix(reqPath, "/_next/") || path.Ext(reqPath) != ""
}

func serveFile(w http.ResponseWriter, fsys fs.FS, name string) error {
	f, err := fsys.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.Copy(w, f)
	return nil
}
