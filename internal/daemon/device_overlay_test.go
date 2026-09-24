package daemon

// device_overlay_test: a manual change made on a device and the end rule the
// coordinator gives it. Frames and clocks are synthetic.

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"freedo/tado"
)

// overlayFromDevice sends the device overlay PUT of ref for zone in the
// captured shape: 6280 holds the new setpoint, 6240=2 the manual overlay.
func overlayFromDevice(t *testing.T, d *daemon, in *tado.Install, ref string, zone int, centiC uint16, mid uint16) {
	t.Helper()
	var body []byte
	for _, tlv := range []tado.TLV{
		{Tag: "6280", Val: []byte{byte(centiC >> 8), byte(centiC)}},
		{Tag: "6240", Val: []byte{0x02}}, {Tag: "6260", Val: []byte{0x01}},
	} {
		body = append(body, tlv.Encode()...)
	}
	feed(t, d, in, ref, false, tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: mid,
		Path:    []string{"h", "7654321", "z", strconv.Itoa(zone), "overlay"},
		Options: []tado.CoAPOption{{Num: 12, Val: []byte{42}}},
		Payload: body,
	})
}

// deviceOverlayDaemon starts the test daemon with end rule on zone 5, puts a
// program on zone 5 (19.5 C, 21 C from 06:00) and seeds both members.
func deviceOverlayDaemon(t *testing.T, end *tado.OverlayEnd, clock *testClock, store *stateStore) (*daemon, string, *tado.Install) {
	t.Helper()
	in := windowInstall(t, func(in *tado.Install) {
		if end != nil {
			in.Options.DeviceOverlayEnds = map[int]tado.OverlayEnd{5: *end}
		}
	})
	d, url := newWindowDaemon(t, in, clock, store)
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/5/schedule", allDayProgram(
		map[string]any{"minute": 0, "setpoint_c": 19.5},
		map[string]any{"minute": 360, "setpoint_c": 21.0},
	), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	seedBlock(t, d, in, "VA1000000006", 5)
	seedBlock(t, d, in, "VA1000000001", 5)
	return d, url, in
}

func expireOverlays(t *testing.T, d *daemon) {
	t.Helper()
	if _, err := d.do(func(e *tado.EmuState) (any, error) { return e.ExpireBoosts(d.retryNow()) }); err != nil {
		t.Fatal(err)
	}
}

func wantDeviceOverlay(t *testing.T, what, url string, setpointC float64, endsAt any) {
	t.Helper()
	target := mustTarget(t, url, 5)
	overlay := overlayOfTarget(t, target)
	if target["source"] != "overlay" || target["targetC"] != setpointC || overlay["kind"] != "manual" || overlay["endsAt"] != endsAt {
		t.Errorf("%s: target = %v, want a manual overlay at %v ending at %v", what, target, setpointC, endsAt)
	}
}

// TestDeviceOverlayTimerExpires: a timer rule ends the device overlay after its
// duration with a write to the zone writer alone: overlay auto, the program
// setpoint, no 6440. The overlay itself writes nothing.
func TestDeviceOverlayTimerExpires(t *testing.T) {
	clock := newTestClock(windowStart)
	d, url, in := deviceOverlayDaemon(t, &tado.OverlayEnd{Mode: tado.OverlayEndTimer, DurationSec: 1800}, clock, nil)
	writer, other := wireOf(t, in, "VA1000000006"), wireOf(t, in, "VA1000000001")

	overlayFromDevice(t, d, in, "VA1000000001", 5, 2200, 0x6101)
	wantNoWrites(t, "the device overlay", d, writer, other)
	wantDeviceOverlay(t, "after the overlay", url, 22, windowStart.Add(30*time.Minute).Format(time.RFC3339))

	clock.advance(30*time.Minute - time.Second)
	expireOverlays(t, d)
	wantNoWrites(t, "one second before the end", d, writer, other)

	clock.advance(time.Second)
	expireOverlays(t, d)
	wantNoWrites(t, "the end on the other member", d, other)
	wantExactTags(t, "the end", deliver(t, d, in, writer), map[string]string{
		"6160": "01", "6180": "00", "6020": "01", "61e0": "01", "6200": "079e", "6240": "00", "62e0": "00",
	})
}

// TestDeviceOverlayEndedOnDevice: the elected reporter returns the zone to its
// schedule on the device and sends DELETE z/N/overlay. The zone follows its
// program at once, the deadline of the device overlay ends, and its expiry
// writes nothing.
func TestDeviceOverlayEndedOnDevice(t *testing.T) {
	clock := newTestClock(windowStart)
	d, url, in := deviceOverlayDaemon(t, &tado.OverlayEnd{Mode: tado.OverlayEndTimer, DurationSec: 1800}, clock, nil)
	writer, other := wireOf(t, in, "VA1000000006"), wireOf(t, in, "VA1000000001")
	overlayFromDevice(t, d, in, "VA1000000006", 5, 2200, 0x6111)
	wantDeviceOverlay(t, "after the overlay", url, 22, windowStart.Add(30*time.Minute).Format(time.RFC3339))

	clock.advance(time.Minute)
	feed(t, d, in, "VA1000000006", false, tado.CoAPReq{
		Type: "CON", Code: "0.04", MID: 0x6112,
		Path: []string{"h", "7654321", "z", "5", "overlay"},
	})
	if target := mustTarget(t, url, 5); target["source"] != "program" || target["overlay"] != nil {
		t.Errorf("target after the end = %v, want the program with no overlay", target)
	}
	if v, err := d.do(func(e *tado.EmuState) (any, error) {
		_, timed := e.ZoneOverlayTimer(5)
		return timed, nil
	}); err != nil || v.(bool) {
		t.Errorf("the overlay deadline outlives the end on the device (err=%v)", err)
	}
	clock.advance(30 * time.Minute)
	expireOverlays(t, d)
	wantNoWrites(t, "the old deadline", d, writer, other)
}

// TestDeviceOverlayUntilNextIsDefault: a zone with no rule ends the device
// overlay at the start of the next program block.
func TestDeviceOverlayUntilNextIsDefault(t *testing.T) {
	clock := newTestClock(windowStart)
	d, url, in := deviceOverlayDaemon(t, nil, clock, nil)
	overlayFromDevice(t, d, in, "VA1000000006", 5, 2250, 0x6201)
	wantDeviceOverlay(t, "until the next block", url, 22.5, windowStart.Add(time.Hour).Format(time.RFC3339))
}

// TestDeviceOverlayManualNeverExpires: a manual rule keeps the device overlay
// until a cancel, and drops the deadline of the overlay it replaces.
func TestDeviceOverlayManualNeverExpires(t *testing.T) {
	clock := newTestClock(windowStart)
	d, url, in := deviceOverlayDaemon(t, &tado.OverlayEnd{Mode: tado.OverlayEndManual}, clock, nil)
	writer, other := wireOf(t, in, "VA1000000006"), wireOf(t, in, "VA1000000001")
	if _, code := postZone(t, url, 5, "setpoint", map[string]any{
		"setpointC": 19, "mode": "timer", "durationSec": 900,
	}); code != http.StatusOK {
		t.Fatalf("timer: code=%d", code)
	}
	deliver(t, d, in, writer)

	overlayFromDevice(t, d, in, "VA1000000001", 5, 2100, 0x6301)
	wantDeviceOverlay(t, "a manual rule", url, 21.0, nil)
	clock.advance(8 * 24 * time.Hour)
	expireOverlays(t, d)
	wantNoWrites(t, "eight days later", d, writer, other)
}

// TestDeviceOverlayCancelCarries6440: the return to schedule the user asks for
// over a device overlay still carries 6440 and drops the deadline.
func TestDeviceOverlayCancelCarries6440(t *testing.T) {
	clock := newTestClock(windowStart)
	d, url, in := deviceOverlayDaemon(t, &tado.OverlayEnd{Mode: tado.OverlayEndTimer, DurationSec: 1800}, clock, nil)
	writer, other := wireOf(t, in, "VA1000000006"), wireOf(t, in, "VA1000000001")
	overlayFromDevice(t, d, in, "VA1000000001", 5, 2200, 0x6401)

	if _, code := postZone(t, url, 5, "auto", nil); code != http.StatusOK {
		t.Fatalf("auto: code=%d", code)
	}
	wantNoWrites(t, "the cancel on the other member", d, other)
	wantTags(t, "the cancel", deliver(t, d, in, writer), map[string]string{"6240": "00", "6440": "0001"}, "6280")
	clock.advance(time.Hour)
	expireOverlays(t, d)
	wantNoWrites(t, "after the cancel", d, writer, other)
}

// TestDeviceOverlaySurvivesRestart: the deadline of a device overlay is part of
// the stored overlay timers, so it ends on time after a restart.
func TestDeviceOverlaySurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	clock := newTestClock(windowStart)
	end := &tado.OverlayEnd{Mode: tado.OverlayEndTimer, DurationSec: 1800}
	d, _, in := deviceOverlayDaemon(t, end, clock, store)
	overlayFromDevice(t, d, in, "VA1000000001", 5, 2200, 0x6501)
	if err := d.saveDesiredState(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	clock.advance(10 * time.Minute)
	d, url := newWindowDaemon(t, in, clock, reopened)
	wantDeviceOverlay(t, "after the restart", url, 22, windowStart.Add(30*time.Minute).Format(time.RFC3339))

	writer := wireOf(t, in, "VA1000000006")
	dropQueue(t, d, writer)
	clock.advance(20 * time.Minute)
	expireOverlays(t, d)
	wantTags(t, "the end after the restart", deliver(t, d, in, writer), map[string]string{"6240": "00"}, "6440")
}

// TestInstallAPIPatchesDeviceOverlayEnd: a heating zone starts on the default
// rule, the zone patch writes and stores another one, a bad rule is refused and
// the default drops the stored row.
func TestInstallAPIPatchesDeviceOverlayEnd(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	zone := a.mutate(http.MethodPost, "/api/install/zones",
		map[string]any{"number": 1, "name": "sala", "kind": "heating"})["zone"].(map[string]any)
	if zone["device_overlay_end"] != "until_next" || zone["device_overlay_end_sec"] != nil {
		t.Fatalf("new zone = %v, want until_next with no duration", zone)
	}

	patched := a.mutate(http.MethodPatch, "/api/install/zones/1", map[string]any{
		"device_overlay_end": "timer", "device_overlay_end_sec": 3600,
	})["zone"].(map[string]any)
	if patched["device_overlay_end"] != "timer" || patched["device_overlay_end_sec"] != 3600.0 {
		t.Fatalf("patched zone = %v, want a 3600 s timer", patched)
	}
	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Options.DeviceOverlayEnd(1); got != (tado.OverlayEnd{Mode: tado.OverlayEndTimer, DurationSec: 3600}) {
		t.Errorf("stored rule = %+v, want a 3600 s timer", got)
	}

	for _, body := range []map[string]any{
		{"device_overlay_end": "forever"},
		{"device_overlay_end": "manual", "device_overlay_end_sec": 60},
		{"device_overlay_end_sec": 0},
	} {
		if code, res := a.call(http.MethodPatch, "/api/install/zones/1", body); code != http.StatusBadRequest {
			t.Errorf("%v: got %d %v, want 400", body, code, res)
		}
	}

	back := a.mutate(http.MethodPatch, "/api/install/zones/1",
		map[string]any{"device_overlay_end": "until_next"})["zone"].(map[string]any)
	if back["device_overlay_end"] != "until_next" || back["device_overlay_end_sec"] != nil {
		t.Errorf("zone = %v, want until_next with no duration", back)
	}
	if stored, _, err = a.store.LoadInstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(stored.Options.DeviceOverlayEnds) != 0 {
		t.Errorf("stored rules = %v, want none for the default", stored.Options.DeviceOverlayEnds)
	}
}
