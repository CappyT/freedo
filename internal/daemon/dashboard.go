package daemon

// dashboard: the handler of the HTTP server. The embedded dashboard needs no
// token, because it holds no data. Every /api path keeps the rules of the token.

import (
	"io/fs"
	"net/http"

	"freedo/internal/web"
)

// serverHandler is the handler of the daemon's HTTP server.
func serverHandler(token string, healthPublic bool, mux http.Handler) http.Handler {
	return dashboardHandler(web.Files(), token, healthPublic, mux)
}

func dashboardHandler(files fs.FS, token string, healthPublic bool, mux http.Handler) http.Handler {
	return web.Handler(files, requireToken(token, healthPublic, mux))
}
