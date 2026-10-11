package daemon

// ingress: Home Assistant ingress. The Supervisor authenticates the user in Home Assistant
// and proxies the request to the add-on from a fixed address. The developer documentation
// for apps tells an app to accept ingress connections from that address only. The daemon
// takes the same rule: with --ingress, a request from the gateway address needs no token.
// Every other source keeps the rules of requireToken.
//
// The decision reads the address of the TCP connection and nothing else. A header such as
// X-Forwarded-For or X-Ingress-Path is under the control of the caller.

import (
	"log"
	"net/http"
	"net/netip"
)

// ingressGateway is the address of the Supervisor on the network of the apps.
var ingressGateway = netip.MustParseAddr("172.30.32.2")

// accessHandler puts the API behind its access rules: the bearer token, and with ingress
// the gateway of Home Assistant as a second way in.
func accessHandler(token string, healthPublic, ingress bool, mux http.Handler) http.Handler {
	checked := requireToken(token, healthPublic, mux)
	if !ingress {
		return checked
	}
	log.Printf("API: requests from the Home Assistant ingress gateway %s need no token", ingressGateway)
	return trustGateway(ingressGateway, mux, checked)
}

// trustGateway serves a request that comes from the gateway address with trusted, and
// every other request with checked.
func trustGateway(gateway netip.Addr, trusted, checked http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fromAddr(r, gateway) {
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
