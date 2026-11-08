package daemon

// advice: the model of the consumption advisor. It splits the fuel of the
// statistics between the rooms and the hot water, and fits the fuel a day burns
// for heating on the degree-hours of that day (heat loss follows the indoor to
// outdoor difference). It only reads the statistics.

import (
	"context"
	"database/sql"
	"math"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"freedo/tado"
)

const (
	// adviceMinDays is the fewest valid days a usable model needs.
	adviceMinDays = 14
	// adviceMinCoverage is the share of a day's slots, and of its slots with
	// an outdoor reading, that makes the day valid.
	adviceMinCoverage = 0.9
	// adviceMinSpread is how far apart the degree-hours of the valid days must
	// be, from the lowest to the highest, as a share of their mean: on days of
	// the same weather the slope has nothing to fit.
	adviceMinSpread = 0.3
	// adviceMinR2 is the fit quality a usable model needs.
	adviceMinR2 = 0.6
)

// adviceWindows are the analysis windows in days. adviceDefaultWindow is the
// one of a request that names none.
var adviceWindows = []int{7, 28, 90}

const adviceDefaultWindow = 28

// adviceBoilerRow and adviceZoneRow are the sample columns the advisor reads.
type adviceBoilerRow struct {
	at       time.Time
	fuel     float64
	burner   *bool
	outdoorC *float64
}

type adviceZoneRow struct {
	zone        int
	at          time.Time
	tempC       *float64
	targetC     *float64
	humidityPct *float64
	demandPct   *int
	on          bool
	window      bool
	away        bool
}

// adviceZoneShare is the part of the heating fuel of one heating zone.
// FuelPerDegreeHour is the fuel the zone burns per degree-hour of indoor to
// outdoor difference, nil while the house model is not usable.
type adviceZoneShare struct {
	Zone              int      `json:"zone"`
	Fuel              float64  `json:"fuel"`
	Share             float64  `json:"share"`
	FuelPerDegreeHour *float64 `json:"fuelPerDegreeHour"`
}

// adviceSplit is where the fuel of the window went. Fuel is in fuel_unit and
// carries the meter factor of each day. Weighted is true when every heating
// zone has radiator_w and the split weighs the demand by it.
type adviceSplit struct {
	Fuel         float64           `json:"fuel"`
	Heating      float64           `json:"heating"`
	HotWater     float64           `json:"hotWater"`
	Unattributed float64           `json:"unattributed"`
	Weighted     bool              `json:"weighted"`
	Zones        []adviceZoneShare `json:"zones"`
}

// adviceModel is the house model: heating fuel of a day = A x degree-hours + B.
// AErr is the standard error of A. Reason says why the model is not usable.
type adviceModel struct {
	ValidDays int      `json:"validDays"`
	Usable    bool     `json:"usable"`
	Reason    string   `json:"reason,omitempty"`
	A         *float64 `json:"fuelPerDegreeHour"`
	AErr      *float64 `json:"fuelPerDegreeHourErr"`
	B         *float64 `json:"fuelPerDayOffset"`
	R2        *float64 `json:"r2"`
}

// adviceStepFuel is the fuel of the step between two boiler samples, and false
// for a step across a gap: the counters run while the statistics are off.
func adviceStepFuel(prev, cur adviceBoilerRow) (float64, bool) {
	if cur.at.Sub(prev.at) > historyGapMax {
		return 0, false
	}
	return max(0, cur.fuel-prev.fuel), true
}

// adviceWeights gives each heating zone the weight of its demand: the
// radiator power when every heating zone has one, else 1 for each. A mix
// would weigh the rooms on two scales.
func adviceWeights(inst *tado.Install) (map[int]float64, bool) {
	w := map[int]float64{}
	all := true
	for _, z := range inst.Zones {
		if z.Kind != tado.ZoneHeating {
			continue
		}
		p, ok := inst.Options.RadiatorW(z.Number)
		if !ok {
			all = false
		}
		w[z.Number] = p
	}
	for zone := range w {
		if !all {
			w[zone] = 1
		}
	}
	return w, all && len(w) > 0
}

// adviceByTime indexes the zone samples by their time.
func adviceByTime(zones []adviceZoneRow) map[int64][]adviceZoneRow {
	byAt := map[int64][]adviceZoneRow{}
	for _, z := range zones {
		byAt[z.at.Unix()] = append(byAt[z.at.Unix()], z)
	}
	return byAt
}

// adviceFuel is the raw fuel of the steps that ended from since on, split as
// adviceSplitFuel tells, with the heating fuel of each zone, of each local
// day, and of each zone in each step, keyed by the step's first sample.
type adviceFuel struct {
	split    adviceSplit
	zone     map[int]float64
	heatDay  map[string]float64
	totalDay map[string]float64
	step     map[int64]map[int]float64
}

// adviceSplitFuel splits the fuel of each step, with the zone state of the
// sample at its start. A step with a heating demand goes to the heating zones
// in proportion to demand x weight. A step without a demand goes to hot water
// when the burner is on and every heating zone reported its demand, else it
// is unattributed: a zone without a fresh report may have been heating. A
// step ends on the local day of its end sample, as in the daily totals.
func adviceSplitFuel(boiler []adviceBoilerRow, byAt map[int64][]adviceZoneRow, weights map[int]float64, loc *time.Location, since time.Time) adviceFuel {
	out := adviceFuel{zone: map[int]float64{}, heatDay: map[string]float64{}, totalDay: map[string]float64{}, step: map[int64]map[int]float64{}}
	for i := 1; i < len(boiler); i++ {
		if boiler[i].at.Before(since) {
			continue
		}
		df, ok := adviceStepFuel(boiler[i-1], boiler[i])
		if !ok || df == 0 {
			continue
		}
		day := boiler[i].at.In(loc).Format(time.DateOnly)
		out.totalDay[day] += df
		out.split.Fuel += df
		var total float64
		unknown := false
		parts := map[int]float64{}
		for _, z := range byAt[boiler[i-1].at.Unix()] {
			w, heating := weights[z.zone]
			switch {
			case !heating:
			case z.demandPct == nil:
				unknown = true
			case *z.demandPct > 0:
				parts[z.zone] = float64(*z.demandPct) * w
				total += parts[z.zone]
			}
		}
		switch {
		case total > 0:
			out.split.Heating += df
			out.heatDay[day] += df
			at := boiler[i-1].at.Unix()
			out.step[at] = map[int]float64{}
			for zone, p := range parts {
				out.zone[zone] += df * p / total
				out.step[at][zone] = df * p / total
			}
		case !unknown && boiler[i-1].burner != nil && *boiler[i-1].burner:
			out.split.HotWater += df
		default:
			out.split.Unattributed += df
		}
	}
	return out
}

// adviceSpaces maps each heating zone to its space: the shared_space name for
// zones that share one air volume, else the zone alone. Heat moves freely
// within a space, so the advisor reads its heat loss and its savings per space.
func adviceSpaces(inst *tado.Install) map[int]string {
	out := map[int]string{}
	for _, z := range inst.Zones {
		if z.Kind != tado.ZoneHeating {
			continue
		}
		if name, ok := inst.Options.SharedSpace(z.Number); ok {
			out[z.Number] = "space:" + name
		} else {
			out[z.Number] = "zone:" + strconv.Itoa(z.Number)
		}
	}
	return out
}

// adviceDegreeHours sums, per space, (space temperature - outdoor) x step
// hours, the space temperature being the mean of its zones that have one, and
// the same sum for the mean of all heating zones over the very steps where the
// space had a temperature. Comparing the two over the same steps keeps a space
// that often lacks a temperature from looking cheap or costly.
func adviceDegreeHours(boiler []adviceBoilerRow, byAt map[int64][]adviceZoneRow, spaces map[int]string) (own, house map[string]float64) {
	own, house = map[string]float64{}, map[string]float64{}
	for i := 1; i < len(boiler); i++ {
		prev := boiler[i-1]
		gap := boiler[i].at.Sub(prev.at)
		if gap > historyGapMax || prev.outdoorC == nil {
			continue
		}
		hours := gap.Hours()
		var sum float64
		var n int
		spaceSum, spaceN := map[string]float64{}, map[string]int{}
		for _, z := range byAt[prev.at.Unix()] {
			space, heating := spaces[z.zone]
			if !heating || z.tempC == nil {
				continue
			}
			sum += *z.tempC
			n++
			spaceSum[space] += *z.tempC
			spaceN[space]++
		}
		if n == 0 {
			continue
		}
		mean := sum/float64(n) - *prev.outdoorC
		for space, t := range spaceSum {
			own[space] += (t/float64(spaceN[space]) - *prev.outdoorC) * hours
			house[space] += mean * hours
		}
	}
	return own, house
}

// adviceFit fits heating fuel = a x degree-hours + b over the valid days. A
// day is valid with a coverage and an outdoor coverage of adviceMinCoverage,
// an indoor mean, and heating fuel: a day that burned nothing for heating sits
// below the bend of the heating curve, where the line does not hold.
func adviceFit(days []dayTotal, heatShare map[string]float64) adviceModel {
	var xs, ys []float64
	for _, d := range days {
		share := heatShare[d.Day]
		if d.Coverage == nil || *d.Coverage < adviceMinCoverage || d.OutdoorC == nil || d.IndoorC == nil ||
			share <= 0 || d.Fuel <= 0 || float64(d.outdoorN)/float64(d.samples) < adviceMinCoverage {
			continue
		}
		hours := float64(d.samples) * historyEvery.Hours()
		xs = append(xs, (*d.IndoorC-*d.OutdoorC)*hours)
		ys = append(ys, d.Fuel*share)
	}
	m := adviceModel{ValidDays: len(xs)}
	if len(xs) < adviceMinDays {
		m.Reason = "needs " + strconv.Itoa(adviceMinDays) + " valid days, has " + strconv.Itoa(len(xs))
		return m
	}
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	mx /= float64(len(xs))
	my /= float64(len(xs))
	if mx <= 0 || (slices.Max(xs)-slices.Min(xs)) < adviceMinSpread*mx {
		m.Reason = "the valid days had too similar weather"
		return m
	}
	var sxx, sxy, syy float64
	for i := range xs {
		sxx += (xs[i] - mx) * (xs[i] - mx)
		sxy += (xs[i] - mx) * (ys[i] - my)
		syy += (ys[i] - my) * (ys[i] - my)
	}
	a := sxy / sxx
	b := my - a*mx
	var ssr float64
	for i := range xs {
		r := ys[i] - (a*xs[i] + b)
		ssr += r * r
	}
	r2 := 0.0
	if syy > 0 {
		r2 = 1 - ssr/syy
	}
	aErr := math.Sqrt(ssr / float64(len(xs)-2) / sxx)
	m.A, m.AErr, m.B, m.R2 = &a, &aErr, &b, &r2
	switch {
	case a <= 0:
		m.Reason = "the fuel does not rise with the indoor to outdoor difference"
	case r2 < adviceMinR2:
		m.Reason = "the fit is poor"
	default:
		m.Usable = true
	}
	return m
}

// adviceHouse is the house model with the fuel per degree-hour of each space,
// both from the longest window.
type adviceHouse struct {
	model  adviceModel
	spaceA map[string]float64
}

// adviceFitHouse fits the house model on days and the samples of the same
// span. A space burns a_s = a x (F_s / F) x (DH_house / DH_s): its share of
// the heating fuel, over its share of the degree-hours.
func adviceFitHouse(inst *tado.Install, loc *time.Location, days []dayTotal, boiler []adviceBoilerRow, zones []adviceZoneRow) adviceHouse {
	weights, _ := adviceWeights(inst)
	byAt := adviceByTime(zones)
	fuel := adviceSplitFuel(boiler, byAt, weights, loc, time.Time{})
	share := map[string]float64{}
	for day, all := range fuel.totalDay {
		share[day] = fuel.heatDay[day] / all
	}
	h := adviceHouse{model: adviceFit(days, share), spaceA: map[string]float64{}}
	if !h.model.Usable || fuel.split.Heating <= 0 {
		return h
	}
	spaces := adviceSpaces(inst)
	spaceFuel := map[string]float64{}
	for zone, f := range fuel.zone {
		spaceFuel[spaces[zone]] += f
	}
	own, house := adviceDegreeHours(boiler, byAt, spaces)
	for space, f := range spaceFuel {
		if own[space] > 0 && house[space] > 0 {
			h.spaceA[space] = *h.model.A * f / fuel.split.Heating * house[space] / own[space]
		}
	}
	return h
}

// adviceWindowSplit splits the fuel of the window. The parts follow the raw
// samples and are scaled to the fuel of the days, which carries the meter
// factor of each day: a zone's fuel carries the mean factor of the window. It
// also returns that scale, from raw counter fuel to the fuel of the days. The
// zones of a shared space carry the fuel per degree-hour of the space.
func adviceWindowSplit(inst *tado.Install, days []dayTotal, fuel adviceFuel, house adviceHouse) (adviceSplit, float64) {
	_, weighted := adviceWeights(inst)
	spaces := adviceSpaces(inst)
	var dayFuel float64
	for _, d := range days {
		dayFuel += d.Fuel
	}
	scale := 0.0
	if fuel.split.Fuel > 0 {
		scale = dayFuel / fuel.split.Fuel
	}
	raw := fuel.split
	split := adviceSplit{
		Fuel: round3(dayFuel), Heating: round3(raw.Heating * scale), HotWater: round3(raw.HotWater * scale),
		Unattributed: round3(raw.Unattributed * scale), Weighted: weighted, Zones: []adviceZoneShare{},
	}
	if raw.Fuel == 0 {
		// The day totals hold fuel that no sample of the window explains.
		split.Unattributed = split.Fuel
	}
	for _, z := range inst.Zones {
		if z.Kind != tado.ZoneHeating {
			continue
		}
		s := adviceZoneShare{Zone: z.Number, Fuel: round3(fuel.zone[z.Number] * scale)}
		if raw.Heating > 0 {
			s.Share = round3(fuel.zone[z.Number] / raw.Heating)
		}
		if a, ok := house.spaceA[spaces[z.Number]]; ok {
			s.FuelPerDegreeHour = &a
		}
		split.Zones = append(split.Zones, s)
	}
	return split, scale
}

// adviceCacheTTL is how long the house model of the longest window serves
// before a request reads the 90 days again. The model moves by one day a day.
const adviceCacheTTL = 15 * time.Minute

type adviceCache struct {
	mu    sync.Mutex
	inst  *tado.Install
	day   string
	at    time.Time
	house adviceHouse
}

// adviceRead reads the day totals and the samples of the heating zones of the
// local days from from to the end of to. The samples start one step early: the step that ends
// at from belongs to its first day.
func adviceRead(ctx context.Context, h *historyStore, loc *time.Location, weights map[int]float64, from, to time.Time) ([]dayTotal, []adviceBoilerRow, []adviceZoneRow, error) {
	days, err := h.days(ctx, from.Format(time.DateOnly), to.Format(time.DateOnly), loc)
	if err != nil {
		return nil, nil, nil, err
	}
	boiler, err := h.adviceBoiler(ctx, from.Add(-historyEvery), to)
	if err != nil {
		return nil, nil, nil, err
	}
	var zones []adviceZoneRow
	for zone := range weights {
		rows, err := h.adviceZones(ctx, zone, from.Add(-historyEvery), to)
		if err != nil {
			return nil, nil, nil, err
		}
		zones = append(zones, rows...)
	}
	return days, boiler, zones, nil
}

// getHistoryAdvice answers the split of the last ?days=7|28|90 local days, 28
// by default, today included, and the house model of the last 90 days.
func (d *daemon) getHistoryAdvice(w http.ResponseWriter, r *http.Request) {
	window := adviceDefaultWindow
	if raw := r.URL.Query().Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || !slices.Contains(adviceWindows, n) {
			writeErr(w, http.StatusBadRequest, "days must be 7, 28 or 90")
			return
		}
		window = n
	}
	inst := d.inst.Load()
	if inst == nil {
		writeResolveErr(w, errNoInstall)
		return
	}
	loc := programLocation(inst.Options)
	now := d.retryNow().In(loc)
	dayStart := func(back int) time.Time {
		return time.Date(now.Year(), now.Month(), now.Day()-back, 0, 0, 0, 0, loc)
	}
	from := dayStart(window - 1)
	hh, ok := d.historyHandle(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	longest := slices.Max(adviceWindows)
	weights, _ := adviceWeights(inst)
	today := now.Format(time.DateOnly)

	c := &d.adviceCache
	c.mu.Lock()
	if c.inst != inst || c.day != today || d.retryNow().Sub(c.at) > adviceCacheTTL {
		days, boiler, zones, err := adviceRead(ctx, hh.store, loc, weights, dayStart(longest-1), now)
		if err != nil {
			c.mu.Unlock()
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		c.house = adviceFitHouse(inst, loc, days, boiler, zones)
		c.inst, c.day, c.at = inst, today, d.retryNow()
	}
	house := c.house
	c.mu.Unlock()
	days, boiler, zones, err := adviceRead(ctx, hh.store, loc, weights, from, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	fuel := adviceSplitFuel(boiler, adviceByTime(zones), weights, loc, from)
	split, scale := adviceWindowSplit(inst, days, fuel, house)
	in := adviceInput{
		inst: inst, loc: loc, days: days, zones: zones, fuel: fuel, scale: scale,
		house: house, comfortMaxC: hh.cfg.comfortMax(), price: hh.cfg.FuelPrice, since: from,
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"days": window, "from": from.Format(time.DateOnly), "to": today,
		"unit": inst.Options.Fuel(), "fuelPrice": hh.cfg.FuelPrice, "currency": hh.cfg.Currency,
		"split": split, "model": house.model, "advice": adviceRules(in),
	})
}

// adviceBoiler reads the boiler samples from from to to, oldest first.
func (h *historyStore) adviceBoiler(ctx context.Context, from, to time.Time) ([]adviceBoilerRow, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT at, fuel, burner, outdoor_c FROM boiler_sample
		WHERE at >= ? AND at <= ? ORDER BY at`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []adviceBoilerRow
	for rows.Next() {
		var sec int64
		var r adviceBoilerRow
		var burner sql.NullBool
		var outdoor sql.NullFloat64
		if err := rows.Scan(&sec, &r.fuel, &burner, &outdoor); err != nil {
			return nil, err
		}
		r.at, r.outdoorC = time.Unix(sec, 0), nullFloat(outdoor)
		if burner.Valid {
			r.burner = &burner.Bool
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// adviceZones reads the samples of one zone from from to to, oldest first. One
// query per zone reads a range of the primary key.
func (h *historyStore) adviceZones(ctx context.Context, zone int, from, to time.Time) ([]adviceZoneRow, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT zone, at, temp_c, target_c, humidity_pct, demand_pct,
			is_on, window_open, away
		FROM zone_sample WHERE zone = ? AND at >= ? AND at <= ? ORDER BY at`, zone, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []adviceZoneRow
	for rows.Next() {
		var sec int64
		var r adviceZoneRow
		var temp, target, humidity sql.NullFloat64
		var demand sql.NullInt64
		if err := rows.Scan(&r.zone, &sec, &temp, &target, &humidity, &demand, &r.on, &r.window, &r.away); err != nil {
			return nil, err
		}
		r.at, r.tempC, r.targetC, r.humidityPct = time.Unix(sec, 0), nullFloat(temp), nullFloat(target), nullFloat(humidity)
		if demand.Valid {
			v := int(demand.Int64)
			r.demandPct = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
