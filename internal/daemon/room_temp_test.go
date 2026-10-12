package daemon

// room_temp_test: the external room temperature of a zone and the shift it
// drives. Zone 3 has one valve and runs 19.5 C all day; the clock, the reports
// and the readings are synthetic.

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"freedo/tado"
)

const (
	// roomStepMin is how far the rig moves the clock between two ticks.
	roomStepMin = 5
	// roomSettle is long enough for the filter and the steps to reach a gap of
	// 2 C; roomSettleFar one of 5 C.
	roomSettle    = 12 * 60
	roomSettleFar = 16 * 60
)

type roomWrite struct {
	at   time.Time
	tags map[string]string
}

type roomRig struct {
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

// loadStored reads the installation back from the store of a, validated.
func loadStored(t *testing.T, a *installAPI) *tado.Install {
	t.Helper()
	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stored.ValidateCommissionable(true); err != nil {
		t.Fatal(err)
	}
	return stored
}

// newRoomRig starts a daemon on a store with zone 3 in offset mode setpoint at
// manualCenti, edit applied to the installation, the program on and the valve
// block known.
func newRoomRig(t *testing.T, manualCenti int, edit func(*tado.Install)) *roomRig {
	t.Helper()
	a := newInstallAPIOn(t, windowInstall(t, func(in *tado.Install) {
		in.Options.ZoneOffsets = map[int]tado.ZoneOffset{3: {Mode: tado.OffsetModeSetpoint, SetpointCenti: manualCenti}}
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
	return &roomRig{t: t, a: a, d: d, url: url, in: stored, clock: clock, valve: wireOf(t, stored, earlyValve)}
}

func (r *roomRig) push(c float64) (map[string]any, int) {
	r.t.Helper()
	var out map[string]any
	code := postJSON(r.t, http.MethodPost, r.url+"/api/zones/3/temperature", map[string]any{"tempC": c}, &out)
	return out, code
}

func (r *roomRig) tick() {
	r.t.Helper()
	if _, err := r.d.do(func(e *tado.EmuState) (any, error) { r.d.tickRoomTemps(e); return nil, nil }); err != nil {
		r.t.Fatal(err)
	}
}

// deliverAll delivers what the valve has queued, in order, and records it.
func (r *roomRig) deliverAll() {
	r.t.Helper()
	for pending(r.t, r.d, r.valve) > 0 {
		r.writes = append(r.writes, roomWrite{at: r.clock.now(), tags: deliver(r.t, r.d, r.in, r.valve)})
	}
}

// run moves the clock by minutes in steps of roomStepMin. Every third step the
// valve reports deviceAt(report index) as its zone temperature, 15 minutes
// apart as a valve does, and every step the external sensor pushes extC, unless
// it is NaN. The reports and the push come before the clock moves, so a tick
// finds both fresh.
func (r *roomRig) run(minutes int, deviceAt func(i int) float64, extC float64) {
	r.t.Helper()
	for range minutes / roomStepMin {
		if r.steps%3 == 0 && deviceAt != nil {
			zoneAct(r.t, r.d, r.in, deviceAt(r.steps/3), 50)
		}
		if !math.IsNaN(extC) {
			if _, code := r.push(extC); code != http.StatusOK {
				r.t.Fatalf("push %v: code=%d", extC, code)
			}
		}
		r.clock.advance(roomStepMin * time.Minute)
		r.tick()
		r.deliverAll()
		r.steps++
	}
}

func constant(c float64) func(int) float64 { return func(int) float64 { return c } }

type roomZones struct {
	Declared []struct {
		Number       int            `json:"number"`
		ExternalTemp map[string]any `json:"externalTemp"`
	} `json:"declared"`
	Demand []struct {
		Number int
		TempC  float64
	} `json:"demand"`
}

// view returns externalTemp of zone 3 and the temperature its demand view
// reports.
func (r *roomRig) view() (map[string]any, float64) {
	r.t.Helper()
	var zones roomZones
	if code := getJSON(r.t, r.url+"/api/zones", &zones); code != http.StatusOK {
		r.t.Fatalf("zones: code=%d", code)
	}
	var ext map[string]any
	for _, z := range zones.Declared {
		if z.Number == 3 {
			ext = z.ExternalTemp
		}
	}
	for _, z := range zones.Demand {
		if z.Number == 3 {
			return ext, z.TempC
		}
	}
	return ext, math.NaN()
}

func (r *roomRig) lastWrite() map[string]string {
	r.t.Helper()
	if len(r.writes) == 0 {
		r.t.Fatal("no write reached the valve")
	}
	return r.writes[len(r.writes)-1].tags
}

func wantView(t *testing.T, what string, view map[string]any, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if view[k] != v {
			t.Errorf("%s: externalTemp.%s = %v, want %v (view %v)", what, k, view[k], v, view)
		}
	}
}

// TestRoomTempShiftSignAndSize: the valve reads 2 C above the external sensor,
// so the room is colder than the valve believes and the setpoint written to the
// valve goes up by 2 C; in the other direction it goes down. The view reports
// the external temperature as the room temperature and keeps the valve's.
func TestRoomTempShiftSignAndSize(t *testing.T) {
	for _, tc := range []struct {
		name            string
		device, ext     float64
		shift           float64
		wire6200, wired string
	}{
		{"valve warmer", 22, 20, -2, "0866", "21.5"},
		{"valve colder", 20, 22, 2, "06d6", "17.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRoomRig(t, 0, nil)
			r.run(roomSettle, constant(tc.device), tc.ext)
			wantTags(t, "the shifted program", r.lastWrite(), map[string]string{"6200": tc.wire6200})
			view, temp := r.view()
			wantView(t, tc.name, view, map[string]any{
				"tempC": tc.ext, "inUse": true, "stale": false, "deviceC": tc.device,
				"shiftC": tc.shift, "offsetC": 0.0,
			})
			if temp != tc.ext {
				t.Errorf("zone temperature = %v, want the external %v", temp, tc.ext)
			}
			if target := mustTarget(t, r.url, 3); target["programC"] != 19.5 {
				t.Errorf("target = %v, want the program 19.5 on the corrected scale", target)
			}
		})
	}
}

// TestRoomTempClampsTheCorrection: a gap past roomShiftMaxC from the manual
// offset is not followed: the shift stops at the bound.
func TestRoomTempClampsTheCorrection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manual   int
		wire6200 string
		shift    float64
	}{
		{"no manual offset", 0, "0992", -5},
		{"manual offset +1", 100, "092e", -4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRoomRig(t, tc.manual, nil)
			r.run(roomSettleFar, constant(30), 18)
			wantTags(t, "the clamped program", r.lastWrite(), map[string]string{"6200": tc.wire6200})
			if view, _ := r.view(); view["shiftC"] != tc.shift {
				t.Errorf("shiftC = %v, want %v", view["shiftC"], tc.shift)
			}
		})
	}
}

// TestRoomTempFiltersTheValveSwing: the sensor of the valve swings by 2 C from
// report to report around a mean 2 C above the external sensor. The shift
// settles on that mean and, once there, writes nothing more.
func TestRoomTempFiltersTheValveSwing(t *testing.T) {
	r := newRoomRig(t, 0, nil)
	swing := func(i int) float64 { return 21 + 2*float64(i%2) }
	r.run(roomSettle, swing, 20)
	before := len(r.writes)
	r.run(6*60, swing, 20)
	if got := len(r.writes) - before; got != 0 {
		t.Errorf("%d writes in the six hours after the shift settled, want none", got)
	}
	if view, _ := r.view(); view["shiftC"] != -2.0 {
		t.Errorf("shiftC = %v, want -2 (the mean gap)", view["shiftC"])
	}
	for _, w := range r.writes {
		sp, err := strconv.ParseUint(w.tags["6200"], 16, 16)
		if err != nil || sp < 1950 || sp > 2150 {
			t.Errorf("write %v: 6200 outside 19.5..21.5 C", w.tags)
		}
	}
}

// TestRoomTempRateLimit: the shifts of a zone are at least roomShiftMinInterval
// apart, the first one waits for the interval the engine start opens, and a
// repeated reading changes nothing.
func TestRoomTempRateLimit(t *testing.T) {
	r := newRoomRig(t, 0, nil)
	r.run(roomSettle, constant(22), 20)
	if len(r.writes) < 3 {
		t.Fatalf("%d writes while the shift settled, want several", len(r.writes))
	}
	last := windowStart
	for _, w := range r.writes {
		if w.at.Sub(last) < roomShiftMinInterval {
			t.Errorf("a write at %s follows one at %s", w.at.Format("15:04"), last.Format("15:04"))
		}
		last = w.at
	}
	settled := len(r.writes)
	r.run(3*60, constant(22), 20)
	if len(r.writes) != settled {
		t.Errorf("%d writes after the shift settled, want none", len(r.writes)-settled)
	}
}

// TestRoomTempStaleReleasesSmoothly: with no new reading the zone stops
// reporting the external temperature after room_temp_max_age_sec, the shift
// walks back to the manual offset one step at a time and never past it, and the
// setpoint ends where the program puts it.
func TestRoomTempStaleReleasesSmoothly(t *testing.T) {
	r := newRoomRig(t, 0, nil)
	r.run(roomSettle, constant(22), 20)
	if _, code := r.push(20.4); code != http.StatusOK {
		t.Fatal("push failed")
	}
	if view, temp := r.view(); view["inUse"] != true || temp != 20.4 {
		t.Fatalf("fresh reading: view %v temperature %v, want in use and 20.4", view, temp)
	}
	settled := len(r.writes)

	r.run(25, constant(22), math.NaN())
	if view, _ := r.view(); view["inUse"] != true {
		t.Errorf("25 minutes after the push: %v, want in use", view)
	}
	r.run(10, constant(22), math.NaN())
	view, temp := r.view()
	wantView(t, "after the limit", view, map[string]any{"inUse": false, "stale": true, "tempC": 20.4})
	if want := 22 + view["shiftC"].(float64); math.Abs(temp-want) > 1e-9 {
		t.Errorf("stale zone temperature = %v, want the valve's 22 plus the shift: %v", temp, want)
	}

	r.run(10*60, constant(22), math.NaN())
	released := r.writes[settled:]
	if len(released) < 3 {
		t.Errorf("%d writes released the shift, want several small steps", len(released))
	}
	prev := math.Inf(1)
	for _, w := range released {
		sp, err := strconv.ParseUint(w.tags["6200"], 16, 16)
		if err != nil || float64(sp) > prev || sp < 1950 {
			t.Errorf("release write %v: 6200 must fall toward 1950 and never pass it", w.tags)
		}
		prev = float64(sp)
	}
	wantTags(t, "the end of the release", r.lastWrite(), map[string]string{"6200": "079e"})
	view, temp = r.view()
	wantView(t, "released", view, map[string]any{"shiftC": 0.0, "wantedC": nil, "stale": true})
	if temp != 22 {
		t.Errorf("released zone temperature = %v, want the valve's 22", temp)
	}
	if shifts, err := r.a.store.RoomShifts(context.Background()); err != nil || len(shifts) != 0 {
		t.Errorf("stored shifts = %v (%v), want none after the release", shifts, err)
	}
}

// TestRoomTempClearReleases: DELETE drops the reading, the zone reports the
// valve temperature at once, and the shift is released as for a stale reading.
func TestRoomTempClearReleases(t *testing.T) {
	r := newRoomRig(t, 0, nil)
	r.run(roomSettle, constant(22), 20)
	var view map[string]any
	if code := postJSON(t, http.MethodDelete, r.url+"/api/zones/3/temperature", map[string]any{}, &view); code != http.StatusOK {
		t.Fatalf("delete: code=%d %v", code, view)
	}
	wantView(t, "cleared", view, map[string]any{"tempC": nil, "inUse": false, "shiftC": -2.0})
	r.run(10*60, constant(22), math.NaN())
	wantTags(t, "the end of the release", r.lastWrite(), map[string]string{"6200": "079e"})
	if view, _ := r.view(); view["shiftC"] != 0.0 || view["tempC"] != nil {
		t.Errorf("after the release: %v, want no shift and no reading", view)
	}
}

// TestRoomTempSourceLabel: the push names its sensor, the view shows the name
// of the last reading, a push without one clears it, a bad label is refused
// with the reading untouched, and a DELETE drops the label with the reading.
func TestRoomTempSourceLabel(t *testing.T) {
	r := newRoomRig(t, 0, nil)
	post := func(body map[string]any) (map[string]any, int) {
		t.Helper()
		var out map[string]any
		return out, postJSON(t, http.MethodPost, r.url+"/api/zones/3/temperature", body, &out)
	}
	answer, code := post(map[string]any{"tempC": 20.4, "source": "Soggiorno thermometer"})
	if code != http.StatusOK {
		t.Fatalf("push: code=%d %v", code, answer)
	}
	wantView(t, "the answer", answer, map[string]any{"tempC": 20.4, "source": "Soggiorno thermometer"})
	view, _ := r.view()
	wantView(t, "GET /api/zones", view, map[string]any{"tempC": 20.4, "source": "Soggiorno thermometer"})

	for _, tc := range []struct {
		name   string
		source any
	}{
		{"too long", strings.Repeat("a", roomTempSourceMaxLen+1)},
		{"control character", "a\tb"},
		{"not a string", 3},
	} {
		if answer, code := post(map[string]any{"tempC": 21.0, "source": tc.source}); code != http.StatusBadRequest {
			t.Errorf("%s: code=%d %v, want 400", tc.name, code, answer)
		}
	}
	view, _ = r.view()
	wantView(t, "after the refused pushes", view, map[string]any{"tempC": 20.4, "source": "Soggiorno thermometer"})

	answer, _ = post(map[string]any{"tempC": 20.6})
	wantView(t, "a push without a source", answer, map[string]any{"tempC": 20.6, "source": nil})

	post(map[string]any{"tempC": 20.8, "source": "Aqara"})
	if code := postJSON(t, http.MethodDelete, r.url+"/api/zones/3/temperature", map[string]any{}, &answer); code != http.StatusOK {
		t.Fatalf("delete: code=%d %v", code, answer)
	}
	wantView(t, "cleared", answer, map[string]any{"tempC": nil, "source": nil})
}

// TestRoomTempRestart: the shift is stored because the valves carry it, the
// reading is not. A daemon started on the store reads a device setpoint with
// the shift, reports the valve temperature and not the old reading, and does
// not write before the interval after its start.
func TestRoomTempRestart(t *testing.T) {
	r := newRoomRig(t, 0, nil)
	r.run(roomSettle, constant(22), 20)
	if shifts, err := r.a.store.RoomShifts(context.Background()); err != nil || shifts[3] != -200 {
		t.Fatalf("stored shifts = %v (%v), want zone 3 at -200", shifts, err)
	}

	clock := newTestClock(r.clock.now().Add(5 * time.Minute))
	d, url := newWindowDaemon(t, r.in, clock, r.a.store)
	if _, err := d.do(func(*tado.EmuState) (any, error) { return nil, d.restoreRoomShifts() }); err != nil {
		t.Fatal(err)
	}
	seedBlock(t, d, r.in, earlyValve, 3)
	r2 := &roomRig{t: t, a: r.a, d: d, url: url, in: r.in, clock: clock, valve: r.valve}
	view, _ := r2.view()
	wantView(t, "after the restart", view, map[string]any{"tempC": nil, "inUse": false, "shiftC": -2.0})

	zoneAct(t, d, r.in, 22, 50)
	if _, temp := r2.view(); temp != 20 {
		t.Errorf("zone temperature after the restart = %v, want the valve's 22 plus the stored shift", temp)
	}
	overlayFromDevice(t, d, r.in, earlyValve, 3, 2250, 0x6201)
	if target := mustTarget(t, url, 3); target["targetC"] != 20.5 {
		t.Errorf("device setpoint 22.5 read with the stored shift: target %v, want 20.5", target)
	}

	r2.run(10, constant(22), math.NaN())
	if len(r2.writes) != 0 {
		t.Errorf("%d writes within 15 minutes of the start, want none", len(r2.writes))
	}
	r2.run(30, constant(22), math.NaN())
	if len(r2.writes) == 0 {
		t.Error("no release step once the interval after the start passed")
	}
}

// TestRoomTempManualOffset: while a reading is fresh the shift replaces the
// manual offset, a new manual offset changes nothing on the valves, and the
// release ends at the manual offset in force then.
func TestRoomTempManualOffset(t *testing.T) {
	r := newRoomRig(t, 100, nil)
	r.run(roomSettle, constant(22), 20)
	wantTags(t, "the shift that replaced +1.0", r.lastWrite(), map[string]string{"6200": "0866"})
	if view, _ := r.view(); view["shiftC"] != -2.0 || view["offsetC"] != 1.0 {
		t.Errorf("view = %v, want shift -2 and manual offset 1", view)
	}

	n := len(r.writes)
	var res map[string]any
	if code := postJSON(t, http.MethodPost, r.url+"/api/zones/3/offset", map[string]any{"offsetC": 0.5}, &res); code != http.StatusOK {
		t.Fatalf("offset: code=%d %v", code, res)
	}
	r.deliverAll()
	if len(r.writes) != n {
		t.Errorf("a manual offset under a fresh reading wrote %d times, want none", len(r.writes)-n)
	}
	if view, _ := r.view(); view["shiftC"] != -2.0 || view["offsetC"] != 0.5 {
		t.Errorf("view = %v, want shift -2 and manual offset 0.5", view)
	}

	r.run(roomSettle, constant(22), math.NaN())
	wantTags(t, "the release to +0.5", r.lastWrite(), map[string]string{"6200": "076c"})
	if view, _ := r.view(); view["shiftC"] != 0.5 {
		t.Errorf("released shift = %v, want the manual 0.5", view["shiftC"])
	}
}

// TestRoomTempOtherOffsetModes: mode config and dbg2 take no reading, because
// they would write tag 0140. A zone that leaves mode setpoint while it holds a
// shift is restated on the valve scale and forgets the reading.
func TestRoomTempOtherOffsetModes(t *testing.T) {
	for _, mode := range []string{tado.OffsetModeConfig, tado.OffsetModeDbg2} {
		t.Run(mode, func(t *testing.T) {
			r := newRoomRig(t, 0, nil)
			r.run(roomSettle, constant(22), 20)
			var res map[string]any
			if code := postJSON(t, http.MethodPatch, r.url+"/api/install/zones/3", map[string]any{"offset_mode": mode}, &res); code != http.StatusOK {
				t.Fatalf("patch: code=%d %v", code, res)
			}
			r.deliverAll()
			wantTags(t, "the restate on leaving mode setpoint", r.lastWrite(), map[string]string{"6200": "079e"})
			view, _ := r.view()
			wantView(t, "after leaving", view, map[string]any{"tempC": nil, "inUse": false, "shiftC": 0.0})
			if _, code := r.push(20); code != http.StatusConflict {
				t.Errorf("push in mode %s: code=%d, want 409", mode, code)
			}
			if shifts, err := r.a.store.RoomShifts(context.Background()); err != nil || len(shifts) != 0 {
				t.Errorf("stored shifts = %v (%v), want none", shifts, err)
			}
		})
	}
}

// TestRoomTempRefusals: hot water, an unknown zone, a bad body and a value out
// of range are refused, and a refused push stores nothing.
func TestRoomTempRefusals(t *testing.T) {
	r := newRoomRig(t, 0, nil)
	post := func(zone int, body any) int {
		var out map[string]any
		return postJSON(t, http.MethodPost, r.url+"/api/zones/"+strconv.Itoa(zone)+"/temperature", body, &out)
	}
	for _, tc := range []struct {
		name string
		zone int
		body any
		code int
	}{
		{"hot water", 0, map[string]any{"tempC": 20}, http.StatusBadRequest},
		{"unknown zone", 99, map[string]any{"tempC": 20}, http.StatusNotFound},
		{"zone in mode config", 5, map[string]any{"tempC": 20}, http.StatusConflict},
		{"no tempC", 3, map[string]any{}, http.StatusBadRequest},
		{"null tempC", 3, map[string]any{"tempC": nil}, http.StatusBadRequest},
		{"above range", 3, map[string]any{"tempC": 80}, http.StatusBadRequest},
		{"below range", 3, map[string]any{"tempC": -30}, http.StatusBadRequest},
		{"unknown field", 3, map[string]any{"tempC": 20, "open": true}, http.StatusBadRequest},
	} {
		if code := post(tc.zone, tc.body); code != tc.code {
			t.Errorf("%s: code=%d, want %d", tc.name, code, tc.code)
		}
	}
	var out map[string]any
	if code := postJSON(t, http.MethodDelete, r.url+"/api/zones/0/temperature", map[string]any{}, &out); code != http.StatusBadRequest {
		t.Errorf("delete on hot water: code=%d, want 400", code)
	}
	if view, _ := r.view(); view["tempC"] != nil {
		t.Errorf("a refused push left a reading: %v", view)
	}
	var zones roomZones
	if code := getJSON(t, r.url+"/api/zones", &zones); code != http.StatusOK {
		t.Fatal("zones failed")
	}
	for _, z := range zones.Declared {
		if (z.Number == 0) != (z.ExternalTemp == nil) {
			t.Errorf("zone %d externalTemp = %v: only heating zones carry one", z.Number, z.ExternalTemp)
		}
	}
}

// TestRoomTempViews: the reading is rounded to 0.01 C, its age follows the
// clock, and it goes stale after room_temp_max_age_sec, which the installation
// sets. With no report from the valve the zone still reports the reading and
// the shift stays where it is.
func TestRoomTempViews(t *testing.T) {
	r := newRoomRig(t, 0, func(in *tado.Install) {
		maxAge := 900
		in.Options.RoomTempMaxAgeSec = &maxAge
	})
	view, code := r.push(20.374)
	if code != http.StatusOK {
		t.Fatalf("push: code=%d", code)
	}
	wantView(t, "pushed", view, map[string]any{
		"tempC": 20.37, "ageSec": 0.0, "inUse": true, "stale": false, "deviceC": nil,
		"at": windowStart.Format(time.RFC3339),
	})
	r.clock.advance(10 * time.Minute)
	if view, temp := r.view(); view["ageSec"] != 600.0 || view["inUse"] != true || temp != 20.37 {
		t.Errorf("after 10 minutes: view %v temperature %v", view, temp)
	}
	r.clock.advance(6 * time.Minute)
	if view, _ := r.view(); view["stale"] != true || view["inUse"] != false {
		t.Errorf("after 16 minutes with a limit of 15: %v, want stale", view)
	}

	r.push(20)
	r.run(2*60, nil, 20)
	if len(r.writes) != 0 {
		t.Errorf("%d writes with no zone report, want none", len(r.writes))
	}
	if view, temp := r.view(); view["shiftC"] != 0.0 || temp != 20 {
		t.Errorf("view %v temperature %v, want no shift and the external 20", view, temp)
	}
}

// TestRoomTempShiftsEveryTarget: the shift applies to whatever target is in
// force. A schedule block, an API overlay and the away temperature go out
// shifted; a change made on the valve is read with the shift, and when the
// shift later moves the overlay keeps its target on the corrected scale.
func TestRoomTempShiftsEveryTarget(t *testing.T) {
	endManual := func(in *tado.Install) {
		in.Options.DeviceOverlayEnds = map[int]tado.OverlayEnd{3: {Mode: tado.OverlayEndManual}}
		in.Options.AwayTemperatures = map[int]float64{3: 15}
	}
	t.Run("overlay and wheel", func(t *testing.T) {
		r := newRoomRig(t, 0, endManual)
		r.run(roomSettle, constant(22), 20)
		wantTags(t, "the program", r.lastWrite(), map[string]string{"6200": "0866"})

		if _, code := postZone(t, r.url, 3, "setpoint", map[string]any{"setpointC": 21.0}); code != http.StatusOK {
			t.Fatalf("setpoint: code=%d", code)
		}
		r.deliverAll()
		wantTags(t, "the API overlay", r.lastWrite(), map[string]string{"6280": "08fc", "6240": "02"})
		if target := mustTarget(t, r.url, 3); target["targetC"] != 21.0 {
			t.Errorf("API overlay target = %v, want 21 on the corrected scale", target)
		}
		if _, code := postZone(t, r.url, 3, "auto", map[string]any{}); code != http.StatusOK {
			t.Fatalf("auto: code=%d", code)
		}
		r.deliverAll()

		n := len(r.writes)
		overlayFromDevice(t, r.d, r.in, earlyValve, 3, 2250, 0x6211)
		r.deliverAll()
		if len(r.writes) != n {
			t.Errorf("a change on the valve wrote %d times, want none", len(r.writes)-n)
		}
		target := mustTarget(t, r.url, 3)
		if target["targetC"] != 20.5 || overlayOfTarget(t, target)["setpointC"] != 20.5 {
			t.Errorf("wheel change to 22.5 on the valve scale: target %v, want 20.5 (22.5 - 2.0)", target)
		}

		r.run(roomSettle, constant(21), 20)
		if view, _ := r.view(); view["shiftC"] != -1.0 {
			t.Fatalf("shiftC = %v, want -1", view["shiftC"])
		}
		wantTags(t, "the overlay after the shift moved", r.lastWrite(), map[string]string{"6280": "0866", "6240": "02"})
		target = mustTarget(t, r.url, 3)
		if target["targetC"] != 20.5 {
			t.Errorf("target after the shift moved = %v, want 20.5 still", target)
		}
		for _, w := range r.writes[n:] {
			sp, err := strconv.ParseUint(w.tags["6280"], 16, 16)
			if err != nil || sp < 2150 || sp > 2250 {
				t.Errorf("write %v: 6280 outside the 21.5..22.5 the corrected 20.5 gives", w.tags)
			}
		}
	})
	t.Run("away", func(t *testing.T) {
		r := newRoomRig(t, 0, endManual)
		r.run(roomSettle, constant(22), 20)
		if code := postJSON(t, http.MethodPatch, r.url+"/api/home", map[string]any{"away": true}, &map[string]any{}); code != http.StatusOK {
			t.Fatalf("away: code=%d", code)
		}
		r.deliverAll()
		wantTags(t, "the switch to away", r.lastWrite(), map[string]string{"6200": "06a4"})
		r.run(roomSettle, constant(21), 20)
		wantTags(t, "away after the shift moved", r.lastWrite(), map[string]string{"6200": "0640"})
	})
}

// TestRoomTempFeedsTheLearners: the early start, the flow adaptation and the
// plant episodes read the zone temperature through zoneReport and plantSample,
// which give the external temperature while it is fresh and the valve's with
// the shift after.
func TestRoomTempFeedsTheLearners(t *testing.T) {
	r := newRoomRig(t, 0, nil)
	zoneAct(t, r.d, r.in, 22, 50)
	if _, code := r.push(20.4); code != http.StatusOK {
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
}

// TestRoomTempMaxAgeOption: room_temp_max_age_sec defaults to 1800, a PATCH
// stores it, and a value that is not positive is refused.
func TestRoomTempMaxAgeOption(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	if got := a.get("/api/install")["room_temp_max_age_sec"]; got != 1800.0 {
		t.Errorf("default room_temp_max_age_sec = %v, want 1800", got)
	}
	a.mutate(http.MethodPatch, "/api/install", map[string]any{"room_temp_max_age_sec": 900})
	if got := a.get("/api/install")["room_temp_max_age_sec"]; got != 900.0 {
		t.Errorf("room_temp_max_age_sec after the patch = %v, want 900", got)
	}
	stored := loadStored(t, a)
	if got := stored.Options.RoomTempMaxAge(); got != 15*time.Minute {
		t.Errorf("stored limit = %v, want 15m", got)
	}
	if code, res := a.call(http.MethodPatch, "/api/install", map[string]any{"room_temp_max_age_sec": 0}); code != http.StatusBadRequest {
		t.Errorf("zero limit: code=%d %v, want 400", code, res)
	}
}
