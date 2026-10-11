package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// site is a small synthetic build output.
func site() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                           {Data: []byte("<!doctype html><title>app</title>")},
		"favicon.svg":                          {Data: []byte("<svg/>")},
		"favicon.ico":                          {Data: []byte("ico")},
		"manifest.webmanifest":                 {Data: []byte("{}")},
		"_app/version.json":                    {Data: []byte(`{"version":"1"}`)},
		"_app/immutable/entry/start.abc.js":    {Data: []byte("export {}")},
		"_app/immutable/assets/app.abc.css":    {Data: []byte("body{}")},
		"_app/immutable/assets/sora.abc.woff2": {Data: []byte("wOF2")},
		"api/shadow.json":                      {Data: []byte("shadow")},
		".hidden":                              {Data: []byte("secret")},
	}
}

var api = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Api", r.Method+" "+r.URL.Path)
	w.WriteHeader(http.StatusTeapot)
})

func get(t *testing.T, h http.Handler, method, target string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRootServesIndexWithoutCaching(t *testing.T) {
	rec := get(t, Handler(site(), api), http.MethodGet, "/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>app</title>") {
		t.Fatalf("GET /: %d %q, want index.html", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache for the HTML", got)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing X-Content-Type-Options: nosniff")
	}
}

func TestContentTypesAndCacheRules(t *testing.T) {
	h := Handler(site(), api)
	for _, tc := range []struct{ path, contentType, cache string }{
		{"/_app/immutable/entry/start.abc.js", "text/javascript; charset=utf-8", immutableCtrl},
		{"/_app/immutable/assets/app.abc.css", "text/css; charset=utf-8", immutableCtrl},
		{"/_app/immutable/assets/sora.abc.woff2", "font/woff2", immutableCtrl},
		{"/_app/version.json", "application/json", "no-cache"},
		{"/favicon.svg", "image/svg+xml", "no-cache"},
		{"/favicon.ico", "image/x-icon", "no-cache"},
		{"/manifest.webmanifest", "application/manifest+json", "no-cache"},
		{"/index.html", "text/html; charset=utf-8", "no-cache"},
	} {
		rec := get(t, h, http.MethodGet, tc.path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: %d", tc.path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Content-Type"); got != tc.contentType {
			t.Errorf("GET %s: Content-Type = %q, want %q", tc.path, got, tc.contentType)
		}
		if got := rec.Header().Get("Cache-Control"); got != tc.cache {
			t.Errorf("GET %s: Cache-Control = %q, want %q", tc.path, got, tc.cache)
		}
	}
}

func TestConditionalRequestAnswers304(t *testing.T) {
	h := Handler(site(), api)
	first := get(t, h, http.MethodGet, "/favicon.svg")
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	again := get(t, h, http.MethodGet, "/favicon.svg", "If-None-Match", etag)
	if again.Code != http.StatusNotModified {
		t.Fatalf("GET with a matching ETag: %d, want 304", again.Code)
	}
}

func TestHeadHasNoBody(t *testing.T) {
	rec := get(t, Handler(site(), api), http.MethodHead, "/")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD /: %d with %d body bytes", rec.Code, rec.Body.Len())
	}
}

func TestUnknownPageFallsBackToIndex(t *testing.T) {
	rec := get(t, Handler(site(), api), http.MethodGet, "/settings")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>app</title>") {
		t.Fatalf("GET /settings: %d %q, want index.html", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q", got)
	}
}

// A nested page would resolve the relative asset URLs of index.html against the
// wrong directory. The redirect is relative, so it holds under any proxy prefix.
func TestNestedPageRedirectsToTheAppRoot(t *testing.T) {
	h := Handler(site(), api)
	for target, want := range map[string]string{
		"/zones/1":   "../",
		"/zones/1/":  "../../",
		"/a/b/c":     "../../",
		"/settings/": "../",
	} {
		rec := get(t, h, http.MethodGet, target)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != want {
			t.Errorf("GET %s: %d Location %q, want 302 %q", target, rec.Code, rec.Header().Get("Location"), want)
		}
	}
}

func TestMissingFileIs404NotIndex(t *testing.T) {
	h := Handler(site(), api)
	for _, target := range []string{"/missing.js", "/_app/immutable/gone.abc.js", "/zones/app.css"} {
		if rec := get(t, h, http.MethodGet, target); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", target, rec.Code)
		}
	}
}

func TestDirectoriesAndDotFilesAreNotServed(t *testing.T) {
	h := Handler(site(), api)
	for _, target := range []string{"/.hidden", "/_app/.hidden"} {
		if rec := get(t, h, http.MethodGet, target); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", target, rec.Code)
		}
	}
	if rec := get(t, h, http.MethodGet, "/_app/immutable"); strings.Contains(rec.Body.String(), "start.abc.js") {
		t.Errorf("GET /_app/immutable listed a directory: %q", rec.Body.String())
	}
}

func TestPathTraversalStaysInsideTheBuild(t *testing.T) {
	h := Handler(site(), api)
	for _, target := range []string{"/../../etc/passwd", "/_app/../../index.html", "/%2e%2e/%2e%2e/etc/passwd"} {
		rec := get(t, h, http.MethodGet, target)
		if rec.Code >= http.StatusInternalServerError || strings.Contains(rec.Body.String(), "root:") {
			t.Errorf("GET %s: %d %q", target, rec.Code, rec.Body.String())
		}
	}
}

func TestAPIPathsAlwaysReachTheAPI(t *testing.T) {
	h := Handler(site(), api)
	for _, target := range []string{"/api", "/api/", "/api/health", "/api/shadow.json", "/api/unknown/path"} {
		rec := get(t, h, http.MethodGet, target)
		if rec.Code != http.StatusTeapot {
			t.Errorf("GET %s: %d, want the API handler", target, rec.Code)
		}
	}
	rec := get(t, h, http.MethodPost, "/api/zones/1/setpoint")
	if rec.Code != http.StatusTeapot || rec.Header().Get("X-Api") != "POST /api/zones/1/setpoint" {
		t.Errorf("POST /api/...: %d %q, want the API handler", rec.Code, rec.Header().Get("X-Api"))
	}
	// A path that only starts with the letters of /api is a page.
	if rec := get(t, h, http.MethodGet, "/apix"); rec.Code != http.StatusOK {
		t.Errorf("GET /apix: %d, want the index", rec.Code)
	}
}

func TestOtherMethodsOnPagesAreRefused(t *testing.T) {
	rec := get(t, Handler(site(), api), http.MethodPost, "/")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST /: %d Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestBuildWithoutDashboardAnswers404(t *testing.T) {
	h := Handler(fstest.MapFS{".gitkeep": {}}, api)
	for _, target := range []string{"/", "/settings"} {
		rec := get(t, h, http.MethodGet, target)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "dashboard") {
			t.Errorf("GET %s: %d %q, want 404 that names the dashboard", target, rec.Code, rec.Body.String())
		}
	}
	if rec := get(t, h, http.MethodGet, "/api/health"); rec.Code != http.StatusTeapot {
		t.Errorf("GET /api/health: %d, want the API handler", rec.Code)
	}
}

func TestEmbeddedFilesOpen(t *testing.T) {
	if _, err := fs.Stat(Files(), "."); err != nil {
		t.Fatalf("the embedded dist directory does not open: %v", err)
	}
}
