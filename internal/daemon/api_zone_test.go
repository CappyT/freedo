package daemon

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"freedo/tado"
)

// zoneTargetOf reads the target view of zone through GET /api/zones.
func zoneTargetOf(t *testing.T, url string, zone int) (map[string]any, string) {
	t.Helper()
	var zones struct {
		Declared []struct {
			Number int            `json:"number"`
			Writer string         `json:"writer"`
			Target map[string]any `json:"target"`
		} `json:"declared"`
	}
	if code := getJSON(t, url+"/api/zones", &zones); code != http.StatusOK {
		t.Fatalf("zones: code=%d", code)
	}
	for _, z := range zones.Declared {
		if z.Number == zone {
			return z.Target, z.Writer
		}
	}
	t.Fatalf("zone %d is not declared", zone)
	return nil, ""
}

func postZone(t *testing.T, url string, zone int, verb string, body any) (map[string]any, int) {
	t.Helper()
	var out map[string]any
	code := postJSON(t, http.MethodPost, url+"/api/zones/"+strconv.Itoa(zone)+"/"+verb, body, &out)
	return out, code
}

func overlayOfTarget(t *testing.T, target map[string]any) map[string]any {
	t.Helper()
	overlay, ok := target["overlay"].(map[string]any)
	if !ok {
		t.Fatalf("target %v carries no overlay", target)
	}
	return overlay
}

// TestZoneSetpointWritesTheReporter: the zone route writes z/s?id=N to the
// demand reporter alone, as the capture shows, the wall thermostat included,
// and no other member takes a write.
func TestZoneSetpointWritesTheReporter(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	for _, tc := range []struct {
		zone          int
		writer, other string
	}{
		{1, "RU1000000001", "VA1000000003"},
		{5, "VA1000000006", "VA1000000001"},
		{8, "a803", "VA1000000004"},
	} {
		writer, other := wireOf(t, in, tc.writer), wireOf(t, in, tc.other)
		body, code := postZone(t, url, tc.zone, "setpoint", map[string]any{"setpointC": 21.5})
		if code != http.StatusOK || body["queued"] != true {
			t.Fatalf("zone %d setpoint: code=%d body=%v", tc.zone, code, body)
		}
		if _, short := zoneTargetOf(t, url, tc.zone); short != wireShort(t, in, tc.writer) {
			t.Errorf("zone %d writer = %q, want %s", tc.zone, short, tc.writer)
		}
		wantNoWrites(t, "a zone setpoint on another member", d, other)
		tags := deliver(t, d, in, writer)
		wantTags(t, "zone "+strconv.Itoa(tc.zone)+" setpoint", tags,
			map[string]string{"6240": "02", "6260": "01", "6280": "0866", "6440": "0001"})
		target, _ := zoneTargetOf(t, url, tc.zone)
		if target["source"] != "overlay" || target["targetC"] != 21.5 || overlayOfTarget(t, target)["kind"] != "manual" {
			t.Errorf("zone %d target = %v, want a manual overlay at 21.5", tc.zone, target)
		}
	}
}

// TestZoneSetpointTimer: a timer is a 6240=3 overlay whose deadline the
// coordinator keeps, and the target view reports that deadline.
func TestZoneSetpointTimer(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	writer := wireOf(t, in, "VA1000000002")
	if body, code := postZone(t, url, 3, "setpoint", map[string]any{
		"setpointC": 19, "mode": "timer", "durationSec": 1800,
	}); code != http.StatusOK {
		t.Fatalf("timer: code=%d body=%v", code, body)
	}
	wantTags(t, "timer", deliver(t, d, in, writer), map[string]string{"6240": "03", "6280": "076c", "6440": "0001"})
	overlay := overlayOfTarget(t, mustTarget(t, url, 3))
	if overlay["kind"] != "until_next" || overlay["endsAt"] != windowStart.Add(30*time.Minute).Format(time.RFC3339) {
		t.Errorf("timer overlay = %v, want until_next ending at +30 min", overlay)
	}

	if _, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 19, "mode": "until_next"}); code != http.StatusOK {
		t.Fatalf("until_next: code=%d", code)
	}
	wantTags(t, "until_next with no program", deliver(t, d, in, writer), map[string]string{"6240": "03"})
	if overlay := overlayOfTarget(t, mustTarget(t, url, 3)); overlay["endsAt"] != nil {
		t.Errorf("until_next with no program ends at %v, want no deadline", overlay["endsAt"])
	}
}

// TestZoneUntilNextToTimerKeepsOverlay: turning a running until_next overlay
// into a timer rewrites the same block without 6440, as captured, and the
// return to schedule the user asks for carries 6440.
func TestZoneUntilNextToTimerKeepsOverlay(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	writer := wireOf(t, in, "VA1000000002")
	if _, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 19, "mode": "until_next"}); code != http.StatusOK {
		t.Fatalf("until_next: code=%d", code)
	}
	wantTags(t, "until_next start", deliver(t, d, in, writer),
		map[string]string{"6240": "03", "6260": "01", "6280": "076c", "62e0": "00", "6440": "0001"})

	if _, code := postZone(t, url, 3, "setpoint", map[string]any{
		"setpointC": 19, "mode": "timer", "durationSec": 900,
	}); code != http.StatusOK {
		t.Fatalf("timer: code=%d", code)
	}
	wantTags(t, "until_next into timer", deliver(t, d, in, writer),
		map[string]string{"6240": "03", "6260": "01", "6280": "076c", "62e0": "00"}, "6440")

	if _, code := postZone(t, url, 3, "auto", map[string]any{}); code != http.StatusOK {
		t.Fatalf("auto: code=%d", code)
	}
	wantTags(t, "return to schedule", deliver(t, d, in, writer),
		map[string]string{"6240": "00", "62e0": "00", "6440": "0001"}, "6260", "6280")
}

func mustTarget(t *testing.T, url string, zone int) map[string]any {
	t.Helper()
	target, _ := zoneTargetOf(t, url, zone)
	return target
}

func wireShort(t *testing.T, in *tado.Install, ref string) string {
	t.Helper()
	dev, err := in.Resolve(ref)
	if err != nil {
		t.Fatal(err)
	}
	return dev.Short()
}

// TestZoneRoutesRejectBadRequests: every refused body names its error and
// queues nothing.
func TestZoneRoutesRejectBadRequests(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	for _, tc := range []struct {
		what, path string
		body       any
		code       int
		msg        string
	}{
		{"no setpoint", "3/setpoint", map[string]any{}, 400, "setpointC is required"},
		{"room above range", "3/setpoint", map[string]any{"setpointC": 25.5}, 400, "within 5..25"},
		{"room below range", "3/setpoint", map[string]any{"setpointC": 4}, 400, "within 5..25"},
		{"three decimals", "3/setpoint", map[string]any{"setpointC": 20.125}, 400, "two decimal"},
		{"bad mode", "3/setpoint", map[string]any{"setpointC": 20, "mode": "auto"}, 400, "bad mode"},
		{"duration on manual", "3/setpoint", map[string]any{"setpointC": 20, "durationSec": 60}, 400, "needs mode timer"},
		{"timer without duration", "3/setpoint", map[string]any{"setpointC": 20, "mode": "timer"}, 400, "positive durationSec"},
		{"timer too long", "3/setpoint", map[string]any{"setpointC": 20, "mode": "timer", "durationSec": 700000}, 400, "exceeds"},
		{"hot water above range", "0/setpoint", map[string]any{"setpointC": 70}, 400, "within 30..65"},
		{"hot water until_next without program", "0/setpoint", map[string]any{"setpointC": 50, "mode": "until_next"}, 400, "hot-water program"},
		{"unknown zone", "99/setpoint", map[string]any{"setpointC": 20}, 404, "zone"},
		{"bad zone number", "x/setpoint", map[string]any{"setpointC": 20}, 400, "invalid zone number"},
		{"room off bad mode", "3/off", map[string]any{"mode": "auto"}, 400, "bad mode"},
		{"room off duration on manual", "3/off", map[string]any{"durationSec": 60}, 400, "needs mode timer"},
		{"boost without on", "3/boost", map[string]any{}, 400, "on is required"},
		{"boost on hot water", "0/boost", map[string]any{"on": true}, 400, "heating zone"},
	} {
		var body map[string]string
		code := postJSON(t, http.MethodPost, url+"/api/zones/"+tc.path, tc.body, &body)
		if code != tc.code || !strings.Contains(body["error"], tc.msg) {
			t.Errorf("%s: code=%d error=%q, want %d with %q", tc.what, code, body["error"], tc.code, tc.msg)
		}
	}
	for _, dev := range in.Devices {
		wantNoWrites(t, "a refused request", d, wireOf(t, in, dev.Short()))
	}
}

// TestZoneWithoutReporterWritesEveryMember: a zone of two members that elects
// no demand reporter has no writer, so its commands go to both members, as the
// engine's write targets do, and to no one else.
func TestZoneWithoutReporterWritesEveryMember(t *testing.T) {
	in := windowInstall(t, func(in *tado.Install) {
		for i := range in.Zones {
			if in.Zones[i].Number == 5 {
				in.Zones[i].Reporter = nil
			}
		}
	})
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	if _, writer := zoneTargetOf(t, url, 5); writer != "" {
		t.Fatalf("zone 5 writer = %q, want none", writer)
	}
	body, code := postZone(t, url, 5, "setpoint", map[string]any{"setpointC": 21.5})
	if code != http.StatusOK || body["queued"] != true || body["pending"] != float64(2) {
		t.Fatalf("setpoint: code=%d body=%v, want 200 with a write queued to each member", code, body)
	}
	if target, _ := body["target"].(string); !strings.Contains(target, "VA1000000001") || !strings.Contains(target, "VA1000000006") {
		t.Errorf("target = %q, want both members", body["target"])
	}
	for _, member := range []string{"VA1000000001", "VA1000000006"} {
		wantTags(t, member+" setpoint", deliver(t, d, in, wireOf(t, in, member)),
			map[string]string{"6240": "02", "6260": "01", "6280": "0866"})
	}
	wantNoWrites(t, "a setpoint on another zone", d, wireOf(t, in, "VA1000000004"))

	for _, verb := range []string{"auto", "off"} {
		if body, code := postZone(t, url, 5, verb, map[string]any{}); code != http.StatusOK || body["pending"] != float64(2) {
			t.Errorf("%s: code=%d body=%v, want 200 with a write queued to each member", verb, code, body)
		}
	}
}

// TestZoneAutoReturnsToSchedule: the return to schedule is the captured block,
// 6240=0 with neither 6260 nor 6280 and with 6440, on the writer alone. It needs
// no earlier block of the node.
func TestZoneAutoReturnsToSchedule(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	thermostat, valve := wireOf(t, in, "RU1000000001"), wireOf(t, in, "VA1000000003")
	if body, code := postZone(t, url, 1, "auto", nil); code != http.StatusOK {
		t.Fatalf("auto: code=%d body=%v", code, body)
	}
	wantNoWrites(t, "a return to schedule on another member", d, valve)
	wantTags(t, "room auto", deliver(t, d, in, thermostat),
		map[string]string{"6020": "01", "6240": "00", "6440": "0001"}, "6260", "6280")

	if body, code := postZone(t, url, 0, "auto", nil); code != http.StatusOK {
		t.Fatalf("hot water auto: code=%d body=%v", code, body)
	}
	wantTags(t, "hot water auto", deliver(t, d, in, thermostat),
		map[string]string{"6020": "02", "6240": "00", "6440": "0001"}, "6260", "6280")
}

// TestZoneHotWater: the hot-water routes write the wall thermostat, and the
// target view reads on/off, the target, the overlay and its deadline back.
func TestZoneHotWater(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	thermostat := wireOf(t, in, "RU1000000001")

	if _, code := postZone(t, url, 0, "setpoint", map[string]any{"setpointC": 50}); code != http.StatusOK {
		t.Fatalf("hot water setpoint: code=%d", code)
	}
	wantTags(t, "hot water on", deliver(t, d, in, thermostat),
		map[string]string{"6020": "02", "6240": "02", "6260": "01", "6280": "1388"})
	target := mustTarget(t, url, 0)
	if target["on"] != true || target["targetC"] != 50.0 || target["source"] != "overlay" ||
		overlayOfTarget(t, target)["kind"] != "manual" {
		t.Errorf("hot water on: target = %v", target)
	}

	if _, code := postZone(t, url, 0, "off", map[string]any{"mode": "timer", "durationSec": 3600}); code != http.StatusOK {
		t.Fatalf("hot water off: code=%d", code)
	}
	wantTags(t, "hot water off", deliver(t, d, in, thermostat),
		map[string]string{"6020": "02", "6240": "03", "6260": "00"}, "6280")
	target = mustTarget(t, url, 0)
	overlay := overlayOfTarget(t, target)
	if target["on"] != false || target["targetC"] != nil || overlay["on"] != false ||
		overlay["endsAt"] != windowStart.Add(time.Hour).Format(time.RFC3339) {
		t.Errorf("hot water off: target = %v", target)
	}
}

// TestZoneBoost: the boost is the captured 25 C overlay on the writer, the
// target view reads it back with its deadline, and the cancel returns the zone
// to schedule.
func TestZoneBoost(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	thermostat, valve := wireOf(t, in, "RU1000000001"), wireOf(t, in, "VA1000000003")
	if body, code := postZone(t, url, 1, "boost", map[string]any{"on": true}); code != http.StatusOK || body["on"] != true {
		t.Fatalf("boost: code=%d body=%v", code, body)
	}
	wantNoWrites(t, "a boost on another member", d, valve)
	wantTags(t, "boost", deliver(t, d, in, thermostat), map[string]string{"6240": "03", "6280": "09c4", "6440": "0001"})
	target := mustTarget(t, url, 1)
	overlay := overlayOfTarget(t, target)
	if target["boost"] != true || target["source"] != "boost" || target["targetC"] != 25.0 ||
		overlay["kind"] != "boost" || overlay["endsAt"] != windowStart.Add(tado.BoostDuration).Format(time.RFC3339) {
		t.Errorf("boost: target = %v", target)
	}

	if _, code := postZone(t, url, 1, "boost", map[string]any{"on": false}); code != http.StatusOK {
		t.Fatalf("boost cancel: code=%d", code)
	}
	wantTags(t, "boost cancel", deliver(t, d, in, thermostat), map[string]string{"6240": "00", "6440": "0001"}, "6280")
	if target := mustTarget(t, url, 1); target["boost"] != false || target["overlay"] != nil {
		t.Errorf("after the cancel: target = %v", target)
	}
}

// TestZoneBoostKeepsOpenWindow: a boost written while the daemon holds the
// zone window open carries 62e0=1, so it does not close the window.
func TestZoneBoostKeepsOpenWindow(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	valve := wireOf(t, in, "VA1000000002")
	seedBlock(t, d, in, "VA1000000002", 3)
	if _, code := postWindow(t, url, 3, true); code != http.StatusOK {
		t.Fatalf("window: code=%d", code)
	}
	wantWindowWrites(t, "the external open", d, in, "01", valve)
	if _, code := postZone(t, url, 3, "boost", map[string]any{"on": true}); code != http.StatusOK {
		t.Fatalf("boost: code=%d", code)
	}
	wantTags(t, "boost with the window open", deliver(t, d, in, valve), map[string]string{"6240": "03", "62e0": "01"})
	if target := mustTarget(t, url, 3); target["source"] != "window" || target["targetC"] != nil || target["boost"] != true {
		t.Errorf("boost with the window open: target = %v", target)
	}
}

// TestZoneTargetProgramAndAway: a zone on its program targets the program
// setpoint; away it targets its own fallback.
func TestZoneTargetProgramAndAway(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	if target := mustTarget(t, url, 3); target["known"] != false || target["source"] != nil {
		t.Errorf("before any block: target = %v", target)
	}
	seedBlock(t, d, in, "VA1000000002", 3)
	target := mustTarget(t, url, 3)
	if target["source"] != "program" || target["targetC"] != 20.0 || target["programC"] != 20.0 || target["overlay"] != nil {
		t.Errorf("on the program: target = %v", target)
	}
	var home map[string]any
	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &home); code != http.StatusOK {
		t.Fatalf("away: code=%d", code)
	}
	if target := mustTarget(t, url, 3); target["source"] != "away" || target["targetC"] != nil {
		t.Errorf("away: target = %v", target)
	}
}

// wantExactTags checks that a write carries want and nothing else.
func wantExactTags(t *testing.T, what string, tags, want map[string]string) {
	t.Helper()
	wantTags(t, what, tags, want)
	if len(tags) != len(want) {
		t.Errorf("%s carries %v, want exactly %v", what, tags, want)
	}
}

// TestZoneRoomOff: the room off overlay is the captured block, 6260=0 with no
// 6280, on the writer alone. The target view reads it as an overlay with no
// target, and the away rewrite and the window stamp restate it instead of
// turning the room back on.
func TestZoneRoomOff(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	putFlatProgram(t)(d, in, url)
	seedBlock(t, d, in, "VA1000000002", 3)
	valve := wireOf(t, in, "VA1000000002")

	if body, code := postZone(t, url, 3, "off", map[string]any{"mode": "until_next"}); code != http.StatusOK || body["queued"] != true {
		t.Fatalf("off: code=%d body=%v", code, body)
	}
	wantExactTags(t, "room off", deliver(t, d, in, valve), map[string]string{
		"6160": "01", "6180": "00", "6020": "01", "61e0": "01", "6200": "079e",
		"6240": "03", "6260": "00", "62e0": "00", "6440": "0001",
	})
	target := mustTarget(t, url, 3)
	overlay := overlayOfTarget(t, target)
	if target["source"] != "overlay" || target["on"] != false || target["targetC"] != nil ||
		overlay["kind"] != "until_next" || overlay["on"] != false || overlay["setpointC"] != nil {
		t.Errorf("room off: target = %v", target)
	}

	var home map[string]any
	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &home); code != http.StatusOK {
		t.Fatalf("away: code=%d", code)
	}
	wantExactTags(t, "away rewrite", deliver(t, d, in, valve), map[string]string{
		"6160": "02", "6180": "00", "6020": "01", "61e0": "00",
		"6240": "03", "6260": "00", "62e0": "00",
	})
	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": false}, &home); code != http.StatusOK {
		t.Fatalf("home: code=%d", code)
	}
	wantTags(t, "home rewrite", deliver(t, d, in, valve),
		map[string]string{"6160": "01", "6240": "03", "6260": "00"}, "6280", "6440")

	if _, code := postWindow(t, url, 3, true); code != http.StatusOK {
		t.Fatalf("window: code=%d", code)
	}
	wantTags(t, "window stamp", deliver(t, d, in, valve),
		map[string]string{"6240": "03", "6260": "00", "62e0": "01"}, "6280", "6440")
	if target := mustTarget(t, url, 3); target["source"] != "window" || target["on"] != false || target["targetC"] != nil {
		t.Errorf("room off with the window open: target = %v", target)
	}
}

// TestZoneRoomOffModes: manual is 6240=2; a timer is 6240=3 whose deadline the
// coordinator keeps, and a write over a running overlay carries no 6440.
func TestZoneRoomOffModes(t *testing.T) {
	in := windowInstall(t, nil)
	d, url := newWindowDaemon(t, in, newTestClock(windowStart), nil)
	valve := wireOf(t, in, "VA1000000002")
	seedBlock(t, d, in, "VA1000000002", 3)

	if _, code := postZone(t, url, 3, "off", nil); code != http.StatusOK {
		t.Fatalf("manual off: code=%d", code)
	}
	wantTags(t, "manual off", deliver(t, d, in, valve),
		map[string]string{"6240": "02", "6260": "00", "6440": "0001"}, "6280")
	if overlay := overlayOfTarget(t, mustTarget(t, url, 3)); overlay["kind"] != "manual" || overlay["endsAt"] != nil {
		t.Errorf("manual off overlay = %v", overlay)
	}

	if _, code := postZone(t, url, 3, "off", map[string]any{"mode": "timer", "durationSec": 900}); code != http.StatusOK {
		t.Fatalf("timer off: code=%d", code)
	}
	wantTags(t, "timer off", deliver(t, d, in, valve),
		map[string]string{"6240": "03", "6260": "00"}, "6280", "6440")
	overlay := overlayOfTarget(t, mustTarget(t, url, 3))
	if overlay["kind"] != "until_next" || overlay["on"] != false ||
		overlay["endsAt"] != windowStart.Add(15*time.Minute).Format(time.RFC3339) {
		t.Errorf("timer off overlay = %v, want until_next ending at +15 min", overlay)
	}

	if _, code := postZone(t, url, 3, "setpoint", map[string]any{"setpointC": 21.5}); code != http.StatusOK {
		t.Fatalf("setpoint: code=%d", code)
	}
	wantTags(t, "back on", deliver(t, d, in, valve), map[string]string{"6240": "02", "6260": "01", "6280": "0866"})
	if target := mustTarget(t, url, 3); target["on"] != true || target["targetC"] != 21.5 {
		t.Errorf("back on: target = %v", target)
	}
}

// TestZoneRoomOffSurvivesRestart: after a restart the node block is gone, and
// the window close restates the off overlay from the stored zone cache.
func TestZoneRoomOffSurvivesRestart(t *testing.T) {
	d, url, in, _, _ := restartAfterOpen(t, putFlatProgram(t), func(d *daemon, in *tado.Install, url string) {
		if _, code := postZone(t, url, 3, "off", nil); code != http.StatusOK {
			t.Fatalf("off: code=%d", code)
		}
		wantTags(t, "room off", deliver(t, d, in, wireOf(t, in, "VA1000000002")),
			map[string]string{"6240": "02", "6260": "00"}, "6280")
		if err := d.saveDesiredState(); err != nil {
			t.Fatal(err)
		}
	})
	if _, code := postWindow(t, url, 3, false); code != http.StatusOK {
		t.Fatalf("close: code=%d", code)
	}
	wantExactTags(t, "the close", deliver(t, d, in, wireOf(t, in, "VA1000000002")), map[string]string{
		"6160": "01", "6180": "00", "6020": "01", "61e0": "01", "6200": "079e",
		"6240": "02", "6260": "00", "62e0": "00",
	})
}
