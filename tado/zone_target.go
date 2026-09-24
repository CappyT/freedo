package tado

import (
	"sort"
	"time"
)

// ZoneBlock is the z/s block the coordinator holds for one zone: the last one a
// member reported on the zone plane, or took from a coordinator write.
type ZoneBlock struct {
	Overlay byte // 6240: 0 auto, 2 manual, 3 until_next
	// Enabled is 6260 and SetpointC 6280, nil when the block omits the tag.
	Enabled    *bool
	SetpointC  *float64
	OpenWindow bool // 62e0
}

// ZoneBlock returns the block zone holds, and false when the coordinator holds
// no overlay (6240) for it.
func (e *EmuState) ZoneBlock(zone int) (ZoneBlock, bool) {
	var out ZoneBlock
	known := false
	for _, tlv := range parseTLVList(e.zoneState[zoneKey(zone, "z/s")]) {
		switch {
		case tlv.Tag == "6240" && len(tlv.Val) == 1:
			out.Overlay, known = tlv.Val[0], true
		case tlv.Tag == "6260" && len(tlv.Val) == 1:
			on := tlv.Val[0] == 0x01
			out.Enabled = &on
		case tlv.Tag == "6280" && len(tlv.Val) == 2:
			c := float64(be(tlv.Val)) / 100
			out.SetpointC = &c
		case tlv.Tag == "62e0" && len(tlv.Val) == 1:
			out.OpenWindow = tlv.Val[0] == 0x01
		}
	}
	return out, known
}

// OverlayTimer is the coordinator-side deadline of the overlay a zone runs.
type OverlayTimer struct {
	Target    string // wire EUI hex of the node the timer restores
	ExpiresAt time.Time
	Boost     bool
}

// ZoneOverlayTimer returns the deadline of the overlay zone runs: the latest
// one when several members hold a timer for it.
func (e *EmuState) ZoneOverlayTimer(zone int) (OverlayTimer, bool) {
	keys := make([]string, 0, len(e.boostTimers))
	for key, timer := range e.boostTimers {
		if timer.id == zone {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return OverlayTimer{}, false
	}
	sort.Strings(keys)
	var out OverlayTimer
	for _, key := range keys {
		timer := e.boostTimers[key]
		if out.Target == "" || timer.expiresAt.After(out.ExpiresAt) {
			out = OverlayTimer{Target: overlayTimerNode(key), ExpiresAt: timer.expiresAt, Boost: timer.boost}
		}
	}
	return out, true
}

// restoredTimer rebuilds the in-memory flags of a stored timer. The store keeps
// the zone and the deadline only: the zone kind says whether the restore is the
// hot-water block, and a room overlay at the boost temperature is the boost,
// since the air carries the same block for both.
func (e *EmuState) restoredTimer(t DesiredBoostTimer) boostTimer {
	out := boostTimer{id: t.ID, expiresAt: t.ExpiresAt, dhw: e.zoneKind(t.ID) == dhwZoneKind}
	if block, ok := e.ZoneBlock(t.ID); ok && !out.dhw && block.Overlay == 3 &&
		block.SetpointC != nil && *block.SetpointC == BoostTemperatureC {
		out.boost = true
	}
	return out
}
