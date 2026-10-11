package daemon

// zone_offset: the room temperature correction of a heating zone in mode
// setpoint (tado.OffsetModeSetpoint). The valve keeps its 0140. The daemon
// works on the corrected scale, corrected = reported + shift, as 0140 does on
// the valve (012d = 012e + 0140, capture-analysis-20260827). The valve reaches
// a target T on that scale when it regulates to T - shift, so every setpoint
// the daemon writes to the zone goes out shifted, and every zone temperature
// and target it reports comes back corrected. The node API stays on the wire
// scale. The shift is the stored correction, or the one an external room
// temperature holds while it is on (room_temp.go).

import (
	"log"
	"math"
	"net/http"
	"time"

	"freedo/tado"
)

// shiftC is the shift of zone: the manual correction of mode setpoint, or the
// one an external room temperature holds while it is on (room_temp.go), which
// replaces the manual one. A zone in another mode has none.
func (d *daemon) shiftC(inst *tado.Install, zone int) float64 {
	if x := d.roomTemps[zone]; x != nil && x.on && inst.Options.ZoneOffset(zone).Mode == tado.OffsetModeSetpoint {
		return x.shiftC
	}
	return inst.Options.SetpointShiftC(zone)
}

// rawSetpointC maps a target of zone on the corrected scale to the setpoint
// the valve takes, inside the range of a room setpoint.
func (d *daemon) rawSetpointC(inst *tado.Install, zone int, c float64) float64 {
	if inst == nil {
		return c
	}
	shift := d.shiftC(inst, zone)
	if shift == 0 {
		return c
	}
	return min(max(roundCenti(c-shift), zoneMinC), zoneMaxC)
}

// correctedC maps a temperature of zone on the wire scale to the corrected one.
func (d *daemon) correctedC(inst *tado.Install, zone int, c float64) float64 {
	if inst == nil {
		return c
	}
	shift := d.shiftC(inst, zone)
	if shift == 0 {
		return c
	}
	return roundCenti(c + shift)
}

func roundCenti(c float64) float64 { return math.Round(c*100) / 100 }

// zoneReport is tado.EmuState.ZoneReport with the temperature corrected.
func (d *daemon) zoneReport(e *tado.EmuState, zone int) (float64, int, time.Time, bool) {
	tempC, demand, updated, ok := e.ZoneReport(zone)
	return d.roomTempC(d.inst.Load(), zone, tempC), demand, updated, ok
}

// correctedZoneViews returns the zone reports with each temperature corrected.
func (d *daemon) correctedZoneViews(zones []tado.ZoneView) []tado.ZoneView {
	inst := d.inst.Load()
	out := make([]tado.ZoneView, len(zones))
	for i, z := range zones {
		out[i] = z
		out[i].TempC = d.roomTempC(inst, z.Number, z.TempC)
	}
	return out
}

// postSetpointOffset stores the correction of a zone in mode setpoint. The
// reload restates the zone with it (restateShiftedZones).
func (d *daemon) postSetpointOffset(w http.ResponseWriter, r *http.Request, zone, centi int) {
	d.mutateInstall(w, "zone", func(store *stateStore) (installResult, any, error) {
		res, err := store.PatchZone(r.Context(), zone, zonePatch{SetpointOffsetCenti: &centi})
		if err != nil {
			return res, nil, err
		}
		return res, viewZoneOf(res.Install, zone), nil
	})
}

// restateShiftedZones writes every zone whose shift moved between two
// installations again, so the valves take the setpoints on the new scale.
func (d *daemon) restateShiftedZones(e *tado.EmuState, before, after *tado.Install) {
	for i := range after.Zones {
		zone := after.Zones[i].Number
		if delta := d.shiftC(after, zone) - d.shiftC(before, zone); delta != 0 {
			d.restateZoneShift(e, after, zone, delta)
		}
	}
}

// restateZoneShift writes zone again to each of its zoneWriteTargets after its
// shift moved by delta.
func (d *daemon) restateZoneShift(e *tado.EmuState, inst *tado.Install, zone int, delta float64) {
	d.applyHomeState(e)
	for _, target := range d.zoneWriteTargets(e, zone) {
		if err := d.restateZone(e, inst, zone, target, delta); err != nil {
			log.Printf("zone %d offset restate: %v", zone, err)
			continue
		}
		d.oweProgramWrite(e, zone, target)
		d.requestQueuedDispatch(target)
	}
	d.markPushed(zone)
}

// restateZone rewrites zone on target after its correction moved by delta. A
// running setpoint overlay keeps its target on the corrected scale and its
// deadline, so its wire setpoint moves by -delta. Any other block is a plain
// rewrite, which carries the new 6200. The block is the one a write still
// queued to target sets, else the one the zone holds: a restate of the held
// overlay would replace a queued return to schedule and keep the overlay on.
func (d *daemon) restateZone(e *tado.EmuState, inst *tado.Install, zone int, target string, delta float64) error {
	block, known := e.PendingZoneBlock(target, zone)
	if !known {
		block, known = e.ZoneBlock(zone)
	}
	timer, held := e.ZoneOverlayTimer(zone)
	timed := held && !timer.ExpiresAt.IsZero()
	remaining := timer.ExpiresAt.Sub(d.retryNow())
	if !known || block.Overlay == overlayAutoMode || block.SetpointC == nil ||
		block.Enabled != nil && !*block.Enabled || timed && (timer.Boost || remaining <= 0) {
		return e.EnqueueZoneRewrite(target, zone)
	}
	sp := tado.SetpointParams{
		ID:             zone,
		Setpoint:       min(max(roundCenti(*block.SetpointC-delta), zoneMinC), zoneMaxC),
		Overlay:        block.Overlay,
		RunningOverlay: true,
		EndRule:        timer.Rule,
	}
	if program, ok := d.zoneSched[zone]; ok {
		sp.Sched = d.rawSetpointC(inst, zone, program.c)
	}
	if timed {
		sp.Duration = remaining
	}
	return e.EnqueueSetpoint(target, sp)
}
