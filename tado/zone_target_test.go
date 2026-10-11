package tado

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"
)

// TestViewJSONKeys pins the JSON keys of the views the Home Assistant
// integration reads: a Go rename must not move them.
func TestViewJSONKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		view any
		want []string
	}{
		{"ValveView", ValveView{}, []string{
			"BatteryMV", "BatteryPct", "CSLPeriod", "CSLPhase", "ChildLock", "CommsWarnings", "Display", "FallbackMode", "Fault", "FaultWord",
			"HeatDemandPct", "HumidityPct", "LastSeenUnix", "LowBattery", "Mode", "Offline", "OpenWindow",
			"RelayState", "Room", "RxCounter", "SchedSetpointC", "Serial", "SessionAdopted", "SessionIssued",
			"SessionIssuedUnix", "SetpointC", "Short", "Telemetry", "TempC",
		}},
		{"ZoneView", ZoneView{}, []string{
			"Circuit", "DemandPct", "Fallback", "FallbackMode", "FallbackSinceUnix", "HeatPct", "Kind",
			"Name", "Number", "Reporter", "Source", "TempC", "UpdatedUnix",
		}},
		{"CircuitView", CircuitView{}, []string{
			"Boiler", "DemandPct", "HeatActive", "HeatSource", "ID", "Name", "Source", "UpdatedUnix",
		}},
	} {
		raw, err := json.Marshal(tc.view)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(m))
		for k := range m {
			got = append(got, k)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s keys = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRestoredTimers: a stored timer comes back with the flags the store does
// not keep. The hot-water deadline restores the hot-water block, and a room
// overlay at the boost temperature reads as the boost.
func TestRestoredTimers(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	cfg := fixtureInstall(t).EmuConfig()
	cfg.Now = func() time.Time { return now }
	e := NewEmu(cfg, nil)
	if err := e.EnqueueDHWOverlay("RU1000000001", 0, 50, true, time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueBoost("camera", 10, true); err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueSetpoint("bagno", SetpointParams{ID: 2, Setpoint: 21, Sched: 20, Overlay: 3, Duration: time.Hour}); err != nil {
		t.Fatal(err)
	}
	e.storeZoneState(10, "z/s", encodeTLVs([]TLV{
		{Tag: "6240", Val: []byte{0x03}}, {Tag: "6260", Val: []byte{0x01}}, {Tag: "6280", Val: []byte{0x09, 0xc4}},
	}))
	e.storeZoneState(2, "z/s", encodeTLVs([]TLV{
		{Tag: "6240", Val: []byte{0x03}}, {Tag: "6260", Val: []byte{0x01}}, {Tag: "6280", Val: []byte{0x08, 0x34}},
	}))
	if timer, ok := e.ZoneOverlayTimer(10); !ok || !timer.Boost {
		t.Fatalf("live boost timer = %+v ok=%t, want a boost", timer, ok)
	}

	restored := NewEmu(cfg, nil)
	if err := restored.RestoreDesiredState(e.ExportDesiredState()); err != nil {
		t.Fatal(err)
	}
	if timer, ok := restored.ZoneOverlayTimer(10); !ok || !timer.Boost || !timer.ExpiresAt.Equal(now.Add(BoostDuration)) {
		t.Errorf("restored boost = %+v ok=%t", timer, ok)
	}
	if timer, ok := restored.ZoneOverlayTimer(2); !ok || timer.Boost {
		t.Errorf("restored 21 C timer = %+v ok=%t, want no boost", timer, ok)
	}
	due, err := restored.ExpireBoosts(now.Add(time.Hour))
	if err != nil || len(due) != 3 {
		t.Fatalf("expiry due=%v err=%v", due, err)
	}
	wire, _, err := restored.resolve("RU1000000001")
	if err != nil {
		t.Fatal(err)
	}
	queue := restored.queue[hexEUI(wire)]
	if len(queue) != 1 || queue[0].kind != "dhw" {
		t.Fatalf("thermostat queue = %+v, want the hot-water restore", queue)
	}
	msg, err := ParseCoAP(WrapAppPayload(queue[0].coap, 1, nil)[8:])
	if err != nil {
		t.Fatal(err)
	}
	tags := map[string]string{}
	for _, tlv := range ParseTLVs(msg.Payload) {
		tags[tlv.Tag] = hex.EncodeToString(tlv.Val)
	}
	if msg.Query != "id=0" || tags["6020"] != "02" || tags["6240"] != "00" {
		t.Errorf("hot-water restore = %s %v, want 6020=02 6240=00 on id=0", msg.Query, tags)
	}
}

// takeRestores delivers the queue of wire, each write answered 2.04, and
// returns "kind query" per write.
func takeRestores(t *testing.T, e *EmuState, wire string) []string {
	t.Helper()
	var out []string
	for len(e.queue[wire]) > 0 {
		msg, err := ParseCoAP(WrapAppPayload(e.queue[wire][0].coap, 1, nil)[8:])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e.queue[wire][0].kind+" "+msg.Query)
		answerQueued(t, e, mustHex(wire), "2.04")
	}
	return out
}

func wantZoneTimer(t *testing.T, e *EmuState, what string, zone int, target string, expires time.Time) {
	t.Helper()
	timer, ok := e.ZoneOverlayTimer(zone)
	if !ok || timer.Target != target || !timer.ExpiresAt.Equal(expires) {
		t.Errorf("%s: zone %d timer = %+v ok=%t, want %s at %s", what, zone, timer, ok, target, expires)
	}
}

// TestOverlayTimersPerZone: the wall thermostat writes the hot-water zone and
// a heating zone, and holds one deadline per zone. A write that ends the
// overlay of one zone leaves the deadline of the other in place, and each
// expiry restores its own zone only.
func TestOverlayTimersPerZone(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	cfg := fixtureInstall(t).EmuConfig()
	cfg.Now = func() time.Time { return now }
	const ru = "05a2cafa07c51b00"
	room := func(e *EmuState, overlay byte, d time.Duration) {
		t.Helper()
		if err := e.EnqueueSetpoint(ru, SetpointParams{ID: 1, Setpoint: 21, Sched: 20, Overlay: overlay, Duration: d}); err != nil {
			t.Fatal(err)
		}
	}
	expire := func(e *EmuState, at time.Time) []string {
		t.Helper()
		due, err := e.ExpireBoosts(at)
		if err != nil {
			t.Fatal(err)
		}
		return due
	}

	t.Run("room write keeps the hot-water timer", func(t *testing.T) {
		e := NewEmu(cfg, nil)
		if err := e.EnqueueDHWOverlay(ru, 0, 50, true, 4*time.Hour, ""); err != nil {
			t.Fatal(err)
		}
		room(e, 2, 0)
		room(e, 3, 30*time.Minute)
		takeRestores(t, e, ru)
		wantZoneTimer(t, e, "armed", 0, ru, now.Add(4*time.Hour))
		wantZoneTimer(t, e, "armed", 1, ru, now.Add(30*time.Minute))

		if due := expire(e, now.Add(30*time.Minute)); !reflect.DeepEqual(due, []string{ru}) {
			t.Fatalf("room expiry due = %v, want %s", due, ru)
		}
		if got := takeRestores(t, e, ru); !reflect.DeepEqual(got, []string{"boost id=1"}) {
			t.Errorf("room expiry writes %v, want the zone 1 restore alone", got)
		}
		wantZoneTimer(t, e, "after the room expiry", 0, ru, now.Add(4*time.Hour))
		if timer, ok := e.ZoneOverlayTimer(1); ok {
			t.Errorf("zone 1 timer = %+v after its expiry", timer)
		}

		if due := expire(e, now.Add(5*time.Hour)); !reflect.DeepEqual(due, []string{ru}) {
			t.Fatalf("hot-water expiry due = %v, want %s", due, ru)
		}
		if got := takeRestores(t, e, ru); !reflect.DeepEqual(got, []string{"dhw id=0"}) {
			t.Errorf("hot-water expiry writes %v, want the zone 0 restore alone", got)
		}
		if due := expire(e, now.Add(6*time.Hour)); len(due) != 0 {
			t.Errorf("expiry after both = %v, want none", due)
		}
	})

	t.Run("hot-water return keeps the room timer", func(t *testing.T) {
		e := NewEmu(cfg, nil)
		room(e, 3, 30*time.Minute)
		if err := e.EnqueueDHWOverlay(ru, 0, 50, true, 4*time.Hour, ""); err != nil {
			t.Fatal(err)
		}
		if err := e.EnqueueDHWSchedule(ru, 0); err != nil {
			t.Fatal(err)
		}
		takeRestores(t, e, ru)
		if timer, ok := e.ZoneOverlayTimer(0); ok {
			t.Errorf("zone 0 timer = %+v after its return to schedule", timer)
		}
		wantZoneTimer(t, e, "after the hot-water return", 1, ru, now.Add(30*time.Minute))
		expire(e, now.Add(time.Hour))
		if got := takeRestores(t, e, ru); !reflect.DeepEqual(got, []string{"boost id=1"}) {
			t.Errorf("expiry writes %v, want the zone 1 restore alone", got)
		}
	})

	t.Run("room return keeps the hot-water timer", func(t *testing.T) {
		e := NewEmu(cfg, nil)
		if err := e.EnqueueDHWOverlay(ru, 0, 50, true, 4*time.Hour, ""); err != nil {
			t.Fatal(err)
		}
		if err := e.EnqueueBoost(ru, 1, true); err != nil {
			t.Fatal(err)
		}
		if err := e.EnqueueSchedule(ru, 1); err != nil {
			t.Fatal(err)
		}
		takeRestores(t, e, ru)
		if timer, ok := e.ZoneOverlayTimer(1); ok {
			t.Errorf("zone 1 timer = %+v after its return to schedule", timer)
		}
		wantZoneTimer(t, e, "after the room return", 0, ru, now.Add(4*time.Hour))
	})

	t.Run("persistence", func(t *testing.T) {
		e := NewEmu(cfg, nil)
		if err := e.EnqueueDHWOverlay(ru, 0, 50, true, 4*time.Hour, ""); err != nil {
			t.Fatal(err)
		}
		room(e, 3, 30*time.Minute)
		restored := NewEmu(cfg, nil)
		if err := restored.RestoreDesiredState(roundTripState(t, e.ExportDesiredState())); err != nil {
			t.Fatal(err)
		}
		wantZoneTimer(t, restored, "restored", 0, ru, now.Add(4*time.Hour))
		wantZoneTimer(t, restored, "restored", 1, ru, now.Add(30*time.Minute))
	})

	t.Run("device-keyed state", func(t *testing.T) {
		state := NewEmu(cfg, nil).ExportDesiredState()
		state.Boost = map[string]DesiredBoostTimer{ru: {ID: 0, ExpiresAt: now.Add(4 * time.Hour)}}
		restored := NewEmu(cfg, nil)
		if err := restored.RestoreDesiredState(state); err != nil {
			t.Fatal(err)
		}
		wantZoneTimer(t, restored, "restored from a device key", 0, ru, now.Add(4*time.Hour))
		room(restored, 2, 0)
		wantZoneTimer(t, restored, "after a zone 1 write", 0, ru, now.Add(4*time.Hour))
		if got := restored.ExportDesiredState().Boost; !reflect.DeepEqual(got, map[string]DesiredBoostTimer{
			ru + "|0": {ID: 0, ExpiresAt: now.Add(4 * time.Hour)},
		}) {
			t.Errorf("exported timers = %v, want the zone key", got)
		}
		expire(restored, now.Add(5*time.Hour))
		if got := takeRestores(t, restored, ru); !reflect.DeepEqual(got[len(got)-1:], []string{"dhw id=0"}) {
			t.Errorf("expiry writes %v, want the hot-water restore last", got)
		}

		state.Boost[ru+"|0"] = DesiredBoostTimer{ID: 0, ExpiresAt: now.Add(time.Hour)}
		both := NewEmu(cfg, nil)
		if err := both.RestoreDesiredState(state); err != nil {
			t.Fatal(err)
		}
		wantZoneTimer(t, both, "zone key beside a device key", 0, ru, now.Add(time.Hour))
	})

	t.Run("invalid keys", func(t *testing.T) {
		for _, key := range []string{ru + "|1", ru + "|x", ru + "|00", "zz|0", ru + "|"} {
			state := NewEmu(cfg, nil).ExportDesiredState()
			state.Boost = map[string]DesiredBoostTimer{key: {ID: 0, ExpiresAt: now}}
			if err := NewEmu(cfg, nil).RestoreDesiredState(state); err == nil {
				t.Errorf("timer key %q for zone 0 restored, want an error", key)
			}
		}
	})
}

// roundTripState passes state through its JSON form.
func roundTripState(t *testing.T, state DesiredState) DesiredState {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var out DesiredState
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestDroppedReturnKeepsTimer: the return to schedule an expiry queues ends
// the deadline only when the node takes it. A tick finds the return queued and
// adds none. A return the retry limit or a 4.xx drops leaves the overlay on
// the node and its deadline due, so the next tick queues the return again.
func TestDroppedReturnKeepsTimer(t *testing.T) {
	const ru = "05a2cafa07c51b00"
	for _, tc := range []struct {
		name, target string
		zone         int
		start        func(e *EmuState) error
		drop         func(e *EmuState)
	}{
		{"room boost, retry limit", cameraEUIHex, 10,
			func(e *EmuState) error { return e.EnqueueBoost(cameraEUIHex, 10, true) },
			func(e *EmuState) {
				for range maxDownlinkTries + 1 {
					e.NextQueuedDownlink(cameraEUIHex)
				}
			}},
		{"hot water, 4.04", ru, 0,
			func(e *EmuState) error { return e.EnqueueDHWOverlay(ru, 0, 50, true, BoostDuration, "") },
			func(e *EmuState) { answerQueued(t, e, mustHex(ru), "4.04") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, now := clockedFixtureEmu(t)
			if err := tc.start(e); err != nil {
				t.Fatal(err)
			}
			answerQueued(t, e, mustHex(tc.target), "2.04")
			*now = now.Add(BoostDuration)
			for tick := range 2 {
				if _, err := e.ExpireBoosts(*now); err != nil {
					t.Fatal(err)
				}
				if n := e.Pending(tc.target); n != 1 {
					t.Fatalf("tick %d: %d writes queued, want the one return", tick, n)
				}
			}
			if _, ok := e.ZoneOverlayTimer(tc.zone); !ok || len(e.ExportDesiredState().Boost) != 1 {
				t.Fatal("the queued return dropped the deadline and its stored form")
			}
			tc.drop(e)
			if n := e.Pending(tc.target); n != 0 {
				t.Fatalf("%d writes queued after the drop, want none", n)
			}
			if _, ok := e.ZoneOverlayTimer(tc.zone); !ok || len(e.ExportDesiredState().Boost) != 1 {
				t.Fatal("the dropped return dropped the deadline and its stored form")
			}
			if due, err := e.ExpireBoosts(*now); err != nil || len(due) != 1 || e.Pending(tc.target) != 1 {
				t.Fatalf("tick after the drop: due=%v err=%v pending=%d, want the return again", due, err, e.Pending(tc.target))
			}
			answerQueued(t, e, mustHex(tc.target), "2.04")
			if timer, ok := e.ZoneOverlayTimer(tc.zone); ok || len(e.ExportDesiredState().Boost) != 0 {
				t.Errorf("delivered return left the deadline %+v", timer)
			}
			if !e.TakeDeliveredChanged() {
				t.Error("the ended deadline is not marked for the owner to persist")
			}
			if due, _ := e.ExpireBoosts(now.Add(time.Hour)); len(due) != 0 {
				t.Errorf("tick after the delivered return queued %v", due)
			}
		})
	}
}

// TestModeEndsTimer: a mode write carries no duration, so the overlay it sets
// has no deadline. The deadline of the overlay it replaces ends when the node
// takes it, and a manual mode then never expires.
func TestModeEndsTimer(t *testing.T) {
	e, now := clockedFixtureEmu(t)
	camera := mustHex(cameraEUIHex)
	e.SetHomeState(true, map[int]float64{10: 20})
	if err := e.EnqueueSetpoint(cameraEUIHex, SetpointParams{ID: 10, Setpoint: 22, Overlay: 3, Duration: time.Hour}); err != nil {
		t.Fatal(err)
	}
	answerQueued(t, e, camera, "2.04")
	if err := e.EnqueueMode(cameraEUIHex, 10, 2); err != nil {
		t.Fatal(err)
	}
	answerQueued(t, e, camera, "2.04")
	if timer, ok := e.ZoneOverlayTimer(10); ok {
		t.Errorf("manual mode left the deadline %+v", timer)
	}
	*now = now.Add(2 * time.Hour)
	if due, err := e.ExpireBoosts(*now); err != nil || len(due) != 0 {
		t.Errorf("manual mode expired: due=%v err=%v", due, err)
	}
}

// TestSetInstallDropsTimersOfRemovedZone: a zone the reload removes loses its
// overlay deadline with the rest of its state.
func TestSetInstallDropsTimersOfRemovedZone(t *testing.T) {
	e, _ := clockedFixtureEmu(t)
	if err := e.EnqueueBoost(cameraEUIHex, 10, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.SetInstall(variantInstall(t, [2]string{zone10, ""})); err != nil {
		t.Fatalf("SetInstall: %v", err)
	}
	if timer, ok := e.ZoneOverlayTimer(10); ok {
		t.Errorf("zone 10 kept its deadline %+v", timer)
	}
}

// TestZoneWritesKeepOpenWindow: on a zone the window engine does not drive, a
// write keeps the 62e0 the zone holds: the one of a write queued ahead, an
// open-window write included, else the node's. A driven zone takes the value
// of the engine.
func TestZoneWritesKeepOpenWindow(t *testing.T) {
	e, _ := clockedFixtureEmu(t)
	camera := mustHex(cameraEUIHex)
	e.SetHomeState(true, map[int]float64{10: 20})
	e.cacheTLVs(e.node(camera), []TLV{
		{Tag: "6280", Val: []byte{0x08, 0x34}}, {Tag: "6240", Val: []byte{0x00}}, {Tag: "62e0", Val: []byte{0x00}},
	}, "", ClassData, "camera", "bridge", "")
	if err := e.EnqueueOpenWindow(cameraEUIHex, 10, true); err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueZoneRewrite(cameraEUIHex, 10); err != nil {
		t.Fatal(err)
	}
	if got := queuedTags(t, e, cameraEUIHex); len(got) != 2 || got[1]["62e0"] != "01" {
		t.Fatalf("rewrite behind the open window = %v, want 62e0 01", got)
	}
	answerQueued(t, e, camera, "2.04")
	answerQueued(t, e, camera, "2.04")

	if err := e.EnqueueSetpoint(cameraEUIHex, SetpointParams{ID: 10, Setpoint: 22, Overlay: 2}); err != nil {
		t.Fatal(err)
	}
	if got := queuedTags(t, e, cameraEUIHex); len(got) != 1 || got[0]["62e0"] != "01" {
		t.Fatalf("setpoint on the open zone = %v, want 62e0 01", got)
	}

	e.SetOpenWindows(map[int]bool{10: false})
	if err := e.EnqueueZoneRewrite(cameraEUIHex, 10); err != nil {
		t.Fatal(err)
	}
	if got := queuedTags(t, e, cameraEUIHex); len(got) != 2 || got[1]["62e0"] != "00" {
		t.Errorf("rewrite on a zone the engine closes = %v, want 62e0 00 behind the setpoint", got)
	}
}

// TestThermostatBlockKeepsItsZone: the node cache holds one heating block, and
// the wall thermostat sits in the hot-water zone and in a room. A rewrite of
// the room does not restate the block the thermostat reported for the other
// zone. No capture holds a z/s?lid=0 report: the frame is synthetic.
func TestThermostatBlockKeepsItsZone(t *testing.T) {
	const ru = "05a2cafa07c51b00"
	in := fixtureInstall(t)
	e := NewEmu(in.EmuConfig(), NewUnhandledLogger(io.Discard))
	e.SetHomeState(true, map[int]float64{0: 55, 1: 20})
	for i, report := range []struct {
		zone int
		tlvs []TLV
	}{
		{1, []TLV{{Tag: "6280", Val: []byte{0x08, 0x98}}, {Tag: "6240", Val: []byte{0x02}}, {Tag: "6260", Val: []byte{0x01}}, {Tag: "62e0", Val: []byte{0x00}}}},
		{0, []TLV{{Tag: "6280", Val: []byte{0x13, 0x88}}, {Tag: "6240", Val: []byte{0x00}}, {Tag: "6260", Val: []byte{0x01}}, {Tag: "62e0", Val: []byte{0x00}}}},
	} {
		e.OnFrame(synthZoneUplink(t, in, mustHex(ru), CoAPReq{
			Type: "CON", Code: "0.03", MID: 0x7601 + uint16(i), Token: []byte{0x23},
			Path: []string{"z", "s"}, Query: []string{"lid=" + strconv.Itoa(report.zone)}, Payload: encodeTLVs(report.tlvs),
		}))
	}
	delete(e.queue, ru)
	if err := e.EnqueueZoneRewrite(ru, 1); err != nil {
		t.Fatal(err)
	}
	if got := queuedTags(t, e, ru); len(got) != 1 || got[0]["6240"] != "02" || got[0]["6280"] != "0898" {
		t.Errorf("room rewrite = %v, want the 22 C overlay of zone 1", got)
	}
}

// TestOverlayNameInLogs: the decoder names every captured 6240 value, 1
// included, which only the app rule "until the next automatic change" writes.
func TestOverlayNameInLogs(t *testing.T) {
	for v, want := range map[byte]string{
		0: "overlay=auto", 1: "overlay=until_auto_change", 2: "overlay=manual", 3: "overlay=until_next", 9: "overlay=9",
	} {
		if got := (TLV{Tag: "6240", Val: []byte{v}}).TagLabel(); got != want {
			t.Errorf("6240=%d decodes as %q, want %q", v, got, want)
		}
	}
}

// TestAutoChangeOverlayTimer: an overlay set with OverlayEndUntilAutoChange is
// the 6240=1 overlay, and its timer has no deadline on a zone with no program.
// No clock ends it, a store keeps it, and the switch between home and away
// makes every such timer due, a deadline too, so ExpireBoosts queues the return
// to schedule of an elapsed timer. A timer of another rule with no deadline is
// refused at restore.
func TestAutoChangeOverlayTimer(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	cfg := fixtureInstall(t).EmuConfig()
	cfg.Now = func() time.Time { return now }
	e := NewEmu(cfg, nil)
	if err := e.EnqueueSetpoint("bagno", SetpointParams{ID: 2, Setpoint: 21, Sched: 20, Overlay: 1, EndRule: OverlayEndUntilAutoChange}); err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueDHWOverlay("RU1000000001", 0, 50, true, 0, OverlayEndUntilAutoChange); err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueSetpoint("camera", SetpointParams{ID: 10, Setpoint: 21, Sched: 20, Overlay: 1, Duration: time.Hour, EndRule: OverlayEndUntilAutoChange}); err != nil {
		t.Fatal(err)
	}
	wire := func(ref string) string {
		t.Helper()
		w, _, err := e.resolve(ref)
		if err != nil {
			t.Fatal(err)
		}
		return hexEUI(w)
	}
	if b := e.queue[wire("bagno")][0].heating; b == nil || b.overlay != 1 {
		t.Errorf("room write = %+v, want 6240=1", b)
	}
	if b := e.queue[wire("RU1000000001")][0].dhw; b == nil || b.overlay != 1 {
		t.Errorf("hot water write = %+v, want 6240=1", b)
	}
	for _, ref := range []string{"RU1000000001", "bagno", "camera"} {
		takeRestores(t, e, wire(ref))
	}
	for _, zone := range []int{0, 2} {
		if timer, ok := e.ZoneOverlayTimer(zone); !ok || !timer.ExpiresAt.IsZero() || timer.Rule != OverlayEndUntilAutoChange {
			t.Errorf("zone %d timer = %+v ok=%t, want the auto change rule with no deadline", zone, timer, ok)
		}
	}
	if timer, ok := e.ZoneOverlayTimer(10); !ok || !timer.ExpiresAt.Equal(now.Add(time.Hour)) || timer.Rule != OverlayEndUntilAutoChange {
		t.Errorf("zone 10 timer = %+v ok=%t, want the auto change rule ending in an hour", timer, ok)
	}
	if due, err := e.ExpireBoosts(now.Add(59 * time.Minute)); err != nil || len(due) != 0 {
		t.Fatalf("before the deadline due=%v err=%v, want none", due, err)
	}

	restored := NewEmu(cfg, nil)
	if err := restored.RestoreDesiredState(e.ExportDesiredState()); err != nil {
		t.Fatal(err)
	}
	for _, zone := range []int{0, 2, 10} {
		timer, ok := restored.ZoneOverlayTimer(zone)
		if !ok || timer.Rule != OverlayEndUntilAutoChange || timer.ExpiresAt.IsZero() != (zone != 10) {
			t.Errorf("restored zone %d timer = %+v ok=%t, want the auto change rule", zone, timer, ok)
		}
	}
	if due, err := restored.ExpireBoosts(now.Add(365 * 24 * time.Hour)); err != nil || len(due) != 1 {
		t.Fatalf("a year later due=%v err=%v, want the timer of zone 10 alone", due, err)
	}

	restored = NewEmu(cfg, nil)
	if err := restored.RestoreDesiredState(e.ExportDesiredState()); err != nil {
		t.Fatal(err)
	}
	if zones := restored.EndHomeChangeOverlays(now); !reflect.DeepEqual(zones, []int{0, 2, 10}) {
		t.Fatalf("overlays ended = %v, want zones 0, 2 and 10", zones)
	}
	if timer, _ := restored.ZoneOverlayTimer(10); !timer.ExpiresAt.Equal(now) {
		t.Errorf("zone 10 deadline = %v, want the time of the switch", timer.ExpiresAt)
	}
	due, err := restored.ExpireBoosts(now)
	if err != nil || len(due) != 3 {
		t.Fatalf("after the switch due=%v err=%v, want the thermostat and the two valves", due, err)
	}
	for ref, want := range map[string]string{"RU1000000001": "dhw schedule id=0", "bagno": "boost id=2 on=false", "camera": "boost id=10 on=false"} {
		w, _, err := restored.resolve(ref)
		if err != nil {
			t.Fatal(err)
		}
		queue := restored.queue[hexEUI(w)]
		if len(queue) == 0 {
			t.Errorf("%s holds no write after the switch", ref)
			continue
		}
		if got := queue[len(queue)-1].desc; got != want {
			t.Errorf("%s write = %q, want %q", ref, got, want)
		}
	}

	bad := e.ExportDesiredState()
	for key, timer := range bad.Boost {
		if timer.ID == 2 {
			timer.Rule = OverlayEndTimer
			bad.Boost[key] = timer
		}
	}
	if err := NewEmu(cfg, nil).RestoreDesiredState(bad); err == nil {
		t.Error("a timer with no deadline and the timer rule was restored")
	}
}
