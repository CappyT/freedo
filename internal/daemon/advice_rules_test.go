package daemon

import (
	"math"
	"testing"
	"time"
)

// ruleDay is the local day of the rule tests; the fixture runs in UTC.
var ruleDay = time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)

// ruleRow is one sample of zone at minute m of day d.
func ruleRow(zone, d, m int, temp, target float64, demand int) adviceZoneRow {
	return adviceZoneRow{
		zone: zone, at: ruleDay.Add(time.Duration(d)*24*time.Hour + time.Duration(m)*time.Minute),
		tempC: new(temp), targetC: new(target), demandPct: new(demand), on: true, humidityPct: new(55.0),
	}
}

// ruleInput builds an input over the given rows, giving each row with demand
// the fuel per step of its zone, and a usable house model with the given fuel
// per degree-hour per space.
func ruleInput(t *testing.T, rows []adviceZoneRow, perStep float64, spaceA map[string]float64, days int) adviceInput {
	t.Helper()
	fuel := adviceFuel{zone: map[int]float64{}, step: map[int64]map[int]float64{}}
	for _, r := range rows {
		if r.demandPct == nil || *r.demandPct == 0 {
			continue
		}
		at := r.at.Unix()
		if fuel.step[at] == nil {
			fuel.step[at] = map[int]float64{}
		}
		fuel.step[at][r.zone] += perStep
		fuel.zone[r.zone] += perStep
	}
	a, aErr, r2 := 0.01, 0.0005, 0.9
	model := adviceModel{Usable: spaceA != nil, A: &a, AErr: &aErr, R2: &r2}
	var dts []dayTotal
	for i := range days {
		dts = append(dts, dayTotal{Day: ruleDay.AddDate(0, 0, i).Format(time.DateOnly)})
	}
	return adviceInput{
		inst: defaultInstall, loc: time.UTC, days: dts, zones: rows, fuel: fuel, scale: 1,
		house: adviceHouse{model: model, spaceA: spaceA}, comfortMaxC: 21, since: ruleDay,
	}
}

func ruleKinds(items []adviceItem, kind string) []adviceItem {
	var out []adviceItem
	for _, it := range items {
		if it.Kind == kind {
			out = append(out, it)
		}
	}
	return out
}

func TestAdviceRuleWindowAndAway(t *testing.T) {
	var rows []adviceZoneRow
	for m := 0; m < 60; m += 5 {
		r := ruleRow(2, 0, 8*60+m, 20, 21, 80)
		r.window = true
		rows = append(rows, r)
		a := ruleRow(10, 0, 14*60+m, 19, 19, 50)
		a.away = true
		rows = append(rows, a)
	}
	items := adviceRules(ruleInput(t, rows, 0.01, nil, 1))
	w := ruleKinds(items, "window")
	if len(w) != 1 || w[0].Zones[0] != 2 || !nearly(w[0].Hours, 1) || !nearly(w[0].Fuel.Value, 0.12) ||
		!nearly(w[0].Fuel.Low, 0.09) || !nearly(w[0].Fuel.High, 0.15) || w[0].Confidence != "high" {
		t.Errorf("window = %+v (fuel %+v)", w, w[0].Fuel)
	}
	if a := ruleKinds(items, "away"); len(a) != 1 || a[0].Zones[0] != 10 || !nearly(a[0].Fuel.Value, 0.12) {
		t.Errorf("away = %+v", a)
	}

	// Ten minutes with the window open make no suggestion.
	short := []adviceZoneRow{}
	for m := 0; m < 10; m += 5 {
		r := ruleRow(2, 0, 8*60+m, 19, 20, 80)
		r.window = true
		short = append(short, r)
	}
	if w := ruleKinds(adviceRules(ruleInput(t, short, 0.01, nil, 1)), "window"); len(w) != 0 {
		t.Errorf("window for 10 minutes = %+v", w)
	}
}

// ventRun is n samples of zone 2 from minute m of day 0 with demand 80, the
// temperature rising by rise and the humidity falling by drop over the run.
func ventRun(m, n int, rise, drop float64) []adviceZoneRow {
	var rows []adviceZoneRow
	for i := range n {
		r := ruleRow(2, 0, m+5*i, 20.5+rise*float64(i)/float64(n-1), 21, 80)
		r.humidityPct = new(60 - drop*float64(i)/float64(n-1))
		rows = append(rows, r)
	}
	return rows
}

func TestAdviceRuleVent(t *testing.T) {
	cases := []struct {
		name  string
		rows  []adviceZoneRow
		found bool
	}{
		{"a night with a tilted sash", ventRun(0, 60, 0.9, 5.8), true},
		{"the room warms", ventRun(0, 60, 1.5, 5.8), false},
		{"the air does not dry", ventRun(0, 60, 0.8, 2.7), false},
		{"too short", ventRun(0, 20, 0.2, 5), false},
		{"a slow drift over a long day", ventRun(0, 96, 0, 3.5), false},
	}
	for _, c := range cases {
		items := ruleKinds(adviceRules(ruleInput(t, c.rows, 0.01, nil, 1)), "vent")
		if (len(items) == 1) != c.found {
			t.Errorf("%s: vent = %+v", c.name, items)
			continue
		}
		if !c.found {
			continue
		}
		p := items[0].Evidence["periods"].([]map[string]any)
		if len(p) != 1 || p[0]["startMs"] != ruleDay.UnixMilli() || p[0]["endMs"] != ruleDay.Add(5*time.Hour).UnixMilli() ||
			!nearly(items[0].Fuel.Value, 0.6) || items[0].Confidence != "medium" {
			t.Errorf("%s: vent = %+v, periods %v", c.name, items[0], p)
		}
	}

	// A sensor that reports the window keeps the run out of vent: the window
	// rule takes it.
	flagged := ventRun(0, 60, 0.9, 5.8)
	for i := range flagged {
		flagged[i].window = true
	}
	if items := ruleKinds(adviceRules(ruleInput(t, flagged, 0.01, nil, 1)), "vent"); len(items) != 0 {
		t.Errorf("vent with the window flag = %+v", items)
	}
}

// comfortDays is zone 3 at 21.5 C from 10:00 to 14:00 and at setback the rest
// of each day, for days days, held at its target.
func comfortDays(zone, days int, setback float64) []adviceZoneRow {
	var rows []adviceZoneRow
	for d := range days {
		for m := 0; m < 24*60; m += 5 {
			target := setback
			if m >= 10*60 && m < 14*60 {
				target = 21.5
			}
			rows = append(rows, ruleRow(zone, d, m, target, target, 30))
		}
	}
	return rows
}

func TestAdviceRuleSetpoint(t *testing.T) {
	rows := comfortDays(3, 7, 18)
	items := ruleKinds(adviceRules(ruleInput(t, rows, 0.001, map[string]float64{"zone:3": 0.002}, 7)), "setpoint")
	if len(items) != 1 {
		t.Fatalf("setpoint = %+v", items)
	}
	s := items[0]
	ev := s.Evidence
	if ev["fromMin"] != 600 || ev["toMin"] != 840 || ev["targetC"] != 21.5 || ev["thresholdC"] != 21.0 || !nearly(s.Hours, 28) {
		t.Errorf("setpoint evidence = %v, hours %v", ev, s.Hours)
	}
	// One degree less over 28 held hours at 0.002 fuel per degree-hour.
	if s.Fuel == nil || !nearly(s.Fuel.Value, 0.056) || s.Fuel.Low >= s.Fuel.Value || s.Fuel.High <= s.Fuel.Value || s.Confidence != "medium" {
		t.Errorf("setpoint fuel = %+v, confidence %s", s.Fuel, s.Confidence)
	}

	// Without a usable model the rule still names the block, without a saving.
	plain := ruleKinds(adviceRules(ruleInput(t, rows, 0.001, nil, 7)), "setpoint")
	if len(plain) != 1 || plain[0].Fuel != nil || plain[0].Cost != nil || plain[0].Confidence != "low" {
		t.Errorf("setpoint without a model = %+v", plain)
	}

	// A block on one day of the seven is a boost, not a habit.
	var boost []adviceZoneRow
	for d := range 7 {
		for m := 0; m < 24*60; m += 5 {
			target := 19.0
			if d == 2 && m >= 10*60 && m < 14*60 {
				target = 22.5
			}
			boost = append(boost, ruleRow(3, d, m, target, target, 30))
		}
	}
	if items := ruleKinds(adviceRules(ruleInput(t, boost, 0.001, nil, 7)), "setpoint"); len(items) != 0 {
		t.Errorf("setpoint for a single boost = %+v", items)
	}

	// A threshold above the block: nothing to suggest.
	in := ruleInput(t, rows, 0.001, nil, 7)
	in.comfortMaxC = 22
	if items := ruleKinds(adviceRules(in), "setpoint"); len(items) != 0 {
		t.Errorf("setpoint above the block = %+v", items)
	}

	// With a price the saving has a cost.
	in = ruleInput(t, rows, 0.001, map[string]float64{"zone:3": 0.002}, 7)
	in.price = new(1.2)
	if items := ruleKinds(adviceRules(in), "setpoint"); items[0].Cost == nil || !nearly(items[0].Cost.Value, round3(0.056*1.2)) {
		t.Errorf("setpoint cost = %+v", items[0].Cost)
	}
}

func TestAdviceRuleSetback(t *testing.T) {
	// The nights stay at 20.5, half a degree under the 21.5 comfort.
	rows := comfortDays(3, 7, 20.5)
	items := ruleKinds(adviceRules(ruleInput(t, rows, 0.001, map[string]float64{"zone:3": 0.002}, 7)), "setback")
	if len(items) != 1 {
		t.Fatalf("setback = %+v", items)
	}
	ev := items[0].Evidence
	// 7 nights: 00:00-06:00 of the first day, then six of 23:00-06:00, 48 h.
	if ev["comfortC"] != 21.5 || ev["setbackC"] != 20.5 || ev["proposedC"] != 19.5 || !nearly(ev["hoursPerDay"].(float64), round3(48.0/7)) {
		t.Errorf("setback evidence = %v", ev)
	}
	// 1 degree of extra depth, half reached on average, over the held night hours.
	held := items[0].Hours
	if f := items[0].Fuel; f == nil || !nearly(f.Value, round3(0.002*held*1*0.5)) || f.Low >= f.Value || f.High <= f.Value {
		t.Errorf("setback fuel = %+v over %v h", f, held)
	}

	// One boost to 25 C does not move the comfort target.
	boosted := comfortDays(3, 7, 20.5)
	for i := range boosted {
		if boosted[i].at.Equal(ruleDay.Add(3*24*time.Hour + 18*time.Hour)) {
			boosted[i].targetC, boosted[i].tempC = new(25.0), new(25.0)
		}
	}
	if items := ruleKinds(adviceRules(ruleInput(t, boosted, 0.001, map[string]float64{"zone:3": 0.002}, 7)), "setback"); len(items) != 1 || items[0].Evidence["comfortC"] != 21.5 {
		t.Errorf("setback after a boost = %+v", items)
	}

	// A deep setback already: nothing to suggest.
	if items := ruleKinds(adviceRules(ruleInput(t, comfortDays(3, 7, 18), 0.001, nil, 7)), "setback"); len(items) != 0 {
		t.Errorf("setback with a deep night = %+v", items)
	}
}

func TestAdviceRuleSharedSpace(t *testing.T) {
	in := ruleInput(t, append(comfortDays(5, 7, 18), comfortDays(8, 7, 18)...), 0.001, map[string]float64{"space:open": 0.003}, 7)
	inst := *defaultInstall
	inst.Options.SharedSpaces = map[int]string{5: "open", 8: "open"}
	in.inst = &inst
	items := ruleKinds(adviceRules(in), "setpoint")
	if len(items) != 1 || len(items[0].Zones) != 2 || items[0].Zones[0] != 5 || items[0].Zones[1] != 8 || items[0].ID != "setpoint:space:open" {
		t.Fatalf("setpoint of a shared space = %+v", items)
	}
	// The space counts each sample time once, not once per zone.
	if !nearly(items[0].Hours, 28) || !nearly(items[0].Fuel.Value, round3(0.003*28)) {
		t.Errorf("shared space hours %v fuel %+v", items[0].Hours, items[0].Fuel)
	}
}

func TestAdviceRuleSkipsUnheatedRooms(t *testing.T) {
	// The room sits above its target and never asks: neighbours heat it.
	var rows []adviceZoneRow
	for d := range 7 {
		for m := 0; m < 24*60; m += 5 {
			rows = append(rows, ruleRow(1, d, m, 20.6, 21.5, 0))
		}
	}
	if items := adviceRules(ruleInput(t, rows, 0.001, map[string]float64{"zone:1": 0.002}, 7)); len(items) != 0 {
		t.Errorf("suggestions for a room that never heated = %+v", items)
	}
}

func TestAdviceRuleUnreachable(t *testing.T) {
	var rows []adviceZoneRow
	for d := range 2 {
		for m := 0; m < 3*60; m += 5 {
			rows = append(rows, ruleRow(10, d, 6*60+m, 19, 21, 100))
		}
	}
	items := ruleKinds(adviceRules(ruleInput(t, rows, 0.001, nil, 2)), "unreachable")
	if len(items) != 1 || !nearly(items[0].Hours, 6) || items[0].Evidence["gapC"] != 2.0 || items[0].Fuel != nil {
		t.Errorf("unreachable = %+v", items)
	}
}

func TestAdviceRuleShortCycle(t *testing.T) {
	in := ruleInput(t, nil, 0, nil, 0)
	in.days = []dayTotal{
		{Day: "2026-01-05", BurnerMin: 300, BurnerStarts: new(int64(40))},
		{Day: "2026-01-06", BurnerMin: 300, BurnerStarts: new(int64(20))},
		{Day: "2026-01-07", BurnerMin: 10, BurnerStarts: new(int64(30))},
		{Day: "2026-01-08", BurnerMin: 300},
	}
	items := adviceRules(in)
	if len(items) != 1 || items[0].Kind != "short_cycle" || items[0].Evidence["days"] != 1 ||
		math.Abs(items[0].Evidence["startsPerBurnerHour"].(float64)-8) > 1e-9 {
		t.Errorf("short cycle = %+v", items)
	}
}

func TestAdviceRulesOrder(t *testing.T) {
	var rows []adviceZoneRow
	for m := 0; m < 60; m += 5 {
		w := ruleRow(2, 0, 8*60+m, 19, 20, 80)
		w.window = true
		rows = append(rows, w)
	}
	rows = append(rows, comfortDays(3, 7, 18)...)
	in := ruleInput(t, rows, 0.01, map[string]float64{"zone:3": 0.002}, 7)
	in.days = append(in.days, dayTotal{Day: "2026-01-12", BurnerMin: 60, BurnerStarts: new(int64(20))})
	items := adviceRules(in)
	// Window 0.12 spent, setpoint 0.056 to save, short cycle without a figure.
	if len(items) != 3 || items[0].Kind != "window" || items[1].Kind != "setpoint" || items[2].Kind != "short_cycle" {
		t.Errorf("order = %+v", items)
	}
}

func TestAdviceTypicalBlock(t *testing.T) {
	slots := map[int]int{}
	for s := 120; s < 168; s++ {
		slots[s] = 7
	}
	slots[200] = 7
	slots[201] = 1
	if from, to := adviceTypicalBlock(slots, 7); from != 600 || to != 840 {
		t.Errorf("block = %d-%d, want 600-840", from, to)
	}
	if from, to := adviceTypicalBlock(map[int]int{30: 1, 31: 2}, 7); from != 155 || to != 160 {
		t.Errorf("block of a rare slot = %d-%d, want the most frequent", from, to)
	}
	// A comfort block from 21:00 to 07:00 crosses midnight.
	night := map[int]int{}
	for s := 252; s < 288; s++ {
		night[s] = 7
	}
	for s := range 84 {
		night[s] = 7
	}
	if from, to := adviceTypicalBlock(night, 7); from != 21*60 || to != 7*60 {
		t.Errorf("night block = %d-%d, want 1260-420", from, to)
	}
	all := map[int]int{}
	for s := range 288 {
		all[s] = 7
	}
	if from, to := adviceTypicalBlock(all, 7); from != 0 || to != 1440 {
		t.Errorf("all-day block = %d-%d", from, to)
	}
}
