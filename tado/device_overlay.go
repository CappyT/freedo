package tado

import (
	"maps"
	"slices"
	"strconv"
	"time"
)

// The end rules of an overlay set on a device. The names are the modes of the
// zone overlay API.
const (
	OverlayEndManual    = "manual"
	OverlayEndUntilNext = "until_next"
	OverlayEndTimer     = "timer"
)

// OverlayMaxDuration bounds a timer overlay.
const OverlayMaxDuration = 7 * 24 * time.Hour

// OverlayEnd is the end rule the coordinator gives an overlay set on a device
// of a zone. The rule does not travel on the air: the coordinator keeps it and
// writes the return to schedule when the overlay ends.
type OverlayEnd struct {
	Mode        string `yaml:"mode" json:"mode"`
	DurationSec int    `yaml:"duration_sec,omitempty" json:"duration_sec,omitempty"`
}

// DefaultDeviceOverlayEnd is the end rule of a zone that states none.
var DefaultDeviceOverlayEnd = OverlayEnd{Mode: OverlayEndUntilNext}

// DeviceOverlayEnd returns the end rule of an overlay set on a device of zone.
func (o Options) DeviceOverlayEnd(zone int) OverlayEnd {
	if end, ok := o.DeviceOverlayEnds[zone]; ok {
		return end
	}
	return DefaultDeviceOverlayEnd
}

// Check reports what is wrong with the rule, or "".
func (r OverlayEnd) Check() string {
	switch r.Mode {
	case OverlayEndManual, OverlayEndUntilNext:
		if r.DurationSec != 0 {
			return "duration_sec needs mode timer"
		}
	case OverlayEndTimer:
		if r.DurationSec <= 0 || time.Duration(r.DurationSec)*time.Second > OverlayMaxDuration {
			return "mode timer needs a duration_sec within 1.." + strconv.Itoa(int(OverlayMaxDuration.Seconds()))
		}
	default:
		return "bad mode " + strconv.Quote(r.Mode) + ": want manual, until_next or timer"
	}
	return ""
}

func (in *Install) checkDeviceOverlayEnds(c *issues) {
	for _, zone := range slices.Sorted(maps.Keys(in.Options.DeviceOverlayEnds)) {
		path := "options.device_overlay_end." + strconv.Itoa(zone)
		switch z, err := in.ZoneByNumber(zone); {
		case err != nil:
			c.errf(path, "no zone %d is declared", zone)
		case z.Kind != ZoneHeating:
			c.errf(path, "zone %d is not a heating zone", zone)
		}
		if msg := in.Options.DeviceOverlayEnds[zone].Check(); msg != "" {
			c.errf(path, "%s", msg)
		}
	}
}

// OverlayReport is one PUT h/<home>/z/<Zone>/overlay from a device: a manual
// change made on it. The captured body carries 6280 (the new setpoint), 6240
// and 6260, no 6200 and no end rule. End marks a DELETE of the same path: the
// elected reporter returned the zone to its schedule (6240 = 0, valve 0x11d1c,
// thermostat 0x11a94).
type OverlayReport struct {
	Zone   int
	Device string // wire EUI hex of the reporter
	End    bool
}

// takeDeviceOverlay records the report and merges the overlay into the zone
// block, so the zone state shows it before the member echoes z/s?lid=N.
func (e *EmuState) takeDeviceOverlay(zone int, dev []byte, body []byte) {
	var updates []TLV
	for _, tlv := range parseTLVList(body) {
		switch {
		case tlv.Tag == "6240" && len(tlv.Val) == 1, tlv.Tag == "6260" && len(tlv.Val) == 1,
			tlv.Tag == "6280" && len(tlv.Val) == 2:
			updates = append(updates, tlv)
		}
	}
	if len(updates) > 0 {
		key := zoneKey(zone, "z/s")
		e.storeZoneState(zone, "z/s", encodeTLVs(mergeConfigTLVs(parseTLVList(e.zoneState[key]), updates)))
		e.restateDeviceOverlay(zone)
	}
	delete(e.zoneWrites, zone)
	e.overlayReports = append(e.overlayReports, OverlayReport{Zone: zone, Device: hexEUI(dev)})
}

// endDeviceOverlay takes the DELETE of h/<home>/z/<Zone>/overlay: the device
// returned the zone to its schedule and the request carries no body. The zone
// block and the reporter's own block go to overlay auto, so a later rewrite
// restates the schedule, and the queued writes that restate the old overlay
// give way to a rewrite. The owner ends the coordinator-side deadline.
func (e *EmuState) endDeviceOverlay(zone int, dev []byte) {
	auto := []TLV{{Tag: "6240", Val: []byte{overlayAuto}}}
	key := zoneKey(zone, "z/s")
	e.storeZoneState(zone, "z/s", encodeTLVs(mergeConfigTLVs(parseTLVList(e.zoneState[key]), auto)))
	if ns, ok := e.nodes[hexEUI(dev)]; ok && ns.holdsZone(zone) && len(ns.raw["6240"]) == 1 {
		e.cacheTLVs(ns, auto, "", ClassData, e.name(dev), "self", "")
	}
	e.restateDeviceOverlay(zone)
	delete(e.zoneWrites, zone)
	e.overlayReports = append(e.overlayReports, OverlayReport{Zone: zone, Device: hexEUI(dev), End: true})
}

// restateDeviceOverlay rebuilds the zone writes queued before a device overlay
// on a heating zone: each one restates the block the zone held before, so it
// would undo the overlay on delivery. A write on the air stays, and so does
// one that marks an event (6440 or a zone configuration tag). Each node that
// lost a write takes one rewrite of the overlay, with the head and the 62e0 of
// now.
func (e *EmuState) restateDeviceOverlay(zone int) {
	if e.zoneKind(zone) == dhwZoneKind {
		return
	}
	for _, key := range slices.Sorted(maps.Keys(e.queue)) {
		q := e.queue[key]
		kept := make([]*queuedDownlink, 0, len(q))
		for _, d := range q {
			if d.zoneBlock && d.zone == zone && !d.inFlight && len(zoneWriteEvents(d.coap)) == 0 {
				e.recordDownlinkOutcome(key, d, "superseded", "device-overlay")
				continue
			}
			kept = append(kept, d)
		}
		if len(kept) == len(q) {
			continue
		}
		e.queue[key] = kept
		if err := e.enqueueZoneRewrite(key, zone, true); err != nil {
			e.log.Log("Data", "self", key, rsnBuildFail+" device overlay rewrite "+err.Error(), "")
		}
	}
}

// TakeOverlayReports returns the device overlay reports received since the
// last call.
func (e *EmuState) TakeOverlayReports() []OverlayReport {
	out := e.overlayReports
	e.overlayReports = nil
	return out
}

// SetZoneOverlayTimer replaces every coordinator-side deadline of the overlay
// zone runs with one of d on each target. A zero d leaves the overlay with no
// deadline. The expiry takes the ExpireBoosts path: overlay auto, no 6440.
func (e *EmuState) SetZoneOverlayTimer(zone int, targets []string, d time.Duration) error {
	wires := make([][]byte, 0, len(targets))
	for _, target := range targets {
		wire, _, err := e.resolve(target)
		if err != nil {
			return err
		}
		wires = append(wires, wire)
	}
	for key, timer := range e.boostTimers {
		if timer.id == zone {
			delete(e.boostTimers, key)
		}
	}
	for _, wire := range wires {
		e.armOverlayTimer(wire, zone, d, e.zoneKind(zone) == dhwZoneKind, false)
	}
	return nil
}
