package tado

import (
	"bytes"
	"crypto/aes"
	"encoding/binary"
	"errors"
)

// The 0x49e8 broadcast carries no CoAP: it is a plain ICMPv6 message to a
// multicast address, 6LoWPAN-compressed. The bridge sends a Router Advertisement
// with the network configuration; devices send Router Solicitations and Neighbor
// Solicitations the same way.
//
//	on air     FCF(2) Seq(1) DstShort(2=ffff) Src(8) SecCtrl(1) FrameCtrLow(2 LE) ct MIC(4)
//	plaintext  FrameCtrHigh(2 LE) | IPHC + ICMPv6 | trailer(2)
//
// The 32-bit frame counter straddles the 16-byte cut that forms the aad: its low
// half is the last header field, its high half opens the plaintext and stays zero
// while the counter is below 65536.
const (
	// iphcMcast is the IPHC pair the broadcast always uses: TF elided, next
	// header inline, hop limit 255, source from the L2 EUI-64, multicast dst.
	iphcMcast0 = 0x7b
	iphcMcast1 = 0x3b // DAM=3: 1 inline byte, ff02::00XX
	nhICMPv6   = 58

	icmpRouterSolicit   = 133
	icmpRouterAdvert    = 134
	icmpNeighborSolicit = 135
	icmpEchoRequest     = 128
	icmpEchoReply       = 129
	raHeaderLen         = 16 // type, code, checksum, hop limit, flags, lifetime, 2 timers

	optSLLAO = 1  // source link-layer address
	optMTU   = 5  // link MTU
	optIPv4  = 25 // RFC 8106 RDNSS layout; the bridge fills it with the IPv4 config
)

// ErrNotRA means the broadcast payload is not an ICMPv6 Router Advertisement in
// the shape the bridge emits.
var ErrNotRA = errors.New("tado: not an ICMPv6 Router Advertisement")

// RouterAdvert is the ICMPv6 Router Advertisement the bridge broadcasts.
type RouterAdvert struct {
	HopLimit       byte
	Flags          byte
	RouterLifetime uint16 // seconds
	SLLAO          []byte // 8-byte EUI-64, big-endian (the bridge's own)
	MTU            uint32
	// Option 25 carries the IPv4 side of the network as ::ffff:a.b.c.d addresses.
	// Its 32-bit field sits where RFC 8106 puts the RDNSS lifetime; on air it
	// reads ffffffff, which is the netmask of the bridge's LAN.
	Netmask uint32
	Gateway []byte // 4 bytes
	DNS     []byte // 4 bytes
}

// BroadcastMsg is a decrypted 0x49e8 broadcast Data frame.
type BroadcastMsg struct {
	Seq          byte
	SrcEUI       []byte // 8 bytes, wire (little-endian)
	SecCtrl      byte   // aux-header Security Control (0x04 on air)
	FrameCounter uint32
	Plaintext    []byte // decrypted payload, counter half and trailer included
	ICMPType     byte   // 134 RA, 133 Router Solicitation, 135 Neighbor Solicitation
	RA           *RouterAdvert
}

// UnicastICMPv6Type parses ICMPv6 inside the unicast tado shim.
// Broadcast plaintext uses a different prefix layout.
func UnicastICMPv6Type(pt []byte) (byte, bool) {
	if len(pt) < 8+2+1+4+2 || !bytes.Equal(pt[:3], EUISuffix) {
		return 0, false
	}
	nh, msg, err := icmpv6Payload(pt[8 : len(pt)-2])
	if err != nil || nh != nhICMPv6 || len(msg) < 4 {
		return 0, false
	}
	return msg[0], true
}

// BuildICMPv6EchoRequest builds the four-byte link-maintenance probe.
// The firmware elides both link-local addresses and sends no Echo ID or payload.
func BuildICMPv6EchoRequest(srcEUI, dstEUI []byte) ([]byte, error) {
	if len(srcEUI) != 8 || len(dstEUI) != 8 {
		return nil, errors.New("tado: ICMPv6 Echo Request needs 8-byte source and destination EUI")
	}
	msg := []byte{icmpEchoRequest, 0, 0, 0}
	binary.BigEndian.PutUint16(msg[2:4], icmpChecksumV6(linkLocalFromWire(srcEUI), linkLocalFromWire(dstEUI), msg))
	return append([]byte{0x7a, iphcLinkLocal1, nhICMPv6}, msg...), nil
}

// ParseBroadcast decrypts a 0x49e8 broadcast frame without PHY CRC and decodes its
// ICMPv6 payload. It returns ErrMICFail when the MIC does not verify under key.
// A payload that is not a Router Advertisement leaves RA nil without an error.
func ParseBroadcast(key, frame []byte) (*BroadcastMsg, error) {
	if len(frame) < headerLen+micLen+4 {
		return nil, errors.New("tado: broadcast frame too short")
	}
	if Classify(frame) != ClassBroadcast {
		return nil, errors.New("tado: not a broadcast (0x49e8/0x59e8) frame")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pt, ok := ccmDecrypt(block, frame[0:nonceLen], frame[0:headerLen],
		frame[headerLen:len(frame)-micLen], frame[len(frame)-micLen:])
	if !ok {
		return nil, ErrMICFail
	}
	if !validMACFCS(frame[:headerLen], pt) {
		return nil, ErrMACFCS
	}
	m := &BroadcastMsg{
		Seq:     frame[2],
		SrcEUI:  append([]byte{}, frame[5:13]...),
		SecCtrl: frame[13],
		FrameCounter: uint32(binary.LittleEndian.Uint16(frame[14:16])) |
			uint32(binary.LittleEndian.Uint16(pt[0:2]))<<16,
		Plaintext: pt,
	}
	nh, msg, err := icmpv6Payload(pt[2 : len(pt)-2])
	if err != nil || nh != nhICMPv6 || len(msg) == 0 {
		return m, nil // decrypted, but not an ICMPv6 message we decode
	}
	m.ICMPType = msg[0]
	if ra, err := parseRA(msg); err == nil {
		m.RA = ra
	}
	return m, nil
}

// icmpv6Payload returns the next-header value and the payload of a compressed
// IPv6 packet. Only the shape the broadcast uses is decoded: traffic class and
// flow label elided, next header inline, no context byte.
func icmpv6Payload(b []byte) (byte, []byte, error) {
	if len(b) < 4 || b[0]&0xfc != 0x78 || b[1]&0x80 != 0 {
		return 0, nil, ErrNotRA
	}
	i, err := skipIPHC(b)
	if err != nil {
		return 0, nil, err
	}
	return b[2], b[i:], nil // the inline next header follows the two IPHC bytes
}

// parseRA decodes a Router Advertisement and the three options the bridge sends.
func parseRA(msg []byte) (*RouterAdvert, error) {
	if len(msg) < raHeaderLen || msg[0] != icmpRouterAdvert {
		return nil, ErrNotRA
	}
	ra := &RouterAdvert{
		HopLimit:       msg[4],
		Flags:          msg[5],
		RouterLifetime: binary.BigEndian.Uint16(msg[6:8]),
	}
	for i := raHeaderLen; i+2 <= len(msg); {
		n := int(msg[i+1]) * 8
		if n < 8 || i+n > len(msg) {
			return nil, ErrNotRA
		}
		v := msg[i+2 : i+n]
		switch {
		case msg[i] == optSLLAO && len(v) >= 8:
			ra.SLLAO = append([]byte{}, v[:8]...)
		case msg[i] == optMTU && len(v) >= 6:
			ra.MTU = binary.BigEndian.Uint32(v[2:6])
		case msg[i] == optIPv4 && len(v) >= 38:
			ra.Netmask = binary.BigEndian.Uint32(v[2:6])
			ra.Gateway = ipv4Mapped(v[6:22])
			ra.DNS = ipv4Mapped(v[22:38])
		}
		i += n
	}
	return ra, nil
}

// ipv4Mapped returns the 4 IPv4 bytes of a ::ffff:a.b.c.d address, nil otherwise.
func ipv4Mapped(a []byte) []byte {
	if len(a) != 16 || a[10] != 0xff || a[11] != 0xff {
		return nil
	}
	return append([]byte{}, a[12:16]...)
}

// BuildRouterAdvert serializes an RA as a compressed IPv6 packet to ff02::1,
// byte for byte as the bridge emits it (checksum included). srcEUI is the 8-byte
// wire address the ICMPv6 checksum's pseudo-header is built from.
func BuildRouterAdvert(srcEUI []byte, ra RouterAdvert) ([]byte, error) {
	if len(srcEUI) != 8 || len(ra.SLLAO) != 8 || len(ra.Gateway) != 4 || len(ra.DNS) != 4 {
		return nil, errors.New("tado: RA needs an 8-byte srcEUI and SLLAO and 4-byte Gateway and DNS")
	}
	msg := make([]byte, 0, 80)
	msg = append(msg, icmpRouterAdvert, 0, 0, 0) // type, code, checksum placeholder
	msg = append(msg, ra.HopLimit, ra.Flags)
	msg = binary.BigEndian.AppendUint16(msg, ra.RouterLifetime)
	msg = append(msg, make([]byte, 8)...) // reachable time, retransmit timer
	msg = append(msg, optSLLAO, 2)
	msg = append(msg, ra.SLLAO...)
	msg = append(msg, make([]byte, 6)...) // pad the option to 16 bytes
	msg = append(msg, optMTU, 1, 0, 0)
	msg = binary.BigEndian.AppendUint32(msg, ra.MTU)
	msg = append(msg, optIPv4, 5, 0, 0)
	msg = binary.BigEndian.AppendUint32(msg, ra.Netmask)
	msg = append(msg, mapIPv4(ra.Gateway)...)
	msg = append(msg, mapIPv4(ra.DNS)...)

	ck := icmpChecksumV6(linkLocalFromWire(srcEUI), allNodesMcast(), msg)
	binary.BigEndian.PutUint16(msg[2:4], ck)
	return append([]byte{iphcMcast0, iphcMcast1, nhICMPv6, 0x01}, msg...), nil
}

// BridgeRouterAdvert is the configuration the real bridge advertises, for the
// 8-byte wire (little-endian) EUI of the sender. The IPv4 values are its LAN's,
// so they belong to the captured site, not to the protocol.
func BridgeRouterAdvert(wireEUI []byte) RouterAdvert {
	return RouterAdvert{
		HopLimit:       64,
		Flags:          0x40, // Other-configuration
		RouterLifetime: 1800,
		SLLAO:          euiBigEndian(wireEUI),
		MTU:            1500,
		Netmask:        0xffffffff,
		Gateway:        []byte{192, 168, 5, 1},
		DNS:            []byte{8, 8, 8, 8},
	}
}

// WrapBroadcastPayload reserves the MAC FCS trailer. BuildBroadcast fills it.
func WrapBroadcastPayload(packet []byte, counterHigh uint16) []byte {
	pt := binary.LittleEndian.AppendUint16(make([]byte, 0, len(packet)+4), counterHigh)
	return append(append(pt, packet...), 0x00, 0x00)
}

// euiBigEndian reverses a wire (little-endian) EUI-64 into its printed order.
func euiBigEndian(wire []byte) []byte {
	be := make([]byte, len(wire))
	for i := range wire {
		be[len(wire)-1-i] = wire[i]
	}
	return be
}

// allNodesMcast is ff02::1.
func allNodesMcast() []byte {
	a := make([]byte, 16)
	a[0], a[1], a[15] = 0xff, 0x02, 0x01
	return a
}

// mapIPv4 returns the ::ffff:a.b.c.d form of a 4-byte IPv4 address.
func mapIPv4(v4 []byte) []byte {
	a := make([]byte, 16)
	a[10], a[11] = 0xff, 0xff
	copy(a[12:], v4)
	return a
}

// icmpChecksumV6 is the RFC 4443 checksum over the IPv6 pseudo-header and the
// ICMPv6 message, whose own checksum field must be zero.
func icmpChecksumV6(src, dst, msg []byte) uint16 {
	buf := make([]byte, 0, 40+len(msg))
	buf = append(buf, src...)
	buf = append(buf, dst...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(msg)))
	buf = append(buf, 0, 0, 0, nhICMPv6)
	buf = append(buf, msg...)

	var sum uint32
	for i := 0; i+1 < len(buf); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(buf[i : i+2]))
	}
	if len(buf)%2 == 1 {
		sum += uint32(buf[len(buf)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
