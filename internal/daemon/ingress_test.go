package daemon

import (
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
)

// send runs one request through h as if the connection came from peer.
func send(h http.Handler, peer, method, target string, header ...string) int {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = peer
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func request(h http.Handler, peer, path string, header ...string) int {
	return send(h, peer, http.MethodGet, path, header...)
}

const gatewayPeer = "172.30.32.2:50000"

func TestFromAddr(t *testing.T) {
	gateway := netip.MustParseAddr("172.30.32.2")
	for _, tc := range []struct {
		peer string
		want bool
	}{
		{"172.30.32.2:48000", true},
		{"[::ffff:172.30.32.2]:48000", true},
		{"172.30.32.3:48000", false},
		{"172.30.32.1:48000", false},
		{"127.0.0.1:48000", false},
		{"[2001:db8::2]:48000", false},
		{"172.30.32.2", false},
		{"", false},
		{"not an address", false},
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = tc.peer
		if got := fromAddr(req, gateway); got != tc.want {
			t.Errorf("peer %q: %v, want %v", tc.peer, got, tc.want)
		}
	}
}

// sample fills a listed route with values, the way the dashboard calls it.
func sample(pair string) (method, path string) {
	method, path, _ = strings.Cut(pair, " ")
	path = strings.ReplaceAll(path, "{n}", "3")
	path = strings.ReplaceAll(path, "{id}", "0201")
	path = strings.ReplaceAll(path, "{ref}", "0201")
	return method, path
}

func TestIngressTrustsTheGatewayAddressOnly(t *testing.T) {
	h := accessHandler(testAPIToken, false, true, okHandler())
	bearer := "Bearer " + testAPIToken
	for _, tc := range []struct {
		name, peer, auth string
		want             int
	}{
		{"the gateway needs no token", gatewayPeer, "", http.StatusOK},
		{"the gateway with a wrong token is still trusted", gatewayPeer, "Bearer wrong", http.StatusOK},
		{"the gateway as an IPv4-mapped address", "[::ffff:172.30.32.2]:50000", "", http.StatusOK},
		{"another app has no token", "172.30.32.4:50000", "", http.StatusUnauthorized},
		{"the host has no token", "172.30.32.1:50000", "", http.StatusUnauthorized},
		{"the LAN has no token", "192.0.2.10:50000", "", http.StatusUnauthorized},
		{"loopback has no token", "127.0.0.1:50000", "", http.StatusUnauthorized},
		{"another source with a wrong token", "192.0.2.10:50000", "Bearer wrong", http.StatusUnauthorized},
		{"another source with the token", "192.0.2.10:50000", bearer, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := request(h, tc.peer, "/api/zones", "Authorization", tc.auth); got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

// The calls of the dashboard pass without a token from the gateway, and from no other source.
func TestIngressPassesEveryListedRoute(t *testing.T) {
	h := accessHandler(testAPIToken, false, true, okHandler())
	if len(ingressRoutes) == 0 {
		t.Fatal("the list of routes is empty")
	}
	var pairs []string
	for _, rt := range ingressRoutes {
		pairs = append(pairs, rt.method+" /"+strings.Join(rt.segments, "/"))
	}
	for _, pair := range pairs {
		method, path := sample(pair)
		if got := send(h, gatewayPeer, method, path); got != http.StatusOK {
			t.Errorf("%s %s from the gateway: %d, want 200", method, path, got)
		}
		if got := send(h, "192.0.2.10:50000", method, path); got != http.StatusUnauthorized {
			t.Errorf("%s %s from another source: %d, want 401", method, path, got)
		}
		// A query string does not change the route.
		if got := send(h, gatewayPeer, method, path+"?x=1"); got != http.StatusOK {
			t.Errorf("%s %s?x=1 from the gateway: %d, want 200", method, path, got)
		}
	}
}

// Everything else from the gateway needs the token like any other request. These are the
// routes that pair, reset, flash or reconfigure, plus the reads of raw device data.
func TestIngressRefusesEveryOtherRoute(t *testing.T) {
	h := accessHandler(testAPIToken, false, true, okHandler())
	bearer := "Bearer " + testAPIToken
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/pair/window"},
		{"GET", "/api/pair/window"},
		{"POST", "/api/pair"},
		{"GET", "/api/install"},
		{"PATCH", "/api/install"},
		{"PUT", "/api/install/network_key"},
		{"POST", "/api/install/devices"},
		{"PATCH", "/api/install/devices/x"},
		{"DELETE", "/api/install/devices/x"},
		{"PATCH", "/api/install/zones/1"},
		{"DELETE", "/api/install/circuits/1"},
		{"POST", "/api/nodes/x/factoryreset"},
		{"POST", "/api/nodes/x/fw"},
		{"GET", "/api/nodes/x/fw"},
		{"DELETE", "/api/nodes/x/fw"},
		{"POST", "/api/nodes/x/reboot"},
		{"POST", "/api/nodes/x/calibrate"},
		{"POST", "/api/nodes/x/setpoint"},
		{"POST", "/api/nodes/x/config"},
		{"POST", "/api/nodes/x/netcmd"},
		{"PUT", "/api/nodes/x/display"},
		{"PUT", "/api/nodes/x/schedule"},
		{"GET", "/api/nodes/x"},
		{"GET", "/api/nodes/x/resource"},
		{"GET", "/api/nodes/x/currentlog"},
		{"GET", "/api/nodes/x/dbg/mem"},
		{"GET", "/api/debug/rx"},
		{"POST", "/api/debug/decrypt"},
		{"GET", "/api/queue/x"},
		{"DELETE", "/api/queue/x"},
		{"POST", "/api/outdoor"},
		{"POST", "/api/boiler/dhw"},
		{"POST", "/api/zones/3/offset"},
		{"POST", "/api/zones/3/temperature"},
		{"PUT", "/api/zones/3/early-start"},
		{"GET", "/api/stats"},
		{"GET", "/api/unhandled"},
		{"GET", "/api/heatsource/capabilities"},
	} {
		if got := send(h, gatewayPeer, tc.method, tc.path); got != http.StatusUnauthorized {
			t.Errorf("%s %s from the gateway: %d, want 401", tc.method, tc.path, got)
		}
		if got := send(h, gatewayPeer, tc.method, tc.path, "Authorization", bearer); got != http.StatusOK {
			t.Errorf("%s %s from the gateway with the token: %d, want 200", tc.method, tc.path, got)
		}
	}
}

// A request cannot be written to look like a listed route.
func TestIngressMatchesExactly(t *testing.T) {
	h := accessHandler(testAPIToken, false, true, okHandler())
	for _, tc := range []struct{ method, path string }{
		{"HEAD", "/api/zones"},
		{"OPTIONS", "/api/zones"},
		{"DELETE", "/api/home"},
		{"PUT", "/api/home"},
		{"GET", "/api/zones/"},
		{"GET", "/api/home/"},
		{"GET", "//api/zones"},
		{"GET", "/api//zones"},
		{"GET", "/api/zones/3/../install"},
		{"GET", "/api/./zones"},
		{"GET", "/api/zones/3/schedule/x"},
		{"POST", "/api/zones//setpoint"},
		{"POST", "/api/zones/3/setpoint/"},
		{"POST", "/api/zones/3/setpoint/../../../install/devices"},
		{"GET", "/API/zones"},
		{"GET", "/api/health/x"},
		{"POST", "/api/nodes/0201/childlock/x"},
		{"POST", "/api/nodes//childlock"},
		{"POST", "/api/nodes/a/b/childlock"},
	} {
		if got := send(h, gatewayPeer, tc.method, tc.path); got == http.StatusOK {
			t.Errorf("%s %s from the gateway passed without a token", tc.method, tc.path)
		}
	}
}

// Every listed pair is a route of the daemon, so a typo in the list cannot hide a route that
// the dashboard needs.
func TestIngressRoutesAreDaemonRoutes(t *testing.T) {
	mux := newInstallAPI(t).d.mux()
	for _, rt := range ingressRoutes {
		method, path := sample(rt.method + " /" + strings.Join(rt.segments, "/"))
		req := httptest.NewRequest(method, path, nil)
		if _, pattern := mux.Handler(req); pattern == "" {
			t.Errorf("%s %s is not a route of the daemon", method, path)
		}
	}
}

// Outside the Supervisor the address 172.30.32.2 belongs to some other host, so the flag alone
// grants nothing.
func TestIngressNeedsTheSupervisor(t *testing.T) {
	env := func(set map[string]string) func(string) (string, bool) {
		return func(key string) (string, bool) { v, ok := set[key]; return v, ok }
	}
	for _, tc := range []struct {
		name      string
		requested bool
		env       map[string]string
		want      bool
		logged    bool
	}{
		{"not requested", false, map[string]string{supervisorTokenEnv: "t"}, false, false},
		{"not requested outside the Supervisor", false, nil, false, false},
		{"requested under the Supervisor", true, map[string]string{supervisorTokenEnv: "t"}, true, false},
		{"requested with no variable", true, nil, false, true},
		{"requested with an empty variable", true, map[string]string{supervisorTokenEnv: ""}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			log.SetOutput(&out)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })
			if got := ingressEnabled(tc.requested, env(tc.env)); got != tc.want {
				t.Fatalf("ingressEnabled = %v, want %v", got, tc.want)
			}
			line := out.String()
			if tc.logged != strings.Contains(line, "--ingress is ignored") {
				t.Fatalf("log line = %q, want a line about the ignored flag: %v", line, tc.logged)
			}
			if tc.logged && !strings.Contains(line, supervisorTokenEnv) {
				t.Errorf("the log line does not name %s: %q", supervisorTokenEnv, line)
			}
		})
	}
}

// The server reads the environment: the flag without the Supervisor leaves the gateway as a
// source like any other.
func TestServerIgnoresIngressOutsideTheSupervisor(t *testing.T) {
	t.Setenv(supervisorTokenEnv, "")
	h := serverHandler(testAPIToken, false, true, okHandler())
	if got := request(h, gatewayPeer, "/api/zones"); got != http.StatusUnauthorized {
		t.Fatalf("gateway outside the Supervisor: status %d, want 401", got)
	}
	if got := request(h, gatewayPeer, "/api/zones", "Authorization", "Bearer "+testAPIToken); got != http.StatusOK {
		t.Fatalf("gateway with the token: status %d, want 200", got)
	}
}

// The caller controls every header. Only the address of the connection counts.
func TestIngressIgnoresForgedHeaders(t *testing.T) {
	h := accessHandler(testAPIToken, false, true, okHandler())
	for _, header := range [][]string{
		{"X-Forwarded-For", "172.30.32.2"},
		{"X-Real-IP", "172.30.32.2"},
		{"Forwarded", "for=172.30.32.2"},
		{"X-Ingress-Path", "/api/hassio_ingress/token"},
		{"X-Hass-Source", "core.ingress"},
	} {
		if got := request(h, "192.0.2.10:50000", "/api/zones", header...); got != http.StatusUnauthorized {
			t.Errorf("header %v from another source: status %d, want 401", header, got)
		}
	}
}

// Without --ingress the gateway is a source like any other.
func TestWithoutIngressTheGatewayNeedsTheToken(t *testing.T) {
	h := accessHandler(testAPIToken, false, false, okHandler())
	if got := request(h, gatewayPeer, "/api/zones"); got != http.StatusUnauthorized {
		t.Fatalf("gateway without --ingress: status %d, want 401", got)
	}
	if got := request(h, gatewayPeer, "/api/zones", "Authorization", "Bearer "+testAPIToken); got != http.StatusOK {
		t.Fatalf("gateway with the token: status %d, want 200", got)
	}
}

func TestIngressKeepsTheOtherAccessRules(t *testing.T) {
	// --health-public still opens the health route to every source.
	h := accessHandler(testAPIToken, true, true, okHandler())
	if got := request(h, "192.0.2.10:50000", "/api/health"); got != http.StatusOK {
		t.Errorf("public health: status %d, want 200", got)
	}
	if got := request(h, "192.0.2.10:50000", "/api/stats"); got != http.StatusUnauthorized {
		t.Errorf("stats from another source: status %d, want 401", got)
	}
}

// The dashboard and the API of one server: a page needs no token from anyone, and the
// gateway reaches the routes of the dashboard through the same handler.
func TestServerWithIngress(t *testing.T) {
	t.Setenv(supervisorTokenEnv, "supervisor-token")
	h := serverHandler(testAPIToken, false, true, okHandler())
	if got := request(h, "192.0.2.10:50000", "/"); got == http.StatusUnauthorized {
		t.Errorf("a page asked for a token")
	}
	if got := request(h, gatewayPeer, "/api/zones"); got != http.StatusOK {
		t.Errorf("gateway API: status %d, want 200", got)
	}
	if got := request(h, "192.0.2.10:50000", "/api/zones"); got != http.StatusUnauthorized {
		t.Errorf("other source API: status %d, want 401", got)
	}
	if got := send(h, gatewayPeer, "POST", "/api/pair/window"); got != http.StatusUnauthorized {
		t.Errorf("a route outside the list from the gateway: status %d, want 401", got)
	}
}
