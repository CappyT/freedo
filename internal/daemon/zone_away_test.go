package daemon

// zone_away_test: the away temperature of a heating zone on /api/install/zones,
// the writes it causes and its storage. The installation is synthetic; the
// write shapes are the ones of the away capture.

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"

	"freedo/internal/install"
	"freedo/tado"
)

func awayView(t *testing.T, res map[string]any) any {
	t.Helper()
	zone, ok := res["zone"].(map[string]any)
	if !ok {
		t.Fatalf("answer %v carries no zone", res)
	}
	value, present := zone["away_temperature_c"]
	if !present {
		t.Fatalf("zone view %v carries no away_temperature_c", zone)
	}
	return value
}

// TestAwayTemperaturePatch: the field reads back as written, null clears it,
// the room minimum is stored as given, and every value outside the room
// setpoint range or the hot water zone is refused with the rows untouched.
func TestAwayTemperaturePatch(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	const zone = "/api/install/zones/3"

	if v := awayView(t, a.get(zone)); v != nil {
		t.Errorf("default away_temperature_c = %v, want null", v)
	}
	if v := awayView(t, a.get("/api/install/zones/0")); v != nil {
		t.Errorf("hot water away_temperature_c = %v, want null", v)
	}
	for _, v := range []float64{15.5, 25, 5} {
		res := a.mutate(http.MethodPatch, zone, map[string]any{"away_temperature_c": v})
		if got := awayView(t, res); got != v {
			t.Errorf("PATCH %g answers %v", v, got)
		}
		if got := awayView(t, a.get(zone)); got != v {
			t.Errorf("GET after PATCH %g reads %v", v, got)
		}
	}
	listed := a.get("/api/install/zones")["zones"].([]any)
	for _, z := range listed {
		view := z.(map[string]any)
		if view["number"] == 3.0 && view["away_temperature_c"] != 5.0 {
			t.Errorf("zone list shows %v for zone 3, want 5", view["away_temperature_c"])
		}
	}
	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Options.AwayTemperatures; len(got) != 1 || got[3] != 5 {
		t.Errorf("stored away temperatures = %v, want zone 3 at 5", got)
	}

	if v := awayView(t, a.mutate(http.MethodPatch, zone, map[string]any{"away_temperature_c": nil})); v != nil {
		t.Errorf("PATCH null answers %v, want null", v)
	}
	stored, _, err = a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Options.AwayTemperatures) != 0 {
		t.Errorf("stored away temperatures = %v after the clear, want none", stored.Options.AwayTemperatures)
	}

	for _, tc := range []struct {
		name string
		path string
		body any
		code int
	}{
		{"below the range", zone, map[string]any{"away_temperature_c": 4.99}, http.StatusBadRequest},
		{"above the range", zone, map[string]any{"away_temperature_c": 25.01}, http.StatusBadRequest},
		{"zero", zone, map[string]any{"away_temperature_c": 0}, http.StatusBadRequest},
		{"three decimals", zone, map[string]any{"away_temperature_c": 15.123}, http.StatusBadRequest},
		{"a string", zone, map[string]any{"away_temperature_c": "warm"}, http.StatusBadRequest},
		{"hot water zone", "/api/install/zones/0", map[string]any{"away_temperature_c": 38}, http.StatusBadRequest},
		{"unknown zone", "/api/install/zones/99", map[string]any{"away_temperature_c": 15}, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, res := a.call(http.MethodPatch, tc.path, tc.body); code != tc.code {
				t.Errorf("code=%d %v, want %d", code, res, tc.code)
			}
		})
	}
	if v := awayView(t, a.get(zone)); v != nil {
		t.Errorf("away_temperature_c = %v after the refused writes, want null", v)
	}
}

// onlyPending checks that target holds want queued writes and no other declared
// device holds any.
func onlyPending(t *testing.T, what string, a *installAPI, target string, want int) {
	t.Helper()
	for key, n := range a.pending() {
		if key == target && n != want || key != target && n != 0 {
			t.Errorf("%s: %s holds %d queued writes, want %d on %s and none elsewhere", what, key, n, want, target)
		}
	}
}

func dropAllQueues(t *testing.T, a *installAPI) {
	t.Helper()
	for key := range a.pending() {
		dropQueue(t, a.d, key)
	}
}

// TestAwayTemperatureWrites: the switch to away writes each zone with its own
// away head, a change while away writes that zone alone at once, a change while
// home writes nothing, and the return home restores the program setpoint.
func TestAwayTemperatureWrites(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	const zone = "/api/install/zones/3"
	office := wireOf(t, a.d.inst.Load(), "VA1000000002")
	bath := wireOf(t, a.d.inst.Load(), "VA1000000007")

	var view map[string]any
	if code := postJSON(t, http.MethodPut, a.base+"/api/zones/3/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 20.0}), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	setAway := func(away bool) {
		t.Helper()
		if code, res := a.call(http.MethodPatch, "/api/home", map[string]any{"away": away}); code != http.StatusOK {
			t.Fatalf("home away=%t: code=%d %v", away, code, res)
		}
	}
	patch := func(v any) {
		t.Helper()
		a.mutate(http.MethodPatch, zone, map[string]any{"away_temperature_c": v})
	}

	dropAllQueues(t, a)
	patch(12.0)
	onlyPending(t, "a change while home", a, office, 0)

	setAway(true)
	wantTags(t, "the switch to away, zone with 12 C", nextZoneWrite(t, a.d, office), map[string]string{
		"6160": "02", "6180": "00", "61e0": "01", "6200": "04b0", "6240": "00", "6440": "0001",
	})
	wantTags(t, "the switch to away, zone at the default", nextZoneWrite(t, a.d, bath), map[string]string{
		"6160": "02", "6180": "00", "61e0": "00", "6240": "00", "6440": "0001",
	}, "6200")

	dropAllQueues(t, a)
	patch(15.0)
	onlyPending(t, "a change while away", a, office, 1)
	wantTags(t, "the change while away", nextZoneWrite(t, a.d, office), map[string]string{
		"6160": "02", "61e0": "01", "6200": "05dc", "6240": "00", "6440": "0001",
	})
	if target := mustTarget(t, a.base, 3); target["source"] != "away" || target["targetC"] != 15.0 {
		t.Errorf("zone 3 target in away = %v, want the away source at 15", target)
	}

	dropAllQueues(t, a)
	patch(15.0)
	onlyPending(t, "the same value again", a, office, 0)

	patch(5.0)
	onlyPending(t, "a change to the room minimum", a, office, 1)
	wantTags(t, "the change to the room minimum", nextZoneWrite(t, a.d, office), map[string]string{
		"6160": "02", "61e0": "00", "6240": "00", "6440": "0001",
	}, "6200")
	dropAllQueues(t, a)
	patch(nil)
	onlyPending(t, "a clear from the room minimum", a, office, 0)

	patch(18.0)
	dropAllQueues(t, a)
	setAway(false)
	wantTags(t, "the return home", nextZoneWrite(t, a.d, office), map[string]string{
		"6160": "01", "6180": "00", "61e0": "01", "6200": "07d0", "6240": "00", "6440": "0001",
	})

	dropAllQueues(t, a)
	patch(21.0)
	onlyPending(t, "a change while home again", a, office, 0)
}

// reportedBlock is the block a valve reports for a zone at home with no overlay:
// the head with the program setpoint centiC, which is what lets the engine
// compare the target of its next write with the one the zone holds.
func reportedBlock(centiC uint16) []tado.TLV {
	sched := []byte{byte(centiC >> 8), byte(centiC)}
	return []tado.TLV{
		{Tag: "6160", Val: []byte{0x01}}, {Tag: "6180", Val: []byte{0x00}}, {Tag: "6020", Val: []byte{0x01}},
		{Tag: "61e0", Val: []byte{0x01}}, {Tag: "6200", Val: sched}, {Tag: "6240", Val: []byte{0x00}},
		{Tag: "6260", Val: []byte{0x01}}, {Tag: "6280", Val: []byte{0x09, 0xc4}}, {Tag: "62e0", Val: []byte{0x00}},
	}
}

// TestAwaySwitchMarksOnlyMovedTargets: 6440 follows the target. Zone 3 holds
// 18.00 C at home and keeps 18.00 C in away, so neither switch carries 6440.
// Zone 10 goes from 20.00 C to off and back, so both do.
func TestAwaySwitchMarksOnlyMovedTargets(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	in := a.d.inst.Load()
	office, bedroom := wireOf(t, in, "VA1000000002"), wireOf(t, in, "VA1000000005")
	for zone, setpoint := range map[int]float64{3: 18, 10: 20} {
		var view map[string]any
		if code := postJSON(t, http.MethodPut, a.base+"/api/zones/"+strconv.Itoa(zone)+"/schedule",
			allDayProgram(map[string]any{"minute": 0, "setpoint_c": setpoint}), &view); code != http.StatusOK {
			t.Fatalf("schedule of zone %d: code=%d %v", zone, code, view)
		}
	}
	seedBlock(t, a.d, in, "VA1000000002", 3, reportedBlock(0x0708)...)
	seedBlock(t, a.d, in, "VA1000000005", 10, reportedBlock(0x07d0)...)
	a.mutate(http.MethodPatch, "/api/install/zones/3", map[string]any{"away_temperature_c": 18.0})

	for _, tc := range []struct {
		name        string
		away        bool
		office, cam map[string]string
	}{{
		name:   "switch to away",
		away:   true,
		office: map[string]string{"6160": "02", "61e0": "01", "6200": "0708"},
		cam:    map[string]string{"6160": "02", "61e0": "00", "6440": "0001"},
	}, {
		name:   "return home",
		office: map[string]string{"6160": "01", "61e0": "01", "6200": "0708"},
		cam:    map[string]string{"6160": "01", "61e0": "01", "6200": "07d0", "6440": "0001"},
	}} {
		if code, res := a.call(http.MethodPatch, "/api/home", map[string]any{"away": tc.away}); code != http.StatusOK {
			t.Fatalf("%s: code=%d %v", tc.name, code, res)
		}
		wantTags(t, tc.name+", zone 3 keeps 18.00 C", deliverAck(t, a.d, in, office), tc.office, "6440")
		wantTags(t, tc.name+", zone 10 moves", deliverAck(t, a.d, in, bedroom), tc.cam)
	}
}

// TestAwayTemperatureChangeOnTheSameWireValue: two away temperatures that reach
// the valve as the same value, here 24.5 and 25 C past the clamp of a
// correction of -0.70 C, still write the zone, because the setting changed. The
// second write moves nothing on the wire and carries no 6440. The same setting
// again writes nothing at all (TestAwayTemperatureWrites).
func TestAwayTemperatureChangeOnTheSameWireValue(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, func(in *tado.Install) {
		in.Options.ZoneOffsets = map[int]tado.ZoneOffset{3: {Mode: tado.OffsetModeSetpoint, SetpointCenti: -70}}
	}))
	in := a.d.inst.Load()
	office := wireOf(t, in, "VA1000000002")
	seedBlock(t, a.d, in, "VA1000000002", 3, reportedBlock(0x07d0)...)
	if code, res := a.call(http.MethodPatch, "/api/home", map[string]any{"away": true}); code != http.StatusOK {
		t.Fatalf("away: code=%d %v", code, res)
	}
	dropAllQueues(t, a)
	for _, c := range []float64{24.5, 25} {
		a.mutate(http.MethodPatch, "/api/install/zones/3", map[string]any{"away_temperature_c": c})
	}
	if n := pending(t, a.d, office); n != 2 {
		t.Fatalf("%s holds %d queued writes, want the two changes", office, n)
	}
	wantTags(t, "24.5 C", deliverAck(t, a.d, in, office), map[string]string{"61e0": "01", "6200": "09c4", "6440": "0001"})
	wantTags(t, "25 C", deliverAck(t, a.d, in, office), map[string]string{"61e0": "01", "6200": "09c4"}, "6440")
}

// TestAwayTemperatureGoesOutOnTheValveScale: the away temperature is a target on
// the corrected scale, so a zone in correction mode setpoint gets it shifted
// like every other setpoint the daemon writes.
func TestAwayTemperatureGoesOutOnTheValveScale(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, func(in *tado.Install) {
		in.Options.ZoneOffsets = map[int]tado.ZoneOffset{3: {Mode: tado.OffsetModeSetpoint, SetpointCenti: -70}}
		in.Options.AwayTemperatures = map[int]float64{3: 15}
	}))
	if code, res := a.call(http.MethodPatch, "/api/home", map[string]any{"away": true}); code != http.StatusOK {
		t.Fatalf("away: code=%d %v", code, res)
	}
	wantTags(t, "the switch to away in correction mode setpoint", nextZoneWrite(t, a.d, wireOf(t, a.d.inst.Load(), "VA1000000002")),
		map[string]string{"6160": "02", "61e0": "01", "6200": "0622"})
}

// TestAwayTemperatureSurvivesRestart: the value lives in the installation rows,
// so a daemon started on them serves it and writes it on the next switch to
// away.
func TestAwayTemperatureSurvivesRestart(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	a.mutate(http.MethodPatch, "/api/install/zones/3", map[string]any{"away_temperature_c": 16.5})

	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stored.ValidateCommissionable(true); err != nil {
		t.Fatal(err)
	}
	d, url := newWindowDaemon(t, stored, newTestClock(windowStart), a.store)
	var view map[string]any
	if code := getJSON(t, url+"/api/install/zones/3", &view); code != http.StatusOK {
		t.Fatalf("zone after restart: code=%d", code)
	}
	if got := awayView(t, view); got != 16.5 {
		t.Errorf("away_temperature_c after restart = %v, want 16.5", got)
	}
	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &view); code != http.StatusOK {
		t.Fatalf("away: code=%d %v", code, view)
	}
	wantTags(t, "the switch to away after a restart", nextZoneWrite(t, d, wireOf(t, stored, "VA1000000002")),
		map[string]string{"6160": "02", "61e0": "01", "6200": "0672"})
}

// TestAwayTemperatureOnAPreviousSchema: the value is an install row, so the
// layout and its version stay as they are. A database of the previous version
// migrates with no away temperature, takes one through the API store and keeps
// it across a reopen.
func TestAwayTemperatureOnAPreviousSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	seed, err := install.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := seed.ReplaceInstall(ctx, windowInstall(t, nil)); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	old := legacyDatabase(t, path, "6", `DROP TABLE plant_episode`)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	store := newTestStore(t, path)
	var version string
	if err := store.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != install.SchemaVersion {
		t.Errorf("schema version = %q, want %q", version, install.SchemaVersion)
	}
	migrated, _, err := store.LoadInstall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrated.Options.AwayTemperatures) != 0 {
		t.Fatalf("the migrated installation holds away temperatures %v", migrated.Options.AwayTemperatures)
	}

	c := 14.0
	if _, err := store.PatchZone(ctx, 3, zonePatch{AwayTemperatureC: optFloat{set: true, value: &c}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := newTestStore(t, path)
	got, _, err := reopened.LoadInstall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if temp, ok := got.Options.AwayTemperature(3); !ok || temp != 14 {
		t.Errorf("away temperature of zone 3 after the reopen = %g, %t, want 14", temp, ok)
	}
}
