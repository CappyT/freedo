package daemon

// warm_state: the in-memory state a short restart must not lose. A Home
// Assistant cold backup stops the add-on for about two minutes: the zone
// controllers of the owned zones would restart from zero, and the wall
// thermostat, which sends hvac/mon as delta reports only, would leave an
// unchanged boiler value unknown until it changes. Both are stored in meta
// warm_state and restored only while the save is at most warmStateMaxAge old:
// an older controller state or boiler value no longer describes the plant.

import (
	"context"
	"encoding/hex"
	"log"
	"slices"
	"time"

	"freedo/tado"
)

const (
	// warmStateMaxAge is the oldest save a start restores.
	warmStateMaxAge = 10 * time.Minute
	// warmStateEvery is the least time between two saves of the tick.
	warmStateEvery = time.Minute
)

type storedOwnerSample struct {
	AtMs  int64   `json:"atMs"`
	TempC float64 `json:"tempC"`
}

// storedZoneOwner is the JSON of one zoneOwner. evalAt and pushedAt are not
// stored: the first step after a restart steps the controller and pushes.
type storedZoneOwner struct {
	Zone   int                 `json:"zone"`
	Integ  float64             `json:"integ"`
	Hist   []storedOwnerSample `json:"hist"`
	TempC  float64             `json:"tempC"`
	Source string              `json:"source"`
	Pct    int                 `json:"pct"`
}

// storedBoilerNode is the JSON of one tado.HVACMonNode.
type storedBoilerNode struct {
	Node string            `json:"node"` // wire EUI hex
	AtMs int64             `json:"atMs"`
	Tags map[string]string `json:"tags"`
}

// storedWarmState is the JSON of meta warm_state.
type storedWarmState struct {
	SavedMs int64              `json:"savedMs"`
	Zones   []storedZoneOwner  `json:"zones"`
	Boiler  []storedBoilerNode `json:"boiler"`
}

// warmStateRows reads the state to store from the engine.
func (d *daemon) warmStateRows(e *tado.EmuState, now time.Time) storedWarmState {
	out := storedWarmState{SavedMs: now.UnixMilli(), Zones: []storedZoneOwner{}, Boiler: []storedBoilerNode{}}
	if inst := d.inst.Load(); inst != nil {
		for zone, x := range d.zoneOwners {
			if !x.haveOut || !inst.Options.CoordinatorOwns(zone) {
				continue
			}
			s := storedZoneOwner{Zone: zone, Integ: x.integ, TempC: x.tempC, Source: x.source, Pct: x.pct, Hist: []storedOwnerSample{}}
			for _, h := range x.hist {
				s.Hist = append(s.Hist, storedOwnerSample{AtMs: h.at.UnixMilli(), TempC: h.c})
			}
			out.Zones = append(out.Zones, s)
		}
		slices.SortFunc(out.Zones, func(a, b storedZoneOwner) int { return a.Zone - b.Zone })
	}
	for _, n := range e.ExportHVACMon() {
		out.Boiler = append(out.Boiler, storedBoilerNode{Node: n.Node, AtMs: n.At.UnixMilli(), Tags: n.Tags})
	}
	return out
}

// saveWarmState writes the state now.
func (d *daemon) saveWarmState(e *tado.EmuState) error {
	now := d.retryNow()
	d.warmSaved = now
	if d.state == nil {
		return nil
	}
	return d.state.SetWarmState(context.Background(), d.warmStateRows(e, now))
}

// tickWarmState saves the state once per warmStateEvery. A failed save is
// retried at the next interval.
func (d *daemon) tickWarmState(e *tado.EmuState) {
	if d.state == nil || (!d.warmSaved.IsZero() && d.retryNow().Sub(d.warmSaved) < warmStateEvery) {
		return
	}
	if err := d.saveWarmState(e); err != nil {
		log.Printf("persist the warm state: %v", err)
	}
}

// saveWarmStateBefore writes the state on the engine goroutine, for the
// shutdown.
func (d *daemon) saveWarmStateBefore(deadline time.Time) error {
	timeout := time.Until(deadline)
	if timeout <= 0 {
		return errEngineBusy
	}
	_, err := d.doWithin(timeout, func(e *tado.EmuState) (any, error) { return nil, d.saveWarmState(e) })
	return err
}

// restoreWarmState loads the stored state before the engine starts, when the
// save is at most warmStateMaxAge old. A controller of a zone the coordinator
// no longer owns, and a boiler node the installation no longer declares, is
// dropped.
func (d *daemon) restoreWarmState(e *tado.EmuState) error {
	if d.state == nil {
		return nil
	}
	stored, err := d.state.WarmState(context.Background())
	if err != nil {
		// A row this start cannot read costs only the warm start.
		log.Printf("restore the warm state: %v, ignored", err)
		return nil
	}
	now := d.retryNow()
	// A save in the future comes from a clock that stepped back: its age is unknown.
	if age := now.Sub(time.UnixMilli(stored.SavedMs)); stored.SavedMs == 0 || age < 0 || age > warmStateMaxAge {
		return nil
	}
	inst := d.inst.Load()
	for _, s := range stored.Zones {
		if inst == nil || d.zoneOwners[s.Zone] != nil {
			continue
		}
		if z, err := inst.ZoneByNumber(s.Zone); err != nil || z.Kind != tado.ZoneHeating || !inst.Options.CoordinatorOwns(s.Zone) {
			log.Printf("zone %d owner: stored controller state of a zone the coordinator does not own, dropped", s.Zone)
			continue
		}
		x := &zoneOwner{integ: s.Integ, tempC: s.TempC, source: s.Source, pct: s.Pct, haveOut: true}
		for _, h := range s.Hist {
			x.hist = append(x.hist, ownerSample{at: time.UnixMilli(h.AtMs).UTC(), c: h.TempC})
		}
		d.zoneOwners[s.Zone] = x
		log.Printf("zone %d owner: controller restored, integ %.0f, %.2f C (%s), demand %d%%", s.Zone, s.Integ, s.TempC, s.Source, s.Pct)
	}
	for _, s := range stored.Boiler {
		set, ok := e.RestoreHVACMon(tado.HVACMonNode{Node: s.Node, At: time.UnixMilli(s.AtMs).UTC(), Tags: s.Tags})
		if !ok {
			log.Printf("boiler: stored monitor tags of %s, a node the installation does not declare, dropped", s.Node)
			continue
		}
		wire, _ := hex.DecodeString(s.Node)
		log.Printf("boiler: %s monitor tags restored, %d set", tado.ShortFromWire(wire), set)
	}
	if len(stored.Boiler) > 0 {
		d.energy.floor = now
	}
	return nil
}
