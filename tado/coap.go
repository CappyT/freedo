package tado

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/udp/coder"
)

// The decrypted app payload of a Data frame is a compressed IPv6 packet:
//
//	[c51b00][04][ctr:4 LE] | 6LoWPAN(IPHC + UDP-NHC) + CoAP | [trailer:2]
//
// It is the same Contiki stack (sicslowpan + Erbium CoAP) the firmware runs, and
// the same CoAP+TLV application protocol as the cloud link. The block that older
// notes called an opaque "meta"/"header" is the compressed IPv6/UDP header plus
// the CoAP message header and options; the TLV list is the CoAP payload.
//
// This file locates and parses the CoAP message and reconstructs the elided IPv6
// addresses, which the UDP checksum needs.

// Context0Prefix is the /64 that 6LoWPAN context 0 stands for on this link.
//
// The zone traffic (the "lid=" resources) compresses both endpoints against that
// context instead of fe80::/64, so a downlink checksum needs the prefix. Two
// independent sources give the same value:
//
//   - Firmware. sicslowpan_init (bridge STM32 @ 0x00029dc8) writes 0xaa twice into
//     addr_contexts[0].prefix at 0x20002732 and leaves the remaining six bytes at
//     their .bss zero. Nothing else ever writes the table, and the firmware parses
//     no RA 6CO option, so context 0 is fixed. A live SRAM image at 0x20002730
//     reads 01 00 aa aa 00 00 00 00 00 00, and a captured uIP buffer holds the
//     bridge's own global address aaaa::21b:c507:fade:a304.
//   - Captures. The UDP checksum of a context-compressed packet depends on the
//     prefix only through its one's-complement sum, which every one of the 366
//     lid= frames of coap_sniff_1786619225/1786609624 puts at 0xaaaa. See
//     TestContext0PrefixVerifiesCapturedChecksums.
var Context0Prefix = mustHex("aaaa000000000000")

// 6LoWPAN context indices, as IPHCAddr.Ctx reports them.
const (
	// ctxStateless means the address carries no context: fe80::/64.
	ctxStateless = -1
	// ctxUnusable marks a form this link never uses (multicast, unspecified,
	// 16-bit short), which IPHCAddr.IPv6 cannot expand.
	ctxUnusable = -2
)

// IPHCAddr is one endpoint of a 6LoWPAN-compressed packet: the prefix it is
// compressed against, and its interface identifier when the header carried one.
type IPHCAddr struct {
	Ctx int    // ctxStateless, ctxUnusable, or a 6LoWPAN context index
	IID []byte // 8 bytes, big-endian; nil when the header elided it (take it from L2)
}

// IPv6 expands the address to its 16 bytes. wireEUI is the little-endian L2
// address of the endpoint, used only when the header elided the IID. It returns
// nil for an address this link's compression forms cannot express.
func (a IPHCAddr) IPv6(wireEUI []byte) []byte {
	iid := a.IID
	if iid == nil {
		iid = modifiedEUI64(wireEUI)
	}
	if len(iid) != 8 {
		return nil
	}
	var prefix []byte
	switch a.Ctx {
	case ctxStateless:
		prefix = []byte{0xfe, 0x80, 0, 0, 0, 0, 0, 0}
	case 0:
		prefix = Context0Prefix
	default:
		return nil // the bridge defines context 0 only
	}
	return append(append(make([]byte, 0, 16), prefix...), iid...)
}

// modifiedEUI64 turns a wire (little-endian) EUI-64 into the interface
// identifier an IPv6 address carries: reversed, with the U/L bit flipped. The
// wire address is already 8 bytes, so there is no RFC 4291 fffe insertion.
func modifiedEUI64(wireEUI []byte) []byte {
	if len(wireEUI) != 8 {
		return nil
	}
	iid := make([]byte, 8)
	for i := range iid {
		iid[i] = wireEUI[7-i]
	}
	iid[0] ^= 0x02
	return iid
}

// CoAPOption is one CoAP option (RFC 7252): a number and its raw value.
type CoAPOption struct {
	Num int
	Val []byte
}

// CoAPMessage is a parsed CoAP message carried on the 868 link.
type CoAPMessage struct {
	Type     string // CON, NON, ACK, RST
	Code     string // e.g. "0.03", "2.04"
	MID      uint16
	Token    []byte
	SrcPort  uint16
	DstPort  uint16
	Src      IPHCAddr // source address as the 6LoWPAN header left it
	Dst      IPHCAddr // destination address as the 6LoWPAN header left it
	Checksum uint16   // inline UDP checksum; 0 when the NHC header elided it
	Raw      []byte   // the serialized message, i.e. the bytes the checksum covers
	Options  []CoAPOption
	Path     string // Uri-Path (option 11) joined by "/"
	Query    string // Uri-Query (option 15) joined by "&"
	Payload  []byte // bytes after the 0xff marker = the TLV list
}

// Contexted reports whether both endpoints are compressed against a 6LoWPAN
// address context rather than the link-local prefix. That is the "lid=" zone
// path; the plain link-local one is what a device uses to talk to the bridge.
func (m *CoAPMessage) Contexted() bool { return m.Src.Ctx >= 0 && m.Dst.Ctx >= 0 }

// LinkLocal reports whether both endpoints are plain fe80::/64 addresses.
func (m *CoAPMessage) LinkLocal() bool {
	return m.Src.Ctx == ctxStateless && m.Dst.Ctx == ctxStateless
}

// RecomputeChecksum returns the UDP checksum the message's addresses and bytes
// imply. srcEUI and dstEUI are the wire (little-endian) L2 addresses of the frame
// that carried it; they supply whichever IID the header elided. It reports false
// when an address cannot be expanded.
//
// Raw holds one datagram's bytes, so this is meaningful only for a message that
// arrived whole: a reassembled one covers more than its first fragment carried.
func (m *CoAPMessage) RecomputeChecksum(srcEUI, dstEUI []byte) (uint16, bool) {
	src, dst := m.Src.IPv6(srcEUI), m.Dst.IPv6(dstEUI)
	if src == nil || dst == nil {
		return 0, false
	}
	return udpChecksumV6(src, dst, m.SrcPort, m.DstPort, m.Raw), true
}

var coapTypes = [4]string{"CON", "NON", "ACK", "RST"}

// ParseCoAP decodes the 6LoWPAN(IPHC+UDP-NHC)+CoAP packet that sits after the
// [c51b00][04][ctr] prefix in a decrypted plaintext. Pass pt[8:] (the packet
// plus the 2-byte app trailer). It returns ErrNoCoAP when the bytes are not a
// recognizable IPHC/UDP/CoAP packet (e.g. a synthetic placeholder meta).
func ParseCoAP(body []byte) (*CoAPMessage, error) {
	if len(body) < 8 {
		return nil, ErrNoCoAP
	}
	return parseCoAPCore(body[:len(body)-2]) // drop the 2-byte app trailer
}

// parseCoAPCore parses a 6LoWPAN(IPHC+UDP-NHC)+CoAP datagram with no trailing bytes.
// Used both by ParseCoAP (after stripping the trailer) and by fragment reassembly.
func parseCoAPCore(core []byte) (*CoAPMessage, error) {
	src, dst, i, err := parseIPHC(core)
	if err != nil {
		return nil, err
	}
	ck, _ := udpChecksumInline(core, i)
	udp, i, err := parseUDPNHC(core, i)
	if err != nil {
		return nil, err
	}
	m, err := parseCoAPMessage(core[i:])
	if err != nil {
		return nil, err
	}
	m.SrcPort, m.DstPort = udp[0], udp[1]
	m.Src, m.Dst, m.Checksum = src, dst, ck
	m.Raw = append([]byte{}, core[i:]...)
	return m, nil
}

// ErrNoCoAP means the plaintext body is not an IPHC/UDP/CoAP packet.
var ErrNoCoAP = errors.New("tado: not a 6LoWPAN/CoAP packet")

// skipIPHC returns the offset where the next header (UDP-NHC) begins.
func skipIPHC(b []byte) (int, error) {
	_, _, i, err := parseIPHC(b)
	return i, err
}

// parseIPHC parses a LOWPAN_IPHC header (RFC 6282). It returns both endpoint
// addresses as the header left them and the offset where the next header begins.
func parseIPHC(b []byte) (src, dst IPHCAddr, n int, err error) {
	if len(b) < 2 || b[0]&0xe0 != 0x60 { // dispatch 011xxxxx
		return src, dst, 0, ErrNoCoAP
	}
	b0, b1 := b[0], b[1]
	tf := (b0 >> 3) & 3
	nh := (b0 >> 2) & 1
	hlim := b0 & 3
	cid := (b1 >> 7) & 1
	sac := (b1 >> 6) & 1
	sam := (b1 >> 4) & 3
	m := (b1 >> 3) & 1
	dac := (b1 >> 2) & 1
	dam := b1 & 3
	i := 2
	var sci, dci byte
	if cid == 1 { // SCI/DCI context byte
		if i >= len(b) {
			return src, dst, 0, ErrNoCoAP
		}
		sci, dci = b[i]>>4, b[i]&0xf
		i++
	}
	switch tf { // Traffic Class + Flow Label inline bytes
	case 0:
		i += 4
	case 1:
		i += 3
	case 2:
		i++
	}
	if nh == 0 {
		i++ // in-line Next Header (not NHC)
	}
	if hlim == 0 {
		i++ // in-line Hop Limit
	}
	src, i = readIPHCAddr(b, i, srcInline(sac, sam), sac, sam, sci)
	if m == 1 { // a multicast destination is never an endpoint we answer
		dst, i = IPHCAddr{Ctx: ctxUnusable}, i+dstInline(dac, dam, m)
	} else {
		dst, i = readIPHCAddr(b, i, dstInline(dac, dam, m), dac, dam, dci)
	}
	if i > len(b) {
		return src, dst, 0, ErrNoCoAP
	}
	return src, dst, i, nil
}

// readIPHCAddr describes one address field and steps over its n inline bytes. It
// never fails: a form it cannot expand yields ctxUnusable, and the caller's
// bounds check catches a truncated header.
func readIPHCAddr(b []byte, i, n int, ac, am, ctx byte) (IPHCAddr, int) {
	a := IPHCAddr{Ctx: ctxUnusable}
	switch {
	case am == 3: // IID elided, reconstructed from the L2 address
		a.Ctx = addrPrefixCtx(ac, ctx)
	case am == 1 && n == 8 && i+n <= len(b): // 64-bit IID inline
		a = IPHCAddr{Ctx: addrPrefixCtx(ac, ctx), IID: append([]byte{}, b[i:i+n]...)}
	}
	return a, i + n
}

// addrPrefixCtx maps the address-compression flag to the prefix source: stateless
// compression is link-local, stateful names a 6LoWPAN context.
func addrPrefixCtx(ac, ctx byte) int {
	if ac == 0 {
		return ctxStateless
	}
	return int(ctx)
}

// srcInline returns the inline byte count of the source address for (SAC, SAM).
func srcInline(ac, am byte) int {
	if ac == 0 { // stateless
		switch am {
		case 0:
			return 16
		case 1:
			return 8
		case 2:
			return 2
		}
		return 0 // am==3: derived from L2
	}
	switch am { // stateful/context
	case 1:
		return 8
	case 2:
		return 2
	}
	return 0 // am==0 (unspecified) or am==3 (from L2)
}

// dstInline returns the inline byte count of the destination address.
func dstInline(ac, am, mcast byte) int {
	if mcast == 0 {
		return srcInline(ac, am)
	}
	if ac == 0 { // stateless multicast
		switch am {
		case 0:
			return 16
		case 1:
			return 6
		case 2:
			return 4
		case 3:
			return 1
		}
	}
	if am == 0 { // stateful multicast
		return 6
	}
	return 0
}

// parseUDPNHC parses an LOWPAN_NHC UDP header. Returns [srcPort, dstPort] and the
// offset where the CoAP message begins.
func parseUDPNHC(b []byte, i int) ([2]uint16, int, error) {
	if i >= len(b) || b[i]&0xf8 != 0xf0 { // 11110CPP
		return [2]uint16{}, 0, ErrNoCoAP
	}
	n := b[i]
	i++
	pp := n & 3
	if i+udpPortBytes(pp) > len(b) {
		return [2]uint16{}, 0, ErrNoCoAP
	}
	var sp, dp uint16
	switch pp {
	case 0:
		sp = binary.BigEndian.Uint16(b[i : i+2])
		dp = binary.BigEndian.Uint16(b[i+2 : i+4])
	case 1:
		sp = binary.BigEndian.Uint16(b[i : i+2])
		dp = 0xf000 | uint16(b[i+2])
	case 2:
		sp = 0xf000 | uint16(b[i])
		dp = binary.BigEndian.Uint16(b[i+1 : i+3])
	case 3:
		sp = 0xf0b0 | uint16(b[i]>>4)
		dp = 0xf0b0 | uint16(b[i]&0xf)
	}
	i += udpPortBytes(pp)
	if (n>>2)&1 == 0 {
		i += 2 // in-line UDP checksum
	}
	if i > len(b) {
		return [2]uint16{}, 0, ErrNoCoAP
	}
	return [2]uint16{sp, dp}, i, nil
}

// udpPortBytes is the inline port byte count of an LOWPAN_NHC UDP header for its
// PP field: both ports, one compressed, the other compressed, or both nibbles.
func udpPortBytes(pp byte) int {
	switch pp {
	case 0:
		return 4
	case 3:
		return 1
	}
	return 3
}

// udpChecksumInline returns the checksum an LOWPAN_NHC UDP header at offset i
// carries, and false when the header elided it (C=1) or is truncated.
func udpChecksumInline(b []byte, i int) (uint16, bool) {
	if i >= len(b) || b[i]&0xf8 != 0xf0 || (b[i]>>2)&1 == 1 {
		return 0, false
	}
	off := i + 1 + udpPortBytes(b[i]&3)
	if off+2 > len(b) {
		return 0, false
	}
	return binary.BigEndian.Uint16(b[off : off+2]), true
}

// parseCoAPMessage decodes a CoAP RFC 7252 message with go-coap's UDP coder.
//
// The coder keeps an option it has no definition for as opaque bytes, which is
// what the proprietary tado option 2048 needs, and it never rewrites a value it
// does know, so the sentinel Uri-Port ffff comes back verbatim.
func parseCoAPMessage(b []byte) (*CoAPMessage, error) {
	msg, err := decodeCoAP(b)
	if err != nil {
		return nil, ErrNoCoAP
	}
	m := &CoAPMessage{
		Type:  coapTypes[msg.Type&3],
		Code:  formatCode(byte(msg.Code)),
		MID:   uint16(msg.MessageID),
		Token: append([]byte{}, msg.Token...),
	}
	var path, query []string
	for _, o := range msg.Options {
		switch o.ID {
		case message.URIPath:
			path = append(path, string(o.Value))
		case message.URIQuery:
			query = append(query, string(o.Value))
		default:
			m.Options = append(m.Options, CoAPOption{Num: int(o.ID), Val: append([]byte{}, o.Value...)})
		}
	}
	if len(msg.Payload) > 0 {
		m.Payload = append([]byte{}, msg.Payload...)
	}
	m.Path = strings.Join(path, "/")
	m.Query = strings.Join(query, "&")
	return m, nil
}

// decodeCoAP unmarshals one CoAP message. The coder never grows the option
// slice, it fails with ErrOptionsTooSmall instead, so retry with a bigger one.
func decodeCoAP(b []byte) (message.Message, error) {
	for n := 32; ; n *= 2 {
		m := message.Message{Options: make(message.Options, 0, n)}
		_, err := coder.DefaultCoder.Decode(b, &m)
		if errors.Is(err, message.ErrOptionsTooSmall) && n < 1024 {
			continue
		}
		return m, err
	}
}

// --- context-0 (zone / "lid=") downlink builder ---
//
// The zone path uses a different IPHC encoding from the link-local one in
// build_coap.go. Verified against the 124 captured bridge answers to a direct
// zone uplink, which are byte-identical to what BuildContextAck emits:
//
//	IPHC     7ef7  TF=3 (elided), NH=1 (NHC), HLIM=2 (64), CID=1,
//	               SAC=1/SAM=3 and DAC=1/DAM=3 (both from the L2 EUI-64,
//	               both prefixed with the context the CID byte names)
//	CID      00    SCI=0, DCI=0: both endpoints use context 0
//	UDP-NHC  f0    C=0 (inline 2-byte checksum), PP=0 (both ports inline)
//	ports    5683 -> 5683
const (
	iphcContext0 = 0x7e // TF=3, NH=NHC, HLIM=64
	iphcContext1 = 0xf7 // CID=1, SAC=1/SAM=3, DAC=1/DAM=3
	cidBothCtx0  = 0x00 // SCI=0, DCI=0
)

// BuildContextCoAP serializes a CoAP datagram addressed under 6LoWPAN context 0,
// the mode the zone resources use. Both IIDs come from the L2 addresses, so the
// checksum is computed over Context0Prefix + each node's modified EUI-64. The
// output is the packet only; wrap it with WrapAppPayload and frame it with
// BuildData.
func BuildContextCoAP(r CoAPReq) ([]byte, error) {
	msg, err := buildCoAPMessage(r)
	if err != nil {
		return nil, err
	}
	src := r.SrcEUI
	if src == nil {
		src = BridgeEUI
	}
	if len(src) != 8 || len(r.DstEUI) != 8 {
		return nil, errors.New("tado: context-mode CoAP needs 8-byte SrcEUI and DstEUI")
	}
	sp, dp := r.SrcPort, r.DstPort
	if sp == 0 {
		sp = coapPort
	}
	if dp == 0 {
		dp = coapPort
	}
	out := make([]byte, 0, 9+len(msg))
	out = append(out, iphcContext0, iphcContext1, cidBothCtx0, nhcUDPPortsIn)
	out = binary.BigEndian.AppendUint16(out, sp)
	out = binary.BigEndian.AppendUint16(out, dp)
	ck := udpChecksumV6(contextAddr(src), contextAddr(r.DstEUI), sp, dp, msg)
	out = binary.BigEndian.AppendUint16(out, ck)
	return append(out, msg...), nil
}

// BuildContextAck builds the answer to a zone uplink: an ACK echoing the
// request's MID and token, carrying the Content-Format option the bridge always
// sets. The addressing arguments are the response's own, so the caller passes the
// uplink's src/dst and ports swapped.
func BuildContextAck(code string, mid uint16, token, srcEUI, dstEUI []byte, srcPort, dstPort uint16) ([]byte, error) {
	if mid == 0 {
		return nil, errors.New("tado: ACK needs the MID of the request it answers")
	}
	if token == nil {
		token = []byte{} // an ACK echoes the request token verbatim, empty included
	}
	return BuildContextCoAP(CoAPReq{
		Type:    "ACK",
		Code:    code,
		MID:     mid,
		Token:   token,
		Options: []CoAPOption{{Num: 12, Val: []byte{42}}}, // application/octet-stream
		SrcEUI:  srcEUI,
		DstEUI:  dstEUI,
		SrcPort: srcPort,
		DstPort: dstPort,
	})
}

// contextAddr is a node's address under 6LoWPAN context 0.
func contextAddr(wireEUI []byte) []byte {
	iid := modifiedEUI64(wireEUI)
	if iid == nil {
		return nil
	}
	return append(append(make([]byte, 0, 16), Context0Prefix...), iid...)
}

func formatCode(c byte) string {
	return string(rune('0'+(c>>5))) + "." + twoDigit(int(c&0x1f))
}

func twoDigit(n int) string {
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

// Option returns the first option with the given number, if present.
func (m *CoAPMessage) Option(num int) (CoAPOption, bool) {
	for _, o := range m.Options {
		if o.Num == num {
			return o, true
		}
	}
	return CoAPOption{}, false
}

// URI returns the resource as "path?query" (query omitted when empty).
func (m *CoAPMessage) URI() string {
	if m.Query == "" {
		return m.Path
	}
	return m.Path + "?" + m.Query
}

// String is a one-line CoAP summary for the dissector.
func (m *CoAPMessage) String() string {
	s := m.Type + " " + m.Code + " " + m.URI()
	if len(m.Payload) > 0 {
		s += " pl=" + hex.EncodeToString(m.Payload)
	}
	return s
}

// Summary renders the message header for one log line: type, message id, token,
// code, URI, then every option the URI does not already carry. A capture pairs a
// request with its response by the message id, and a block-wise transfer by the
// ETag and the block option, so all three stay on the line. The token prints as
// "-" when the message carries none, which keeps the field in place.
func (m *CoAPMessage) Summary() string {
	tok := "-"
	if len(m.Token) > 0 {
		tok = hex.EncodeToString(m.Token)
	}
	s := fmt.Sprintf("%s mid=0x%04x tok=%s %s", m.Type, m.MID, tok, m.Code)
	if u := m.URI(); u != "" {
		s += " " + u
	}
	for _, o := range m.Options {
		s += " " + o.Label()
	}
	return s
}

// PayloadStartsDocument reports whether the payload begins the representation of
// the resource. An RFC 7959 transfer past block 0 carries a slice that can cut a
// TLV in half, so a TLV reader must not run on it.
func (m *CoAPMessage) PayloadStartsDocument() bool {
	for _, o := range m.Options {
		switch message.OptionID(o.Num) {
		case message.Block1, message.Block2:
			if o.Uint()>>4 != 0 {
				return false
			}
		}
	}
	return true
}

// Uint reads the option value as the RFC 7252 network-order unsigned integer.
func (o CoAPOption) Uint() uint64 {
	var v uint64
	for _, b := range o.Val {
		v = v<<8 | uint64(b)
	}
	return v
}

// Label renders one option for a log line. The options this link uses get a
// short name; every other option prints as o<number>=<hex>, which is what the
// proprietary tado option 2048 needs.
func (o CoAPOption) Label() string {
	switch message.OptionID(o.Num) {
	case message.ETag:
		return "etag=" + hex.EncodeToString(o.Val)
	case message.Observe:
		return "obs=" + strconv.FormatUint(o.Uint(), 10)
	case message.URIPort:
		return "port=" + strconv.FormatUint(o.Uint(), 10)
	case message.ContentFormat:
		return "cf=" + strconv.FormatUint(o.Uint(), 10)
	case message.MaxAge:
		return "maxage=" + strconv.FormatUint(o.Uint(), 10)
	case message.Size1:
		return "size1=" + strconv.FormatUint(o.Uint(), 10)
	case message.Size2:
		return "size2=" + strconv.FormatUint(o.Uint(), 10)
	case message.Block1:
		return "blk1=" + o.blockLabel()
	case message.Block2:
		return "blk2=" + o.blockLabel()
	}
	return "o" + strconv.Itoa(o.Num) + "=" + hex.EncodeToString(o.Val)
}

// blockLabel renders an RFC 7959 block option as number/more/size.
func (o CoAPOption) blockLabel() string {
	v := o.Uint()
	return fmt.Sprintf("%d/%d/%d", v>>4, (v>>3)&1, 1<<((v&7)+4))
}
