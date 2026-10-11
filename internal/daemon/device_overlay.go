package daemon

// device_overlay: a manual change made on a device. The device PUTs
// h/<home>/z/N/overlay and echoes the new block on z/s?lid=N. The end rule
// does not travel on the air: the coordinator applies the rule of the zone and
// writes the return to schedule when the overlay ends.

import (
	"context"
	"log"
	"time"

	"freedo/tado"
)

// takeOverlayReports gives every device overlay the end rule of its zone: the
// deadline goes on the zoneWriteTargets, and its expiry writes overlay auto
// with no 6440, as the captured expiry does. An end report drops the deadline
// of the zone: the device already follows its schedule.
func (d *daemon) takeOverlayReports(e *tado.EmuState) {
	reports := e.TakeOverlayReports()
	if len(reports) == 0 {
		return
	}
	inst := d.inst.Load()
	for _, r := range reports {
		if inst == nil {
			log.Printf("device overlay from %s: %v", r.Device, errNoInstall)
			continue
		}
		z, err := inst.ZoneByNumber(r.Zone)
		if err != nil {
			log.Printf("device overlay from %s: %v", r.Device, err)
			continue
		}
		if !d.zoneMember(r.Zone, r.Device) {
			log.Printf("device overlay from %s: not a member of zone %d", r.Device, r.Zone)
			continue
		}
		if r.End {
			if err := e.SetZoneOverlayTimer(r.Zone, nil, 0, ""); err != nil {
				log.Printf("device overlay end on zone %d: %v", r.Zone, err)
			}
			continue
		}
		if z.Kind != tado.ZoneHeating {
			log.Printf("device overlay from %s: zone %d is not a heating zone, no end rule applies", r.Device, r.Zone)
			continue
		}
		duration := d.deviceOverlayDuration(e, inst, r.Zone)
		rule := inst.Options.DeviceOverlayEnd(r.Zone).Mode
		if err := e.SetZoneOverlayTimer(r.Zone, d.zoneWriteTargets(e, r.Zone), duration, rule); err != nil {
			log.Printf("device overlay on zone %d: %v", r.Zone, err)
		}
	}
	d.saveEngineState(context.Background(), e, "device overlay")
}

// deviceOverlayDuration is how long a device overlay on zone lasts from now
// under the end rule of the zone. Zero means no deadline: a manual rule, or an
// until_next rule on a zone whose program the daemon does not hold.
func (d *daemon) deviceOverlayDuration(e *tado.EmuState, inst *tado.Install, zone int) time.Duration {
	rule := inst.Options.DeviceOverlayEnd(zone)
	switch rule.Mode {
	case tado.OverlayEndTimer:
		return time.Duration(rule.DurationSec) * time.Second
	case tado.OverlayEndUntilNext:
		next, ok := d.untilNext(e, zone)
		if !ok {
			log.Printf("device overlay on zone %d: no program holds a next block, the overlay stays until cancelled", zone)
		}
		return next
	}
	return 0
}
