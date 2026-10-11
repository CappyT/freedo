package daemon

// flow_limit_test: the weather compensation of the circuit flow limit. The
// installation, the curve and the clock are synthetic.

import (
	"context"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"freedo/tado"
)

// testCurve gives 60 C at -5 C outdoor and 35 C at 15 C: 1.25 C per degree.
var testCurve = tado.FlowCurve{
	Enabled: true,
	Points:  [2]tado.FlowCurvePoint{{OutdoorC: -5, FlowC: 60}, {OutdoorC: 15, FlowC: 35}},
	MinC:    35, MaxC: 60,
}

type flowView struct {
	FlowLimit struct {
		DefaultC int `json:"defaultC"`
		Outdoor  *struct {
			TempC  float64 `json:"tempC"`
			AgeSec int     `json:"ageSec"`
			Stale  bool    `json:"stale"`
		} `json:"outdoor"`
		Circuits []struct {
			ID        int     `json:"id"`
			Curve     bool    `json:"curve"`
			ComputedC *int    `json:"computedC"`
			ServedC   float64 `json:"servedC"`
			ChangedAt *string `json:"changedAt"`
			Fetched   bool    `json:"fetched"`
		} `json:"circuits"`
	} `json:"flowLimit"`
}

func boilerFlow(t *testing.T, url string) flowView {
	t.Helper()
	var v flowView
	if code := getJSON(t, url+"/api/boiler", &v); code != http.StatusOK {
		t.Fatalf("GET /api/boiler: %d", code)
	}
	if len(v.FlowLimit.Circuits) != 1 {
		t.Fatalf("circuits = %+v, want circuit 1", v.FlowLimit.Circuits)
	}
	return v
}

func pushOutdoor(t *testing.T, url string, tempC float64) {
	t.Helper()
	var body map[string]any
	if code := postJSON(t, http.MethodPost, url+"/api/outdoor", map[string]any{"tempC": tempC}, &body); code != http.StatusOK {
		t.Fatalf("POST /api/outdoor %g: %d %v", tempC, code, body)
	}
}

// TestFlowCurveFollowsOutdoor: a circuit with an enabled curve is served the
// value the outdoor reading gives, after the minimum interval, beyond the
// hysteresis, and options.flow_limit_c on a stale reading. No device gets a
// downlink for it.
func TestFlowCurveFollowsOutdoor(t *testing.T) {
	hysteresis := 2
	in := windowInstall(t, func(in *tado.Install) {
		in.Options.FlowCurves = map[int]tado.FlowCurve{1: testCurve}
		in.Options.FlowHysteresisC = &hysteresis
		in.Options.FlowLimitC = new(48)
	})
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	served := func(want float64, step string) {
		t.Helper()
		v := boilerFlow(t, url)
		if got := v.FlowLimit.Circuits[0].ServedC; got != want {
			t.Fatalf("%s: served %g, want %g", step, got, want)
		}
	}

	served(48, "before any reading")
	pushOutdoor(t, url, 0) // 53.75 -> 54
	v := boilerFlow(t, url)
	if c := v.FlowLimit.Circuits[0]; !c.Curve || c.ComputedC == nil || *c.ComputedC != 54 || c.ServedC != 48 || c.ChangedAt != nil {
		t.Fatalf("inside the first interval: %+v, want computed 54 and served 48", c)
	}

	clock.advance(30 * time.Minute)
	pushOutdoor(t, url, 0)
	v = boilerFlow(t, url)
	if c := v.FlowLimit.Circuits[0]; c.ServedC != 54 || c.ChangedAt == nil || c.Fetched {
		t.Fatalf("after the interval: %+v, want 54 served, changed, not fetched", c)
	}

	clock.advance(time.Minute)
	pushOutdoor(t, url, 3) // 50
	served(54, "inside the interval")

	clock.advance(30 * time.Minute)
	pushOutdoor(t, url, 1.6) // 51.75 -> 52, 2 away from 54
	served(52, "past the interval")

	clock.advance(31 * time.Minute)
	pushOutdoor(t, url, 1) // 52.5 -> 53, inside the hysteresis
	served(52, "inside the hysteresis")

	clock.advance(61 * time.Minute)
	if _, err := d.do(func(e *tado.EmuState) (any, error) { return d.tickFlowLimits(e), nil }); err != nil {
		t.Fatal(err)
	}
	v = boilerFlow(t, url)
	if v.FlowLimit.Outdoor == nil || !v.FlowLimit.Outdoor.Stale {
		t.Fatalf("outdoor = %+v, want stale", v.FlowLimit.Outdoor)
	}
	if c := v.FlowLimit.Circuits[0]; *c.ComputedC != 48 || c.ServedC != 48 {
		t.Fatalf("stale reading: %+v, want the fixed 48", c)
	}

	var refused map[string]any
	if code := postJSON(t, http.MethodPost, url+"/api/boiler/flow", map[string]any{"tempC": 50}, &refused); code != http.StatusConflict {
		t.Errorf("a manual flow limit on a curve circuit: %d %v, want 409", code, refused)
	}

	pending, err := d.do(func(e *tado.EmuState) (any, error) {
		n := 0
		for i := range in.Devices {
			n += e.Pending(hex.EncodeToString(in.Devices[i].WireEUI()))
		}
		return n, nil
	})
	if err != nil || pending.(int) != 0 {
		t.Errorf("downlinks queued = %v %v, want none", pending, err)
	}
}

// TestFlowCurveDisableRestoresDefault: disabling a curve hands the circuit
// back to options.flow_limit_c at once.
func TestFlowCurveDisableRestoresDefault(t *testing.T) {
	in := windowInstall(t, func(in *tado.Install) {
		in.Options.FlowCurves = map[int]tado.FlowCurve{1: testCurve}
		in.Options.FlowLimitC = new(48)
	})
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, in, clock, nil)
	boilerFlow(t, url)
	clock.advance(30 * time.Minute)
	pushOutdoor(t, url, -5)
	if got := boilerFlow(t, url).FlowLimit.Circuits[0].ServedC; got != 60 {
		t.Fatalf("served %g, want 60", got)
	}
	off := windowInstall(t, func(in *tado.Install) {
		curve := testCurve
		curve.Enabled = false
		in.Options.FlowCurves = map[int]tado.FlowCurve{1: curve}
		in.Options.FlowLimitC = new(48)
	})
	if _, err := d.do(func(*tado.EmuState) (any, error) {
		_, _, err := d.setInstall(off)
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	v := boilerFlow(t, url)
	if c := v.FlowLimit.Circuits[0]; c.Curve || c.ComputedC != nil || c.ServedC != 48 {
		t.Errorf("after disabling: %+v, want no curve and 48", c)
	}
}

// TestFlowLimitUnset: while options.flow_limit_c is unset a circuit nothing
// manages is served no limit, a manual value is served as it is, and the option
// is served once set.
func TestFlowLimitUnset(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	flow := func() (defaultC any, circuit map[string]any) {
		t.Helper()
		code, body := a.call(http.MethodGet, "/api/boiler", nil)
		if code != http.StatusOK {
			t.Fatalf("GET /api/boiler: %d %v", code, body)
		}
		view := body["flowLimit"].(map[string]any)
		return view["defaultC"], view["circuits"].([]any)[0].(map[string]any)
	}
	if def, c := flow(); def != nil || c["servedC"] != nil {
		t.Errorf("unset: defaultC=%v servedC=%v, want null and null", def, c["servedC"])
	}
	var resp map[string]any
	if code := postJSON(t, http.MethodPost, a.base+"/api/boiler/flow", map[string]any{"tempC": 46}, &resp); code != http.StatusOK {
		t.Fatalf("a manual limit: %d %v, want 200", code, resp)
	}
	if def, c := flow(); def != nil || c["servedC"] != float64(46) {
		t.Errorf("manual: defaultC=%v servedC=%v, want null and 46", def, c["servedC"])
	}
	a.mutate(http.MethodPatch, "/api/install", map[string]any{"flow_limit_c": 52})
	if got := a.get("/api/install")["flow_limit_c"]; got != float64(52) {
		t.Errorf("flow_limit_c = %v after the patch, want 52", got)
	}
	if def, _ := flow(); def != float64(52) {
		t.Errorf("defaultC = %v after the patch, want 52", def)
	}
}

// TestInstallAPIPatchesFlowOptions: the flow limit options round-trip through
// the install table, a curve on an undeclared circuit is refused and a null
// curve removes it.
func TestInstallAPIPatchesFlowOptions(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	view := a.get("/api/install")
	if view["flow_limit_c"] != nil {
		t.Errorf("default flow_limit_c = %v, want null", view["flow_limit_c"])
	}
	for key, want := range map[string]float64{
		"outdoor_max_age_sec": 3600, "flow_hysteresis_c": 1, "flow_min_interval_sec": 1800,
	} {
		if view[key] != want {
			t.Errorf("default %s = %v, want %g", key, view[key], want)
		}
	}
	a.mutate(http.MethodPatch, "/api/install", map[string]any{
		"flow_limit_c": 50, "outdoor_max_age_sec": 900, "flow_hysteresis_c": 2, "flow_min_interval_sec": 3600,
		"flow_curves": map[string]any{"1": testCurve},
	})
	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o := stored.Options
	if limit, _ := o.FlowLimit(); limit != 50 || o.OutdoorMaxAge() != 15*time.Minute || o.FlowHysteresis() != 2 || o.FlowMinInterval() != time.Hour {
		t.Errorf("stored options = %+v", o)
	}
	if c, ok := o.EnabledFlowCurve(1); !ok || c != testCurve {
		t.Errorf("stored curve = %+v %v, want %+v", c, ok, testCurve)
	}
	if err := stored.ValidateCommissionable(true); err != nil {
		t.Fatal(err)
	}
	if a.lastRev, err = a.store.ReplaceInstall(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	if again, _, err := a.store.LoadInstall(context.Background()); err != nil || again.Options.FlowLimitC == nil || *again.Options.FlowLimitC != 50 || again.Options.FlowCurves[1] != testCurve {
		t.Errorf("options after a rewrite = %+v %v", again.Options, err)
	}

	// Deleting a circuit removes its curve.
	a.mutate(http.MethodPost, "/api/install/circuits", map[string]any{"id": 2, "name": "annex", "heat_source": "RU1000000001"})
	a.mutate(http.MethodPatch, "/api/install", map[string]any{"flow_curves": map[string]any{"2": testCurve}})
	a.mutate(http.MethodDelete, "/api/install/circuits/2", nil)
	if stored, _, err = a.store.LoadInstall(context.Background()); err != nil || len(stored.Options.FlowCurves) != 1 {
		t.Errorf("curves after deleting circuit 2 = %v %v, want circuit 1 only", stored.Options.FlowCurves, err)
	}
	if code, body := a.call(http.MethodPatch, "/api/install", map[string]any{"flow_curves": map[string]any{"7": testCurve}}); code != http.StatusBadRequest {
		t.Errorf("a curve on circuit 7: %d %v, want 400", code, body)
	}
	if code, body := a.call(http.MethodPatch, "/api/install", map[string]any{"flow_limit_c": 90}); code != http.StatusBadRequest {
		t.Errorf("flow_limit_c 90: %d %v, want 400", code, body)
	}
	a.mutate(http.MethodPatch, "/api/install", map[string]any{"flow_curves": map[string]any{"1": nil}})
	if curves := a.get("/api/install")["flow_curves"].(map[string]any); len(curves) != 0 {
		t.Errorf("flow_curves after null = %v, want none", curves)
	}
}
