package daemon

// history_test: the optional statistics. Reports, counters and clocks are
// synthetic. A test reports first and then moves the clock to the next slot
// before it samples: the engine's own one-second tick then samples the same
// state, so it cannot change what the test reads.

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"freedo/tado"
)

// historyDaemon starts the early start daemon on a new store and returns the
// path of the state database too.
func historyDaemon(t *testing.T, edit func(*tado.Install)) (*daemon, string, *testClock, *tado.Install, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "freedo-state.db")
	d, url, clock, in := newEarlyDaemon(t, false, newTestStore(t, path), edit)
	return d, url, clock, in, path
}

func putHistory(t *testing.T, url string, body map[string]any) (int, map[string]any) {
	t.Helper()
	var resp map[string]any
	code := postJSON(t, http.MethodPut, url+"/api/history", body, &resp)
	return code, resp
}

func enableHistory(t *testing.T, url string) {
	t.Helper()
	if code, resp := putHistory(t, url, map[string]any{"enabled": true}); code != http.StatusOK || resp["enabled"] != true {
		t.Fatalf("PUT /api/history enabled: code=%d %v", code, resp)
	}
}

// sampleHistory runs the history tick on the engine goroutine.
func sampleHistory(t *testing.T, d *daemon) {
	t.Helper()
	if _, err := d.do(func(e *tado.EmuState) (any, error) { d.tickHistory(e); return nil, nil }); err != nil {
		t.Fatal(err)
	}
}

// historyStep moves the clock by step and samples.
func historyStep(t *testing.T, d *daemon, clock *testClock, step time.Duration) {
	t.Helper()
	clock.advance(step)
	sampleHistory(t, d)
}

func setCounters(t *testing.T, d *daemon, m3, kwh float64) {
	t.Helper()
	if _, err := d.do(func(*tado.EmuState) (any, error) {
		d.energy.totalM3, d.energy.totalKWh = m3, kwh
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

type historyDay struct {
	Date     string        `json:"date"`
	First    *string       `json:"first"`
	Samples  []zoneSample  `json:"samples"`
	Episodes []heatSummary `json:"episodes"`
}

func zoneDay(t *testing.T, url string, zone, query string) (int, historyDay) {
	t.Helper()
	var day historyDay
	code := getJSON(t, url+"/api/history/zones/"+zone+query, &day)
	return code, day
}

type historyEnergy struct {
	Unit      string     `json:"unit"`
	FuelPrice *float64   `json:"fuelPrice"`
	Currency  string     `json:"currency"`
	Days      []dayTotal `json:"days"`
}

func energyDays(t *testing.T, url, query string) (int, historyEnergy) {
	t.Helper()
	var e historyEnergy
	code := getJSON(t, url+"/api/history/energy"+query, &e)
	return code, e
}

func TestHistoryPath(t *testing.T) {
	for in, want := range map[string]string{
		"/data/freedo-state.db": "/data/freedo-state-history.db",
		"state":                 "state-history.db",
		"/x/a.b/state.sqlite":   "/x/a.b/state-history.db",
	} {
		if got := historyPath(in); got != want {
			t.Errorf("historyPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestHistoryOffByDefault: no file and no sample while off, and the read
// routes say how to turn the statistics on.
func TestHistoryOffByDefault(t *testing.T) {
	d, url, _, _, path := historyDaemon(t, nil)
	var view map[string]any
	if code := getJSON(t, url+"/api/history", &view); code != http.StatusOK {
		t.Fatalf("GET /api/history: code=%d", code)
	}
	if view["available"] != true || view["enabled"] != false || view["sizeBytes"] != 0.0 || view["since"] != nil ||
		view["currency"] != "EUR" || view["fuelPrice"] != nil || view["intervalSec"] != 300.0 {
		t.Errorf("history view = %v, want available, off, empty, EUR and no price", view)
	}
	sampleHistory(t, d)
	if _, err := os.Stat(historyPath(path)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("history file while off: %v, want none", err)
	}
	if code, _ := zoneDay(t, url, "3", ""); code != http.StatusConflict {
		t.Errorf("GET zone history while off: code=%d, want 409", code)
	}
	if code, _ := energyDays(t, url, ""); code != http.StatusConflict {
		t.Errorf("GET energy history while off: code=%d, want 409", code)
	}
}

// TestHistorySamplesTheRoom: one sample per five-minute slot with the room
// temperature, the target and the demand of the zone report.
func TestHistorySamplesTheRoom(t *testing.T) {
	d, url, clock, in, _ := historyDaemon(t, nil)
	zoneAct(t, d, in, 17.0, 0)
	enableHistory(t, url)
	sampleHistory(t, d)
	zoneAct(t, d, in, 16.8, 80)
	historyStep(t, d, clock, 5*time.Minute)
	historyStep(t, d, clock, 2*time.Minute)

	code, day := zoneDay(t, url, "3", "?date=2026-01-05")
	if code != http.StatusOK {
		t.Fatalf("GET zone history: code=%d", code)
	}
	if len(day.Samples) != 2 {
		t.Fatalf("samples = %+v, want two: the third tick is in the second slot", day.Samples)
	}
	want := []struct {
		at     time.Time
		tempC  float64
		demand int
	}{{windowStart, 17.0, 0}, {windowStart.Add(5 * time.Minute), 16.8, 80}}
	for i, w := range want {
		s := day.Samples[i]
		if s.AtMs != w.at.UnixMilli() || s.TempC == nil || *s.TempC != w.tempC || s.DemandPct == nil ||
			*s.DemandPct != w.demand || s.TargetC == nil || *s.TargetC != 17 || !s.On || s.Window || s.Away ||
			s.HumidityPct != nil {
			t.Errorf("sample %d = %+v, want %v at %v C, %d %% demand, target 17 C", i, s, w.at, w.tempC, w.demand)
		}
	}
	if day.First == nil || *day.First != "2026-01-05" {
		t.Errorf("first = %v, want 2026-01-05", day.First)
	}
	if code, hot := zoneDay(t, url, "0", "?date=2026-01-05"); code != http.StatusOK || len(hot.Samples) != 2 ||
		hot.Samples[0].DemandPct != nil {
		t.Errorf("hot water history: code=%d %+v, want two samples without demand", code, hot.Samples)
	}
	if code, other := zoneDay(t, url, "3", "?date=2026-01-04"); code != http.StatusOK || len(other.Samples) != 0 {
		t.Errorf("day before: code=%d %+v, want no sample", code, other.Samples)
	}
}

// TestHistoryStaleReportIsEmpty: a zone report older than offline_after_sec
// gives no temperature and no demand.
func TestHistoryStaleReportIsEmpty(t *testing.T) {
	d, url, clock, in, _ := historyDaemon(t, nil)
	zoneAct(t, d, in, 17.0, 10)
	clock.advance(in.Options.OfflineAfter() + 5*time.Minute)
	enableHistory(t, url)
	sampleHistory(t, d)
	date := clock.now().Format(time.DateOnly)
	_, day := zoneDay(t, url, "3", "?date="+date)
	if len(day.Samples) != 1 || day.Samples[0].TempC != nil || day.Samples[0].DemandPct != nil {
		t.Errorf("samples = %+v, want one without temperature and demand", day.Samples)
	}
}

// TestHistoryCountsTheFuelOfTheDay: each step adds the counter delta times the
// meter factor in force; the first sample and a step over a long gap add none.
func TestHistoryCountsTheFuelOfTheDay(t *testing.T) {
	d, url, clock, _, _ := historyDaemon(t, nil)
	setCounters(t, d, 10, 100)
	enableHistory(t, url)
	sampleHistory(t, d)
	setCounters(t, d, 11.5, 112)
	historyStep(t, d, clock, 5*time.Minute)
	if _, err := d.do(func(*tado.EmuState) (any, error) {
		d.energy.gasReadings = []gasReading{{At: windowStart, Reading: 100, Estimate: 0}, {At: windowStart, Reading: 111, Estimate: 10}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	setCounters(t, d, 12.5, 120)
	historyStep(t, d, clock, 5*time.Minute)
	setCounters(t, d, 20, 180)
	historyStep(t, d, clock, 2*time.Hour)
	setCounters(t, d, 21, 188)
	historyStep(t, d, clock, 5*time.Minute)

	code, e := energyDays(t, url, "?from=2026-01-05&to=2026-01-05")
	if code != http.StatusOK || len(e.Days) != 1 {
		t.Fatalf("energy history: code=%d %+v, want one day", code, e)
	}
	got := e.Days[0]
	if got.Day != "2026-01-05" || !nearly(got.FuelEstimate, 1.5+1+1) || !nearly(got.Fuel, 1.5+1.1+1.1) ||
		!nearly(got.HeatKWh, 12+8+8) || got.BurnerMin != 0 {
		t.Errorf("day = %+v, want an estimate of 3.5, 3.7 with the factor, 28 kWh and no burner report", got)
	}
	if e.Unit != "m3" || e.Currency != "EUR" || e.FuelPrice != nil {
		t.Errorf("energy fields = %+v, want m3 in EUR without a price", e)
	}
}

func nearly(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestHistoryDayFollowsTheClock: a sample after local midnight belongs to the
// next local day.
func TestHistoryDayFollowsTheClock(t *testing.T) {
	plusOne := 60
	d, url, clock, _, _ := historyDaemon(t, func(in *tado.Install) { in.Options.UTCOffsetMin = &plusOne })
	clock.advance(time.Date(2026, 1, 5, 23, 30, 0, 0, time.UTC).Sub(clock.now()))
	enableHistory(t, url)
	sampleHistory(t, d)
	if _, e := energyDays(t, url, "?from=2026-01-01&to=2026-01-31"); len(e.Days) != 1 || e.Days[0].Day != "2026-01-06" {
		t.Errorf("days = %+v, want the sample of 23:30 UTC on 2026-01-06 at UTC+1", e.Days)
	}
	if _, day := zoneDay(t, url, "3", "?date=2026-01-06"); len(day.Samples) != 1 {
		t.Errorf("samples of 2026-01-06 = %+v, want the one of 00:30 local", day.Samples)
	}
	if _, day := zoneDay(t, url, "3", "?date=2026-01-05"); len(day.Samples) != 0 {
		t.Errorf("samples of 2026-01-05 = %+v, want none", day.Samples)
	}
}

// TestHistoryPrune: samples and episodes older than 90 days and daily totals
// before the first of 13 months go.
func TestHistoryPrune(t *testing.T) {
	h, err := openHistory(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	old, recent := now.AddDate(0, 0, -91), now.AddDate(0, 0, -89)
	for _, at := range []time.Time{old, recent} {
		if err := h.add(ctx, at, []zoneSample{{zone: 3, On: true}}, boilerSample{}, dayTotal{Day: at.Format(time.DateOnly)}); err != nil {
			t.Fatal(err)
		}
		if err := h.addEpisode(ctx, 3, heatSummary{StartedMs: at.UnixMilli(), EndedMs: at.UnixMilli(), EndReason: "settled"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, day := range []string{"2025-09-30", "2025-10-01"} {
		if err := h.add(ctx, now, nil, boilerSample{}, dayTotal{Day: day}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.prune(ctx, now, time.UTC); err != nil {
		t.Fatal(err)
	}
	samples, _ := h.zoneSamples(ctx, 3, old.Add(-time.Hour), now)
	if len(samples) != 1 || samples[0].AtMs != recent.Unix()*1000 {
		t.Errorf("samples after prune = %+v, want the one of 89 days ago", samples)
	}
	eps, _ := h.episodes(ctx, 3, old.Add(-time.Hour), now)
	if len(eps) != 1 || eps[0].StartedMs != recent.UnixMilli() {
		t.Errorf("episodes after prune = %+v, want the one of 89 days ago", eps)
	}
	days, _ := h.days(ctx, "2025-01-01", "2026-12-31")
	if len(days) != 3 || days[0].Day != "2025-10-01" {
		t.Errorf("days after prune = %+v, want 2025-10-01 and the two days of the samples", days)
	}
}

// TestHistoryOffKeepsOrDeletes: off keeps the file and its rows, off with
// delete removes it, and delete with the statistics on is refused.
func TestHistoryOffKeepsOrDeletes(t *testing.T) {
	d, url, _, _, path := historyDaemon(t, nil)
	enableHistory(t, url)
	sampleHistory(t, d)
	code, view := putHistory(t, url, map[string]any{"enabled": false})
	if code != http.StatusOK || view["enabled"] != false || view["sizeBytes"].(float64) <= 0 {
		t.Fatalf("off: code=%d %v, want off with the file kept", code, view)
	}
	if code, _ := zoneDay(t, url, "3", ""); code != http.StatusConflict {
		t.Errorf("GET zone history while off: code=%d, want 409", code)
	}
	enableHistory(t, url)
	if _, day := zoneDay(t, url, "3", "?date=2026-01-05"); len(day.Samples) != 1 {
		t.Errorf("samples after off and on = %+v, want the kept one", day.Samples)
	}
	if code, _ := putHistory(t, url, map[string]any{"delete": true}); code != http.StatusBadRequest {
		t.Errorf("delete while on: code=%d, want 400", code)
	}
	code, view = putHistory(t, url, map[string]any{"enabled": false, "delete": true})
	if code != http.StatusOK || view["enabled"] != false || view["sizeBytes"] != 0.0 {
		t.Fatalf("off and delete: code=%d %v", code, view)
	}
	for _, name := range []string{historyPath(path), historyPath(path) + "-wal", historyPath(path) + "-shm"} {
		if _, err := os.Stat(name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s after delete: %v, want none", name, err)
		}
	}
}

// TestHistorySettings: the checks of PUT /api/history, and the settings and
// the open file across a restart.
func TestHistorySettings(t *testing.T) {
	_, url, _, _, path := historyDaemon(t, nil)
	for _, body := range []map[string]any{
		{}, {"fuelPrice": -1}, {"fuelPrice": 1001}, {"currency": "eur"}, {"currency": "EURO"}, {"enabled": "yes"},
	} {
		if code, resp := putHistory(t, url, body); code != http.StatusBadRequest {
			t.Errorf("PUT %v: code=%d %v, want 400", body, code, resp)
		}
	}
	code, view := putHistory(t, url, map[string]any{"enabled": true, "fuelPrice": 1.05, "currency": "CHF"})
	if code != http.StatusOK || view["fuelPrice"] != 1.05 || view["currency"] != "CHF" {
		t.Fatalf("PUT price: code=%d %v", code, view)
	}

	store := newTestStore(t, filepath.Join(filepath.Dir(path), "freedo-state.db"))
	cfg, err := store.HistorySettings(context.Background())
	if err != nil || !cfg.Enabled || cfg.FuelPrice == nil || *cfg.FuelPrice != 1.05 || cfg.Currency != "CHF" {
		t.Fatalf("stored settings = %+v, %v", cfg, err)
	}
	d2, _ := newMeterDaemon(t, windowInstall(t, nil), newTestClock(windowStart), store)
	ok, err := d2.do(func(*tado.EmuState) (any, error) {
		if err := d2.restoreHistory(); err != nil {
			return nil, err
		}
		return d2.history != nil && d2.historyCfg.Enabled, nil
	})
	if err != nil || ok != true {
		t.Errorf("restored history open = %v, %v, want an open file", ok, err)
	}

	code, view = putHistory(t, url, map[string]any{"fuelPrice": nil})
	if code != http.StatusOK || view["fuelPrice"] != nil || view["currency"] != "CHF" {
		t.Errorf("PUT null price: code=%d %v, want no price and the currency kept", code, view)
	}
}

func TestHistoryWithoutDatabase(t *testing.T) {
	_, url := newMeterDaemon(t, windowInstall(t, nil), newTestClock(windowStart), nil)
	var view map[string]any
	if code := getJSON(t, url+"/api/history", &view); code != http.StatusOK || view["available"] != false {
		t.Errorf("GET /api/history: code=%d %v, want unavailable", code, view)
	}
	if code, _ := putHistory(t, url, map[string]any{"enabled": true}); code != http.StatusConflict {
		t.Errorf("PUT enabled without a database: code=%d, want 409", code)
	}
}

func TestHistoryBadQueries(t *testing.T) {
	_, url, _, _, _ := historyDaemon(t, nil)
	enableHistory(t, url)
	for _, c := range []struct {
		path string
		code int
	}{
		{"/api/history/zones/3?date=05-01-2026", http.StatusBadRequest},
		{"/api/history/zones/x", http.StatusBadRequest},
		{"/api/history/zones/4", http.StatusNotFound},
		{"/api/history/energy?from=2026-13-01", http.StatusBadRequest},
		{"/api/history/energy?from=2026-02-01&to=2026-01-01", http.StatusBadRequest},
	} {
		var resp map[string]any
		if code := getJSON(t, url+c.path, &resp); code != c.code {
			t.Errorf("GET %s: code=%d %v, want %d", c.path, code, resp, c.code)
		}
	}
}

// TestHistoryKeepsTheHeatUp: a finished heat-up episode shows on its day with
// its summary.
func TestHistoryKeepsTheHeatUp(t *testing.T) {
	d, url, clock, in, _ := historyDaemon(t, nil)
	enableHistory(t, url)
	plantHeatUp(t, d, url, in, clock)
	at(t, d, in, clock, "09:40", math.NaN(), 0)
	_, day := zoneDay(t, url, "3", "?date=2026-01-05")
	if len(day.Episodes) != 1 {
		t.Fatalf("episodes = %+v, want the heat-up", day.Episodes)
	}
	ep := day.Episodes[0]
	if ep.StartedMs != ms("07:00") || ep.EndedMs != ms("09:40") || ep.StartC != 16.8 || ep.TargetC == nil ||
		*ep.TargetC != 21 || ep.CrossedMs == nil || *ep.CrossedMs != ms("08:45") || ep.EndReason != "settled" ||
		ep.OvershootC == nil || ep.DeadTimeSec == nil {
		t.Errorf("episode = %+v, want 07:00 -> 09:40 from 16.8 to 21 C, crossed at 08:45, settled", ep)
	}
}
