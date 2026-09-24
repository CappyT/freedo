package daemon

// schedule_program_test: the hot-water program the daemon keeps for itself,
// the 61e0 of a disabled program block, and the per-device 02b3 an operator
// can drop again.

import (
	"context"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"freedo/internal/radio"
	"freedo/tado"
)

// hotWaterWeek is a hot-water program for every day: 45 C at night, 55 C from
// 06:00 and a disabled 50 C block from 22:00.
func hotWaterWeek() map[string]any {
	return allDayProgram(
		map[string]any{"minute": 0, "setpoint_c": 45.0},
		map[string]any{"minute": 360, "setpoint_c": 55.0},
		map[string]any{"minute": 1320, "setpoint_c": 50.0, "enabled": false},
	)
}

func tickHome(t *testing.T, d *daemon) {
	t.Helper()
	if _, err := d.do(func(e *tado.EmuState) (any, error) { d.tickHomeState(e); return nil, nil }); err != nil {
		t.Fatal(err)
	}
}

func putHotWaterWeek(t *testing.T, url string) {
	t.Helper()
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/0/schedule", hotWaterWeek(), &view); code != http.StatusOK {
		t.Fatalf("hot-water schedule: code=%d %v", code, view)
	}
}

// TestHotWaterProgramPushesBlockChanges: each block change of the hot-water
// program writes the thermostat's hot-water zone once, in auto, with 6200 and
// 61e0 from the entry in force.
func TestHotWaterProgramPushesBlockChanges(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url := newHomeDaemon(t, clock)
	thermostat := hex.EncodeToString(mustResolve(t, dhwNode).WireEUI())
	putHotWaterWeek(t, url)
	tickHome(t, d)
	dropQueue(t, d, thermostat)

	clock.advance(2 * time.Hour)
	tickHome(t, d)
	if n := pending(t, d, thermostat); n != 1 {
		t.Fatalf("the 06:00 block queued %d writes, want 1", n)
	}
	wantTags(t, "the 06:00 block", nextZoneWrite(t, d, thermostat), map[string]string{
		"6020": "02", "6160": "01", "61e0": "01", "6200": "157c", "6240": "00",
	}, "6260", "6280", "6440")
	dropQueue(t, d, thermostat)

	clock.advance(16 * time.Hour)
	tickHome(t, d)
	wantTags(t, "the disabled 22:00 block", nextZoneWrite(t, d, thermostat), map[string]string{
		"6020": "02", "6160": "01", "61e0": "00", "6200": "1388", "6240": "00",
	}, "6260", "6280", "6440")
}

// TestHotWaterProgramNeverServed: the hot-water program stays with the daemon.
// No device holds a sched document for it, and a setpoint outside the range the
// thermostat clamps hot water to is refused.
func TestHotWaterProgramNeverServed(t *testing.T) {
	d, url := scheduleDaemon(t)
	var stored scheduleView
	if code := postJSON(t, http.MethodPut, url+"/api/zones/0/schedule", hotWaterWeek(), &stored); code != http.StatusOK {
		t.Fatalf("put: code=%d %+v", code, stored)
	}
	var read scheduleView
	if code := getJSON(t, url+"/api/zones/0/schedule", &read); code != http.StatusOK || !reflect.DeepEqual(read, stored) {
		t.Fatalf("get: code=%d %+v, want %+v", code, read, stored)
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.ExportDesiredState().Schedule, nil })
	if err != nil {
		t.Fatal(err)
	}
	if served := v.(map[string]tado.DesiredScheduleDocument); len(served) != 0 {
		t.Errorf("served schedule documents = %d, want none", len(served))
	}
	var missing struct {
		Error string `json:"error"`
	}
	if code := getJSON(t, url+"/api/zones/1/schedule", &missing); code != http.StatusNotFound {
		t.Errorf("the thermostat's heating zone: got %d, want 404", code)
	}

	cold := allDayProgram(map[string]any{"minute": 0, "setpoint_c": 20.0})
	if code := postJSON(t, http.MethodPut, url+"/api/zones/0/schedule", cold, &missing); code != http.StatusBadRequest ||
		!strings.Contains(missing.Error, "hot water") {
		t.Errorf("a 20 C hot-water entry: got %d %q, want 400", code, missing.Error)
	}
}

// TestHotWaterProgramKeepsOverlay: a block change during a hot-water overlay
// restates the overlay and carries the new block in 6200, and the end of the
// overlay returns to that block.
func TestHotWaterProgramKeepsOverlay(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url := newHomeDaemon(t, clock)
	thermostat := hex.EncodeToString(mustResolve(t, dhwNode).WireEUI())
	putHotWaterWeek(t, url)
	tickHome(t, d)
	dropQueue(t, d, thermostat)

	var res map[string]any
	if code := postJSON(t, http.MethodPost, url+"/api/boiler/dhw",
		map[string]any{"tempC": 60.0, "durationSec": 3 * 3600}, &res); code != http.StatusOK {
		t.Fatalf("dhw overlay: code=%d %v", code, res)
	}
	wantTags(t, "the overlay", deliver(t, d, defaultInstall, thermostat), map[string]string{
		"6240": "03", "6280": "1770", "6200": "1194", "61e0": "01", "6440": "0001",
	})

	clock.advance(2 * time.Hour)
	tickHome(t, d)
	wantTags(t, "the block change under the overlay", deliver(t, d, defaultInstall, thermostat), map[string]string{
		"6020": "02", "6240": "03", "6260": "01", "6280": "1770", "6200": "157c", "61e0": "01",
	}, "6440")

	clock.advance(time.Hour)
	if _, err := d.do(func(e *tado.EmuState) (any, error) { return e.ExpireBoosts(d.retryNow()) }); err != nil {
		t.Fatal(err)
	}
	wantTags(t, "the end of the overlay", nextZoneWrite(t, d, thermostat), map[string]string{
		"6020": "02", "6240": "00", "6200": "157c", "61e0": "01", "6440": "0001",
	}, "6260", "6280")
}

// TestHotWaterProgramKeepsOffOverlay: a manual hot-water off survives a block
// change, the disabled one included.
func TestHotWaterProgramKeepsOffOverlay(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url := newHomeDaemon(t, clock)
	thermostat := hex.EncodeToString(mustResolve(t, dhwNode).WireEUI())
	putHotWaterWeek(t, url)
	tickHome(t, d)
	dropQueue(t, d, thermostat)

	var res map[string]any
	if code := postJSON(t, http.MethodPost, url+"/api/boiler/dhw", map[string]any{"on": false}, &res); code != http.StatusOK {
		t.Fatalf("dhw off: code=%d %v", code, res)
	}
	deliver(t, d, defaultInstall, thermostat)

	clock.advance(18 * time.Hour)
	tickHome(t, d)
	wantTags(t, "the disabled block under a manual off", nextZoneWrite(t, d, thermostat), map[string]string{
		"6240": "02", "6260": "00", "6200": "1388", "61e0": "00",
	}, "6280", "6440")
}

// TestHotWaterProgramAwayUnchanged: away, a hot-water block change writes
// nothing; the return home writes the block then in force.
func TestHotWaterProgramAwayUnchanged(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url := newHomeDaemon(t, clock)
	thermostat := hex.EncodeToString(mustResolve(t, dhwNode).WireEUI())
	putHotWaterWeek(t, url)
	tickHome(t, d)
	var res map[string]any
	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &res); code != http.StatusOK {
		t.Fatalf("away: code=%d %v", code, res)
	}
	dropQueue(t, d, thermostat)

	clock.advance(2 * time.Hour)
	tickHome(t, d)
	if n := pending(t, d, thermostat); n != 0 {
		t.Fatalf("away, the 06:00 block queued %d writes, want none", n)
	}

	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": false}, &res); code != http.StatusOK {
		t.Fatalf("home: code=%d %v", code, res)
	}
	wantTags(t, "the return home", nextZoneWrite(t, d, thermostat), map[string]string{
		"6020": "02", "6160": "01", "61e0": "01", "6200": "157c",
	}, "6440")
}

// TestDisabledRoomBlockPowersOff: a disabled block of a room program writes
// 61e0=0 with no 6200, as captured, and the next enabled block 61e0=1 with
// its setpoint in 6200.
func TestDisabledRoomBlockPowersOff(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url := newHomeDaemon(t, clock)
	target := hex.EncodeToString(mustResolve(t, "VA1000000005").WireEUI())
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule", allDayProgram(
		map[string]any{"minute": 0, "setpoint_c": 20.0},
		map[string]any{"minute": 360, "setpoint_c": 18.0, "enabled": false},
		map[string]any{"minute": 1080, "setpoint_c": 21.0},
	), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	tickHome(t, d)
	dropQueue(t, d, target)

	clock.advance(2 * time.Hour)
	tickHome(t, d)
	wantTags(t, "the disabled block", nextZoneWrite(t, d, target), map[string]string{
		"6160": "01", "6180": "00", "6020": "01", "61e0": "00", "6240": "00", "62e0": "00",
	}, "6200", "6260", "6280", "6440")
	dropQueue(t, d, target)

	clock.advance(12 * time.Hour)
	tickHome(t, d)
	wantTags(t, "the next enabled block", nextZoneWrite(t, d, target), map[string]string{
		"6160": "01", "61e0": "01", "6200": "0834",
	}, "6440")
}

// TestHomeScheduledOffFlag: GET /api/home flags the zone whose block in force is
// disabled, next to the setpoint that block carries in 6200.
func TestHomeScheduledOffFlag(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url := newHomeDaemon(t, clock)
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule", allDayProgram(
		map[string]any{"minute": 0, "setpoint_c": 20.0},
		map[string]any{"minute": 360, "setpoint_c": 18.0, "enabled": false},
	), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	home := func() (map[string]float64, map[string]bool) {
		t.Helper()
		var h struct {
			Scheduled    map[string]float64 `json:"scheduled"`
			ScheduledOff map[string]bool    `json:"scheduledOff"`
		}
		if code := getJSON(t, url+"/api/home", &h); code != http.StatusOK {
			t.Fatalf("GET /api/home: status %d", code)
		}
		return h.Scheduled, h.ScheduledOff
	}
	tickHome(t, d)
	if sched, off := home(); sched["10"] != 20 || off["10"] {
		t.Errorf("the enabled block: scheduled %v off %v, want 20 and not off", sched["10"], off["10"])
	}
	clock.advance(2 * time.Hour)
	tickHome(t, d)
	if sched, off := home(); sched["10"] != 18 || !off["10"] {
		t.Errorf("the disabled block: scheduled %v off %v, want 18 and off", sched["10"], off["10"])
	}
}

// TestPurgeHotWaterScheduleLeftover: the start drops the hot-water program an
// earlier version stored as the thermostat's sched document, logs it and saves
// the state without it.
func TestPurgeHotWaterScheduleLeftover(t *testing.T) {
	var logs strings.Builder
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	in := windowInstall(t, nil)
	emu := tado.NewEmu(in.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if err := emu.SetSchedule("RU1000000001", []tado.ScheduleProgram{{DayMask: 0x7f, Entries: []tado.ScheduleEntry{
		{Minute: 0, SetpointCenti: 4500, Enabled: true},
		{Minute: 360, SetpointCenti: 5500, Enabled: true},
	}}}); err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t, t.TempDir()+"/state.db")
	if err := store.Save(context.Background(), emu.ExportDesiredState(), 3); err != nil {
		t.Fatal(err)
	}
	d := newDaemon(in, emu, radio.NewMock(nil), true, "mock")
	if err := d.attachState(store); err != nil {
		t.Fatal(err)
	}
	if err := d.purgeHotWaterSchedules(emu); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "dropped the sched document of RU1000000001") {
		t.Errorf("log misses the drop:\n%s", logs.String())
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if key := hex.EncodeToString(mustResolve(t, "RU1000000001").WireEUI()); len(saved.Schedule[key].Body) != 0 {
		t.Error("the stored state still holds the thermostat's sched document")
	}
}

// TestHotWaterProgramSurvivesStoreReload: the program is durable and a fresh
// database holds none.
func TestHotWaterProgramSurvivesStoreReload(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if programs, err := store.HotWaterProgram(context.Background()); err != nil || programs != nil {
		t.Fatalf("a fresh database reads %v err=%v, want none", programs, err)
	}
	want := []tado.ScheduleProgram{{DayMask: 0x7f, Entries: []tado.ScheduleEntry{
		{Minute: 0, SetpointCenti: 4500, Enabled: true},
		{Minute: 1320, SetpointCenti: 5000, Enabled: false},
	}}}
	if err := store.SetHotWaterProgram(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.HotWaterProgram(context.Background())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("reloaded %+v err=%v, want %+v", got, err, want)
	}
}

// TestClearOfflineSchedule: dropping a per-device 02b3 makes the device follow
// options.offline_schedule again. It writes the valve once when its value
// moves and not at all when it stays.
func TestClearOfflineSchedule(t *testing.T) {
	d, url := scheduleDaemon(t)
	valve := hex.EncodeToString(mustResolve(t, "VA1000000005").WireEUI())
	served := func() (byte, bool) {
		t.Helper()
		v, err := d.do(func(e *tado.EmuState) (any, error) {
			state := e.ExportDesiredState()
			_, own := state.OfflineSchedule[valve]
			for _, tlv := range tado.ParseTLVs(state.Config[valve].Body) {
				if tlv.Tag == "02b3" && len(tlv.Val) == 1 {
					return [2]any{tlv.Val[0], own}, nil
				}
			}
			return [2]any{byte(0xff), own}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		pair := v.([2]any)
		return pair[0].(byte), pair[1].(bool)
	}
	type answer struct {
		Queued  bool   `json:"queued"`
		Pending int    `json:"pending"`
		Error   string `json:"error"`
	}
	clearOwn := func(what string, wantQueued bool) {
		t.Helper()
		var res answer
		code := postJSON(t, http.MethodDelete, url+"/api/nodes/VA1000000005/config/offlineScheduleEnabled", nil, &res)
		if code != http.StatusOK || res.Queued != wantQueued {
			t.Fatalf("%s: code=%d %+v, want queued=%t", what, code, res, wantQueued)
		}
		if value, own := served(); value != 1 || own {
			t.Errorf("%s: 02b3=%d own=%t, want the installation's 1 and no own value", what, value, own)
		}
	}
	set := func(on bool) {
		t.Helper()
		var res answer
		if code := postJSON(t, http.MethodPost, url+"/api/nodes/VA1000000005/config",
			map[string]any{"offlineScheduleEnabled": on}, &res); code != http.StatusOK {
			t.Fatalf("set 02b3=%t: code=%d %+v", on, code, res)
		}
		dropQueue(t, d, valve)
	}

	clearOwn("no own value", false)

	set(false)
	if value, own := served(); value != 0 || !own {
		t.Fatalf("after the write: 02b3=%d own=%t, want 0 and an own value", value, own)
	}
	clearOwn("an own value that moves", true)
	if n := pending(t, d, valve); n != 1 {
		t.Fatalf("the clear queued %d writes, want 1", n)
	}
	dropQueue(t, d, valve)

	set(true)
	clearOwn("an own value equal to the installation", false)
	if n := pending(t, d, valve); n != 0 {
		t.Errorf("the clear of an unchanged value queued %d writes, want none", n)
	}
}
