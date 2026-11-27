package tado

import (
	"strings"
	"testing"
	"time"
)

// What makes a member hold the newest block on air, and what counts as a change
// of the zone for the stale rule. Zone 5 of the fixture: the owner is VA1000000006,
// the member is VA1000000001.

// deliverHop sends the head of the queue of dev as the radio does: nextDownlink,
// then the Enhanced ACK of the frame (macAck), then the CoAP 2.04 of the device to
// the originator of the relayed request (coapAck).
func deliverHop(t *testing.T, r *echoRig, dev, origin []byte, macAck, coapAck bool) {
	t.Helper()
	q := r.e.queue[hexEUI(dev)]
	if len(q) == 0 {
		t.Fatal("no hop queued")
	}
	hop := q[0]
	if _, ok := r.e.nextDownlink(dev, "member"); !ok {
		t.Fatal("nextDownlink sent nothing")
	}
	if macAck {
		r.e.OnFrame(BuildEnhAck(hop.seq, BridgeEUI, dev, 0x1234))
	}
	if coapAck {
		r.e.OnFrame(zoneUplink(t, dev, origin, CoAPReq{Type: "ACK", Code: "2.04", MID: hop.mid, Token: []byte{byte(hop.mid)}}))
	}
}

// TestRelayedPushHoldsBlockWhateverAckCloses: the owner took 25.00 and pushes it
// to the member through the freedo relay. The hop has no CoAP requirement, so the
// Enhanced ACK of the member closes it before its CoAP 2.04 arrives. A person
// then turns the member wheel back to the block the write replaced: the member
// held 25.00, so its report is a change.
func TestRelayedPushHoldsBlockWhateverAckCloses(t *testing.T) {
	for _, tc := range []struct {
		name           string
		macAck, coapOK bool
	}{
		{"coap ack only", false, true},
		{"mac ack only", true, false},
		{"mac ack then coap ack", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newEchoRig(t, nil)
			r.report(r.member, r.owner, 0x7000, echoBlock(0x02, 2460))
			r.write()
			r.e.OnFrame(zoneUplink(t, r.owner, r.member, CoAPReq{
				Type: "CON", Code: "0.03", MID: 0x5101, Token: []byte{0x01},
				Path: []string{"z", "s"}, Query: []string{"lid=5"}, Payload: echoBlock(0x02, 2500),
			}))
			deliverHop(t, r, r.member, r.owner, tc.macAck, tc.coapOK)
			if !r.e.heldNewest(r.member, 5) {
				t.Fatal("the member that took the relayed push does not hold the newest block")
			}
			r.now = r.now.Add(3 * time.Minute)
			r.report(r.member, r.owner, 0x7001, echoBlock(0x02, 2460))
			if got := r.setpoint(); got != 24.6 {
				t.Errorf("zone setpoint = %.2f, want the wheel change 24.60", got)
			}
		})
	}
}

// TestHAWriteThenMemberWheelChangeIsTaken: the whole flow. A coordinator write
// to the owner, the owner push over the relay with both ACKs, then the member
// wheel back to 24.60 at 3 min.
func TestHAWriteThenMemberWheelChangeIsTaken(t *testing.T) {
	r := newEchoRig(t, nil)
	full := func(c uint16) []byte { return concat(echoBlock(0x02, c), TLV{Tag: "62e0", Val: []byte{0}}.Encode()) }
	r.report(r.owner, BridgeEUI, 0x6000, full(2460))
	r.report(r.member, BridgeEUI, 0x6001, full(2460))
	r.e.queue[kitchenAEUIHex], r.e.queue[kitchenBEUIHex] = nil, nil
	if err := r.e.EnqueueSetpoint(kitchenBEUIHex, SetpointParams{ID: 5, Setpoint: 25, Overlay: 2}); err != nil {
		t.Fatal(err)
	}
	w := r.e.queue[kitchenBEUIHex][0]
	if _, ok := r.e.nextDownlink(r.owner, "owner"); !ok {
		t.Fatal("no write")
	}
	r.ack(r.owner, w)
	r.now = r.now.Add(10 * time.Second)
	r.e.OnFrame(zoneUplink(t, r.owner, r.member, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x5201, Token: []byte{0x01},
		Path: []string{"z", "s"}, Query: []string{"lid=5"}, Payload: full(2500),
	}))
	deliverHop(t, r, r.member, r.owner, true, true)
	r.now = r.now.Add(3 * time.Minute)
	r.report(r.member, r.owner, 0x6002, full(2460))
	if got := r.setpoint(); got != 24.6 {
		t.Errorf("zone setpoint = %.2f, want the wheel change 24.60", got)
	}
	if got := r.e.Pending(kitchenAEUIHex); got != 0 {
		t.Errorf("the member holds %d queued writes, want none", got)
	}
}

// TestRelayedGetAnswerHoldsBlock: the member reads the block from the owner with
// GET z/s. The answer reaches it through the relay and its Enhanced ACK closes
// the hop. The member then holds that block.
func TestRelayedGetAnswerHoldsBlock(t *testing.T) {
	r := newEchoRig(t, nil)
	r.report(r.member, r.owner, 0x7000, echoBlock(0x02, 2460))
	r.write()
	r.e.OnFrame(zoneUplink(t, r.member, r.owner, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7301, Token: []byte{0x01}, Path: []string{"z", "s"}, Query: []string{"lid=5"},
	}))
	r.e.OnFrame(zoneUplink(t, r.owner, r.member, CoAPReq{
		Type: "ACK", Code: "2.05", MID: 0x7301, Token: []byte{0x01}, Payload: echoBlock(0x02, 2500),
	}))
	if r.e.heldNewest(r.member, 5) {
		t.Fatal("the member holds the block before the answer reached it")
	}
	deliverHop(t, r, r.member, r.owner, true, false)
	if !r.e.heldNewest(r.member, 5) {
		t.Error("the member read 25.00 from the owner and does not hold it")
	}
}

// TestCoordinatorOwnedZoneHasNoOwnerDevice: the measuring device of a zone the
// coordinator owns sends no overlay PUT, so its report of a replaced block is
// judged as a member's. Having confirmed the newest block, its wheel change is
// taken at once.
func TestCoordinatorOwnedZoneHasNoOwnerDevice(t *testing.T) {
	y := strings.Replace(fixtureInstallYAML, "  utc_offset_min: 60\n", "  utc_offset_min: 60\n  zone_owner: {5: coordinator}\n", 1)
	for _, confirmed := range []bool{true, false} {
		r := newEchoRig(t, func(c *EmuConfig) {
			in, err := Parse([]byte(y))
			if err != nil {
				t.Fatal(err)
			}
			*c = in.EmuConfig()
			c.Now = func() time.Time { return time.Date(2026, 1, 5, 6, 0, 0, 0, time.UTC) }
		})
		dev := mustHex(capKitchenOwner) // the measuring device
		r.write()
		if confirmed {
			r.report(dev, BridgeEUI, 0x7400, echoBlock(0x02, 2500))
		}
		r.now = r.now.Add(3 * time.Minute)
		r.report(dev, BridgeEUI, 0x7401, echoBlock(0x02, 2460))
		got := r.setpoint()
		switch {
		case confirmed && got != 24.6:
			t.Errorf("confirmed: zone setpoint = %.2f, want the wheel change 24.60", got)
		case !confirmed && got != 25:
			t.Errorf("not confirmed: zone setpoint = %.2f, want 25.00", got)
		}
		if !confirmed && r.e.Pending(capKitchenOwner) != 1 {
			t.Errorf("not confirmed: the device holds %d queued writes, want the rewrite", r.e.Pending(capKitchenOwner))
		}
	}
}

// TestDeviceOverlayIsAZoneChangeForStaleReports: the stale rewrite is on the air
// and the member takes it, then the owner wheel sets 22.00. The member pushes the
// 25.00 it took. The overlay replaced 25.00 and the member did not confirm 22.00,
// so the push is stale: it does not reach the owner, which would take it as a
// local change over its overlay. An overlay end counts the same way.
func TestDeviceOverlayIsAZoneChangeForStaleReports(t *testing.T) {
	in := fixtureInstall(t)
	for _, tc := range []struct {
		name string
		step func(r *echoRig)
		want float64
	}{
		{"overlay", func(r *echoRig) {
			r.e.OnFrame(synthDeviceUplink(t, in, r.owner, CoAPReq{
				Type: "CON", Code: "0.03", MID: 0x7a02, Token: []byte{0x33},
				Path: homePath(in, "z", "5", "overlay"), Options: []CoAPOption{optOctets},
				Payload: echoBlock(0x02, 2200),
			}))
		}, 22},
		{"end", func(r *echoRig) {
			r.e.OnFrame(synthDeviceUplink(t, in, r.owner, CoAPReq{
				Type: "CON", Code: "0.04", MID: 0x7a03, Token: []byte{0x34},
				Path: homePath(in, "z", "5", "overlay"), Options: []CoAPOption{optOctets},
			}))
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newEchoRig(t, nil)
			r.report(r.member, BridgeEUI, 0x7a00, concat(echoBlock(0x02, 2460), TLV{Tag: "62e0", Val: []byte{0}}.Encode()))
			r.e.queue[kitchenBEUIHex] = nil
			r.write()
			r.report(r.member, BridgeEUI, 0x7a01, echoBlock(0x02, 2460))
			if q := r.e.queue[kitchenAEUIHex]; len(q) != 1 || !q[0].staleRewrite || !q[0].inFlight {
				t.Fatalf("queue = %+v, want the stale rewrite on the air", r.e.Queue(kitchenAEUIHex))
			}
			r.now = r.now.Add(2 * time.Second)
			tc.step(r)

			relays := r.e.Stats().ZoneRelays
			r.now = r.now.Add(5 * time.Second)
			r.report(r.member, r.owner, 0x7a04, echoBlock(0x02, 2500))
			if got := r.e.Stats().ZoneRelays - relays; got != 0 {
				t.Errorf("the push of the replaced block was relayed %d times, want none", got)
			}
			if tc.want != 0 {
				if got := r.setpoint(); got != tc.want {
					t.Errorf("zone setpoint = %.2f, want the overlay %.2f", got, tc.want)
				}
			}
			// The owner pushing its own overlay block is no stale report.
			r.report(r.owner, r.member, 0x7a05, echoBlock(0x02, 2200))
			if tc.want != 0 && r.setpoint() != tc.want {
				t.Errorf("zone setpoint = %.2f after the owner push, want %.2f", r.setpoint(), tc.want)
			}
		})
	}
}

// TestStaleVerdictsArePruned: a verdict older than the keep time leaves the map
// when the next verdict is stored.
func TestStaleVerdictsArePruned(t *testing.T) {
	r := newEchoRig(t, nil)
	r.write()
	for i := range 3 {
		r.report(r.member, r.owner, 0x7b00+uint16(i), echoBlock(0x02, 2460))
	}
	if got := len(r.e.staleVerdicts); got != 3 {
		t.Fatalf("staleVerdicts = %d, want 3", got)
	}
	r.now = r.now.Add(staleVerdictKeep + time.Second)
	r.report(r.member, r.owner, 0x7b10, echoBlock(0x02, 2460))
	if got := len(r.e.staleVerdicts); got != 1 {
		t.Errorf("staleVerdicts = %d after the keep time, want only the new one", got)
	}
	for i := range 360 {
		r.now = r.now.Add(10 * time.Second)
		r.report(r.member, r.owner, 0x1000+uint16(i), echoBlock(0x02, 2460))
	}
	if got := len(r.e.staleVerdicts); got > 31 {
		t.Errorf("staleVerdicts = %d after an hour of reports, want at most 31", got)
	}
}

// TestDropDeviceForgetsHeldBlocks: a device that leaves the installation takes
// its held blocks with it and no other device loses its own.
func TestDropDeviceForgetsHeldBlocks(t *testing.T) {
	r := newEchoRig(t, nil)
	block := heatingTagsOf(parseTLVList(echoBlock(0x02, 2500)))
	r.e.holdBlock(r.member, 5, block)
	r.e.holdBlock(r.member, 3, block)
	r.e.holdBlock(r.owner, 5, block)
	r.e.dropDevice(hexEUI(r.member))
	if _, ok := r.e.heldBlocks[heldKey(r.member, 5)]; ok {
		t.Error("the dropped device keeps its block of zone 5")
	}
	if _, ok := r.e.heldBlocks[heldKey(r.member, 3)]; ok {
		t.Error("the dropped device keeps its block of zone 3")
	}
	if _, ok := r.e.heldBlocks[heldKey(r.owner, 5)]; !ok {
		t.Error("another device lost its block")
	}
}
