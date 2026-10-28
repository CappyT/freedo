package daemon

// flow_default_curve_test: the default weather curve of the flow limit. The
// installation, the outdoor readings and the clock are synthetic. The rig of
// flow_adapt_test.go ticks the limits itself, one 5 minute step at a time, and
// holds a manual limit of 56 C.

import (
	"math"
	"net/http"
	"testing"
	"time"

	"freedo/tado"
)

// newDefaultCurveRig starts a rig with the default curve on and the adaptation
// off.
func newDefaultCurveRig(t *testing.T, edit func(*tado.Install)) *adaptRig {
	t.Helper()
	return newAdaptRig(t, nil, func(in *tado.Install) {
		on := true
		in.Options.FlowAutoAdapt, in.Options.FlowDefaultCurve = nil, &on
		if edit != nil {
			edit(in)
		}
	})
}

func (r *adaptRig) outdoor(c float64) {
	r.d.outdoor = &outdoorReading{tempC: c, at: r.clock.now()}
}

// circuit is the flow limit view of circuit 1.
func (r *adaptRig) circuit() map[string]any {
	r.t.Helper()
	return r.d.flowLimitView(r.d.emu)["circuits"].([]map[string]any)[0]
}

// TestFlowDefaultCurveFollowsOutdoor: the circuit is served the manual limit
// minus the reduction of the outdoor temperature, after the minimum interval
// and never above the manual limit.
func TestFlowDefaultCurveFollowsOutdoor(t *testing.T) {
	for _, tc := range []struct {
		outdoor   float64
		served    float64
		reduction int
	}{
		{-20, 56, 0}, // colder than the data: the full manual limit
		{-5, 54, 2},
		{3, 52, 4},
		{10, 49, 7},
		{16, 48, 8},
		{20, 48, 8}, // the reduction stops growing
	} {
		r := newDefaultCurveRig(t, nil)
		r.outdoor(tc.outdoor)
		r.run(25*time.Minute, nil)
		if c := r.circuit(); c["computedC"] != int(tc.served) || c["servedC"] != float64(56) || len(r.writes) != 0 {
			t.Errorf("outdoor %g, inside the interval: %v, writes %v, want computed %g and 56 served", tc.outdoor, c, r.writes, tc.served)
		}
		r.run(5*time.Minute, nil)
		c := r.circuit()
		if c["servedC"] != tc.served || c["computedC"] != int(tc.served) || c["defaultCurve"] != true || c["curve"] != false || c["reductionC"] != tc.reduction {
			t.Errorf("outdoor %g: %v, want %g served and a reduction of %d", tc.outdoor, c, tc.served, tc.reduction)
		}
		if tc.served > 56 {
			t.Errorf("outdoor %g: served %g, above the manual limit", tc.outdoor, tc.served)
		}
	}
}

// TestFlowDefaultCurveStaleReadingFallsBack: without a fresh outdoor reading
// the circuit returns to the manual limit, as a curve of its own does.
func TestFlowDefaultCurveStaleReadingFallsBack(t *testing.T) {
	r := newDefaultCurveRig(t, nil)
	r.run(time.Hour, nil)
	if c := r.circuit(); c["servedC"] != float64(56) || c["computedC"] != 56 || c["defaultCurve"] != true || c["reductionC"] != nil {
		t.Fatalf("before any reading: %v, want the manual 56 and no reduction", c)
	}
	r.outdoor(16)
	r.run(30*time.Minute, nil)
	r.wantAdapted("fresh reading", 0, 48)

	r.run(31*time.Minute, nil)
	if o := r.d.freshOutdoor(r.in.Options, r.clock.now()); o != nil {
		t.Fatalf("the reading is still fresh: %g", *o)
	}
	r.wantAdapted("stale reading", 0, 56)
	if c := r.circuit(); c["computedC"] != 56 || c["reductionC"] != nil || c["defaultCurve"] != true {
		t.Errorf("stale reading: %v, want the manual 56 and no reduction", c)
	}
	if len(r.writes) != 2 {
		t.Errorf("writes %v, want 48 and 56", r.writes)
	}
}

// TestFlowDefaultCurveOffChangesNothing: without the switch the outdoor reading
// moves no circuit and the view shows no default curve.
func TestFlowDefaultCurveOffChangesNothing(t *testing.T) {
	for name, edit := range map[string]func(*tado.Install){
		"unset": func(in *tado.Install) { in.Options.FlowDefaultCurve = nil },
		"false": func(in *tado.Install) { off := false; in.Options.FlowDefaultCurve = &off },
	} {
		r := newDefaultCurveRig(t, edit)
		r.outdoor(16)
		r.run(2*time.Hour, func(time.Duration) { r.outdoor(16) })
		c := r.circuit()
		if c["defaultCurve"] != false || c["reductionC"] != nil || c["computedC"] != nil || c["servedC"] != float64(56) || len(r.writes) != 0 {
			t.Errorf("%s: %v, writes %v, want 56 served and no default curve", name, c, r.writes)
		}
		if flowManaged(r.in.Options, 1) {
			t.Errorf("%s: the circuit is managed", name)
		}
	}
}

// TestFlowDefaultCurveNeedsTheManualMaximum: the curve does nothing while
// flow_limit_c is unset.
func TestFlowDefaultCurveNeedsTheManualMaximum(t *testing.T) {
	r := newDefaultCurveRig(t, func(in *tado.Install) { in.Options.FlowLimitC = nil })
	r.outdoor(16)
	r.run(2*time.Hour, func(time.Duration) { r.outdoor(16) })
	c := r.circuit()
	if c["defaultCurve"] != false || c["reductionC"] != nil || c["computedC"] != nil || c["servedC"] != nil {
		t.Errorf("%v, want no limit and no default curve", c)
	}
}

// TestFlowDefaultCurveYieldsToACurveOfItsOwn: a circuit with an enabled curve
// follows that curve, whatever the default curve gives. A disabled curve does
// not count.
func TestFlowDefaultCurveYieldsToACurveOfItsOwn(t *testing.T) {
	r := newDefaultCurveRig(t, func(in *tado.Install) {
		in.Options.FlowCurves = map[int]tado.FlowCurve{1: testCurve}
	})
	r.outdoor(10) // the curve of its own gives 41, the default curve 49
	r.run(35*time.Minute, nil)
	c := r.circuit()
	if c["servedC"] != float64(41) || c["curve"] != true || c["defaultCurve"] != false || c["reductionC"] != nil {
		t.Errorf("own curve: %v, want 41 served by the curve of the circuit", c)
	}

	disabled := testCurve
	disabled.Enabled = false
	r = newDefaultCurveRig(t, func(in *tado.Install) {
		in.Options.FlowCurves = map[int]tado.FlowCurve{1: disabled}
	})
	r.outdoor(10)
	r.run(35*time.Minute, nil)
	c = r.circuit()
	if c["servedC"] != float64(49) || c["curve"] != false || c["defaultCurve"] != true || c["reductionC"] != 7 {
		t.Errorf("disabled curve: %v, want 49 served by the default curve", c)
	}
}

// TestFlowDefaultCurveWritesAtMostOncePerInterval: an outdoor temperature that
// keeps moving changes the served limit by the hysteresis or more, and not more
// often than the minimum interval.
func TestFlowDefaultCurveWritesAtMostOncePerInterval(t *testing.T) {
	r := newDefaultCurveRig(t, nil)
	r.run(12*time.Hour, func(elapsed time.Duration) {
		r.outdoor(-5 + 25*elapsed.Hours()/12) // -5 C to 20 C
	})
	if len(r.writes) < 4 {
		t.Fatalf("writes %v, want several steps", r.writes)
	}
	prev := 56.0
	for i, w := range r.writes {
		if i > 0 {
			if gap := w.at.Sub(r.writes[i-1].at); gap < 30*time.Minute {
				t.Errorf("writes %d and %d are %s apart, want 30 minutes or more", i-1, i, gap)
			}
		}
		if w.c > 56 || w.c < 48 || math.Abs(w.c-prev) < 1 {
			t.Errorf("write %d = %g after %g, want a step of 1 C or more inside 48..56", i, w.c, prev)
		}
		prev = w.c
	}
	if r.served != 48 {
		t.Errorf("served %g, want 48", r.served)
	}
}

// TestFlowDefaultCurveTrimsWithTheAdaptation: the default curve gives the
// ceiling and the adaptation trims under it, as under a curve of the user.
func TestFlowDefaultCurveTrimsWithTheAdaptation(t *testing.T) {
	r := newDefaultCurveRig(t, func(in *tado.Install) {
		on := true
		in.Options.FlowAutoAdapt = &on
	})
	r.target(adaptValve3, 3, 21)
	calm := func(c float64) func(time.Duration) {
		return func(time.Duration) {
			r.outdoor(c)
			r.report(adaptValve3, 3, 21, 25)
		}
	}
	r.run(time.Hour, calm(16)) // the default curve gives 48
	r.wantAdapted("curve under the manual limit", 48, 48)
	r.run(12*time.Hour, calm(16))
	r.wantAdapted("trimmed once", 46, 46)

	r.run(time.Hour, calm(-5)) // the default curve gives 54
	r.wantAdapted("colder, the trim stays under the curve", 46, 46)
	r.run(2*time.Hour, func(time.Duration) { r.report(adaptValve3, 3, 21, 25) })
	r.wantAdapted("stale outdoor, the manual limit is the ceiling", 46, 46)
}

// TestFlowDefaultCurveSwitchOffReleasesTheCircuit: turning the switch off hands
// the circuit back to the manual limit at once.
func TestFlowDefaultCurveSwitchOffReleasesTheCircuit(t *testing.T) {
	r := newDefaultCurveRig(t, nil)
	r.outdoor(16)
	r.run(35*time.Minute, nil)
	r.wantAdapted("on", 0, 48)

	off := windowInstall(t, func(in *tado.Install) { in.Options.FlowLimitC = new(56) })
	if _, _, err := r.d.setInstall(off); err != nil {
		t.Fatal(err)
	}
	r.wantAdapted("off", 0, 56)
	if c := r.circuit(); c["defaultCurve"] != false || c["computedC"] != nil || c["reductionC"] != nil {
		t.Errorf("off: %v, want no default curve", c)
	}
}

// TestFlowDefaultCurveAPI: the switch is an install setting that the daemon
// refuses without flow_limit_c. A circuit that follows the curve reports the
// reduction and refuses a manual value, and the switch off frees it.
func TestFlowDefaultCurveAPI(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	circuit := func() map[string]any {
		t.Helper()
		code, body := a.call(http.MethodGet, "/api/boiler", nil)
		if code != http.StatusOK {
			t.Fatalf("GET /api/boiler: %d %v", code, body)
		}
		return body["flowLimit"].(map[string]any)["circuits"].([]any)[0].(map[string]any)
	}
	if got := a.get("/api/install")["flow_default_curve"]; got != false {
		t.Errorf("default flow_default_curve = %v, want false", got)
	}
	if c := circuit(); c["defaultCurve"] != false || c["reductionC"] != nil {
		t.Errorf("off: %v, want no default curve", c)
	}

	if code, body := a.call(http.MethodPatch, "/api/install", map[string]any{"flow_default_curve": true}); code != http.StatusBadRequest {
		t.Errorf("the switch without flow_limit_c: %d %v, want 400", code, body)
	} else if msg, _ := body["error"].(string); msg == "" {
		t.Errorf("the refusal has no reason: %v", body)
	}
	if got := a.get("/api/install")["flow_default_curve"]; got != false {
		t.Errorf("flow_default_curve after the refusal = %v, want false", got)
	}

	a.mutate(http.MethodPatch, "/api/install", map[string]any{"flow_limit_c": 56, "flow_default_curve": true})
	if got := a.get("/api/install")["flow_default_curve"]; got != true {
		t.Errorf("flow_default_curve after the patch = %v, want true", got)
	}
	stored, _, err := a.store.LoadInstall(t.Context())
	if err != nil || !stored.Options.FlowDefaultCurveEnabled() {
		t.Fatalf("stored options %+v %v, want the default curve on", stored.Options, err)
	}
	if err := stored.ValidateCommissionable(true); err != nil {
		t.Fatal(err)
	}
	if a.lastRev, err = a.store.ReplaceInstall(t.Context(), stored); err != nil {
		t.Fatal(err)
	}
	if again, _, err := a.store.LoadInstall(t.Context()); err != nil || !again.Options.FlowDefaultCurveEnabled() {
		t.Errorf("options after a rewrite %+v %v, want the default curve on", again.Options, err)
	}

	var body map[string]any
	if code := postJSON(t, http.MethodPost, a.base+"/api/outdoor", map[string]any{"tempC": 10}, &body); code != http.StatusOK {
		t.Fatalf("POST /api/outdoor: %d %v", code, body)
	}
	// The daemon has just started, so the minimum interval keeps the served value.
	if c := circuit(); c["defaultCurve"] != true || c["curve"] != false || c["reductionC"] != float64(7) || c["computedC"] != float64(49) || c["servedC"] != float64(56) {
		t.Errorf("on: %v, want a reduction of 7, 49 computed and the manual 56 served", c)
	}
	var refused map[string]any
	if code := postJSON(t, http.MethodPost, a.base+"/api/boiler/flow", map[string]any{"tempC": 50}, &refused); code != http.StatusConflict {
		t.Errorf("a manual limit while the circuit follows the default curve: %d %v, want 409", code, refused)
	}

	a.mutate(http.MethodPatch, "/api/install", map[string]any{"flow_default_curve": false})
	if c := circuit(); c["defaultCurve"] != false || c["reductionC"] != nil || c["computedC"] != nil || c["servedC"] != float64(56) {
		t.Errorf("off again: %v, want the manual 56 served and no default curve", c)
	}
	if code := postJSON(t, http.MethodPost, a.base+"/api/boiler/flow", map[string]any{"tempC": 50}, &refused); code != http.StatusOK {
		t.Errorf("a manual limit with the switch off: %d %v, want 200", code, refused)
	}
}

// holdOutdoor runs span with the outdoor readings in values, one per 5 minute
// step in turn, and fails when the served limit of circuit 1 is not want after.
func (r *adaptRig) holdOutdoor(span time.Duration, values []float64, want float64) {
	r.t.Helper()
	n := 0
	r.run(span, func(time.Duration) {
		r.outdoor(values[n%len(values)])
		n++
	})
	if got, _, _ := r.d.emu.ServedFlow(1); got != want {
		r.t.Errorf("outdoor %v: served %g, want %g", values, got, want)
	}
}

// TestFlowDefaultCurveHysteresisUsesUnroundedValue: a reading that crosses a
// rounding boundary of the reduction does not move the served limit; it moves
// once the unrounded value is a full hysteresis away.
func TestFlowDefaultCurveHysteresisUsesUnroundedValue(t *testing.T) {
	r := newDefaultCurveRig(t, nil)
	r.holdOutdoor(35*time.Minute, []float64{11.5}, 49) // exact 49.005
	n := len(r.writes)
	r.holdOutdoor(3*time.Hour, []float64{13.0, 13.1, 13.6, 13.1}, 49)
	r.holdOutdoor(35*time.Minute, []float64{15}, 48) // exact 48.0
	r.holdOutdoor(3*time.Hour, []float64{12.5, 13.0, 12.5}, 48)
	r.holdOutdoor(35*time.Minute, []float64{11}, 49)
	if len(r.writes) != n+2 {
		t.Errorf("writes %v, want 2 after the first served limit", r.writes)
	}
	if c := r.circuit(); c["computedC"] != 49 {
		t.Errorf("view %v, want computedC 49", c)
	}
}

// TestFlowCurveHysteresisUsesUnroundedValue: the same deadband for a custom
// curve, with a value near a .5 boundary. testCurve gives 48.375 at 4.3 C and
// 48.625 at 4.1 C.
func TestFlowCurveHysteresisUsesUnroundedValue(t *testing.T) {
	r := newAdaptRig(t, nil, func(in *tado.Install) {
		in.Options.FlowAutoAdapt = nil
		in.Options.FlowCurves = map[int]tado.FlowCurve{1: testCurve}
	})
	r.holdOutdoor(35*time.Minute, []float64{4.3}, 48)
	n := len(r.writes)
	r.holdOutdoor(3*time.Hour, []float64{4.1, 4.3, 4.1}, 48)
	r.holdOutdoor(35*time.Minute, []float64{3.3}, 50) // exact 49.625
	r.holdOutdoor(35*time.Minute, []float64{4.3}, 48) // exact 48.375
	if len(r.writes) != n+2 {
		t.Errorf("writes %v, want 2 after the first served limit", r.writes)
	}
}
