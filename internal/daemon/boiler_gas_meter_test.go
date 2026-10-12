package daemon

// boiler_gas_meter_test: the gas meter readings recorded against the fuel
// counter. Readings, counters and clocks are synthetic.

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"freedo/internal/radio"
	"freedo/tado"
)

// newMeterDaemon starts a dry-run daemon on in with an injected clock, an
// optional store and the stored energy restored, and returns it with its API
// base URL.
func newMeterDaemon(t *testing.T, in *tado.Install, clock *testClock, store *stateStore) (*daemon, string) {
	t.Helper()
	cfg := in.EmuConfig()
	cfg.Now = clock.now
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	if err := restoreDesiredState(store, emu); err != nil {
		t.Fatal(err)
	}
	d := newDaemon(in, emu, radio.NewMock(nil), true, "mock")
	d.retryNow = clock.now
	d.state = store
	if err := d.restoreEnergy(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	return d, srv.URL
}

// setGasCounter moves the fuel counter on the engine goroutine.
func setGasCounter(t *testing.T, d *daemon, m3 float64) {
	t.Helper()
	if _, err := d.do(func(*tado.EmuState) (any, error) { d.energy.totalM3, d.energy.shownM3 = m3, m3; return nil, nil }); err != nil {
		t.Fatal(err)
	}
}

// meterOf reads energy.gas.meter through GET /api/boiler.
func meterOf(t *testing.T, url string) map[string]any {
	t.Helper()
	var boiler struct {
		Energy struct {
			Gas struct {
				Meter map[string]any `json:"meter"`
			} `json:"gas"`
		} `json:"energy"`
	}
	if code := getJSON(t, url+"/api/boiler", &boiler); code != http.StatusOK {
		t.Fatalf("GET /api/boiler: code=%d", code)
	}
	return boiler.Energy.Gas.Meter
}

// postReading records one reading and returns the response body.
func postReading(t *testing.T, url string, body map[string]any) (int, map[string]any) {
	t.Helper()
	var resp map[string]any
	code := postJSON(t, http.MethodPost, url+"/api/boiler/gas/meter", body, &resp)
	return code, resp
}

// nearMeter checks a meter value to the millilitre the view rounds to.
func nearMeter(t *testing.T, what string, got any, want float64) {
	t.Helper()
	f, ok := got.(float64)
	if !ok || math.Abs(f-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// TestGasFactor: the factor comes from the two most recent readings at least
// one unit of estimate apart, and an implausible one is refused.
func TestGasFactor(t *testing.T) {
	r := func(at, reading, estimate float64) gasReading {
		return gasReading{Reading: reading, Estimate: estimate}
	}
	for name, tc := range map[string]struct {
		rs     []gasReading
		factor float64
		count  int
	}{
		"none":             {nil, 1, 0},
		"one":              {[]gasReading{r(0, 100, 0)}, 1, 0},
		"delta of 10":      {[]gasReading{r(0, 100, 0), r(0, 111, 10)}, 1.1, 2},
		"under one unit":   {[]gasReading{r(0, 100, 0), r(0, 100.4, 0.5)}, 1, 0},
		"implausible low":  {[]gasReading{r(0, 100, 0), r(0, 103, 10)}, 1, 0},
		"implausible high": {[]gasReading{r(0, 100, 0), r(0, 130, 10)}, 1, 0},
		"skips a close one": {[]gasReading{r(0, 100, 0), r(0, 101, 10), r(0, 111, 10.5)},
			11.0 / 10.5, 2},
	} {
		factor, count := gasFactor(tc.rs)
		if math.Abs(factor-tc.factor) > 1e-9 || count != tc.count {
			t.Errorf("%s: factor = %v (%d), want %v (%d)", name, factor, count, tc.factor, tc.count)
		}
	}
}

// TestGasMeterFirstReading: the first reading is the meter value with factor 1,
// and the field default estimate is the current counter.
func TestGasMeterFirstReading(t *testing.T) {
	clock := newTestClock(energyStart)
	_, url := newMeterDaemon(t, installWithGas(t, 2.7, 0.3), clock, nil)

	if m := meterOf(t, url); m != nil {
		t.Errorf("meter before a reading = %v, want null", m)
	}
	code, resp := postReading(t, url, map[string]any{"reading": 8785.957})
	if code != http.StatusOK {
		t.Fatalf("POST reading: code=%d %v", code, resp)
	}
	nearMeter(t, "value", resp["value"], 8785.957)
	if resp["factor"] != 1.0 || resp["factorReadings"] != 0.0 || resp["readings"] != 1.0 {
		t.Errorf("first reading = %v, want factor 1 with 0 factor readings and 1 reading", resp)
	}
	if resp["lastReading"] != 8785.957 || resp["lastReadingAt"] != energyStart.Format(time.RFC3339) {
		t.Errorf("first reading fields = %v, want the reading at %v", resp, energyStart)
	}
	m := meterOf(t, url)
	nearMeter(t, "served value", m["value"], 8785.957)
}

// TestGasMeterFollowsTheCounter: between readings the value follows the fuel
// counter at the learned factor, and the raw counter and flow stay unchanged.
func TestGasMeterFollowsTheCounter(t *testing.T) {
	clock := newTestClock(energyStart)
	d, url := newMeterDaemon(t, installWithGas(t, 2.7, 0.3), clock, nil)
	setGasCounter(t, d, 10)
	if code, resp := postReading(t, url, map[string]any{"reading": 100}); code != http.StatusOK {
		t.Fatalf("POST reading: code=%d %v", code, resp)
	}
	setGasCounter(t, d, 25)
	nearMeter(t, "value following the counter", meterOf(t, url)["value"], 115)

	var boiler struct {
		Energy struct {
			Gas map[string]any `json:"gas"`
		} `json:"energy"`
	}
	if code := getJSON(t, url+"/api/boiler", &boiler); code != http.StatusOK {
		t.Fatalf("GET /api/boiler: code=%d", code)
	}
	if boiler.Energy.Gas["totalM3"] != 25.0 {
		t.Errorf("totalM3 = %v, want the raw counter 25", boiler.Energy.Gas["totalM3"])
	}
}

// TestGasMeterLearnsTheFactor: two readings ten units of estimate apart with a
// meter delta of eleven learn 1.1, and the value uses it.
func TestGasMeterLearnsTheFactor(t *testing.T) {
	clock := newTestClock(energyStart)
	d, url := newMeterDaemon(t, installWithGas(t, 2.7, 0.3), clock, nil)
	setGasCounter(t, d, 10)
	if code, resp := postReading(t, url, map[string]any{"reading": 100}); code != http.StatusOK {
		t.Fatalf("first reading: code=%d %v", code, resp)
	}
	setGasCounter(t, d, 20)
	code, resp := postReading(t, url, map[string]any{"reading": 111})
	if code != http.StatusOK {
		t.Fatalf("second reading: code=%d %v", code, resp)
	}
	if resp["factor"] != 1.1 || resp["factorReadings"] != 2.0 {
		t.Fatalf("factor after two readings = %v, want 1.1 with 2 factor readings", resp)
	}
	nearMeter(t, "value at the reading", resp["value"], 111)
	setGasCounter(t, d, 30)
	nearMeter(t, "value with the factor", meterOf(t, url)["value"], 122)
}

// TestGasMeterRefusesAnImplausibleFactor: a meter delta that does not match the
// estimate delta leaves the factor at 1.
func TestGasMeterRefusesAnImplausibleFactor(t *testing.T) {
	clock := newTestClock(energyStart)
	d, url := newMeterDaemon(t, installWithGas(t, 2.7, 0.3), clock, nil)
	setGasCounter(t, d, 0)
	postReading(t, url, map[string]any{"reading": 100})
	setGasCounter(t, d, 10)
	code, resp := postReading(t, url, map[string]any{"reading": 103})
	if code != http.StatusOK {
		t.Fatalf("second reading: code=%d %v", code, resp)
	}
	if resp["factor"] != 1.0 || resp["factorReadings"] != 0.0 {
		t.Errorf("factor with a delta of 3 over 10 = %v, want 1 with 0 factor readings", resp)
	}
}

// TestGasMeterNoFactorUnderOneUnit: two readings less than one unit of estimate
// apart learn nothing.
func TestGasMeterNoFactorUnderOneUnit(t *testing.T) {
	clock := newTestClock(energyStart)
	d, url := newMeterDaemon(t, installWithGas(t, 2.7, 0.3), clock, nil)
	setGasCounter(t, d, 0)
	postReading(t, url, map[string]any{"reading": 100})
	setGasCounter(t, d, 0.5)
	code, resp := postReading(t, url, map[string]any{"reading": 100.4})
	if code != http.StatusOK {
		t.Fatalf("second reading: code=%d %v", code, resp)
	}
	if resp["factor"] != 1.0 || resp["factorReadings"] != 0.0 {
		t.Errorf("factor under one unit = %v, want 1 with 0 factor readings", resp)
	}
}

// TestGasMeterEstimateAndAt: the stored estimate defaults to the counter and can
// be given; at is stored for display.
func TestGasMeterEstimateAndAt(t *testing.T) {
	clock := newTestClock(energyStart)
	d, url := newMeterDaemon(t, installWithGas(t, 2.7, 0.3), clock, nil)
	setGasCounter(t, d, 50)
	taken := energyStart.Add(-time.Hour)
	code, resp := postReading(t, url, map[string]any{
		"reading": 200, "estimate": 20, "at": taken.Format(time.RFC3339),
	})
	if code != http.StatusOK {
		t.Fatalf("POST reading: code=%d %v", code, resp)
	}
	if resp["lastReadingAt"] != taken.UTC().Format(time.RFC3339) {
		t.Errorf("lastReadingAt = %v, want %v", resp["lastReadingAt"], taken.UTC().Format(time.RFC3339))
	}
	// The estimate of 20, not the counter of 50, is the baseline: the value
	// follows the counter from the older estimate.
	nearMeter(t, "value from the estimate", resp["value"], 230)
	setGasCounter(t, d, 70)
	nearMeter(t, "value following the counter", meterOf(t, url)["value"], 250)
}

// TestGasMeterValidation: a bad reading, an estimate above the counter, a
// reading below the previous one and a future at are refused.
func TestGasMeterValidation(t *testing.T) {
	clock := newTestClock(energyStart)
	d, url := newMeterDaemon(t, installWithGas(t, 2.7, 0.3), clock, nil)
	setGasCounter(t, d, 50)
	for what, body := range map[string]map[string]any{
		"missing reading":   {},
		"negative reading":  {"reading": -1},
		"reading over 1e9":  {"reading": 2e9},
		"estimate negative": {"reading": 100, "estimate": -1},
		"estimate over":     {"reading": 100, "estimate": 51},
		"bad at":            {"reading": 100, "at": "yesterday"},
		"at in the future":  {"reading": 100, "at": energyStart.Add(time.Hour).Format(time.RFC3339)},
	} {
		if code, resp := postReading(t, url, body); code != http.StatusBadRequest {
			t.Errorf("%s: code=%d %v, want 400", what, code, resp)
		}
	}
	if code, resp := postReading(t, url, map[string]any{"reading": 100}); code != http.StatusOK {
		t.Fatalf("valid reading: code=%d %v", code, resp)
	}
	if code, resp := postReading(t, url, map[string]any{"reading": 99}); code != http.StatusBadRequest {
		t.Errorf("lower reading: code=%d %v, want 400", code, resp)
	}
}

// TestGasMeterDelete: DELETE removes the last reading and answers 404 when none
// is stored.
func TestGasMeterDelete(t *testing.T) {
	clock := newTestClock(energyStart)
	d, url := newMeterDaemon(t, installWithGas(t, 2.7, 0.3), clock, nil)
	setGasCounter(t, d, 10)
	if code := postJSON(t, http.MethodDelete, url+"/api/boiler/gas/meter", nil, &map[string]any{}); code != http.StatusNotFound {
		t.Errorf("DELETE with no reading: code=%d, want 404", code)
	}
	postReading(t, url, map[string]any{"reading": 100})
	postReading(t, url, map[string]any{"reading": 120})
	var resp map[string]any
	if code := postJSON(t, http.MethodDelete, url+"/api/boiler/gas/meter", nil, &resp); code != http.StatusOK {
		t.Fatalf("DELETE: code=%d %v", code, resp)
	}
	if resp["readings"] != 1.0 || resp["lastReading"] != 100.0 {
		t.Errorf("after DELETE = %v, want one reading of 100", resp)
	}
	postJSON(t, http.MethodDelete, url+"/api/boiler/gas/meter", nil, &resp)
	if m := meterOf(t, url); m != nil {
		t.Errorf("meter after removing every reading = %v, want null", m)
	}
}

// TestGasMeterWithoutFuelFlow: the meter needs the fuel counter, so a boiler
// with no gas flow answers 503.
func TestGasMeterWithoutFuelFlow(t *testing.T) {
	clock := newTestClock(energyStart)
	_, url := newMeterDaemon(t, installWithKW(t, 24), clock, nil)
	if code, resp := postReading(t, url, map[string]any{"reading": 100}); code != http.StatusServiceUnavailable {
		t.Errorf("POST without a fuel flow: code=%d %v, want 503", code, resp)
	}
}

// TestGasMeterWritesAtOnce: POST and DELETE write the readings in the same call.
func TestGasMeterWritesAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := newTestStore(t, path)
	clock := newTestClock(energyStart)
	d, url := newMeterDaemon(t, installWithGas(t, 2.7, 0.3), clock, store)
	setGasCounter(t, d, 10)
	postReading(t, url, map[string]any{"reading": 100})
	ctx := context.Background()
	rs, err := store.GasMeterReadings(ctx)
	if err != nil || len(rs) != 1 || rs[0].Reading != 100 || rs[0].Estimate != 10 {
		t.Fatalf("stored readings after POST = %+v, %v, want one reading of 100 at 10", rs, err)
	}
	var resp map[string]any
	if code := postJSON(t, http.MethodDelete, url+"/api/boiler/gas/meter", nil, &resp); code != http.StatusOK {
		t.Fatalf("DELETE: code=%d %v", code, resp)
	}
	if rs, err := store.GasMeterReadings(ctx); err != nil || len(rs) != 0 {
		t.Errorf("stored readings after DELETE = %+v, %v, want none", rs, err)
	}
}

// TestGasMeterRestoresAcrossAReopen: the readings and the learned factor
// survive a store reopen, and the restart serves the same meter value.
func TestGasMeterRestoresAcrossAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := newTestStore(t, path)
	ctx := context.Background()
	readings := []gasReading{
		{At: energyStart, Reading: 100, Estimate: 10},
		{At: energyStart.Add(time.Hour), Reading: 111, Estimate: 20},
	}
	if err := store.SetGasMeterReadings(ctx, readings); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBoilerGas(ctx, 30); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	clock := newTestClock(energyStart)
	d := newEnergyDaemon(t, installWithGas(t, 2.7, 0.3), clock, newTestStore(t, path))
	m := d.gasMeterView()
	if m["readings"] != 2 || m["factor"] != 1.1 || m["factorReadings"] != 2 {
		t.Fatalf("restored meter = %v, want two readings and factor 1.1", m)
	}
	if m["lastReading"] != 111.0 || m["lastReadingAt"] != energyStart.Add(time.Hour).Format(time.RFC3339) {
		t.Errorf("restored last reading = %v, want 111 at the stored time", m)
	}
	nearMeter(t, "restored value", m["value"], 111+1.1*(30-20))
}
