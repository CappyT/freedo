package tado

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sort"
	"strconv"
	"sync/atomic"

	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/udp/coder"
)

// Builder side of coap.go: serialize a 6LoWPAN(IPHC + UDP-NHC) + CoAP datagram
// from scratch, so a downlink no longer has to copy a captured meta block.
//
// The encoding reproduces what the real bridge emits. Verified against every
// captured "z/s?id=N" downlink in testdata/frames_69ec.txt:
//
//	IPHC     7e33  TF=3 (elided), NH=1 (NHC), HLIM=2 (64), CID=0,
//	               SAC=0/SAM=3 and DAC=0/DAM=3 (both link-local, from the L2 EUI-64)
//	UDP-NHC  f0    C=0 (inline 2-byte checksum), PP=0 (both ports inline)
//	ports    4005 -> 5683
//	CoAP     CON 0.03 (PUT), options 7=ffff, 11="z", 11="s", 12=42, 15="id=N"
//
// The other captured modes (7ef5/7ef7/7cd7) compress the addresses against
// 6LoWPAN context 0 and carry a relayed source, so their checksum cannot be
// recomputed from the L2 header alone. The builder only emits the link-local
// mode, which is the one the bridge uses for its own commands.
const (
	iphcLinkLocal0 = 0x7e // TF=3, NH=NHC, HLIM=64
	iphcLinkLocal1 = 0x33 // SAC=0/SAM=3, DAC=0/DAM=3
	nhcUDPPortsIn  = 0xf0 // 11110CPP with C=0, PP=0
	nhcUDPElideCk  = 0xf4 // same with C=1

	coapPort = 5683 // device-side CoAP server
	// bridgeCoAPPort is the source port the bridge uses for its own requests.
	bridgeCoAPPort = 4005
)

// coapSessionOption is the proprietary option that carries the session id of the
// node that authored the message. Its number is above every standard option, so
// it always encodes last, right before the payload marker.
const coapSessionOption = 2048

// CoAPReq describes a CoAP message to serialize over 6LoWPAN(IPHC + UDP-NHC).
type CoAPReq struct {
	Type          string       // "CON","NON","ACK","RST"; "" => "CON"
	Code          string       // "0.01".."2.05" dotted form (see CoAPCode)
	MID           uint16       // sent verbatim, 0 included, unless GenerateMID
	GenerateMID   bool         // mint the next MID instead of using MID
	Token         []byte       // nil => generated (>=1 byte)
	Path          []string     // Uri-Path segments (option 11)
	Query         []string     // Uri-Query items (option 15), e.g. "id=3"
	Options       []CoAPOption // extra options (7 Uri-Port, 12 Content-Format, 2048 tag, ...) merged+sorted by Num
	Payload       []byte       // raw payload bytes (TLV list); 0xFF marker added when non-empty
	SrcEUI        []byte       // 8B wire EUI (src) for IPv6 link-local + UDP checksum; nil => BridgeEUI
	DstEUI        []byte       // 8B wire EUI (dst); required for a correct checksum
	SrcPort       uint16       // 0 => 5683
	DstPort       uint16       // 0 => 5683
	ElideChecksum bool         // UDP-NHC C=1 (no inline checksum)
}

// BuildCoAP serializes a full 6LoWPAN(IPHC+UDP-NHC)+CoAP datagram. The output
// is the packet only (no Tado [c51b00][04][ctr] shim, no app trailer) so it can
// be wrapped by WrapAppPayload and framed by BuildData.
func BuildCoAP(r CoAPReq) ([]byte, error) {
	msg, err := buildCoAPMessage(r)
	if err != nil {
		return nil, err
	}
	src := r.SrcEUI
	if src == nil {
		src = BridgeEUI
	}
	if len(src) != 8 {
		return nil, errors.New("tado: SrcEUI must be 8 bytes")
	}
	if len(r.DstEUI) != 8 && !(r.ElideChecksum && r.DstEUI == nil) {
		return nil, errors.New("tado: DstEUI must be 8 bytes")
	}
	sp, dp := r.SrcPort, r.DstPort
	if sp == 0 {
		sp = coapPort
	}
	if dp == 0 {
		dp = coapPort
	}

	out := make([]byte, 0, 9+len(msg))
	out = append(out, iphcLinkLocal0, iphcLinkLocal1)
	if r.ElideChecksum {
		out = append(out, nhcUDPElideCk)
	} else {
		out = append(out, nhcUDPPortsIn)
	}
	out = binary.BigEndian.AppendUint16(out, sp)
	out = binary.BigEndian.AppendUint16(out, dp)
	if !r.ElideChecksum {
		ck := udpChecksumV6(linkLocalFromWire(src), linkLocalFromWire(r.DstEUI), sp, dp, msg)
		out = binary.BigEndian.AppendUint16(out, ck)
	}
	return append(out, msg...), nil
}

// WrapAppPayload reserves the MAC FCS trailer. BuildData fills it.
func WrapAppPayload(coapPacket []byte, counter uint32, trailer []byte) []byte {
	if trailer == nil {
		trailer = []byte{0x00, 0x00}
	}
	ctr := make([]byte, 4)
	binary.LittleEndian.PutUint32(ctr, counter)
	return concat(EUISuffix, []byte{0x04}, ctr, coapPacket, trailer)
}

// BuildCoAPAck builds an ACK response (2.04 Changed / 2.05 Content / 2.03 Valid)
// to a device uplink, echoing its MID and token. The addressing arguments are
// the response's own: the caller passes the uplink's src/dst and ports swapped.
func BuildCoAPAck(code string, mid uint16, token []byte, payload []byte, srcEUI, dstEUI []byte, srcPort, dstPort uint16, extra ...CoAPOption) ([]byte, error) {
	r := CoAPReq{
		Type:    "ACK",
		Code:    code,
		MID:     mid,
		Token:   token,
		Payload: payload,
		SrcEUI:  srcEUI,
		DstEUI:  dstEUI,
		SrcPort: srcPort,
		DstPort: dstPort,
	}
	if token == nil {
		r.Token = []byte{} // an ACK echoes the request token verbatim, empty included
	}
	if len(payload) > 0 {
		r.Options = []CoAPOption{{Num: 12, Val: []byte{42}}} // application/octet-stream
	}
	r.Options = append(r.Options, extra...)
	return BuildCoAP(r)
}

// --- zone-plane forwarding ---
//
// On the "lid=" plane a device addresses its zone peer's IPv6 address directly
// and uses the bridge only as the L2 next hop, so the bridge forwards rather
// than answers: it keeps both endpoints, decrements the hop limit and leaves the
// UDP checksum alone. Verified byte for byte against every relayed pair in the
// capture, see TestBuildZoneRelayMatchesCapture:
//
//	IPHC     7cd7  TF=3 (elided), NH=1 (NHC), HLIM=0 (inline: 63 no longer fits
//	               a compressed value), CID=1, SAC=1/SAM=1 (source IID inline,
//	               it is no longer the L2 sender), DAC=1/DAM=3 (destination from
//	               the new L2 address)
//	CID      00    SCI=0, DCI=0: both endpoints on context 0
//	hop      3f    63, one hop spent
//	UDP-NHC  f0    inline checksum, both ports inline
const (
	iphcRelay0    = 0x7c
	iphcRelay1    = 0xd7
	relayHopLimit = 0x3f
)

// BuildZoneRelay re-emits a context-0 datagram on a second L2 hop. msg is the
// CoAP message verbatim (CoAPMessage.Raw), so the Message-ID, the token, the
// options and the payload survive untouched; srcIID is the 8-byte modified
// EUI-64 of the IPv6 source to preserve and dstEUI the wire EUI of the node the
// frame is forwarded to. The checksum is recomputed from those two addresses,
// which reproduces the sender's own whenever the destination is unchanged.
func BuildZoneRelay(msg, srcIID, dstEUI []byte, srcPort, dstPort uint16) ([]byte, error) {
	if len(msg) == 0 {
		return nil, errors.New("tado: zone relay carries no CoAP message")
	}
	if len(srcIID) != 8 || len(dstEUI) != 8 {
		return nil, errors.New("tado: zone relay needs an 8-byte source IID and destination EUI")
	}
	sp, dp := srcPort, dstPort
	if sp == 0 {
		sp = coapPort
	}
	if dp == 0 {
		dp = coapPort
	}
	src := append(append(make([]byte, 0, 16), Context0Prefix...), srcIID...)
	out := make([]byte, 0, 15+len(msg))
	out = append(out, iphcRelay0, iphcRelay1, cidBothCtx0, relayHopLimit)
	out = append(out, srcIID...)
	out = append(out, nhcUDPPortsIn)
	out = binary.BigEndian.AppendUint16(out, sp)
	out = binary.BigEndian.AppendUint16(out, dp)
	out = binary.BigEndian.AppendUint16(out, udpChecksumV6(src, contextAddr(dstEUI), sp, dp, msg))
	return append(out, msg...), nil
}

// StampSessionID inserts option 2048 set to id into a CoAP message. It returns
// msg untouched when id is empty or the message already carries the option,
// which is the normal case on the relay path: a device stamps its own session
// id and the bridge forwards those bytes verbatim.
//
// The insertion splices the serialized message and decodes no option value, so
// every byte of the original stays, malformed options included. Only the header
// of the option that follows 2048, if any, is re-encoded for its new delta.
func StampSessionID(msg, id []byte) ([]byte, error) {
	if len(id) == 0 {
		return msg, nil
	}
	opts, end, err := walkCoAPOptions(msg)
	if err != nil {
		return nil, err
	}
	prev, next := 0, len(opts)
	for i, o := range opts {
		if o.num == coapSessionOption {
			return msg, nil
		}
		if o.num > coapSessionOption {
			next = i
			break
		}
		prev = o.num
	}
	at := end
	if next < len(opts) {
		at = opts[next].start
	}
	out := make([]byte, 0, len(msg)+len(id)+8)
	out = append(out, msg[:at]...)
	out = appendCoAPOptionHeader(out, coapSessionOption-prev, len(id))
	out = append(out, id...)
	if next < len(opts) {
		o := opts[next]
		out = appendCoAPOptionHeader(out, o.num-coapSessionOption, o.end-o.val)
		at = o.val
	}
	return append(out, msg[at:]...), nil
}

// BuildContextAckBody is BuildContextAck carrying a body: the answer to a zone
// GET the bridge serves from its own cache. The extra options follow the
// Content-Format one, so the caller passes them in ascending option order.
func BuildContextAckBody(code string, mid uint16, token, payload, srcEUI, dstEUI []byte, srcPort, dstPort uint16, extra ...CoAPOption) ([]byte, error) {
	if token == nil {
		token = []byte{}
	}
	return BuildContextCoAP(CoAPReq{
		Type:    "ACK",
		Code:    code,
		MID:     mid,
		Token:   token,
		Options: append([]CoAPOption{{Num: 12, Val: []byte{42}}}, extra...), // application/octet-stream
		Payload: payload,
		SrcEUI:  srcEUI,
		DstEUI:  dstEUI,
		SrcPort: srcPort,
		DstPort: dstPort,
	})
}

// Fixed control tags of every captured "z/s" block. Tags 6160 and 61e0 carry
// the home or away state of the whole home, the same pair on every zone.
const (
	zoneCtlHome  = 0x01 // 6160 at home, with 61e0 = 1
	zoneCtlAway  = 0x02 // 6160 away, with 61e0 = 0
	roomZoneKind = 0x01 // 6020, room zone; hot water uses dhwZoneKind
	overlayAuto  = 0x00 // 6240, follow the program
)

// zoneWrite holds the fields of one coordinator-authored "z/s?id=N" block. The
// room, the hot water and the boost builders all render it, because the capture
// shows one tag order behind all three:
//
//	6160 6180 6020 61e0 [6200] 6240 [6260] [6280] 62e0 [6440]
type zoneWrite struct {
	home       bool
	programOff bool    // 61e0=0 in the home state: the program block is off
	schedC     float64 // 6200, home state only, and never on a room block that is off
	noSchedC   bool    // leaves 6200 out: the zone's program block is unknown
	kind       byte    // 6020
	overlay    byte    // 6240
	enabled    *byte   // 6260
	setpoint   []byte  // 6280, centi-degrees
	openWindow byte    // 62e0
	circuit    *byte   // 6060
	zoneFlags  *byte   // 6340
	ctrlEnable *byte   // 6400
	newOverlay bool    // 6440
}

// zoneWriteHead renders the home state and the program block of a zone write:
// 6160 6180 6020 61e0 [6200].
func zoneWriteHead(z zoneWrite) []TLV {
	mode, presence := byte(zoneCtlAway), byte(0)
	if z.home {
		mode = zoneCtlHome
		if !z.programOff {
			presence = 1
		}
	}
	head := []TLV{
		{Tag: "6160", Val: []byte{mode}},
		{Tag: "6180", Val: []byte{0x00}},
		{Tag: "6020", Val: []byte{z.kind}},
		{Tag: "61e0", Val: []byte{presence}},
	}
	if sched := z.sentSchedC(); sched != nil {
		head = append(head, TLV{Tag: "6200", Val: sched})
	}
	return head
}

// sentSchedC returns the 6200 the write carries, nil when it leaves 6200 out.
// Only the room capture shows a block that is off, so hot water keeps 6200.
func (z zoneWrite) sentSchedC() []byte {
	if !z.home || z.noSchedC || z.programOff && z.kind == roomZoneKind {
		return nil
	}
	return tempC(z.schedC)
}

// sentSchedC returns the 6200 the room write carries, nil when it leaves 6200
// out.
func (c SetpointCoAP) sentSchedC() []byte {
	return zoneWrite{home: c.Home, programOff: c.ProgramOff, schedC: c.SchedC, noSchedC: c.NoSchedC, kind: roomZoneKind}.sentSchedC()
}

// sentSchedC returns the 6200 the hot-water write carries, nil when it leaves
// 6200 out.
func (c DHWSetpointCoAP) sentSchedC() []byte {
	return zoneWrite{home: c.Home, programOff: c.ProgramOff, schedC: c.SchedC, noSchedC: c.NoSchedC, kind: dhwZoneKind}.sentSchedC()
}

// encodeZoneWrite renders the block in the captured tag order.
func encodeZoneWrite(z zoneWrite) []byte {
	body := encodeTLVs(zoneWriteHead(z))
	body = append(body, TLV{Tag: "6240", Val: []byte{z.overlay}}.Encode()...)
	if z.enabled != nil {
		body = append(body, TLV{Tag: "6260", Val: []byte{*z.enabled}}.Encode()...)
	}
	if z.setpoint != nil {
		body = append(body, TLV{Tag: "6280", Val: z.setpoint}.Encode()...)
	}
	body = append(body, TLV{Tag: "62e0", Val: []byte{z.openWindow}}.Encode()...)
	// Zone configuration, in ascending tag order, after the block the capture
	// pins and before 6440. Absent unless the caller asks for it.
	if z.circuit != nil {
		body = append(body, TLV{Tag: "6060", Val: []byte{*z.circuit}}.Encode()...)
	}
	if z.zoneFlags != nil {
		body = append(body, TLV{Tag: "6340", Val: []byte{*z.zoneFlags}}.Encode()...)
	}
	if z.ctrlEnable != nil {
		body = append(body, TLV{Tag: "6400", Val: []byte{*z.ctrlEnable}}.Encode()...)
	}
	if z.newOverlay {
		body = append(body, TLV{Tag: "6440", Val: []byte{0x00, 0x01}}.Encode()...)
	}
	return body
}

// overlayEnabled is the 6260 value of a write that carries an overlay.
func overlayEnabled() *byte {
	v := byte(0x01)
	return &v
}

// Zone-flag bits of the role bitmask, from the four firmware scanners that
// consume it. bit2 and bit3 are mutually exclusive by design:
// zone_primary_select@0x30b88 elects a device only on kind == 1 &&
// (flags & 0x0c) == 4. The same bitmask is the first byte of a d/config 015e
// record, which is where the air carries it: the captured elected valve reads
// 015e = 0d0a, role 0x0d for zone 10. Tag 6340 is the unobserved alternative.
const (
	ZoneFlagActive  = 0x01 // active-zone selection, app_zone_select_active@0x24f7c
	ZoneFlagCircuit = 0x02 // an explicit circuit binding (6060) is present
	ZoneFlagPrimary = 0x04 // this device publishes the zone's demand (0x40a0)
	ZoneFlagMember  = 0x08 // membership, zone_membership_scan@0x2b52c
)

// SetpointCoAP is a from-scratch setpoint downlink (replaces the meta-copy path).
type SetpointCoAP struct {
	ID         int     // CoAP query id=N (device logical id)
	Setpoint   float64 // active setpoint C -> 6280, sent only with a manual overlay
	Overlay    byte    // 6240: 0 auto, 2 manual, 3 until_next
	OpenWindow byte    // 62e0
	// Home is the state of the whole home: 6160=1 and 61e0=1 with SchedC in
	// 6200, against 6160=2 and 61e0=0 with no 6200 when away.
	Home   bool
	SchedC float64 // 6200, the setpoint the weekly program holds now
	// NoSchedC leaves 6200 out of a home write: the program block of the zone
	// is unknown, so the device keeps its own.
	NoSchedC bool
	// ProgramOff writes 61e0=0 and no 6200 in the home state: the program
	// block in force is disabled (entry field 3 false), so the device targets
	// its fallback.
	ProgramOff bool
	// NewOverlay writes 6440=0001, which marks the start or the end of an
	// overlay. A write that updates a running overlay leaves it false.
	NewOverlay bool
	// Off writes the room off overlay: 6260=0 and no 6280. It needs an
	// overlay (6240 != 0) and ignores Setpoint.
	Off    bool
	MID    uint16 // sent verbatim, 0 included
	Token  []byte // nil => generated
	SrcEUI []byte // nil => BridgeEUI
	DstEUI []byte // target valve wire EUI (8B)

	// Per-zone configuration, emitted only when set, and unobserved on air: no
	// captured frame carries any of the three, in either direction, so they stay
	// absent by default and the block reproduces the capture byte for byte. They
	// remain as a commissioning hypothesis for a device that has never joined,
	// because the firmware does read them. A running installation needs none of
	// them: the role that elects the publisher of 40a0 reaches the device in
	// d/config tag 015e, and the demand chain works without a single one of these
	// tags ever going out.
	ZoneFlags  *byte // 6340, see the ZoneFlag* bits
	Circuit    *byte // 6060, heating circuit id; read when 6340 bit1 is set
	CtrlEnable *byte // 6400; on the wall thermostat the value marks the observe of the zone owner's z/p as lost
}

// BuildSetpointCoAP builds the setpoint downlink the bridge sends on "z/s?id=N",
// from scratch, with a correct UDP checksum. Returns the CoAP datagram (wrap
// with WrapAppPayload). The method is CoAP 0.03 (PUT), as in every capture.
//
// The block reproduces the captured shapes byte for byte, see
// TestBuildSetpointCoAPMatchesCaptureBlock. A manual overlay carries 6260 and
// 6280. The off overlay carries 6260=0 and no 6280. The return to schedule
// (6240=0) omits both: the device keeps the setpoint it already holds and
// applies its own program.
func BuildSetpointCoAP(c SetpointCoAP) ([]byte, error) {
	if len(c.DstEUI) != 8 {
		return nil, errors.New("tado: DstEUI must be 8 bytes")
	}
	body, err := setpointBody(c)
	if err != nil {
		return nil, err
	}
	return BuildCoAP(CoAPReq{
		Type:  "CON",
		Code:  "0.03",
		MID:   c.MID,
		Token: c.Token,
		Path:  []string{"z", "s"},
		Query: []string{"id=" + strconv.Itoa(c.ID)},
		Options: []CoAPOption{
			{Num: 7, Val: []byte{0xff, 0xff}}, // Uri-Port, as sent by the bridge
			{Num: 12, Val: []byte{42}},        // Content-Format application/octet-stream
		},
		Payload: body,
		SrcEUI:  c.SrcEUI,
		DstEUI:  c.DstEUI,
		SrcPort: bridgeCoAPPort,
		DstPort: coapPort,
	})
}

// setpointBody renders the z/s block of a room write.
func setpointBody(c SetpointCoAP) ([]byte, error) {
	if c.Off && c.Overlay == overlayAuto {
		return nil, errors.New("tado: a room off write needs an overlay")
	}
	z := zoneWrite{
		home: c.Home, programOff: c.ProgramOff, schedC: c.SchedC, noSchedC: c.NoSchedC, kind: roomZoneKind,
		overlay: c.Overlay, openWindow: c.OpenWindow,
		circuit: c.Circuit, zoneFlags: c.ZoneFlags, ctrlEnable: c.CtrlEnable,
		newOverlay: c.NewOverlay,
	}
	switch {
	case c.Off:
		off := byte(0x00)
		z.enabled = &off
	case c.Overlay != overlayAuto:
		z.enabled, z.setpoint = overlayEnabled(), tempC(c.Setpoint)
	}
	return encodeZoneWrite(z), nil
}

// DHW setpoint bounds the wall thermostat enforces (nRF52
// hvac_setpoint_clamp_heatsource clamps the heat-source channel to
// 3000..6500 centi-C before it reaches the boiler).
const (
	DHWMinC = 30.0
	DHWMaxC = 65.0
)

const dhwZoneKind = 0x02 // 6020: hot water (room zones use roomZoneKind)

// DHWSetpointCoAP is a domestic-hot-water setpoint downlink for the wall
// thermostat.
//
// It is the same "z/s?id=N" write the valves get, addressed to the zone the
// cloud commissioned as hot water. The block differs from a room setpoint in
// one tag:
//
//	6020 = 02  zone kind hot water (room zones carry 01)
//
// An enabled write carries 6280, the setpoint in centi-degrees. On the device it
// lands on the heat-source channel, crosses the inter-MCU link to the STM32 and
// comes out as OpenTherm WRITE-DATA data-id 0x38 (TdhwSet) in f8.8. The device
// echoes it back a few seconds later as tag 045b on "h/<home>/hvac/mon/dhw".
// A manual OFF omits 6280; the official bridge changes only the channel state.
type DHWSetpointCoAP struct {
	ID       int      // CoAP query id=N: the hot-water zone (0 in every capture)
	Setpoint *float64 // 6280, degrees C; nil for a disabled write and for overlay 0
	Enabled  *bool    // 6260, nil => enabled
	Overlay  byte     // 6240: 0 auto, 2 manual, 3 until_next
	// Home, SchedC, NoSchedC and ProgramOff carry the state of the whole home
	// and of the program block, as in SetpointCoAP, except that ProgramOff keeps
	// 6200.
	Home       bool
	SchedC     float64
	NoSchedC   bool
	ProgramOff bool
	NewOverlay bool   // 6440
	MID        uint16 // sent verbatim, 0 included
	Token      []byte // nil => generated
	SrcEUI     []byte // nil => BridgeEUI
	DstEUI     []byte // wall thermostat wire EUI (8B)
}

// BuildDHWSetpointCoAP builds the hot-water setpoint downlink. Returns the CoAP
// datagram (wrap with WrapAppPayload). The method is CoAP 0.03 (PUT), as in
// every capture.
//
// The manual overlays (2, 3) are capture-proven. A return to schedule
// (6240=0) drops both 6260 and 6280, as the room zones do.
func BuildDHWSetpointCoAP(c DHWSetpointCoAP) ([]byte, error) {
	if len(c.DstEUI) != 8 {
		return nil, errors.New("tado: DstEUI must be 8 bytes")
	}
	if c.Overlay == overlayAuto && (c.Setpoint != nil || c.Enabled != nil) {
		return nil, errors.New("tado: DHW return to schedule must omit setpoint and enabled")
	}
	if c.Overlay != overlayAuto && c.Setpoint == nil && (c.Enabled == nil || *c.Enabled) {
		return nil, errors.New("tado: enabled DHW write needs a setpoint")
	}
	if c.Setpoint != nil && c.Enabled != nil && !*c.Enabled {
		return nil, errors.New("tado: disabled DHW write must omit setpoint")
	}
	if c.Setpoint != nil && (*c.Setpoint < DHWMinC || *c.Setpoint > DHWMaxC) {
		return nil, errors.New("tado: DHW setpoint out of range " +
			strconv.FormatFloat(DHWMinC, 'f', -1, 64) + ".." +
			strconv.FormatFloat(DHWMaxC, 'f', -1, 64) + " C")
	}
	z := zoneWrite{
		home: c.Home, programOff: c.ProgramOff, schedC: c.SchedC, noSchedC: c.NoSchedC, kind: dhwZoneKind,
		overlay: c.Overlay, newOverlay: c.NewOverlay,
	}
	if c.Overlay != overlayAuto {
		enabled := byte(1)
		if c.Enabled != nil && !*c.Enabled {
			enabled = 0
		}
		z.enabled = &enabled
	}
	if c.Setpoint != nil {
		z.setpoint = tempC(*c.Setpoint)
	}
	body := encodeZoneWrite(z)
	return BuildCoAP(CoAPReq{
		Type:  "CON",
		Code:  "0.03",
		MID:   c.MID,
		Token: c.Token,
		Path:  []string{"z", "s"},
		Query: []string{"id=" + strconv.Itoa(c.ID)},
		Options: []CoAPOption{
			{Num: 7, Val: []byte{0xff, 0xff}},
			{Num: 12, Val: []byte{42}},
		},
		Payload: body,
		SrcEUI:  c.SrcEUI,
		DstEUI:  c.DstEUI,
		SrcPort: bridgeCoAPPort,
		DstPort: coapPort,
	})
}

// CoAPCode maps a dotted code string ("0.03","2.04") to its byte.
func CoAPCode(dotted string) (byte, error) {
	if len(dotted) != 4 || dotted[1] != '.' {
		return 0, errors.New("tado: bad CoAP code " + dotted)
	}
	class, err := strconv.Atoi(dotted[0:1])
	if err != nil {
		return 0, errors.New("tado: bad CoAP code " + dotted)
	}
	detail, err := strconv.Atoi(dotted[2:4])
	if err != nil || class > 7 || detail > 31 {
		return 0, errors.New("tado: bad CoAP code " + dotted)
	}
	return byte(class<<5 | detail), nil
}

// buildCoAPMessage serializes the RFC 7252 message (header, token, options,
// payload) with go-coap's UDP coder.
func buildCoAPMessage(r CoAPReq) ([]byte, error) {
	typ, err := coapTypeCode(r.Type)
	if err != nil {
		return nil, err
	}
	code, err := CoAPCode(r.Code)
	if err != nil {
		return nil, err
	}
	mid := r.MID
	if r.GenerateMID {
		mid = nextMID()
	}
	tok := r.Token
	if tok == nil {
		if tok, err = randBytes(4); err != nil {
			return nil, err
		}
	}
	if len(tok) > 8 {
		return nil, errors.New("tado: CoAP token must be at most 8 bytes")
	}

	opts := make(message.Options, 0, len(r.Options)+len(r.Path)+len(r.Query))
	for _, o := range r.Options {
		if o.Num < 0 || o.Num > 0xffff {
			return nil, errors.New("tado: CoAP option number out of range")
		}
		opts = append(opts, message.Option{ID: message.OptionID(o.Num), Value: o.Val})
	}
	for _, s := range r.Path {
		opts = append(opts, message.Option{ID: message.URIPath, Value: []byte(s)})
	}
	for _, s := range r.Query {
		opts = append(opts, message.Option{ID: message.URIQuery, Value: []byte(s)})
	}
	// The coder encodes each option as a delta from the one before it in slice
	// order, so the slice has to be ascending. Stable: repeated Uri-Path and
	// Uri-Query options keep the caller's order.
	sort.SliceStable(opts, func(i, j int) bool { return opts[i].ID < opts[j].ID })

	msg := message.Message{
		Type:      message.Type(typ),
		Code:      codes.Code(code),
		MessageID: int32(mid),
		Token:     tok,
		Options:   opts,
		Payload:   r.Payload,
	}
	n, err := coder.DefaultCoder.Size(msg)
	if err != nil {
		return nil, err
	}
	out := make([]byte, n)
	if _, err := coder.DefaultCoder.Encode(msg, out); err != nil {
		return nil, err
	}
	return out, nil
}

func coapTypeCode(s string) (byte, error) {
	if s == "" {
		return 0, nil // CON
	}
	for i, t := range coapTypes {
		if t == s {
			return byte(i), nil
		}
	}
	return 0, errors.New("tado: bad CoAP type " + s)
}

// linkLocalFromWire builds fe80::/64 + the modified EUI-64 of a node. The wire
// EUI is little-endian and already 8 bytes, so there is no RFC 4291 fffe
// insertion: reverse it and flip the U/L bit.
func linkLocalFromWire(wire []byte) []byte {
	a := make([]byte, 16)
	a[0], a[1] = 0xfe, 0x80
	if len(wire) != 8 {
		return a
	}
	for i := 0; i < 8; i++ {
		a[8+i] = wire[7-i]
	}
	a[8] ^= 0x02
	return a
}

// udpChecksumV6 is the RFC 768 checksum over the IPv6 pseudo-header
// (src||dst||length:4||zero:3||nh=17), the UDP header and the payload.
func udpChecksumV6(src, dst []byte, sp, dp uint16, payload []byte) uint16 {
	ulen := uint32(8 + len(payload))
	buf := make([]byte, 0, 48+len(payload))
	buf = append(buf, src...)
	buf = append(buf, dst...)
	buf = binary.BigEndian.AppendUint32(buf, ulen)
	buf = append(buf, 0, 0, 0, 17)
	buf = binary.BigEndian.AppendUint16(buf, sp)
	buf = binary.BigEndian.AppendUint16(buf, dp)
	buf = binary.BigEndian.AppendUint16(buf, uint16(ulen))
	buf = append(buf, 0, 0) // checksum field itself
	buf = append(buf, payload...)

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
	if c := ^uint16(sum); c != 0 {
		return c
	}
	return 0xffff
}

func randBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// midCounter holds the last Message ID minted. The commercial bridge takes its
// requests from one counter shared by every device (capture-app-20260907: its
// own requests advance from 0x0efc to 0x608f), as Contiki's coap_get_mid does
// from a random start. A sequential MID cannot repeat inside a device's
// EXCHANGE_LIFETIME the way two random ones can, and a repeat reads as a
// duplicate that the device answers without applying.
var midCounter atomic.Uint32

func init() {
	if b, err := randBytes(2); err == nil {
		midCounter.Store(uint32(binary.BigEndian.Uint16(b)))
	}
}

// nextMID mints the Message ID of one new request. It wraps through 0, which
// is a valid Message ID.
func nextMID() uint16 {
	return uint16(midCounter.Add(1))
}
