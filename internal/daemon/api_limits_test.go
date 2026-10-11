package daemon

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// postRaw sends body verbatim and returns the status and the error message.
func postRaw(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	msg, _ := out["error"].(string)
	return resp.StatusCode, msg
}

// TestHeatingSetpointsKeepTheRoomRange: a heating setpoint outside 5..25 C
// wraps on the wire (700 C reads 44.64 C in u16), and a node setpoint with no
// tempC would queue 0 C. Every heating write path refuses both and queues
// nothing.
func TestHeatingSetpointsKeepTheRoomRange(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	week := func(c float64, enabled bool) map[string]any {
		return map[string]any{"programs": []map[string]any{{"days": 0x7f, "entries": []map[string]any{
			{"minute": 0, "setpoint_c": 18.0},
			{"minute": 360, "setpoint_c": c, "enabled": enabled},
		}}}}
	}
	nodeWeek := func(c float64) map[string]any {
		return map[string]any{"dayMask": 127, "id": 3, "entries": []map[string]any{{"startMin": 0, "tempC": c}}}
	}
	for _, tc := range []struct {
		what, method, path string
		body               any
		msg                string
	}{
		{"zone schedule above range", http.MethodPut, "/api/zones/3/schedule", week(700, true), "within 5..25"},
		{"zone schedule below range", http.MethodPut, "/api/zones/3/schedule", week(4.5, true), "within 5..25"},
		{"zone schedule off entry below range", http.MethodPut, "/api/zones/3/schedule", week(0, false), "within 5..25"},
		{"node schedule above range", http.MethodPut, "/api/nodes/VA1000000002/schedule", nodeWeek(700), "within 5..25"},
		{"node schedule below range", http.MethodPut, "/api/nodes/VA1000000002/schedule", nodeWeek(-3), "within 5..25"},
		{"node setpoint above range", http.MethodPost, "/api/nodes/VA1000000002/setpoint",
			map[string]any{"tempC": 700, "mode": "manual", "id": 3}, "within 5..25"},
		{"node setpoint without tempC", http.MethodPost, "/api/nodes/VA1000000002/setpoint",
			map[string]any{"mode": "manual", "id": 3}, "tempC is required"},
		{"node sched above range", http.MethodPost, "/api/nodes/VA1000000002/setpoint",
			map[string]any{"tempC": 20, "sched": 700, "mode": "manual", "id": 3}, "within 5..25"},
	} {
		var body map[string]any
		code := postJSON(t, tc.method, url+tc.path, tc.body, &body)
		msg, _ := body["error"].(string)
		if code != http.StatusBadRequest || !strings.Contains(msg, tc.msg) {
			t.Errorf("%s: code=%d error=%q, want 400 with %q", tc.what, code, msg, tc.msg)
		}
	}
	wantNoWrites(t, "a refused heating setpoint", d, wireOf(t, in, "VA1000000002"))
	var stored map[string]any
	if code := getJSON(t, url+"/api/zones/3/schedule", &stored); code != http.StatusNotFound {
		t.Errorf("zone 3 schedule after refused writes: code=%d %v, want 404", code, stored)
	}
	if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule", week(5, false), &stored); code != http.StatusOK {
		t.Errorf("an off entry at 5 C: code=%d %v, want 200", code, stored)
	}
}

// TestOverlayDurationsAreCapped: a durationSec past the cap overflows
// time.Duration into a negative or tiny timer, which leaves the overlay on
// forever. Every endpoint that takes one refuses it and queues nothing.
func TestOverlayDurationsAreCapped(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	maxSec := int(zoneMaxDuration / time.Second)
	// 18446744074 s is 2^64 ns plus 0.29 s: the product wraps to a tiny timer.
	for _, sec := range []int{maxSec + 1, 18446744074, 1 << 40} {
		for _, tc := range []struct {
			what, path string
			body       map[string]any
		}{
			{"node setpoint", "/api/nodes/VA1000000002/setpoint", map[string]any{"tempC": 20, "mode": "manual", "id": 3}},
			{"boiler dhw", "/api/boiler/dhw", map[string]any{"tempC": 50}},
			{"zone timer", "/api/zones/3/setpoint", map[string]any{"setpointC": 20, "mode": "timer"}},
			{"zone off timer", "/api/zones/3/off", map[string]any{"mode": "timer"}},
		} {
			tc.body["durationSec"] = sec
			var body map[string]any
			code := postJSON(t, http.MethodPost, url+tc.path, tc.body, &body)
			msg, _ := body["error"].(string)
			if code != http.StatusBadRequest || !strings.Contains(msg, "exceeds") {
				t.Errorf("%s durationSec=%d: code=%d error=%q, want 400 with \"exceeds\"", tc.what, sec, code, msg)
			}
		}
	}
	wantNoWrites(t, "a refused duration", d, wireOf(t, in, "VA1000000002"), wireOf(t, in, "RU1000000001"))
	var body map[string]any
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/VA1000000002/setpoint",
		map[string]any{"tempC": 20, "mode": "manual", "id": 3, "durationSec": maxSec}, &body); code != http.StatusOK {
		t.Errorf("node setpoint at the cap: code=%d %v, want 200", code, body)
	}
}

// TestJSONBodiesAreStrict: a body with a field the endpoint does not know, a
// second JSON value after the first or more bytes than the cap is refused, and
// queues nothing. The requests of the Home Assistant integration still pass.
func TestJSONBodiesAreStrict(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	for _, tc := range []struct {
		what, body string
		code       int
		msg        string
	}{
		{"unknown field", `{"setpointC": 20, "mode": "manual", "setpoint": 30}`, 400, "unknown field"},
		{"trailing value", `{"setpointC": 20, "mode": "manual"}{"setpointC": 25}`, 400, "one JSON value"},
		{"trailing garbage", `{"setpointC": 20, "mode": "manual"} x`, 400, ""},
		{"too long", `{"setpointC": 20, "mode": "manual", "pad": "` + strings.Repeat("x", apiMaxBody) + `"}`, 413, "longer than"},
	} {
		code, msg := postRaw(t, http.MethodPost, url+"/api/zones/3/setpoint", tc.body)
		if code != tc.code || !strings.Contains(msg, tc.msg) {
			t.Errorf("%s: code=%d error=%q, want %d with %q", tc.what, code, msg, tc.code, tc.msg)
		}
	}
	wantNoWrites(t, "a refused body", d, wireOf(t, in, "VA1000000002"))

	// The requests custom_components/freedo/api.py sends, with
	// the path its entities build: a display entity names the valve by its
	// short address. Each one succeeds.
	short := wireShort(t, in, "VA1000000002")
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodPatch, "/api/home", `{"away": true}`},
		{http.MethodPatch, "/api/home", `{"away": false}`},
		{http.MethodPut, "/api/nodes/" + short + "/display", `{"orientation": "vertical"}`},
		{http.MethodPut, "/api/nodes/" + short + "/display", `{"notification": false}`},
		{http.MethodPost, "/api/outdoor", `{"tempC": 4.5}`},
		{http.MethodPost, "/api/zones/3/window", `{"open": true}`},
		{http.MethodPost, "/api/zones/3/window", `{"open": false}`},
		{http.MethodPost, "/api/zones/3/setpoint", `{"setpointC": 21.5, "mode": "manual"}`},
		{http.MethodPost, "/api/zones/3/setpoint", `{"setpointC": 19, "mode": "timer", "durationSec": 1800}`},
		{http.MethodPost, "/api/zones/3/auto", ``},
		{http.MethodPost, "/api/zones/3/off", `{"mode": "manual"}`},
		{http.MethodPost, "/api/zones/3/boost", `{"on": true}`},
		{http.MethodPost, "/api/zones/3/boost", `{"on": false}`},
		{http.MethodPost, "/api/zones/0/setpoint", `{"setpointC": 50, "mode": "manual"}`},
		{http.MethodPost, "/api/zones/0/off", `{"mode": "manual"}`},
		{http.MethodPost, "/api/zones/0/auto", ``},
	} {
		if code, msg := postRaw(t, tc.method, url+tc.path, tc.body); code != http.StatusOK {
			t.Errorf("%s %s %s: code=%d error=%q, want 200", tc.method, tc.path, tc.body, code, msg)
		}
	}
}

// TestBoilerFlowRefusesUnknownCircuit: a flow setpoint for a circuit the
// installation does not declare would be served on no poll at all.
func TestBoilerFlowRefusesUnknownCircuit(t *testing.T) {
	in := windowInstall(t, nil)
	_, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	var body map[string]any
	code := postJSON(t, http.MethodPost, url+"/api/boiler/flow", map[string]any{"tempC": 50, "id": 7}, &body)
	if msg, _ := body["error"].(string); code != http.StatusBadRequest || !strings.Contains(msg, "circuit 7") {
		t.Fatalf("unknown circuit: code=%d body=%v, want 400 naming circuit 7", code, body)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/boiler/flow", map[string]any{"tempC": 50, "id": flowCircuitID}, &body); code != http.StatusOK {
		t.Fatalf("declared circuit: code=%d body=%v, want 200", code, body)
	}
}

// TestAPIServerBoundsHeaders: a client that never finishes its headers must not
// hold a connection open forever.
func TestAPIServerBoundsHeaders(t *testing.T) {
	srv := newAPIServer(":0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout = %v, want a bound", srv.ReadHeaderTimeout)
	}
}
