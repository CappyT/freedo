package daemon

// plant_episode_test: the plant episode recorder. Reports, programs and clocks
// are synthetic; zone 3 has one valve and the thermostat publishes its 4160.

import (
	"context"
	"math"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"freedo/internal/install"
	"freedo/tado"
)

// cpeHeat sends the 4160 the thermostat publishes for zone 3 on z/cpe.
func cpeHeat(t *testing.T, d *daemon, in *tado.Install, pct int) {
	t.Helper()
	feed(t, d, in, "RU1000000001", true, tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x6800 + uint16(earlyMID.Add(1)),
		Path: []string{"z", "cpe"}, Query: []string{"lid=3"},
		Payload: tado.TLV{Tag: "4160", Val: []byte{byte(pct)}}.Encode(),
	})
	tickHome(t, d)
}

func plantEpisodesOf(t *testing.T, store *stateStore, zone int) []plantEpisode {
	t.Helper()
	eps, err := store.PlantEpisodes(context.Background(), zone)
	if err != nil {
		t.Fatal(err)
	}
	return eps
}

func ms(hhmm string) int64 {
	when, err := time.Parse("15:04", hhmm)
	if err != nil {
		panic(err)
	}
	return windowStart.Truncate(24 * time.Hour).Add(time.Duration(when.Hour())*time.Hour +
		time.Duration(when.Minute())*time.Minute).UnixMilli()
}

// plantHeatUp runs a zero-demand stretch from 06:30, the 17 -> 21 C block at
// 07:00, 40a0 from 07:05 and 4160 from 07:10, the first 0.1 C rise at 07:25,
// the crossing at 08:45, a 0.4 C peak at 09:00 and the band from 09:10.
func plantHeatUp(t *testing.T, d *daemon, url string, in *tado.Install, clock *testClock) {
	t.Helper()
	at(t, d, in, clock, "06:20", math.NaN(), 0)
	pushOutdoor(t, url, 4.5)
	at(t, d, in, clock, "06:30", 17.0, 0)
	at(t, d, in, clock, "06:50", 16.8, 0)
	at(t, d, in, clock, "07:00", math.NaN(), 0)
	at(t, d, in, clock, "07:05", 16.8, 80)
	at(t, d, in, clock, "07:10", math.NaN(), 0)
	cpeHeat(t, d, in, 100)
	at(t, d, in, clock, "07:20", 16.8, 70)
	at(t, d, in, clock, "07:25", 16.9, 70)
	at(t, d, in, clock, "08:00", 19.5, 60)
	at(t, d, in, clock, "08:30", 20.6, 40)
	at(t, d, in, clock, "08:45", 21.3, 10)
	at(t, d, in, clock, "09:00", 21.4, 0)
	at(t, d, in, clock, "09:10", 21.1, 0)
	at(t, d, in, clock, "09:39", math.NaN(), 0)
}

func TestPlantRecordsCoolDownAndHeatUp(t *testing.T) {
	store := newTestStore(t, t.TempDir()+"/state.db")
	d, url, clock, in := newEarlyDaemon(t, false, store, nil)
	plantHeatUp(t, d, url, in, clock)
	if eps := plantEpisodesOf(t, store, 3); len(eps) != 1 || eps[0].Kind != plantCool {
		t.Fatalf("episodes at 09:39 = %+v, want the cool-down alone: the heat-up has not settled", eps)
	}
	at(t, d, in, clock, "09:40", math.NaN(), 0)
	eps := plantEpisodesOf(t, store, 3)
	if len(eps) != 2 {
		t.Fatalf("episodes = %+v, want a cool-down and a heat-up", eps)
	}

	cool := eps[0]
	if cool.Kind != plantCool || cool.StartedMs != ms("06:30") || cool.EndedMs != ms("07:00") ||
		cool.EndReason != "heat-up" || len(cool.Samples) != 2 {
		t.Errorf("cool-down = %+v, want 06:30 -> 07:00 ended by the heat-up with 2 samples", cool)
	}
	for i, s := range cool.Samples {
		if s.OutdoorC == nil || *s.OutdoorC != 4.5 || s.DemandPct == nil || *s.DemandPct != 0 {
			t.Errorf("cool-down sample %d = %+v, want 0%% demand at 4.5 C outdoor", i, s)
		}
	}
	if cool.TargetC != nil || cool.DeadTimeSec != nil || cool.OvershootC != nil || cool.SettlingSec != nil {
		t.Errorf("cool-down derived = %+v, want none", cool)
	}

	heat := eps[1]
	if heat.Kind != plantHeat || heat.StartedMs != ms("07:00") || heat.EndedMs != ms("09:40") ||
		heat.EndReason != "settled" || heat.TargetC == nil || *heat.TargetC != 21 {
		t.Errorf("heat-up = %+v, want 07:00 -> 09:40 at 21 C, settled", heat)
	}
	var times []int64
	for _, s := range heat.Samples {
		times = append(times, s.AtMs)
	}
	want := []int64{ms("06:50"), ms("07:05"), ms("07:10"), ms("07:20"), ms("07:25"), ms("08:00"),
		ms("08:30"), ms("08:45"), ms("09:00"), ms("09:10")}
	if len(times) != len(want) {
		t.Fatalf("heat-up samples at %v, want %v", times, want)
	}
	for i := range want {
		if times[i] != want[i] {
			t.Fatalf("heat-up samples at %v, want %v", times, want)
		}
	}
	if s := heat.Samples[2]; s.TempC != 16.8 || *s.DemandPct != 80 || s.HeatPct == nil || *s.HeatPct != 100 {
		t.Errorf("07:10 sample = %+v, want 16.8 C, 40a0 80, 4160 100", s)
	}
	if heat.Samples[1].HeatPct != nil {
		t.Errorf("07:05 sample 4160 = %d, want none reported yet", *heat.Samples[1].HeatPct)
	}
	if heat.DeadTimeSec == nil || *heat.DeadTimeSec != 900 || heat.DeadTimeFrom != "4160" {
		t.Errorf("dead time = %v from %q, want 900 s from 4160", heat.DeadTimeSec, heat.DeadTimeFrom)
	}
	if heat.CrossedMs == nil || *heat.CrossedMs != ms("08:45") {
		t.Errorf("crossed = %v, want 08:45", heat.CrossedMs)
	}
	if heat.OvershootC == nil || *heat.OvershootC != 0.4 {
		t.Errorf("overshoot = %v, want 0.4 C", heat.OvershootC)
	}
	if heat.SettlingSec == nil || *heat.SettlingSec != 1500 {
		t.Errorf("settling = %v, want 1500 s", heat.SettlingSec)
	}

	var view map[string]any
	if code := getJSON(t, url+"/api/zones/3/episodes", &view); code != http.StatusOK {
		t.Fatalf("GET episodes: code=%d %v", code, view)
	}
	list, _ := view["episodes"].([]any)
	if view["zone"] != 3.0 || view["settleBandC"] != plantSettleBandC || view["settleHoldSec"] != 1800.0 || len(list) != 2 {
		t.Fatalf("view = %v, want zone 3 with 2 episodes", view)
	}
	got := list[1].(map[string]any)
	samples, _ := got["samples"].([]any)
	if got["kind"] != "heat" || got["deadTimeSec"] != 900.0 || got["deadTimeFrom"] != "4160" ||
		got["overshootC"] != 0.4 || got["settlingSec"] != 1500.0 || len(samples) != 10 {
		t.Errorf("heat-up view = %v", got)
	}
	first := samples[0].(map[string]any)
	for _, key := range []string{"atMs", "tempC", "demandPct", "heatPct", "outdoorC"} {
		if _, ok := first[key]; !ok {
			t.Errorf("sample %v has no %s", first, key)
		}
	}
	if first["heatPct"] != nil {
		t.Errorf("first sample heatPct = %v, want null", first["heatPct"])
	}
	if c := list[0].(map[string]any); c["kind"] != "cool" || c["targetC"] != nil || c["deadTimeSec"] != nil {
		t.Errorf("cool-down view = %v", c)
	}
}

// TestPlantHeatUpEndings: a heat-up the program cuts short keeps its samples
// and derives nothing it did not reach, and one that never settles ends two
// hours after the crossing.
func TestPlantHeatUpEndings(t *testing.T) {
	t.Run("target changed", func(t *testing.T) {
		store := newTestStore(t, t.TempDir()+"/state.db")
		d, url, clock, in := newEarlyDaemon(t, false, store, nil)
		at(t, d, in, clock, "06:50", 17.0, 0)
		at(t, d, in, clock, "07:00", math.NaN(), 0)
		at(t, d, in, clock, "07:30", 18.0, 80)
		var view map[string]any
		if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule", allDayProgram(
			map[string]any{"minute": 0, "setpoint_c": 17.0},
			map[string]any{"minute": 420, "setpoint_c": 19.0},
		), &view); code != http.StatusOK {
			t.Fatalf("schedule: code=%d %v", code, view)
		}
		at(t, d, in, clock, "07:31", math.NaN(), 0)
		eps := plantEpisodesOf(t, store, 3)
		if len(eps) != 1 || eps[0].Kind != plantHeat || eps[0].EndReason != "target changed" || len(eps[0].Samples) != 2 ||
			eps[0].CrossedMs != nil || eps[0].OvershootC != nil || eps[0].SettlingSec != nil {
			t.Fatalf("episodes = %+v, want one heat-up ended by the target change, never crossed", eps)
		}
		if eps[0].DeadTimeSec != nil {
			t.Errorf("dead time = %g, want none: 4060 never rose after the onset", *eps[0].DeadTimeSec)
		}
	})
	t.Run("not settled", func(t *testing.T) {
		store := newTestStore(t, t.TempDir()+"/state.db")
		d, _, clock, in := newEarlyDaemon(t, false, store, nil)
		at(t, d, in, clock, "06:50", 17.0, 0)
		at(t, d, in, clock, "07:00", math.NaN(), 0)
		at(t, d, in, clock, "07:30", 18.0, 80)
		at(t, d, in, clock, "08:00", 21.0, 20)
		for _, hhmm := range []string{"08:30", "09:00", "09:30", "09:59"} {
			at(t, d, in, clock, hhmm, 21.5, 0)
		}
		if eps := plantEpisodesOf(t, store, 3); len(eps) != 0 {
			t.Fatalf("episodes at 09:59 = %+v, want none", eps)
		}
		at(t, d, in, clock, "10:01", 21.5, 0)
		eps := plantEpisodesOf(t, store, 3)
		if len(eps) != 1 || eps[0].EndReason != "not settled" || eps[0].SettlingSec != nil ||
			eps[0].OvershootC == nil || *eps[0].OvershootC != 0.5 || eps[0].DeadTimeFrom != "40a0" || *eps[0].DeadTimeSec != 1800 {
			t.Fatalf("episodes = %+v, want one unsettled heat-up, 0.5 C over, 1800 s dead time from 40a0", eps)
		}
	})
}

// TestPlantCoolDownSegmentation: demand ends a cool-down, a short one is not
// kept, and an open window stops one.
func TestPlantCoolDownSegmentation(t *testing.T) {
	cases := []struct {
		name   string
		run    func(t *testing.T, d *daemon, url string, in *tado.Install, clock *testClock)
		reason string
		n      int
	}{
		{"demand", func(t *testing.T, d *daemon, url string, in *tado.Install, clock *testClock) {
			at(t, d, in, clock, "05:30", 17.0, 0)
			at(t, d, in, clock, "05:50", 16.9, 0)
			at(t, d, in, clock, "06:10", 16.7, 0)
			at(t, d, in, clock, "06:20", 16.7, 30)
		}, "demand", 3},
		{"too short", func(t *testing.T, d *daemon, url string, in *tado.Install, clock *testClock) {
			at(t, d, in, clock, "05:30", 17.0, 0)
			at(t, d, in, clock, "05:50", 16.9, 0)
			at(t, d, in, clock, "05:55", 16.9, 30)
		}, "", 0},
		{"open window", func(t *testing.T, d *daemon, url string, in *tado.Install, clock *testClock) {
			at(t, d, in, clock, "05:30", 17.0, 0)
			at(t, d, in, clock, "06:10", 16.9, 0)
			if _, code := postWindow(t, url, 3, true); code != http.StatusOK {
				t.Fatalf("open: code=%d", code)
			}
			at(t, d, in, clock, "06:11", 16.5, 0)
		}, "open window", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t, t.TempDir()+"/state.db")
			d, url, clock, in := newEarlyDaemon(t, false, store, nil)
			tc.run(t, d, url, in, clock)
			eps := plantEpisodesOf(t, store, 3)
			if tc.n == 0 {
				if len(eps) != 0 {
					t.Fatalf("episodes = %+v, want none", eps)
				}
				return
			}
			if len(eps) != 1 || eps[0].Kind != plantCool || eps[0].EndReason != tc.reason || len(eps[0].Samples) != tc.n ||
				eps[0].StartedMs != ms("05:30") {
				t.Fatalf("episodes = %+v, want one cool-down from 05:30 ended by %s with %d samples", eps, tc.reason, tc.n)
			}
			for _, s := range eps[0].Samples {
				if *s.DemandPct != 0 || s.OutdoorC != nil {
					t.Errorf("sample %+v, want zero demand and no outdoor reading", s)
				}
			}
		})
	}
}

func TestPlantDeadTime(t *testing.T) {
	pct := func(v int) *int { return &v }
	sample := func(min int, tempC float64, demand, heat *int) plantSample {
		return plantSample{AtMs: int64(min) * 60000, TempC: tempC, DemandPct: demand, HeatPct: heat}
	}
	cases := []struct {
		name    string
		samples []plantSample
		sec     float64
		from    string
		ok      bool
	}{
		{"4160 onset", []plantSample{
			sample(0, 18, pct(0), pct(0)), sample(5, 17.9, pct(60), pct(0)), sample(10, 17.9, pct(60), pct(100)),
			sample(20, 17.9, pct(60), pct(100)), sample(30, 18.0, pct(50), pct(100)),
		}, 1200, "4160", true},
		{"40a0 without 4160", []plantSample{
			sample(0, 18, pct(0), nil), sample(5, 17.9, pct(60), nil), sample(15, 18.0, pct(60), nil),
		}, 600, "40a0", true},
		{"demand from the start", []plantSample{
			sample(0, 18, pct(40), nil), sample(10, 18.5, pct(40), nil),
		}, 0, "", false},
		{"no rise", []plantSample{
			sample(0, 18, pct(0), nil), sample(5, 17.9, pct(60), nil), sample(15, 17.8, pct(60), nil),
		}, 0, "", false},
	}
	for _, tc := range cases {
		sec, from, ok := plantDeadTime(tc.samples)
		if sec != tc.sec || from != tc.from || ok != tc.ok {
			t.Errorf("%s: dead time = %g from %q ok=%t, want %g from %q ok=%t", tc.name, sec, from, ok, tc.sec, tc.from, tc.ok)
		}
	}
}

func TestPlantEpisodeRetention(t *testing.T) {
	store := newTestStore(t, t.TempDir()+"/state.db")
	ctx := context.Background()
	add := func(zone int, kind string, started int64) {
		if err := store.AddPlantEpisode(ctx, zone, &plantEpisode{Kind: kind, StartedMs: started}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range plantEpisodesKept + 5 {
		add(3, plantHeat, int64(i))
	}
	for i := range 3 {
		add(3, plantCool, int64(100+i))
	}
	add(2, plantHeat, 200)
	var heat, cool []int64
	for _, ep := range plantEpisodesOf(t, store, 3) {
		switch ep.Kind {
		case plantHeat:
			heat = append(heat, ep.StartedMs)
		case plantCool:
			cool = append(cool, ep.StartedMs)
		}
	}
	if len(heat) != plantEpisodesKept || heat[0] != 5 || heat[len(heat)-1] != plantEpisodesKept+4 {
		t.Errorf("heat-ups kept = %v, want the last %d", heat, plantEpisodesKept)
	}
	if len(cool) != 3 || cool[0] != 100 {
		t.Errorf("cool-downs kept = %v, want all 3", cool)
	}
	if eps := plantEpisodesOf(t, store, 2); len(eps) != 1 {
		t.Errorf("zone 2 episodes = %+v, want its one", eps)
	}
	var rows int
	if err := store.db.QueryRow(`SELECT count(*) FROM plant_episode`).Scan(&rows); err != nil || rows != plantEpisodesKept+4 {
		t.Errorf("rows = %d err=%v, want %d", rows, err, plantEpisodesKept+4)
	}
}

func TestPlantEpisodesAPI(t *testing.T) {
	_, url, _, _ := newEarlyDaemon(t, false, nil, nil)
	var res map[string]any
	if code := getJSON(t, url+"/api/zones/3/episodes", &res); code != http.StatusServiceUnavailable {
		t.Errorf("without a store: code=%d %v, want 503", code, res)
	}
	_, url, _, _ = newEarlyDaemon(t, false, newTestStore(t, t.TempDir()+"/state.db"), nil)
	if code := getJSON(t, url+"/api/zones/0/episodes", &res); code != http.StatusBadRequest {
		t.Errorf("hot water zone: code=%d %v, want 400", code, res)
	}
	if code := getJSON(t, url+"/api/zones/4/episodes", &res); code != http.StatusNotFound {
		t.Errorf("unknown zone: code=%d %v, want 404", code, res)
	}
	res = nil
	if code := getJSON(t, url+"/api/zones/3/episodes", &res); code != http.StatusOK {
		t.Fatalf("zone 3: code=%d %v", code, res)
	}
	if list, ok := res["episodes"].([]any); !ok || len(list) != 0 {
		t.Errorf("zone 3 episodes = %v, want an empty list", res["episodes"])
	}
}

// TestOpenStateStoreMigratesPlantEpisode: a version "6" database gets the
// plant_episode table and keeps its schedules as they are.
func TestOpenStateStoreMigratesPlantEpisode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	old := legacyDatabase(t, path, "6", `DROP TABLE plant_episode`)
	current := tado.EncodeSchedulePrograms([]tado.ScheduleProgram{{DayMask: 0x7f, Entries: []tado.ScheduleEntry{
		{Minute: 0, SetpointCenti: 2100, Enabled: true},
		{Minute: 390, SetpointCenti: 1600, Enabled: false},
	}}})
	const schedKey = "02a6cefa07c51b00"
	if _, err := old.Exec(`INSERT INTO sched_doc (key, body, etag) VALUES (?, ?, ?)`,
		schedKey, current, []byte{1, 2, 3, 4, 5, 6, 7, 8}); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	store := newTestStore(t, path)
	var version string
	if err := store.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != install.SchemaVersion {
		t.Errorf("schema version = %q, want %q", version, install.SchemaVersion)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := state.Schedule[schedKey].Body; string(got) != string(current) {
		t.Errorf("migrated schedule = %x, want it unchanged %x", got, current)
	}
	if err := store.AddPlantEpisode(context.Background(), 3, &plantEpisode{Kind: plantCool}); err != nil {
		t.Fatalf("add after the migration: %v", err)
	}
}
