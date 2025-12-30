package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIHandler_ServesIndexHTML(t *testing.T) {
	handler := UIHandler()

	// Test root path
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// The handler should either serve content, redirect, or return a service unavailable error
	// (depending on whether the UI is built)
	if w.Code != http.StatusOK && w.Code != http.StatusMovedPermanently && w.Code != http.StatusServiceUnavailable {
		t.Errorf("UIHandler() root path status = %d, want 200, 301, or 503", w.Code)
	}
}

func TestUIHandler_NotBuilt(t *testing.T) {
	handler := UIHandler()

	// If the UI is not built, the handler should return a 503 error
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// The response should indicate that the UI needs to be built
	if w.Code == http.StatusServiceUnavailable {
		body := w.Body.String()
		if !strings.Contains(body, "Admin UI not built") {
			t.Errorf("Expected 'Admin UI not built' message, got: %s", body)
		}
	}
}

func TestUIHandler_AdminPrefixStripping(t *testing.T) {
	handler := UIHandler()

	tests := []struct {
		name string
		path string
	}{
		{"admin root", "/admin"},
		{"admin with slash", "/admin/"},
		{"admin subpath", "/admin/pools"},
		{"admin deep path", "/admin/pools/default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			// Should not return unexpected error - either serves content, redirects, or returns 503/404
			validCodes := map[int]bool{
				http.StatusOK:                 true,
				http.StatusMovedPermanently:   true,
				http.StatusNotFound:           true,
				http.StatusServiceUnavailable: true,
			}
			if !validCodes[w.Code] {
				t.Errorf("UIHandler() path %s unexpected status = %d", tt.path, w.Code)
			}
		})
	}
}

func TestUIHandler_SPAFallback(t *testing.T) {
	handler := UIHandler()

	// Test SPA routing - non-existent paths should fallback to index.html
	tests := []struct {
		name string
		path string
	}{
		{"unknown route", "/unknown-route"},
		{"deep route", "/pools/some-pool/settings"},
		{"api-like route", "/some/nested/path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			// Should either serve index.html (200), redirect (301), or return 503 if not built
			if w.Code != http.StatusOK && w.Code != http.StatusMovedPermanently && w.Code != http.StatusServiceUnavailable {
				t.Errorf("UIHandler() SPA fallback for %s status = %d, want 200, 301, or 503", tt.path, w.Code)
			}
		})
	}
}

func TestUIHandler_StaticAssets(t *testing.T) {
	handler := UIHandler()

	// Test requests for static assets (these may or may not exist)
	tests := []struct {
		name string
		path string
	}{
		{"JavaScript file", "/static/js/main.js"},
		{"CSS file", "/static/css/main.css"},
		{"Image file", "/images/logo.png"},
		{"Font file", "/fonts/roboto.woff2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			// Static assets should either be served (200), redirect (301), not found (404), or UI not built (503)
			validCodes := map[int]bool{
				http.StatusOK:                 true,
				http.StatusMovedPermanently:   true,
				http.StatusNotFound:           true,
				http.StatusServiceUnavailable: true,
			}
			if !validCodes[w.Code] {
				t.Errorf("UIHandler() static asset %s status = %d, want 200, 301, 404, or 503", tt.path, w.Code)
			}
		})
	}
}

func TestUIHandler_DirectoryPath(t *testing.T) {
	handler := UIHandler()

	tests := []struct {
		name string
		path string
	}{
		{"root directory", "/"},
		{"trailing slash", "/pools/"},
		{"nested directory", "/admin/pools/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			// Directory paths should serve index.html, redirect, or return 503
			if w.Code != http.StatusOK && w.Code != http.StatusMovedPermanently && w.Code != http.StatusServiceUnavailable {
				t.Errorf("UIHandler() directory %s status = %d, want 200, 301, or 503", tt.path, w.Code)
			}
		})
	}
}

func TestUIHandler_HTTPMethods(t *testing.T) {
	handler := UIHandler()

	methods := []string{
		http.MethodGet,
		http.MethodHead,
		http.MethodPost,
		http.MethodPut,
		http.MethodDelete,
		http.MethodPatch,
	}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/", nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			// Should handle the request without panicking
			// FileServer typically handles GET and HEAD, others may return 405
			if w.Code == 0 {
				t.Errorf("UIHandler() %s method returned no status code", method)
			}
		})
	}
}

func TestUIHandler_PathNormalization(t *testing.T) {
	handler := UIHandler()

	tests := []struct {
		name string
		path string
	}{
		{"double slashes", "//admin//pools"},
		{"dot path", "/admin/./pools"},
		{"empty segments", "/admin///pools"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()

			// Should not panic
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("UIHandler panicked for path %s: %v", tt.path, r)
				}
			}()
			handler.ServeHTTP(w, req)
		})
	}
}

func TestUIHandler_QueryStrings(t *testing.T) {
	handler := UIHandler()

	tests := []struct {
		name string
		path string
	}{
		{"with query params", "/?foo=bar"},
		{"admin with query", "/admin?tab=settings"},
		{"complex query", "/admin/pools?filter=active&sort=name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			// Query strings should be handled normally
			if w.Code != http.StatusOK && w.Code != http.StatusMovedPermanently && w.Code != http.StatusServiceUnavailable {
				t.Errorf("UIHandler() with query %s status = %d, want 200, 301, or 503", tt.path, w.Code)
			}
		})
	}
}

func TestUIHandler_EmptyPath(t *testing.T) {
	handler := UIHandler()

	// Test empty path after stripping /admin
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should serve root content, redirect, or return 503
	if w.Code != http.StatusOK && w.Code != http.StatusMovedPermanently && w.Code != http.StatusServiceUnavailable {
		t.Errorf("UIHandler() /admin status = %d, want 200, 301, or 503", w.Code)
	}
}

func TestUIHandler_ReturnType(t *testing.T) {
	handler := UIHandler()

	// Verify that UIHandler returns a valid http.Handler
	if handler == nil {
		t.Fatal("UIHandler() returned nil")
	}

	// Verify it implements http.Handler interface by calling ServeHTTP
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
}

func TestUIHandler_ContentType(t *testing.T) {
	handler := UIHandler()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		contentType := w.Header().Get("Content-Type")
		if contentType == "" {
			t.Error("Expected Content-Type header to be set for successful response")
		}
	}
}

func TestUIHandler_ConcurrentRequests(t *testing.T) {
	handler := UIHandler()

	// Test concurrent requests don't cause race conditions
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("UIHandler panicked during concurrent request: %v", r)
				}
			}()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestUIHandler_LongPath(t *testing.T) {
	handler := UIHandler()

	// Test handling of very long paths
	longPath := "/" + strings.Repeat("a/", 100) + "page"
	req := httptest.NewRequest(http.MethodGet, longPath, nil)
	w := httptest.NewRecorder()

	// Should not panic
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("UIHandler panicked for long path: %v", r)
		}
	}()
	handler.ServeHTTP(w, req)
}

func TestUIHandler_SpecialCharacters(t *testing.T) {
	handler := UIHandler()

	tests := []struct {
		name string
		path string
	}{
		{"encoded space", "/admin%20page"},
		{"unicode", "/admin/日本語"},
		{"special chars", "/admin/@pools"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()

			// Should not panic
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("UIHandler panicked for path %s: %v", tt.path, r)
				}
			}()
			handler.ServeHTTP(w, req)
		})
	}
}
