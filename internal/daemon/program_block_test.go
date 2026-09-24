package daemon

// program_block_test: a program block change reaches every zone it moves,
// whether a command arrives before the tick that sees it or the daemon was down
// across it.

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"freedo/internal/radio"
	"freedo/tado"
)

// TestHandlerInBoundaryGapKeepsBlockChange: a zone command between the 06:00
// boundary of the hot-water program and the next tick leaves the block change
// owed, and the tick still writes it to the thermostat.
func TestHandlerInBoundaryGapKeepsBlockChange(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url := newHomeDaemon(t, clock)
	thermostat := hex.EncodeToString(mustResolve(t, dhwNode).WireEUI())
	valve := hex.EncodeToString(mustResolve(t, "VA1000000002").WireEUI())
	putHotWaterWeek(t, url)
	tickHome(t, d)
	dropQueue(t, d, thermostat)

	clock.advance(time.Hour)
	var res map[string]any
	if code := postJSON(t, http.MethodPost, url+"/api/zones/3/setpoint",
		map[string]any{"setpointC": 21.0, "mode": "manual"}, &res); code != http.StatusOK {
		t.Fatalf("zone 3 setpoint: code=%d %v", code, res)
	}
	if n := pending(t, d, valve); n != 1 {
		t.Fatalf("zone 3 holds %d queued writes, want 1", n)
	}
	tickHome(t, d)
	if n := pending(t, d, thermostat); n != 1 {
		t.Fatalf("the 06:00 hot-water block queued %d writes, want 1", n)
	}
	wantTags(t, "the 06:00 hot-water block", nextZoneWrite(t, d, thermostat), map[string]string{
		"6020": "02", "61e0": "01", "6200": "157c", "6240": "00",
	}, "6440")
	dropQueue(t, d, thermostat)
	tickHome(t, d)
	if n := pending(t, d, thermostat); n != 0 {
		t.Errorf("a second tick queued %d more writes, want none", n)
	}
	if n := pending(t, d, valve); n != 1 {
		t.Errorf("zone 3 holds %d queued writes after the ticks, want its own 1", n)
	}
}

// startStoredDaemon starts a dry-run daemon on in and store the way Main does,
// with an injected clock, and returns it with a stop function.
func startStoredDaemon(t *testing.T, in *tado.Install, clock *testClock, store *stateStore) (*daemon, string, func()) {
	t.Helper()
	return startDaemonAs(t, in, clock, store, true, "mock", 0)
}

// startDaemonAs is startStoredDaemon with the transmit switch, the radio mode
// and the configured frame counter seed of Main.
func startDaemonAs(t *testing.T, in *tado.Install, clock *testClock, store *stateStore, dryRun bool, mode string, txSeed uint32) (*daemon, string, func()) {
	t.Helper()
	cfg := in.EmuConfig()
	cfg.Now = clock.now
	cfg.TxCounter = txSeed
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	if err := restoreDesiredState(store, emu); err != nil {
		t.Fatal(err)
	}
	d := newDaemon(in, emu, radio.NewMock(nil), dryRun, mode)
	d.retryNow = clock.now
	if err := d.attachState(store); err != nil {
		t.Fatal(err)
	}
	if err := d.restoreHomeState(emu); err != nil {
		t.Fatal(err)
	}
	if err := d.restoreCounters(emu); err != nil {
		t.Fatal(err)
	}
	d.applyHomeState(emu)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.run(ctx); close(done) }()
	srv := httptest.NewServer(d.mux())
	stop := sync.OnceFunc(func() {
		srv.Close()
		cancel()
		<-done
	})
	t.Cleanup(stop)
	return d, srv.URL, stop
}

// TestRestartAcrossBoundaryRewritesDueZones: a daemon down across the 06:00
// boundary writes, on its first tick, each zone whose program moved while it
// was down, once. A zone whose block did not move and whose writes were
// delivered takes no write.
func TestRestartAcrossBoundaryRewritesDueZones(t *testing.T) {
	path := t.TempDir() + "/state.db"
	in := windowInstall(t, nil)
	thermostat := wireOf(t, in, dhwNode)
	moved := wireOf(t, in, "VA1000000005")
	steady := wireOf(t, in, "VA1000000002")

	before := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url, stop := startStoredDaemon(t, in, before, newTestStore(t, path))
	putHotWaterWeek(t, url)
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule", allDayProgram(
		map[string]any{"minute": 0, "setpoint_c": 20.0},
		map[string]any{"minute": 360, "setpoint_c": 18.0},
	), &view); code != http.StatusOK {
		t.Fatalf("zone 10 schedule: code=%d %v", code, view)
	}
	if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 19.0}), &view); code != http.StatusOK {
		t.Fatalf("zone 3 schedule: code=%d %v", code, view)
	}
	tickHome(t, d)
	for _, target := range []string{thermostat, moved, steady} {
		deliverQueue(t, d, in, target)
	}
	tickHome(t, d)
	stop()

	after := newTestClock(time.Date(2026, 1, 5, 7, 0, 0, 0, time.UTC))
	d, _, _ = startStoredDaemon(t, in, after, newTestStore(t, path))
	tickHome(t, d)
	if n := pending(t, d, thermostat); n != 1 {
		t.Fatalf("the hot-water zone took %d writes after the restart, want 1", n)
	}
	wantTags(t, "the hot-water block after the restart", nextZoneWrite(t, d, thermostat), map[string]string{
		"6020": "02", "61e0": "01", "6200": "157c", "6240": "00",
	}, "6440")
	if n := pending(t, d, moved); n != 1 {
		t.Fatalf("zone 10 took %d writes after the restart, want 1", n)
	}
	wantTags(t, "the zone 10 block after the restart", nextZoneWrite(t, d, moved), map[string]string{
		"61e0": "01", "6200": "0708",
	}, "6440")
	if n := pending(t, d, steady); n != 0 {
		t.Errorf("zone 3 kept its block and took %d writes, want none", n)
	}
	for _, target := range []string{thermostat, moved, steady} {
		dropQueue(t, d, target)
	}
	tickHome(t, d)
	for _, target := range []string{thermostat, moved, steady} {
		if n := pending(t, d, target); n != 0 {
			t.Errorf("%s took %d more writes on the second tick, want none", target, n)
		}
	}
}

// TestStartWithoutStoredBlocksOnlySeeds: a database with no stored blocks, as
// after an upgrade, seeds the baseline on start, writes no zone and stores it.
func TestStartWithoutStoredBlocksOnlySeeds(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store := newTestStore(t, path)
	if err := store.SetHotWaterProgram(context.Background(), []tado.ScheduleProgram{{DayMask: 0x7f, Entries: []tado.ScheduleEntry{
		{Minute: 0, SetpointCenti: 4500, Enabled: true},
		{Minute: 360, SetpointCenti: 5500, Enabled: true},
	}}}); err != nil {
		t.Fatal(err)
	}
	in := windowInstall(t, nil)
	clock := newTestClock(time.Date(2026, 1, 5, 7, 0, 0, 0, time.UTC))
	d, _, stop := startStoredDaemon(t, in, clock, store)
	tickHome(t, d)
	if n := pending(t, d, wireOf(t, in, dhwNode)); n != 0 {
		t.Errorf("the first start queued %d writes on the thermostat, want none", n)
	}
	stop()
	blocks, stored, err := newTestStore(t, path).ProgramBlocks(context.Background())
	if err != nil || !stored || blocks[dhwZoneID] != (scheduledBlock{c: 55}) {
		t.Fatalf("stored blocks=%v stored=%t err=%v, want the 55 C hot-water block", blocks, stored, err)
	}
}

// deliverQueue answers every write queued for target with the 2.04 of the
// device, head first, and returns how many it answered.
func deliverQueue(t *testing.T, d *daemon, in *tado.Install, target string) int {
	t.Helper()
	n := 0
	for pending(t, d, target) > 0 {
		v, err := d.do(func(e *tado.EmuState) (any, error) {
			actions, ok, err := e.NextQueuedDownlink(target)
			if err != nil || !ok || len(actions) == 0 {
				return nil, fmt.Errorf("no queued downlink for %s (err=%v)", target, err)
			}
			return actions[0].Bytes, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		frame, err := tado.ParseData(in.NetworkKey(), v.([]byte))
		if err != nil {
			t.Fatal(err)
		}
		m := tado.ParseApp(frame.Plaintext).CoAP
		feed(t, d, in, target, false, tado.CoAPReq{Type: "ACK", Code: "2.04", MID: m.MID, Token: m.Token})
		n++
	}
	return n
}

// TestRestartBeforeDeliveryKeepsBlockChange: the 06:00 hot-water block is
// queued, and the daemon restarts at 06:02 before the thermostat takes it. The
// first tick after the restart queues it again, once. After its delivery a
// restart writes nothing.
func TestRestartBeforeDeliveryKeepsBlockChange(t *testing.T) {
	path := t.TempDir() + "/state.db"
	in := windowInstall(t, nil)
	thermostat := wireOf(t, in, dhwNode)
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url, stop := startStoredDaemon(t, in, clock, newTestStore(t, path))
	putHotWaterWeek(t, url)
	tickHome(t, d)
	deliverQueue(t, d, in, thermostat)
	clock.advance(time.Hour)
	tickHome(t, d)
	if n := pending(t, d, thermostat); n != 1 {
		t.Fatalf("the 06:00 hot-water block queued %d writes, want 1", n)
	}
	stop()

	clock.advance(2 * time.Minute)
	d, _, stop = startStoredDaemon(t, in, clock, newTestStore(t, path))
	tickHome(t, d)
	if n := pending(t, d, thermostat); n != 1 {
		t.Fatalf("the undelivered block took %d writes after the restart, want 1", n)
	}
	tickHome(t, d)
	if n := pending(t, d, thermostat); n != 1 {
		t.Fatalf("a second tick left %d writes, want the 1 owed", n)
	}
	wantTags(t, "the owed block", nextZoneWrite(t, d, thermostat), map[string]string{
		"6020": "02", "61e0": "01", "6200": "157c", "6240": "00",
	}, "6440")
	if n := deliverQueue(t, d, in, thermostat); n != 1 {
		t.Fatalf("delivered %d writes, want 1", n)
	}
	tickHome(t, d)
	stop()

	clock.advance(time.Minute)
	d, _, _ = startStoredDaemon(t, in, clock, newTestStore(t, path))
	tickHome(t, d)
	if n := pending(t, d, thermostat); n != 0 {
		t.Errorf("a delivered block took %d writes after the restart, want none", n)
	}
}

// TestRestartBeforeDeliveryKeepsAwaySwitch: the away burst is queued and the
// daemon restarts before any member takes it. The first tick queues every owed
// write again, and once they are delivered a restart writes nothing.
func TestRestartBeforeDeliveryKeepsAwaySwitch(t *testing.T) {
	path := t.TempDir() + "/state.db"
	in := windowInstall(t, nil)
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url, stop := startStoredDaemon(t, in, clock, newTestStore(t, path))
	tickHome(t, d)
	var res map[string]any
	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &res); code != http.StatusOK {
		t.Fatalf("away: code=%d %v", code, res)
	}
	targets := make([]string, 0, len(in.Devices))
	want := map[string]int{}
	for i := range in.Devices {
		target := hex.EncodeToString(in.Devices[i].WireEUI())
		targets = append(targets, target)
		want[target] = pending(t, d, target)
	}
	valve := wireOf(t, in, "VA1000000002")
	if want[valve] != 1 {
		t.Fatalf("the away burst queued %d writes on zone 3, want 1", want[valve])
	}
	stop()

	clock.advance(time.Minute)
	d, _, stop = startStoredDaemon(t, in, clock, newTestStore(t, path))
	tickHome(t, d)
	for _, target := range targets {
		if n := pending(t, d, target); n != want[target] {
			t.Errorf("%s holds %d writes after the restart, want the %d owed", target, n, want[target])
		}
	}
	if pending(t, d, valve) > 0 {
		wantTags(t, "the owed away write", nextZoneWrite(t, d, valve), map[string]string{"6160": "02", "61e0": "00"})
	}
	for _, target := range targets {
		deliverQueue(t, d, in, target)
	}
	tickHome(t, d)
	stop()

	clock.advance(time.Minute)
	d, _, _ = startStoredDaemon(t, in, clock, newTestStore(t, path))
	tickHome(t, d)
	for _, target := range targets {
		if n := pending(t, d, target); n != 0 {
			t.Errorf("%s took %d writes after a restart with nothing owed, want none", target, n)
		}
	}
}

// TestRestartDuringEarlyStartKeepsBlock: zone 3 starts its 07:00 21 C block at
// 06:00 and reaches it at 06:45. A restart at 06:50 keeps the early block: the
// zone takes no write back to 17 C, and none at 07:00.
func TestRestartDuringEarlyStartKeepsBlock(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store := newTestStore(t, path)
	m := &thermalModel{}
	for i := range 3 {
		m.add(thermalEpisode{EndedMs: int64(i), StartC: 17, TargetC: 21, Rate: 4})
	}
	if err := store.SetThermalModel(context.Background(), 3, m); err != nil {
		t.Fatal(err)
	}
	in := windowInstall(t, func(in *tado.Install) { in.Options.EarlyStartZones = map[int]bool{3: true} })
	valve := wireOf(t, in, earlyValve)
	clock := newTestClock(windowStart)
	d, url, stop := startStoredDaemon(t, in, clock, store)
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule", allDayProgram(
		map[string]any{"minute": 0, "setpoint_c": 17.0},
		map[string]any{"minute": 420, "setpoint_c": 21.0},
	), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	seedBlock(t, d, in, earlyValve, 3)
	at(t, d, in, clock, "05:55", 17.0, 0)
	deliverQueue(t, d, in, valve)
	at(t, d, in, clock, "06:00", math.NaN(), 0)
	wantTags(t, "the early block", deliver(t, d, in, valve), map[string]string{"6200": "0834"})
	at(t, d, in, clock, "06:45", 21.2, 10)
	stop()

	clock.advance(5 * time.Minute)
	d, _, _ = startStoredDaemon(t, in, clock, newTestStore(t, path))
	tickHome(t, d)
	if n := pending(t, d, valve); n != 0 {
		t.Fatalf("the restart at 06:50 queued %d writes: %v, want none", n, nextZoneWrite(t, d, valve))
	}
	at(t, d, in, clock, "07:00", math.NaN(), 0)
	at(t, d, in, clock, "07:01", math.NaN(), 0)
	if n := pending(t, d, valve); n != 0 {
		t.Errorf("the 07:00 block change queued %d writes, want none", n)
	}
}
