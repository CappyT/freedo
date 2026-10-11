// Package web serves the dashboard that the build embeds in the daemon.
//
// dist holds the output of the SvelteKit build in dashboard/. The repository
// tracks only dist/.gitkeep, so go build works on a machine without Node. A binary
// built that way answers every page request with 404, and the API works as before.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed all:dist
var dist embed.FS

// Files returns the embedded dashboard with dist as its root.
func Files() fs.FS {
	files, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // dist is part of the embed pattern, so it always exists
	}
	return files
}

const (
	immutableDir  = "_app/immutable/"
	immutableCtrl = "public, max-age=31536000, immutable"
	missingBuild  = "the dashboard is not part of this build\n"
)

// contentTypes covers the files of the build. mime.TypeByExtension reads the
// tables of the host, which a minimal container image lacks.
var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".webmanifest": "application/manifest+json",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".ico":         "image/x-icon",
	".woff2":       "font/woff2",
	".txt":         "text/plain; charset=utf-8",
}

// Handler serves files and sends every /api path to api. A path that names no
// file and has no extension is a page of the app: it gets index.html, or a redirect
// to the root of the app when it is nested, because the page uses relative URLs and
// the browser would resolve them against the wrong directory.
func Handler(files fs.FS, api http.Handler) http.Handler {
	return &handler{files: files, api: api}
}

type handler struct {
	files fs.FS
	api   http.Handler
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		h.api.ServeHTTP(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	if h.serve(w, r, name) {
		return
	}
	if name != "index.html" && path.Ext(name) != "" {
		http.NotFound(w, r)
		return
	}
	// The slashes after the first one count the directories between the request
	// and the root of the app, wherever a proxy mounts that root.
	if depth := strings.Count(r.URL.Path, "/") - 1; depth > 0 {
		w.Header().Set("Location", strings.Repeat("../", depth))
		w.WriteHeader(http.StatusFound)
		return
	}
	if !h.serve(w, r, "index.html") {
		http.Error(w, missingBuild, http.StatusNotFound)
	}
}

// serve answers with the file name and reports whether the file exists.
func (h *handler) serve(w http.ResponseWriter, r *http.Request, name string) bool {
	if !fs.ValidPath(name) || strings.HasPrefix(name, ".") || strings.Contains(name, "/.") {
		return false
	}
	f, err := h.files.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.IsDir() {
		return false
	}
	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return true
	}
	header := w.Header()
	header.Set("Content-Type", contentType(name))
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256(data)))
	if strings.HasPrefix(name, immutableDir) {
		header.Set("Cache-Control", immutableCtrl)
	} else {
		header.Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	return true
}

func contentType(name string) string {
	ext := path.Ext(name)
	if t, ok := contentTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}
