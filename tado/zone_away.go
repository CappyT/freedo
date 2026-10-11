package tado

import (
	"bytes"
	"maps"
	"math"
	"slices"
	"strconv"
)

// The room setpoint range of the tado app. The captured writes span 5.00 C
// (away) to 25.00 C (boost).
const (
	RoomMinC = 5.0
	RoomMaxC = 25.0
)

// AwayTemperature returns the temperature zone keeps in away, and false when the
// zone is off in away: no value, or the room minimum. The cloud writes that
// default as 61e0=0 with no 6200 (capture-away-20261011); a higher value goes
// out as 61e0=1 and 6200=<value>.
func (o Options) AwayTemperature(zone int) (float64, bool) {
	if c := o.AwayTemperatures[zone]; c > RoomMinC {
		return c, true
	}
	return 0, false
}

func (in *Install) checkAwayTemperatures(c *issues) {
	for _, zone := range slices.Sorted(maps.Keys(in.Options.AwayTemperatures)) {
		path := "options.away_temperature_c." + strconv.Itoa(zone)
		switch z, err := in.ZoneByNumber(zone); {
		case err != nil:
			c.errf(path, "no zone %d is declared", zone)
		case z.Kind != ZoneHeating:
			c.errf(path, "zone %d is not a heating zone", zone)
		}
		if t := in.Options.AwayTemperatures[zone]; math.IsNaN(t) || t < RoomMinC || t > RoomMaxC {
			c.errf(path, "%g is out of range %g..%g C", t, RoomMinC, RoomMaxC)
		}
	}
}

// headTarget is the target a zone head sets while the zone runs no overlay: off,
// or on at the 6200 temperature, as 61e0 and 6200 say. 61e0 decides the power;
// the temperature of an off head is stale and never compared.
type headTarget struct {
	known bool   // the head carries 61e0
	on    bool   // 61e0 is 1
	sched []byte // 6200 as on the wire, nil when the head leaves it out
}

func targetOf(head []TLV) headTarget {
	var t headTarget
	for _, tlv := range head {
		switch {
		case tlv.Tag == "61e0" && len(tlv.Val) == 1:
			t.known, t.on = true, tlv.Val[0] != 0
		case tlv.Tag == "6200":
			t.sched = tlv.Val
		}
	}
	return t
}

// movesTo reports whether the write that sets next changes the target the zone
// had: its power, or its temperature on the wire scale. A target that cannot be
// compared counts as a change. A write that leaves 6200 out keeps the one the
// device holds.
func (t headTarget) movesTo(next headTarget) bool {
	switch {
	case !t.known || !next.known, t.on != next.on:
		return true
	case !next.on, next.sched == nil:
		return false
	}
	return !bytes.Equal(t.sched, next.sched)
}

// zoneHeadTarget is the target zone has on wire once the writes queued ahead are
// taken: the head of the newest one, else the head the zone cache holds.
func (e *EmuState) zoneHeadTarget(wire []byte, zone int) headTarget {
	if b := e.pendingHeating(wire, zone); b != nil {
		if b.body != nil {
			return targetOf(parseTLVList(b.body))
		}
		return targetOf(writtenHead(b.home, b.programOff, roomZoneKind, b.sched))
	}
	return targetOf(parseTLVList(e.zoneState[zoneKey(zone, "z/s")]))
}
