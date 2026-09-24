package daemon

// flow_limit: the heating flow limit (tag 2040) each circuit is served. A
// circuit with an enabled curve follows the outdoor temperature Home Assistant
// pushes, and falls back to options.flow_limit_c when the reading is stale.
// The thermostat applies a new value only by re-opening its OpenTherm session,
// so a curve change needs a minimum step and a minimum interval.

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

// flowTarget is the flow limit a curve asks for now: options.flow_limit_c
// without a fresh outdoor reading.
func (d *daemon) flowTarget(o tado.Options, curve tado.FlowCurve, now time.Time) int {
	if d.outdoor == nil || now.Sub(d.outdoor.at) > o.OutdoorMaxAge() {
		return o.FlowLimit()
	}
	return curve.FlowAt(d.outdoor.tempC)
}

// tickFlowLimits serves the target of each circuit with an enabled curve,
// when it differs from the served value by the hysteresis and the last change
// is older than the minimum interval. The engine start opens the interval, so
// a restart does not change a value at once. It reports whether a served
// value changed.
func (d *daemon) tickFlowLimits(e *tado.EmuState) bool {
	inst := d.inst.Load()
	if inst == nil {
		return false
	}
	now := d.retryNow()
	changed := false
	for i := range inst.Circuits {
		c := &inst.Circuits[i]
		curve, ok := inst.Options.EnabledFlowCurve(c.ID)
		if !ok {
			continue
		}
		target := d.flowTarget(inst.Options, curve, now)
		served, _ := e.ServedFlow(c.ID)
		if math.Abs(float64(target)-served) < float64(inst.Options.FlowHysteresis()) {
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

// releaseFlowCurves hands a circuit whose curve the new installation disables
// or drops back to options.flow_limit_c.
func (d *daemon) releaseFlowCurves(e *tado.EmuState, before, after *tado.Install) {
	if before == nil {
		return
	}
	for id, curve := range before.Options.FlowCurves {
		if _, still := after.Options.EnabledFlowCurve(id); curve.Enabled && !still {
			e.ClearFlowSetpoint(id)
			delete(d.flowChanged, id)
		}
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
// its curve computes, the one it is served, when a curve last changed it and
// whether the thermostat has fetched it.
func (d *daemon) flowLimitView(e *tado.EmuState) map[string]any {
	out := map[string]any{"outdoor": nil, "circuits": []map[string]any{}}
	inst := d.inst.Load()
	if inst == nil {
		return out
	}
	now := d.retryNow()
	out["defaultC"] = inst.Options.FlowLimit()
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
		served, fetched := e.ServedFlow(id)
		view := map[string]any{
			"id": id, "curve": false, "computedC": nil, "servedC": served,
			"changedAt": nil, "fetched": fetched,
		}
		if curve, ok := inst.Options.EnabledFlowCurve(id); ok {
			target := d.flowTarget(inst.Options, curve, now)
			view["curve"], view["computedC"] = true, target
		}
		if t, ok := d.flowChanged[id]; ok {
			view["changedAt"] = t.UTC().Format(time.RFC3339)
		}
		circuits = append(circuits, view)
	}
	out["circuits"] = circuits
	return out
}
