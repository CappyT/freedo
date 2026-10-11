package tado

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// icmpMsg fills in the ICMPv6 checksum of msg for src -> dst.
func icmpMsg(src, dst, msg []byte) []byte {
	out := append([]byte(nil), msg...)
	out[2], out[3] = 0, 0
	binary.BigEndian.PutUint16(out[2:4], icmpChecksumV6(src, dst, out))
	return out
}

// deviceICMPFrame is a Data frame from dev to the bridge carrying packet.
func deviceICMPFrame(t *testing.T, dev, packet []byte) []byte {
	t.Helper()
	f, err := BuildData(NetworkKey, 0x44, BridgeEUI, dev[:5], WrapAppPayload(packet, 900, nil))
	if err != nil {
		t.Fatal(err)
	}
	return f.Bytes()
}

// icmpDownlink returns the IPv6 packet of the one queued ICMPv6 answer.
func icmpDownlink(t *testing.T, actions []Action, reason string) (*Frame, []byte) {
	t.Helper()
	a, ok := findAction(actions, ActData, "downlink "+reason)
	if !ok {
		t.Fatalf("no %q downlink in %v", reason, reasons(actions))
	}
	f, err := ParseData(NetworkKey, a.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return f, f.Plaintext[8 : len(f.Plaintext)-2]
}

// TestEchoRequestAnswered: an Echo Request from a device to the bridge gets an
// Echo Reply with the same identifier, sequence and data, in the form the
// commercial bridge sends (the pairing capture): link-local addresses
// elided, hop limit 64.
func TestEchoRequestAnswered(t *testing.T) {
	dev := mustB(NodeByAny("camera").WireLE)
	src, dst := linkLocalFromWire(dev), linkLocalFromWire(BridgeEUI)
	req := icmpMsg(src, dst, []byte{icmpEchoRequest, 0, 0, 0, 0x40, 0x40, 0x07, 0x08})
	e, _, _ := newTestEmu(t)
	actions := e.OnFrame(deviceICMPFrame(t, dev, append([]byte{0x7a, 0x33, nhICMPv6}, req...)))

	f, got := icmpDownlink(t, actions, "icmpv6 echo-reply")
	if !bytes.Equal(f.DstEUI, dev) {
		t.Errorf("reply to %x, want %x", f.DstEUI, dev)
	}
	want := append([]byte{0x7a, 0x33, nhICMPv6},
		icmpMsg(dst, src, []byte{icmpEchoReply, 0, 0, 0, 0x40, 0x40, 0x07, 0x08})...)
	if !bytes.Equal(got, want) {
		t.Errorf("reply %x, want %x", got, want)
	}
	// Type 128 -> 129 is the only change, so the checksum drops by 0x0100.
	if binary.BigEndian.Uint16(req[2:4])-binary.BigEndian.Uint16(got[5:7]) != 0x0100 {
		t.Errorf("checksum %x for request %x", got[5:7], req[2:4])
	}
	if e.Stats().ICMPv6Answers != 1 {
		t.Errorf("ICMPv6Answers=%d, want 1", e.Stats().ICMPv6Answers)
	}

	// The same request again does not queue a second reply.
	e.OnFrame(deviceICMPFrame(t, dev, append([]byte{0x7a, 0x33, nhICMPv6}, req...)))
	if e.Stats().ICMPv6Answers != 1 {
		t.Errorf("a repeated request queued another reply: %d", e.Stats().ICMPv6Answers)
	}
}

// TestEchoRequestRejected: a bad checksum or an Echo Reply gets no answer.
func TestEchoRequestRejected(t *testing.T) {
	dev := mustB(NodeByAny("camera").WireLE)
	src, dst := linkLocalFromWire(dev), linkLocalFromWire(BridgeEUI)
	bad := icmpMsg(src, dst, []byte{icmpEchoRequest, 0, 0, 0, 0, 1, 0, 2})
	bad[3] ^= 1
	reply := icmpMsg(src, dst, []byte{icmpEchoReply, 0, 0, 0, 0, 1, 0, 2})
	for name, msg := range map[string][]byte{"bad checksum": bad, "echo reply": reply} {
		e, _, _ := newTestEmu(t)
		actions := e.OnFrame(deviceICMPFrame(t, dev, append([]byte{0x7a, 0x33, nhICMPv6}, msg...)))
		if _, ok := findAction(actions, ActData, "downlink icmpv6"); ok || e.Stats().ICMPv6Answers != 0 {
			t.Errorf("%s: answered: %v", name, reasons(actions))
		}
	}
}

// neighborSolicit builds the Neighbor Solicitation a device broadcasts for
// target, to its solicited-node address, as the pairing capture holds it.
func neighborSolicit(dev, target []byte) []byte {
	src, dst := linkLocalFromWire(dev), solicitedNode(target)
	msg := []byte{icmpNeighborSolicit, 0, 0, 0, 0, 0, 0, 0}
	msg = append(msg, target...)
	msg = append(msg, optSLLAO, 2)
	msg = append(msg, euiBigEndian(dev)...)
	msg = append(msg, make([]byte, 6)...)
	packet := []byte{0x7b, 0x39, nhICMPv6, 0x02, 0x01, 0xff}
	packet = append(packet, target[13:16]...)
	return append(packet, icmpMsg(src, dst, msg)...)
}

// TestNeighborSolicitationAnswered: a Neighbor Solicitation for the bridge
// address gets a unicast Neighbor Advertisement with R, S and O set and the
// bridge EUI-64 as target link-layer address, hop limit 255, both addresses
// elided: the bytes of the captured advertisement.
func TestNeighborSolicitationAnswered(t *testing.T) {
	dev := mustB(NodeByAny("camera").WireLE)
	self := linkLocalFromWire(BridgeEUI)
	ns, err := BuildBroadcast(NetworkKey, 0x6e, dev, 6, bcastSecCtrl, WrapBroadcastPayload(neighborSolicit(dev, self), 0))
	if err != nil {
		t.Fatal(err)
	}
	e, _, _ := newTestEmu(t)
	f, got := icmpDownlink(t, e.OnFrame(ns), "icmpv6 neighbor-advertisement")
	if !bytes.Equal(f.DstEUI, dev) {
		t.Errorf("advertisement to %x, want %x", f.DstEUI, dev)
	}
	msg := []byte{icmpNeighborAdvert, 0, 0, 0, 0xe0, 0, 0, 0}
	msg = append(msg, self...)
	msg = append(msg, optTLLAO, 2)
	msg = append(msg, euiBigEndian(BridgeEUI)...)
	msg = append(msg, make([]byte, 6)...)
	want := append([]byte{0x7b, 0x33, nhICMPv6}, icmpMsg(self, linkLocalFromWire(dev), msg)...)
	if !bytes.Equal(got, want) {
		t.Errorf("advertisement %x, want %x", got, want)
	}
	if st := e.Stats(); st.NeighborSolicitations != 1 || st.ICMPv6Answers != 1 {
		t.Errorf("stats %+v", st)
	}
}

// TestNeighborSolicitationForOthersIgnored: a solicitation for another address
// is not the bridge's to answer.
func TestNeighborSolicitationForOthersIgnored(t *testing.T) {
	dev := mustB(NodeByAny("camera").WireLE)
	other := linkLocalFromWire(mustB(NodeByAny("bagno").WireLE))
	ns, err := BuildBroadcast(NetworkKey, 0x6e, dev, 6, bcastSecCtrl, WrapBroadcastPayload(neighborSolicit(dev, other), 0))
	if err != nil {
		t.Fatal(err)
	}
	e, _, _ := newTestEmu(t)
	if actions := e.OnFrame(ns); actions != nil {
		t.Errorf("actions %v, want none", reasons(actions))
	}
}

// icmpNoiseFrame returns a Data frame from dev carrying packet, with an app
// counter picked so that ParseApp's TLV heuristic accepts the ICMPv6 bytes. The
// heuristic reads the app trailer too, so which counters it accepts varies with
// the packet.
func icmpNoiseFrame(t *testing.T, dev, packet []byte) []byte {
	t.Helper()
	for ctr := uint32(1); ctr < 4096; ctr++ {
		f, err := BuildData(NetworkKey, 0x44, BridgeEUI, dev[:5], WrapAppPayload(packet, ctr, nil))
		if err != nil {
			t.Fatal(err)
		}
		p, err := ParseData(NetworkKey, f.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		if app := ParseApp(p.Plaintext); app != nil && app.CoAP == nil && len(app.TLVs) > 0 {
			return f.Bytes()
		}
	}
	t.Fatal("no app counter makes the TLV heuristic accept the packet")
	return nil
}

// TestICMPv6BodyIsNotTelemetry: ICMPv6 carries no TLVs, but ParseApp's TLV
// heuristic reads some of its bytes as tags. A valve's Neighbor Solicitation
// holds its own EUI-64 in the source link-layer option, which reads as tag 1bc5
// with a 7-byte value and an empty tag 0000. Its Echo Request holds the next
// header and type bytes 3a80 and then the checksum. None of it is telemetry.
func TestICMPv6BodyIsNotTelemetry(t *testing.T) {
	dev := mustB("4433221107c51b00")
	src, dst := linkLocalFromWire(dev), linkLocalFromWire(BridgeEUI)
	echo := append([]byte{0x7a, 0x33, nhICMPv6}, icmpMsg(src, dst, []byte{icmpEchoRequest, 0, 0, 0})...)
	for name, packet := range map[string][]byte{
		"neighbor solicitation": neighborSolicit(dev, dst),
		"echo request":          echo,
	} {
		e, _, _ := newTestEmu(t)
		e.OnFrame(icmpNoiseFrame(t, dev, packet))
		ns := e.node(dev)
		if len(ns.Telemetry) != 0 || len(ns.raw) != 0 {
			t.Errorf("%s: cached telemetry %v and %d raw tags, want none", name, ns.Telemetry, len(ns.raw))
		}
		if got := e.Stats().ICMPv6; got != 1 {
			t.Errorf("%s: ICMPv6 counter = %d, want 1", name, got)
		}
	}
}
