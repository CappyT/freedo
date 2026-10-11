package daemon

// overlay_rule_test: the zone target view reports the end rule an overlay was
// set with, in overlay.rule, next to the kind the 6240 gives and the deadline.
// Frames and clocks are synthetic.

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"freedo/tado"
)

// wantOverlayRule checks the kind, the rule and the deadline of the overlay of
// zone. endsAt is nil for an overlay with no deadline.
func wantOverlayRule(t *testing.T, what, url string, zone int, kind, rule string, endsAt any) {
	t.Helper()
	overlay := overlayOfTarget(t, mustTarget(t, url, zone))
	if overlay["kind"] != kind || overlay["rule"] != rule || overlay["endsAt"] != endsAt {
		t.Errorf("%s: overlay = %v, want kind %s, rule %s, ends %v", what, overlay, kind, rule, endsAt)
	}
}

// TestZoneOverlayRule: every rule the zone routes take comes back in
// overlay.rule. A timer and an until_next overlay share the kind until_next
// and the 6240, and the rule tells them apart. A new overlay replaces the rule,
// and the return to schedule removes the overlay with it.
func TestZoneOverlayRule(t *testing.T) {
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	writer := wireOf(t, in, "VA1000000002")
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule", allDayProgram(
		map[string]any{"minute": 0, "setpoint_c": 19.5},
		map[string]any{"minute": 360, "setpoint_c": 21.0},
	), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	dropQueue(t, d, writer)
	set := func(body map[string]any) {
		t.Helper()
		if res, code := postZone(t, url, 3, "setpoint", body); code != http.StatusOK {
			t.Fatalf("%v: code=%d %v", body, code, res)
		}
		deliver(t, d, in, writer)
	}

	set(map[string]any{"setpointC": 19, "mode": "timer", "durationSec": 1800})
	wantOverlayRule(t, "timer", url, 3, "until_next", "timer", windowStart.Add(30*time.Minute).Format(time.RFC3339))

	set(map[string]any{"setpointC": 19, "mode": "until_next"})
	wantOverlayRule(t, "until the next block", url, 3, "until_next", "until_next", windowStart.Add(time.Hour).Format(time.RFC3339))

	set(map[string]any{"setpointC": 19, "mode": "until_home_change"})
	wantOverlayRule(t, "until the home changes", url, 3, "manual", "until_home_change", nil)

	set(map[string]any{"setpointC": 19, "mode": "manual"})
	wantOverlayRule(t, "manual", url, 3, "manual", "manual", nil)

	if res, code := postZone(t, url, 3, "off", map[string]any{"mode": "timer", "durationSec": 600}); code != http.StatusOK {
		t.Fatalf("off: code=%d %v", code, res)
	}
	deliver(t, d, in, writer)
	wantOverlayRule(t, "off for a timer", url, 3, "until_next", "timer", windowStart.Add(10*time.Minute).Format(time.RFC3339))

	if _, code := postZone(t, url, 3, "auto", nil); code != http.StatusOK {
		t.Fatalf("auto: code=%d", code)
	}
	deliver(t, d, in, writer)
	if target := mustTarget(t, url, 3); target["overlay"] != nil {
		t.Errorf("after the return to schedule: target = %v, want no overlay", target)
	}

	// An until_next overlay with no program has no block to end at: the rule
	// stays and the deadline is null.
	if _, code := postZone(t, url, 10, "setpoint", map[string]any{"setpointC": 20, "mode": "until_next"}); code != http.StatusOK {
		t.Fatalf("until_next with no program: code=%d", code)
	}
	deliver(t, d, in, wireOf(t, in, "VA1000000005"))
	wantOverlayRule(t, "until_next with no program", url, 10, "until_next", "until_next", nil)
}

// TestZoneOverlayRuleHotWaterAndBoost: the hot water zone reports the rule of
// its overlay, and the boost is a timer.
func TestZoneOverlayRuleHotWaterAndBoost(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	thermostat := wireOf(t, in, "RU1000000001")
	if _, code := postZone(t, url, 0, "setpoint", map[string]any{"setpointC": 50, "mode": "timer", "durationSec": 3600}); code != http.StatusOK {
		t.Fatalf("hot water timer: code=%d", code)
	}
	deliver(t, d, in, thermostat)
	wantOverlayRule(t, "hot water timer", url, 0, "until_next", "timer", windowStart.Add(time.Hour).Format(time.RFC3339))
	if _, code := postZone(t, url, 0, "setpoint", map[string]any{"setpointC": 50}); code != http.StatusOK {
		t.Fatalf("hot water manual: code=%d", code)
	}
	deliver(t, d, in, thermostat)
	wantOverlayRule(t, "hot water manual", url, 0, "manual", "manual", nil)

	if _, code := postZone(t, url, 1, "boost", map[string]any{"on": true}); code != http.StatusOK {
		t.Fatalf("boost: code=%d", code)
	}
	deliver(t, d, in, thermostat)
	wantOverlayRule(t, "boost", url, 1, "boost", "timer", windowStart.Add(tado.BoostDuration).Format(time.RFC3339))
}

// TestDeviceOverlayRule: an overlay set on a device reports the end rule of its
// zone, not the manual overlay the device sent.
func TestDeviceOverlayRule(t *testing.T) {
	for name, tc := range map[string]struct {
		end    tado.OverlayEnd
		rule   string
		endsAt any
	}{
		"timer":      {tado.OverlayEnd{Mode: tado.OverlayEndTimer, DurationSec: 1800}, "timer", windowStart.Add(30 * time.Minute).Format(time.RFC3339)},
		"until_next": {tado.OverlayEnd{Mode: tado.OverlayEndUntilNext}, "until_next", windowStart.Add(time.Hour).Format(time.RFC3339)},
		"manual":     {tado.OverlayEnd{Mode: tado.OverlayEndManual}, "manual", nil},
	} {
		d, url, in := deviceOverlayDaemon(t, &tc.end, newTestClock(windowStart), nil)
		overlayFromDevice(t, d, in, "VA1000000001", 5, 2200, 0x6601)
		wantOverlayRule(t, name, url, 5, "manual", tc.rule, tc.endsAt)
	}
}

// TestOverlayRuleOfLowLevelRoutes: the node routes with a durationSec arm a
// timer, and the hot-water route too.
func TestOverlayRuleOfLowLevelRoutes(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	var res map[string]any
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/VA1000000002/setpoint",
		map[string]any{"tempC": 20.0, "mode": "manual", "id": 3, "durationSec": 600}, &res); code != http.StatusOK {
		t.Fatalf("node setpoint: code=%d %v", code, res)
	}
	deliver(t, d, in, wireOf(t, in, "VA1000000002"))
	wantOverlayRule(t, "node setpoint with a duration", url, 3, "manual", "timer", windowStart.Add(10*time.Minute).Format(time.RFC3339))

	if code := postJSON(t, http.MethodPost, url+"/api/boiler/dhw", map[string]any{"tempC": 50.0, "durationSec": 900}, &res); code != http.StatusOK {
		t.Fatalf("hot water: code=%d %v", code, res)
	}
	deliver(t, d, in, wireOf(t, in, "RU1000000001"))
	wantOverlayRule(t, "hot water with a duration", url, 0, "until_next", "timer", windowStart.Add(15*time.Minute).Format(time.RFC3339))
}

// TestOverlayRuleSurvivesRestart: the rule of a timer comes back from the
// database next to its deadline, and a save of timers with no rule clears the
// stored rules.
func TestOverlayRuleSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	in := windowInstall(t, nil)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, store)
	if _, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 19, "mode": "timer", "durationSec": 3600}); code != http.StatusOK {
		t.Fatalf("timer: code=%d", code)
	}
	deliver(t, d, in, wireOf(t, in, "VA1000000002"))
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
	_, url = newWindowDaemon(t, in, clock, reopened)
	wantOverlayRule(t, "after the restart", url, 3, "until_next", "timer", windowStart.Add(time.Hour).Format(time.RFC3339))

	state, err := reopened.Load()
	if err != nil {
		t.Fatal(err)
	}
	for key, timer := range state.Boost {
		timer.Rule = ""
		state.Boost[key] = timer
	}
	if err := reopened.Save(t.Context(), state, 1<<40); err != nil {
		t.Fatal(err)
	}
	if stored, err := reopened.Load(); err != nil || len(stored.Boost) != 1 {
		t.Fatalf("stored state after a save without rules: %d timers, err=%v", len(stored.Boost), err)
	} else {
		for key, timer := range stored.Boost {
			if timer.Rule != "" {
				t.Errorf("timer %s keeps the rule %q after a save without rules", key, timer.Rule)
			}
		}
	}
}

// TestOverlayRuleOf: a timer with no stored rule reads as until_next, an
// overlay with no timer reads from its 6240.
func TestOverlayRuleOf(t *testing.T) {
	for name, tc := range map[string]struct {
		overlay byte
		timer   tado.OverlayTimer
		timed   bool
		want    string
	}{
		"manual":                    {2, tado.OverlayTimer{}, false, "manual"},
		"until_next with no timer":  {3, tado.OverlayTimer{}, false, "until_next"},
		"a timer with no rule":      {3, tado.OverlayTimer{ExpiresAt: windowStart}, true, "until_next"},
		"a device timer, no rule":   {2, tado.OverlayTimer{ExpiresAt: windowStart}, true, "until_next"},
		"a timer with its rule":     {3, tado.OverlayTimer{ExpiresAt: windowStart, Rule: tado.OverlayEndTimer}, true, "timer"},
		"the rule over the 6240":    {2, tado.OverlayTimer{Rule: tado.OverlayEndHomeChange}, true, "until_home_change"},
		"a rule with no timer held": {3, tado.OverlayTimer{Rule: tado.OverlayEndTimer}, false, "until_next"},
	} {
		if got := overlayRule(tc.overlay, tc.timer, tc.timed); got != tc.want {
			t.Errorf("%s: rule = %q, want %q", name, got, tc.want)
		}
	}
}
