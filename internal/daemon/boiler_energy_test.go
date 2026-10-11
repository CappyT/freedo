package daemon

// boiler_energy_test: the boiler energy estimate. Samples, frames and clocks
// are synthetic.

import (
	"context"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"freedo/internal/install"
	"freedo/internal/radio"
	"freedo/tado"
)

var energyStart = time.Date(2026, 1, 5, 6, 0, 0, 0, time.UTC)

func kwOpt(maxKW float64, minKW ...float64) tado.Options {
	o := tado.Options{BoilerMaxPowerKW: &maxKW}
	if len(minKW) > 0 {
		o.BoilerMinPowerKW = &minKW[0]
	}
	return o
}

func burnerSample(at time.Time, modPct int, on bool) tado.BoilerSample {
	return tado.BoilerSample{At: at, ModulationPct: &modPct, Burner: &on}
}

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// feedEvery observes one sample per step from start up to and including start+span.
func feedEvery(m *energyMeter, o tado.Options, start time.Time, span, step time.Duration, mod int, on bool) {
	for at := start; !at.After(start.Add(span)); at = at.Add(step) {
		m.observe(burnerSample(at, mod, on), o)
	}
}

// TestEnergySteadyModulation: an hour at 50 % of 24 kW is 12 kWh, and a
// minimum output of 4 kW moves the 50 % point to 14 kW.
func TestEnergySteadyModulation(t *testing.T) {
	var m energyMeter
	feedEvery(&m, kwOpt(24), energyStart, time.Hour, 10*time.Second, 50, true)
	near(t, "energy at 50 % of 24 kW", m.totalKWh, 12)

	var withMin energyMeter
	feedEvery(&withMin, kwOpt(24, 4), energyStart, time.Hour, 10*time.Second, 50, true)
	near(t, "energy at 50 % of 4..24 kW", withMin.totalKWh, 14)

	var atMin energyMeter
	feedEvery(&atMin, kwOpt(24, 4), energyStart, time.Hour, 10*time.Second, 0, true)
	near(t, "energy with the burner on at 0 %", atMin.totalKWh, 4)

	var clamped energyMeter
	feedEvery(&clamped, kwOpt(24), energyStart, time.Hour, 10*time.Second, 250, true)
	near(t, "energy at an impossible 250 %", clamped.totalKWh, 24)
}

// TestEnergyBurnerOff: the stretch after an off report adds nothing, whatever
// modulation the boiler keeps reporting.
func TestEnergyBurnerOff(t *testing.T) {
	var m energyMeter
	o := kwOpt(24)
	feedEvery(&m, o, energyStart, 30*time.Minute, 10*time.Second, 100, true)
	near(t, "energy of 30 minutes at 100 %", m.totalKWh, 12)
	// The off report closes the running stretch at the old output.
	off := energyStart.Add(30*time.Minute + 10*time.Second)
	m.observe(burnerSample(off, 40, false), o)
	closed := m.totalKWh
	feedEvery(&m, o, off.Add(10*time.Second), 30*time.Minute, 10*time.Second, 40, false)
	near(t, "energy after the burner went off", m.totalKWh, closed)
}

// TestEnergyLongGap: a stretch longer than energyHold between two samples adds
// nothing, one of exactly energyHold counts, and the sample after the gap
// starts the next stretch.
func TestEnergyLongGap(t *testing.T) {
	o := kwOpt(24)
	var m energyMeter
	m.observe(burnerSample(energyStart, 50, true), o)
	m.observe(burnerSample(energyStart.Add(energyHold+time.Second), 50, true), o)
	near(t, "energy of a gap over the hold", m.totalKWh, 0)
	m.observe(burnerSample(energyStart.Add(energyHold+31*time.Second), 50, true), o)
	near(t, "energy of 30 s after the gap", m.totalKWh, 12.0*30/3600)

	var edge energyMeter
	edge.observe(burnerSample(energyStart, 50, true), o)
	edge.observe(burnerSample(energyStart.Add(energyHold), 50, true), o)
	near(t, "energy of a gap of exactly the hold", edge.totalKWh, 12*energyHold.Hours())
}

// TestEnergyRepeatAndOutOfOrder: a repeated sample adds nothing, a sample at
// the same time takes the newer state, and a sample older than the last one is
// dropped, so the counter only grows.
func TestEnergyRepeatAndOutOfOrder(t *testing.T) {
	o := kwOpt(24)
	var m energyMeter
	m.observe(burnerSample(energyStart, 50, true), o)
	m.observe(burnerSample(energyStart, 50, true), o)
	near(t, "energy after a repeat", m.totalKWh, 0)
	if !m.updated.IsZero() {
		t.Errorf("a repeat counts as an integration at %v", m.updated)
	}

	m.observe(burnerSample(energyStart.Add(60*time.Second), 100, true), o)
	one := m.totalKWh
	near(t, "energy of the first minute", one, 12.0/60)
	// An older sample does not move the baseline back, and adds nothing.
	m.observe(burnerSample(energyStart.Add(30*time.Second), 100, true), o)
	near(t, "energy after an older sample", m.totalKWh, one)
	m.observe(burnerSample(energyStart.Add(60*time.Second), 100, true), o)
	near(t, "energy after the same sample again", m.totalKWh, one)
	if want := energyStart.Add(60 * time.Second); !m.updated.Equal(want) {
		t.Errorf("last integration = %v, want %v", m.updated, want)
	}
	m.observe(burnerSample(energyStart.Add(120*time.Second), 100, true), o)
	near(t, "energy of the second minute at 100 %", m.totalKWh, one+24.0/60)

	// Two frames at one instant: the later one decides the next stretch.
	var same energyMeter
	same.observe(burnerSample(energyStart, 50, true), o)
	same.observe(burnerSample(energyStart, 100, true), o)
	same.observe(burnerSample(energyStart.Add(time.Minute), 100, true), o)
	near(t, "energy of a minute at the state set last", same.totalKWh, 24.0/60)

	var empty energyMeter
	empty.observe(tado.BoilerSample{}, o)
	if !empty.prev.At.IsZero() {
		t.Error("an empty sample became the baseline")
	}
}

// TestEnergyUnknownState: the estimate adds nothing while the maximum power,
// the burner flag or, with the burner on, the modulation is unknown.
func TestEnergyUnknownState(t *testing.T) {
	on, mod := true, 50
	for name, tc := range map[string]struct {
		o    tado.Options
		s    func(at time.Time) tado.BoilerSample
		want string
	}{
		"no maximum": {tado.Options{}, func(at time.Time) tado.BoilerSample { return burnerSample(at, 50, true) }, energyNoMaxPower},
		"no burner flag": {kwOpt(24), func(at time.Time) tado.BoilerSample {
			return tado.BoilerSample{At: at, ModulationPct: &mod}
		}, energyNoBurnerFlag},
		"no modulation": {kwOpt(24), func(at time.Time) tado.BoilerSample {
			return tado.BoilerSample{At: at, Burner: &on}
		}, energyNoModulation},
	} {
		var m energyMeter
		m.observe(tc.s(energyStart), tc.o)
		m.observe(tc.s(energyStart.Add(time.Minute)), tc.o)
		if m.totalKWh != 0 {
			t.Errorf("%s: energy = %v, want 0", name, m.totalKWh)
		}
		if _, why := boilerKW(tc.o, tc.s(energyStart)); why != tc.want {
			t.Errorf("%s: reason = %q, want %q", name, why, tc.want)
		}
	}
	// Burner off needs no modulation.
	off := false
	if kw, why := boilerKW(kwOpt(24), tado.BoilerSample{Burner: &off}); kw != 0 || why != "" {
		t.Errorf("burner off without modulation = %v, %q, want 0 and known", kw, why)
	}
}

// newEnergyDaemon builds a daemon the test drives by hand, with no engine loop:
// every call runs on the test goroutine.
func newEnergyDaemon(t *testing.T, in *tado.Install, clock *testClock, store *stateStore) *daemon {
	t.Helper()
	cfg := in.EmuConfig()
	cfg.Now = clock.now
	d := newDaemon(in, tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard)), radio.NewMock(nil), true, "mock")
	d.retryNow = clock.now
	d.state = store
	if err := d.restoreEnergy(); err != nil {
		t.Fatal(err)
	}
	return d
}

// boilerReport feeds an hvac/mon report from the declared thermostat.
func boilerReport(t *testing.T, d *daemon, mid uint16, tlvs ...tado.TLV) {
	t.Helper()
	var body []byte
	for _, tlv := range tlvs {
		body = append(body, tlv.Encode()...)
	}
	frame := uplinkFrame(t, d.inst.Load(), "RU1000000001", false, tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: mid, Path: []string{"h", "7654321", "hvac", "mon"}, Payload: body,
	})
	d.handleFrame(frame, true)
}

func modFlag(mod, flag byte) []tado.TLV {
	return []tado.TLV{{Tag: "0452", Val: []byte{0, mod}}, {Tag: "0457", Val: []byte{flag}}}
}

var flameCurrent = tado.TLV{Tag: "0458", Val: []byte{0x02, 0xaa}}

func installWithKW(t *testing.T, maxKW float64) *tado.Install {
	return windowInstall(t, func(in *tado.Install) { in.Options.BoilerMaxPowerKW = &maxKW })
}

// TestEnergyFromFrames: hvac/mon frames of the thermostat drive the counter and
// the view. Only the frames carry time: the clock moves between them.
func TestEnergyFromFrames(t *testing.T) {
	clock := newTestClock(energyStart)
	d := newEnergyDaemon(t, installWithKW(t, 24), clock, nil)

	if v := d.energyView(clock.now()); v["running"] != false || v["reason"] != energyNoReport {
		t.Errorf("view before a report = %v, want not running with %q", v, energyNoReport)
	}

	boilerReport(t, d, 1, modFlag(50, 1)...)
	v := d.energyView(clock.now())
	if v["running"] != true || v["reason"] != nil || v["powerKW"] != 12.0 || v["totalKWh"] != 0.0 {
		t.Errorf("view after the first report = %v, want running at 12 kW with 0 kWh", v)
	}
	if v["reportedAt"] != energyStart.Format(time.RFC3339) || v["updatedAt"] != nil {
		t.Errorf("times after the first report = %v / %v, want the report time and null", v["reportedAt"], v["updatedAt"])
	}

	// A delta without modulation or flag holds both and closes the stretch.
	clock.advance(time.Minute)
	boilerReport(t, d, 2, flameCurrent)
	d.tickEnergy()
	v = d.energyView(clock.now())
	if v["totalKWh"] != 0.2 || v["powerKW"] != 12.0 || v["updatedAt"] != clock.now().Format(time.RFC3339) {
		t.Errorf("view after a delta = %v, want 0.2 kWh at 12 kW", v)
	}

	clock.advance(time.Minute)
	boilerReport(t, d, 3, modFlag(0, 0)...)
	d.tickEnergy()
	v = d.energyView(clock.now())
	if v["totalKWh"] != 0.4 || v["powerKW"] != 0.0 || v["running"] != true {
		t.Errorf("view with the burner off = %v, want 0.4 kWh at 0 kW and running", v)
	}

	// Ten silent minutes: the view says stale, and the stretch adds nothing.
	clock.advance(10 * time.Minute)
	if v := d.energyView(clock.now()); v["running"] != false || v["reason"] != energyStale || v["powerKW"] != 0.0 {
		t.Errorf("view after a silence = %v, want not running with %q", v, energyStale)
	}
	boilerReport(t, d, 4, modFlag(100, 1)...)
	d.tickEnergy()
	if got := d.energyView(clock.now())["totalKWh"]; got != 0.4 {
		t.Errorf("total after the gap = %v, want 0.4", got)
	}
	clock.advance(30 * time.Second)
	boilerReport(t, d, 5, flameCurrent)
	d.tickEnergy()
	if got := d.energyView(clock.now())["totalKWh"]; got != 0.6 {
		t.Errorf("total after 30 s at 24 kW = %v, want 0.6", got)
	}
}

// TestEnergyOffWithoutMaximumPower: with no maximum the estimate says so, adds
// nothing and still reports the frames.
func TestEnergyOffWithoutMaximumPower(t *testing.T) {
	clock := newTestClock(energyStart)
	d := newEnergyDaemon(t, windowInstall(t, nil), clock, nil)
	boilerReport(t, d, 1, modFlag(80, 1)...)
	clock.advance(time.Minute)
	boilerReport(t, d, 2, flameCurrent)
	d.tickEnergy()
	v := d.energyView(clock.now())
	if v["running"] != false || v["reason"] != energyNoMaxPower || v["totalKWh"] != 0.0 ||
		v["powerKW"] != 0.0 || v["maxPowerKW"] != nil || v["minPowerKW"] != 0.0 {
		t.Errorf("view without a maximum = %v, want off with %q and no counter", v, energyNoMaxPower)
	}
	if v["reportedAt"] == nil {
		t.Error("the report time is missing: the frames did arrive")
	}
}

// TestEnergySurvivesRestart: the counter is written at a bounded rate, the API
// serves only what the database holds, and a new daemon on the same database
// continues from it without adding the downtime.
func TestEnergySurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := newTestStore(t, path)
	clock := newTestClock(energyStart)
	d := newEnergyDaemon(t, installWithKW(t, 24), clock, store)
	ctx := context.Background()
	stored := func(s *stateStore) float64 {
		t.Helper()
		kwh, err := s.BoilerEnergy(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return kwh
	}
	if got := stored(store); got != 0 {
		t.Fatalf("counter of a new database = %v, want 0", got)
	}

	boilerReport(t, d, 1, modFlag(50, 1)...)
	clock.advance(time.Minute)
	boilerReport(t, d, 2, flameCurrent)
	if got := d.energyView(clock.now())["totalKWh"]; got != 0.0 {
		t.Errorf("served total before a write = %v, want 0: the API serves the stored part", got)
	}
	d.tickEnergy()
	if got, shown := stored(store), d.energyView(clock.now())["totalKWh"]; got != 0.2 || shown != 0.2 {
		t.Errorf("stored %v, served %v after the first tick, want 0.2 both", got, shown)
	}

	// More energy, then a tick inside the write interval: nothing is written.
	clock.advance(30 * time.Second)
	boilerReport(t, d, 3, flameCurrent)
	d.tickEnergy()
	if got, shown := stored(store), d.energyView(clock.now())["totalKWh"]; got != 0.2 || shown != 0.2 {
		t.Errorf("stored %v, served %v inside the write interval, want 0.2 both", got, shown)
	}
	clock.advance(31 * time.Second)
	boilerReport(t, d, 4, flameCurrent)
	d.tickEnergy()
	servedBefore := d.energyView(clock.now())["totalKWh"].(float64)
	if got := stored(store); round3(got) != servedBefore || servedBefore <= 0.2 {
		t.Errorf("stored %v, served %v after the interval, want the same value above 0.2", got, servedBefore)
	}

	// Energy that no write holds yet is flushed by saveEnergy, as on a shutdown.
	clock.advance(10 * time.Second)
	boilerReport(t, d, 5, flameCurrent)
	if err := d.saveEnergy(); err != nil {
		t.Fatal(err)
	}
	flushed := stored(store)
	if flushed <= servedBefore {
		t.Errorf("flushed %v, want above %v", flushed, servedBefore)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := newEnergyDaemon(t, installWithKW(t, 24), clock, newTestStore(t, path))
	if got := restarted.energyView(clock.now())["totalKWh"]; got != round3(flushed) {
		t.Errorf("served total after the restart = %v, want %v", got, round3(flushed))
	}
	// An hour of downtime adds nothing: the first report only sets the baseline.
	clock.advance(time.Hour)
	boilerReport(t, restarted, 1, modFlag(100, 1)...)
	if got := restarted.energyView(clock.now())["totalKWh"]; got != round3(flushed) {
		t.Errorf("served total after the first report = %v, want %v", got, round3(flushed))
	}
	clock.advance(time.Minute)
	boilerReport(t, restarted, 2, flameCurrent)
	restarted.tickEnergy()
	got := restarted.energyView(clock.now())["totalKWh"].(float64)
	if got < flushed || math.Abs(got-(flushed+0.4)) > 0.001 {
		t.Errorf("served total after a minute at 24 kW = %v, want %v", got, flushed+0.4)
	}
}

// TestEnergyStoreRefusesACorruptCounter: a stored counter that is not a number
// stops the start instead of restarting from zero.
func TestEnergyStoreRefusesACorruptCounter(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "state.db"))
	for _, raw := range []string{"abc", "-1", "NaN", "+Inf"} {
		if _, err := store.db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, boilerEnergyKey, raw); err != nil {
			t.Fatal(err)
		}
		if kwh, err := store.BoilerEnergy(context.Background()); err == nil {
			t.Errorf("stored %q read as %v, want an error", raw, kwh)
		}
	}
}

// TestEnergyMigratesFromVersionSix: a database of the previous layout holds no
// counter and reads 0. The counter lives in meta, so the layout stays at its
// version and a counter written once survives the next open.
func TestEnergyMigratesFromVersionSix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	old := legacyDatabase(t, path, "6", `DROP TABLE plant_episode`)
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
	ctx := context.Background()
	if kwh, err := store.BoilerEnergy(ctx); err != nil || kwh != 0 {
		t.Fatalf("counter of a migrated database = %v, %v, want 0", kwh, err)
	}
	if err := store.SetBoilerEnergy(ctx, 1234.5678); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if kwh, err := newTestStore(t, path).BoilerEnergy(ctx); err != nil || kwh != 1234.5678 {
		t.Errorf("counter after a reopen = %v, %v, want 1234.5678", kwh, err)
	}
}

// TestEnergyAPI: GET /api/boiler carries the estimate next to the boiler view,
// and the shutdown flush stores the counter.
func TestEnergyAPI(t *testing.T) {
	clock := newTestClock(energyStart)
	store := newTestStore(t, filepath.Join(t.TempDir(), "state.db"))
	in := installWithKW(t, 24)
	d, url := newWindowDaemon(t, in, clock, store)
	frame := func(mid uint16, tlvs ...tado.TLV) {
		t.Helper()
		var body []byte
		for _, tlv := range tlvs {
			body = append(body, tlv.Encode()...)
		}
		feed(t, d, in, "RU1000000001", false, tado.CoAPReq{
			Type: "CON", Code: "0.03", MID: mid, Path: []string{"h", "7654321", "hvac", "mon"}, Payload: body,
		})
	}
	frame(1, modFlag(50, 1)...)
	clock.advance(time.Minute)
	frame(2, flameCurrent)
	if _, err := d.do(func(*tado.EmuState) (any, error) { d.tickEnergy(); return nil, nil }); err != nil {
		t.Fatal(err)
	}

	var boiler struct {
		ModulationPct *int           `json:"ModulationPct"`
		Active        bool           `json:"Active"`
		Energy        map[string]any `json:"energy"`
	}
	if code := getJSON(t, url+"/api/boiler", &boiler); code != http.StatusOK {
		t.Fatalf("GET /api/boiler: code=%d", code)
	}
	if boiler.ModulationPct == nil || *boiler.ModulationPct != 50 || !boiler.Active {
		t.Errorf("boiler view = %+v, want modulation 50 and active", boiler)
	}
	want := map[string]any{
		"running": true, "reason": nil, "powerKW": 12.0, "totalKWh": 0.2,
		"maxPowerKW": 24.0, "minPowerKW": 0.0, "holdSec": 300.0,
		"reportedAt": clock.now().Format(time.RFC3339), "updatedAt": clock.now().Format(time.RFC3339),
	}
	for key, w := range want {
		if got, ok := boiler.Energy[key]; !ok || got != w {
			t.Errorf("energy.%s = %v (present %t), want %v", key, got, ok, w)
		}
	}
	if len(boiler.Energy) != len(want) {
		t.Errorf("energy holds %d fields, want %d: %v", len(boiler.Energy), len(want), boiler.Energy)
	}

	clock.advance(10 * time.Second)
	frame(3, flameCurrent)
	if err := d.saveEnergyBefore(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	kwh, err := store.BoilerEnergy(context.Background())
	if err != nil || math.Abs(kwh-12.0*70/3600) > 1e-9 {
		t.Errorf("stored after the flush = %v, %v, want %v", kwh, err, 12.0*70/3600)
	}
}

// TestInstallAPIPatchesBoilerPower: the installation takes the output of the
// boiler, refuses a value that cannot be one, and null removes it. The estimate
// follows without a restart.
func TestInstallAPIPatchesBoilerPower(t *testing.T) {
	a := newInstallAPI(t)
	view := a.get("/api/install")
	if view["boiler_max_power_kw"] != nil || view["boiler_min_power_kw"] != 0.0 {
		t.Errorf("defaults = %v, %v, want null and 0", view["boiler_max_power_kw"], view["boiler_min_power_kw"])
	}
	boilerEnergy := func() map[string]any {
		t.Helper()
		var boiler struct {
			Energy map[string]any `json:"energy"`
		}
		if code := getJSON(t, a.base+"/api/boiler", &boiler); code != http.StatusOK {
			t.Fatalf("GET /api/boiler: code=%d", code)
		}
		return boiler.Energy
	}
	if e := boilerEnergy(); e["reason"] != energyNoMaxPower || e["maxPowerKW"] != nil {
		t.Errorf("energy without a maximum = %v, want %q", e, energyNoMaxPower)
	}

	a.mutate(http.MethodPatch, "/api/install", map[string]any{"boiler_max_power_kw": 24.5, "boiler_min_power_kw": 2.5})
	if view := a.get("/api/install"); view["boiler_max_power_kw"] != 24.5 || view["boiler_min_power_kw"] != 2.5 {
		t.Errorf("view = %v, %v, want 24.5 and 2.5", view["boiler_max_power_kw"], view["boiler_min_power_kw"])
	}
	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if maxKW, minKW, ok := stored.Options.BoilerPower(); !ok || maxKW != 24.5 || minKW != 2.5 {
		t.Errorf("stored output = %v, %v, %v, want 24.5, 2.5, true", maxKW, minKW, ok)
	}
	if e := boilerEnergy(); e["reason"] != energyNoReport || e["maxPowerKW"] != 24.5 || e["minPowerKW"] != 2.5 {
		t.Errorf("energy with a maximum = %v, want %q at 24.5 and 2.5", e, energyNoReport)
	}

	for what, body := range map[string]map[string]any{
		"zero maximum":          {"boiler_max_power_kw": 0},
		"maximum in watts":      {"boiler_max_power_kw": 24000},
		"negative minimum":      {"boiler_min_power_kw": -1},
		"minimum above maximum": {"boiler_min_power_kw": 30},
		"a string":              {"boiler_max_power_kw": "24"},
	} {
		if code, resp := a.call(http.MethodPatch, "/api/install", body); code != http.StatusBadRequest {
			t.Errorf("%s: code=%d %v, want 400", what, code, resp)
		}
	}

	a.mutate(http.MethodPatch, "/api/install", map[string]any{"boiler_max_power_kw": nil, "boiler_min_power_kw": nil})
	if view := a.get("/api/install"); view["boiler_max_power_kw"] != nil || view["boiler_min_power_kw"] != 0.0 {
		t.Errorf("view after null = %v, %v, want null and 0", view["boiler_max_power_kw"], view["boiler_min_power_kw"])
	}
	if e := boilerEnergy(); e["reason"] != energyNoMaxPower {
		t.Errorf("energy after null = %v, want %q", e, energyNoMaxPower)
	}
}
