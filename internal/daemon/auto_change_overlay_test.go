package daemon

// auto_change_overlay_test: the end rule until_auto_change, the app's
// TADO_MODE, which the cloud writes as 6240=1 (capture-overlay-20261011). The
// overlay ends at the next block of the program or at the next switch between
// home and away, in either direction, whichever comes first, with the write an
// elapsed timer gets. With no program only the switch ends it. Frames and
// clocks are synthetic.

import (
	"net/http"
	"strconv"
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

// TestAutoChangeOverlayEndsAtEachSwitch: with no program a room overlay set
// with the rule writes 6240=1, no clock ends it, and the switch to away and the
// switch back each end it with the return to schedule of an elapsed timer: no
// 6440, and the new home state in 6160. The zone then follows the away state or
// the program. The zone takes that write in place of the switch write, and the
// other zones take theirs.
func TestAutoChangeOverlayEndsAtEachSwitch(t *testing.T) {
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	writer := wireOf(t, in, "VA1000000002")
	seedBlock(t, d, in, "VA1000000002", 3)

	if body, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 19, "mode": "until_auto_change"}); code != http.StatusOK {
		t.Fatalf("overlay: code=%d body=%v", code, body)
	}
	wantTags(t, "the overlay", deliver(t, d, in, writer),
		map[string]string{"6240": "01", "6260": "01", "6280": "076c", "6160": "01", "6440": "0001"})
	target := mustTarget(t, url, 3)
	overlay := overlayOfTarget(t, target)
	if target["source"] != "overlay" || target["targetC"] != 19.0 || overlay["kind"] != "until_auto_change" ||
		overlay["rule"] != "until_auto_change" || overlay["endsAt"] != nil {
		t.Errorf("overlay: target = %v, want an until_auto_change overlay at 19 with no end", target)
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
	if _, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 21, "mode": "until_auto_change"}); code != http.StatusOK {
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

// TestAutoChangeOverlayLeavesOtherRules: a switch ends only the overlays with
// the rule. A manual overlay and a timer overlay keep running and take the
// switch write, which restates the overlay.
func TestAutoChangeOverlayLeavesOtherRules(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	ufficio, camera, bagno := wireOf(t, in, "VA1000000002"), wireOf(t, in, "VA1000000005"), wireOf(t, in, "VA1000000007")
	for zone, body := range map[int]map[string]any{
		3:  {"setpointC": 19, "mode": "until_auto_change"},
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

// TestAutoChangeOverlayHotWater: the hot water zone takes the rule too, with no
// program. The switch to away ends the overlay with the hot-water return to
// schedule, and the zone then follows the away state.
func TestAutoChangeOverlayHotWater(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	thermostat := wireOf(t, in, "RU1000000001")
	if body, code := postZone(t, url, 0, "setpoint", map[string]any{"setpointC": 50, "mode": "until_auto_change"}); code != http.StatusOK {
		t.Fatalf("hot water overlay: code=%d body=%v", code, body)
	}
	wantTags(t, "hot water on", deliver(t, d, in, thermostat),
		map[string]string{"6020": "02", "6240": "01", "6260": "01", "6280": "1388"})
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

// TestAutoChangeOverlayWaitsForAQueuedWrite: a write still queued to the
// writer holds the end back, as it holds an elapsed timer back. The timer stays
// due, and the next tick queues the end once the node took that write.
func TestAutoChangeOverlayWaitsForAQueuedWrite(t *testing.T) {
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	writer := wireOf(t, in, "VA1000000002")
	if _, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 19, "mode": "until_auto_change"}); code != http.StatusOK {
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

// TestAutoChangeOverlaySurvivesRestart: the rule and its missing deadline come
// back from the database, and the switch after the restart ends the overlay.
func TestAutoChangeOverlaySurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, store)
	writer := wireOf(t, in, "VA1000000002")
	if _, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 19, "mode": "until_auto_change"}); code != http.StatusOK {
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
	if !ok || timer.Rule != tado.OverlayEndUntilAutoChange || !timer.ExpiresAt.IsZero() {
		t.Fatalf("restored timer = %+v ok=%t, want the auto change rule with no deadline", timer, ok)
	}
	if target := mustTarget(t, url, 3); target["source"] != "overlay" || target["targetC"] != 19.0 {
		t.Errorf("restored target = %v, want the overlay at 19", target)
	}
	expireOverlays(t, d)
	wantNoWrites(t, "two days after the restart", d, writer)

	setHome(t, url, true)
	wantTags(t, "the end after the restart", deliver(t, d, in, writer), map[string]string{"6240": "00", "6160": "02"})
}

// TestAutoChangeOverlayKeepsRuleOnRestate: a rewrite of the running overlay, as
// a correction change does, keeps the rule.
func TestAutoChangeOverlayKeepsRuleOnRestate(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	writer := wireOf(t, in, "VA1000000002")
	seedBlock(t, d, in, "VA1000000002", 3)
	if _, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 19, "mode": "until_auto_change"}); code != http.StatusOK {
		t.Fatalf("overlay: code=%d", code)
	}
	deliver(t, d, in, writer)
	if _, err := d.do(func(e *tado.EmuState) (any, error) {
		return nil, d.restateZone(e, in, 3, writer, 0.5)
	}); err != nil {
		t.Fatal(err)
	}
	wantTags(t, "the restate", deliver(t, d, in, writer), map[string]string{"6240": "01", "6260": "01"})
	if timer, ok := zoneTimer(t, d, 3); !ok || timer.Rule != tado.OverlayEndUntilAutoChange || !timer.ExpiresAt.IsZero() {
		t.Errorf("timer after the restate = %+v ok=%t, want the auto change rule with no deadline", timer, ok)
	}
}

// TestAutoChangeOverlayRefusals: the rule is refused where the other rules
// are: with durationSec, and as the end rule of a device overlay.
func TestAutoChangeOverlayRefusals(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	for path, body := range map[string]map[string]any{
		"3/setpoint": {"setpointC": 20, "mode": "until_auto_change", "durationSec": 60},
		"0/setpoint": {"setpointC": 50, "mode": "until_auto_change", "durationSec": 60},
		"3/off":      {"mode": "until_auto_change", "durationSec": 60},
		"0/off":      {"mode": "until_auto_change", "durationSec": 60},
	} {
		var res map[string]string
		if code := postJSON(t, http.MethodPost, url+"/api/zones/"+path, body, &res); code != http.StatusBadRequest || res["error"] != "durationSec needs mode timer" {
			t.Errorf("%s with durationSec: code=%d error=%q, want 400 durationSec needs mode timer", path, code, res["error"])
		}
	}
	var res map[string]string
	if code := postJSON(t, http.MethodPost, url+"/api/zones/3/setpoint", map[string]any{"setpointC": 20, "mode": "until_away"}, &res); code != http.StatusBadRequest ||
		res["error"] != "bad mode until_away: want manual, until_next, timer or until_auto_change" {
		t.Errorf("unknown mode: code=%d error=%q", code, res["error"])
	}
	if msg := (tado.OverlayEnd{Mode: tado.OverlayEndUntilAutoChange}).Check(); msg == "" {
		t.Error("a device overlay takes the rule")
	}
	for _, ref := range []string{"VA1000000002", "RU1000000001"} {
		wantNoWrites(t, "a refused request", d, wireOf(t, in, ref))
	}
}

// putProgram3 gives zone 3 a program of 19.5 C, and 21 C from 06:00, and drops
// the writes that program queued to its writer.
func putProgram3(t *testing.T, d *daemon, url, writer string) {
	t.Helper()
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule", allDayProgram(
		map[string]any{"minute": 0, "setpoint_c": 19.5},
		map[string]any{"minute": 360, "setpoint_c": 21.0},
	), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	dropQueue(t, d, writer)
}

// TestAutoChangeOverlayEndsAtTheNextBlock: with a program the overlay ends at
// the next block or at the next switch between home and away, whichever comes
// first. The first overlay (05:00) meets the 06:00 block, the second (06:00)
// meets the switch to away at 07:00, and the block of the next day then ends
// nothing.
func TestAutoChangeOverlayEndsAtTheNextBlock(t *testing.T) {
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	writer := wireOf(t, in, "VA1000000002")
	putProgram3(t, d, url, writer)

	if body, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 19, "mode": "until_auto_change"}); code != http.StatusOK {
		t.Fatalf("overlay: code=%d body=%v", code, body)
	}
	wantTags(t, "the overlay", deliver(t, d, in, writer),
		map[string]string{"6240": "01", "6260": "01", "6280": "076c", "6200": "079e"})
	block := windowStart.Add(time.Hour).Format(time.RFC3339)
	wantOverlayRule(t, "before the block", url, 3, "until_auto_change", "until_auto_change", block)

	clock.advance(time.Hour - time.Second)
	expireOverlays(t, d)
	wantNoWrites(t, "a second before the block", d, writer)
	clock.advance(time.Second)
	expireOverlays(t, d)
	wantTags(t, "the end at the block", deliver(t, d, in, writer),
		map[string]string{"6240": "00", "6160": "01"}, "6260", "6280", "6440")
	if target := mustTarget(t, url, 3); target["source"] != "program" || target["overlay"] != nil {
		t.Errorf("after the block: target = %v, want the program", target)
	}
	if _, ok := zoneTimer(t, d, 3); ok {
		t.Error("the timer survived the delivered end")
	}

	if _, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 19, "mode": "until_auto_change"}); code != http.StatusOK {
		t.Fatalf("second overlay: code=%d", code)
	}
	deliver(t, d, in, writer)
	wantOverlayRule(t, "the second overlay", url, 3, "until_auto_change", "until_auto_change", "2026-01-06T00:00:00Z")
	clock.advance(time.Hour)
	setHome(t, url, true)
	wantTags(t, "the end at the switch", deliver(t, d, in, writer),
		map[string]string{"6240": "00", "6160": "02"}, "6260", "6280", "6440")
	if _, ok := zoneTimer(t, d, 3); ok {
		t.Error("the timer survived the end at the switch")
	}
	clock.advance(24 * time.Hour)
	expireOverlays(t, d)
	wantNoWrites(t, "the next block after the switch", d, writer)
}

// TestOverlayWireValueOfEachRule: the 6240 of an overlay write follows the
// rule as the cloud writes it: manual 2, a timer 3, until_next 3 and
// until_auto_change 1. The hot water zone writes the same values.
func TestOverlayWireValueOfEachRule(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	writer, thermostat := wireOf(t, in, "VA1000000002"), wireOf(t, in, "RU1000000001")
	putProgram3(t, d, url, writer)
	for _, tc := range []struct {
		zone int
		node string
		body map[string]any
		want string
	}{
		{3, writer, map[string]any{"setpointC": 19, "mode": "manual"}, "02"},
		{3, writer, map[string]any{"setpointC": 19, "mode": "timer", "durationSec": 600}, "03"},
		{3, writer, map[string]any{"setpointC": 19, "mode": "until_next"}, "03"},
		{3, writer, map[string]any{"setpointC": 19, "mode": "until_auto_change"}, "01"},
		{0, thermostat, map[string]any{"setpointC": 50, "mode": "manual"}, "02"},
		{0, thermostat, map[string]any{"setpointC": 50, "mode": "timer", "durationSec": 600}, "03"},
		{0, thermostat, map[string]any{"setpointC": 50, "mode": "until_auto_change"}, "01"},
	} {
		if res, code := postZone(t, url, tc.zone, "setpoint", tc.body); code != http.StatusOK {
			t.Fatalf("zone %d %v: code=%d %v", tc.zone, tc.body, code, res)
		}
		wantTags(t, tc.body["mode"].(string)+" on zone "+strconv.Itoa(tc.zone), deliver(t, d, in, tc.node),
			map[string]string{"6240": tc.want, "6260": "01"})
	}
}

// TestAutoChangeOverlayDeadlineSurvivesRestart: the rule and the block deadline
// come back from the database, and the block after the restart ends the
// overlay.
func TestAutoChangeOverlayDeadlineSurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, store)
	writer := wireOf(t, in, "VA1000000002")
	putProgram3(t, d, url, writer)
	if _, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 19, "mode": "until_auto_change"}); code != http.StatusOK {
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
	clock.advance(30 * time.Minute)
	d, url = newWindowDaemon(t, in, clock, reopened)
	block := windowStart.Add(time.Hour)
	timer, ok := zoneTimer(t, d, 3)
	if !ok || timer.Rule != tado.OverlayEndUntilAutoChange || !timer.ExpiresAt.Equal(block) {
		t.Fatalf("restored timer = %+v ok=%t, want the auto change rule ending at %v", timer, ok, block)
	}
	wantOverlayRule(t, "after the restart", url, 3, "until_auto_change", "until_auto_change", block.Format(time.RFC3339))
	expireOverlays(t, d)
	wantNoWrites(t, "before the block", d, writer)
	clock.advance(30 * time.Minute)
	expireOverlays(t, d)
	wantTags(t, "the end at the block after the restart", deliver(t, d, in, writer),
		map[string]string{"6240": "00"}, "6260", "6280", "6440")
}
