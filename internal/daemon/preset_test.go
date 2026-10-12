package daemon

// preset_test: the program presets: the migration of the first start, the
// away program a zone follows in away, the create, rename, delete and
// activation routes, the restart guarantees across presets and the schedule
// routes with mode and preset.

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
	"time"

	"freedo/internal/install"
	"freedo/internal/radio"
	"freedo/tado"
)

// presetFixtureYAML is the fixture of the preset tests: the hot-water
// thermostat serves no heating zone, so every writer holds one write per zone.
const presetFixtureYAML = `
version: 1
home_id: 7654321
network_key: "0f1e2d3c4b5a69788796a5b4c3d2e1f0"
coordinator:
  eui: "09a1cefa07c51b00"
  serial: IB1000000001
devices:
  - {kind: thermostat, eui: "05a2cafa07c51b00", serial: RU1000000001, label: hall}
  - {kind: valve, eui: "02a6cefa07c51b00", serial: VA1000000005, label: bedroom}
  - {kind: valve, eui: "08a4defa07c51b00", serial: VA1000000002, label: office}
zones:
  - {number: 0, name: hot water, kind: hotwater, members: [RU1000000001]}
  - {number: 3, name: office, kind: heating, circuit: 1, members: [VA1000000002], reporter: VA1000000002}
  - {number: 10, name: bedroom, kind: heating, circuit: 1, members: [VA1000000005], reporter: VA1000000005}
circuits:
  - {id: 1, name: house, heat_source: RU1000000001}
options:
  utc_offset_min: 0
`

func presetInstall(t *testing.T, edit func(*tado.Install)) *tado.Install {
	t.Helper()
	in, err := tado.Parse([]byte(presetFixtureYAML))
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(in)
	}
	return in
}

// newPresetDaemon is newHomeDaemon on the preset fixture.
func newPresetDaemon(t *testing.T, clock *testClock) (*daemon, string) {
	t.Helper()
	return newPresetDaemonOn(t, clock, nil)
}

// newPresetDaemonOn is newPresetDaemon with an edit of the fixture options.
func newPresetDaemonOn(t *testing.T, clock *testClock, edit func(*tado.Install)) (*daemon, string) {
	t.Helper()
	in := presetInstall(t, edit)
	cfg := in.EmuConfig()
	cfg.Now = clock.now
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(in, emu, radio.NewMock(nil), true, "mock")
	d.retryNow = clock.now
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	return d, srv.URL
}

// presetHome reads the preset GET /api/home reports.
func presetHome(t *testing.T, url string) (int64, string) {
	t.Helper()
	var h struct {
		Preset struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"preset"`
	}
	if code := getJSON(t, url+"/api/home", &h); code != http.StatusOK {
		t.Fatalf("GET /api/home: code=%d", code)
	}
	return h.Preset.ID, h.Preset.Name
}

// weekPrograms is a one-program weekly schedule for every day.
func weekPrograms(entries ...tado.ScheduleEntry) []tado.ScheduleProgram {
	return []tado.ScheduleProgram{{DayMask: 0x7f, Entries: entries}}
}

// TestPresetMigrationKeepsServedDocuments: the first start with the program
// store creates the preset Default from the programs the coordinator already
// holds, makes it active and leaves the served documents and their ETags
// alone, so a restart serves them without a re-fetch. A later restart keeps
// the same documents, ETags and preset.
func TestPresetMigrationKeepsServedDocuments(t *testing.T) {
	path := t.TempDir() + "/state.db"
	in := presetInstall(t, nil)
	officePrograms := weekPrograms(
		tado.ScheduleEntry{Minute: 0, SetpointCenti: 1900, Enabled: true},
		tado.ScheduleEntry{Minute: 420, SetpointCenti: 1600, Enabled: true},
	)
	hwPrograms := weekPrograms(
		tado.ScheduleEntry{Minute: 0, SetpointCenti: 4500, Enabled: true},
		tado.ScheduleEntry{Minute: 360, SetpointCenti: 5500, Enabled: true},
	)
	// A database as an earlier coordinator held it: the served sched documents,
	// the hot-water program in its own store, and no preset.
	emu := tado.NewEmu(in.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	office := wireOf(t, in, "VA1000000002")
	if err := emu.SetSchedule(office, officePrograms); err != nil {
		t.Fatal(err)
	}
	if err := emu.SetSchedule(wireOf(t, in, dhwNode), weekPrograms(
		tado.ScheduleEntry{Minute: 0, SetpointCenti: 2100, Enabled: true},
	)); err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t, path)
	if err := store.Save(context.Background(), emu.ExportDesiredState(), 1); err != nil {
		t.Fatal(err)
	}
	if err := store.SetHotWaterProgram(context.Background(), hwPrograms); err != nil {
		t.Fatal(err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	beforeETag := string(st.Schedule[office].ETag)
	if beforeETag == "" {
		t.Fatal("the seeded state holds no office document")
	}

	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url, stop := startStoredDaemon(t, in, clock, store)
	if _, name := presetHome(t, url); name != "Default" {
		t.Errorf("the active preset is %q, want Default", name)
	}
	wantDoc := func(step string) {
		t.Helper()
		v, err := d.do(func(e *tado.EmuState) (any, error) {
			_, set, err := e.Schedule(office)
			etag := string(e.ExportDesiredState().Schedule[office].ETag)
			return []any{set, etag}, err
		})
		if err != nil {
			t.Fatal(err)
		}
		got := v.([]any)
		if !got[0].(bool) {
			t.Errorf("%s: the office holds no served document", step)
		}
		if got[1] != beforeETag {
			t.Errorf("%s: the office ETag is %q, want %q as before the migration", step, got[1], beforeETag)
		}
	}
	wantDoc("after the migration")
	if got := engineValue(t, d, func() []tado.ScheduleProgram {
		home, _ := d.zonePrograms(3)
		return home
	}); !reflect.DeepEqual(got, officePrograms) {
		t.Errorf("the office home program is %v, want the one of the served document", got)
	}
	if got := engineValue(t, d, func() []tado.ScheduleProgram {
		home, _ := d.zonePrograms(0)
		return home
	}); !reflect.DeepEqual(got, hwPrograms) {
		t.Errorf("the hot-water home program is %v, want the one of the daemon store", got)
	}
	// The stored-block baseline came from the pre-upgrade state: the first tick
	// writes nothing and queues no fetch trigger.
	tickHome(t, d)
	for _, ref := range []string{"VA1000000002", "VA1000000005", dhwNode} {
		if n := pending(t, d, wireOf(t, in, ref)); n != 0 {
			t.Errorf("%s took %d writes on the first tick, want none", ref, n)
		}
	}
	stop()

	d, url2, _ := startStoredDaemon(t, in, clock, newTestStore(t, path))
	wantDoc("after the restart")
	if _, name := presetHome(t, url2); name != "Default" {
		t.Errorf("the active preset after the restart is %q, want Default", name)
	}
	tickHome(t, d)
	for _, ref := range []string{"VA1000000002", "VA1000000005", dhwNode} {
		if n := pending(t, d, wireOf(t, in, ref)); n != 0 {
			t.Errorf("%s took %d writes on the restart tick, want none", ref, n)
		}
	}
}

// TestAwayProgramWritesBlocksInAway: in away a zone with an away program
// follows it: the away burst carries its block in force (61e0=1, 6200), a
// boundary of the away program moves the write, once, and no home boundary
// does. A zone without an away program keeps the off of the away fallback.
func TestAwayProgramWritesBlocksInAway(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url := newPresetDaemon(t, clock)
	bedroom := wireOf(t, presetInstall(t, nil), "VA1000000005")
	office := wireOf(t, presetInstall(t, nil), "VA1000000002")
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 20.0}, map[string]any{"minute": 360, "setpoint_c": 18.0}), &view); code != http.StatusOK {
		t.Fatalf("bedroom home schedule: code=%d %v", code, view)
	}
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule?mode=away",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 16.0}, map[string]any{"minute": 420, "setpoint_c": 15.0}), &view); code != http.StatusOK {
		t.Fatalf("bedroom away schedule: code=%d %v", code, view)
	}
	if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 19.0}), &view); code != http.StatusOK {
		t.Fatalf("office home schedule: code=%d %v", code, view)
	}
	dropQueue(t, d, bedroom)
	dropQueue(t, d, office)

	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &view); code != http.StatusOK {
		t.Fatalf("away: code=%d %v", code, view)
	}
	wantTags(t, "the away burst on the bedroom", nextZoneWrite(t, d, bedroom), map[string]string{
		"6160": "02", "61e0": "01", "6200": "0640", "6440": "0001",
	})
	wantTags(t, "the away burst on the office", nextZoneWrite(t, d, office), map[string]string{
		"6160": "02", "61e0": "00", "6440": "0001",
	}, "6200")
	dropQueue(t, d, bedroom)
	dropQueue(t, d, office)

	// 06:00 is a boundary of the home programs alone: no zone moves.
	clock.advance(time.Hour)
	tickHome(t, d)
	if n := pending(t, d, bedroom); n != 0 {
		t.Errorf("the bedroom took %d writes at a home boundary, want none", n)
	}
	if n := pending(t, d, office); n != 0 {
		t.Errorf("the office took %d writes at its home boundary, want none", n)
	}

	// 07:00 is the away boundary of the bedroom: it moves, once.
	clock.advance(time.Hour)
	tickHome(t, d)
	wantTags(t, "the away block change", nextZoneWrite(t, d, bedroom), map[string]string{
		"6160": "02", "61e0": "01", "6200": "05dc",
	}, "6440")
	if n := pending(t, d, office); n != 0 {
		t.Errorf("the office took %d writes at the away boundary of the bedroom, want none", n)
	}
	// The write owes a delivery until the valve answers: a second tick re-queues
	// nothing once it is dropped.
	dropQueue(t, d, bedroom)
	tickHome(t, d)
	if n := pending(t, d, bedroom); n != 0 {
		t.Errorf("a second tick queued %d more writes on the bedroom, want none", n)
	}
}

// TestAwayProgramHotWater: the hot-water zone gets an away program too: in away
// the thermostat takes its blocks, off in away stays the fallback of a zone
// without one.
func TestAwayProgramHotWater(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url := newPresetDaemon(t, clock)
	in := presetInstall(t, nil)
	thermostat := wireOf(t, in, dhwNode)
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/0/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 45.0}, map[string]any{"minute": 360, "setpoint_c": 55.0}), &view); code != http.StatusOK {
		t.Fatalf("hot-water home schedule: code=%d %v", code, view)
	}
	if code := postJSON(t, http.MethodPut, url+"/api/zones/0/schedule?mode=away",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 50.0}, map[string]any{"minute": 360, "setpoint_c": 40.0}), &view); code != http.StatusOK {
		t.Fatalf("hot-water away schedule: code=%d %v", code, view)
	}

	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &view); code != http.StatusOK {
		t.Fatalf("away: code=%d %v", code, view)
	}
	wantTags(t, "the away burst on the thermostat", nextZoneWrite(t, d, thermostat), map[string]string{
		"6160": "02", "61e0": "01", "6200": "1388",
	}, "6440")
	dropQueue(t, d, thermostat)

	clock.advance(time.Hour)
	tickHome(t, d)
	wantTags(t, "the 06:00 away block", nextZoneWrite(t, d, thermostat), map[string]string{
		"6160": "02", "61e0": "01", "6200": "0fa0",
	}, "6440")
	dropQueue(t, d, thermostat)

	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": false}, &view); code != http.StatusOK {
		t.Fatalf("home: code=%d %v", code, view)
	}
	wantTags(t, "the return home on the thermostat", nextZoneWrite(t, d, thermostat), map[string]string{
		"6160": "01", "61e0": "01", "6200": "157c",
	}, "6440")
}

// TestAwayProgramFallbackKeepsAwayTemperature: a zone with an away temperature
// and no away program keeps the behaviour of today: the away burst keeps the
// fixed temperature, and a home boundary crossed in away writes it again,
// never the home block.
func TestAwayProgramFallbackKeepsAwayTemperature(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url := newPresetDaemonOn(t, clock, func(in *tado.Install) {
		in.Options.AwayTemperatures = map[int]float64{10: 14.5}
	})
	bedroom := wireOf(t, presetInstall(t, nil), "VA1000000005")
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 20.0}, map[string]any{"minute": 360, "setpoint_c": 18.0}), &view); code != http.StatusOK {
		t.Fatalf("bedroom home schedule: code=%d %v", code, view)
	}
	dropQueue(t, d, bedroom)

	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &view); code != http.StatusOK {
		t.Fatalf("away: code=%d %v", code, view)
	}
	wantTags(t, "the away burst", nextZoneWrite(t, d, bedroom), map[string]string{
		"6160": "02", "61e0": "01", "6200": "05aa", "6440": "0001",
	})
	dropQueue(t, d, bedroom)

	clock.advance(time.Hour)
	tickHome(t, d)
	wantTags(t, "the 06:00 block of the home program, in away", nextZoneWrite(t, d, bedroom), map[string]string{
		"6160": "02", "61e0": "01", "6200": "05aa",
	}, "6440")
}

// TestPresetCreateRenameDelete: a create copies the programs of the source
// preset, the active one absent copy_from. Names stay unique without case. The
// active preset and the last one cannot be deleted.
func TestPresetCreateRenameDelete(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	_, url := newPresetDaemon(t, clock)
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule?mode=away",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 16.0}), &view); code != http.StatusOK {
		t.Fatalf("bedroom away schedule: code=%d %v", code, view)
	}
	listPresets := func() []presetView {
		t.Helper()
		var out struct {
			Presets []presetView `json:"presets"`
		}
		if code := getJSON(t, url+"/api/presets", &out); code != http.StatusOK {
			t.Fatalf("GET /api/presets: code=%d", code)
		}
		return out.Presets
	}
	if got := listPresets(); len(got) != 1 || got[0].Name != "Default" || !got[0].Active {
		t.Fatalf("presets=%+v, want Default alone and active", got)
	}

	var created presetView
	if code := postJSON(t, http.MethodPost, url+"/api/presets", map[string]any{"name": "Winter"}, &created); code != http.StatusOK {
		t.Fatalf("create: code=%d %v", code, created)
	}
	if created.Active || created.Name != "Winter" {
		t.Errorf("create=%+v, want the inactive Winter", created)
	}
	var copied scheduleView
	if code := getJSON(t, url+"/api/zones/10/schedule?mode=away&preset=2", &copied); code != http.StatusOK {
		t.Fatalf("GET the copied away program: code=%d", code)
	}
	if len(copied.Programs) != 1 || copied.Programs[0].Entries[0].SetpointC != 16 {
		t.Errorf("the copy holds %+v, want the 16 C away program", copied.Programs)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/presets", map[string]any{"name": "winter"}, &view); code != http.StatusConflict {
		t.Errorf("create with a taken name: code=%d %v, want 409", code, view)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/presets", map[string]any{"name": "x", "copy_from": 9}, &view); code != http.StatusNotFound {
		t.Errorf("create from an unknown preset: code=%d %v, want 404", code, view)
	}

	var renamed presetView
	if code := postJSON(t, http.MethodPatch, url+"/api/presets/2", map[string]any{"name": "Summer"}, &renamed); code != http.StatusOK {
		t.Fatalf("rename: code=%d %v", code, renamed)
	}
	if renamed.Name != "Summer" || renamed.Active {
		t.Errorf("rename=%+v, want the inactive Summer", renamed)
	}
	if code := postJSON(t, http.MethodPatch, url+"/api/presets/2", map[string]any{"name": "default"}, &view); code != http.StatusConflict {
		t.Errorf("rename to a taken name: code=%d %v, want 409", code, view)
	}
	if code := postJSON(t, http.MethodPatch, url+"/api/presets/9", map[string]any{"name": "x"}, &view); code != http.StatusNotFound {
		t.Errorf("rename an unknown preset: code=%d %v, want 404", code, view)
	}

	if code := postJSON(t, http.MethodDelete, url+"/api/presets/1", nil, &view); code != http.StatusConflict {
		t.Errorf("delete the active preset: code=%d %v, want 409", code, view)
	}
	if code := postJSON(t, http.MethodDelete, url+"/api/presets/9", nil, &view); code != http.StatusNotFound {
		t.Errorf("delete an unknown preset: code=%d %v, want 404", code, view)
	}
	if code := postJSON(t, http.MethodDelete, url+"/api/presets/2", nil, &view); code != http.StatusOK {
		t.Fatalf("delete the Summer preset: code=%d %v", code, view)
	}
	if got := listPresets(); len(got) != 1 || !got[0].Active {
		t.Errorf("presets after the delete=%+v, want Default alone and active", got)
	}
	if code := postJSON(t, http.MethodDelete, url+"/api/presets/1", nil, &view); code != http.StatusConflict {
		t.Errorf("delete the last preset: code=%d %v, want 409", code, view)
	}
}

// TestPresetActivationServesAndWrites: the activation of a preset is one
// action: its home programs are served to the zone members, whose unchanged
// documents take no fetch trigger, and each zone whose block in force moved
// gets one write. Editing without naming a preset edits the active one.
func TestPresetActivationServesAndWrites(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url := newPresetDaemon(t, clock)
	in := presetInstall(t, nil)
	bedroom := wireOf(t, in, "VA1000000005")
	office := wireOf(t, in, "VA1000000002")
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 20.0}), &view); code != http.StatusOK {
		t.Fatalf("bedroom schedule: code=%d %v", code, view)
	}
	if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 19.0}), &view); code != http.StatusOK {
		t.Fatalf("office schedule: code=%d %v", code, view)
	}
	tickHome(t, d)
	dropQueue(t, d, bedroom)
	dropQueue(t, d, office)

	var created presetView
	if code := postJSON(t, http.MethodPost, url+"/api/presets", map[string]any{"name": "Summer"}, &created); code != http.StatusOK {
		t.Fatalf("create: code=%d %v", code, created)
	}
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule?preset=2",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 22.0}), &view); code != http.StatusOK {
		t.Fatalf("bedroom schedule of the Summer preset: code=%d %v", code, view)
	}
	var read scheduleView
	if code := getJSON(t, url+"/api/zones/10/schedule", &read); code != http.StatusOK {
		t.Fatalf("GET the bedroom schedule: code=%d", code)
	}
	if read.Programs[0].Entries[0].SetpointC != 20 {
		t.Errorf("the active preset holds %v C, want the unchanged 20", read.Programs[0].Entries[0].SetpointC)
	}
	if n := pending(t, d, bedroom); n != 0 {
		t.Errorf("editing an inactive preset queued %d writes, want none", n)
	}

	if code := postJSON(t, http.MethodPost, url+"/api/presets/2/activate", nil, &view); code != http.StatusOK {
		t.Fatalf("activate: code=%d %v", code, view)
	}
	if id, name := presetHome(t, url); id != 2 || name != "Summer" {
		t.Errorf("active preset=(%d, %q), want (2, Summer)", id, name)
	}
	// The served office document kept its bytes: no fetch trigger and no write.
	if n := pending(t, d, office); n != 0 {
		t.Errorf("the office holds %d queued writes, want none", n)
	}
	// The bedroom document moved: the fetch trigger ahead of one block write.
	n, head := takeQueue(t, d, "VA1000000005")
	if n != 2 || head == nil || head.Code != "0.02" || head.URI() != "d/sched" {
		t.Errorf("the bedroom holds %d writes headed by %v, want the fetch trigger and one block write", n, head)
	}
	if block := engineValue(t, d, func() scheduledBlock { return d.schedPushed[10] }); block != (scheduledBlock{c: 22}) {
		t.Errorf("the bedroom follows %v, want the 22 C block", block)
	}
	if code := getJSON(t, url+"/api/zones/10/schedule", &read); code != http.StatusOK {
		t.Fatalf("GET the bedroom schedule after the activation: code=%d", code)
	}
	if read.Programs[0].Entries[0].SetpointC != 22 {
		t.Errorf("the active preset holds %v C, want 22", read.Programs[0].Entries[0].SetpointC)
	}
	// Activating the active preset is a silent no-op.
	if code := postJSON(t, http.MethodPost, url+"/api/presets/2/activate", nil, &view); code != http.StatusOK {
		t.Fatalf("activate again: code=%d %v", code, view)
	}
	if n := pending(t, d, bedroom); n != 0 {
		t.Errorf("activating the active preset queued %d writes, want none", n)
	}
	tickHome(t, d)
	if n := pending(t, d, bedroom); n != 0 {
		t.Errorf("a tick after the activation queued %d more writes, want none", n)
	}
}

// TestPresetActivationInAway: the activation of a preset while away serves the
// home programs and writes each zone whose away block moved, in the away
// shape.
func TestPresetActivationInAway(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url := newPresetDaemon(t, clock)
	in := presetInstall(t, nil)
	bedroom := wireOf(t, in, "VA1000000005")
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 20.0}), &view); code != http.StatusOK {
		t.Fatalf("bedroom schedule: code=%d %v", code, view)
	}
	tickHome(t, d)
	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &view); code != http.StatusOK {
		t.Fatalf("away: code=%d %v", code, view)
	}
	dropQueue(t, d, bedroom)

	var created presetView
	if code := postJSON(t, http.MethodPost, url+"/api/presets", map[string]any{"name": "Winter"}, &created); code != http.StatusOK {
		t.Fatalf("create: code=%d %v", code, created)
	}
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule?preset=2",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 22.0}), &view); code != http.StatusOK {
		t.Fatalf("home schedule of the Winter preset: code=%d %v", code, view)
	}
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule?preset=2&mode=away",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 16.0}), &view); code != http.StatusOK {
		t.Fatalf("away schedule of the Winter preset: code=%d %v", code, view)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/presets/2/activate", nil, &view); code != http.StatusOK {
		t.Fatalf("activate: code=%d %v", code, view)
	}
	// The away program of the Winter preset is in force: the fetch trigger of
	// the moved home document ahead of one block write in the away shape.
	n, head := takeQueue(t, d, "VA1000000005")
	if n != 2 || head == nil || head.Code != "0.02" || head.URI() != "d/sched" {
		t.Errorf("the bedroom holds %d writes headed by %v, want the fetch trigger and one block write", n, head)
	}
	if block := engineValue(t, d, func() scheduledBlock { return d.schedPushed[10] }); block != (scheduledBlock{c: 16}) {
		t.Errorf("the bedroom follows %v, want the 16 C away block", block)
	}
	tickHome(t, d)
	if n := pending(t, d, bedroom); n != 0 {
		t.Errorf("a tick after the activation queued %d more writes, want none", n)
	}
}

// TestPresetRestartKeepsActivePresetAndBlocks: a restart keeps the active
// preset and the block each zone took, so the first tick writes each zone
// whose program in force moved while the daemon was down, in the away shape
// while away, and no other zone.
func TestPresetRestartKeepsActivePresetAndBlocks(t *testing.T) {
	path := t.TempDir() + "/state.db"
	in := presetInstall(t, nil)
	bedroom := wireOf(t, in, "VA1000000005")
	office := wireOf(t, in, "VA1000000002")
	thermostat := wireOf(t, in, dhwNode)

	before := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url, stop := startStoredDaemon(t, in, before, newTestStore(t, path))
	var view map[string]any
	put := func(path string, body map[string]any, what string) {
		t.Helper()
		if code := postJSON(t, http.MethodPut, url+path, body, &view); code != http.StatusOK {
			t.Fatalf("%s: code=%d %v", what, code, view)
		}
	}
	put("/api/zones/10/schedule", allDayProgram(map[string]any{"minute": 0, "setpoint_c": 20.0}), "bedroom home schedule")
	put("/api/zones/10/schedule?mode=away",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 16.0}, map[string]any{"minute": 420, "setpoint_c": 15.0}), "bedroom away schedule")
	put("/api/zones/0/schedule", allDayProgram(map[string]any{"minute": 0, "setpoint_c": 45.0}), "hot-water home schedule")
	put("/api/zones/0/schedule?mode=away", allDayProgram(map[string]any{"minute": 0, "setpoint_c": 50.0}, map[string]any{"minute": 360, "setpoint_c": 40.0}), "hot-water away schedule")
	var created presetView
	if code := postJSON(t, http.MethodPost, url+"/api/presets", map[string]any{"name": "Summer"}, &created); code != http.StatusOK {
		t.Fatalf("create: code=%d %v", code, created)
	}
	put("/api/zones/3/schedule?preset=2", allDayProgram(map[string]any{"minute": 0, "setpoint_c": 19.0}), "office schedule of the Summer preset")
	if code := postJSON(t, http.MethodPost, url+"/api/presets/2/activate", nil, &view); code != http.StatusOK {
		t.Fatalf("activate: code=%d %v", code, view)
	}
	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &view); code != http.StatusOK {
		t.Fatalf("away: code=%d %v", code, view)
	}
	tickHome(t, d)
	for _, target := range []string{bedroom, office, thermostat} {
		deliverQueue(t, d, in, target)
	}
	tickHome(t, d)
	stop()

	after := newTestClock(time.Date(2026, 1, 5, 7, 30, 0, 0, time.UTC))
	d, url, _ = startStoredDaemon(t, in, after, newTestStore(t, path))
	if id, name := presetHome(t, url); id != 2 || name != "Summer" {
		t.Errorf("the active preset after the restart is (%d, %q), want (2, Summer)", id, name)
	}
	tickHome(t, d)
	// The away boundaries crossed while the daemon was down: the bedroom away
	// block 16 -> 15 and the hot-water one 50 -> 40, each written once in the
	// away shape. The office, whose block never moved, takes nothing.
	wantTags(t, "the bedroom block after the restart", nextZoneWrite(t, d, bedroom), map[string]string{
		"6160": "02", "61e0": "01", "6200": "05dc",
	}, "6440")
	wantTags(t, "the hot-water block after the restart", nextZoneWrite(t, d, thermostat), map[string]string{
		"6160": "02", "61e0": "01", "6200": "0fa0",
	}, "6440")
	if n := pending(t, d, office); n != 0 {
		t.Errorf("the office took %d writes after the restart, want none", n)
	}
	for _, target := range []string{bedroom, thermostat} {
		dropQueue(t, d, target)
	}
	tickHome(t, d)
	for _, target := range []string{bedroom, office, thermostat} {
		if n := pending(t, d, target); n != 0 {
			t.Errorf("%s took %d more writes on the second tick, want none", target, n)
		}
	}
	var read scheduleView
	if code := getJSON(t, url+"/api/zones/3/schedule", &read); code != http.StatusOK ||
		read.Programs[0].Entries[0].SetpointC != 19 {
		t.Errorf("GET the office schedule after the restart: code=%d programs=%+v, want the 19 C program of the Summer preset", code, read.Programs)
	}
}

// TestUntilNextInAwayEndsAtAwayBlock: an until_next overlay in away ends at the
// next block of the away program, and the end restores the away block then in
// force.
func TestUntilNextInAwayEndsAtAwayBlock(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url := newPresetDaemon(t, clock)
	in := presetInstall(t, nil)
	bedroom := wireOf(t, in, "VA1000000005")
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 20.0}, map[string]any{"minute": 360, "setpoint_c": 18.0}), &view); code != http.StatusOK {
		t.Fatalf("bedroom home schedule: code=%d %v", code, view)
	}
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule?mode=away",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 16.0}, map[string]any{"minute": 420, "setpoint_c": 15.0}), &view); code != http.StatusOK {
		t.Fatalf("bedroom away schedule: code=%d %v", code, view)
	}
	tickHome(t, d)
	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &view); code != http.StatusOK {
		t.Fatalf("away: code=%d %v", code, view)
	}
	dropQueue(t, d, bedroom)

	if code := postJSON(t, http.MethodPost, url+"/api/zones/10/setpoint",
		map[string]any{"setpointC": 21.0, "mode": "until_next"}, &view); code != http.StatusOK {
		t.Fatalf("until_next: code=%d %v", code, view)
	}
	deliver(t, d, in, bedroom)
	// 06:00 is the home boundary, 07:00 the away one: the overlay ends at the
	// away block.
	wanted := time.Date(2026, 1, 5, 7, 0, 0, 0, time.UTC).Format(time.RFC3339)
	if overlay := overlayOfTarget(t, mustTarget(t, url, 10)); overlay["endsAt"] != wanted {
		t.Errorf("the until_next overlay ends at %v, want %v, the away boundary", overlay["endsAt"], wanted)
	}

	// 07:00 moves the away block under the running overlay, and the end of the
	// overlay restores the away block then in force.
	clock.advance(2 * time.Hour)
	tickHome(t, d)
	dropQueue(t, d, bedroom)
	if _, err := d.do(func(e *tado.EmuState) (any, error) { return e.ExpireBoosts(d.retryNow()) }); err != nil {
		t.Fatal(err)
	}
	wantTags(t, "the end of the overlay restores the away block", nextZoneWrite(t, d, bedroom), map[string]string{
		"6160": "02", "61e0": "01", "6200": "05dc",
	}, "6440")
}

// TestScheduleRoutesModeAndPreset: the schedule routes take mode=home|away and
// preset=<id>; a bad mode is a 400, an unknown preset a 404, and the setpoint
// ranges of the zone hold on either mode. A DELETE of the away program returns
// the zone to the fallback, a DELETE of the home program leaves the zone
// without a program and drops the served document of every member.
func TestScheduleRoutesModeAndPreset(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url := newPresetDaemon(t, clock)
	in := presetInstall(t, nil)
	bedroom := wireOf(t, in, "VA1000000005")
	var view map[string]any
	var read scheduleView

	if code := getJSON(t, url+"/api/zones/10/schedule?mode=junk", &read); code != http.StatusBadRequest {
		t.Errorf("GET ?mode=junk: code=%d, want 400", code)
	}
	if code := getJSON(t, url+"/api/zones/10/schedule?preset=9", &read); code != http.StatusNotFound {
		t.Errorf("GET ?preset=9: code=%d, want 404", code)
	}
	if code := getJSON(t, url+"/api/zones/10/schedule?mode=away", &read); code != http.StatusNotFound {
		t.Errorf("GET ?mode=away with no away program: code=%d, want 404", code)
	}
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule?mode=away",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 30.0}), &view); code != http.StatusBadRequest {
		t.Errorf("PUT a 30 C away program on a bedroom: code=%d, want 400", code)
	}
	if code := postJSON(t, http.MethodPut, url+"/api/zones/0/schedule?mode=away",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 25.0}), &view); code != http.StatusBadRequest {
		t.Errorf("PUT a 25 C away program on hot water: code=%d, want 400", code)
	}
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule?mode=away&preset=9",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 16.0}), &view); code != http.StatusNotFound {
		t.Errorf("PUT ?preset=9: code=%d, want 404", code)
	}

	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule?mode=away",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 16.0}), &view); code != http.StatusOK {
		t.Fatalf("PUT the away program: code=%d %v", code, view)
	}
	if code := getJSON(t, url+"/api/zones/10/schedule?mode=away", &read); code != http.StatusOK {
		t.Fatalf("GET the away program: code=%d", code)
	}
	if read.Mode != "away" || read.Programs[0].Entries[0].SetpointC != 16 {
		t.Errorf("the away program reads back as mode=%q %v, want away at 16 C", read.Mode, read.Programs)
	}
	if code := getJSON(t, url+"/api/zones/10/schedule", &read); code != http.StatusNotFound {
		t.Errorf("GET the home program before one exists: code=%d, want 404", code)
	}
	// The away program is never served: the member holds no document.
	wantServed := func(want bool, step string) {
		t.Helper()
		v, err := d.do(func(e *tado.EmuState) (any, error) {
			_, set, err := e.Schedule(bedroom)
			return set, err
		})
		if err != nil {
			t.Fatal(err)
		}
		if set := v.(bool); set != want {
			t.Errorf("%s: the bedroom %s a served document", step, map[bool]string{true: "holds", false: "holds no"}[!set])
		}
	}
	wantServed(false, "after the away PUT")

	if code := postJSON(t, http.MethodDelete, url+"/api/zones/10/schedule?mode=away", nil, &view); code != http.StatusOK {
		t.Fatalf("DELETE the away program: code=%d %v", code, view)
	}
	if code := getJSON(t, url+"/api/zones/10/schedule?mode=away", &read); code != http.StatusNotFound {
		t.Errorf("GET ?mode=away after the DELETE: code=%d, want 404", code)
	}

	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 20.0}), &view); code != http.StatusOK {
		t.Fatalf("PUT the home program: code=%d %v", code, view)
	}
	dropQueue(t, d, bedroom)
	wantServed(true, "after the home PUT")
	if code := postJSON(t, http.MethodDelete, url+"/api/zones/10/schedule", nil, &view); code != http.StatusOK {
		t.Fatalf("DELETE the home program: code=%d %v", code, view)
	}
	if code := getJSON(t, url+"/api/zones/10/schedule", &read); code != http.StatusNotFound {
		t.Errorf("GET after the DELETE: code=%d, want 404", code)
	}
	wantServed(false, "after the DELETE")
	tickHome(t, d)
	if n := pending(t, d, bedroom); n != 0 {
		t.Errorf("a zone with no program took %d writes on a tick, want none", n)
	}
	if code := postJSON(t, http.MethodDelete, url+"/api/zones/10/schedule", nil, &view); code != http.StatusNotFound {
		t.Errorf("DELETE the home program again: code=%d, want 404", code)
	}
	if code := postJSON(t, http.MethodDelete, url+"/api/zones/10/schedule?mode=away", nil, &view); code != http.StatusNotFound {
		t.Errorf("DELETE the away program again: code=%d, want 404", code)
	}
}

// TestPresetStoreNameAndMissingErrors: the preset store maps the unique
// violation of a name, in a create or a rename, to errPresetNameTaken, and a
// rename or a delete of an unknown id to sql.ErrNoRows, so the routes answer
// 409 and 404 instead of 500 and a success.
func TestPresetStoreNameAndMissingErrors(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.CreatePreset(ctx, "Default", 0); err != nil {
		t.Fatal(err)
	}
	copied, err := store.CreatePreset(ctx, "Winter", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePreset(ctx, "default", 0); !errors.Is(err, errPresetNameTaken) {
		t.Errorf("create a taken name: err=%v, want errPresetNameTaken", err)
	}
	if err := store.RenamePreset(ctx, copied, "DEFAULT"); !errors.Is(err, errPresetNameTaken) {
		t.Errorf("rename to a taken name: err=%v, want errPresetNameTaken", err)
	}
	if err := store.RenamePreset(ctx, copied, "WINTER"); err != nil {
		t.Errorf("rename to its own name in another case: err=%v, want nil", err)
	}
	if err := store.RenamePreset(ctx, 4242, "x"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("rename an unknown id: err=%v, want sql.ErrNoRows", err)
	}
	if err := store.DeletePreset(ctx, 4242); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("delete an unknown id: err=%v, want sql.ErrNoRows", err)
	}
	if err := store.DeletePreset(ctx, copied); err != nil {
		t.Errorf("delete a stored preset: err=%v, want nil", err)
	}
}

// TestPresetMigrationIsAtomic: the first start with the program store writes
// the Default preset, its programs and the active id in one transaction. A
// write that fails after the preset row leaves no preset behind, and the next
// start migrates the programs again.
func TestPresetMigrationIsAtomic(t *testing.T) {
	in := presetInstall(t, nil)
	store := openTestStore(t)
	officePrograms := weekPrograms(tado.ScheduleEntry{Minute: 0, SetpointCenti: 1900, Enabled: true})
	start := func() (*daemon, error) {
		emu := tado.NewEmu(in.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
		if err := emu.SetSchedule(wireOf(t, in, "VA1000000002"), officePrograms); err != nil {
			t.Fatal(err)
		}
		d := newDaemon(in, emu, radio.NewMock(nil), true, "mock")
		d.state = store
		return d, d.restorePresets(context.Background(), emu)
	}
	failWrites(t, store, "zone_program", "")
	if _, err := start(); err == nil {
		t.Fatal("the start with a failing program write succeeded")
	}
	var presets int
	if err := store.db.QueryRow(`SELECT count(*) FROM preset`).Scan(&presets); err != nil {
		t.Fatal(err)
	}
	if presets != 0 {
		t.Errorf("a failed migration left %d presets, want none", presets)
	}
	if _, found, err := install.ReadMetaUint(context.Background(), store.db, "active_preset"); err != nil || found {
		t.Errorf("a failed migration left an active preset: found=%v err=%v", found, err)
	}

	if _, err := store.db.Exec(`DROP TRIGGER fail_zone_program`); err != nil {
		t.Fatal(err)
	}
	d, err := start()
	if err != nil {
		t.Fatalf("the second start: %v", err)
	}
	if home, held := d.zonePrograms(3); !held || !reflect.DeepEqual(home, officePrograms) {
		t.Errorf("the office home program after the second start is %v (held %v), want the served document", home, held)
	}
	st, err := store.PresetState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 || st.rows[0].name != "Default" || !st.activeFound || st.active != st.rows[0].id {
		t.Errorf("stored presets %+v active %d (found %v), want Default alone and active", st.rows, st.active, st.activeFound)
	}
}

// TestPresetStateSkipsBrokenPrograms: a stored program that does not decode or
// breaks the week rules is skipped with a log line, not a failed start, and a
// database with presets and no stored active id starts on the first preset.
func TestPresetStateSkipsBrokenPrograms(t *testing.T) {
	path := t.TempDir() + "/state.db"
	in := presetInstall(t, nil)
	store := newTestStore(t, path)
	ctx := context.Background()
	officePrograms := weekPrograms(tado.ScheduleEntry{Minute: 0, SetpointCenti: 1900, Enabled: true})
	if err := store.MigrateDefaultPreset(ctx, "Default", map[int][]tado.ScheduleProgram{3: officePrograms}); err != nil {
		t.Fatal(err)
	}
	mondayOnly := tado.EncodeSchedulePrograms([]tado.ScheduleProgram{{DayMask: 0x01,
		Entries: []tado.ScheduleEntry{{Minute: 0, SetpointCenti: 2000, Enabled: true}}}})
	for _, row := range []struct {
		zone int
		mode string
		body []byte
	}{{10, "home", mondayOnly}, {10, "away", []byte{0xff}}} {
		if _, err := store.db.Exec(`INSERT INTO zone_program (preset, zone, mode, body) VALUES (1, ?, ?, ?)`,
			row.zone, row.mode, row.body); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`DELETE FROM meta WHERE key = 'active_preset'`); err != nil {
		t.Fatal(err)
	}

	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url, _ := startStoredDaemon(t, in, clock, store)
	if id, name := presetHome(t, url); id != 1 || name != "Default" {
		t.Errorf("the active preset is (%d, %q), want (1, Default)", id, name)
	}
	if active, found, err := install.ReadMetaUint(ctx, store.db, "active_preset"); err != nil || !found || active != 1 {
		t.Errorf("stored active preset %d (found %v, err %v), want 1", active, found, err)
	}
	got := engineValue(t, d, func() []bool {
		_, home := d.zonePrograms(10)
		_, away := d.awayProgramOf(10)
		_, office := d.zonePrograms(3)
		return []bool{home, away, office}
	})
	if got[0] || got[1] || !got[2] {
		t.Errorf("bedroom home %v away %v office %v, want the broken bedroom programs skipped and the office one kept", got[0], got[1], got[2])
	}
}

// TestHotWaterProgramMovesInWithTheZone: the hot-water program an older daemon
// kept in meta waits there while the installation declares no hot-water zone,
// and moves into every preset once one is declared, at runtime or at the next
// start, and leaves meta.
func TestHotWaterProgramMovesInWithTheZone(t *testing.T) {
	path := t.TempDir() + "/state.db"
	full := presetInstall(t, nil)
	noHotWater := presetInstall(t, func(in *tado.Install) {
		in.Zones = slices.DeleteFunc(in.Zones, func(z tado.Zone) bool { return z.Kind == tado.ZoneHotWater })
	})
	if err := noHotWater.Validate(); err != nil {
		t.Fatal(err)
	}
	hwPrograms := weekPrograms(tado.ScheduleEntry{Minute: 0, SetpointCenti: 4500, Enabled: true})
	store := newTestStore(t, path)
	if err := store.SetHotWaterProgram(context.Background(), hwPrograms); err != nil {
		t.Fatal(err)
	}
	inMeta := func() bool {
		t.Helper()
		var n int
		if err := store.db.QueryRow(`SELECT count(*) FROM meta WHERE key = 'hot_water_program'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	homeOf := func(d *daemon, preset int64) []tado.ScheduleProgram {
		return engineValue(t, d, func() []tado.ScheduleProgram { return d.presetZonePrograms(preset, 0).home })
	}

	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url, stop := startStoredDaemon(t, noHotWater, clock, store)
	if !inMeta() {
		t.Fatal("the hot-water program left meta while no hot-water zone exists")
	}
	var created presetView
	if code := postJSON(t, http.MethodPost, url+"/api/presets", map[string]any{"name": "Winter"}, &created); code != http.StatusOK {
		t.Fatalf("create: code=%d %v", code, created)
	}
	if _, err := d.do(func(*tado.EmuState) (any, error) {
		_, _, err := d.setInstall(full)
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	for _, preset := range []int64{1, created.ID} {
		if got := homeOf(d, preset); !reflect.DeepEqual(got, hwPrograms) {
			t.Errorf("preset %d holds the hot-water program %v after the zone appeared, want %v", preset, got, hwPrograms)
		}
	}
	if inMeta() {
		t.Error("the hot-water program stays in meta after it moved in")
	}
	stop()

	d, _, _ = startStoredDaemon(t, full, clock, newTestStore(t, path))
	for _, preset := range []int64{1, created.ID} {
		if got := homeOf(d, preset); !reflect.DeepEqual(got, hwPrograms) {
			t.Errorf("preset %d holds the hot-water program %v after the restart, want %v", preset, got, hwPrograms)
		}
	}
}
