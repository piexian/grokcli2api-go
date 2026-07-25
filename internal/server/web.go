package server

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	embeddedweb "github.com/Futureppo/grokcli2api-go"
)

const immutableCacheControl = "public, max-age=31536000, immutable"

type spaHandler struct {
	files fs.FS
}

func newSPAHandler(webDist string) http.Handler {
	webDist = strings.TrimSpace(webDist)
	if webDist == "" {
		webDist = "frontend/dist"
	}
	if strings.EqualFold(webDist, "off") {
		return nil
	}

	var files fs.FS
	if info, err := os.Stat(filepath.Join(webDist, "index.html")); err == nil && !info.IsDir() {
		files = os.DirFS(webDist)
	} else {
		files = embeddedweb.Embedded()
	}
	if files == nil {
		return nil
	}
	if info, err := fs.Stat(files, "index.html"); err != nil || info.IsDir() {
		return nil
	}
	return &spaHandler{files: files}
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	content, err := fs.ReadFile(h.files, name)
	if err != nil {
		name = "index.html"
		content, err = fs.ReadFile(h.files, name)
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}

	switch {
	case name == "index.html":
		w.Header().Set("Cache-Control", "no-cache")
	case strings.HasPrefix(name, "assets/"):
		w.Header().Set("Cache-Control", immutableCacheControl)
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(content))
}

func (s *Server) runtimeConfig(w http.ResponseWriter, _ *http.Request) {
	payload, err := json.Marshal(struct {
		APIBaseURL       string `json:"apiBaseUrl"`
		PublicAPIBaseURL string `json:"publicApiBaseUrl"`
	}{
		APIBaseURL:       "",
		PublicAPIBaseURL: s.cfg.PublicBaseURL,
	})
	if err != nil {
		http.Error(w, "encode runtime config", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	_, _ = w.Write([]byte("window.__GROK2API_RUNTIME_CONFIG__ = "))
	_, _ = w.Write(payload)
	_, _ = w.Write([]byte(";\n"))
}

func acceptsHTML(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}

func isAPIPath(requestPath string) bool {
	return requestPath == "/v1" || strings.HasPrefix(requestPath, "/v1/")
}
