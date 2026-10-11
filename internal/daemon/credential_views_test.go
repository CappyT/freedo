package daemon

// credential_views_test: the credentials a device sends reach no non-debug
// route, while the debug routes and the owner routes keep their contents.
// Frames and values are synthetic.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freedo/tado"
)

// getBody reads the body of a GET and fails the test on any status but 200.
func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := boundedClient.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", url, resp.StatusCode, body)
	}
	return string(body)
}

func TestNonDebugRoutesServeNoCredentials(t *testing.T) {
	const device = "VA1000000005"
	const sentSerial = "VA7777777777"
	var (
		token     = bytes.Repeat([]byte{0xa7}, 16)
		rotate    = bytes.Repeat([]byte{0xc3}, 16)
		response  = bytes.Repeat([]byte{0xe9}, 16)
		session   = []byte{0x5e, 0x55, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0a}
		networkKy = hex.EncodeToString(defaultInstall.NetworkKey())
	)
	tlv := func(tag string, val []byte) []byte { return tado.TLV{Tag: tag, Val: val}.Encode() }
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	content := []tado.CoAPOption{{Num: 12, Val: []byte{42}}}

	dev, err := defaultInstall.Resolve(device)
	if err != nil {
		t.Fatal(err)
	}
	short := dev.Short()

	d, url := newHomeDaemon(t, newTestClock(time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)))
	authFrame := uplinkFrame(t, defaultInstall, device, false, tado.CoAPReq{
		Type: "CON", Code: "0.02", MID: 0x7101, Token: []byte{0x01},
		Path: []string{"auth", "token"}, Options: content,
		Payload: cat(tlv("0260", []byte(sentSerial)), tlv("0007", token)),
	})
	if _, err := d.do(func(*tado.EmuState) (any, error) { d.handleFrame(authFrame, true); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	feed(t, d, defaultInstall, device, false, tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7102, Token: []byte{0x02},
		Path: []string{"d", device, "sen"}, Options: content,
		Payload: cat(tlv("012d", []byte{0x07, 0xd0}), tlv("0135", []byte{0x01, 0x2c}),
			tlv("003a", []byte{0x01, 0x02}), tlv("01fa", []byte{0x02}), tlv("01fb", []byte{0x00})),
	})
	feed(t, d, defaultInstall, device, false, tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7103, Token: []byte{0x03},
		Path: []string{"d", device, "sen"}, Options: content,
		Payload: cat(tlv("025e", session), tlv("0261", rotate), tlv("0262", response)),
	})
	// The engine keeps what it needs: the secret values stay in its own state.
	type engineView struct {
		telemetry map[string]string
		session   []byte
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		for _, n := range e.Nodes() {
			if n.Short == short {
				return engineView{n.Telemetry, n.SessionID}, nil
			}
		}
		return nil, io.EOF
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := v.(engineView)
	for key, want := range map[string]string{
		"auth_token": hex.EncodeToString(token), "serial_id": sentSerial,
		"challenge_key_rotate": hex.EncodeToString(rotate), "0262": hex.EncodeToString(response),
	} {
		if engine.telemetry[key] != want {
			t.Errorf("the engine holds Telemetry %s = %q, want %q", key, engine.telemetry[key], want)
		}
	}
	if len(engine.session) == 0 {
		t.Fatal("the engine served no session id")
	}

	secrets := map[string]string{
		"auth token": hex.EncodeToString(token), "serial": sentSerial,
		"challenge key": hex.EncodeToString(rotate), "challenge response": hex.EncodeToString(response),
		"sent session id": hex.EncodeToString(session), "served session id": hex.EncodeToString(engine.session),
		"network key": networkKy,
	}
	for _, path := range []string{
		"/api/health", "/api/nodes", "/api/nodes/" + device, "/api/boiler", "/api/circuits",
		"/api/stats", "/api/home", "/api/zones", "/api/unhandled", "/api/queue/" + device,
		"/api/pair/window", "/api/early-start", "/api/install", "/api/install/devices",
		"/api/install/devices/" + device, "/api/install/zones", "/api/install/circuits",
	} {
		body := getBody(t, url+path)
		for what, value := range secrets {
			if strings.Contains(body, value) {
				t.Errorf("GET %s serves the %s:\n%s", path, what, body)
			}
		}
	}

	// The keys Home Assistant reads keep their values.
	var nodes []struct {
		Short     string            `json:"Short"`
		Serial    string            `json:"Serial"`
		Telemetry map[string]string `json:"Telemetry"`
	}
	if code := getJSON(t, url+"/api/nodes", &nodes); code != http.StatusOK {
		t.Fatalf("GET /api/nodes: status %d", code)
	}
	var found bool
	for _, n := range nodes {
		if n.Short != short {
			continue
		}
		found = true
		if n.Serial != device {
			t.Errorf("node Serial = %q, want the declared %q", n.Serial, device)
		}
		for _, key := range []string{"temp_C", "humidity_pct", "fw_run_version", "calib_state", "calib_error"} {
			if n.Telemetry[key] == "" || n.Telemetry[key] != engine.telemetry[key] {
				t.Errorf("Telemetry %s = %q, want the engine's %q", key, n.Telemetry[key], engine.telemetry[key])
			}
		}
	}
	if !found {
		t.Fatalf("GET /api/nodes lists no %s", device)
	}

	// The owner routes show the installation: the declared serial and EUI, and a
	// network key that is present but never its value.
	if body := getBody(t, url+"/api/install/devices/"+device); !strings.Contains(body, `"serial":"`+device+`"`) ||
		!strings.Contains(body, wireOf(t, defaultInstall, device)) {
		t.Errorf("GET /api/install/devices/%s lacks the declared serial or EUI: %s", device, body)
	}
	if body := getBody(t, url+"/api/install"); !strings.Contains(body, `"network_key":{"present":true}`) {
		t.Errorf("GET /api/install lacks the network key presence flag: %s", body)
	}

	// The debug routes keep the plaintext.
	d.debugAPI = true
	debug := httptest.NewServer(d.mux())
	t.Cleanup(debug.Close)
	for _, path := range []string{"/api/debug/dissect", "/api/debug/decrypt"} {
		var got map[string]any
		if code := postJSON(t, http.MethodPost, debug.URL+path, map[string]any{"hex": hex.EncodeToString(authFrame)}, &got); code != http.StatusOK {
			t.Fatalf("POST %s: status %d", path, code)
		}
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		out := string(raw)
		for what, value := range map[string]string{"auth token": hex.EncodeToString(token), "serial": sentSerial} {
			if !strings.Contains(out, value) {
				t.Errorf("POST %s lost the %s: %s", path, what, out)
			}
		}
	}
}
