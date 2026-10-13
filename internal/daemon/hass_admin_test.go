package daemon

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fakeSupervisorToken = "synthetic-supervisor-token"

// The users of the fake Home Assistant. The ids are invented.
const (
	userOwner    = "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a01"
	userAdmin    = "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a02"
	userPlain    = "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a03"
	userInactive = "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a04"
	userSystem   = "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a05"
	userUnknown  = "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0aff"
)

var fakeUsers = []hassUser{
	{ID: userOwner, IsOwner: true, IsActive: true, GroupIDs: []string{"system-admin"}},
	{ID: userAdmin, IsActive: true, GroupIDs: []string{"system-admin"}},
	{ID: userPlain, IsActive: true, GroupIDs: []string{"system-users"}},
	{ID: userInactive, IsActive: false, GroupIDs: []string{"system-admin"}},
	{ID: userSystem, IsActive: true, SystemGenerated: true, GroupIDs: []string{"system-admin"}},
}

// fakeCore is the WebSocket proxy of the Supervisor in front of Home Assistant
// Core, as far as config/auth/list needs it.
type fakeCore struct {
	// mode changes the behaviour: "" answers, and the other values name a fault.
	mode  string
	calls atomic.Int32
}

// serverFrame writes one unmasked frame, as a server does.
func serverFrame(conn net.Conn, first byte, payload []byte) {
	frame := []byte{first}
	if n := len(payload); n < 126 {
		frame = append(frame, byte(n))
	} else {
		frame = binary.BigEndian.AppendUint16(append(frame, 126), uint16(n))
	}
	_, _ = conn.Write(append(frame, payload...))
}

func (f *fakeCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	if r.URL.Path != coreWebSocketPath || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || f.mode == "no websocket" {
		http.Error(w, "not here", http.StatusBadGateway)
		return
	}
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + wsGUID))
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
	_ = rw.Flush()
	ws := &wsConn{conn: conn, r: bufio.NewReader(rw)}
	send := func(v any) {
		raw, _ := json.Marshal(v)
		serverFrame(conn, 0x80|wsText, raw)
	}
	if f.mode == "silent" {
		time.Sleep(3 * time.Second)
		return
	}
	send(map[string]string{"type": "auth_required"})
	var auth struct {
		Type  string `json:"type"`
		Token string `json:"access_token"`
	}
	raw, err := ws.read()
	if err != nil || json.Unmarshal(raw, &auth) != nil {
		return
	}
	if auth.Type != "auth" || auth.Token != fakeSupervisorToken || f.mode == "auth refused" {
		send(map[string]string{"type": "auth_invalid", "message": "Invalid access"})
		return
	}
	send(map[string]string{"type": "auth_ok"})
	var cmd struct {
		ID   int    `json:"id"`
		Type string `json:"type"`
	}
	raw, err = ws.read()
	if err != nil || json.Unmarshal(raw, &cmd) != nil || cmd.Type != "config/auth/list" {
		return
	}
	switch f.mode {
	case "command refused":
		send(map[string]any{"id": cmd.ID, "type": "result", "success": false, "error": map[string]string{"code": "unauthorized"}})
	case "bad answer":
		send(map[string]any{"id": cmd.ID, "type": "result", "success": true, "result": "not a list"})
	case "closes":
		serverFrame(conn, 0x80|wsClose, nil)
	case "fragments":
		// A ping, a message of another id, then the answer in two fragments.
		serverFrame(conn, 0x80|wsPing, []byte("hi"))
		send(map[string]any{"id": cmd.ID + 7, "type": "event"})
		answer, _ := json.Marshal(map[string]any{"id": cmd.ID, "type": "result", "success": true, "result": fakeUsers})
		serverFrame(conn, wsText, answer[:len(answer)/2])
		serverFrame(conn, 0x80, answer[len(answer)/2:])
	default:
		send(map[string]any{"id": cmd.ID, "type": "result", "success": true, "result": fakeUsers})
	}
	// Let the client read before the connection closes.
	_, _ = ws.read()
}

func newFakeCore(t *testing.T, mode string) (*fakeCore, coreAdmins) {
	t.Helper()
	core := &fakeCore{mode: mode}
	srv := httptest.NewServer(core)
	t.Cleanup(srv.Close)
	return core, coreAdmins{addr: srv.Listener.Addr().String(), token: fakeSupervisorToken}
}

func TestCoreAdminsReadsTheUserList(t *testing.T) {
	for _, mode := range []string{"", "fragments"} {
		_, admins := newFakeCore(t, mode)
		for _, tc := range []struct {
			name, id string
			want     bool
		}{
			{"the owner", userOwner, true},
			{"an administrator", userAdmin, true},
			{"a user", userPlain, false},
			{"an inactive administrator", userInactive, false},
			{"a system user", userSystem, false},
			{"an unknown id", userUnknown, false},
			{"an empty id", "", false},
		} {
			got, err := admins.IsAdmin(context.Background(), tc.id)
			if err != nil || got != tc.want {
				t.Errorf("mode %q, %s: %v %v, want %v", mode, tc.name, got, err, tc.want)
			}
		}
	}
}

func TestCoreAdminsFailsClosed(t *testing.T) {
	for _, mode := range []string{"no websocket", "auth refused", "command refused", "bad answer", "closes"} {
		_, admins := newFakeCore(t, mode)
		if got, err := admins.IsAdmin(context.Background(), userAdmin); got || err == nil {
			t.Errorf("%s: %v %v, want false and an error", mode, got, err)
		}
	}
	t.Run("a wrong Supervisor token", func(t *testing.T) {
		_, admins := newFakeCore(t, "")
		admins.token = "another-token"
		if got, err := admins.IsAdmin(context.Background(), userAdmin); got || err == nil {
			t.Errorf("%v %v, want false and an error", got, err)
		}
	})
	t.Run("no Supervisor token", func(t *testing.T) {
		core, admins := newFakeCore(t, "")
		admins.token = ""
		if got, err := admins.IsAdmin(context.Background(), userAdmin); got || err == nil || core.calls.Load() != 0 {
			t.Errorf("%v %v after %d calls, want false, an error and no call", got, err, core.calls.Load())
		}
	})
	t.Run("nobody listens", func(t *testing.T) {
		admins := coreAdmins{addr: freeLoopbackAddr(t), token: fakeSupervisorToken}
		if got, err := admins.IsAdmin(context.Background(), userAdmin); got || err == nil {
			t.Errorf("%v %v, want false and an error", got, err)
		}
	})
	t.Run("no answer in time", func(t *testing.T) {
		_, admins := newFakeCore(t, "silent")
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		if got, err := admins.IsAdmin(ctx, userAdmin); got || err == nil || time.Since(start) > 2*time.Second {
			t.Errorf("%v %v after %s, want false and an error at the deadline", got, err, time.Since(start))
		}
	})
}

// The third access class: the export and the import of the state pass from the
// gateway for an administrator, and for nobody else without the token.
func TestIngressAdminRoutes(t *testing.T) {
	core, admins := newFakeCore(t, "")
	h := accessHandlerWith(testAPIToken, false, true, admins, okHandler())
	bearer := "Bearer " + testAPIToken
	const lan = "192.0.2.10:50000"
	for _, pair := range []string{"GET /api/state/export", "POST /api/state/import"} {
		method, path, _ := strings.Cut(pair, " ")
		for _, tc := range []struct {
			name, peer string
			header     []string
			want       int
		}{
			{"the owner through the gateway", gatewayPeer, []string{remoteUserHeader, userOwner}, http.StatusOK},
			{"an administrator through the gateway", gatewayPeer, []string{remoteUserHeader, userAdmin}, http.StatusOK},
			{"a user through the gateway", gatewayPeer, []string{remoteUserHeader, userPlain}, http.StatusForbidden},
			{"an inactive administrator", gatewayPeer, []string{remoteUserHeader, userInactive}, http.StatusForbidden},
			{"a system user", gatewayPeer, []string{remoteUserHeader, userSystem}, http.StatusForbidden},
			{"an unknown id", gatewayPeer, []string{remoteUserHeader, userUnknown}, http.StatusForbidden},
			{"no header", gatewayPeer, nil, http.StatusForbidden},
			{"an empty header", gatewayPeer, []string{remoteUserHeader, ""}, http.StatusForbidden},
			{"a user with a wrong token", gatewayPeer, []string{remoteUserHeader, userPlain, "Authorization", "Bearer wrong"}, http.StatusForbidden},
			{"the token through the gateway, no header", gatewayPeer, []string{"Authorization", bearer}, http.StatusOK},
			{"the token through the gateway, a user", gatewayPeer, []string{remoteUserHeader, userPlain, "Authorization", bearer}, http.StatusOK},
			{"the header from the LAN", lan, []string{remoteUserHeader, userAdmin}, http.StatusUnauthorized},
			{"the header from another app", "172.30.32.4:50000", []string{remoteUserHeader, userOwner}, http.StatusUnauthorized},
			{"the header from loopback", "127.0.0.1:50000", []string{remoteUserHeader, userOwner}, http.StatusUnauthorized},
			{"the token from the LAN", lan, []string{"Authorization", bearer}, http.StatusOK},
			{"a wrong token from the LAN with the header", lan, []string{remoteUserHeader, userAdmin, "Authorization", "Bearer wrong"}, http.StatusUnauthorized},
		} {
			if got := send(h, tc.peer, method, path+"?confirm=true", tc.header...); got != tc.want {
				t.Errorf("%s, %s: %d, want %d", pair, tc.name, got, tc.want)
			}
		}
	}

	// A request that is not from the gateway never reaches Home Assistant.
	before := core.calls.Load()
	send(h, lan, "GET", "/api/state/export", remoteUserHeader, userAdmin)
	send(h, gatewayPeer, "GET", "/api/state/export", "Authorization", bearer)
	if core.calls.Load() != before {
		t.Error("a request with the token or from another address asked Home Assistant")
	}
	// The administrator of the admin class gets nothing more from the header.
	if got := send(h, gatewayPeer, "PUT", "/api/install/network_key", remoteUserHeader, userOwner); got != http.StatusUnauthorized {
		t.Errorf("a token only route for the owner: %d, want 401", got)
	}
	// Each request asks again: an answer is not kept for the next user.
	before = core.calls.Load()
	send(h, gatewayPeer, "GET", "/api/state/export", remoteUserHeader, userAdmin)
	if got := send(h, gatewayPeer, "GET", "/api/state/export", remoteUserHeader, userPlain); got != http.StatusForbidden || core.calls.Load() != before+2 {
		t.Errorf("a user after an administrator: %d after %d calls", got, core.calls.Load()-before)
	}
}

func TestIngressAdminRoutesFailClosed(t *testing.T) {
	for _, mode := range []string{"no websocket", "auth refused", "command refused", "bad answer", "closes"} {
		_, admins := newFakeCore(t, mode)
		h := accessHandlerWith(testAPIToken, false, true, admins, okHandler())
		if got := send(h, gatewayPeer, "GET", "/api/state/export", remoteUserHeader, userOwner); got != http.StatusServiceUnavailable {
			t.Errorf("%s: %d, want 503", mode, got)
		}
		if got := send(h, gatewayPeer, "GET", "/api/state/export", "Authorization", "Bearer "+testAPIToken); got != http.StatusOK {
			t.Errorf("%s: the token gets %d, want 200", mode, got)
		}
	}
	// Outside the Supervisor the flag has no effect: the header and the address give nothing.
	t.Setenv(supervisorTokenEnv, "")
	_, admins := newFakeCore(t, "")
	h := accessHandlerWith(testAPIToken, false, ingressEnabled(true, func(string) (string, bool) { return "", false }), admins, okHandler())
	if got := send(h, gatewayPeer, "GET", "/api/state/export", remoteUserHeader, userOwner); got != http.StatusUnauthorized {
		t.Errorf("without the Supervisor: %d, want 401", got)
	}
}

// The class is exactly the export and the import. A new pair is a decision
// about access: it must change this list too.
func TestIngressAdminRoutesAreExactlyTheStateRoutes(t *testing.T) {
	var got []string
	for _, rt := range ingressAdminRoutes {
		got = append(got, rt.method+" /"+strings.Join(rt.segments, "/"))
	}
	want := []string{"GET /api/state/export", "POST /api/state/import"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("admin routes = %v, want %v", got, want)
	}
	mux := newInstallAPI(t).d.mux()
	for _, rt := range ingressAdminRoutes {
		path := "/" + strings.Join(rt.segments, "/")
		if _, pattern := mux.Handler(httptest.NewRequest(rt.method, path, nil)); pattern == "" {
			t.Errorf("%s %s is not a route of the daemon", rt.method, path)
		}
		// No pair is in both classes: the plain ingress list would let every user in.
		if matchesAny(ingressRoutes, httptest.NewRequest(rt.method, path, nil)) {
			t.Errorf("%s %s is also in ingress_routes.json", rt.method, path)
		}
	}
}
