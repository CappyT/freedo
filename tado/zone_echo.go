package tado

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// staleEchoWindow bounds how long after a delivered write a report of the block
// that write replaced can be a late echo. On air the valve echoes z/s?lid=N 6 to
// 14 s after it takes a write. One MID is retransmitted up to 225 s after its
// first copy, a new MID can carry an old block up to 465 s after it, and a
// pending endpoint repeats its block every 8 minutes.
const staleEchoWindow = 8 * time.Minute

// staleVerdictKeep is how long the verdict of a report stays known by source
// and MID: a retransmission of a request keeps the verdict of its first copy.
// The last copy leaves 225 s after the first.
const staleVerdictKeep = 5 * time.Minute

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
	// Restated marks a stale report of a member that is not the zone owner: it
	// was not relayed, and the newest block is queued back to the reporter.
	Restated bool
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

// staleVerdict is the verdict on one z/s report. stale means the report shows a
// block that a delivered write replaced inside staleEchoWindow: the zone does
// not cache it. member means the reporter is a member that is not the zone
// owner and did not confirm the newest block: freedo does not relay the report
// either, answers it and restates the newest block to the reporter.
type staleVerdict struct {
	stale, member bool
	at            time.Time
}

// staleZoneEcho judges a z/s?lid=N report. The valve echoes each write it takes,
// and a report of an older block can arrive after a newer write: taking it would
// undo the newer write. A report from the zone owner, or from a zone that has no
// resolvable owner, is stale when it shows a replaced block. The owner pass can
// send an old body after the owner took the new one, and a wheel change on the
// owner sends the overlay PUT first, which drops the record. A report from
// another member is stale only when that member did not confirm the newest
// block (heldNewest): a member that did confirm it and then reports a replaced
// block changed the zone itself. A retransmission keeps the verdict of the first
// copy of its source and MID.
func (e *EmuState) staleZoneEcho(m *CoAPMessage, src []byte) staleVerdict {
	zone, ok := zoneQuery(m.Query)
	if !ok || m.Code != "0.03" || normalizeResource(m.Path) != "z/s" {
		return staleVerdict{}
	}
	now := e.cfg.Now()
	key := fmt.Sprintf("%s|%04x", hexEUI(src), m.MID)
	if v, ok := e.staleVerdicts[key]; ok && now.Sub(v.at) <= staleVerdictKeep {
		return v
	}
	v := staleVerdict{at: now, stale: e.replacedBlockReport(zone, m.Payload, now)}
	if v.stale {
		if !e.isZoneOwner(zone, src) {
			v.member = !e.heldNewest(src, zone)
			v.stale = v.member
		}
	}
	for k, old := range e.staleVerdicts {
		if now.Sub(old.at) > staleVerdictKeep {
			delete(e.staleVerdicts, k)
		}
	}
	e.staleVerdicts[key] = v
	return v
}

// isZoneOwner reports whether src owns zone for the stale rule: it sends the
// overlay PUT for a local change. In a zone the coordinator owns every device is
// a member, the measuring device too. A zone with no resolvable owner answers
// true, which keeps the rule of the owner.
func (e *EmuState) isZoneOwner(zone int, src []byte) bool {
	if e.inst != nil && e.inst.Options.CoordinatorOwns(zone) {
		return false
	}
	owner := e.ZoneOwner(zone)
	return owner == "" || owner == ShortFromWire(src)
}

// replacedBlockReport reports whether payload shows a heating block that a
// delivered write to zone replaced inside staleEchoWindow.
func (e *EmuState) replacedBlockReport(zone int, payload []byte, now time.Time) bool {
	w, ok := e.zoneWrites[zone]
	if !ok {
		return false
	}
	report := heatingTagsOf(parseTLVList(payload))
	for _, r := range w.replaced {
		if now.Sub(r.at) <= staleEchoWindow && echoOf(report, r.block, w.written) {
			return true
		}
	}
	return false
}

func heldKey(wire []byte, zone int) string { return fmt.Sprintf("%s|%d", hexEUI(wire), zone) }

// holdBlock records the heating block a member holds for zone: the body of the
// last PUT z/s the member answered with 2.04, or of its last z/s report,
// whichever is later. A body with no heating tag leaves the record as it is.
func (e *EmuState) holdBlock(wire []byte, zone int, block map[string][]byte) {
	if len(block) == 0 {
		return
	}
	e.heldBlocks[heldKey(wire, zone)] = maps.Clone(block)
}

// heldNewest reports whether the member holds the newest block that a delivered
// write set in zone. A member that holds nothing does not.
func (e *EmuState) heldNewest(wire []byte, zone int) bool {
	held := e.heldBlocks[heldKey(wire, zone)]
	if len(held) == 0 {
		return false
	}
	written := e.zoneWrites[zone].written
	for tag, v := range held {
		if !bytes.Equal(v, written[tag]) {
			return false
		}
	}
	return true
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

// restateAfterStaleEcho queues the newest block of the zone to the member that
// reported a stale block: that device holds an older block and repeats it until
// it takes a newer one. It is the ordinary zone rewrite, restated from the zone
// cache, which the stale report did not change. A write of the zone block that
// already waits for the member carries the newest block, so a repeated report
// queues no second write.
func (e *EmuState) restateAfterStaleEcho(m *CoAPMessage, src []byte, class FrameClass, srcLbl, dstLbl, hexFrame string) {
	if e.cfg.NoAutoQueue {
		return
	}
	zone, ok := zoneQuery(m.Query)
	if !ok || e.pendingZoneBlock(src, zone) {
		return
	}
	if err := e.enqueueZoneRewrite(hexEUI(src), zone, true, false); err != nil {
		e.log.Log(class.String(), srcLbl, dstLbl, rsnBuildFail+" stale echo rewrite "+err.Error(), hexFrame)
		return
	}
	q := e.queue[hexEUI(src)]
	q[len(q)-1].staleRewrite = true
}

// pendingZoneBlock reports whether a write of the zone block waits in the queue
// of wire, on the air or not.
func (e *EmuState) pendingZoneBlock(wire []byte, zone int) bool {
	return slices.ContainsFunc(e.queue[hexEUI(wire)], func(d *queuedDownlink) bool {
		return d.zoneBlock && d.zone == zone
	})
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

func (e *EmuState) noteZoneStateReport(zone int, src []byte, body []byte, v staleVerdict, behind bool) {
	e.zoneStateReports = append(e.zoneStateReports, ZoneStateReport{
		Zone: zone, Device: ShortFromWire(src), Body: append([]byte(nil), body...),
		Stale: v.stale, Behind: behind, Restated: v.member,
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
