package daemon

// dashboard: the handler of the HTTP server. The embedded dashboard needs no
// token, because it holds no data. Every /api path keeps its access rules.

import (
	"io/fs"
	"net/http"
	"os"

	"freedo/internal/web"
)

// serverHandler is the handler of the daemon's HTTP server. ingress is the flag --ingress,
// which only takes effect under the Home Assistant Supervisor.
func serverHandler(token string, healthPublic, ingress bool, mux http.Handler) http.Handler {
	return dashboardHandler(web.Files(), accessHandler(token, healthPublic, ingressEnabled(ingress, os.LookupEnv), mux))
}

func dashboardHandler(files fs.FS, api http.Handler) http.Handler {
	return web.Handler(files, api)
}
