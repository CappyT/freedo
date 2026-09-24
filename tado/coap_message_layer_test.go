package tado

import (
	"bytes"
	"encoding/hex"
	"strconv"
	"testing"
	"time"
)

// macRetransmit rebuilds a device Data frame as the device's MAC retransmission
// of it: the same MAC sequence number and the same datagram under a new frame
// counter. The captured retransmissions keep the seq and advance the counter by
// one (capture-app-20260907, corridoio seq 75, 78, 7d, 8d, a3).
func macRetransmit(t *testing.T, raw []byte, ctr uint32) []byte {
	t.Helper()
	f, err := ParseData(NetworkKey, raw)
	if err != nil {
		t.Fatal(err)
	}
	return reframe(t, raw, f.Seq, ctr)
}

// reframe carries the datagram of a device Data frame in a new frame with the
// given MAC sequence number and frame counter. A CoAP retransmission is a new
// frame with a new seq (capture-app-20260907: camera GET mid 0x7437 at seq 19
// and 1a).
func reframe(t *testing.T, raw []byte, seq byte, ctr uint32) []byte {
	t.Helper()
	f, err := ParseData(NetworkKey, raw)
	if err != nil {
		t.Fatal(err)
	}
	pkt := f.Plaintext[8 : len(f.Plaintext)-2]
	out, err := BuildData(NetworkKey, seq, f.DstEUI, f.SrcPrefix, WrapAppPayload(pkt, ctr, nil))
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// relayedCoAP returns the CoAP message of the last frame ScheduleDownlink builds
// for peer.
func relayedCoAP(t *testing.T, e *EmuState, log *UnhandledLogger, peer []byte) *CoAPMessage {
	t.Helper()
	out, err := e.ScheduleDownlink(hex.EncodeToString(peer))
	if err != nil {
		t.Fatalf("%v; unhandled:\n%s", err, log.Summary())
	}
	f, err := ParseData(NetworkKey, out[len(out)-1].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	app := ParseApp(f.Plaintext)
	if app == nil || app.CoAP == nil {
		t.Fatal("the relayed frame carries no CoAP message")
	}
	return app.CoAP
}

// TestZoneRelayEmptyMessageVerbatim: the commercial bridge relays an empty ACK
// and an empty RST as they came, 4 bytes with no option 2048
// (capture-app-20260907: corridoio -> bridge -> camera, ufficio, bagno,
// cucina, soggiorno). An empty message carries no options (RFC 7252, 4.1).
func TestZoneRelayEmptyMessageVerbatim(t *testing.T) {
	sender := mustHex(NodeByAny("0a01").WireLE)
	peer := mustHex(NodeByAny("a205").WireLE)
	for _, typ := range []string{"ACK", "RST"} {
		t.Run(typ, func(t *testing.T) {
			e, log, _ := newTestEmu(t)
			raw := zoneUplink(t, sender, peer, CoAPReq{Type: typ, Code: "0.00", MID: 0x742b, Token: []byte{}})
			up, err := ParseData(NetworkKey, raw)
			if err != nil {
				t.Fatal(err)
			}
			want := ParseApp(up.Plaintext).CoAP.Raw
			e.OnFrame(raw)
			got := relayedCoAP(t, e, log, peer).Raw
			if len(got) != 4 || !bytes.Equal(got, want) {
				t.Errorf("relayed empty %s = %x, want the 4 bytes %x", typ, got, want)
			}
		})
	}
}

// TestStampSessionIDLeavesEmptyMessage: an empty message takes no option.
func TestStampSessionIDLeavesEmptyMessage(t *testing.T) {
	for _, msg := range [][]byte{{0x60, 0x00, 0x74, 0x2b}, {0x70, 0x00, 0x74, 0x2b}} {
		got, err := StampSessionID(msg, mustHex("5e55000000000001"))
		if err != nil || !bytes.Equal(got, msg) {
			t.Errorf("StampSessionID(%x) = %x, %v; want it untouched", msg, got, err)
		}
	}
}

// TestBlock2ReasmIgnoresRetransmittedBlock: a block that arrives again, as the
// device's MAC retransmission of it, leaves the transfer open. A block with the
// same number and other bytes still ends the transfer.
func TestBlock2ReasmIgnoresRetransmittedBlock(t *testing.T) {
	now := time.Unix(1000, 0)
	r := newBlock2Reasm(func() time.Time { return now })
	b0, b1, b2 := bytes.Repeat([]byte{0xa0}, 16), bytes.Repeat([]byte{0xa1}, 16), []byte{0xa2, 0xa2}
	steps := []struct {
		num  uint32
		more bool
		data []byte
	}{{0, true, b0}, {0, true, b0}, {1, true, b1}, {1, true, b1}, {2, false, b2}}
	var body []byte
	for i, s := range steps {
		got, done, err := r.Add("k", s.num, s.more, 0, s.data)
		if err != nil {
			t.Fatalf("step %d block %d: %v", i, s.num, err)
		}
		if done {
			body = got
		}
	}
	if want := append(append(append([]byte(nil), b0...), b1...), b2...); !bytes.Equal(body, want) {
		t.Errorf("body=%x, want %x", body, want)
	}

	r.Add("k", 0, true, 0, b0)
	r.Add("k", 1, true, 0, b1)
	if _, _, err := r.Add("k", 1, true, 0, b2); err != errBlock2OutOfOrder {
		t.Errorf("a changed block 1 gave err=%v, want %v", err, errBlock2OutOfOrder)
	}
}

// TestBlock2ReasmTimeoutIsIdle: block2Timeout is the idle life of a transfer,
// so a transfer whose blocks keep arriving outlives it.
func TestBlock2ReasmTimeoutIsIdle(t *testing.T) {
	now := time.Unix(1000, 0)
	r := newBlock2Reasm(func() time.Time { return now })
	step := block2Timeout * 2 / 3
	for num := uint32(0); num < 4; num++ {
		body, done, err := r.Add("k", num, num < 3, 0, bytes.Repeat([]byte{byte(num)}, 16))
		if err != nil {
			t.Fatalf("block %d after %s: %v", num, time.Duration(num)*step, err)
		}
		if num == 3 && (!done || len(body) != 64) {
			t.Fatalf("done=%t len=%d, want the whole 64-byte body", done, len(body))
		}
		now = now.Add(step)
	}
	r.Add("k", 0, true, 0, bytes.Repeat([]byte{9}, 16))
	now = now.Add(block2Timeout + time.Second)
	if _, _, err := r.Add("k", 1, false, 0, []byte{9}); err != errBlock2NoTransfer {
		t.Errorf("a block after the idle life gave err=%v, want %v", err, errBlock2NoTransfer)
	}
}

// TestMACRetransmissionProcessedOnce: the device's MAC retransmission of a
// frame we already took gets its Enhanced ACK again, since the device missed the
// first one, and nothing else: the zone push it carries is relayed once.
func TestMACRetransmissionProcessedOnce(t *testing.T) {
	sender := mustHex(NodeByAny("0a01").WireLE)
	peer := mustHex(NodeByAny("a205").WireLE)
	now := time.Unix(1000, 0)
	e := NewEmu(EmuConfig{TxCounter: 100000, CSLPhase: 0x1234, Now: func() time.Time { return now }}, nil)
	raw := zoneUplink(t, sender, peer, CoAPReq{
		Type: "NON", Code: "0.03", MID: 0x5ef9, Token: []byte{},
		Path: []string{"z", "cpe"}, Query: []string{"lid=10"}, Options: []CoAPOption{optOctets},
		Payload: TLV{Tag: "4160", Val: []byte{0}}.Encode(),
	})
	e.OnFrame(raw)
	if n := e.Pending(hex.EncodeToString(peer)); n != 1 {
		t.Fatalf("pending=%d after the first copy, want 1", n)
	}
	out, err := e.ScheduleDownlink(hex.EncodeToString(peer))
	if err != nil {
		t.Fatal(err)
	}
	relay, err := ParseData(NetworkKey, out[len(out)-1].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	e.OnFrame(BuildEnhAck(relay.Seq, BridgeEUI, peer, 0x0101))
	if n := e.Pending(hex.EncodeToString(peer)); n != 0 {
		t.Fatalf("pending=%d after the relay was acknowledged, want 0", n)
	}

	now = now.Add(2 * time.Second)
	actions := e.OnFrame(macRetransmit(t, raw, 801))
	if _, ok := findAction(actions, ActEnhAck, ""); !ok {
		t.Error("the MAC retransmission got no Enhanced ACK")
	}
	if n := e.Pending(hex.EncodeToString(peer)); n != 0 {
		t.Errorf("the MAC retransmission was relayed again: pending=%d", n)
	}
	if st := e.Stats(); st.ZoneRelays != 1 {
		t.Errorf("ZoneRelays=%d, want 1", st.ZoneRelays)
	}

	// Past the window the same seq and bytes are a new frame: the device can
	// reuse its seq after a restart.
	now = now.Add(time.Minute)
	e.OnFrame(macRetransmit(t, raw, 900))
	if n := e.Pending(hex.EncodeToString(peer)); n != 1 {
		t.Errorf("pending=%d for the frame past the window, want 1", n)
	}
}

// TestMACSeqReuseWithOtherBytesIsProcessed: the same seq with another datagram
// is a new frame, never a retransmission.
func TestMACSeqReuseWithOtherBytesIsProcessed(t *testing.T) {
	sender := mustHex(NodeByAny("0a01").WireLE)
	peer := mustHex(NodeByAny("a205").WireLE)
	e, _, _ := newTestEmu(t)
	for i, v := range []byte{10, 20} {
		e.OnFrame(zoneUplink(t, sender, peer, CoAPReq{
			Type: "CON", Code: "0.03", MID: 0x5e00 + uint16(i), Token: []byte{},
			Path: []string{"z", "s"}, Query: []string{"lid=10"}, Options: []CoAPOption{optOctets},
			Payload: TLV{Tag: "6280", Val: []byte{0x07, v}}.Encode(),
		}))
	}
	if st := e.Stats(); st.ZoneRelays != 2 {
		t.Errorf("ZoneRelays=%d, want 2", st.ZoneRelays)
	}
}

// TestEnhAckFromUnknownNodeKeepsNoState: an Enhanced ACK carries no MIC, so one
// from a node outside the installation creates no node state. A node that sent
// an authenticated Data frame has its CSL timing learned from its ACKs.
func TestEnhAckFromUnknownNodeKeepsNoState(t *testing.T) {
	stranger := []byte{0x67, 0x45, 0x23, 0x31, 0x07, 0xc5, 0x1b, 0x00}
	if NodeByAny(hexEUI(stranger)) != nil {
		t.Fatal("the stranger is in the inventory")
	}
	e, _, _ := newTestEmu(t)
	e.OnFrame(BuildEnhAck(0x33, BridgeEUI, stranger, 0x9edc))
	if nodes := e.Nodes(); len(nodes) != 0 {
		t.Fatalf("an Enhanced ACK from an unknown node created %d node(s)", len(nodes))
	}

	e.OnFrame(deviceUplink(t, stranger, CoAPReq{
		Type: "NON", Code: "0.03", MID: 0x0101, Token: []byte{},
		Path: []string{"d", "x", "sen"}, Options: []CoAPOption{optOctets},
		Payload: TLV{Tag: "0162", Val: []byte{0x0a, 0x00}}.Encode(),
	}))
	e.OnFrame(BuildEnhAck(0x34, BridgeEUI, stranger, 0x9edc))
	nodes := e.Nodes()
	if len(nodes) != 1 || nodes[0].CSLPhase != 0x9edc {
		t.Fatalf("nodes=%+v, want the authenticated node with CSL phase 9edc", nodes)
	}
}

// TestRelayedAckConfirmsOnlyRelays: a relayed ACK carries a MID of its
// originator's space, so it closes a queued zone relay and never a request
// we authored. An ACK addressed to us carries a MID of our space, so it never
// closes a zone relay.
func TestRelayedAckConfirmsOnlyRelays(t *testing.T) {
	camera := mustHex(NodeByAny("camera").WireLE)
	peer := mustHex(NodeByAny("a205").WireLE)

	t.Run("relay path", func(t *testing.T) {
		e, _, _ := newTestEmu(t)
		if err := e.EnqueueChildLock("camera", 10, true); err != nil {
			t.Fatal(err)
		}
		wire, label, err := e.resolve("camera")
		if err != nil {
			t.Fatal(err)
		}
		out, ok := e.nextDownlink(wire, label)
		if !ok {
			t.Fatal("no downlink")
		}
		df, err := ParseData(NetworkKey, out[0].Bytes)
		if err != nil {
			t.Fatal(err)
		}
		ours := ParseApp(df.Plaintext).CoAP
		e.OnFrame(zoneUplink(t, camera, peer, CoAPReq{Type: "ACK", Code: "2.04", MID: ours.MID, Token: ours.Token}))
		if n := e.Pending("camera"); n != 1 {
			t.Errorf("a relayed ACK closed our own downlink: pending=%d", n)
		}
	})

	t.Run("self path", func(t *testing.T) {
		e, log, _ := newTestEmu(t)
		sender := mustHex(NodeByAny("0a01").WireLE)
		const mid = 0x3101
		e.OnFrame(zoneUplink(t, sender, peer, CoAPReq{
			Type: "CON", Code: "0.03", MID: mid, Token: []byte{0x11},
			Path: []string{"z", "s"}, Query: []string{"lid=10"}, Options: []CoAPOption{optOctets},
			Payload: TLV{Tag: "40a0", Val: []byte{56}}.Encode(),
		}))
		relayedCoAP(t, e, log, peer)
		target := hex.EncodeToString(peer)
		e.OnFrame(deviceUplink(t, peer, CoAPReq{Type: "ACK", Code: "2.04", MID: mid, Token: []byte{0x11}}))
		if n := e.Pending(target); n != 1 {
			t.Fatalf("an ACK addressed to us closed a zone relay: pending=%d", n)
		}
		e.OnFrame(zoneUplink(t, peer, sender, CoAPReq{Type: "ACK", Code: "2.04", MID: mid, Token: []byte{0x11}}))
		if n := e.Pending(target); n != 0 {
			t.Errorf("the relayed ACK left the zone relay pending=%d", n)
		}
	})
}

// TestResetEndsOurExchange: an RST that names the MID of our CON ends that
// exchange at once (RFC 7252, 4.2): the device will not process it, so a retry
// under the same MID gets the same RST.
func TestResetEndsOurExchange(t *testing.T) {
	e, _, _ := newTestEmu(t)
	if err := e.EnqueueSetpoint("camera", SetpointParams{ID: 4, Setpoint: 5, Sched: 5, Overlay: 2}); err != nil {
		t.Fatal(err)
	}
	wire, label, err := e.resolve("camera")
	if err != nil {
		t.Fatal(err)
	}
	out, ok := e.nextDownlink(wire, label)
	if !ok {
		t.Fatal("no downlink")
	}
	df, err := ParseData(NetworkKey, out[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	ours := ParseApp(df.Plaintext).CoAP
	e.OnFrame(deviceUplink(t, wire, CoAPReq{Type: "RST", Code: "0.00", MID: ours.MID + 1, Token: []byte{}}))
	if n := e.Pending("camera"); n != 1 {
		t.Fatalf("an RST with another MID ended the exchange: pending=%d", n)
	}
	e.OnFrame(deviceUplink(t, wire, CoAPReq{Type: "RST", Code: "0.00", MID: ours.MID, Token: []byte{}}))
	if n := e.Pending("camera"); n != 0 {
		t.Fatalf("the RST left pending=%d, want 0", n)
	}
	h := e.QueueHistory("camera")
	if len(h) != 1 || h[0].Result != "dropped" || h[0].Reason != "coap-rst" {
		t.Errorf("history=%+v, want dropped/coap-rst", h)
	}
	if st := e.Stats(); st.DownlinksDropped != 1 || st.DownlinksDelivered != 0 {
		t.Errorf("dropped=%d delivered=%d, want 1 and 0", st.DownlinksDropped, st.DownlinksDelivered)
	}
}

// TestPingGetsReset: a CON empty message is a CoAP ping, and the answer is an
// RST with its MID (RFC 7252, 4.3).
func TestPingGetsReset(t *testing.T) {
	camera := mustHex(NodeByAny("camera").WireLE)
	e, log, _ := newTestEmu(t)
	actions := e.OnFrame(deviceUplink(t, camera, CoAPReq{Type: "CON", Code: "0.00", MID: 0x4242, Token: []byte{}}))
	m := servedAnswer(t, actions, 0x4242)
	if want := []byte{0x70, 0x00, 0x42, 0x42}; !bytes.Equal(m.Raw, want) {
		t.Errorf("answer=%x, want the empty RST %x; unhandled:\n%s", m.Raw, want, log.Summary())
	}
}

// TestDirectZoneReadCarriesETagAndBlock2: every captured answer to GET
// h/<home>/z/N/s (21 in capture-app-20260907) carries an ETag and Block2
// 0/0/128, and no Content-Format. Reads of the same block carry the same ETag,
// whichever zone and device: 107917f7351475a6 answers zones 8, 5, 2 and 1.
func TestDirectZoneReadCarriesETagAndBlock2(t *testing.T) {
	in := synthInstall(t)
	ru, err := in.Resolve("RU0000000001")
	if err != nil {
		t.Fatal(err)
	}
	va, err := in.Resolve("VA0000000001")
	if err != nil {
		t.Fatal(err)
	}
	e := NewEmu(in.EmuConfig(), nil)
	e.SetHomeState(true, map[int]float64{0: 55, 4: 20})
	read := func(dev []byte, zone int, mid uint16) *CoAPMessage {
		t.Helper()
		raw := synthDeviceUplink(t, in, dev, CoAPReq{
			Type: "CON", Code: "0.01", MID: mid, Token: []byte{},
			Path:    homePath(in, "z", strconv.Itoa(zone), "s"),
			Options: []CoAPOption{optOctets, {Num: 23, Val: []byte{0x03}}},
		})
		return synthAnswer(t, in, e.OnFrame(raw), mid)
	}
	ruRead, vaRead, waterRead := read(ru.WireEUI(), 4, 0x7101), read(va.WireEUI(), 4, 0x7102), read(ru.WireEUI(), 0, 0x7103)
	for _, m := range []*CoAPMessage{ruRead, vaRead, waterRead} {
		etag := optionValue(m, 4)
		if m.Code != "2.05" || len(etag) != 8 || len(m.Payload) == 0 {
			t.Fatalf("answer %s etag=%x body=%x, want 2.05 with an 8-byte ETag and a body", m.Code, etag, m.Payload)
		}
		if block := optionValue(m, 23); !bytes.Equal(block, []byte{0x03}) {
			t.Errorf("Block2=%x, want 03 (0/0/128)", block)
		}
		if cf, ok := m.Option(12); ok {
			t.Errorf("the answer carries Content-Format %x, the captured one none", cf.Val)
		}
	}
	if !bytes.Equal(ruRead.Payload, vaRead.Payload) || !bytes.Equal(optionValue(ruRead, 4), optionValue(vaRead, 4)) {
		t.Errorf("one block read twice: etags %x and %x", optionValue(ruRead, 4), optionValue(vaRead, 4))
	}
	if bytes.Equal(optionValue(ruRead, 4), optionValue(waterRead, 4)) {
		t.Errorf("two blocks share the ETag %x", optionValue(ruRead, 4))
	}
}
