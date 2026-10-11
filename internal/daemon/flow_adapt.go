package daemon

// flow_adapt: the trim of the flow limit (tag 2040) from the room feedback.
// A condensing boiler returns more heat from its flue gas when the return
// water is cool, so the limit should sit as low as the rooms allow. The
// limit a circuit is served is min(adapted, ceiling). The ceiling is what the
// manual value options.flow_limit_c gives, or the curve while it follows a
// fresh outdoor reading. The adapted limit starts at the value served when the
// adaptation turns on and moves in two ways only:
//
//   - down by flowAdaptLowerStepC, after flowAdaptLowerAfter without a
//     shortage, once the circuit called for heat for flowAdaptMinHeated and no
//     room is short now;
//   - up by flowAdaptRaiseStepC, when a room that calls for full heat stays
//     thermalMinRiseC or more below its target and gains less than
//     flowAdaptMinGainC over flowAdaptRaiseAfter of zone reports.
//
// It reads only the zone chain (4060 temperature, 40a0 demand) and the zone
// target the daemon keeps. The boiler values (control setpoint 0450, flow
// 044c) are reported as changes and read 0 when a call ends, so they decide
// nothing. A stale zone report or a zone with no target blocks the lowering
// and never raises. The writes go through tickFlowLimits: its hysteresis and
// minimum interval bound the rate. The limit is tag 2040, the OpenTherm
// MaxTSet of central heating; hot water has its own TdhwSet write.

import (
	"context"
	"log"
	"time"

	"freedo/tado"
)

const (
	// flowAdaptFloorC is the lowest adapted limit. A radiator emits less than
	// half its rated output at a 40 C flow, and a room that falls short climbs
	// back through the raise path.
	flowAdaptFloorC = 40
	// flowAdaptLowerStepC is above the default hysteresis, so each step is
	// served, and moves a radiator output by about 6 %.
	flowAdaptLowerStepC = 2
	// flowAdaptRaiseStepC is twice the lowering step: a cold room costs more
	// than a warm flow.
	flowAdaptRaiseStepC = 4
	// flowAdaptLowerAfter spans a day and a night program change, and many
	// room time constants of 1 to 3 hours.
	flowAdaptLowerAfter = 12 * time.Hour
	// flowAdaptMinHeated is the heat call time a lowering needs as evidence,
	// since a limit does nothing while the burner does not run.
	flowAdaptMinHeated = 2 * time.Hour
	// flowAdaptRaiseAfter and flowAdaptMinGainC separate a flow limit that is
	// too low from a plain heat-up: a room that climbs under full demand is
	// heating up, a room that stands still is starved.
	flowAdaptRaiseAfter = time.Hour
	flowAdaptMinGainC   = 0.2
	// flowAdaptFullPct is the demand that counts as full: the zone controller
	// clamps 40a0 at 100 and a persistent error winds its integral up to it.
	flowAdaptFullPct = 90
	// flowAdaptEvery paces the evaluation. A longer gap than flowAdaptMaxStep
	// since the last one earns no heat call time.
	flowAdaptEvery   = time.Minute
	flowAdaptMaxStep = 5 * time.Minute
)

// The reason of the last move of an adapted limit.
const (
	adaptStart     = "start"
	adaptSatisfied = "satisfied"
	adaptShort     = "short"
)

// flowAdapt is the adapted limit of one circuit. c, at and reason are stored
// and survive a restart. The rest starts empty with the daemon: evalAt is the
// last evaluation, heated the heat call time since the last move, shortAt when
// a shortage last stopped the lowering, and rooms the shortage each zone runs.
// Time in a zone report is the time of the report, not of the tick, so a
// report that stopped coming proves nothing.
type flowAdapt struct {
	c       int
	at      time.Time
	reason  string
	evalAt  time.Time
	heated  time.Duration
	shortAt time.Time
	rooms   map[int]shortRoom
}

// shortRoom is a zone that stays below its target under full demand: the
// report it began with, and its temperature and target then.
type shortRoom struct {
	at             time.Time
	tempC, targetC float64
}

// limit is the adapted limit under the ceiling.
func (a *flowAdapt) limit(ceiling int) int { return min(a.c, ceiling) }

// zoneTargetC is the temperature zone z targets now, false when it has none:
// an open window, an off overlay or program block, or away without a
// temperature.
func (d *daemon) zoneTargetC(e *tado.EmuState, z *tado.Zone) (float64, bool) {
	c, ok := d.zoneTargetView(e, z)["targetC"].(float64)
	return c, ok
}

// adaptFlow runs the room feedback of circuit id and moves its adapted limit.
func (d *daemon) adaptFlow(e *tado.EmuState, inst *tado.Install, id int, now time.Time) {
	o := inst.Options
	ceiling := d.flowCeiling(o, id, now)
	a := d.flowAdapt[id]
	if a == nil {
		served, _ := e.ServedFlow(id)
		a = &flowAdapt{c: min(int(served), ceiling), at: now, reason: adaptStart, rooms: map[int]shortRoom{}}
		d.flowAdapt[id] = a
		d.saveFlowAdapt()
	}
	dt := now.Sub(a.evalAt)
	if dt < flowAdaptEvery && !a.evalAt.IsZero() {
		return
	}
	a.evalAt = now
	if dt > flowAdaptMaxStep {
		dt = 0
	}
	eff := a.limit(ceiling)

	var seen, calling, stale, shortNow, raise bool
	short := map[int]bool{}
	for i := range inst.Zones {
		z := &inst.Zones[i]
		if z.Kind != tado.ZoneHeating || z.Circuit == nil || *z.Circuit != id {
			continue
		}
		target, ok := d.zoneTargetC(e, z)
		if !ok {
			continue
		}
		tempC, demand, updated, ok := d.zoneReport(e, z.Number)
		if !ok || now.Sub(updated) > o.OfflineAfter() {
			stale = true
			continue
		}
		seen = true
		calling = calling || demand > 0
		if target-tempC < thermalMinRiseC || demand < flowAdaptFullPct {
			continue
		}
		shortNow = true
		short[z.Number] = true
		r, tracked := a.rooms[z.Number]
		switch {
		case !tracked || r.targetC != target:
			a.rooms[z.Number] = shortRoom{at: updated, tempC: tempC, targetC: target}
		case updated.Sub(r.at) < flowAdaptRaiseAfter:
		case tempC-r.tempC < flowAdaptMinGainC:
			raise = true
		default:
			a.rooms[z.Number] = shortRoom{at: updated, tempC: tempC, targetC: target}
		}
	}
	for zone := range a.rooms {
		if !short[zone] {
			delete(a.rooms, zone)
		}
	}
	if calling {
		a.heated += dt
	}

	quiet := a.at
	if a.shortAt.After(quiet) {
		quiet = a.shortAt
	}
	switch {
	case raise:
		a.shortAt = now
		if next := min(eff+flowAdaptRaiseStepC, ceiling); next > eff {
			d.moveFlowAdapt(id, a, eff, next, adaptShort, now)
		} else {
			clear(a.rooms)
		}
	case !seen || stale || shortNow || eff <= flowAdaptFloorC:
	case now.Sub(quiet) >= flowAdaptLowerAfter && a.heated >= flowAdaptMinHeated:
		d.moveFlowAdapt(id, a, eff, max(eff-flowAdaptLowerStepC, flowAdaptFloorC), adaptSatisfied, now)
	}
}

// moveFlowAdapt sets the adapted limit of circuit id and starts its evidence
// again.
func (d *daemon) moveFlowAdapt(id int, a *flowAdapt, from, c int, reason string, now time.Time) {
	log.Printf("circuit %d adapted flow limit %d -> %d C (%s)", id, from, c, reason)
	a.c, a.at, a.reason = c, now, reason
	a.heated = 0
	clear(a.rooms)
	d.saveFlowAdapt()
}

// saveFlowAdapt stores the adapted limits.
func (d *daemon) saveFlowAdapt() {
	if d.state == nil {
		return
	}
	stored := make(map[int]storedFlowAdapt, len(d.flowAdapt))
	for id, a := range d.flowAdapt {
		stored[id] = storedFlowAdapt{C: a.c, AtMs: a.at.UnixMilli(), Reason: a.reason}
	}
	if err := d.state.SetFlowAdapt(context.Background(), stored); err != nil {
		log.Printf("persist the adapted flow limits: %v", err)
	}
}

// restoreFlowAdapt loads the stored adapted limits before the engine starts. A
// value the circuits cannot be served is dropped: the adaptation starts again
// from the served limit.
func (d *daemon) restoreFlowAdapt() error {
	if d.state == nil {
		return nil
	}
	stored, err := d.state.FlowAdapt(context.Background())
	if err != nil {
		return err
	}
	for id, s := range stored {
		if s.C < tado.FlowMinC || s.C > tado.FlowMaxC {
			continue
		}
		d.flowAdapt[id] = &flowAdapt{c: s.C, at: time.UnixMilli(s.AtMs).UTC(), reason: s.Reason, rooms: map[int]shortRoom{}}
	}
	return nil
}
