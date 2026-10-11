package daemon

// dashboard: the handler of the HTTP server. The embedded dashboard needs no
// token, because it holds no data. Every /api path keeps its access rules.

import (
	"io/fs"
	"net/http"

	"freedo/internal/web"
)

// serverHandler is the handler of the daemon's HTTP server.
func serverHandler(token string, healthPublic, ingress bool, mux http.Handler) http.Handler {
	return dashboardHandler(web.Files(), accessHandler(token, healthPublic, ingress, mux))
}

func dashboardHandler(files fs.FS, api http.Handler) http.Handler {
	return web.Handler(files, api)
}
