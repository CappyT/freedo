package tado

import (
	"bytes"
	"fmt"
	"maps"
	"strings"
	"time"
)

// staleEchoWindow bounds how long after a delivered write a member report of
// the block that write replaced counts as a late echo. On air the valve echoes
// z/s?lid=N 6 to 14 s after it takes a write.
const staleEchoWindow = time.Minute

// zoneHeatingTags are the tags of the heating block a coordinator write sets and
// a member reports back.
var zoneHeatingTags = []string{"6240", "6260", "6280"}

// deliveredWrite is the last delivered write that changed the heating block of
// a zone, and the block it replaced.
type deliveredWrite struct {
	at       time.Time
	written  map[string][]byte
	replaced map[string][]byte
}

// ZoneStateReport is one z/s?lid=N report of a zone member. Stale marks a late
// echo of a block a newer write replaced: the zone did not take it.
type ZoneStateReport struct {
	Zone   int
	Device string // short address of the reporter
	Body   []byte
	Stale  bool
}

func heatingTagsOf(list []TLV) map[string][]byte {
	out := map[string][]byte{}
	for _, tlv := range list {
		for _, tag := range zoneHeatingTags {
			if tlv.Tag == tag {
				out[tag] = tlv.Val
			}
		}
	}
	return out
}

func (e *EmuState) zoneHeating(zone int) map[string][]byte {
	return heatingTagsOf(parseTLVList(e.zoneState[zoneKey(zone, "z/s")]))
}

// noteZoneWrite records a delivered write to zone when it changed the heating
// block from before. A write that restates the block keeps the older record,
// so the block that record replaced stays known as stale.
func (e *EmuState) noteZoneWrite(zone int, before map[string][]byte) {
	after := e.zoneHeating(zone)
	if maps.EqualFunc(before, after, bytes.Equal) {
		return
	}
	e.zoneWrites[zone] = deliveredWrite{at: e.cfg.Now(), written: after, replaced: before}
}

// staleZoneEcho reports whether m is a member report of the heating block the
// last delivered write to its zone replaced. The valve echoes each write it
// takes, and the echo of an older write can arrive after a newer one: taking
// it would undo the newer write. A wheel change sends its overlay PUT first,
// which drops the record.
func (e *EmuState) staleZoneEcho(m *CoAPMessage) bool {
	zone, ok := zoneQuery(m.Query)
	if !ok || m.Code != "0.03" || normalizeResource(m.Path) != "z/s" {
		return false
	}
	w, ok := e.zoneWrites[zone]
	if !ok || e.cfg.Now().Sub(w.at) > staleEchoWindow {
		return false
	}
	report := heatingTagsOf(parseTLVList(m.Payload))
	differs := false
	for tag, v := range report {
		if !bytes.Equal(v, w.replaced[tag]) {
			return false
		}
		differs = differs || !bytes.Equal(v, w.written[tag])
	}
	return differs
}

// adoptZoneReport drops the write record of zone when the member reports a
// block other than the written one: the device changed the zone itself.
func (e *EmuState) adoptZoneReport(zone int, updates []TLV) {
	w, ok := e.zoneWrites[zone]
	if !ok {
		return
	}
	for tag, v := range heatingTagsOf(updates) {
		if !bytes.Equal(v, w.written[tag]) {
			delete(e.zoneWrites, zone)
			return
		}
	}
}

func (e *EmuState) noteZoneStateReport(zone int, src []byte, body []byte, stale bool) {
	e.zoneStateReports = append(e.zoneStateReports, ZoneStateReport{
		Zone: zone, Device: ShortFromWire(src), Body: append([]byte(nil), body...), Stale: stale,
	})
}

// TakeZoneStateReports returns the z/s member reports received since the last
// call.
func (e *EmuState) TakeZoneStateReports() []ZoneStateReport {
	out := e.zoneStateReports
	e.zoneStateReports = nil
	return out
}

// ZoneStateSummary renders the zone state tags of a z/s body on one line, the
// temperatures in C.
func ZoneStateSummary(body []byte) string {
	var parts []string
	for _, tlv := range parseTLVList(body) {
		switch {
		case (tlv.Tag == "6200" || tlv.Tag == "6280") && len(tlv.Val) == 2:
			parts = append(parts, fmt.Sprintf("%s=%.2f", tlv.Tag, float64(be(tlv.Val))/100))
		case tlv.Tag == "61e0", tlv.Tag == "6240", tlv.Tag == "6260", tlv.Tag == "62e0", tlv.Tag == "6440":
			parts = append(parts, fmt.Sprintf("%s=%x", tlv.Tag, tlv.Val))
		}
	}
	return strings.Join(parts, " ")
}

// pendingHeating returns the newest write queued to wire that sets the heating
// block of zone, or nil.
func (e *EmuState) pendingHeating(wire []byte, zone int) *heatingBlock {
	q := e.queue[hexEUI(wire)]
	for i := len(q) - 1; i >= 0; i-- {
		if b := q[i].heating; b != nil && b.zone == zone && !b.windowOnly {
			return b
		}
	}
	return nil
}

// restatePending makes a rewrite keep the block of a write queued ahead of it,
// which the node has not taken yet: the node's last block is older than it.
// The off overlay comes from the pending block alone: the cache still holds
// the off overlay a queued return to schedule ends.
func restatePending(c *SetpointCoAP, b *heatingBlock) {
	if b.body == nil {
		c.Setpoint, c.Overlay, c.Off = float64(be(b.setpoint))/100, b.overlay, false
		return
	}
	off := false
	for _, tlv := range parseTLVList(b.body) {
		switch {
		case tlv.Tag == "6240" && len(tlv.Val) == 1:
			c.Overlay = tlv.Val[0]
		case tlv.Tag == "6260" && len(tlv.Val) == 1:
			off = tlv.Val[0] == 0x00
		case tlv.Tag == "6280" && len(tlv.Val) == 2:
			c.Setpoint = float64(be(tlv.Val)) / 100
		}
	}
	c.Off = off && c.Overlay != overlayAuto
}
