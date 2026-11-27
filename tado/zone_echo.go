package tado

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
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
// a zone, and the blocks it and the writes before it replaced inside
// staleEchoWindow.
type deliveredWrite struct {
	written  map[string][]byte
	replaced []replacedBlock
}

// replacedBlock is a heating block a delivered write replaced, and when.
type replacedBlock struct {
	at    time.Time
	block map[string][]byte
}

// heldOverlay is a device overlay the zone took from a PUT
// h/<home>/z/<n>/overlay, and the heating block it replaced. The other members
// take it later through the zone relays and report the replaced block until
// then.
type heldOverlay struct {
	written  map[string][]byte
	replaced map[string][]byte
}

// ZoneStateReport is one z/s?lid=N report of a zone member. Stale marks a late
// echo of a block a newer write replaced, and Behind a report of the block a
// held device overlay replaced: the zone took the heating tags of neither.
type ZoneStateReport struct {
	Zone   int
	Device string // short address of the reporter
	Body   []byte
	Stale  bool
	Behind bool
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
// so the block that record replaced stays known as stale. The blocks earlier
// writes replaced stay stale until their own window ends: a member can echo the
// first of two quick writes after the second.
func (e *EmuState) noteZoneWrite(zone int, before map[string][]byte) {
	after := e.zoneHeating(zone)
	if maps.EqualFunc(before, after, bytes.Equal) {
		return
	}
	now := e.cfg.Now()
	var kept []replacedBlock
	for _, r := range e.zoneWrites[zone].replaced {
		if now.Sub(r.at) <= staleEchoWindow && !maps.EqualFunc(r.block, after, bytes.Equal) {
			kept = append(kept, r)
		}
	}
	kept = append(kept, replacedBlock{at: now, block: before})
	e.zoneWrites[zone] = deliveredWrite{written: after, replaced: kept}
	delete(e.heldOverlays, zone)
}

// staleZoneEcho reports whether m is a member report of a heating block that
// a delivered write to its zone replaced inside staleEchoWindow. The valve
// echoes each write it takes, and the echo of an older write can arrive after a
// newer one: taking it would undo the newer write. A wheel change sends its
// overlay PUT first, which drops the record.
func (e *EmuState) staleZoneEcho(m *CoAPMessage) bool {
	zone, ok := zoneQuery(m.Query)
	if !ok || m.Code != "0.03" || normalizeResource(m.Path) != "z/s" {
		return false
	}
	w, ok := e.zoneWrites[zone]
	if !ok {
		return false
	}
	now := e.cfg.Now()
	report := heatingTagsOf(parseTLVList(m.Payload))
	for _, r := range w.replaced {
		if now.Sub(r.at) <= staleEchoWindow && echoOf(report, r.block, w.written) {
			return true
		}
	}
	return false
}

// echoOf reports whether report shows the replaced block and not the written one.
func echoOf(report, replaced, written map[string][]byte) bool {
	differs := false
	for tag, v := range report {
		if !bytes.Equal(v, replaced[tag]) {
			return false
		}
		differs = differs || !bytes.Equal(v, written[tag])
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

// behindDeviceOverlay reports whether a member report shows the block that the
// held device overlay of its zone replaced, or the program: the member has not
// taken the overlay yet. Only the reporter's DELETE or a coordinator write ends
// a device overlay, so such a report must not undo it (issue #38). A report of
// another overlay is a new change, and the zone takes it.
func (e *EmuState) behindDeviceOverlay(zone int, updates []TLV) bool {
	h, ok := e.heldOverlays[zone]
	if !ok {
		return false
	}
	report := heatingTagsOf(updates)
	differs, replaced := false, true
	for tag, v := range report {
		differs = differs || !bytes.Equal(v, h.written[tag])
		replaced = replaced && bytes.Equal(v, h.replaced[tag])
	}
	auto := len(report["6240"]) == 1 && report["6240"][0] == overlayAuto
	return differs && (replaced || auto)
}

// withoutHeating returns updates without the heating block tags.
func withoutHeating(updates []TLV) []TLV {
	out := make([]TLV, 0, len(updates))
	for _, tlv := range updates {
		if !slices.Contains(zoneHeatingTags, tlv.Tag) {
			out = append(out, tlv)
		}
	}
	return out
}

func (e *EmuState) noteZoneStateReport(zone int, src []byte, body []byte, stale, behind bool) {
	e.zoneStateReports = append(e.zoneStateReports, ZoneStateReport{
		Zone: zone, Device: ShortFromWire(src), Body: append([]byte(nil), body...), Stale: stale, Behind: behind,
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
// the off overlay a queued return to schedule ends. The 62e0 comes from
// zoneOpenWindow, which reads the pending writes too.
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
