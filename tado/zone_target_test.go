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
