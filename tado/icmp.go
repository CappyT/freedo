package tado

import (
	"bytes"
	"encoding/binary"
)

// The coordinator answers two ICMPv6 messages a device sends it: an Echo
// Request with an Echo Reply, and a Neighbor Solicitation for its own address
// with a unicast Neighbor Advertisement. the pairing capture holds one of
// each from a valve, and the bridge queued both answers for the next CSL
// rendezvous of the device.
const (
	icmpNeighborAdvert = 136
	optTLLAO           = 2 // target link-layer address
	// naFlagsRSO sets Router, Solicited and Override, as the bridge does.
	naFlagsRSO = 0xe0

	iphcHopLimit64  = 0x7a // TF elided, next header inline, hop limit 64
	iphcHopLimit255 = 0x7b // the same with hop limit 255, which RFC 4861 requires
)

// icmpv6In is an inbound ICMPv6 message with its expanded IPv6 addresses.
type icmpv6In struct {
	hopLimit byte
	src, dst []byte // 16 bytes each
	msg      []byte
}

// parseICMPv6 decodes the compressed IPv6 forms a device sends ICMPv6 in: TF
// elided, next header inline, no context, source from the L2 address, and a
// destination either elided (unicast to l2Dst) or an inline multicast address.
// It rejects a message whose ICMPv6 checksum does not verify.
func parseICMPv6(packet, l2Src, l2Dst []byte) (icmpv6In, bool) {
	var in icmpv6In
	if len(packet) < 3 || packet[0]&0xfc != 0x78 || packet[1]&0xf0 != 0x30 || packet[2] != nhICMPv6 {
		return in, false
	}
	switch packet[0] & 3 {
	case 1:
		in.hopLimit = 1
	case 2:
		in.hopLimit = 64
	case 3:
		in.hopLimit = 255
	default:
		return in, false // an inline hop limit: no device sends one
	}
	in.src = linkLocalFromWire(l2Src)
	i := 3
	switch packet[1] & 0x0f {
	case 0x03: // M=0, DAC=0, DAM=3: link-local from the L2 destination
		in.dst = linkLocalFromWire(l2Dst)
	case 0x09: // M=1, DAM=1: ffXX::00XX:XXXX:XXXX
		if len(packet) < i+6 {
			return in, false
		}
		in.dst = make([]byte, 16)
		in.dst[0], in.dst[1] = 0xff, packet[i]
		copy(in.dst[11:], packet[i+1:i+6])
		i += 6
	case 0x0b: // M=1, DAM=3: ff02::00XX
		if len(packet) < i+1 {
			return in, false
		}
		in.dst = make([]byte, 16)
		in.dst[0], in.dst[1], in.dst[15] = 0xff, 0x02, packet[i]
		i++
	default:
		return in, false
	}
	in.msg = packet[i:]
	if len(in.msg) < 4 {
		return in, false
	}
	zeroed := append([]byte(nil), in.msg...)
	zeroed[2], zeroed[3] = 0, 0
	if icmpChecksumV6(in.src, in.dst, zeroed) != binary.BigEndian.Uint16(in.msg[2:4]) {
		return in, false
	}
	return in, true
}

// solicitedNode returns the solicited-node multicast address of addr.
func solicitedNode(addr []byte) []byte {
	a := make([]byte, 16)
	a[0], a[1], a[11], a[12] = 0xff, 0x02, 0x01, 0xff
	copy(a[13:], addr[13:16])
	return a
}

// icmpv6Answer returns the compressed IPv6 packet that answers an ICMPv6
// message from l2Src, and the ICMPv6 type of the answer. It answers an Echo
// Request to selfEUI with an Echo Reply that echoes identifier, sequence and
// data, and a Neighbor Solicitation for the address of selfEUI with a Neighbor
// Advertisement that carries selfEUI as target link-layer address. Every other
// message gets no answer.
func icmpv6Answer(selfEUI, l2Src, l2Dst, packet []byte) ([]byte, byte, bool) {
	in, ok := parseICMPv6(packet, l2Src, l2Dst)
	if !ok || in.msg[1] != 0 {
		return nil, 0, false
	}
	self := linkLocalFromWire(selfEUI)
	var iphc0 byte
	var msg []byte
	switch in.msg[0] {
	case icmpEchoRequest:
		if !bytes.Equal(in.dst, self) {
			return nil, 0, false
		}
		iphc0 = iphcHopLimit64
		msg = append([]byte{icmpEchoReply, 0, 0, 0}, in.msg[4:]...)
	case icmpNeighborSolicit:
		// RFC 4861 section 7.1.1: hop limit 255 and room for the target.
		if in.hopLimit != 255 || len(in.msg) < 24 || !bytes.Equal(in.msg[8:24], self) {
			return nil, 0, false
		}
		if !bytes.Equal(in.dst, self) && !bytes.Equal(in.dst, solicitedNode(self)) {
			return nil, 0, false
		}
		iphc0 = iphcHopLimit255
		msg = make([]byte, 0, 40)
		msg = append(msg, icmpNeighborAdvert, 0, 0, 0, naFlagsRSO, 0, 0, 0)
		msg = append(msg, self...)
		msg = append(msg, optTLLAO, 2)
		msg = append(msg, euiBigEndian(selfEUI)...)
		msg = append(msg, make([]byte, 6)...) // pad the option to 16 bytes
	default:
		return nil, 0, false
	}
	binary.BigEndian.PutUint16(msg[2:4], icmpChecksumV6(self, in.src, msg))
	return append([]byte{iphc0, iphcLinkLocal1, nhICMPv6}, msg...), msg[0], true
}

// icmpKind is the queue kind of an ICMPv6 answer.
const icmpKind = "icmp"

// answerICMPv6 queues the answer to an ICMPv6 packet from src, if it needs one,
// and reports whether it did. The bridge retries the answer under the same MAC
// seq with a new frame counter each time, as it does a CoAP response. A copy
// already queued is not queued twice.
func (e *EmuState) answerICMPv6(src, l2Dst, packet []byte) bool {
	pkt, typ, ok := icmpv6Answer(e.selfEUI, src, l2Dst, packet)
	if !ok {
		return false
	}
	key := hexEUI(src)
	for _, d := range e.queue[key] {
		if d.kind == icmpKind && bytes.Equal(d.coap, pkt) {
			return true
		}
	}
	desc := "icmpv6 echo-reply"
	if typ == icmpNeighborAdvert {
		desc = "icmpv6 neighbor-advertisement"
	}
	e.queue[key] = append(e.queue[key], &queuedDownlink{
		kind: icmpKind, desc: desc, coap: pkt, key: e.key, responseRetry: true,
	})
	e.stats.ICMPv6Answers++
	return true
}
