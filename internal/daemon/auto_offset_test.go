package daemon

// auto_offset_test: the automatic calibration of a zone's temperature
// correction from an external room temperature. Zone 3 has one valve and runs
// 19.5 C all day; the clock, the reports and the readings are synthetic.

import (
	"context"
	"math"
	"net/http"
	"testing"
	"time"

	"freedo/tado"
)

type autoRig struct {
	t      *testing.T
	a      *installAPI
	d      *daemon
	url    string
	in     *tado.Install
	clock  *testClock
	valve  string
	steps  int
	writes []roomWrite
}

// newAutoRig starts a daemon on zone 3 in offset mode config with the automatic
// calibration on at intervalSec.
func newAutoRig(t *testing.T, intervalSec int, edit func(*tado.Install)) *autoRig {
	t.Helper()
	a := newInstallAPIOn(t, windowInstall(t, func(in *tado.Install) {
		in.Options.AutoOffsets = map[int]tado.AutoOffset{3: {Enabled: true, IntervalSec: intervalSec}}
		if edit != nil {
			edit(in)
		}
	}))
	stored := loadStored(t, a)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, stored, clock, a.store)
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 19.5}), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	seedBlock(t, d, stored, earlyValve, 3)
	return &autoRig{t: t, a: a, d: d, url: url, in: stored, clock: clock, valve: wireOf(t, stored, earlyValve)}
}

func (r *autoRig) push(c float64) int {
	var out map[string]any
	return postJSON(r.t, http.MethodPost, r.url+"/api/zones/3/temperature", map[string]any{"tempC": c}, &out)
}

func (r *autoRig) tick() {
	r.t.Helper()
	if _, err := r.d.do(func(e *tado.EmuState) (any, error) { r.d.tickAutoOffsets(e); return nil, nil }); err != nil {
		r.t.Fatal(err)
	}
}

func (r *autoRig) deliverAll() {
	r.t.Helper()
	for pending(r.t, r.d, r.valve) > 0 {
		r.writes = append(r.writes, roomWrite{at: r.clock.now(), tags: deliver(r.t, r.d, r.in, r.valve)})
	}
}

// run moves the clock by minutes in steps of roomStepMin. Every step the valve
// reports deviceAt(step) and the external sensor pushes extC, unless it is NaN.
// The report and the push come before the clock moves, so a tick finds both.
func (r *autoRig) run(minutes int, deviceAt func(int) float64, extC float64) {
	r.t.Helper()
	for range minutes / roomStepMin {
		if deviceAt != nil {
			zoneAct(r.t, r.d, r.in, deviceAt(r.steps), 50)
		}
		if !math.IsNaN(extC) {
			if code := r.push(extC); code != http.StatusOK {
				r.t.Fatalf("push %v: code=%d", extC, code)
			}
		}
		r.clock.advance(roomStepMin * time.Minute)
		r.tick()
		r.deliverAll()
		r.steps++
	}
}

func (r *autoRig) setWindow(zone int, open bool) {
	r.t.Helper()
	if _, err := r.d.do(func(e *tado.EmuState) (any, error) {
		if open {
			r.d.windows[zone] = zoneWindow{windowExternal: windowHold{}}
		} else {
			delete(r.d.windows, zone)
		}
		return nil, nil
	}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *autoRig) setHeat(zone int, running bool) {
	r.t.Helper()
	if _, err := r.d.do(func(e *tado.EmuState) (any, error) {
		if running {
			r.d.heating[zone] = &heatEpisode{targetC: 21}
		} else {
			delete(r.d.heating, zone)
		}
		return nil, nil
	}); err != nil {
		r.t.Fatal(err)
	}
}

type autoZones struct {
	Declared []struct {
		Number       int            `json:"number"`
		AutoOffset   map[string]any `json:"autoOffset"`
		ExternalTemp map[string]any `json:"externalTemp"`
	} `json:"declared"`
}

func autoOffsetOf(t *testing.T, url string, zone int) map[string]any {
	t.Helper()
	var zones autoZones
	if code := getJSON(t, url+"/api/zones", &zones); code != http.StatusOK {
		t.Fatalf("zones: code=%d", code)
	}
	for _, z := range zones.Declared {
		if z.Number == zone {
			return z.AutoOffset
		}
	}
	t.Fatalf("zone %d not declared", zone)
	return nil
}

// TestAutoOffsetWaitsForTwoHoursOfSamples: no calibration before two hours of
// samples, one after, through the mode config path with the filtered difference.
func TestAutoOffsetWaitsForTwoHoursOfSamples(t *testing.T) {
	r := newAutoRig(t, 3600, nil)
	r.run(90, constant(22), 20)
	if len(r.writes) != 0 {
		t.Fatalf("%d writes before two hours of samples, want none", len(r.writes))
	}
	r.run(45, constant(22), 20)
	if len(r.writes) != 1 {
		t.Fatalf("%d writes after two hours of samples, want one", len(r.writes))
	}
	wantTags(t, "the calibration", r.writes[0].tags, map[string]string{"0140": "ff38"})
}

// TestAutoOffsetOneWritePerInterval: with a six-hour interval the first write
// waits for the interval, and no second write comes before the next one.
func TestAutoOffsetOneWritePerInterval(t *testing.T) {
	r := newAutoRig(t, 21600, nil)
	r.run(6*60+10, constant(22), 20)
	if len(r.writes) != 1 {
		t.Fatalf("%d writes, want one", len(r.writes))
	}
	r.run(3*60, constant(22), 20)
	if len(r.writes) != 1 {
		t.Errorf("a second write inside the interval: %d writes", len(r.writes))
	}
}

// TestAutoOffsetDeadband: a difference under 0.3 C is not worth a write.
func TestAutoOffsetDeadband(t *testing.T) {
	r := newAutoRig(t, 3600, nil)
	r.run(135, constant(20), 20.1)
	if len(r.writes) != 0 {
		t.Errorf("%d writes for a difference of 0.1 C, want none", len(r.writes))
	}
}

// TestAutoOffsetClampsTheCorrection: a gap past the bound writes the bound.
// TestAutoOffsetStartsFromTheServedCorrection: a correction set by hand before
// the calibration runs is the base of the first calibration, not zero.
func TestAutoOffsetStartsFromTheServedCorrection(t *testing.T) {
	r := newAutoRig(t, 3600, nil)
	var out map[string]any
	if code := postJSON(t, http.MethodPost, r.url+"/api/zones/3/offset", map[string]any{"offsetC": -0.7}, &out); code != http.StatusOK {
		t.Fatalf("manual offset: code=%d %v", code, out)
	}
	r.deliverAll()
	r.writes = nil
	r.run(135, constant(20), 21)
	if len(r.writes) != 1 {
		t.Fatalf("%d writes, want one", len(r.writes))
	}
	// -0.7 + 1.0 = +0.3 C = 30 centi-degrees.
	wantTags(t, "the calibration on top of the manual correction", r.writes[0].tags, map[string]string{"0140": "001e"})
}

func TestAutoOffsetClampsTheCorrection(t *testing.T) {
	r := newAutoRig(t, 3600, nil)
	r.run(135, constant(10), 30)
	if len(r.writes) != 1 {
		t.Fatalf("%d writes, want one", len(r.writes))
	}
	wantTags(t, "the clamped calibration", r.writes[0].tags, map[string]string{"0140": "01f4"})
}

// TestAutoOffsetRestartsTheFilterAfterAWrite: the sampled span restarts at each
// write, because the write moves 4060 by the delta.
func TestAutoOffsetRestartsTheFilterAfterAWrite(t *testing.T) {
	r := newAutoRig(t, 3600, nil)
	r.run(135, constant(22), 20)
	if len(r.writes) != 1 {
		t.Fatalf("%d writes, want one", len(r.writes))
	}
	r.run(20, constant(22), 20)
	if got := autoOffsetOf(t, r.url, 3)["sampleSpanSec"]; got.(float64) >= 3600 {
		t.Errorf("sample span after the write = %v, want the filter restarted", got)
	}
}

// TestAutoOffsetSkipsSamples: an open window and a running heat-up both stop the
// sampling, so no span builds and no write comes.
func TestAutoOffsetSkipsSamples(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(r *autoRig)
	}{
		{"open window", func(r *autoRig) { r.setWindow(3, true) }},
		{"heat-up", func(r *autoRig) { r.setHeat(3, true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAutoRig(t, 3600, nil)
			tc.setup(r)
			r.run(135, constant(22), 20)
			if len(r.writes) != 0 {
				t.Errorf("%d writes with the sample skipped, want none", len(r.writes))
			}
			if got := autoOffsetOf(t, r.url, 3)["sampleSpanSec"]; got != 0.0 {
				t.Errorf("sample span = %v, want 0", got)
			}
		})
	}
}

// TestAutoOffsetDefersAWriteDuringHeatUp: with enough samples the write would be
// due, but a running heat-up episode defers it to the next check after it ends.
func TestAutoOffsetDefersAWriteDuringHeatUp(t *testing.T) {
	r := newAutoRig(t, 3600, nil)
	r.run(115, constant(22), 20)
	if len(r.writes) != 0 {
		t.Fatalf("%d writes before the samples built, want none", len(r.writes))
	}
	r.setHeat(3, true)
	r.run(10, constant(22), 20)
	if len(r.writes) != 0 {
		t.Fatalf("%d writes during the heat-up episode, want none", len(r.writes))
	}
	r.setHeat(3, false)
	r.run(10, constant(22), 20)
	if len(r.writes) == 0 {
		t.Error("no write after the heat-up episode ended")
	}
}

// TestAutoOffsetRestart: the correction and the last write time survive a
// restart, and no new write comes before two more hours of samples.
func TestAutoOffsetRestart(t *testing.T) {
	r := newAutoRig(t, 3600, nil)
	r.run(135, constant(22), 20)
	if len(r.writes) != 1 {
		t.Fatalf("%d writes, want one", len(r.writes))
	}
	before := autoOffsetOf(t, r.url, 3)
	if before["lastWriteAt"] == nil {
		t.Fatal("the calibration left no write time")
	}

	clock := newTestClock(r.clock.now().Add(5 * time.Minute))
	d, url := newWindowDaemon(t, r.in, clock, r.a.store)
	if _, err := d.do(func(*tado.EmuState) (any, error) { return nil, d.restoreAutoOffsets() }); err != nil {
		t.Fatal(err)
	}
	seedBlock(t, d, r.in, earlyValve, 3)
	r2 := &autoRig{t: t, a: r.a, d: d, url: url, in: r.in, clock: clock, valve: r.valve}
	after := autoOffsetOf(t, url, 3)
	if after["lastWriteAt"] != before["lastWriteAt"] {
		t.Errorf("last write after the restart = %v, want %v", after["lastWriteAt"], before["lastWriteAt"])
	}
	r2.run(90, constant(22), 20)
	if len(r2.writes) != 0 {
		t.Errorf("%d writes within two hours of the restart, want none", len(r2.writes))
	}
}

// TestAutoOffsetFeedsLearners: with auto_offset on and a fresh reading, the
// early start, the plant episodes and the flow adaptation read the external
// temperature; stale, they read the valve's.
func TestAutoOffsetFeedsLearners(t *testing.T) {
	r := newAutoRig(t, 3600, nil)
	zoneAct(t, r.d, r.in, 22, 50)
	if code := r.push(20.4); code != http.StatusOK {
		t.Fatal("push failed")
	}
	read := func() (float64, float64) {
		v, err := r.d.do(func(e *tado.EmuState) (any, error) {
			c, _, _, _ := r.d.zoneReport(e, 3)
			s, _, ok := r.d.plantSample(e, r.d.inst.Load(), 3, r.d.retryNow())
			if !ok {
				return nil, context.Canceled
			}
			return [2]float64{c, s.TempC}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got := v.([2]float64)
		return got[0], got[1]
	}
	if c, s := read(); c != 20.4 || s != 20.4 {
		t.Errorf("fresh: zoneReport %v, plantSample %v, want 20.4", c, s)
	}
	r.clock.advance(31 * time.Minute)
	if c, s := read(); c != 22 || s != 22 {
		t.Errorf("stale: zoneReport %v, plantSample %v, want the valve's 22", c, s)
	}
	var zones autoZones
	if code := getJSON(t, r.url+"/api/zones", &zones); code != http.StatusOK {
		t.Fatal("zones failed")
	}
	for _, z := range zones.Declared {
		if z.Number == 3 && z.ExternalTemp["learnInUse"] != false {
			t.Errorf("learnInUse stale = %v, want false", z.ExternalTemp["learnInUse"])
		}
	}
}

// TestAutoOffsetReadingModes: a reading is taken in mode config with auto_offset
// on, and refused in the same mode without it.
func TestAutoOffsetReadingModes(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	push := func() int {
		var out map[string]any
		return postJSON(t, http.MethodPost, a.base+"/api/zones/3/temperature", map[string]any{"tempC": 20}, &out)
	}
	if code := push(); code != http.StatusConflict {
		t.Errorf("mode config without auto_offset: code=%d, want 409", code)
	}
	a.mutate(http.MethodPatch, "/api/install/zones/3", map[string]any{"auto_offset": true})
	if code := push(); code != http.StatusOK {
		t.Errorf("mode config with auto_offset: code=%d, want 200", code)
	}
	a.mutate(http.MethodPatch, "/api/install/zones/3", map[string]any{"auto_offset": false})
	if code := push(); code != http.StatusConflict {
		t.Errorf("mode config after auto_offset off: code=%d, want 409", code)
	}
}

// TestAutoOffsetOptionValidation: the option defaults, its interval range, the
// hot water zone and the mode setpoint conflict are checked.
func TestAutoOffsetOptionValidation(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	z := a.get("/api/install/zones/3")["zone"].(map[string]any)
	if z["auto_offset"] != false || z["auto_offset_interval_sec"] != 86400.0 {
		t.Errorf("zone 3 defaults = %v, want auto_offset false and 86400", z)
	}
	a.mutate(http.MethodPatch, "/api/install/zones/3", map[string]any{"auto_offset": true})
	if got := loadStored(t, a).Options.AutoOffset(3); got != (tado.AutoOffset{Enabled: true, IntervalSec: 86400}) {
		t.Errorf("stored auto offset = %+v, want enabled 86400", got)
	}
	for _, bad := range []int{0, 100, 604801} {
		if code, res := a.call(http.MethodPatch, "/api/install/zones/3", map[string]any{"auto_offset_interval_sec": bad}); code != http.StatusBadRequest {
			t.Errorf("interval %d: code=%d %v, want 400", bad, code, res)
		}
	}
	for _, ok := range []int{3600, 604800} {
		a.mutate(http.MethodPatch, "/api/install/zones/3", map[string]any{"auto_offset_interval_sec": ok})
	}
	if code, res := a.call(http.MethodPatch, "/api/install/zones/0", map[string]any{"auto_offset": true}); code != http.StatusBadRequest {
		t.Errorf("hot water: code=%d %v, want 400", code, res)
	}
	if code, res := a.call(http.MethodPatch, "/api/install/zones/3", map[string]any{"offset_mode": "setpoint"}); code != http.StatusBadRequest {
		t.Errorf("switch to setpoint with auto_offset on: code=%d %v, want 400", code, res)
	}
	if code, res := a.call(http.MethodPatch, "/api/install/zones/2", map[string]any{"offset_mode": "setpoint"}); code != http.StatusOK {
		t.Fatalf("setpoint on zone 2: code=%d %v", code, res)
	}
	if code, res := a.call(http.MethodPatch, "/api/install/zones/2", map[string]any{"auto_offset": true}); code != http.StatusBadRequest {
		t.Errorf("auto_offset in mode setpoint: code=%d %v, want 400", code, res)
	}
}

// heatUpRunning reports whether zone runs a heat-up episode now.
func heatUpRunning(t *testing.T, d *daemon, zone int) bool {
	t.Helper()
	v, err := d.do(func(*tado.EmuState) (any, error) { return d.heating[zone] != nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	return v.(bool)
}

// heatRig starts a daemon whose zone 3 runs 17 C then 21 C, with the store
// backing the management routes.
func heatRig(t *testing.T) (*daemon, string, *testClock, *tado.Install, *installAPI) {
	t.Helper()
	a := newInstallAPIOn(t, windowInstall(t, func(in *tado.Install) {
		in.Options.EarlyStartZones = map[int]bool{3: true}
	}))
	stored := loadStored(t, a)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, stored, clock, a.store)
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule", allDayProgram(
		map[string]any{"minute": 0, "setpoint_c": 17.0},
		map[string]any{"minute": 420, "setpoint_c": 21.0},
	), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	seedBlock(t, d, stored, earlyValve, 3)
	return d, url, clock, stored, a
}

// TestAutoOffsetManualOffsetRejectsHeatUp: a manual offset in mode config moves
// 4060 and rejects the running heat-up episode when the learner reads 4060.
func TestAutoOffsetManualOffsetRejectsHeatUp(t *testing.T) {
	d, url, clock, in, _ := heatRig(t)
	at(t, d, in, clock, "06:50", 17.0, 0)
	at(t, d, in, clock, "07:00", math.NaN(), 0)
	at(t, d, in, clock, "07:30", 18.0, 80)
	if !heatUpRunning(t, d, 3) {
		t.Fatal("no heat-up episode to reject")
	}
	var res map[string]any
	if code := postJSON(t, http.MethodPost, url+"/api/zones/3/offset", map[string]any{"offsetC": 0.5}, &res); code != http.StatusOK {
		t.Fatalf("offset: code=%d %v", code, res)
	}
	if heatUpRunning(t, d, 3) {
		t.Error("the manual offset did not reject the heat-up episode")
	}
}

// TestAutoOffsetManualOffsetKeepsHeatUp: with auto_offset on and a fresh
// reading, the learner reads the external temperature and the offset does not
// reject the running episode.
func TestAutoOffsetManualOffsetKeepsHeatUp(t *testing.T) {
	d, url, clock, in, _ := heatRig(t)
	var res map[string]any
	if code := postJSON(t, http.MethodPatch, url+"/api/install/zones/3", map[string]any{"auto_offset": true}, &res); code != http.StatusOK {
		t.Fatalf("auto_offset: code=%d %v", code, res)
	}
	at(t, d, in, clock, "06:50", 17.0, 0)
	at(t, d, in, clock, "07:00", math.NaN(), 0)
	at(t, d, in, clock, "07:30", 18.0, 80)
	if !heatUpRunning(t, d, 3) {
		t.Fatal("no heat-up episode to keep")
	}
	if code := postJSON(t, http.MethodPost, url+"/api/zones/3/temperature", map[string]any{"tempC": 20}, &res); code != http.StatusOK {
		t.Fatalf("temperature: code=%d %v", code, res)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/zones/3/offset", map[string]any{"offsetC": 0.5}, &res); code != http.StatusOK {
		t.Fatalf("offset: code=%d %v", code, res)
	}
	if !heatUpRunning(t, d, 3) {
		t.Error("a fresh external reading must keep the heat-up episode")
	}
}
