package daemon

// ingress: Home Assistant ingress. The Supervisor authenticates the user in Home Assistant
// and proxies the request to the add-on from a fixed address. The developer documentation
// for apps tells an app to accept ingress connections from that address only. The daemon
// takes the same rule with two limits, because Home Assistant lets every user open an
// ingress session, and panel_admin hides only the sidebar entry:
//
//   - A request from the gateway address skips the token only for the method and path
//     pairs in ingress_routes.json, the calls that the dashboard makes. Every other request
//     from the gateway needs the token like a request from any other source. The list
//     holds the installation and pairing calls of the dashboard, because an add-on user
//     cannot read a generated token. The routes that write the identity of the network or
//     the network key, pair with a key of the caller, reset or flash a device, read raw
//     device answers, and export or import the state database are not in the list. A
//     listed write that shares a route with such a field has a route of its own that
//     carries one field.
//   - The decision reads the address of the TCP connection and nothing else. A header such
//     as X-Forwarded-For or X-Ingress-Path is under the control of the caller.
//
// A third class sits between the two: the pairs in ingress_admin_routes.json, the export
// and the import of the state. The export holds the network key and the import replaces the
// identity of the network, so a request from the gateway passes without the token only when
// Home Assistant says that the user is an administrator. The Supervisor names the user of
// the ingress session in X-Remote-User-Id and removes a copy that the browser sent. The
// header counts only on a connection from the gateway address.

import (
	_ "embed"
	"encoding/json"
	"log"
	"net/http"
	"net/netip"
	"os"
	"path"
	"strings"
)

// ingressGateway is the address of the Supervisor on the network of the apps.
var ingressGateway = netip.MustParseAddr("172.30.32.2")

// supervisorTokenEnv is set by the Supervisor in every app container. A process outside a
// Supervisor has no gateway to trust: 172.30.32.2 is the address of some other host there.
const supervisorTokenEnv = "SUPERVISOR_TOKEN"

// ingressRoutesJSON lists the method and path pairs of the dashboard. dashboard/tests checks
// that every call of dashboard/src/lib/api.ts is in it. A segment in braces matches one segment.
//
//go:embed ingress_routes.json
var ingressRoutesJSON []byte

var ingressRoutes = mustRoutes(ingressRoutesJSON)

// ingressAdminRoutesJSON lists the pairs that the gateway may call without a token for a
// Home Assistant administrator only.
//
//go:embed ingress_admin_routes.json
var ingressAdminRoutesJSON []byte

var ingressAdminRoutes = mustRoutes(ingressAdminRoutesJSON)

// remoteUserHeader carries the id of the Home Assistant user of an ingress session.
const remoteUserHeader = "X-Remote-User-Id"

// ingressEnabled reports whether the trust of the gateway applies. The flag alone is not
// enough: the process must run under the Supervisor. The log line tells the operator why a
// requested flag has no effect.
func ingressEnabled(requested bool, lookupEnv func(string) (string, bool)) bool {
	if !requested {
		return false
	}
	if v, ok := lookupEnv(supervisorTokenEnv); !ok || v == "" {
		log.Printf("WARNING: --ingress is ignored: %s is not set, so this process does not run under the Home Assistant Supervisor", supervisorTokenEnv)
		return false
	}
	return true
}

// accessHandler puts the API behind its access rules: the bearer token, and with ingress
// the gateway of Home Assistant as a second way in for the routes of the dashboard.
func accessHandler(token string, healthPublic, ingress bool, mux http.Handler) http.Handler {
	admins := coreAdmins{addr: supervisorAddr, token: os.Getenv(supervisorTokenEnv)}
	return accessHandlerWith(token, healthPublic, ingress, admins, mux)
}

func accessHandlerWith(token string, healthPublic, ingress bool, admins adminChecker, mux http.Handler) http.Handler {
	checked := requireToken(token, healthPublic, mux)
	// With no token every request passes already.
	if !ingress || token == "" {
		return checked
	}
	log.Printf("API: the dashboard routes need no token from the Home Assistant ingress gateway %s", ingressGateway)
	adminOnly := requireAdmin(token, admins, mux)
	return trustGateway(ingressGateway, ingressRoutes, mux,
		trustGateway(ingressGateway, ingressAdminRoutes, adminOnly, checked))
}

// trustGateway serves a request with trusted when it comes from the gateway address and
// matches one of the routes. Every other request goes to checked.
func trustGateway(gateway netip.Addr, routes []route, trusted, checked http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fromAddr(r, gateway) && matchesAny(routes, r) {
			trusted.ServeHTTP(w, r)
			return
		}
		checked.ServeHTTP(w, r)
	})
}

// requireAdmin serves a request of the gateway that carries the API token, or whose Home
// Assistant user is an active administrator. Everything else is refused: no user id, a user
// that Home Assistant does not know, and a question that got no answer.
func requireAdmin(token string, admins adminChecker, next http.Handler) http.Handler {
	accepts := tokenMatcher(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, ok := bearerToken(r); ok && accepts(got) {
			next.ServeHTTP(w, r)
			return
		}
		const who = "only a Home Assistant administrator can export or import the state"
		id := r.Header.Get(remoteUserHeader)
		if id == "" {
			writeErr(w, http.StatusForbidden, who+": the request names no Home Assistant user")
			return
		}
		admin, err := admins.IsAdmin(r.Context(), id)
		if err != nil {
			log.Printf("ingress: cannot ask Home Assistant about the user of %s %s: %v", r.Method, r.URL.Path, err)
			writeErr(w, http.StatusServiceUnavailable,
				"Home Assistant did not say whether the user is an administrator: "+err.Error())
			return
		}
		if !admin {
			writeErr(w, http.StatusForbidden, who)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// fromAddr reports whether the peer of the connection is want. A peer that does not
// parse is not trusted.
func fromAddr(r *http.Request, want netip.Addr) bool {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	return err == nil && peer.Addr().Unmap() == want
}

// route is one method and path pair. A segment in braces matches one non-empty segment.
type route struct {
	method   string
	segments []string
}

func mustRoutes(data []byte) []route {
	var pairs []string
	if err := json.Unmarshal(data, &pairs); err != nil {
		panic("ingress_routes.json: " + err.Error())
	}
	routes := make([]route, 0, len(pairs))
	for _, pair := range pairs {
		method, p, ok := strings.Cut(pair, " ")
		if !ok || method == "" || !strings.HasPrefix(p, "/api/") || p != path.Clean(p) {
			panic("ingress_routes.json: bad pair " + pair)
		}
		routes = append(routes, route{method: method, segments: strings.Split(p[1:], "/")})
	}
	return routes
}

func matchesAny(routes []route, r *http.Request) bool {
	for _, rt := range routes {
		if rt.matches(r) {
			return true
		}
	}
	return false
}

// matches compares the method and the decoded path exactly. A path that is not in its clean
// form (a dot segment, a double slash, a trailing slash) never matches, so a request cannot
// be written to look like a listed route and reach another one.
func (rt route) matches(r *http.Request) bool {
	p := r.URL.Path
	if r.Method != rt.method || p != path.Clean(p) || !strings.HasPrefix(p, "/") {
		return false
	}
	parts := strings.Split(p[1:], "/")
	if len(parts) != len(rt.segments) {
		return false
	}
	for i, segment := range rt.segments {
		if strings.HasPrefix(segment, "{") {
			if parts[i] == "" {
				return false
			}
			continue
		}
		if parts[i] != segment {
			return false
		}
	}
	return true
}
