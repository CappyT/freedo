package daemon

// preset_test: the program presets: the migration of the first start, the
// away program a zone follows in away, the create, rename, delete and
// activation routes, the restart guarantees across presets and the schedule
// routes with mode and preset.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
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

// TestPresetSchedulesRoute: GET /api/presets/{id}/schedules answers every
// declared zone of one preset with its home and away programs, null for a
// mode without one, and the block the preset holds now in the current home
// state.
func TestPresetSchedulesRoute(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	_, url := newPresetDaemon(t, clock)
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule", allDayProgram(
		map[string]any{"minute": 0, "setpoint_c": 18.0},
		map[string]any{"minute": 360, "setpoint_c": 21.0}), &view); code != http.StatusOK {
		t.Fatalf("bedroom schedule: code=%d %v", code, view)
	}
	var created presetView
	if code := postJSON(t, http.MethodPost, url+"/api/presets", map[string]any{"name": "Summer"}, &created); code != http.StatusOK {
		t.Fatalf("create: code=%d %v", code, created)
	}
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule?preset=2&mode=away",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 15.0, "enabled": false}), &view); code != http.StatusOK {
		t.Fatalf("away schedule of the Summer preset: code=%d %v", code, view)
	}

	read := func(id string) presetSchedulesView {
		t.Helper()
		var out presetSchedulesView
		if code := getJSON(t, url+"/api/presets/"+id+"/schedules", &out); code != http.StatusOK {
			t.Fatalf("GET /api/presets/%s/schedules: code=%d", id, code)
		}
		return out
	}
	zoneOf := func(v presetSchedulesView, n int) presetZoneSchedules {
		t.Helper()
		for _, z := range v.Zones {
			if z.Zone == n {
				return z
			}
		}
		t.Fatalf("zone %d missing from %+v", n, v.Zones)
		return presetZoneSchedules{}
	}

	def := read("1")
	if !def.Preset.Active || def.Preset.Name != "Default" || len(def.Zones) != 3 {
		t.Fatalf("Default reads %+v, want the active preset with the three zones", def)
	}
	bedroom := zoneOf(def, 10)
	if bedroom.Name != "bedroom" || bedroom.Kind != tado.ZoneHeating || len(bedroom.Home) != 1 || bedroom.Away != nil {
		t.Errorf("bedroom of Default=%+v, want one home program and no away program", bedroom)
	}
	if bedroom.InForce == nil || *bedroom.InForce != (presetBlockView{Mode: "home", SetpointC: 18}) {
		t.Errorf("bedroom in force=%+v, want the 18 C home block of 05:00", bedroom.InForce)
	}
	if office := zoneOf(def, 3); office.Home != nil || office.InForce != nil {
		t.Errorf("office of Default=%+v, want no program and nothing in force", office)
	}

	summer := zoneOf(read("2"), 10)
	if summer.Away == nil || summer.Away[0].Entries[0].Enabled {
		t.Errorf("bedroom of Summer=%+v, want the copied home program and the off away program", summer)
	}

	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &view); code != http.StatusOK {
		t.Fatalf("away: code=%d %v", code, view)
	}
	if in := zoneOf(read("1"), 10).InForce; in != nil {
		t.Errorf("in away, Default holds %+v for the bedroom, want nothing: the away fallback applies", in)
	}
	if in := zoneOf(read("2"), 10).InForce; in == nil || *in != (presetBlockView{Mode: "away", SetpointC: 15, Off: true}) {
		t.Errorf("in away, Summer holds %+v for the bedroom, want its off away block", in)
	}

	if code := getJSON(t, url+"/api/presets/9/schedules", &view); code != http.StatusNotFound {
		t.Errorf("GET an unknown preset: code=%d, want 404", code)
	}
	if code := getJSON(t, url+"/api/presets/x/schedules", &view); code != http.StatusBadRequest {
		t.Errorf("GET a preset id that is not a number: code=%d, want 400", code)
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

// TestAwayTargetMovesWithoutBlockChange: in away the write a zone takes
// depends on its away 6200, not on its block alone. A change that keeps the
// block and moves the away value, an away program put, deleted or left behind
// by an activation, writes the zone once with the new away head.
func TestAwayTargetMovesWithoutBlockChange(t *testing.T) {
	put := func(t *testing.T, url, path string, body map[string]any) {
		t.Helper()
		var view map[string]any
		if code := postJSON(t, http.MethodPut, url+path, body, &view); code != http.StatusOK {
			t.Fatalf("PUT %s: code=%d %v", path, code, view)
		}
	}
	send := func(t *testing.T, method, url string) {
		t.Helper()
		var view map[string]any
		if code := postJSON(t, method, url, nil, &view); code != http.StatusOK {
			t.Fatalf("%s %s: code=%d %v", method, url, code, view)
		}
	}
	goAway := func(t *testing.T, d *daemon, url string, targets ...string) {
		t.Helper()
		var view map[string]any
		if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &view); code != http.StatusOK {
			t.Fatalf("away: code=%d %v", code, view)
		}
		for _, target := range targets {
			dropQueue(t, d, target)
		}
	}
	bed18 := allDayProgram(map[string]any{"minute": 0, "setpoint_c": 18.0})
	in := presetInstall(t, nil)
	bedroom := wireOf(t, in, "VA1000000005")
	thermostat := wireOf(t, in, dhwNode)

	t.Run("away program put with the home value", func(t *testing.T) {
		d, url := newPresetDaemon(t, newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC)))
		put(t, url, "/api/zones/10/schedule", bed18)
		tickHome(t, d)
		goAway(t, d, url, bedroom)
		put(t, url, "/api/zones/10/schedule?mode=away", bed18)
		tickHome(t, d)
		wantTags(t, "the bedroom after its away program", nextZoneWrite(t, d, bedroom), map[string]string{
			"6160": "02", "61e0": "01", "6200": "0708",
		}, "6440")
		dropQueue(t, d, bedroom)
		tickHome(t, d)
		if n := pending(t, d, bedroom); n != 0 {
			t.Errorf("a second tick queued %d more writes, want none", n)
		}
	})

	t.Run("away program deleted with the home value", func(t *testing.T) {
		d, url := newPresetDaemon(t, newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC)))
		put(t, url, "/api/zones/10/schedule", bed18)
		put(t, url, "/api/zones/10/schedule?mode=away", bed18)
		tickHome(t, d)
		goAway(t, d, url, bedroom)
		send(t, http.MethodDelete, url+"/api/zones/10/schedule?mode=away")
		tickHome(t, d)
		wantTags(t, "the bedroom after the away delete", nextZoneWrite(t, d, bedroom), map[string]string{
			"6160": "02", "61e0": "00",
		}, "6200", "6440")
	})

	t.Run("activation drops the away program with the home value", func(t *testing.T) {
		d, url := newPresetDaemon(t, newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC)))
		put(t, url, "/api/zones/10/schedule", bed18)
		var created presetView
		if code := postJSON(t, http.MethodPost, url+"/api/presets", map[string]any{"name": "NoAway"}, &created); code != http.StatusOK {
			t.Fatalf("create: code=%d %v", code, created)
		}
		put(t, url, "/api/zones/10/schedule?mode=away", bed18)
		tickHome(t, d)
		goAway(t, d, url, bedroom)
		send(t, http.MethodPost, url+"/api/presets/2/activate")
		wantTags(t, "the bedroom after the activation", nextZoneWrite(t, d, bedroom), map[string]string{
			"6160": "02", "61e0": "00",
		}, "6200", "6440")
	})

	t.Run("hot water away program deleted", func(t *testing.T) {
		d, url := newPresetDaemon(t, newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC)))
		put(t, url, "/api/zones/0/schedule", allDayProgram(map[string]any{"minute": 0, "setpoint_c": 45.0}))
		put(t, url, "/api/zones/0/schedule?mode=away", allDayProgram(map[string]any{"minute": 0, "setpoint_c": 50.0}))
		tickHome(t, d)
		goAway(t, d, url, thermostat)
		send(t, http.MethodDelete, url+"/api/zones/0/schedule?mode=away")
		tickHome(t, d)
		wantTags(t, "the thermostat after the away delete", nextZoneWrite(t, d, thermostat), map[string]string{
			"6160": "02", "61e0": "00",
		}, "6200", "6440")
		dropQueue(t, d, thermostat)
		tickHome(t, d)
		if n := pending(t, d, thermostat); n != 0 {
			t.Errorf("a second tick queued %d more writes, want none", n)
		}
	})

	t.Run("hot water activation without an away program", func(t *testing.T) {
		d, url := newPresetDaemon(t, newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC)))
		put(t, url, "/api/zones/0/schedule", allDayProgram(map[string]any{"minute": 0, "setpoint_c": 45.0}))
		var created presetView
		if code := postJSON(t, http.MethodPost, url+"/api/presets", map[string]any{"name": "NoAway"}, &created); code != http.StatusOK {
			t.Fatalf("create: code=%d %v", code, created)
		}
		put(t, url, "/api/zones/0/schedule?mode=away", allDayProgram(map[string]any{"minute": 0, "setpoint_c": 50.0}))
		tickHome(t, d)
		goAway(t, d, url, thermostat)
		send(t, http.MethodPost, url+"/api/presets/2/activate")
		wantTags(t, "the thermostat after the activation", nextZoneWrite(t, d, thermostat), map[string]string{
			"6160": "02", "61e0": "00",
		}, "6200", "6440")
	})
}

// TestAwayTargetSurvivesRestart: the away 6200 each zone took is stored with
// its block, so an away program that appeared while the daemon was down
// writes the zone on the first tick. Blocks stored without it, as a daemon
// before the away programs left them, only seed it.
func TestAwayTargetSurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/state.db"
	in := presetInstall(t, nil)
	bedroom := wireOf(t, in, "VA1000000005")
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url, stop := startStoredDaemon(t, in, clock, newTestStore(t, path))
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 18.0}), &view); code != http.StatusOK {
		t.Fatalf("bedroom schedule: code=%d %v", code, view)
	}
	tickHome(t, d)
	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &view); code != http.StatusOK {
		t.Fatalf("away: code=%d %v", code, view)
	}
	deliverQueue(t, d, in, bedroom)
	tickHome(t, d)
	stop()

	store := newTestStore(t, path)
	if err := store.SetZoneProgram(context.Background(), 1, 10, true,
		weekPrograms(tado.ScheduleEntry{Minute: 0, SetpointCenti: 1800, Enabled: true})); err != nil {
		t.Fatal(err)
	}
	store.Close()
	d, _, stop = startStoredDaemon(t, in, clock, newTestStore(t, path))
	tickHome(t, d)
	wantTags(t, "the bedroom after the restart", deliver(t, d, in, bedroom), map[string]string{
		"6160": "02", "61e0": "01", "6200": "0708",
	}, "6440")
	tickHome(t, d)
	stop()

	store = newTestStore(t, path)
	if err := store.DeleteZoneProgram(context.Background(), 1, 10, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DELETE FROM meta WHERE key = 'away_targets'`); err != nil {
		t.Fatal(err)
	}
	store.Close()
	d, _, _ = startStoredDaemon(t, in, clock, newTestStore(t, path))
	tickHome(t, d)
	if n := pending(t, d, bedroom); n != 0 {
		t.Errorf("blocks stored with no away 6200 queued %d writes on the first tick, want none", n)
	}
}

// TestScheduleFailedSaveServesNothing: a home program whose save fails answers
// 500 and reaches neither the engine nor the members: no served document and
// no fetch trigger.
func TestScheduleFailedSaveServesNothing(t *testing.T) {
	in := presetInstall(t, nil)
	store := openTestStore(t)
	d, url, _ := startStoredDaemon(t, in, newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC)), store)
	bedroom := wireOf(t, in, "VA1000000005")
	failWrites(t, store, "zone_program", "")
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 21.0}), &view); code != http.StatusInternalServerError {
		t.Fatalf("PUT with a failing save: code=%d %v, want 500", code, view)
	}
	got := engineValue(t, d, func() []bool {
		_, served, _ := d.emu.Schedule(bedroom)
		_, held := d.zonePrograms(10)
		return []bool{served, held}
	})
	if got[0] || got[1] {
		t.Errorf("after the failed save the bedroom is served a document: %v, the engine holds the program: %v, want neither", got[0], got[1])
	}
	if n := pending(t, d, bedroom); n != 0 {
		t.Errorf("the failed save queued %d writes on the bedroom, want none", n)
	}
	var read scheduleView
	if code := getJSON(t, url+"/api/zones/10/schedule", &read); code != http.StatusNotFound {
		t.Errorf("GET after the failed save: code=%d %+v, want 404", code, read.Programs)
	}
}

// TestPresetActivationPartialServe: a member that refuses its document leaves
// the switch in place. The answer is a 500 that names the active preset, the
// active id is stored, and every other member and zone still takes its
// document and its write.
func TestPresetActivationPartialServe(t *testing.T) {
	in := presetInstall(t, nil)
	store := openTestStore(t)
	d, url, _ := startStoredDaemon(t, in, newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC)), store)
	bedroom := wireOf(t, in, "VA1000000005")
	var view map[string]any
	put := func(path string, c float64) {
		t.Helper()
		if code := postJSON(t, http.MethodPut, url+path, allDayProgram(map[string]any{"minute": 0, "setpoint_c": c}), &view); code != http.StatusOK {
			t.Fatalf("PUT %s: code=%d %v", path, code, view)
		}
	}
	put("/api/zones/10/schedule", 20)
	tickHome(t, d)
	dropQueue(t, d, bedroom)
	var created presetView
	if code := postJSON(t, http.MethodPost, url+"/api/presets", map[string]any{"name": "Summer"}, &created); code != http.StatusOK {
		t.Fatalf("create: code=%d %v", code, created)
	}
	put("/api/zones/10/schedule?preset=2", 22)
	put("/api/zones/3/schedule?preset=2", 19)
	// A program the emulator refuses: the office member fails its document.
	engineValue(t, d, func() bool {
		zp := d.presets[2].programs[3]
		zp.home = []tado.ScheduleProgram{{DayMask: 0x01, Entries: []tado.ScheduleEntry{{Minute: 0, SetpointCenti: 1900, Enabled: true}}}}
		d.presets[2].programs[3] = zp
		return true
	})

	var answer struct {
		Error string `json:"error"`
	}
	if code := postJSON(t, http.MethodPost, url+"/api/presets/2/activate", nil, &answer); code != http.StatusInternalServerError ||
		!strings.Contains(answer.Error, "is active") {
		t.Errorf("activate with a refused document: code=%d %q, want 500 naming the active preset", code, answer.Error)
	}
	if id, _ := presetHome(t, url); id != 2 {
		t.Errorf("the active preset is %d, want 2", id)
	}
	if active, _, err := install.ReadMetaUint(context.Background(), store.db, "active_preset"); err != nil || active != 2 {
		t.Errorf("the stored active preset is %d (err %v), want 2", active, err)
	}
	n, head := takeQueue(t, d, "VA1000000005")
	if n != 2 || head == nil || head.URI() != "d/sched" {
		t.Errorf("the bedroom holds %d writes headed by %v, want the fetch trigger and one block write", n, head)
	}
	if block := engineValue(t, d, func() scheduledBlock { return d.schedPushed[10] }); block != (scheduledBlock{c: 22}) {
		t.Errorf("the bedroom follows %v, want the 22 C block", block)
	}
}

// TestPresetServedDocumentsFollowTheStore: the served documents follow the
// home programs of the active preset. A start serves them again, so a program
// stored and not served before a crash reaches the members, and a document
// with no program behind it goes; a GET of the home program reads the store,
// not a member; an installation change serves a member that moved zones.
func TestPresetServedDocumentsFollowTheStore(t *testing.T) {
	path := t.TempDir() + "/state.db"
	in := presetInstall(t, nil)
	bedroom := wireOf(t, in, "VA1000000005")
	office := wireOf(t, in, "VA1000000002")
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url, stop := startStoredDaemon(t, in, clock, newTestStore(t, path))
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 20.0}), &view); code != http.StatusOK {
		t.Fatalf("bedroom schedule: code=%d %v", code, view)
	}
	setDoc := func(d *daemon, target string, c int) {
		t.Helper()
		if _, err := d.do(func(e *tado.EmuState) (any, error) {
			return nil, e.SetSchedule(target, weekPrograms(tado.ScheduleEntry{Minute: 0, SetpointCenti: c, Enabled: true}))
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A document no program explains, as a crash between two writes leaves it.
	setDoc(d, office, 1700)
	if err := d.persist(nil); err != nil {
		t.Fatal(err)
	}
	var read scheduleView
	if code := getJSON(t, url+"/api/zones/3/schedule", &read); code != http.StatusNotFound {
		t.Errorf("GET the office home program with no program stored: code=%d %+v, want 404", code, read.Programs)
	}
	stop()

	// A program stored and not served before a crash.
	twentyTwo := weekPrograms(tado.ScheduleEntry{Minute: 0, SetpointCenti: 2200, Enabled: true})
	store := newTestStore(t, path)
	if err := store.SetZoneProgram(context.Background(), 1, 10, false, twentyTwo); err != nil {
		t.Fatal(err)
	}
	store.Close()

	d, url, stop = startStoredDaemon(t, in, clock, newTestStore(t, path))
	served := func(target string) []tado.ScheduleProgram {
		return engineValue(t, d, func() []tado.ScheduleProgram {
			p, _, _ := d.emu.Schedule(target)
			return p
		})
	}
	if got := served(bedroom); !reflect.DeepEqual(got, twentyTwo) {
		t.Errorf("the bedroom is served %v after the start, want the stored 22 C program", got)
	}
	if n, head := takeQueue(t, d, "VA1000000005"); n != 1 || head == nil || head.URI() != "d/sched" {
		t.Errorf("the bedroom holds %d writes headed by %v after the start, want one fetch trigger", n, head)
	}
	if got := served(office); got != nil {
		t.Errorf("the office is served %v after the start, want no document: zone 3 has no home program", got)
	}
	stop()
	saved, err := newTestStore(t, path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if body := saved.Schedule[bedroom].Body; !bytes.Equal(body, tado.EncodeSchedulePrograms(twentyTwo)) {
		t.Errorf("the stored bedroom document is %x, want the 22 C program", body)
	}
	if _, held := saved.Schedule[office]; held {
		t.Error("the stored state keeps the office document")
	}

	d, url, _ = startStoredDaemon(t, in, clock, newTestStore(t, path))
	if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 19.0}), &view); code != http.StatusOK {
		t.Fatalf("office schedule: code=%d %v", code, view)
	}
	setDoc(d, bedroom, 1500)
	if code := getJSON(t, url+"/api/zones/10/schedule", &read); code != http.StatusOK || read.Programs[0].Entries[0].SetpointC != 22 {
		t.Errorf("GET the bedroom home program: code=%d %+v, want the stored 22 C program", code, read.Programs)
	}

	// The two valves swap zones: no member stays to hand its document over.
	swapped := presetInstall(t, func(in *tado.Install) {
		for i := range in.Zones {
			switch in.Zones[i].Number {
			case 3:
				in.Zones[i].Members, in.Zones[i].Reporter = []string{"VA1000000005"}, []string{"VA1000000005"}
			case 10:
				in.Zones[i].Members, in.Zones[i].Reporter = []string{"VA1000000002"}, []string{"VA1000000002"}
			}
		}
	})
	if _, err := d.do(func(*tado.EmuState) (any, error) {
		_, _, err := d.setInstall(swapped)
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	served = func(target string) []tado.ScheduleProgram {
		return engineValue(t, d, func() []tado.ScheduleProgram {
			p, _, _ := d.emu.Schedule(target)
			return p
		})
	}
	if got := served(bedroom); len(got) != 1 || got[0].Entries[0].SetpointCenti != 1900 {
		t.Errorf("the bedroom valve in zone 3 is served %v, want the 19 C office program", got)
	}
	if got := served(office); !reflect.DeepEqual(got, twentyTwo) {
		t.Errorf("the office valve in zone 10 is served %v, want the 22 C bedroom program", got)
	}
}

// TestDeletedZoneLeavesNoPrograms: a zone delete drops the programs of the
// zone from every preset, in the database and in the engine, and a kind
// change drops them too: a zone later declared under that number, or with the
// other kind, starts with no program.
func TestDeletedZoneLeavesNoPrograms(t *testing.T) {
	a := newInstallAPIOn(t, presetInstall(t, nil))
	rows := func(zone int) int {
		t.Helper()
		var n int
		if err := a.store.db.QueryRow(`SELECT count(*) FROM zone_program WHERE zone = ?`, zone).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	held := func(zone int) bool {
		return engineValue(t, a.d, func() bool {
			for _, p := range a.d.presets {
				if zp := p.programs[zone]; zp.home != nil || zp.away != nil {
					return true
				}
			}
			return false
		})
	}
	for _, path := range []string{"/api/zones/10/schedule", "/api/zones/10/schedule?mode=away"} {
		if code, res := a.call(http.MethodPut, path, allDayProgram(map[string]any{"minute": 0, "setpoint_c": 18.0})); code != http.StatusOK {
			t.Fatalf("PUT %s: code=%d %v", path, code, res)
		}
	}
	if code, res := a.call(http.MethodPost, "/api/presets", map[string]any{"name": "Copy"}); code != http.StatusOK {
		t.Fatalf("create: code=%d %v", code, res)
	}
	if code, res := a.call(http.MethodPut, "/api/zones/0/schedule", allDayProgram(map[string]any{"minute": 0, "setpoint_c": 45.0})); code != http.StatusOK {
		t.Fatalf("PUT the hot-water schedule: code=%d %v", code, res)
	}
	if rows(10) != 4 {
		t.Fatalf("zone 10 holds %d stored programs before the delete, want 4", rows(10))
	}

	a.mutate(http.MethodDelete, "/api/install/zones/10", nil)
	if n := rows(10); n != 0 {
		t.Errorf("the deleted zone keeps %d stored programs, want none", n)
	}
	if held(10) {
		t.Error("a preset keeps a program of the deleted zone")
	}
	a.mutate(http.MethodPost, "/api/install/zones", map[string]any{"number": 10, "name": "bedroom", "kind": "heating", "circuit": 1})
	a.mutate(http.MethodPut, "/api/install/zones/10/members", map[string]any{"members": []string{"VA1000000005"}})
	if code, res := a.call(http.MethodGet, "/api/zones/10/schedule", nil); code != http.StatusNotFound {
		t.Errorf("GET the schedule of the new zone 10: code=%d %v, want 404", code, res)
	}

	a.mutate(http.MethodPatch, "/api/install/zones/0", map[string]any{"kind": "heating", "circuit": 1})
	if n := rows(0); n != 0 {
		t.Errorf("zone 0 keeps %d stored programs after its kind changed, want none", n)
	}
	if held(0) {
		t.Error("a preset keeps the hot-water program on the zone that became a heating zone")
	}
}

// TestReplaceInstallDropsProgramsOfGoneZones: a seed or import that drops a
// zone number, or declares it with another kind, drops its programs in the
// same transaction; the programs of every zone it keeps stay.
func TestReplaceInstallDropsProgramsOfGoneZones(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.ReplaceInstall(ctx, presetInstall(t, nil)); err != nil {
		t.Fatal(err)
	}
	week := weekPrograms(tado.ScheduleEntry{Minute: 0, SetpointCenti: 2000, Enabled: true})
	if err := store.MigrateDefaultPreset(ctx, "Default", map[int][]tado.ScheduleProgram{0: week, 3: week, 10: week, 42: week}); err != nil {
		t.Fatal(err)
	}
	next := presetInstall(t, func(in *tado.Install) {
		in.Zones = slices.DeleteFunc(in.Zones, func(z tado.Zone) bool { return z.Number == 10 })
		for i := range in.Zones {
			if in.Zones[i].Number == 0 {
				circuit := 1
				in.Zones[i].Kind, in.Zones[i].Circuit = tado.ZoneHeating, &circuit
			}
		}
	})
	if _, err := store.ReplaceInstall(ctx, next); err != nil {
		t.Fatal(err)
	}
	var zones []int
	err := store.scan(`SELECT zone FROM zone_program ORDER BY zone`, func(rows *sql.Rows) error {
		var zone int
		if err := rows.Scan(&zone); err != nil {
			return err
		}
		zones = append(zones, zone)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(zones, []int{3}) {
		t.Errorf("zone_program holds zones %v after the replace, want [3]", zones)
	}
}

// TestHasProgramMeansProgramInForce: hasProgram reports the program in force.
// A zone with only an away program has none at home, so a client picks no
// until_next there, which the daemon refuses on hot water, and has one in
// away.
func TestHasProgramMeansProgramInForce(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	_, url := newPresetDaemon(t, clock)
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/0/schedule?mode=away",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 50.0}), &view); code != http.StatusOK {
		t.Fatalf("hot-water away schedule: code=%d %v", code, view)
	}
	if got := mustTarget(t, url, 0)["hasProgram"]; got != false {
		t.Errorf("hasProgram of hot water with only an away program, at home: %v, want false", got)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/zones/0/setpoint",
		map[string]any{"setpointC": 50.0, "mode": "until_next"}, &view); code != http.StatusBadRequest {
		t.Errorf("until_next on hot water with no program in force: code=%d %v, want 400", code, view)
	}
	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &view); code != http.StatusOK {
		t.Fatalf("away: code=%d %v", code, view)
	}
	if got := mustTarget(t, url, 0)["hasProgram"]; got != true {
		t.Errorf("hasProgram of hot water with an away program, in away: %v, want true", got)
	}
}

// TestAwayTemperatureChangeSkipsAwayProgramZones: in away a change of
// away_temperature_c rewrites a zone without an away program at once and
// leaves a zone that follows one alone.
func TestAwayTemperatureChangeSkipsAwayProgramZones(t *testing.T) {
	in := presetInstall(t, nil)
	a := newInstallAPIOn(t, in)
	bedroom := wireOf(t, in, "VA1000000005")
	office := wireOf(t, in, "VA1000000002")
	for path, c := range map[string]float64{
		"/api/zones/10/schedule": 20, "/api/zones/10/schedule?mode=away": 16, "/api/zones/3/schedule": 19,
	} {
		if code, res := a.call(http.MethodPut, path, allDayProgram(map[string]any{"minute": 0, "setpoint_c": c})); code != http.StatusOK {
			t.Fatalf("PUT %s: code=%d %v", path, code, res)
		}
	}
	if code, res := a.call(http.MethodPatch, "/api/home", map[string]any{"away": true}); code != http.StatusOK {
		t.Fatalf("away: code=%d %v", code, res)
	}
	dropQueue(t, a.d, bedroom)
	dropQueue(t, a.d, office)

	a.mutate(http.MethodPatch, "/api/install/zones/10", map[string]any{"away_temperature_c": 14.0})
	a.mutate(http.MethodPatch, "/api/install/zones/3", map[string]any{"away_temperature_c": 15.0})
	if n := pending(t, a.d, bedroom); n != 0 {
		t.Errorf("the bedroom, on its away program, took %d writes on the away temperature change, want none", n)
	}
	wantTags(t, "the office on the away temperature change", nextZoneWrite(t, a.d, office), map[string]string{
		"6160": "02", "61e0": "01", "6200": "05dc",
	})
	dropQueue(t, a.d, office)
	tickHome(t, a.d)
	for _, target := range []string{bedroom, office} {
		if n := pending(t, a.d, target); n != 0 {
			t.Errorf("%s took %d writes on the next tick, want none", target, n)
		}
	}
}

// A wall thermostat that is a member of every heating zone, listed first in a
// zone of valves, holds the document of its own zone. The migration takes the
// zone program from the valve, so the start serves the valves nothing new.
func TestPresetMigrationPrefersASingleZoneMember(t *testing.T) {
	path := t.TempDir() + "/state.db"
	in := presetInstall(t, func(in *tado.Install) {
		for i := range in.Zones {
			if in.Zones[i].Number == 3 {
				in.Zones[i].Members = append([]string{"RU1000000001"}, in.Zones[i].Members...)
			}
		}
		circuit := 1
		in.Zones = append(in.Zones, tado.Zone{Number: 1, Name: "hall", Kind: tado.ZoneHeating, Circuit: &circuit,
			Members: []string{"RU1000000001"}, Reporter: []string{"RU1000000001"}})
	})
	if err := in.Validate(); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	hallPrograms := weekPrograms(tado.ScheduleEntry{Minute: 0, SetpointCenti: 2200, Enabled: true})
	officePrograms := weekPrograms(
		tado.ScheduleEntry{Minute: 0, SetpointCenti: 1900, Enabled: true},
		tado.ScheduleEntry{Minute: 420, SetpointCenti: 1600, Enabled: true},
	)
	emu := tado.NewEmu(in.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	hall, office := wireOf(t, in, "RU1000000001"), wireOf(t, in, "VA1000000002")
	if err := emu.SetSchedule(hall, hallPrograms); err != nil {
		t.Fatal(err)
	}
	if err := emu.SetSchedule(office, officePrograms); err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t, path)
	if err := store.Save(context.Background(), emu.ExportDesiredState(), 1); err != nil {
		t.Fatal(err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	officeETag, hallETag := string(st.Schedule[office].ETag), string(st.Schedule[hall].ETag)

	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, _, _ := startStoredDaemon(t, in, clock, store)
	if got := engineValue(t, d, func() []tado.ScheduleProgram {
		home, _ := d.zonePrograms(3)
		return home
	}); !reflect.DeepEqual(got, officePrograms) {
		t.Errorf("the office home program is %v, want the valve's, not the thermostat's", got)
	}
	if got := engineValue(t, d, func() []tado.ScheduleProgram {
		home, _ := d.zonePrograms(1)
		return home
	}); !reflect.DeepEqual(got, hallPrograms) {
		t.Errorf("the hall home program is %v, want the thermostat's", got)
	}
	for _, c := range []struct{ name, wire, etag string }{{"office", office, officeETag}, {"thermostat", hall, hallETag}} {
		v, err := d.do(func(e *tado.EmuState) (any, error) {
			return string(e.ExportDesiredState().Schedule[c.wire].ETag), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if v.(string) != c.etag {
			t.Errorf("the %s ETag is %q, want %q as before the migration", c.name, v, c.etag)
		}
		if n := pending(t, d, c.wire); n != 0 {
			t.Errorf("the %s took %d writes at start, want none", c.name, n)
		}
	}
}
