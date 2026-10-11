package daemon

// flow_limit: the heating flow limit (tag 2040) each circuit is served. A
// circuit with an enabled curve follows the outdoor temperature Home Assistant
// pushes, and falls back to options.flow_limit_c when the reading is stale. With
// options.flow_default_curve a circuit without a curve of its own follows the
// default curve, a reduction under options.flow_limit_c, on the same terms. A
// circuit that no curve and no adaptation manages is served options.flow_limit_c
// when it is set, and no limit otherwise.
// With options.flow_auto_adapt the room feedback trims that limit, see
// flow_adapt.go. The thermostat applies a new value only by re-opening its
// OpenTherm session, so a change needs a minimum step and a minimum interval.

import (
	"encoding/hex"
	"log"
	"math"
	"net/http"
	"time"

	"freedo/tado"
)

// outdoorReading is the last outdoor temperature pushed to the daemon. It is
// not persisted: after a restart the curves wait for the next push.
type outdoorReading struct {
	tempC float64
	at    time.Time
}

// outdoorBoundC bounds a pushed outdoor temperature.
const outdoorBoundC = 60

// flowBase is the limit a managed circuit falls back to: options.flow_limit_c,
// or the default while that is unset.
func flowBase(o tado.Options) int {
	if limit, ok := o.FlowLimit(); ok {
		return limit
	}
	return tado.DefaultFlowLimitC
}

// flowDefaultCurve says whether circuit id follows the default weather curve:
// it is switched on and the circuit has no enabled curve of its own.
func flowDefaultCurve(o tado.Options, id int) bool {
	_, own := o.EnabledFlowCurve(id)
	return o.FlowDefaultCurveEnabled() && !own
}

// flowCeiling is the flow limit the manual value or the curve gives circuit id
// now: flowBase without a fresh outdoor reading. The adaptation never goes
// above it.
func (d *daemon) flowCeiling(o tado.Options, id int, now time.Time) int {
	if outdoor := d.freshOutdoor(o, now); outdoor != nil {
		if curve, ok := o.EnabledFlowCurve(id); ok {
			return curve.FlowAt(*outdoor)
		}
		if flowDefaultCurve(o, id) {
			limit, _ := o.FlowLimit()
			return tado.DefaultFlowAt(limit, *outdoor)
		}
	}
	return flowBase(o)
}

// flowManaged says whether a curve or the adaptation sets the flow limit of
// circuit id, which then refuses a manual value.
func flowManaged(o tado.Options, id int) bool {
	_, curved := o.EnabledFlowCurve(id)
	return curved || o.FlowAutoAdaptEnabled() || o.FlowDefaultCurveEnabled()
}

// flowWanted is the flow limit circuit id asks for now: the ceiling, trimmed
// by the adaptation. It is false when nothing manages the circuit.
func (d *daemon) flowWanted(o tado.Options, id int, now time.Time) (int, bool) {
	if !flowManaged(o, id) {
		return 0, false
	}
	ceiling := d.flowCeiling(o, id, now)
	if a := d.flowAdapt[id]; a != nil && o.FlowAutoAdaptEnabled() {
		return a.limit(ceiling), true
	}
	return ceiling, true
}

// tickFlowLimits serves the target of each managed circuit, when it differs
// from the served value by the hysteresis and the last change is older than
// the minimum interval. The engine start opens the interval, so a restart
// does not change a value at once. It reports whether a served value changed.
func (d *daemon) tickFlowLimits(e *tado.EmuState) bool {
	inst := d.inst.Load()
	if inst == nil {
		return false
	}
	now := d.retryNow()
	changed := false
	for i := range inst.Circuits {
		c := &inst.Circuits[i]
		if inst.Options.FlowAutoAdaptEnabled() {
			d.adaptFlow(e, inst, c.ID, now)
		}
		target, ok := d.flowWanted(inst.Options, c.ID, now)
		if !ok {
			continue
		}
		_, maxC := e.FlowBounds(c.ID)
		target = min(target, maxC)
		served, isServed, _ := e.ServedFlow(c.ID)
		if isServed && math.Abs(float64(target)-served) < float64(inst.Options.FlowHysteresis()) {
			continue
		}
		last, ok := d.flowChanged[c.ID]
		if !ok {
			last = d.flowEpoch
		}
		if now.Sub(last) < inst.Options.FlowMinInterval() {
			continue
		}
		src, err := inst.Resolve(c.HeatSource)
		if err != nil {
			log.Printf("circuit %d flow limit: %v", c.ID, err)
			continue
		}
		if err := e.EnqueueFlowSetpoint(hex.EncodeToString(src.WireEUI()), c.ID, float64(target)); err != nil {
			log.Printf("circuit %d flow limit: %v", c.ID, err)
			continue
		}
		d.flowChanged[c.ID] = now
		changed = true
		log.Printf("circuit %d flow limit %g -> %d C", c.ID, served, target)
	}
	return changed
}

// releaseFlowLimits hands a circuit that the new installation no longer
// manages back to options.flow_limit_c, and drops the adaptation of a circuit
// it no longer adapts.
func (d *daemon) releaseFlowLimits(e *tado.EmuState, before, after *tado.Install) {
	if before == nil {
		return
	}
	dropped := false
	for i := range before.Circuits {
		id := before.Circuits[i].ID
		_, err := after.CircuitByID(id)
		declared := err == nil
		if flowManaged(before.Options, id) && !(declared && flowManaged(after.Options, id)) {
			e.ClearFlowSetpoint(id)
			delete(d.flowChanged, id)
		}
		if _, ok := d.flowAdapt[id]; ok && !(declared && after.Options.FlowAutoAdaptEnabled()) {
			delete(d.flowAdapt, id)
			dropped = true
		}
	}
	if dropped {
		d.saveFlowAdapt()
	}
}

// postOutdoor records the outdoor temperature, {"tempC": number}, and applies
// it to the curves at once.
func (d *daemon) postOutdoor(w http.ResponseWriter, r *http.Request) {
	var b struct {
		TempC *float64 `json:"tempC"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	if b.TempC == nil || math.Abs(*b.TempC) > outdoorBoundC {
		writeErr(w, http.StatusBadRequest, "tempC is required, within -60..60")
		return
	}
	var changed bool
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		d.outdoor = &outdoorReading{tempC: *b.TempC, at: d.retryNow()}
		changed = d.tickFlowLimits(e)
		return d.flowLimitView(e), nil
	})
	if err == nil && changed {
		err = d.persist(nil)
	}
	if writeHandlerErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// flowLimitView renders the outdoor reading and, per circuit, the flow limit
// the curve and the adaptation compute, the one it is served, the lowest and
// highest value it accepts (minC and maxC: the boiler can cut the top), when
// either last changed it and whether the thermostat has fetched it. adaptedC, adaptedAt
// and adaptReason describe the adaptation: its limit, and when and why it last
// moved. defaultCurve says that the circuit follows the default curve, and
// reductionC how far under options.flow_limit_c it puts the limit now: null
// while the outdoor reading is missing or stale.
func (d *daemon) flowLimitView(e *tado.EmuState) map[string]any {
	out := map[string]any{"outdoor": nil, "circuits": []map[string]any{}}
	inst := d.inst.Load()
	if inst == nil {
		return out
	}
	now := d.retryNow()
	out["defaultC"] = nil
	if limit, ok := inst.Options.FlowLimit(); ok {
		out["defaultC"] = limit
	}
	if r := d.outdoor; r != nil {
		age := now.Sub(r.at)
		out["outdoor"] = map[string]any{
			"tempC":  r.tempC,
			"at":     r.at.UTC().Format(time.RFC3339),
			"ageSec": int(age / time.Second),
			"stale":  age > inst.Options.OutdoorMaxAge(),
		}
	}
	circuits := make([]map[string]any, 0, len(inst.Circuits))
	for i := range inst.Circuits {
		id := inst.Circuits[i].ID
		served, isServed, fetched := e.ServedFlow(id)
		minC, maxC := e.FlowBounds(id)
		view := map[string]any{
			"id": id, "curve": false, "computedC": nil, "servedC": nil, "minC": minC, "maxC": maxC,
			"changedAt": nil, "fetched": fetched,
			"adapt": inst.Options.FlowAutoAdaptEnabled(), "adaptedC": nil, "adaptedAt": nil, "adaptReason": nil,
			"defaultCurve": flowDefaultCurve(inst.Options, id), "reductionC": nil,
		}
		if isServed {
			view["servedC"] = served
		}
		if _, ok := inst.Options.EnabledFlowCurve(id); ok {
			view["curve"] = true
		}
		if target, ok := d.flowWanted(inst.Options, id, now); ok {
			view["computedC"] = target
		}
		if outdoor := d.freshOutdoor(inst.Options, now); outdoor != nil && flowDefaultCurve(inst.Options, id) {
			limit, _ := inst.Options.FlowLimit()
			view["reductionC"] = limit - tado.DefaultFlowAt(limit, *outdoor)
		}
		if a := d.flowAdapt[id]; a != nil && inst.Options.FlowAutoAdaptEnabled() {
			view["adaptedC"], view["adaptedAt"], view["adaptReason"] = a.c, a.at.UTC().Format(time.RFC3339), a.reason
		}
		if t, ok := d.flowChanged[id]; ok {
			view["changedAt"] = t.UTC().Format(time.RFC3339)
		}
		circuits = append(circuits, view)
	}
	out["circuits"] = circuits
	return out
}
