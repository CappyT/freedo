package daemon

// early_start_test: the heat-up learner and the early start of a program block.
// Reports, programs and clocks are synthetic; zone 3 has one valve.

import (
	"context"
	"math"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"freedo/tado"
)

const earlyValve = "VA1000000002"

var earlyMID atomic.Uint32

// newEarlyDaemon starts a daemon at 05:00 on a Monday whose zone 3 runs 17 C
// from midnight and 21 C from 07:00.
func newEarlyDaemon(t *testing.T, enabled bool, store *stateStore, edit func(*tado.Install)) (*daemon, string, *testClock, *tado.Install) {
	t.Helper()
	in := windowInstall(t, func(in *tado.Install) {
		if enabled {
			in.Options.EarlyStartZones = map[int]bool{3: true}
		}
		if edit != nil {
			edit(in)
		}
	})
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, store)
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule", allDayProgram(
		map[string]any{"minute": 0, "setpoint_c": 17.0},
		map[string]any{"minute": 420, "setpoint_c": 21.0},
	), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	seedBlock(t, d, in, earlyValve, 3)
	return d, url, clock, in
}

// zoneAct sends the zone report of zone 3: temperature 4060 and demand 40a0.
func zoneAct(t *testing.T, d *daemon, in *tado.Install, tempC float64, demand int) {
	t.Helper()
	centi := int(math.Round(tempC * 100))
	body := append(tado.TLV{Tag: "4060", Val: []byte{byte(centi >> 8), byte(centi)}}.Encode(),
		tado.TLV{Tag: "40a0", Val: []byte{byte(demand)}}.Encode()...)
	feed(t, d, in, earlyValve, false, tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x6000 + uint16(earlyMID.Add(1)),
		Path: []string{"h", "7654321", "z", "3", "act"}, Payload: body,
	})
}

// at moves the clock to hh:mm of the start day, reports tempC when it is not
// NaN, and ticks the home state.
func at(t *testing.T, d *daemon, in *tado.Install, clock *testClock, hhmm string, tempC float64, demand int) {
	t.Helper()
	when, err := time.Parse("15:04", hhmm)
	if err != nil {
		t.Fatal(err)
	}
	target := windowStart.Truncate(24 * time.Hour).Add(time.Duration(when.Hour())*time.Hour + time.Duration(when.Minute())*time.Minute)
	clock.advance(target.Sub(clock.now()))
	if !math.IsNaN(tempC) {
		zoneAct(t, d, in, tempC, demand)
	}
	tickHome(t, d)
}

func modelOf(t *testing.T, d *daemon, zone int) thermalModel {
	t.Helper()
	v, err := d.do(func(*tado.EmuState) (any, error) {
		if m := d.thermal[zone]; m != nil {
			return *m, nil
		}
		return thermalModel{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return v.(thermalModel)
}

// cleanHeatUp runs 17 -> 21 C from 07:00. The crossing interpolates to 08:40
// between 20.6 C at 08:30 and 21.2 C at 08:45: 4 C in 100 min, 2.4 C/h.
func cleanHeatUp(t *testing.T, d *daemon, in *tado.Install, clock *testClock) {
	t.Helper()
	at(t, d, in, clock, "06:50", 17.0, 0)
	at(t, d, in, clock, "07:00", math.NaN(), 0)
	at(t, d, in, clock, "07:30", 18.0, 80)
	at(t, d, in, clock, "08:00", 19.5, 70)
	at(t, d, in, clock, "08:30", 20.6, 40)
	at(t, d, in, clock, "08:45", 21.2, 10)
}

func TestHeatUpLearnsCleanEpisode(t *testing.T) {
	d, _, clock, in := newEarlyDaemon(t, false, nil, nil)
	cleanHeatUp(t, d, in, clock)
	m := modelOf(t, d, 3)
	if len(m.Episodes) != 1 {
		t.Fatalf("episodes = %+v, want one", m.Episodes)
	}
	ep := m.Episodes[0]
	if ep.StartC != 17 || ep.TargetC != 21 || ep.Minutes != 100 || ep.Rate != 2.4 || ep.OutdoorC != nil {
		t.Errorf("episode = %+v, want 17 -> 21 C in 100 min at 2.4 C/h, no outdoor", ep)
	}
	if want := windowStart.Add(3*time.Hour + 40*time.Minute).UnixMilli(); ep.EndedMs != want {
		t.Errorf("ended = %d, want %d", ep.EndedMs, want)
	}
}

func TestHeatUpRejectsDisturbedEpisode(t *testing.T) {
	cases := []struct {
		name    string
		disturb func(t *testing.T, d *daemon, url string, in *tado.Install, clock *testClock)
	}{
		{"open window", func(t *testing.T, d *daemon, url string, in *tado.Install, clock *testClock) {
			if _, code := postWindow(t, url, 3, true); code != http.StatusOK {
				t.Fatalf("open: code=%d", code)
			}
			tickHome(t, d)
			if _, code := postWindow(t, url, 3, false); code != http.StatusOK {
				t.Fatalf("close: code=%d", code)
			}
		}},
		{"overlay", func(t *testing.T, d *daemon, url string, in *tado.Install, clock *testClock) {
			seedBlock(t, d, in, earlyValve, 3, tado.TLV{Tag: "6240", Val: []byte{0x02}})
			seedBlock(t, d, in, earlyValve, 3, tado.TLV{Tag: "6240", Val: []byte{0x00}})
		}},
		{"away", func(t *testing.T, d *daemon, url string, in *tado.Install, clock *testClock) {
			var res map[string]any
			for _, away := range []bool{true, false} {
				if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": away}, &res); code != http.StatusOK {
					t.Fatalf("away=%t: code=%d %v", away, code, res)
				}
				tickHome(t, d)
			}
		}},
		{"missing reports", func(t *testing.T, d *daemon, url string, in *tado.Install, clock *testClock) {
			clock.advance(61 * time.Minute)
			tickHome(t, d)
			clock.advance(-61 * time.Minute)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, url, clock, in := newEarlyDaemon(t, false, nil, nil)
			at(t, d, in, clock, "06:50", 17.0, 0)
			at(t, d, in, clock, "07:00", math.NaN(), 0)
			at(t, d, in, clock, "07:30", 18.0, 80)
			tc.disturb(t, d, url, in, clock)
			at(t, d, in, clock, "08:00", 19.5, 70)
			at(t, d, in, clock, "08:45", 21.2, 10)
			if m := modelOf(t, d, 3); len(m.Episodes) != 0 {
				t.Errorf("episodes = %+v, want none", m.Episodes)
			}
		})
	}
}

func TestHeatUpModelPersists(t *testing.T) {
	store := newTestStore(t, t.TempDir()+"/state.db")
	d, url, clock, in := newEarlyDaemon(t, false, store, nil)
	cleanHeatUp(t, d, in, clock)
	stored, err := store.ThermalModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m := stored[3]; m == nil || len(m.Episodes) != 1 || m.Episodes[0] != modelOf(t, d, 3).Episodes[0] {
		t.Fatalf("stored = %+v, want the learned episode", stored)
	}

	var view map[string]any
	if code := postJSON(t, http.MethodDelete, url+"/api/zones/3/early-start/model", nil, &view); code != http.StatusOK ||
		view["episodes"].(float64) != 0 {
		t.Fatalf("reset: code=%d %v", code, view)
	}
	if stored, err := store.ThermalModels(context.Background()); err != nil || len(stored) != 0 {
		t.Errorf("stored after the reset = %v err=%v, want none", stored, err)
	}
}

func TestThermalModelRate(t *testing.T) {
	outdoor := func(c float64) *float64 { return &c }
	m := &thermalModel{}
	for i, rate := range []float64{1, 3, 2, 8} {
		m.add(thermalEpisode{EndedMs: int64(i), Rate: rate, OutdoorC: outdoor(float64(i * 5))})
	}
	if got, _ := m.rate(nil, 3); got != 2.5 {
		t.Errorf("median = %g, want 2.5", got)
	}
	if got, _ := m.rate(outdoor(14), 3); got != 3 {
		t.Errorf("nearest to 14 C = %g, want the median of 3, 2, 8", got)
	}
	for i := range tado.EarlyStartEpisodesKept {
		m.add(thermalEpisode{EndedMs: int64(100 + i), Rate: 4})
	}
	if len(m.Episodes) != tado.EarlyStartEpisodesKept || m.Episodes[0].EndedMs != 100 {
		t.Errorf("kept %d episodes from %d, want %d from 100", len(m.Episodes), m.Episodes[0].EndedMs, tado.EarlyStartEpisodesKept)
	}
}

// learned gives zone 3 n episodes at rate C/h.
func learned(t *testing.T, d *daemon, n int, rate float64) {
	t.Helper()
	if _, err := d.do(func(*tado.EmuState) (any, error) {
		m := &thermalModel{}
		for i := range n {
			m.add(thermalEpisode{EndedMs: int64(i), StartC: 17, TargetC: 21, Rate: rate})
		}
		d.thermal[3] = m
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func earlyPlanOf(t *testing.T, d *daemon) (earlyStart, bool) {
	t.Helper()
	type result struct {
		plan earlyStart
		ok   bool
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		plan, ok := d.earlyPlan(e, d.inst.Load(), 3, d.retryNow(), nodeOverlays(e))
		return result{plan, ok}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r := v.(result)
	return r.plan, r.ok
}

func TestEarlyStartLeadIsCapped(t *testing.T) {
	block := windowStart.Add(2 * time.Hour)
	for _, tc := range []struct {
		rate float64
		lead time.Duration
	}{
		{4, time.Hour},
		{2, 2 * time.Hour},
		{1, 2 * time.Hour},
		{8, 30 * time.Minute},
	} {
		d, _, clock, in := newEarlyDaemon(t, true, nil, nil)
		learned(t, d, 3, tc.rate)
		at(t, d, in, clock, "05:00", 17.0, 0)
		plan, ok := earlyPlanOf(t, d)
		if !ok || !plan.boundary.Equal(block) || plan.block.c != 21 || !plan.at.Equal(block.Add(-tc.lead)) {
			t.Errorf("rate %g: plan = %+v ok=%t, want 21 C at %s", tc.rate, plan, ok, block.Add(-tc.lead))
		}
	}
}

func TestEarlyStartCustomCap(t *testing.T) {
	d, url, clock, in := newEarlyDaemon(t, true, nil, func(in *tado.Install) {
		lead := 45 * 60
		in.Options.EarlyStartMaxLeadSec = &lead
	})
	learned(t, d, 3, 1)
	at(t, d, in, clock, "05:00", 17.0, 0)
	var view map[string]any
	if code := getJSON(t, url+"/api/zones/3/early-start", &view); code != http.StatusOK {
		t.Fatalf("get: code=%d %v", code, view)
	}
	next, _ := view["next"].(map[string]any)
	if next == nil || next["at"] != "2026-01-05T06:15:00Z" || next["leadSec"].(float64) != 2700 ||
		view["enabled"] != true || view["episodes"].(float64) != 3 || view["rateCPerHour"].(float64) != 1 {
		t.Errorf("view = %v, want the next early start at 06:15 after 3 episodes at 1 C/h", view)
	}
}

// TestEarlyStartPushesOnce: the 21 C block goes out one hour early, and the
// block change at 07:00 pushes nothing more.
func TestEarlyStartPushesOnce(t *testing.T) {
	d, url, clock, in := newEarlyDaemon(t, true, nil, nil)
	office := wireOf(t, in, earlyValve)
	learned(t, d, 3, 4)
	at(t, d, in, clock, "05:55", 17.0, 0)
	if n := pending(t, d, office); n != 0 {
		t.Fatalf("05:55 queued %d writes, want none", n)
	}
	at(t, d, in, clock, "06:00", math.NaN(), 0)
	if n := pending(t, d, office); n != 1 {
		t.Fatalf("06:00 queued %d writes, want the early block", n)
	}
	wantTags(t, "the early block", nextZoneWrite(t, d, office), map[string]string{"6200": "0834", "6240": "00", "61e0": "01"})
	var view map[string]any
	if code := getJSON(t, url+"/api/zones/3/early-start", &view); code != http.StatusOK || view["active"] == nil {
		t.Errorf("view during the early start = %v, want it active", view)
	}
	dropQueue(t, d, office)

	at(t, d, in, clock, "06:30", 18.5, 60)
	at(t, d, in, clock, "07:00", math.NaN(), 0)
	at(t, d, in, clock, "07:01", math.NaN(), 0)
	if n := pending(t, d, office); n != 0 {
		t.Errorf("the 07:00 block change queued %d writes, want none", n)
	}
	if code := getJSON(t, url+"/api/zones/3/early-start", &view); code != http.StatusOK || view["active"] != nil {
		t.Errorf("view after the block start = %v, want no early start", view)
	}
}

// TestEarlyStartHeld: no early start without enough episodes, with the switch
// off, away, or under an overlay. The block then starts on time, once.
func TestEarlyStartHeld(t *testing.T) {
	cases := []struct {
		name     string
		enabled  bool
		episodes int
		setup    func(t *testing.T, d *daemon, url string, in *tado.Install)
	}{
		{"too few episodes", true, 2, nil},
		{"switch off", false, 3, nil},
		{"away", true, 3, func(t *testing.T, d *daemon, url string, in *tado.Install) {
			var res map[string]any
			if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &res); code != http.StatusOK {
				t.Fatalf("away: code=%d %v", code, res)
			}
		}},
		{"overlay", true, 3, func(t *testing.T, d *daemon, url string, in *tado.Install) {
			seedBlock(t, d, in, earlyValve, 3, tado.TLV{Tag: "6240", Val: []byte{0x02}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, url, clock, in := newEarlyDaemon(t, tc.enabled, nil, nil)
			office := wireOf(t, in, earlyValve)
			learned(t, d, tc.episodes, 4)
			if tc.setup != nil {
				tc.setup(t, d, url, in)
			}
			dropQueue(t, d, office)
			at(t, d, in, clock, "06:30", 17.0, 0)
			if n := pending(t, d, office); n != 0 {
				t.Fatalf("06:30 queued %d writes, want none", n)
			}
			if _, ok := earlyPlanOf(t, d); ok {
				t.Errorf("a plan is due, want none")
			}
			at(t, d, in, clock, "07:00", math.NaN(), 0)
			if n := pending(t, d, office); n != 1 {
				t.Errorf("07:00 queued %d writes, want the block change", n)
			}
		})
	}
}

// TestEarlyStartNeverHotWater: the hot water zone takes no switch, and a plan
// is never made for it even when the options name it.
func TestEarlyStartNeverHotWater(t *testing.T) {
	in := windowInstall(t, nil)
	in.Options.EarlyStartZones = map[int]bool{0: true}
	if err := in.Validate(); err == nil {
		t.Error("an install with early start on the hot water zone validates")
	}
	d, url, _, _ := newEarlyDaemon(t, false, nil, func(in *tado.Install) {
		in.Options.EarlyStartZones = map[int]bool{0: true}
	})
	putHotWaterWeek(t, url)
	if _, err := d.do(func(*tado.EmuState) (any, error) {
		d.thermal[0] = &thermalModel{Episodes: []thermalEpisode{{Rate: 4}, {Rate: 4}, {Rate: 4}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		_, ok := d.earlyPlan(e, d.inst.Load(), 0, d.retryNow(), nodeOverlays(e))
		return ok, nil
	})
	if err != nil || v.(bool) {
		t.Errorf("hot water plan: ok=%v err=%v, want none", v, err)
	}
	var res map[string]any
	if code := getJSON(t, url+"/api/zones/0/early-start", &res); code != http.StatusBadRequest {
		t.Errorf("hot water view: code=%d %v, want 400", code, res)
	}
}

func TestEarlyStartInstallOptions(t *testing.T) {
	store := newTestStore(t, t.TempDir()+"/state.db")
	in := windowInstall(t, func(in *tado.Install) {
		lead, episodes := 3600, 5
		in.Options.EarlyStartZones = map[int]bool{3: true, 10: true}
		in.Options.EarlyStartMaxLeadSec, in.Options.EarlyStartMinEpisodes = &lead, &episodes
	})
	if _, err := store.ReplaceInstall(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Options.EarlyStart(3) || !got.Options.EarlyStart(10) || got.Options.EarlyStart(5) ||
		got.Options.EarlyStartMaxLead() != time.Hour || got.Options.EarlyStartMinEpisodeCount() != 5 {
		t.Errorf("loaded options = %+v", got.Options)
	}
	for _, tc := range []struct {
		zone    int
		enabled bool
	}{{3, false}, {5, true}} {
		if _, err := store.PatchZone(context.Background(), tc.zone, zonePatch{EarlyStart: &tc.enabled}); err != nil {
			t.Fatal(err)
		}
	}
	on := true
	if _, err := store.PatchZone(context.Background(), 0, zonePatch{EarlyStart: &on}); err == nil {
		t.Error("the hot water zone took the early start switch")
	}
	bad := 0
	if _, err := store.PatchInstall(context.Background(), installPatch{EarlyStartMinEpisodes: &bad}); err == nil {
		t.Error("early_start_min_episodes 0 was accepted")
	}
	got, _, err = store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Options.EarlyStart(3) || !got.Options.EarlyStart(5) || !got.Options.EarlyStart(10) {
		t.Errorf("switches after the patch = %v, want 5 and 10", got.Options.EarlyStartZones)
	}
}
