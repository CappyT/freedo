package daemon

// flow_adapt_test: the room feedback trim of the flow limit. The installation,
// the zone reports and the clock are synthetic. The rig has no engine
// goroutine and no HTTP: the test calls the tick itself, one 5 minute step at
// a time, so a run of days takes no real time.

import (
	"encoding/hex"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"freedo/internal/radio"
	"freedo/tado"
)

const (
	adaptStep   = 5 * time.Minute
	adaptValve3 = "VA1000000002" // zone 3
	adaptValve2 = "VA1000000007" // zone 2
)

type adaptWrite struct {
	at time.Time
	c  float64
}

type adaptRig struct {
	t      *testing.T
	d      *daemon
	in     *tado.Install
	clock  *testClock
	mid    uint16
	served float64
	writes []adaptWrite
}

// newAdaptRig starts a daemon on the test installation with the adaptation on
// and a manual limit of 56 C. store may be nil.
func newAdaptRig(t *testing.T, store *stateStore, edit func(*tado.Install)) *adaptRig {
	t.Helper()
	in := windowInstall(t, func(in *tado.Install) {
		on, limit := true, 56
		in.Options.FlowAutoAdapt, in.Options.FlowLimitC = &on, &limit
		if edit != nil {
			edit(in)
		}
	})
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	clock := newTestClock(windowStart)
	cfg := in.EmuConfig()
	cfg.Now = clock.now
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	if err := restoreDesiredState(store, emu); err != nil {
		t.Fatal(err)
	}
	d := newDaemon(in, emu, radio.NewMock(nil), true, "mock")
	d.retryNow = clock.now
	d.state = store
	if err := d.restoreFlowAdapt(); err != nil {
		t.Fatal(err)
	}
	d.flowEpoch = clock.now()
	r := &adaptRig{t: t, d: d, in: in, clock: clock}
	r.served, _, _ = emu.ServedFlow(1)
	return r
}

func (r *adaptRig) feed(ref string, contexted bool, req tado.CoAPReq) {
	r.t.Helper()
	r.d.handleFrame(uplinkFrame(r.t, r.in, ref, contexted, req), true)
}

// target gives the zone a manual overlay at tempC, which is the zone target.
func (r *adaptRig) target(ref string, zone int, tempC float64) {
	r.t.Helper()
	centi := int(math.Round(tempC * 100))
	var body []byte
	for _, tlv := range []tado.TLV{
		{Tag: "6280", Val: []byte{byte(centi >> 8), byte(centi)}}, {Tag: "6240", Val: []byte{0x02}},
		{Tag: "6260", Val: []byte{0x01}}, {Tag: "6200", Val: []byte{0x07, 0xd0}}, {Tag: "62e0", Val: []byte{0x00}},
	} {
		body = append(body, tlv.Encode()...)
	}
	r.mid++
	r.feed(ref, true, tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x5100 + r.mid, Path: []string{"z", "s"},
		Query: []string{"lid=" + strconv.Itoa(zone)}, Payload: body,
	})
}

// report sends the zone report of ref: temperature 4060 and demand 40a0.
func (r *adaptRig) report(ref string, zone int, tempC float64, demand int) {
	r.t.Helper()
	centi := int(math.Round(tempC * 100))
	body := append(tado.TLV{Tag: "4060", Val: []byte{byte(centi >> 8), byte(centi)}}.Encode(),
		tado.TLV{Tag: "40a0", Val: []byte{byte(demand)}}.Encode()...)
	r.mid++
	r.feed(ref, false, tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x6000 + r.mid,
		Path: []string{"h", "7654321", "z", strconv.Itoa(zone), "act"}, Payload: body,
	})
}

// run advances the clock by span, one step at a time. step reports the zones
// and gets the time since run began. Each change of the served limit is
// recorded in writes.
func (r *adaptRig) run(span time.Duration, step func(elapsed time.Duration)) {
	r.t.Helper()
	for elapsed := time.Duration(0); elapsed < span; {
		r.clock.advance(adaptStep)
		elapsed += adaptStep
		if step != nil {
			step(elapsed)
		}
		r.d.tickFlowLimits(r.d.emu)
		if served, _, _ := r.d.emu.ServedFlow(1); served != r.served {
			r.served = served
			r.writes = append(r.writes, adaptWrite{r.clock.now(), served})
		}
	}
}

// satisfied runs span with zone 3 at its target, calling for some heat.
func (r *adaptRig) satisfied(span time.Duration) {
	r.t.Helper()
	r.run(span, func(time.Duration) { r.report(adaptValve3, 3, 21, 25) })
}

// adapted is the adapted limit of circuit 1, 0 before it starts.
func (r *adaptRig) adapted() int {
	if a := r.d.flowAdapt[1]; a != nil {
		return a.c
	}
	return 0
}

func (r *adaptRig) wantAdapted(step string, adapted int, served float64) {
	r.t.Helper()
	if got := r.adapted(); got != adapted {
		r.t.Errorf("%s: adapted %d, want %d", step, got, adapted)
	}
	if got, _, _ := r.d.emu.ServedFlow(1); got != served {
		r.t.Errorf("%s: served %g, want %g", step, got, served)
	}
}

// startAt puts the adaptation of circuit 1 at c, served, as if it had got
// there by itself.
func (r *adaptRig) startAt(c int) {
	r.t.Helper()
	src, err := r.in.Resolve("RU1000000001")
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.d.emu.EnqueueFlowSetpoint(hex.EncodeToString(src.WireEUI()), 1, float64(c)); err != nil {
		r.t.Fatal(err)
	}
	r.served = float64(c)
	r.d.flowAdapt[1] = &flowAdapt{c: c, at: r.clock.now(), reason: adaptStart, rooms: map[int]shortRoom{}}
}

// TestFlowAdaptLowersStepByStepToTheFloor: rooms at their target while the
// circuit calls for heat lower the limit by 2 C every 12 hours, the served
// value follows with one write per step, and the floor stops it.
func TestFlowAdaptLowersStepByStepToTheFloor(t *testing.T) {
	r := newAdaptRig(t, nil, nil)
	r.target(adaptValve3, 3, 21)
	r.satisfied(adaptStep)
	r.wantAdapted("start", 56, 56)
	if a := r.d.flowAdapt[1]; a.reason != adaptStart {
		t.Errorf("reason %q, want %q", a.reason, adaptStart)
	}

	r.satisfied(11*time.Hour + 30*time.Minute)
	r.wantAdapted("inside the first window", 56, 56)
	r.satisfied(time.Hour)
	r.wantAdapted("after 12 hours", 54, 54)
	if a := r.d.flowAdapt[1]; a.reason != adaptSatisfied {
		t.Errorf("reason %q, want %q", a.reason, adaptSatisfied)
	}
	r.satisfied(11 * time.Hour)
	r.wantAdapted("inside the second window", 54, 54)
	r.satisfied(2 * time.Hour)
	r.wantAdapted("after 24 hours", 52, 52)

	r.satisfied(10 * 24 * time.Hour)
	r.wantAdapted("after ten days", 40, 40)
	if len(r.writes) != 8 {
		t.Fatalf("%d writes, want 8 steps of 2 C from 56 to 40: %v", len(r.writes), r.writes)
	}
	for i, w := range r.writes {
		if want := float64(54 - 2*i); w.c != want {
			t.Errorf("write %d = %g, want %g", i, w.c, want)
		}
		if i > 0 && w.at.Sub(r.writes[i-1].at) < 12*time.Hour {
			t.Errorf("write %d came %s after the one before", i, w.at.Sub(r.writes[i-1].at))
		}
	}
}

// TestFlowAdaptWaitsForAHeatCall: a circuit that never calls for heat has no
// evidence, so days of rooms above their target lower nothing.
func TestFlowAdaptWaitsForAHeatCall(t *testing.T) {
	r := newAdaptRig(t, nil, nil)
	r.target(adaptValve3, 3, 21)
	r.run(3*24*time.Hour, func(time.Duration) { r.report(adaptValve3, 3, 22, 0) })
	r.wantAdapted("no heat call", 56, 56)
}

// TestFlowAdaptRaisesWhenARoomStaysShort: a room under full demand that
// climbs is heating up and raises nothing. A room that stands still for an
// hour raises the limit by 4 C, again after each further hour, up to the
// manual limit and no further.
func TestFlowAdaptRaisesWhenARoomStaysShort(t *testing.T) {
	r := newAdaptRig(t, nil, nil)
	r.target(adaptValve3, 3, 21)
	r.startAt(44)

	r.run(3*time.Hour, func(elapsed time.Duration) { r.report(adaptValve3, 3, 18+0.5*elapsed.Hours(), 100) })
	r.wantAdapted("heating up", 44, 44)

	stuck := func(time.Duration) { r.report(adaptValve3, 3, 19.5, 100) }
	r.run(50*time.Minute, stuck)
	r.wantAdapted("short for 50 minutes", 44, 44)
	r.run(20*time.Minute, stuck)
	r.wantAdapted("short for an hour", 48, 48)
	if a := r.d.flowAdapt[1]; a.reason != adaptShort {
		t.Errorf("reason %q, want %q", a.reason, adaptShort)
	}
	r.run(time.Hour+10*time.Minute, stuck)
	r.wantAdapted("short for two hours", 52, 52)
	r.run(time.Hour+10*time.Minute, stuck)
	r.wantAdapted("short for three hours", 56, 56)
	r.run(10*time.Hour, stuck)
	r.wantAdapted("at the manual limit", 56, 56)
}

// TestFlowAdaptManualLimitCaps: the adapted limit never goes above the manual
// limit, and a manual limit lowered under it takes over at once.
func TestFlowAdaptManualLimitCaps(t *testing.T) {
	r := newAdaptRig(t, nil, func(in *tado.Install) {
		limit := 50
		in.Options.FlowLimitC = &limit
	})
	r.target(adaptValve3, 3, 21)
	r.startAt(46)
	r.run(6*time.Hour, func(time.Duration) { r.report(adaptValve3, 3, 19, 100) })
	r.wantAdapted("short for hours", 50, 50)

	lower := 45
	r.in.Options.FlowLimitC = &lower
	r.run(time.Hour, func(time.Duration) { r.report(adaptValve3, 3, 21, 25) })
	r.wantAdapted("manual limit lowered", 50, 45)
	view := r.d.flowLimitView(r.d.emu)["circuits"].([]map[string]any)[0]
	if view["computedC"] != 45 || view["adaptedC"] != 50 || view["adapt"] != true {
		t.Errorf("view %v, want computed 45 under adapted 50", view)
	}
}

// TestFlowAdaptHoldsOnStaleInput: a zone report older than offline_after_sec
// stops both moves, and a zone that never reported gives no evidence.
func TestFlowAdaptHoldsOnStaleInput(t *testing.T) {
	t.Run("lowering", func(t *testing.T) {
		r := newAdaptRig(t, nil, nil)
		r.target(adaptValve3, 3, 21)
		r.satisfied(11 * time.Hour)
		r.run(3*time.Hour, nil)
		r.wantAdapted("reports stopped", 56, 56)
		r.satisfied(time.Hour)
		r.wantAdapted("reports back", 54, 54)
	})
	t.Run("raising", func(t *testing.T) {
		r := newAdaptRig(t, nil, nil)
		r.target(adaptValve3, 3, 21)
		r.startAt(44)
		r.run(30*time.Minute, func(time.Duration) { r.report(adaptValve3, 3, 19.5, 100) })
		r.run(5*time.Hour, nil)
		r.wantAdapted("reports stopped", 44, 44)
	})
	t.Run("never reported", func(t *testing.T) {
		r := newAdaptRig(t, nil, nil)
		r.target(adaptValve3, 3, 21)
		r.run(30*time.Hour, nil)
		r.wantAdapted("no report", 56, 56)
	})
	t.Run("no target", func(t *testing.T) {
		r := newAdaptRig(t, nil, nil)
		r.satisfied(30 * time.Hour)
		r.wantAdapted("no zone block", 56, 56)
	})
}

// TestFlowAdaptOneShortRoomStopsTheLowering: a second room at its target does
// not hide the first one that is short now, and a shortage shorter than an hour
// raises nothing.
func TestFlowAdaptOneShortRoomStopsTheLowering(t *testing.T) {
	r := newAdaptRig(t, nil, nil)
	r.target(adaptValve3, 3, 21)
	r.target(adaptValve2, 2, 20)
	both := func(temp float64, demand int) func(time.Duration) {
		return func(time.Duration) {
			r.report(adaptValve3, 3, 21, 25)
			r.report(adaptValve2, 2, temp, demand)
		}
	}
	r.run(11*time.Hour+50*time.Minute, both(20, 25))
	r.run(50*time.Minute, both(18, 100))
	r.wantAdapted("a room short when the lowering is due", 56, 56)
	r.run(10*time.Minute, both(20, 25))
	r.wantAdapted("the room back at its target", 54, 54)
}

// TestFlowAdaptWritesFollowTheMinimumInterval: the adapted limit moves every
// hour under a shortage, and the thermostat is served one write per interval.
func TestFlowAdaptWritesFollowTheMinimumInterval(t *testing.T) {
	r := newAdaptRig(t, nil, func(in *tado.Install) {
		sec := 2 * 60 * 60
		in.Options.FlowMinIntervalSec = &sec
	})
	r.target(adaptValve3, 3, 21)
	r.startAt(40)
	r.run(10*time.Hour, func(time.Duration) { r.report(adaptValve3, 3, 19, 100) })
	if len(r.writes) < 2 {
		t.Fatalf("writes %v, want several", r.writes)
	}
	for i := 1; i < len(r.writes); i++ {
		if gap := r.writes[i].at.Sub(r.writes[i-1].at); gap < 2*time.Hour {
			t.Errorf("writes %d and %d are %s apart, want 2 hours or more", i-1, i, gap)
		}
	}
	if r.served != 56 {
		t.Errorf("served %g, want 56", r.served)
	}
}

// TestFlowAdaptTrimsTheCurve: the curve gives the ceiling and the trim stays
// under it, whatever the weather does.
func TestFlowAdaptTrimsTheCurve(t *testing.T) {
	r := newAdaptRig(t, nil, func(in *tado.Install) {
		in.Options.FlowCurves = map[int]tado.FlowCurve{1: testCurve}
	})
	outdoor := func(c float64) { r.d.outdoor = &outdoorReading{tempC: c, at: r.clock.now()} }
	r.target(adaptValve3, 3, 21)
	calm := func(time.Duration) {
		outdoor(0) // the curve gives 54
		r.report(adaptValve3, 3, 21, 25)
	}
	r.run(time.Hour, calm)
	r.wantAdapted("curve under the manual limit", 54, 54)
	r.run(12*time.Hour, calm)
	r.wantAdapted("trimmed once", 52, 52)

	r.run(time.Hour, func(time.Duration) { outdoor(-5); r.report(adaptValve3, 3, 21, 25) })
	r.wantAdapted("colder, the trim stays under the 60 C curve", 52, 52)
	r.run(time.Hour, func(time.Duration) { outdoor(15); r.report(adaptValve3, 3, 21, 25) })
	r.wantAdapted("warmer, the 35 C curve takes over", 52, 35)
	r.run(2*time.Hour, func(time.Duration) { r.report(adaptValve3, 3, 21, 25) })
	r.wantAdapted("stale outdoor, the manual limit is the ceiling", 52, 52)
}

// TestFlowAdaptOffChangesNothing: without flow_auto_adapt the circuit keeps
// its manual limit and no adaptation state appears.
func TestFlowAdaptOffChangesNothing(t *testing.T) {
	r := newAdaptRig(t, nil, func(in *tado.Install) { in.Options.FlowAutoAdapt = nil })
	r.target(adaptValve3, 3, 21)
	r.satisfied(48 * time.Hour)
	r.wantAdapted("off", 0, 56)
	if len(r.d.flowAdapt) != 0 || len(r.writes) != 0 {
		t.Errorf("state %v writes %v, want none", r.d.flowAdapt, r.writes)
	}
	c := r.d.flowLimitView(r.d.emu)["circuits"].([]map[string]any)[0]
	if c["adapt"] != false || c["adaptedC"] != nil || c["adaptedAt"] != nil || c["adaptReason"] != nil || c["computedC"] != nil {
		t.Errorf("view %v, want no adaptation", c)
	}
}

// TestFlowAdaptSurvivesRestart: the adapted limit and the served value come
// back from the database, the first ticks write nothing, and the next lowering
// waits for its 12 hours.
func TestFlowAdaptSurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store := newTestStore(t, path)
	r := newAdaptRig(t, store, nil)
	r.target(adaptValve3, 3, 21)
	r.satisfied(25 * time.Hour)
	r.wantAdapted("before the restart", 52, 52)
	movedAt := r.d.flowAdapt[1].at
	r.d.saveEngineState(t.Context(), r.d.emu, "the restart test")
	if r.d.unsaved {
		t.Fatal("the desired state was not saved")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	again := newAdaptRig(t, newTestStore(t, path), nil)
	again.clock.advance(r.clock.now().Sub(windowStart))
	again.d.flowEpoch = again.clock.now()
	again.target(adaptValve3, 3, 21)
	again.wantAdapted("restored", 52, 52)
	if a := again.d.flowAdapt[1]; !a.at.Equal(movedAt) || a.reason != adaptSatisfied {
		t.Errorf("restored %+v, want the move at %s", a, movedAt)
	}
	again.satisfied(time.Hour)
	again.wantAdapted("an hour after the restart", 52, 52)
	if len(again.writes) != 0 {
		t.Errorf("writes %v after the restart, want none", again.writes)
	}
	again.satisfied(10 * time.Hour)
	again.wantAdapted("11 hours after the last move", 52, 52)
	again.satisfied(time.Hour)
	again.wantAdapted("12 hours after the last move", 50, 50)
}

// TestFlowAdaptWithoutLimit: the adaptation of a circuit with no
// options.flow_limit_c starts from the default limit, and serves it.
func TestFlowAdaptWithoutLimit(t *testing.T) {
	r := newAdaptRig(t, nil, func(in *tado.Install) { in.Options.FlowLimitC = nil })
	if _, served, _ := r.d.emu.ServedFlow(1); served {
		t.Fatal("a circuit with no limit starts served")
	}
	r.satisfied(time.Hour)
	r.wantAdapted("an hour after the start", tado.DefaultFlowLimitC, tado.DefaultFlowLimitC)
}

// TestFlowAdaptAPI: flow_auto_adapt is an install setting, the circuit view
// reports the adaptation next to the computed and served values, and a manual
// value is refused while the adaptation runs.
func TestFlowAdaptAPI(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	circuit := func() map[string]any {
		t.Helper()
		code, body := a.call(http.MethodGet, "/api/boiler", nil)
		if code != http.StatusOK {
			t.Fatalf("GET /api/boiler: %d %v", code, body)
		}
		return body["flowLimit"].(map[string]any)["circuits"].([]any)[0].(map[string]any)
	}
	if a.get("/api/install")["flow_auto_adapt"] != false {
		t.Errorf("default flow_auto_adapt = %v, want false", a.get("/api/install")["flow_auto_adapt"])
	}
	if c := circuit(); c["adapt"] != false || c["adaptedC"] != nil || c["adaptReason"] != nil || c["adaptedAt"] != nil {
		t.Errorf("off: %v", c)
	}
	var refused map[string]any
	if code := postJSON(t, http.MethodPost, a.base+"/api/boiler/flow", map[string]any{"tempC": 46}, &refused); code != http.StatusOK {
		t.Fatalf("a manual limit with the adaptation off: %d %v, want 200", code, refused)
	}

	a.mutate(http.MethodPatch, "/api/install", map[string]any{"flow_auto_adapt": true})
	if a.get("/api/install")["flow_auto_adapt"] != true {
		t.Errorf("flow_auto_adapt after the patch = %v, want true", a.get("/api/install")["flow_auto_adapt"])
	}
	if code := postJSON(t, http.MethodPost, a.base+"/api/boiler/flow", map[string]any{"tempC": 50}, &refused); code != http.StatusConflict {
		t.Errorf("a manual limit while the circuit adapts: %d %v, want 409", code, refused)
	}
	stored, _, err := a.store.LoadInstall(t.Context())
	if err != nil || !stored.Options.FlowAutoAdaptEnabled() {
		t.Fatalf("stored options %+v %v, want the adaptation on", stored.Options, err)
	}
	if err := stored.ValidateCommissionable(true); err != nil {
		t.Fatal(err)
	}
	if a.lastRev, err = a.store.ReplaceInstall(t.Context(), stored); err != nil {
		t.Fatal(err)
	}
	if again, _, err := a.store.LoadInstall(t.Context()); err != nil || !again.Options.FlowAutoAdaptEnabled() {
		t.Errorf("options after a rewrite %+v %v, want the adaptation on", again.Options, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for circuit()["adaptedC"] == nil {
		if time.Now().After(deadline) {
			t.Fatalf("no adapted limit: %v", circuit())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if c := circuit(); c["adapt"] != true || c["adaptedC"] != float64(46) || c["adaptReason"] != adaptStart || c["computedC"] != float64(46) || c["servedC"] != float64(46) {
		t.Errorf("on: %v, want the 46 C served at the start adopted", c)
	}

	a.mutate(http.MethodPatch, "/api/install", map[string]any{"flow_auto_adapt": false})
	if c := circuit(); c["adapt"] != false || c["adaptedC"] != nil || c["computedC"] != nil || c["servedC"] != nil {
		t.Errorf("off again: %v, want no limit served and no adaptation", c)
	}
}

// TestFlowAdaptRaiseToTheCurveCeilingPasses: a raise that ends a cut at the
// rounded curve ceiling is served, although the exact ceiling is less than the
// hysteresis from the served cut. testCurve gives 47.625 C at 4.9 C outdoor.
func TestFlowAdaptRaiseToTheCurveCeilingPasses(t *testing.T) {
	r := newAdaptRig(t, nil, func(in *tado.Install) {
		in.Options.FlowCurves = map[int]tado.FlowCurve{1: testCurve}
	})
	outdoor := func() { r.d.outdoor = &outdoorReading{tempC: 4.9, at: r.clock.now()} }
	r.target(adaptValve3, 3, 21)
	outdoor()
	r.startAt(47)
	r.d.flowTrimmed[1] = true
	r.run(3*time.Hour, func(time.Duration) { outdoor(); r.report(adaptValve3, 3, 19.5, 100) })
	r.wantAdapted("short at a cut under the ceiling", 48, 48)
	n := len(r.writes)
	r.run(3*time.Hour, func(time.Duration) { outdoor(); r.report(adaptValve3, 3, 19.5, 100) })
	r.wantAdapted("at the ceiling", 48, 48)
	if len(r.writes) != n {
		t.Errorf("writes %v, want none at the ceiling", r.writes[n:])
	}
}
