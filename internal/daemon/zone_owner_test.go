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

// TestZoneOwnerPatchOneRequest: one request moves a zone between the
// correction methods, and the owner is checked against the values the request
// leaves.
func TestZoneOwnerPatchOneRequest(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	url := a.base + "/api/install/zones/3"
	var res map[string]any
	patch := func(body map[string]any, want int) {
		t.Helper()
		if code := postJSON(t, http.MethodPatch, url, body, &res); code != want {
			t.Fatalf("%v: code=%d %v, want %d", body, code, res, want)
		}
	}
	owner := func(want string) {
		t.Helper()
		if got := loadStored(t, a).Options.ZoneOwner(3); got != want {
			t.Fatalf("stored owner = %q, want %q", got, want)
		}
	}
	// shift -> coordinator, and back to shift.
	patch(map[string]any{"offset_mode": "setpoint"}, http.StatusOK)
	patch(map[string]any{"offset_mode": "config", "owner": "coordinator"}, http.StatusOK)
	owner(tado.ZoneOwnerCoordinator)
	patch(map[string]any{"owner": "device", "offset_mode": "setpoint"}, http.StatusOK)
	owner(tado.ZoneOwnerDevice)
	// calibration -> coordinator, and back to calibration.
	patch(map[string]any{"offset_mode": "config", "auto_offset": true}, http.StatusOK)
	patch(map[string]any{"auto_offset": false, "owner": "coordinator"}, http.StatusOK)
	owner(tado.ZoneOwnerCoordinator)
	patch(map[string]any{"owner": "device", "auto_offset": true}, http.StatusOK)
	owner(tado.ZoneOwnerDevice)
	// The request still cannot hold both.
	patch(map[string]any{"auto_offset": false, "offset_mode": "setpoint"}, http.StatusOK)
	patch(map[string]any{"owner": "coordinator", "offset_mode": "setpoint"}, http.StatusBadRequest)
	patch(map[string]any{"offset_mode": "config", "auto_offset": true}, http.StatusOK)
	patch(map[string]any{"owner": "coordinator", "auto_offset": true}, http.StatusBadRequest)
	owner(tado.ZoneOwnerDevice)
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
	// 18.00 C, 78 %, and the humidity tag at 0: the fixture valve reported none.
	if m.URI() != "z/p?lid=3" || hex.EncodeToString(m.Payload) != "4060020708"+"40a0014e"+"0135020000" {
		t.Errorf("push = %s %x, want z/p?lid=3 with 18.00 C, 78 %% and 0135", m.URI(), m.Payload)
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
	if got := hex.EncodeToString(v.([]byte)); got != "4060020708"+"40a0014e"+"0135020000" {
		t.Errorf("served measure = %s, want the pushed one", got)
	}
	// The wall thermostat observes the measure: the next change notifies it.
	thermostat := wireOf(t, stored, "RU1000000001")
	if _, err := d.do(func(e *tado.EmuState) (any, error) {
		e.OnFrame(uplinkFrame(t, stored, "RU1000000001", true, tado.CoAPReq{
			Type: "CON", Code: "0.01", MID: 0x7a02, Token: []byte{0xa1, 0xb2},
			Path: []string{"z", "p"}, Options: []tado.CoAPOption{{Num: 6, Val: []byte{}}},
		}))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	queued := pending(t, d, thermostat)
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
	if got := tado.ZoneMeasureSummary(m.Payload); got != "4060=18.00 40a0=80% 0135=0000" {
		t.Errorf("second push = %s, want a demand of 80", got)
	}
	if n := pending(t, d, thermostat); n != queued+1 {
		t.Errorf("thermostat pending = %d, want %d: the notification", n, queued+1)
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
	ru, _ := stored.Resolve("RU1000000001")
	if obs, _ := owner["observers"].([]any); len(obs) != 1 || obs[0].(map[string]any)["device"] != ru.Short() ||
		obs[0].(map[string]any)["token"] != "a1b2" || obs[0].(map[string]any)["seq"] != 2.0 {
		t.Errorf("observers = %v, want the thermostat under a1b2 at 2", owner["observers"])
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

// TestZoneOwnerRolesFollowOwner: the owner of a zone decides the election. The
// reload that follows an owner change rewrites the 015e record of the valve:
// no elected bit under the coordinator, the declared reporter elected again
// under the device, with a config write queued for the valve.
func TestZoneOwnerRolesFollowOwner(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	a.reportThermostatModule(71)
	a.mutate(http.MethodPost, "/api/install/zones", map[string]any{"number": 1, "kind": "heating"})
	a.mutate(http.MethodPut, "/api/install/zones/1/members", map[string]any{"members": []string{"VA9000000001"}})
	a.mutate(http.MethodPatch, "/api/install/devices/VA9000000001", map[string]any{"zone": 1})
	a.mutate(http.MethodPut, "/api/install/zones/1/reporter", map[string]any{"ref": "VA9000000001"})
	if role, ok := a.zoneRole(apiValveEUI, 1); !ok || role != 0x0d {
		t.Fatalf("valve 015e role = %#02x (present %v), want the elected 0x0d", role, ok)
	}
	a.mutate(http.MethodPatch, "/api/install/zones/1", map[string]any{"owner": "coordinator"})
	if role, ok := a.zoneRole(apiValveEUI, 1); !ok || role != 0x05 {
		t.Fatalf("valve 015e role under the coordinator = %#02x (present %v), want the plain 0x05", role, ok)
	}
	// One config write waits for the valve: the reload coalesces it with the
	// one the election queued.
	if n := a.pending()[apiValveEUI]; n != 1 {
		t.Fatalf("config writes queued to the valve = %d, want 1", n)
	}
	a.mutate(http.MethodPatch, "/api/install/zones/1", map[string]any{"owner": "device"})
	if role, ok := a.zoneRole(apiValveEUI, 1); !ok || role != 0x0d {
		t.Fatalf("valve 015e role back under the device = %#02x (present %v), want the elected 0x0d", role, ok)
	}
}

// TestZoneOwnerObserveSurvivesRestart: the observe registration of the wall
// thermostat is stored, and the first step after a restart on the same
// database notifies it under the same token with the next sequence, so the
// thermostat does not hold its last demand until its own deadline.
func TestZoneOwnerObserveSurvivesRestart(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, func(in *tado.Install) {
		in.Options.ZoneOwners = map[int]string{3: tado.ZoneOwnerCoordinator}
	}))
	stored := loadStored(t, a)
	thermostat := wireOf(t, stored, "RU1000000001")
	start := func(at time.Time, in *tado.Install) (*daemon, *testClock, string) {
		t.Helper()
		clock := newTestClock(at)
		d, url := newWindowDaemon(t, in, clock, a.store)
		if _, err := d.do(func(e *tado.EmuState) (any, error) { return nil, d.restoreZoneObservers(e) }); err != nil {
			t.Fatal(err)
		}
		seedBlock(t, d, in, earlyValve, 3)
		if !in.Options.CoordinatorOwns(3) {
			return d, clock, url
		}
		var view map[string]any
		if code := postJSON(t, http.MethodPost, url+"/api/zones/3/temperature", map[string]any{"tempC": 18}, &view); code != http.StatusOK {
			t.Fatalf("temperature: code=%d %v", code, view)
		}
		return d, clock, url
	}
	tick := func(d *daemon) {
		t.Helper()
		if _, err := d.do(func(e *tado.EmuState) (any, error) { d.tickZoneOwners(e); return nil, nil }); err != nil {
			t.Fatal(err)
		}
	}
	notified := func(d *daemon, desc string) bool {
		t.Helper()
		v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Queue(thermostat), nil })
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range v.([]tado.QueueEntry) {
			if q.Desc == desc {
				return true
			}
		}
		return false
	}

	d, clock, url := start(windowStart, stored)
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 19.5}), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	tick(d)
	if _, err := d.do(func(e *tado.EmuState) (any, error) {
		e.OnFrame(uplinkFrame(t, stored, "RU1000000001", true, tado.CoAPReq{
			Type: "CON", Code: "0.01", MID: 0x7a02, Token: []byte{0xa1, 0xb2},
			Path: []string{"z", "p"}, Options: []tado.CoAPOption{{Num: 6, Val: []byte{}}},
		}))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/zones/3/temperature", map[string]any{"tempC": 18.5}, &view); code != http.StatusOK {
		t.Fatalf("temperature: code=%d %v", code, view)
	}
	clock.advance(time.Minute)
	tick(d)
	if !notified(d, "zone notify z/p lid=3 obs=2 tok=a1b2") {
		t.Fatal("no notification at obs=2 before the restart")
	}
	rows, err := a.store.ZoneObservers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Zone != 3 || rows[0].Target != thermostat || rows[0].Token != "a1b2" || rows[0].Seq != 2 {
		t.Fatalf("stored observers = %+v, want the thermostat under a1b2 at 2", rows)
	}

	// The restart: the measure is the same, and the thermostat still gets it
	// within the first step.
	d2, _, _ := start(clock.now().Add(5*time.Minute), stored)
	tick(d2)
	if !notified(d2, "zone notify z/p lid=3 obs=3 tok=a1b2") {
		v, _ := d2.do(func(e *tado.EmuState) (any, error) { return e.Queue(thermostat), nil })
		t.Fatalf("no notification at obs=3 after the restart, queue %+v", v)
	}
	if rows, err := a.store.ZoneObservers(t.Context()); err != nil || len(rows) != 1 || rows[0].Seq != 3 {
		t.Errorf("stored observers after the restart = %+v (err %v), want seq 3", rows, err)
	}

	// A zone the coordinator no longer owns drops the stored registration.
	if code := postJSON(t, http.MethodPatch, a.base+"/api/install/zones/3", map[string]any{"owner": "device"}, &view); code != http.StatusOK {
		t.Fatalf("owner device: code=%d %v", code, view)
	}
	d3, _, _ := start(clock.now().Add(10*time.Minute), loadStored(t, a))
	tick(d3)
	if rows, err := a.store.ZoneObservers(t.Context()); err != nil || len(rows) != 0 {
		t.Errorf("stored observers of a zone the device owns = %+v (err %v), want none", rows, err)
	}
}
