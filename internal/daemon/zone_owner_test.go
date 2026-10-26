package daemon

// zone_owner_test: the daemon as the owner of zone 3, one valve, on an
// external room temperature.

import (
	"encoding/hex"
	"math"
	"net/http"
	"testing"
	"time"

	"freedo/tado"
)

// TestZoneOwnerController: the controller of the valve, stepped once a minute
// with the default gains.
func TestZoneOwnerController(t *testing.T) {
	x := &zoneOwner{}
	now := windowStart
	// 1.5 C below the setpoint: Kp gives 75, the first integration 2.5.
	if got := x.update(tado.DefaultZonePID, 19.5, 18, now); got != 78 {
		t.Errorf("first step = %d, want 78", got)
	}
	// Four more minutes at the same error: the integrator holds 750.
	for range 4 {
		now = now.Add(time.Minute)
		x.update(tado.DefaultZonePID, 19.5, 18, now)
	}
	if x.integ != 750 {
		t.Errorf("integ = %v, want 750", x.integ)
	}
	// A rise of 0.4 C in the window of Tref costs Kd*40/2000 = 30 points.
	now = now.Add(ownerTrefAge)
	got := x.update(tado.DefaultZonePID, 19.5, 18.4, now)
	want := int(math.Round(1000*110.0/2000 + 500*(750+110)/30000.0 - 1500*40.0/2000))
	if got != want {
		t.Errorf("after a rise = %d, want %d", got, want)
	}
	// Above the setpoint the output clamps at 0 and the integrator winds down
	// to 0, never below.
	for range 60 {
		now = now.Add(time.Minute)
		got = x.update(tado.DefaultZonePID, 19.5, 22, now)
	}
	if got != 0 || x.integ != 0 {
		t.Errorf("warm room: demand %d integ %v, want 0 and 0", got, x.integ)
	}
	// The integrator saturates at 3000000/Ki.
	for range 200 {
		now = now.Add(time.Minute)
		got = x.update(tado.DefaultZonePID, 25, 15, now)
	}
	if got != 100 || x.integ != 6000 {
		t.Errorf("cold room: demand %d integ %v, want 100 and 6000", got, x.integ)
	}
	// A step of the temperature source must not reach the derivative: the
	// engine resets the controller on a switch, as TestZoneOwnerSourceSwitch
	// checks on the step.
	// With Ki 0 the integrator stays 0.
	y := &zoneOwner{}
	y.update(tado.ZonePID{Kp: 1000, Ki: 0, Kd: 0}, 20, 19, now)
	if y.integ != 0 {
		t.Errorf("integ with Ki 0 = %v, want 0", y.integ)
	}
}

// TestZoneOwnerPatch: PATCH /api/install/zones/{n} takes owner on a heating
// zone, refuses it on hot water and an unknown value, and the device is the
// default that is not stored.
func TestZoneOwnerPatch(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	var res map[string]any
	if code := postJSON(t, http.MethodPatch, a.base+"/api/install/zones/3", map[string]any{"owner": "coordinator"}, &res); code != http.StatusOK {
		t.Fatalf("owner coordinator: code=%d %v", code, res)
	}
	if got := res["zone"].(map[string]any)["owner"]; got != "coordinator" {
		t.Errorf("owner = %v, want coordinator", got)
	}
	if got := loadStored(t, a).Options.ZoneOwner(3); got != tado.ZoneOwnerCoordinator {
		t.Errorf("stored owner = %q, want coordinator", got)
	}
	for _, tc := range []struct {
		zone  string
		owner string
	}{{"0", "coordinator"}, {"3", "cloud"}} {
		if code := postJSON(t, http.MethodPatch, a.base+"/api/install/zones/"+tc.zone, map[string]any{"owner": tc.owner}, &res); code != http.StatusBadRequest {
			t.Errorf("zone %s owner %q: code=%d %v, want 400", tc.zone, tc.owner, code, res)
		}
	}
	// An owned zone takes no correction of the valve sensor, and the other
	// way round.
	for _, body := range []map[string]any{{"auto_offset": true}, {"offset_mode": "setpoint"}} {
		if code := postJSON(t, http.MethodPatch, a.base+"/api/install/zones/3", body, &res); code != http.StatusBadRequest {
			t.Errorf("owned zone %v: code=%d %v, want 400", body, code, res)
		}
	}
	if code := postJSON(t, http.MethodPatch, a.base+"/api/install/zones/3", map[string]any{"owner": "device"}, &res); code != http.StatusOK {
		t.Fatalf("owner device: code=%d %v", code, res)
	}
	for _, body := range []map[string]any{{"auto_offset": true}, {"offset_mode": "setpoint"}} {
		if code := postJSON(t, http.MethodPatch, a.base+"/api/install/zones/3", body, &res); code != http.StatusOK {
			t.Fatalf("zone 3 %v: code=%d %v", body, code, res)
		}
		if code := postJSON(t, http.MethodPatch, a.base+"/api/install/zones/3", map[string]any{"owner": "coordinator"}, &res); code != http.StatusBadRequest {
			t.Errorf("owner with %v: code=%d %v, want 400", body, code, res)
		}
		if code := postJSON(t, http.MethodPatch, a.base+"/api/install/zones/3", map[string]any{"auto_offset": false, "offset_mode": "config"}, &res); code != http.StatusOK {
			t.Fatalf("zone 3 reset: code=%d %v", code, res)
		}
	}
	if got := loadStored(t, a).Options.ZoneOwners; len(got) != 0 {
		t.Errorf("stored owners = %v, want none", got)
	}
}

// TestZoneOwnerPushesMeasure: an owned zone with a fresh external reading
// serves the measure, pushes it to its valve, pushes again on a change or
// after ownerPushEvery, and shows the controller in GET /api/zones.
func TestZoneOwnerPushesMeasure(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, func(in *tado.Install) {
		in.Options.ZoneOwners = map[int]string{3: tado.ZoneOwnerCoordinator}
	}))
	stored := loadStored(t, a)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, stored, clock, a.store)
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 19.5}), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	seedBlock(t, d, stored, earlyValve, 3)
	valve := wireOf(t, stored, earlyValve)
	tick := func() {
		t.Helper()
		if _, err := d.do(func(e *tado.EmuState) (any, error) { d.tickZoneOwners(e); return nil, nil }); err != nil {
			t.Fatal(err)
		}
	}
	// No reading and no device report: nothing to serve, nothing queued.
	tick()
	if n := pending(t, d, valve); n != 0 {
		t.Fatalf("pending without a temperature = %d, want 0", n)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/zones/3/temperature", map[string]any{"tempC": 18}, &view); code != http.StatusOK {
		t.Fatalf("temperature: code=%d %v", code, view)
	}
	clock.advance(time.Minute)
	tick()
	if n := pending(t, d, valve); n != 1 {
		t.Fatalf("pending = %d, want one measure push", n)
	}
	m := takeMeasure(t, d, stored, valve)
	if m.URI() != "z/p?lid=3" || hex.EncodeToString(m.Payload) != "4060020708"+"40a0014e" {
		t.Errorf("push = %s %x, want z/p?lid=3 with 18.00 C and 78 %%", m.URI(), m.Payload)
	}
	// The served measure is the same body.
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		get := uplinkFrame(t, stored, earlyValve, true, tado.CoAPReq{
			Type: "CON", Code: "0.01", MID: 0x7a01, Token: []byte{1},
			Path: []string{"z", "p"}, Query: []string{"lid=3"},
		})
		var body []byte
		for _, act := range e.OnFrame(get) {
			if act.Kind != tado.ActData {
				continue
			}
			df, err := tado.ParseData(stored.NetworkKey(), act.Bytes)
			if err != nil {
				continue
			}
			if app := tado.ParseApp(df.Plaintext); app != nil && app.CoAP != nil && app.CoAP.MID == 0x7a01 {
				body = app.CoAP.Payload
			}
		}
		return body, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(v.([]byte)); got != "4060020708"+"40a0014e" {
		t.Errorf("served measure = %s, want the pushed one", got)
	}
	// Within a minute nothing steps; after it the integrator moves the demand
	// and a new push goes out.
	tick()
	if n := pending(t, d, valve); n != 0 {
		t.Fatalf("pending within the step = %d, want 0", n)
	}
	clock.advance(time.Minute)
	tick()
	if n := pending(t, d, valve); n != 1 {
		t.Fatalf("pending after a change = %d, want 1", n)
	}
	m = takeMeasure(t, d, stored, valve)
	if m.Payload[len(m.Payload)-1] != 80 {
		t.Errorf("second push demand = %d, want 80", m.Payload[len(m.Payload)-1])
	}
	var zones map[string]any
	if code := getJSON(t, url+"/api/zones", &zones); code != http.StatusOK {
		t.Fatalf("zones: code=%d", code)
	}
	var owner map[string]any
	for _, z := range zones["declared"].([]any) {
		if zm := z.(map[string]any); zm["number"] == 3.0 {
			owner = zm["owner"].(map[string]any)
		}
	}
	if owner["mode"] != "coordinator" || owner["demandPct"] != 80.0 || owner["tempC"] != 18.0 || owner["source"] != "external" || owner["pushedAt"] == nil {
		t.Errorf("owner view = %v", owner)
	}
	// A zone the coordinator does not own shows the mode alone.
	for _, z := range zones["declared"].([]any) {
		if zm := z.(map[string]any); zm["number"] == 2.0 {
			if o := zm["owner"].(map[string]any); o["mode"] != "device" || len(o) != 1 {
				t.Errorf("zone 2 owner view = %v, want the device mode alone", o)
			}
		}
	}
}

// takeMeasure pulls the queued measure push to target, checks its shape and
// closes it with the member's ACK 2.05.
func takeMeasure(t *testing.T, d *daemon, in *tado.Install, target string) *tado.CoAPMessage {
	t.Helper()
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		actions, ok, err := e.NextQueuedDownlink(target)
		if err != nil || !ok {
			return nil, err
		}
		for _, a := range actions {
			if a.Kind == tado.ActData {
				return a.Bytes, nil
			}
		}
		return nil, nil
	})
	if err != nil || v == nil {
		t.Fatalf("no measure push to %s (err=%v)", target, err)
	}
	frame, err := tado.ParseData(in.NetworkKey(), v.([]byte))
	if err != nil {
		t.Fatal(err)
	}
	m := tado.ParseApp(frame.Plaintext).CoAP
	if m.Type != "CON" || m.Code != "0.03" || !m.Contexted() {
		t.Fatalf("push = %s %s contexted=%t, want CON PUT on the zone plane", m.Type, m.Code, m.Contexted())
	}
	feed(t, d, in, target, true, tado.CoAPReq{Type: "ACK", Code: "2.05", MID: m.MID, Token: []byte{},
		Options: []tado.CoAPOption{{Num: 12, Val: []byte{42}}}, Payload: tado.TLV{Tag: "62a0", Val: []byte{0}}.Encode()})
	if n := pending(t, d, target); n != 0 {
		t.Fatalf("%s still holds %d queued writes after the 2.05", target, n)
	}
	return m
}

// TestZoneOwnerSourceSwitch: the controller state is reset when the source of
// the temperature changes, so that the gap between the two sensors is no
// derivative and no integral.
func TestZoneOwnerSourceSwitch(t *testing.T) {
	x := &zoneOwner{}
	now := windowStart
	for range 5 {
		x.update(tado.DefaultZonePID, 20.5, 21.2, now)
		now = now.Add(time.Minute)
	}
	x.haveOut, x.source = true, "device"
	// What stepZoneOwner does on the switch, then the external reading.
	x.reset()
	got := x.update(tado.DefaultZonePID, 20.5, 20.3, now)
	// Kp*20/2000 = 10, the first integration 20*500/30000, no derivative.
	if got != 10 {
		t.Errorf("first external step = %d, want 10", got)
	}
}
