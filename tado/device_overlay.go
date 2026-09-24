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
// and 6260, no 6200 and no end rule.
type OverlayReport struct {
	Zone   int
	Device string // wire EUI hex of the reporter
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
	}
	delete(e.zoneWrites, zone)
	e.overlayReports = append(e.overlayReports, OverlayReport{Zone: zone, Device: hexEUI(dev)})
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
