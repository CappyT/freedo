package daemon

// home_change_overlay_test: the end rule until_home_change. The overlay has no
// deadline and ends at the next switch between home and away, in either
// direction, with the write an elapsed timer gets. Frames and clocks are
// synthetic.

import (
	"net/http"
	"testing"
	"time"

	"freedo/tado"
)

// setHome switches the home and returns how many switch writes it queued.
func setHome(t *testing.T, url string, away bool) int {
	t.Helper()
	var res struct {
		Queued int `json:"queued"`
	}
	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": away}, &res); code != http.StatusOK {
		t.Fatalf("home away=%t: code=%d", away, code)
	}
	return res.Queued
}

// zoneTimer returns the overlay timer of zone as the engine holds it.
func zoneTimer(t *testing.T, d *daemon, zone int) (tado.OverlayTimer, bool) {
	t.Helper()
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		timer, ok := e.ZoneOverlayTimer(zone)
		return []any{timer, ok}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	pair := v.([]any)
	return pair[0].(tado.OverlayTimer), pair[1].(bool)
}

// TestHomeChangeOverlayEndsAtEachSwitch: a room overlay set with the rule
// writes a manual overlay, no clock ends it, and the switch to away and the
// switch back each end it with the return to schedule of an elapsed timer: the
// program setpoint, no 6440, and the new home state in 6160. The zone then
// follows the away state or the program. The zone takes that write in place of
// the switch write, and the other zones take theirs.
func TestHomeChangeOverlayEndsAtEachSwitch(t *testing.T) {
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	writer := wireOf(t, in, "VA1000000002")
	seedBlock(t, d, in, "VA1000000002", 3)

	if body, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 19, "mode": "until_home_change"}); code != http.StatusOK {
		t.Fatalf("overlay: code=%d body=%v", code, body)
	}
	wantTags(t, "the overlay", deliver(t, d, in, writer),
		map[string]string{"6240": "02", "6260": "01", "6280": "076c", "6160": "01"})
	target := mustTarget(t, url, 3)
	overlay := overlayOfTarget(t, target)
	if target["source"] != "overlay" || target["targetC"] != 19.0 || overlay["kind"] != "manual" || overlay["endsAt"] != nil {
		t.Errorf("overlay: target = %v, want a manual overlay at 19 with no end", target)
	}

	clock.advance(30 * 24 * time.Hour)
	expireOverlays(t, d)
	wantNoWrites(t, "a month with no switch", d, writer)

	zones := len(in.Zones)
	if queued := setHome(t, url, true); queued != zones-1 {
		t.Errorf("away queued %d switch writes, want %d: the zone with the overlay takes the end instead", queued, zones-1)
	}
	if n := pending(t, d, writer); n != 1 {
		t.Fatalf("the writer holds %d writes after the switch to away, want the end alone", n)
	}
	wantExactTags(t, "the end at away", deliver(t, d, in, writer), map[string]string{
		"6020": "01", "6160": "02", "6180": "00", "61e0": "00", "6240": "00", "62e0": "00",
	})
	target = mustTarget(t, url, 3)
	if target["source"] != "away" || target["overlay"] != nil {
		t.Errorf("after the end at away: target = %v, want the away state", target)
	}
	if _, ok := zoneTimer(t, d, 3); ok {
		t.Error("the timer survived the delivered end")
	}

	// Away, a new overlay with the rule ends at the return home.
	if _, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 21, "mode": "until_home_change"}); code != http.StatusOK {
		t.Fatalf("overlay away: code=%d", code)
	}
	deliver(t, d, in, writer)
	if queued := setHome(t, url, false); queued != zones-1 {
		t.Errorf("home queued %d switch writes, want %d", queued, zones-1)
	}
	wantTags(t, "the end at home", deliver(t, d, in, writer),
		map[string]string{"6160": "01", "6240": "00"}, "6260", "6280", "6440")
	if target := mustTarget(t, url, 3); target["source"] != "program" || target["overlay"] != nil {
		t.Errorf("after the end at home: target = %v, want the program", target)
	}
}

// TestHomeChangeOverlayLeavesOtherRules: a switch ends only the overlays with
// the rule. A manual overlay and a timer overlay keep running and take the
// switch write, which restates the overlay.
func TestHomeChangeOverlayLeavesOtherRules(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	ufficio, camera, bagno := wireOf(t, in, "VA1000000002"), wireOf(t, in, "VA1000000005"), wireOf(t, in, "VA1000000007")
	for zone, body := range map[int]map[string]any{
		3:  {"setpointC": 19, "mode": "until_home_change"},
		10: {"setpointC": 20, "mode": "manual"},
		2:  {"setpointC": 21, "mode": "timer", "durationSec": 3600},
	} {
		if _, code := postZone(t, url, zone, "setpoint", body); code != http.StatusOK {
			t.Fatalf("zone %d overlay: code=%d", zone, code)
		}
	}
	for _, target := range []string{ufficio, camera, bagno} {
		deliver(t, d, in, target)
	}
	setHome(t, url, true)
	wantTags(t, "the end", deliver(t, d, in, ufficio), map[string]string{"6240": "00"})
	wantTags(t, "the manual overlay", deliver(t, d, in, camera), map[string]string{"6240": "02", "6160": "02"})
	wantTags(t, "the timer overlay", deliver(t, d, in, bagno), map[string]string{"6240": "03", "6160": "02"})
}

// TestHomeChangeOverlayHotWater: the hot water zone takes the rule too, with no
// program. The switch to away ends the overlay with the hot-water return to
// schedule, and the zone then follows the away state.
func TestHomeChangeOverlayHotWater(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	thermostat := wireOf(t, in, "RU1000000001")
	if body, code := postZone(t, url, 0, "setpoint", map[string]any{"setpointC": 50, "mode": "until_home_change"}); code != http.StatusOK {
		t.Fatalf("hot water overlay: code=%d body=%v", code, body)
	}
	wantTags(t, "hot water on", deliver(t, d, in, thermostat),
		map[string]string{"6020": "02", "6240": "02", "6260": "01", "6280": "1388"})
	if overlay := overlayOfTarget(t, mustTarget(t, url, 0)); overlay["endsAt"] != nil {
		t.Errorf("hot water overlay ends at %v, want no deadline", overlay["endsAt"])
	}
	setHome(t, url, true)
	wantTags(t, "the hot water end", nextZoneWrite(t, d, thermostat),
		map[string]string{"6020": "02", "6240": "00", "6160": "02"}, "6260", "6280")
	deliverQueue(t, d, in, thermostat)
	if target := mustTarget(t, url, 0); target["source"] != "away" || target["on"] != false || target["overlay"] != nil {
		t.Errorf("hot water after the end: target = %v, want off in away", target)
	}
}

// TestHomeChangeOverlayWaitsForAQueuedWrite: a write still queued to the
// writer holds the end back, as it holds an elapsed timer back. The timer stays
// due, and the next tick queues the end once the node took that write.
func TestHomeChangeOverlayWaitsForAQueuedWrite(t *testing.T) {
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	writer := wireOf(t, in, "VA1000000002")
	if _, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 19, "mode": "until_home_change"}); code != http.StatusOK {
		t.Fatalf("overlay: code=%d", code)
	}
	setHome(t, url, true)
	if n := pending(t, d, writer); n != 1 {
		t.Fatalf("the writer holds %d writes, want the overlay alone", n)
	}
	deliver(t, d, in, writer)
	expireOverlays(t, d)
	wantTags(t, "the end after the overlay", deliver(t, d, in, writer), map[string]string{"6240": "00", "6160": "02"})
}

// TestHomeChangeOverlaySurvivesRestart: the rule and its missing deadline come
// back from the database, and the switch after the restart ends the overlay.
func TestHomeChangeOverlaySurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, store)
	writer := wireOf(t, in, "VA1000000002")
	if _, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 19, "mode": "until_home_change"}); code != http.StatusOK {
		t.Fatalf("overlay: code=%d", code)
	}
	deliver(t, d, in, writer)
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
	clock.advance(48 * time.Hour)
	d, url = newWindowDaemon(t, in, clock, reopened)
	timer, ok := zoneTimer(t, d, 3)
	if !ok || timer.Rule != tado.OverlayEndHomeChange || !timer.ExpiresAt.IsZero() {
		t.Fatalf("restored timer = %+v ok=%t, want the home change rule with no deadline", timer, ok)
	}
	if target := mustTarget(t, url, 3); target["source"] != "overlay" || target["targetC"] != 19.0 {
		t.Errorf("restored target = %v, want the overlay at 19", target)
	}
	expireOverlays(t, d)
	wantNoWrites(t, "two days after the restart", d, writer)

	setHome(t, url, true)
	wantTags(t, "the end after the restart", deliver(t, d, in, writer), map[string]string{"6240": "00", "6160": "02"})
}

// TestHomeChangeOverlayKeepsRuleOnRestate: a rewrite of the running overlay, as
// a correction change does, keeps the rule.
func TestHomeChangeOverlayKeepsRuleOnRestate(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	writer := wireOf(t, in, "VA1000000002")
	seedBlock(t, d, in, "VA1000000002", 3)
	if _, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 19, "mode": "until_home_change"}); code != http.StatusOK {
		t.Fatalf("overlay: code=%d", code)
	}
	deliver(t, d, in, writer)
	if _, err := d.do(func(e *tado.EmuState) (any, error) {
		return nil, d.restateZone(e, in, 3, writer, 0.5)
	}); err != nil {
		t.Fatal(err)
	}
	wantTags(t, "the restate", deliver(t, d, in, writer), map[string]string{"6240": "02", "6260": "01"})
	if timer, ok := zoneTimer(t, d, 3); !ok || timer.Rule != tado.OverlayEndHomeChange || !timer.ExpiresAt.IsZero() {
		t.Errorf("timer after the restate = %+v ok=%t, want the home change rule with no deadline", timer, ok)
	}
}

// TestHomeChangeOverlayRefusals: the rule is refused where the other rules
// are: with durationSec, and as the end rule of a device overlay.
func TestHomeChangeOverlayRefusals(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	for path, body := range map[string]map[string]any{
		"3/setpoint": {"setpointC": 20, "mode": "until_home_change", "durationSec": 60},
		"0/setpoint": {"setpointC": 50, "mode": "until_home_change", "durationSec": 60},
		"3/off":      {"mode": "until_home_change", "durationSec": 60},
		"0/off":      {"mode": "until_home_change", "durationSec": 60},
	} {
		var res map[string]string
		if code := postJSON(t, http.MethodPost, url+"/api/zones/"+path, body, &res); code != http.StatusBadRequest || res["error"] != "durationSec needs mode timer" {
			t.Errorf("%s with durationSec: code=%d error=%q, want 400 durationSec needs mode timer", path, code, res["error"])
		}
	}
	var res map[string]string
	if code := postJSON(t, http.MethodPost, url+"/api/zones/3/setpoint", map[string]any{"setpointC": 20, "mode": "until_away"}, &res); code != http.StatusBadRequest ||
		res["error"] != "bad mode until_away: want manual, until_next, timer or until_home_change" {
		t.Errorf("unknown mode: code=%d error=%q", code, res["error"])
	}
	if msg := (tado.OverlayEnd{Mode: tado.OverlayEndHomeChange}).Check(); msg == "" {
		t.Error("a device overlay takes the rule")
	}
	for _, ref := range []string{"VA1000000002", "RU1000000001"} {
		wantNoWrites(t, "a refused request", d, wireOf(t, in, ref))
	}
}
