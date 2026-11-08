package daemon

// advice_rules: the suggestions of the consumption advisor. Each rule reads the
// samples of the window and the house model, and yields suggestions with their
// evidence. The advisor only suggests: nothing here writes a program or a
// setting.

import (
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"time"

	"freedo/tado"
)

const (
	// adviceAttribution is the relative error of the fuel split per room.
	adviceAttribution = 0.25
	// adviceHeldBandC is how close to its target a room must be to count as
	// held at that target.
	adviceHeldBandC = 0.5
	// adviceSetbackMinC is the setback below comfort that the setback rule
	// asks for, and adviceSetbackMinHours the night hours it must cover.
	adviceSetbackMinC     = 2.0
	adviceSetbackMinHours = 6.0
	// adviceSetbackDecay is the share of a setback the room reaches on
	// average over a night, with its range: a room cools toward the new
	// target, it does not drop at once.
	adviceSetbackDecay, adviceSetbackDecayLow, adviceSetbackDecayHigh = 0.5, 0.3, 0.7
	// adviceVent* detect an open window that no sensor reports: a long run of
	// strong demand while the room barely warms and the air dries.
	adviceVentMinSteps   = 24
	adviceVentMinDemand  = 60
	adviceVentMaxRiseC   = 1.0
	adviceVentMinHumDrop = 3.0
	// adviceVentMinHumRate is how fast the humidity must fall, in points per
	// hour: a slow drift over a long cold day is no open window.
	adviceVentMinHumRate  = 0.75
	adviceVentMaxPeriods  = 5
	adviceUnreachGapC     = 0.5
	adviceUnreachDemand   = 90
	adviceShortCycleRate  = 6.0
	adviceShortCycleMinBn = 30
)

// adviceNightFrom and adviceNightTo bound the night of the setback rule, in
// minutes of the local day.
const adviceNightFrom, adviceNightTo = 23 * 60, 6 * 60

// adviceRange is an estimate with its low and high bounds.
type adviceRange struct {
	Value float64 `json:"value"`
	Low   float64 `json:"low"`
	High  float64 `json:"high"`
}

// adviceItem is one suggestion. Fuel is the fuel spent (window, vent, away) or
// the fuel the change would have saved over the window (setpoint, setback),
// in fuel_unit; Cost is Fuel times the fuel price.
type adviceItem struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"`
	Zones      []int          `json:"zones"`
	Hours      float64        `json:"hours"`
	Fuel       *adviceRange   `json:"fuel"`
	Cost       *adviceRange   `json:"cost"`
	Confidence string         `json:"confidence"`
	Evidence   map[string]any `json:"evidence"`
}

// adviceInput is what the rules read.
type adviceInput struct {
	inst        *tado.Install
	loc         *time.Location
	days        []dayTotal
	zones       []adviceZoneRow
	fuel        adviceFuel
	scale       float64
	house       adviceHouse
	comfortMaxC float64
	price       *float64
	since       time.Time
}

// adviceSpace is one space with its zones and their samples in the window,
// each zone's samples oldest first.
type adviceSpace struct {
	key   string
	zones []int
	rows  map[int][]adviceZoneRow
}

func (in adviceInput) spaces() []adviceSpace {
	byKey := map[string]*adviceSpace{}
	for zone, key := range adviceSpaces(in.inst) {
		sp := byKey[key]
		if sp == nil {
			sp = &adviceSpace{key: key, rows: map[int][]adviceZoneRow{}}
			byKey[key] = sp
		}
		sp.zones = append(sp.zones, zone)
	}
	// The step that ends at since belongs to the window, and starts one sample
	// earlier, as in adviceSplitFuel.
	cut := in.since.Add(-historyEvery)
	for _, r := range in.zones {
		if r.at.Before(cut) {
			continue
		}
		for _, sp := range byKey {
			if slices.Contains(sp.zones, r.zone) {
				sp.rows[r.zone] = append(sp.rows[r.zone], r)
			}
		}
	}
	out := []adviceSpace{}
	for _, sp := range byKey {
		slices.Sort(sp.zones)
		out = append(out, *sp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].zones[0] < out[j].zones[0] })
	return out
}

// stepFuel is the fuel of zone in the step that starts at at, meter-corrected.
func (in adviceInput) stepFuel(zone int, at time.Time) float64 {
	return in.fuel.step[at.Unix()][zone] * in.scale
}

// spent turns fuel attributed by the split into a range.
func (in adviceInput) spent(fuel float64) *adviceRange {
	return &adviceRange{Value: round3(fuel), Low: round3(fuel * (1 - adviceAttribution)), High: round3(fuel * (1 + adviceAttribution))}
}

// saved is the fuel that degree-hours less of a space would have saved, with
// the error of the slope (two standard errors) and of the split; nil while the
// model is not usable. lowF and highF bound the degree-hours themselves. The
// slope comes from the meter-corrected day totals, so it needs no scale.
func (in adviceInput) saved(space string, degreeHours, lowF, highF float64) *adviceRange {
	a, ok := in.house.spaceA[space]
	if !ok || !in.house.model.Usable || degreeHours <= 0 {
		return nil
	}
	rel := 0.0
	if m := in.house.model; m.A != nil && m.AErr != nil && *m.A > 0 {
		rel = 2 * *m.AErr / *m.A
	}
	v := a * degreeHours
	return &adviceRange{
		Value: round3(v),
		Low:   round3(max(0, v*lowF*(1-rel)*(1-adviceAttribution))),
		High:  round3(v * highF * (1 + rel) * (1 + adviceAttribution)),
	}
}

func (in adviceInput) cost(f *adviceRange) *adviceRange {
	if f == nil || in.price == nil {
		return nil
	}
	p := *in.price
	return &adviceRange{Value: round3(f.Value * p), Low: round3(f.Low * p), High: round3(f.High * p)}
}

func (in adviceInput) item(kind string, sp adviceSpace, zones []int, hours float64, fuel *adviceRange, confidence string, evidence map[string]any) adviceItem {
	if evidence == nil {
		evidence = map[string]any{}
	}
	id := kind + ":" + sp.key
	if len(zones) == 1 && len(sp.zones) > 1 {
		id = kind + ":zone:" + strconv.Itoa(zones[0])
	}
	return adviceItem{
		ID: id, Kind: kind, Zones: zones, Hours: round3(hours), Fuel: fuel, Cost: in.cost(fuel),
		Confidence: confidence, Evidence: evidence,
	}
}

// heated is the fuel the split gave a space over the window. A space that got
// none gets no suggestion: rooms heated by their neighbours never ask.
func (in adviceInput) heated(sp adviceSpace) bool {
	for _, z := range sp.zones {
		if in.fuel.zone[z] > 0 {
			return true
		}
	}
	return false
}

// adviceRules runs every rule over the window, highest saving first; a
// suggestion without a fuel figure comes after those with one.
func adviceRules(in adviceInput) []adviceItem {
	out := []adviceItem{}
	for _, sp := range in.spaces() {
		if !in.heated(sp) {
			continue
		}
		out = append(out, adviceFlagged(in, sp, "window", func(r adviceZoneRow) bool { return r.window })...)
		out = append(out, adviceFlagged(in, sp, "away", func(r adviceZoneRow) bool { return r.away })...)
		out = append(out, adviceVent(in, sp)...)
		out = append(out, adviceSetpoint(in, sp)...)
		out = append(out, adviceSetback(in, sp)...)
		out = append(out, adviceUnreachable(in, sp)...)
	}
	out = append(out, adviceShortCycle(in)...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Fuel, out[j].Fuel
		switch {
		case a == nil:
			return false
		case b == nil:
			return true
		}
		return a.Value > b.Value
	})
	return out
}

// adviceFlagged: fuel a zone burned while a sample flag was set, such as an
// open window that the window detection did not stop, or heat in away.
func adviceFlagged(in adviceInput, sp adviceSpace, kind string, flag func(adviceZoneRow) bool) []adviceItem {
	var out []adviceItem
	for _, z := range sp.zones {
		var fuel float64
		var steps int
		for _, r := range sp.rows[z] {
			if !flag(r) {
				continue
			}
			if f := in.stepFuel(z, r.at); f > 0 {
				fuel += f
				steps++
			}
		}
		if steps == 0 {
			continue
		}
		out = append(out, in.item(kind, sp, []int{z}, float64(steps)*historyEvery.Hours(), in.spent(fuel), "high", nil))
	}
	return out
}

// adviceVent finds runs of consecutive samples with strong demand and no
// window flag, in which the room warms less than adviceVentMaxRiseC while the
// humidity falls adviceVentMinHumDrop points or more: fresh air keeps coming
// in. It is an inference, so its confidence is medium.
func adviceVent(in adviceInput, sp adviceSpace) []adviceItem {
	var out []adviceItem
	for _, z := range sp.zones {
		rows := sp.rows[z]
		var periods []map[string]any
		var fuel float64
		var steps int
		flush := func(run []adviceZoneRow) {
			if len(run) < adviceVentMinSteps {
				return
			}
			first, last := run[0], run[len(run)-1]
			if first.tempC == nil || last.tempC == nil || first.humidityPct == nil || last.humidityPct == nil {
				return
			}
			rise := *last.tempC - *first.tempC
			drop := *first.humidityPct - *last.humidityPct
			hours := float64(len(run)) * historyEvery.Hours()
			if rise >= adviceVentMaxRiseC || drop < adviceVentMinHumDrop || drop/hours < adviceVentMinHumRate {
				return
			}
			var spent float64
			for _, r := range run {
				spent += in.stepFuel(z, r.at)
			}
			if spent <= 0 {
				return
			}
			fuel += spent
			steps += len(run)
			periods = append(periods, map[string]any{
				"startMs": first.at.UnixMilli(), "endMs": last.at.Add(historyEvery).UnixMilli(),
				"riseC": round3(rise), "humidityDropPct": round3(drop),
			})
		}
		var run []adviceZoneRow
		for i, r := range rows {
			strong := !r.window && r.demandPct != nil && *r.demandPct >= adviceVentMinDemand
			contiguous := i > 0 && r.at.Sub(rows[i-1].at) <= historyEvery+time.Minute
			if strong && (len(run) == 0 || contiguous) {
				run = append(run, r)
				continue
			}
			flush(run)
			run = nil
			if strong {
				run = []adviceZoneRow{r}
			}
		}
		flush(run)
		if len(periods) == 0 {
			continue
		}
		if len(periods) > adviceVentMaxPeriods {
			periods = periods[len(periods)-adviceVentMaxPeriods:]
		}
		out = append(out, in.item("vent", sp, []int{z}, float64(steps)*historyEvery.Hours(), in.spent(fuel), "medium",
			map[string]any{"periods": periods}))
	}
	return out
}

// adviceSteps visits the samples of a space by time: for each sample time, the
// rows of its zones at that time.
func adviceSteps(sp adviceSpace, visit func(at time.Time, rows []adviceZoneRow)) {
	byAt := map[int64][]adviceZoneRow{}
	var times []int64
	for _, z := range sp.zones {
		for _, r := range sp.rows[z] {
			if byAt[r.at.Unix()] == nil {
				times = append(times, r.at.Unix())
			}
			byAt[r.at.Unix()] = append(byAt[r.at.Unix()], r)
		}
	}
	slices.Sort(times)
	for _, t := range times {
		visit(time.Unix(t, 0), byAt[t])
	}
}

// adviceTarget is the highest target of the rows that are on, and whether
// every such row held it: its temperature within adviceHeldBandC of target.
func adviceTarget(rows []adviceZoneRow) (target float64, ok, held bool) {
	held = true
	for _, r := range rows {
		if !r.on || r.targetC == nil {
			continue
		}
		if !ok || *r.targetC > target {
			target = *r.targetC
		}
		ok = true
		if r.tempC == nil || math.Abs(*r.tempC-*r.targetC) > adviceHeldBandC {
			held = false
		}
	}
	return target, ok, held && ok
}

func minuteOfDay(t time.Time, loc *time.Location) int {
	l := t.In(loc)
	return l.Hour()*60 + l.Minute()
}

// adviceSetpoint: a space held at comfortMaxC or above. A degree less over
// the hours it held that target saves a_s degree-hours each hour.
func adviceSetpoint(in adviceInput, sp adviceSpace) []adviceItem {
	var hours, heldHours, maxTarget float64
	slots := map[int]int{}
	days := map[string]bool{}
	adviceSteps(sp, func(at time.Time, rows []adviceZoneRow) {
		target, ok, held := adviceTarget(rows)
		if !ok || target < in.comfortMaxC {
			return
		}
		hours += historyEvery.Hours()
		if held {
			heldHours += historyEvery.Hours()
		}
		maxTarget = max(maxTarget, target)
		slots[minuteOfDay(at, in.loc)/int(historyEvery/time.Minute)]++
		days[at.In(in.loc).Format(time.DateOnly)] = true
	})
	if hours < 1 {
		return nil
	}
	from, to := adviceTypicalBlock(slots, len(days))
	conf := "medium"
	fuel := in.saved(sp.key, heldHours, 1, 1)
	if fuel == nil {
		conf = "low"
	}
	return []adviceItem{in.item("setpoint", sp, sp.zones, hours, fuel, conf, map[string]any{
		"targetC": round3(maxTarget), "thresholdC": in.comfortMaxC, "fromMin": from, "toMin": to,
	})}
}

// adviceTypicalBlock is the longest run of 5-minute slots of the day that
// appear on at least half of the days, as minutes of the day. The run may
// cross midnight: then toMin is less than fromMin. toMin is exclusive, from 5
// to 1440. Without such a run it is the single most frequent slot.
func adviceTypicalBlock(slots map[int]int, days int) (from, to int) {
	step := int(historyEvery / time.Minute)
	n := 24 * 60 / step
	on := func(i int) bool { return days > 0 && slots[(i%n+n)%n]*2 >= days }
	all := true
	for i := range n {
		all = all && on(i)
	}
	if all {
		return 0, 24 * 60
	}
	bestStart, bestLen := -1, 0
	for i := range n {
		if !on(i) || on(i-1) {
			continue
		}
		l := 0
		for on(i + l) {
			l++
		}
		if l > bestLen {
			bestStart, bestLen = i, l
		}
	}
	if bestStart < 0 {
		top := 0
		for s, c := range slots {
			if c > slots[top] || (c == slots[top] && s < top) {
				top = s
			}
		}
		return top * step, (top + 1) * step
	}
	return bestStart * step, ((bestStart+bestLen-1)%n + 1) * step
}

// adviceSetback: nights in which a space stays within adviceSetbackMinC of
// its comfort target for adviceSetbackMinHours or more. A setback to comfort
// minus adviceSetbackMinC lowers the room by part of the extra depth, as it
// cools slowly toward the new target.
func adviceSetback(in adviceInput, sp adviceSpace) []adviceItem {
	// The comfort target is the median of the daily highest targets, so one
	// boost does not move it.
	dayMax := map[string]float64{}
	adviceSteps(sp, func(at time.Time, rows []adviceZoneRow) {
		if t, ok, _ := adviceTarget(rows); ok {
			day := at.In(in.loc).Format(time.DateOnly)
			dayMax[day] = max(dayMax[day], t)
		}
	})
	if len(dayMax) == 0 {
		return nil
	}
	maxima := slices.Sorted(maps.Values(dayMax))
	comfort := maxima[len(maxima)/2]
	night := func(m int) bool { return m >= adviceNightFrom || m < adviceNightTo }
	type nightSum struct {
		shallow, held int
		targets       []float64
	}
	nights := map[string]*nightSum{}
	adviceSteps(sp, func(at time.Time, rows []adviceZoneRow) {
		m := minuteOfDay(at, in.loc)
		if !night(m) {
			return
		}
		target, ok, held := adviceTarget(rows)
		if !ok || comfort-target >= adviceSetbackMinC {
			return
		}
		// A night belongs to the day it ends on.
		key := at.In(in.loc).Add(time.Duration(24*60-adviceNightFrom) * time.Minute).Format(time.DateOnly)
		n := nights[key]
		if n == nil {
			n = &nightSum{}
			nights[key] = n
		}
		n.shallow++
		n.targets = append(n.targets, target)
		if held {
			n.held++
		}
	})
	var count int
	var hours, held float64
	var targets []float64
	for _, n := range nights {
		// Whole steps: a sum of 5-minute hours in floating point falls short of 6.
		if time.Duration(n.shallow)*historyEvery < adviceSetbackMinHours*time.Hour {
			continue
		}
		count++
		hours += float64(n.shallow) * historyEvery.Hours()
		held += float64(n.held) * historyEvery.Hours()
		targets = append(targets, n.targets...)
	}
	if count == 0 {
		return nil
	}
	slices.Sort(targets)
	setback := targets[len(targets)/2]
	proposed := comfort - adviceSetbackMinC
	extra := setback - proposed
	conf := "medium"
	fuel := in.saved(sp.key, held*extra*adviceSetbackDecay, adviceSetbackDecayLow/adviceSetbackDecay, adviceSetbackDecayHigh/adviceSetbackDecay)
	if fuel == nil {
		conf = "low"
	}
	return []adviceItem{in.item("setback", sp, sp.zones, hours, fuel, conf, map[string]any{
		"comfortC": round3(comfort), "setbackC": round3(setback), "proposedC": round3(proposed),
		"hoursPerDay": round3(hours / float64(count)),
	})}
}

// adviceUnreachable: a zone that stays more than adviceUnreachGapC under its
// target with its valve nearly open, for an hour a day or more over the
// window. Its radiator, its valve or the flow temperature falls short.
func adviceUnreachable(in adviceInput, sp adviceSpace) []adviceItem {
	var out []adviceItem
	days := max(1, len(in.days))
	for _, z := range sp.zones {
		var steps int
		var gap float64
		for _, r := range sp.rows[z] {
			if !r.on || r.window || r.targetC == nil || r.tempC == nil || r.demandPct == nil || *r.demandPct < adviceUnreachDemand {
				continue
			}
			if g := *r.targetC - *r.tempC; g > adviceUnreachGapC {
				steps++
				gap += g
			}
		}
		hours := float64(steps) * historyEvery.Hours()
		if hours < float64(days) {
			continue
		}
		out = append(out, in.item("unreachable", sp, []int{z}, hours, nil, "medium", map[string]any{"gapC": round3(gap / float64(steps))}))
	}
	return out
}

// adviceShortCycle: days on which the burner started more than
// adviceShortCycleRate times per hour of burning. Short runs waste fuel on
// each start and wear the boiler.
func adviceShortCycle(in adviceInput) []adviceItem {
	var count int
	var worst, hours float64
	for _, d := range in.days {
		if d.BurnerStarts == nil || d.BurnerMin < adviceShortCycleMinBn {
			continue
		}
		rate := float64(*d.BurnerStarts) / (float64(d.BurnerMin) / 60)
		if rate <= adviceShortCycleRate {
			continue
		}
		count++
		hours += float64(d.BurnerMin) / 60
		worst = max(worst, rate)
	}
	if count == 0 {
		return nil
	}
	return []adviceItem{{
		ID: "short_cycle", Kind: "short_cycle", Zones: []int{}, Hours: round3(hours), Confidence: "medium",
		Evidence: map[string]any{"days": count, "startsPerBurnerHour": round3(worst)},
	}}
}
