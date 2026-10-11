package daemon

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// request runs one request through h as if the connection came from peer.
func request(h http.Handler, peer, path string, header ...string) int {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = peer
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

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

func TestIngressTrustsTheGatewayAddressOnly(t *testing.T) {
	h := accessHandler(testAPIToken, false, true, okHandler())
	bearer := "Bearer " + testAPIToken
	for _, tc := range []struct {
		name, peer, auth string
		want             int
	}{
		{"the gateway needs no token", "172.30.32.2:50000", "", http.StatusOK},
		{"the gateway with a wrong token is still trusted", "172.30.32.2:50000", "Bearer wrong", http.StatusOK},
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
	if got := request(h, "172.30.32.2:50000", "/api/zones"); got != http.StatusUnauthorized {
		t.Fatalf("gateway without --ingress: status %d, want 401", got)
	}
	if got := request(h, "172.30.32.2:50000", "/api/zones", "Authorization", "Bearer "+testAPIToken); got != http.StatusOK {
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
// gateway reaches the API through the same handler.
func TestServerWithIngress(t *testing.T) {
	h := serverHandler(testAPIToken, false, true, okHandler())
	if got := request(h, "192.0.2.10:50000", "/"); got == http.StatusUnauthorized {
		t.Errorf("a page asked for a token")
	}
	if got := request(h, "172.30.32.2:50000", "/api/zones"); got != http.StatusOK {
		t.Errorf("gateway API: status %d, want 200", got)
	}
	if got := request(h, "192.0.2.10:50000", "/api/zones"); got != http.StatusUnauthorized {
		t.Errorf("other source API: status %d, want 401", got)
	}
}
