package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestDashboardNeedsNoTokenAndTheAPIStillDoes(t *testing.T) {
	files := fstest.MapFS{"index.html": {Data: []byte("<title>app</title>")}}
	srv := httptest.NewServer(dashboardHandler(files, requireToken(testAPIToken, false, okHandler())))
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		name, path, auth string
		want             int
	}{
		{"page without a token", "/", "", http.StatusOK},
		{"unknown page without a token", "/settings", "", http.StatusOK},
		{"api without a token", "/api/zones", "", http.StatusUnauthorized},
		{"api root without a token", "/api", "", http.StatusUnauthorized},
		{"api with a wrong token", "/api/zones", "Bearer wrong", http.StatusUnauthorized},
		{"api with the token", "/api/zones", "Bearer " + testAPIToken, http.StatusOK},
		{"health stays private", "/api/health", "", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := authGet(t, srv.URL, tc.path, tc.auth).StatusCode; got != tc.want {
				t.Fatalf("GET %s = %d, want %d", tc.path, got, tc.want)
			}
		})
	}
}

// The server answers an API path from the daemon routes, so a file under api/
// in the build can never replace one.
func TestDashboardNeverShadowsTheAPI(t *testing.T) {
	files := fstest.MapFS{
		"index.html":      {Data: []byte("<title>app</title>")},
		"api/health.json": {Data: []byte("shadow")},
	}
	srv := httptest.NewServer(dashboardHandler(files, requireToken(testAPIToken, true, okHandler())))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "shadow") {
		t.Fatalf("GET /api/health = %d %q, want the API answer", resp.StatusCode, body)
	}
}

func TestServerHandlerKeepsTheAPIRules(t *testing.T) {
	srv := httptest.NewServer(serverHandler(testAPIToken, false, false, okHandler()))
	t.Cleanup(srv.Close)
	if got := authGet(t, srv.URL, "/api/stats", "Bearer "+testAPIToken).StatusCode; got != http.StatusOK {
		t.Fatalf("GET /api/stats = %d, want 200", got)
	}
}
