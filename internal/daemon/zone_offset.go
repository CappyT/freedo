package daemon

// zone_offset: what the manual room temperature correction of a heating zone
// changes in the daemon, and the release of the setpoint shift a zone in offset
// mode setpoint carried before the store moved it to mode config
// (install.ShiftReleaseKey).

import (
	"context"
	"errors"
	"log"
	"maps"
	"slices"

	"freedo/tado"
)

// noteOffsetChange records that the correction of zone moved, on the engine
// goroutine. Unless the learners read the external reading, it rejects a
// running heat-up episode and ends a running plant heat episode: the write
// moves 4060 by the delta, so an episode that spans it would read a step that
// is not from the plant.
func (d *daemon) noteOffsetChange(inst *tado.Install, zone int) {
	if d.learnerReadsExternal(inst, zone) {
		return
	}
	if d.heating[zone] != nil {
		d.rejectHeatUp(zone, "offset changed")
	}
	if run := d.plant[zone]; run != nil && run.ep.Kind == plantHeat {
		d.endPlant(zone, "offset changed", d.retryNow())
	}
}

// releaseShifts writes each zone in install.ShiftReleaseKey again to each of
// its zoneWriteTargets: its valves still hold setpoints shifted by the old
// correction. It runs once every state is restored and before the engine
// starts, so nothing is delivered before the key goes, and it deletes the key
// only once the owed writes are stored: a start that dies first releases on the
// next start. transmit says the start reaches the valves. Without it the key
// waits for a start that does, because a write never delivered comes back as a
// plain rewrite of the held block, which keeps the shift of an overlay.
func (d *daemon) releaseShifts(e *tado.EmuState, transmit bool) error {
	if d.state == nil {
		return nil
	}
	ctx := context.Background()
	released, err := d.state.ShiftRelease(ctx)
	if err != nil || len(released) == 0 {
		return err
	}
	zones := slices.Sorted(maps.Keys(released))
	if !transmit {
		log.Printf("zones %v: the release of the setpoint shift waits for a start that transmits", zones)
		return nil
	}
	d.applyHomeState(e)
	for _, zone := range zones {
		shiftC := float64(released[zone]) / 100
		for _, target := range d.zoneWriteTargets(e, zone) {
			if err := d.releaseShift(e, zone, target, shiftC); err != nil {
				log.Printf("zone %d shift release: %v", zone, err)
				continue
			}
			d.oweProgramWrite(e, zone, target)
			d.requestQueuedDispatch(target)
		}
		d.markPushed(zone)
	}
	d.saveProgramBlocks()
	if d.schedUnsaved {
		return errors.New("shift release: the owed zone rewrites are not stored")
	}
	return d.state.ClearShiftRelease(ctx)
}

// releaseShift rewrites zone on target without the shift shiftC its setpoints
// carry. A running setpoint overlay keeps its target and its deadline, so its
// setpoint moves by shiftC. Any other block is a plain rewrite, which carries
// the 6200 of the program. The block is the one a write still queued to target
// sets, else the one the zone holds: a rewrite of the held overlay would
// replace a queued return to schedule and keep the overlay on.
func (d *daemon) releaseShift(e *tado.EmuState, zone int, target string, shiftC float64) error {
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
		Setpoint:       min(max(roundCenti(*block.SetpointC+shiftC), zoneMinC), zoneMaxC),
		Overlay:        block.Overlay,
		RunningOverlay: true,
		EndRule:        timer.Rule,
	}
	if program, ok := d.zoneSched[zone]; ok {
		sp.Sched = program.c
	}
	if timed {
		sp.Duration = remaining
	}
	return e.EnqueueSetpoint(target, sp)
}
