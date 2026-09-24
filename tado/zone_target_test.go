package tado

import (
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"
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
			"BatteryMV", "BatteryPct", "CSLPeriod", "CSLPhase", "Display", "FallbackMode", "Fault", "FaultWord",
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
	if err := e.EnqueueDHWOverlay("RU1000000001", 0, 50, true, time.Hour); err != nil {
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

// takeRestores drains the queue of wire and returns "kind query" per write.
func takeRestores(t *testing.T, e *EmuState, wire string) []string {
	t.Helper()
	var out []string
	for _, d := range e.queue[wire] {
		msg, err := ParseCoAP(WrapAppPayload(d.coap, 1, nil)[8:])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d.kind+" "+msg.Query)
	}
	delete(e.queue, wire)
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
		if err := e.EnqueueDHWOverlay(ru, 0, 50, true, 4*time.Hour); err != nil {
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
		if err := e.EnqueueDHWOverlay(ru, 0, 50, true, 4*time.Hour); err != nil {
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
		if err := e.EnqueueDHWOverlay(ru, 0, 50, true, 4*time.Hour); err != nil {
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
		if err := e.EnqueueDHWOverlay(ru, 0, 50, true, 4*time.Hour); err != nil {
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
