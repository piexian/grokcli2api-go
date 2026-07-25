package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Futureppo/grokcli2api-go/internal/config"
)

func TestSPAStaticHostingAndFallback(t *testing.T) {
	dist := writeTestWebDist(t)
	handler := newWebTestMux(config.Config{WebDist: dist})

	fallback := httptest.NewRecorder()
	handler.ServeHTTP(fallback, httptest.NewRequest(http.MethodGet, "/admin/accounts", nil))
	if fallback.Code != http.StatusOK || !strings.Contains(fallback.Body.String(), `<div id="root"></div>`) {
		t.Fatalf("SPA fallback status = %d, body = %q", fallback.Code, fallback.Body.String())
	}
	if got := fallback.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("SPA fallback Content-Type = %q", got)
	}
	if got := fallback.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("SPA fallback Cache-Control = %q", got)
	}

	asset := httptest.NewRecorder()
	handler.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/assets/app-a1b2c3.js", nil))
	if asset.Code != http.StatusOK || asset.Body.String() != `console.log("admin");` {
		t.Fatalf("asset status = %d, body = %q", asset.Code, asset.Body.String())
	}
	if got := asset.Header().Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Fatalf("asset Content-Type = %q", got)
	}
	if got := asset.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("asset Cache-Control = %q", got)
	}

	api := httptest.NewRecorder()
	handler.ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/v1/not-a-route", nil))
	if api.Code != http.StatusNotFound || strings.Contains(api.Body.String(), `<div id="root"></div>`) {
		t.Fatalf("unknown API status = %d, body = %q", api.Code, api.Body.String())
	}

	protectedHandler := newWebTestMux(config.Config{WebDist: dist, APIKeys: []string{"api-key"}})
	protected := httptest.NewRecorder()
	protectedHandler.ServeHTTP(protected, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if protected.Code != http.StatusUnauthorized || strings.Contains(protected.Body.String(), `<div id="root"></div>`) {
		t.Fatalf("protected API status = %d, body = %q", protected.Code, protected.Body.String())
	}
}

func TestSPARootUsesContentNegotiation(t *testing.T) {
	handler := newWebTestMux(config.Config{WebDist: writeTestWebDist(t)})

	api := httptest.NewRecorder()
	handler.ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/", nil))
	if api.Code != http.StatusOK || !strings.Contains(api.Body.String(), `"name":"grokcli2api-go"`) {
		t.Fatalf("service info status = %d, body = %q", api.Code, api.Body.String())
	}

	browserRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	browserRequest.Header.Set("Accept", "text/html,application/xhtml+xml")
	browser := httptest.NewRecorder()
	handler.ServeHTTP(browser, browserRequest)
	if browser.Code != http.StatusOK || !strings.Contains(browser.Body.String(), `<div id="root"></div>`) {
		t.Fatalf("browser root status = %d, body = %q", browser.Code, browser.Body.String())
	}
}

func TestRuntimeConfig(t *testing.T) {
	handler := newWebTestMux(config.Config{
		WebDist:       "off",
		PublicBaseURL: "https://grok.example.test/base",
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runtime-config.js", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/javascript") {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	want := "window.__GROK2API_RUNTIME_CONFIG__ = {\"apiBaseUrl\":\"\",\"publicApiBaseUrl\":\"https://grok.example.test/base\"};\n"
	if rec.Body.String() != want {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestSPAHostingOff(t *testing.T) {
	handler := newWebTestMux(config.Config{WebDist: "off"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

func newWebTestMux(cfg config.Config) http.Handler {
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.routes()
	return s.mux
}

func writeTestWebDist(t *testing.T) string {
	t.Helper()
	dist := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dist, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte(`<!doctype html><div id="root"></div>`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "assets", "app-a1b2c3.js"), []byte(`console.log("admin");`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dist
}
