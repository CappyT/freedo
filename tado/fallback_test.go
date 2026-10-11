package tado

import (
	"io"
	"reflect"
	"testing"
	"time"
)

// Fallback reports with synthetic bodies: the wall thermostat enters, changes
// and leaves a zone fallback, and reports its device-level mode.

func fallbackEmu(t *testing.T, now *time.Time) *EmuState {
	t.Helper()
	cfg := fixtureInstall(t).EmuConfig()
	cfg.Now = func() time.Time { return *now }
	return NewEmu(cfg, NewUnhandledLogger(io.Discard))
}

func zoneFallbackFrame(t *testing.T, src []byte, zone string, mode byte) []byte {
	t.Helper()
	return uplinkFrame(t, src, "0.03", []string{"h", "7654321", "z", zone, "fallback"}, nil,
		TLV{Tag: "6460", Val: []byte{mode}}.Encode())
}

func deviceFallbackFrame(t *testing.T, src []byte, serial string, mode byte) []byte {
	t.Helper()
	return uplinkFrame(t, src, "0.03", []string{"d", serial, "fallback"}, nil,
		TLV{Tag: "0182", Val: []byte{mode}}.Encode())
}

func TestZoneFallbackEntryAndExit(t *testing.T) {
	now := time.Date(2026, 1, 5, 6, 0, 0, 0, time.UTC)
	e := fallbackEmu(t, &now)
	thermostat := mustHex(capHall)

	e.OnFrame(zoneFallbackFrame(t, thermostat, "10", FallbackNone))
	if ev := e.TakeFallbackEvents(); len(ev) != 0 {
		t.Fatalf("a first report of mode 0 raised %v, want nothing", ev)
	}

	now = now.Add(time.Minute)
	e.OnFrame(zoneFallbackFrame(t, thermostat, "10", FallbackFixed))
	z := zoneOf(t, e, 10)
	if z.FallbackMode != FallbackFixed || z.Fallback != "fixed" || z.FallbackSinceUnix != now.Unix() {
		t.Fatalf("zone 10 after entry = %+v, want mode 2 since %d", z, now.Unix())
	}
	want := []FallbackEvent{{Zone: 10, Device: "a205", From: FallbackNone, To: FallbackFixed}}
	if ev := e.TakeFallbackEvents(); !reflect.DeepEqual(ev, want) {
		t.Fatalf("entry events = %v, want %v", ev, want)
	}

	entered := now
	now = now.Add(time.Minute)
	e.OnFrame(zoneFallbackFrame(t, thermostat, "10", FallbackFixed))
	if ev := e.TakeFallbackEvents(); len(ev) != 0 {
		t.Fatalf("a repeated mode raised %v", ev)
	}
	if z := zoneOf(t, e, 10); z.FallbackSinceUnix != entered.Unix() {
		t.Fatalf("a repeated mode moved since to %d, want %d", z.FallbackSinceUnix, entered.Unix())
	}

	e.OnFrame(zoneFallbackFrame(t, thermostat, "10", FallbackNone))
	want = []FallbackEvent{{Zone: 10, Device: "a205", From: FallbackFixed, To: FallbackNone}}
	if ev := e.TakeFallbackEvents(); !reflect.DeepEqual(ev, want) {
		t.Fatalf("exit events = %v, want %v", ev, want)
	}
	if z := zoneOf(t, e, 10); z.FallbackMode != FallbackNone || z.Fallback != "none" {
		t.Fatalf("zone 10 after exit = %+v, want mode 0", z)
	}
	if counts := e.log.Counts(); len(counts) != 0 {
		t.Errorf("the fallback reports logged unhandled traffic: %v", counts)
	}
}

// A zone the installation does not declare still shows its reported fallback.
func TestZoneFallbackUndeclaredZone(t *testing.T) {
	now := time.Date(2026, 1, 5, 6, 0, 0, 0, time.UTC)
	e := fallbackEmu(t, &now)
	e.OnFrame(zoneFallbackFrame(t, mustHex(capHall), "17", FallbackFrost))
	if z := zoneOf(t, e, 17); z.FallbackMode != FallbackFrost {
		t.Fatalf("zone 17 = %+v, want mode 3", z)
	}
	if got := e.ZoneFallbacks(); len(got) != 1 || got[0].Zone != 17 || got[0].Source != "a205" {
		t.Fatalf("ZoneFallbacks = %+v, want zone 17 from a205", got)
	}
}

func TestDeviceFallbackMode(t *testing.T) {
	now := time.Date(2026, 1, 5, 6, 0, 0, 0, time.UTC)
	e := fallbackEmu(t, &now)
	thermostat, valve := mustHex(capHall), mustHex(capBedroom)

	e.OnFrame(deviceFallbackFrame(t, valve, "VA1000000005", FallbackNone))
	e.OnFrame(deviceFallbackFrame(t, thermostat, "RU1000000001", FallbackUnset))
	if ev := e.TakeFallbackEvents(); len(ev) != 0 {
		t.Fatalf("mode 0 and unset raised %v, want nothing", ev)
	}

	e.OnFrame(deviceFallbackFrame(t, thermostat, "RU1000000001", FallbackHold))
	want := []FallbackEvent{{Zone: -1, Device: "a205", From: FallbackUnset, To: FallbackHold}}
	if ev := e.TakeFallbackEvents(); !reflect.DeepEqual(ev, want) {
		t.Fatalf("device entry events = %v, want %v", ev, want)
	}
	modes := map[string]int{}
	for _, v := range e.NodeViews() {
		modes[v.Short] = v.FallbackMode
	}
	if modes["a205"] != FallbackHold || modes["a602"] != FallbackNone {
		t.Fatalf("device modes = %v, want thermostat 1 and valve 0", modes)
	}
	if len(e.ZoneFallbacks()) != 0 {
		t.Fatal("a device-level report created a zone fallback")
	}
}

// A GET answer carries no path, so it never names a zone fallback.
func TestFallbackIgnoresGet(t *testing.T) {
	now := time.Date(2026, 1, 5, 6, 0, 0, 0, time.UTC)
	e := fallbackEmu(t, &now)
	e.OnFrame(uplinkFrame(t, mustHex(capHall), "0.01", []string{"h", "7654321", "z", "10", "fallback"}, nil,
		TLV{Tag: "6460", Val: []byte{FallbackFrost}}.Encode()))
	if got := e.ZoneFallbacks(); len(got) != 0 {
		t.Fatalf("a GET recorded %+v", got)
	}
}
