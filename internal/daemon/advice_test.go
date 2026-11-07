package daemon

import (
	"maps"
	"math"
	"net/http"
	"slices"
	"testing"
	"time"
)

// adviceRows builds boiler samples every 5 minutes from t0 with the given fuel
// steps, burner flags and outdoor temperature.
func adviceRows(t0 time.Time, steps []float64, burner []bool, outdoor float64) []adviceBoilerRow {
	rows := []adviceBoilerRow{}
	fuel := 10.0
	for i := 0; i <= len(steps); i++ {
		r := adviceBoilerRow{at: t0.Add(time.Duration(i) * historyEvery), fuel: fuel, outdoorC: new(outdoor)}
		if i < len(burner) {
			r.burner = new(burner[i])
		}
		rows = append(rows, r)
		if i < len(steps) {
			fuel += steps[i]
		}
	}
	return rows
}

func TestAdviceSplitFuel(t *testing.T) {
	t0 := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	// Step 0 heats zones 3 and 5, step 1 heats nothing with the burner on (hot
	// water), step 2 burns with the burner off (unattributed).
	boiler := adviceRows(t0, []float64{0.8, 0.1, 0.05}, []bool{true, true, false}, 5)
	zones := []adviceZoneRow{
		{zone: 3, at: t0, tempC: new(20.0), demandPct: new(60)},
		{zone: 5, at: t0, tempC: new(18.0), demandPct: new(20)},
		{zone: 3, at: t0.Add(historyEvery), demandPct: new(0)},
		{zone: 5, at: t0.Add(historyEvery), demandPct: new(0)},
	}
	equal := map[int]float64{3: 1, 5: 1}
	f := adviceSplitFuel(boiler, adviceByTime(zones), equal, time.UTC, time.Time{})
	if !nearly(f.split.Fuel, 0.95) || !nearly(f.split.Heating, 0.8) || !nearly(f.split.HotWater, 0.1) || !nearly(f.split.Unattributed, 0.05) {
		t.Fatalf("split = %+v", f.split)
	}
	if !nearly(f.zone[3], 0.6) || !nearly(f.zone[5], 0.2) {
		t.Errorf("zone fuel = %v, want 0.6 and 0.2 by demand", f.zone)
	}
	if !nearly(f.heatDay["2026-01-05"], 0.8) || !nearly(f.totalDay["2026-01-05"], 0.95) {
		t.Errorf("day fuel = %v of %v", f.heatDay, f.totalDay)
	}

	// Radiator power moves the split: zone 5 has three times the radiators.
	weighted := adviceSplitFuel(boiler, adviceByTime(zones), map[int]float64{3: 1000, 5: 3000}, time.UTC, time.Time{})
	if !nearly(weighted.zone[3], 0.4) || !nearly(weighted.zone[5], 0.4) {
		t.Errorf("weighted zone fuel = %v, want 0.4 each (60 x 1000 and 20 x 3000)", weighted.zone)
	}

	// A heating zone without a fresh report may have heated: with the burner
	// on and no known demand the step is unattributed, not hot water.
	stale := []adviceZoneRow{{zone: 3, at: t0.Add(historyEvery), demandPct: new(0)}, {zone: 5, at: t0.Add(historyEvery)}}
	if f := adviceSplitFuel(boiler, adviceByTime(stale), equal, time.UTC, t0.Add(2*historyEvery)); f.split.HotWater != 0 || !nearly(f.split.Unattributed, 0.15) {
		t.Errorf("step with a zone of unknown demand = %+v, want unattributed", f.split)
	}

	// since keeps the steps that end from it on.
	if f := adviceSplitFuel(boiler, adviceByTime(zones), equal, time.UTC, t0.Add(2*historyEvery)); !nearly(f.split.Fuel, 0.15) {
		t.Errorf("fuel since the second step = %v, want 0.15", f.split.Fuel)
	}

	// A step across a gap holds fuel the statistics did not see.
	gap := []adviceBoilerRow{boiler[0], {at: t0.Add(2 * time.Hour), fuel: 15}}
	if f := adviceSplitFuel(gap, adviceByTime(zones), equal, time.UTC, time.Time{}); f.split.Fuel != 0 {
		t.Errorf("a step across a gap counted %v", f.split.Fuel)
	}
}

// TestAdviceDegreeHoursSameSteps: two equal rooms burn the same per
// degree-hour, also when one of them often lacks a temperature.
func TestAdviceDegreeHoursSameSteps(t *testing.T) {
	t0 := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	steps := make([]float64, 100)
	boiler := adviceRows(t0, steps, nil, 0)
	var zones []adviceZoneRow
	for i := range 100 {
		at := t0.Add(time.Duration(i) * historyEvery)
		zones = append(zones, adviceZoneRow{zone: 3, at: at, tempC: new(20.0)})
		if i%10 == 0 {
			zones = append(zones, adviceZoneRow{zone: 5, at: at, tempC: new(20.0)})
		}
	}
	own, house := adviceDegreeHours(boiler, adviceByTime(zones), map[int]float64{3: 1, 5: 1})
	if !nearly(own[3]/house[3], 1) || !nearly(own[5]/house[5], 1) {
		t.Errorf("own/house = %v / %v, want 1 for both rooms at the house temperature", own, house)
	}
}

// adviceDays builds valid days whose heating fuel follows a x degree-hours + b.
func adviceDays(n int, a, b float64) ([]dayTotal, map[string]float64) {
	days := []dayTotal{}
	share := map[string]float64{}
	for i := range n {
		outdoor := -2 + float64(i%10)*1.5
		indoor := 20.0
		day := time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
		dh := (indoor - outdoor) * 24
		// One day in two burns 5 % above or below the line.
		noise := 1 + 0.05*float64(i%2*2-1)
		days = append(days, dayTotal{
			Day: day, Fuel: (a*dh + b) * noise / 0.8, OutdoorC: new(outdoor), IndoorC: new(indoor), Coverage: new(1.0),
			samples: 288, outdoorN: 288,
		})
		share[day] = 0.8
	}
	return days, share
}

func TestAdviceFit(t *testing.T) {
	days, share := adviceDays(20, 0.01, -0.5)
	m := adviceFit(days, share)
	if !m.Usable || m.ValidDays != 20 || math.Abs(*m.A-0.01) > 0.001 || *m.R2 < 0.9 || *m.AErr <= 0 {
		t.Fatalf("model = %+v (a %v, r2 %v), want a usable fit near 0.01", m, *m.A, *m.R2)
	}

	few, fewShare := adviceDays(adviceMinDays-1, 0.01, -0.5)
	if m := adviceFit(few, fewShare); m.Usable || m.Reason == "" || m.A != nil {
		t.Errorf("model of %d days = %+v, want a reason and no fit", adviceMinDays-1, m)
	}

	same, sameShare := adviceDays(20, 0.01, -0.5)
	for i := range same {
		same[i].OutdoorC = new(5.0)
	}
	if m := adviceFit(same, sameShare); m.Usable || m.Reason != "the valid days had too similar weather" {
		t.Errorf("model on one weather = %+v", m)
	}

	// Days that do not count: a day of schema 1, a short day, a day without
	// enough outdoor readings, a day without samples to split its fuel, a day
	// that burned only for hot water.
	bad, badShare := adviceDays(20, 0.01, -0.5)
	bad[0].Coverage, bad[0].OutdoorC = nil, nil
	bad[1].Coverage = new(0.5)
	bad[2].outdoorN = 100
	delete(badShare, bad[3].Day)
	badShare[bad[4].Day] = 0
	if m := adviceFit(bad, badShare); m.ValidDays != 15 {
		t.Errorf("valid days = %d, want 15", m.ValidDays)
	}

	// The degree-hours count the hours sampled: a day of 23 hours (276 slots)
	// at the same temperatures has fewer than a day of 24.
	dst, dstShare := adviceDays(20, 0.01, -0.5)
	for i := range dst {
		if i%2 == 0 {
			dst[i].samples, dst[i].outdoorN = 276, 276
		}
	}
	if a, b := adviceFit(dst, dstShare), adviceFit(days, share); *a.A == *b.A {
		t.Error("the fit does not follow the hours sampled")
	}
}

func TestHistoryAdviceRoute(t *testing.T) {
	d, url, clock, _, _ := historyDaemon(t, nil)
	var body map[string]any
	if code := getJSON(t, url+"/api/history/advice", &body); code != http.StatusConflict {
		t.Errorf("advice with the statistics off: code=%d, want 409", code)
	}
	enableHistory(t, url)
	if code := getJSON(t, url+"/api/history/advice?days=30", &body); code != http.StatusBadRequest {
		t.Errorf("advice of 30 days: code=%d, want 400", code)
	}
	setCounters(t, d, 10, 100)
	sampleHistory(t, d)
	setCounters(t, d, 10.5, 105)
	historyStep(t, d, clock, 5*time.Minute)
	var got struct {
		Days  int         `json:"days"`
		From  string      `json:"from"`
		Split adviceSplit `json:"split"`
		Model adviceModel `json:"model"`
	}
	if code := getJSON(t, url+"/api/history/advice?days=7", &got); code != http.StatusOK {
		t.Fatalf("advice: code=%d", code)
	}
	if got.Days != 7 || got.From != "2025-12-30" || !nearly(got.Split.Fuel, 0.5) || got.Model.Usable || got.Model.Reason == "" {
		t.Errorf("advice = %+v, want 7 days from 2025-12-30, 0.5 of fuel and no usable model", got)
	}
	if !nearly(got.Split.Heating+got.Split.HotWater+got.Split.Unattributed, got.Split.Fuel) {
		t.Errorf("split parts do not add up: %+v", got.Split)
	}
}

func TestAdviceWeights(t *testing.T) {
	in := *defaultInstall
	in.Options.RadiatorsW = nil
	w, weighted := adviceWeights(&in)
	if weighted || len(w) == 0 {
		t.Fatalf("weights without radiators = %v, %t", w, weighted)
	}
	for zone, v := range w {
		if v != 1 {
			t.Errorf("zone %d weight %v, want 1 without radiators", zone, v)
		}
	}
	// One zone with a power and the others without: equal weights.
	zones := slices.Sorted(maps.Keys(w))
	in.Options.RadiatorsW = map[int]float64{zones[0]: 2000}
	if w, weighted := adviceWeights(&in); weighted || w[zones[0]] != 1 {
		t.Errorf("weights with one radiator = %v, %t, want equal", w, weighted)
	}
	in.Options.RadiatorsW = map[int]float64{}
	for i, zone := range zones {
		in.Options.RadiatorsW[zone] = float64(1000 * (i + 1))
	}
	if w, weighted := adviceWeights(&in); !weighted || w[zones[0]] != 1000 || w[zones[len(zones)-1]] != float64(1000*len(zones)) {
		t.Errorf("weights with every radiator = %v, %t", w, weighted)
	}
}
