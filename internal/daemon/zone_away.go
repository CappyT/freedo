package daemon

// zone_away: the away temperature of a heating zone. The valve takes it as the
// 6200 of the away block, with 61e0=1 (tado.Options.AwayTemperature). At home
// nothing carries it, so a change waits for the next switch to away.

import "freedo/tado"

// restateAwayZones writes every zone whose away temperature moved between two
// installations, once the home is away. At home it writes nothing.
func (d *daemon) restateAwayZones(e *tado.EmuState, before, after *tado.Install) {
	if !d.away {
		return
	}
	var zones []int
	for i := range after.Zones {
		zone := after.Zones[i].Number
		was, _ := before.Options.AwayTemperature(zone)
		now, _ := after.Options.AwayTemperature(zone)
		if was != now {
			zones = append(zones, zone)
		}
	}
	if len(zones) == 0 {
		return
	}
	d.applyHomeState(e)
	d.rewriteZones(e, zones, d.requestQueuedDispatch, true)
}
