package daemon

// ingress: Home Assistant ingress. The Supervisor authenticates the user in Home Assistant
// and proxies the request to the add-on from a fixed address. The developer documentation
// for apps tells an app to accept ingress connections from that address only. The daemon
// takes the same rule with two limits, because Home Assistant lets every user open an
// ingress session, and panel_admin hides only the sidebar entry:
//
//   - A request from the gateway address skips the token only for the method and path
//     pairs in ingress_routes.json, the calls that the dashboard makes. Every other request
//     from the gateway needs the token like a request from any other source. The routes
//     that pair devices, change the installation or the network key, reset or flash a
//     device, and read raw device answers are not in the list.
//   - The decision reads the address of the TCP connection and nothing else. A header such
//     as X-Forwarded-For or X-Ingress-Path is under the control of the caller.

import (
	_ "embed"
	"encoding/json"
	"log"
	"net/http"
	"net/netip"
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
	checked := requireToken(token, healthPublic, mux)
	if !ingress {
		return checked
	}
	log.Printf("API: the dashboard routes need no token from the Home Assistant ingress gateway %s", ingressGateway)
	return trustGateway(ingressGateway, ingressRoutes, mux, checked)
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
