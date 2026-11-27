package tado

import (
	"io"
	"strings"
	"testing"
	"time"
)

// Stale z/s reports in zone 5 of the fixture. The owner is VA1000000006 (0a01)
// and the member is VA1000000001 (0a06). The coordinator write set manual 25.00
// and replaced manual 24.60.

func echoBlock(overlay byte, centi uint16) []byte {
	return concat(
		TLV{Tag: "6240", Val: []byte{overlay}}.Encode(),
		TLV{Tag: "6260", Val: []byte{0x01}}.Encode(),
		TLV{Tag: "6280", Val: []byte{byte(centi >> 8), byte(centi)}}.Encode(),
	)
}

type echoRig struct {
	t             *testing.T
	e             *EmuState
	now           time.Time
	member, owner []byte
}

func newEchoRig(t *testing.T, edit func(*EmuConfig)) *echoRig {
	t.Helper()
	r := &echoRig{t: t, now: time.Date(2026, 1, 5, 6, 0, 0, 0, time.UTC),
		member: mustHex(kitchenAEUIHex), owner: mustHex(kitchenBEUIHex)}
	cfg := fixtureInstall(t).EmuConfig()
	cfg.Now = func() time.Time { return r.now }
	if edit != nil {
		edit(&cfg)
	}
	r.e = NewEmu(cfg, NewUnhandledLogger(io.Discard))
	return r
}

// write seeds the delivered write: zone 5 holds 25.00 and 24.60 was replaced now.
func (r *echoRig) write() {
	r.e.zoneState[zoneKey(5, "z/s")] = echoBlock(0x02, 2500)
	r.e.zoneWrites[5] = deliveredWrite{
		written:  heatingTagsOf(parseTLVList(echoBlock(0x02, 2500))),
		replaced: []replacedBlock{{at: r.now, block: heatingTagsOf(parseTLVList(echoBlock(0x02, 2460)))}},
	}
}

func (r *echoRig) setpoint() float64 {
	b, _ := r.e.ZoneBlock(5)
	if b.SetpointC == nil {
		return 0
	}
	return *b.SetpointC
}

// report sends a z/s?lid=5 report from src to dst (the coordinator or a peer).
func (r *echoRig) report(src, dst []byte, mid uint16, block []byte) []Action {
	r.t.Helper()
	return r.e.OnFrame(zoneUplink(r.t, src, dst, CoAPReq{
		Type: "CON", Code: "0.03", MID: mid, Token: []byte{byte(mid)},
		Path: []string{"z", "s"}, Query: []string{"lid=5"}, Payload: block,
	}))
}

func (r *echoRig) answered(actions []Action, mid uint16) bool {
	for _, a := range actions {
		if a.Kind == ActData && a.AnswerCounts && a.AnswerMID == mid {
			return true
		}
	}
	return false
}

// ack answers the downlink q with 2.04.
func (r *echoRig) ack(dev []byte, d *queuedDownlink) {
	r.t.Helper()
	pkt, err := BuildCoAPAck("2.04", d.mid, d.responseToken, nil, dev, BridgeEUI, coapPort, coapPort)
	if err != nil {
		r.t.Fatal(err)
	}
	f, err := BuildData(NetworkKey, byte(d.mid), BridgeEUI, dev[:5], WrapAppPayload(pkt, uint32(d.mid), nil))
	if err != nil {
		r.t.Fatal(err)
	}
	r.e.OnFrame(f.Bytes())
}

func (r *echoRig) wantRewrite(who []byte, centi string) {
	r.t.Helper()
	q := r.e.queue[hexEUI(who)]
	if len(q) != 1 || !q[0].staleRewrite || !strings.Contains(q[0].desc, "6280="+centi) {
		r.t.Fatalf("queue of %x = %+v, want the one rewrite of 6280=%s", who, r.e.Queue(hexEUI(who)), centi)
	}
}

// TestMemberThatConfirmedNewestBlockChangesZone: the member reported the written
// block, then a person turned its wheel back to the block the write replaced.
// The report is a real change: it reaches the owner and the zone takes it.
func TestMemberThatConfirmedNewestBlockChangesZone(t *testing.T) {
	r := newEchoRig(t, nil)
	r.write()
	r.report(r.member, r.owner, 0x7101, echoBlock(0x02, 2500))
	r.now = r.now.Add(3 * time.Minute)

	r.report(r.member, r.owner, 0x7102, echoBlock(0x02, 2460))
	if got := r.e.Stats().ZoneRelays; got != 2 {
		t.Errorf("relays = %d, want both reports relayed", got)
	}
	if got := r.setpoint(); got != 24.6 {
		t.Errorf("zone setpoint = %.2f, want the member's 24.60", got)
	}
	if got := r.e.Pending(kitchenAEUIHex); got != 0 {
		t.Errorf("the member holds %d queued writes, want none", got)
	}
}

// TestMemberThatDidNotConfirmNewestBlockIsStale: the member still holds the
// replaced block. Its report is not taken and not relayed. We answer it and
// queue the newest block to it. A report at 9 min is past the window.
func TestMemberThatDidNotConfirmNewestBlockIsStale(t *testing.T) {
	r := newEchoRig(t, nil)
	r.write()
	r.e.holdBlock(r.member, 5, heatingTagsOf(parseTLVList(echoBlock(0x02, 2460))))

	r.now = r.now.Add(7 * time.Minute)
	actions := r.report(r.member, r.owner, 0x7201, echoBlock(0x02, 2460))
	if got := r.e.Stats().ZoneRelays; got != 0 {
		t.Errorf("a stale report was relayed %d times, want none", got)
	}
	if !r.answered(actions, 0x7201) {
		t.Errorf("a stale report got no answer in %d actions", len(actions))
	}
	if got := r.setpoint(); got != 25 {
		t.Errorf("zone setpoint = %.2f, want 25.00", got)
	}
	r.wantRewrite(r.member, "25.00")
	if got := r.e.Pending(kitchenBEUIHex); got != 0 {
		t.Errorf("the owner holds %d queued frames, want none", got)
	}

	r.now = r.now.Add(2 * time.Minute)
	r.report(r.member, r.owner, 0x7202, echoBlock(0x02, 2460))
	if got := r.setpoint(); got != 24.6 {
		t.Errorf("zone setpoint = %.2f after the window, want the member's 24.60", got)
	}
}

// TestMemberThatHoldsNothingIsStale: after a restart of the daemon the record of
// the write is gone, so the report is relayed. With a record and no held block
// the member counts as not confirmed.
func TestMemberThatHoldsNothingIsStale(t *testing.T) {
	r := newEchoRig(t, nil)
	r.write()
	r.report(r.member, r.owner, 0x7301, echoBlock(0x02, 2460))
	if got := r.e.Stats().ZoneRelays; got != 0 {
		t.Errorf("relays = %d, want none", got)
	}
}

// TestStaleVerdictKeepsForRetransmission: a copy of the same source and MID keeps
// the verdict of the first copy, also after the member confirmed the newest
// block in between and 250 s later.
func TestStaleVerdictKeepsForRetransmission(t *testing.T) {
	r := newEchoRig(t, nil)
	r.write()
	m := &CoAPMessage{
		Code: "0.03", Path: "z/s", Query: "lid=5", MID: 0x7401,
		Payload: echoBlock(0x02, 2460),
	}
	if !r.e.staleZoneEcho(m, r.member).member {
		t.Fatal("the first copy is not stale")
	}
	r.e.holdBlock(r.member, 5, heatingTagsOf(parseTLVList(echoBlock(0x02, 2500))))
	r.now = r.now.Add(250 * time.Second)
	if !r.e.staleZoneEcho(m, r.member).member {
		t.Error("the copy at 250 s lost the verdict of the first copy")
	}
	other := *m
	other.MID = 0x7402
	if r.e.staleZoneEcho(&other, r.member).stale {
		t.Error("a new MID of a member that confirmed the newest block is stale")
	}
}

// TestStaleZoneEchoAfterRestartIsRelayed: no record of a write, so the report is
// a change and goes to the owner as before.
func TestStaleZoneEchoAfterRestartIsRelayed(t *testing.T) {
	r := newEchoRig(t, nil)
	r.report(r.member, r.owner, 0x7501, echoBlock(0x02, 2460))
	if got := r.e.Stats().ZoneRelays; got != 1 {
		t.Errorf("relays = %d, want the report relayed", got)
	}
	if got := r.e.Pending(kitchenAEUIHex); got != 0 {
		t.Errorf("the member holds %d queued writes, want none", got)
	}
}

// TestOwnerReportOfReplacedBlockIsNotRewritten: the owner pass encoded the old
// body before the write and sends it under a new MID to each endpoint. The zone
// does not cache it, the report is relayed as before and the owner gets no
// rewrite: it already holds the newest block.
func TestOwnerReportOfReplacedBlockIsNotRewritten(t *testing.T) {
	r := newEchoRig(t, nil)
	r.write()
	peers := [][]byte{r.member, mustHex("08a4defa07c51b00"), mustHex("05a2cafa07c51b00")}
	for i, peer := range peers {
		r.now = r.now.Add(20 * time.Second)
		r.report(r.owner, peer, 0x7600+uint16(i), echoBlock(0x02, 2460))
	}
	if got := r.e.Stats().ZoneRelays; got != 3 {
		t.Errorf("relays = %d, want the 3 reports relayed", got)
	}
	if got := r.e.Pending(kitchenBEUIHex); got != 0 {
		t.Errorf("the owner holds %d queued writes, want none", got)
	}
	if got := r.setpoint(); got != 25 {
		t.Errorf("zone setpoint = %.2f, want 25.00", got)
	}
	r.now = r.now.Add(8 * time.Minute)
	r.report(r.owner, r.member, 0x7610, echoBlock(0x02, 2460))
	if got := r.setpoint(); got != 24.6 {
		t.Errorf("zone setpoint = %.2f after the window, want the owner's 24.60", got)
	}
}

// TestStaleRewriteDedupsWithPendingWrite: a second stale report queues no second
// rewrite while the first waits, and none while another write of the zone block
// waits for the member.
func TestStaleRewriteDedupsWithPendingWrite(t *testing.T) {
	r := newEchoRig(t, nil)
	r.write()
	r.report(r.member, r.owner, 0x7701, echoBlock(0x02, 2460))
	r.wantRewrite(r.member, "25.00")
	r.report(r.member, r.owner, 0x7702, echoBlock(0x02, 2460))
	r.wantRewrite(r.member, "25.00")
	if got := r.e.Stats().ZoneRelays; got != 0 {
		t.Errorf("relays = %d, want none", got)
	}
}

// TestStaleEchoWithNoAutoQueueQueuesNothing: with NoAutoQueue on, a stale report
// is not taken and not relayed, and no rewrite is queued.
func TestStaleEchoWithNoAutoQueueQueuesNothing(t *testing.T) {
	r := newEchoRig(t, func(c *EmuConfig) { c.NoAutoQueue = true })
	r.write()
	r.report(r.member, r.owner, 0x7801, echoBlock(0x02, 2460))
	if got := r.e.Pending(kitchenAEUIHex); got != 0 {
		t.Errorf("the member holds %d queued frames, want none", got)
	}
	if got := r.e.Stats().ZoneRelays; got != 0 {
		t.Errorf("relays = %d, want none", got)
	}
	if got := r.setpoint(); got != 25 {
		t.Errorf("zone setpoint = %.2f, want 25.00", got)
	}
}

// TestHeldBlockSources: the three events that make a member hold a block.
func TestHeldBlockSources(t *testing.T) {
	r := newEchoRig(t, nil)
	r.write()
	want := heatingTagsOf(parseTLVList(echoBlock(0x02, 2500)))
	if r.e.heldNewest(r.member, 5) {
		t.Fatal("a member that holds nothing holds the newest block")
	}

	// A report of the member.
	r.report(r.member, r.owner, 0x7901, echoBlock(0x02, 2500))
	if !r.e.heldNewest(r.member, 5) {
		t.Error("a report of the newest block does not make the member hold it")
	}
	r.report(r.member, r.owner, 0x7902, echoBlock(0x02, 2400))
	if r.e.heldNewest(r.member, 5) {
		t.Error("a later report of another block leaves the held block unchanged")
	}

	// A relayed PUT z/s that the member answers with 2.04. The report of another
	// block above made the zone adopt it, so the write is seeded again.
	r.write()
	r.e.OnFrame(zoneUplink(t, r.owner, r.member, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7903, Token: []byte{0x03},
		Path: []string{"z", "s"}, Query: []string{"lid=5"}, Payload: echoBlock(0x02, 2500),
	}))
	hop := r.e.queue[kitchenAEUIHex][0]
	hop.inFlight = true
	r.e.OnFrame(zoneUplink(t, r.member, r.owner, CoAPReq{
		Type: "ACK", Code: "2.04", MID: 0x7903, Token: []byte{0x03},
	}))
	if !r.e.heldNewest(r.member, 5) {
		t.Errorf("a relayed push answered with 2.04 does not make the member hold it (held %v, want %v)",
			r.e.heldBlocks[heldKey(r.member, 5)], want)
	}

	// A delivered write.
	r.report(r.member, r.owner, 0x7904, echoBlock(0x02, 2400))
	r.write()
	r.e.promoteHeatingBlock(r.member, &heatingBlock{zone: 5, body: echoBlock(0x02, 2500)})
	if !r.e.heldNewest(r.member, 5) {
		t.Error("a delivered write does not make the member hold its block")
	}
}

// TestStaleRewriteInFlightYieldsToDeviceOverlay: the rewrite of a stale report is
// on the air when the owner's wheel sets an overlay, or ends one. The old
// rewrite leaves the queue, so its ACK writes nothing, and the member takes a
// rewrite of the overlay.
func TestStaleRewriteInFlightYieldsToDeviceOverlay(t *testing.T) {
	in := fixtureInstall(t)
	for _, tc := range []struct {
		name  string
		step  func(r *echoRig)
		setpC float64
		rest  string
	}{
		{"overlay", func(r *echoRig) {
			r.e.OnFrame(synthDeviceUplink(t, in, r.owner, CoAPReq{
				Type: "CON", Code: "0.03", MID: 0x7a02, Token: []byte{0x33},
				Path: homePath(in, "z", "5", "overlay"), Options: []CoAPOption{optOctets},
				Payload: echoBlock(0x02, 2200),
			}))
		}, 22, "6280=22.00"},
		{"end", func(r *echoRig) {
			r.e.OnFrame(synthDeviceUplink(t, in, r.owner, CoAPReq{
				Type: "CON", Code: "0.04", MID: 0x7a03, Token: []byte{0x34},
				Path: homePath(in, "z", "5", "overlay"), Options: []CoAPOption{optOctets},
			}))
		}, 0, "6240=00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newEchoRig(t, nil)
			r.write()
			r.report(r.member, BridgeEUI, 0x7a01, echoBlock(0x02, 2460))
			old := r.e.queue[kitchenAEUIHex]
			if len(old) != 1 || !old[0].staleRewrite {
				t.Fatalf("queue = %+v, want the stale rewrite", r.e.Queue(kitchenAEUIHex))
			}
			if !old[0].inFlight {
				t.Fatal("the rewrite is not on the air")
			}

			r.now = r.now.Add(2 * time.Second)
			tc.step(r)
			q := r.e.queue[kitchenAEUIHex]
			if len(q) != 1 || q[0] == old[0] || q[0].staleRewrite || !strings.Contains(q[0].desc, tc.rest) {
				t.Fatalf("queue = %+v, want one rewrite of the overlay (%s)", r.e.Queue(kitchenAEUIHex), tc.rest)
			}
			r.ack(r.member, old[0])
			if tc.setpC != 0 {
				if got := r.setpoint(); got != tc.setpC {
					t.Errorf("zone setpoint = %.2f after the old ACK, want %.2f", got, tc.setpC)
				}
			}
			if r.e.heldOverlays[5].written == nil && tc.name == "overlay" {
				t.Error("the ACK of the old rewrite dropped the held device overlay")
			}
		})
	}
}
