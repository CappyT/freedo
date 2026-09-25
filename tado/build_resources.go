package tado

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Downlink writes to the device resources other than z/s. All of them are CoAP
// 0.03 (PUT) CON from the bridge port 4005 to the device CoAP server 5683, the
// same addressing BuildSetpointCoAP uses.

// PairingKey encrypts the Data frame that carries the d/pair payload: that
// payload contains the network key itself, so the frame cannot be secured with
// it. Accepted only while the device commissioning window is open (physical
// button long-press). Unvalidated on air.
var PairingKey = []byte("tado pairing key")

var (
	optUriPort = CoAPOption{Num: 7, Val: []byte{0xff, 0xff}} // Uri-Port, as sent by the bridge
	optOctets  = CoAPOption{Num: 12, Val: []byte{42}}        // Content-Format application/octet-stream
	// Max-Age of 1800 s. The captured "z/p" push carries it and so does the
	// answer to a "z/p" read.
	optZoneMeasureAge = CoAPOption{Num: 14, Val: []byte{0x07, 0x08}}
)

// encodeTLV8 serializes an 8-bit-tag TLV [tag:1][len:1][value], the form used
// inside the d/pair payload. TLV.Encode is the 2-byte-tag form used everywhere else.
func encodeTLV8(tag byte, val []byte) []byte {
	out := make([]byte, 0, 2+len(val))
	out = append(out, tag, byte(len(val)))
	return append(out, val...)
}

// BuildChildLockCoAP builds the d/lock child-lock write (0290 = 0/1). The
// coordinator writes the bare path with no query.
func BuildChildLockCoAP(on bool, mid uint16, token, srcEUI, dstEUI []byte) ([]byte, error) {
	v := byte(0)
	if on {
		v = 1
	}
	return deviceWrite(deviceReq{
		Path:    []string{"d", "lock"},
		Options: []CoAPOption{optUriPort, optOctets},
		Payload: TLV{Tag: "0290", Val: []byte{v}}.Encode(),
		MID:     mid, Token: token, SrcEUI: srcEUI, DstEUI: dstEUI,
	})
}

// BuildIdentifyCoAP builds the d/identify blink request. The coordinator writes
// the bare path with no query, and it keeps the content format on the empty
// payload.
func BuildIdentifyCoAP(mid uint16, token, srcEUI, dstEUI []byte) ([]byte, error) {
	return deviceWrite(deviceReq{
		Path:    []string{"d", "identify"},
		Options: []CoAPOption{optUriPort, optOctets},
		MID:     mid, Token: token, SrcEUI: srcEUI, DstEUI: dstEUI,
	})
}

// BuildCalibrateCoAP builds the d/calibrate motor calibration request. The
// valve serves the bare path with PUT only and ignores the payload; no capture
// of the commercial request exists, so the options follow d/identify.
func BuildCalibrateCoAP(mid uint16, token, srcEUI, dstEUI []byte) ([]byte, error) {
	return deviceWrite(deviceReq{
		Path:    []string{"d", "calibrate"},
		Options: []CoAPOption{optUriPort, optOctets},
		MID:     mid, Token: token, SrcEUI: srcEUI, DstEUI: dstEUI,
	})
}

// BuildSchedFetchCoAP builds an empty POST d/sched, the offline schedule fetch
// trigger. The resource takes POST only: its dispatch node (init image at
// valve 0x3ad8c, thermostat 0x5caa4) holds no other method. Its handler
// (valve 0x2a1e8, thermostat 0x291f0) ignores the payload and runs the fetch
// timer callback (valve 0x2a1c4, thermostat 0x291cc): it arms the fetch,
// resets the Block2 state and keeps the held ETag, so the device sends a
// conditional GET h/<home>/z/<N>/sched at its next report cycle. The build (valve 0x2a1f0) sends that GET only when 02b3 is 1.
// The handler sets no response code, so the device answers the server default
// 2.05.
func BuildSchedFetchCoAP(mid uint16, token, srcEUI, dstEUI []byte) ([]byte, error) {
	return devicePost(deviceReq{
		Path:    []string{"d", "sched"},
		Options: []CoAPOption{optUriPort},
		MID:     mid, Token: token, SrcEUI: srcEUI, DstEUI: dstEUI,
	})
}

// BuildRebootCoAP builds the d/reboot?id=N request (no payload).
func BuildRebootCoAP(id int, mid uint16, token, srcEUI, dstEUI []byte) ([]byte, error) {
	return devicePost(deviceReq{
		Path:    []string{"d", "reboot"},
		Query:   []string{idQuery(id)},
		Options: []CoAPOption{optUriPort},
		MID:     mid, Token: token, SrcEUI: srcEUI, DstEUI: dstEUI,
	})
}

// BuildNetcmdCoAP builds d/test/v1?t=T. The valve handler reads only t.
func BuildNetcmdCoAP(t int, mid uint16, token, srcEUI, dstEUI []byte) ([]byte, error) {
	return deviceWrite(deviceReq{
		Path:    []string{"d", "test", "v1"},
		Query:   []string{"t=" + strconv.Itoa(t)},
		Options: []CoAPOption{optUriPort},
		MID:     mid, Token: token, SrcEUI: srcEUI, DstEUI: dstEUI,
	})
}

// BuildGetCoAP builds a bridge-initiated CoAP GET to a device resource.
func BuildGetCoAP(path, query []string, mid uint16, token, srcEUI, dstEUI []byte) ([]byte, error) {
	return buildGetCoAP(path, query, nil, mid, token, srcEUI, dstEUI)
}

// BuildGetBlock2CoAP requests one Block2 response block from a device resource.
func BuildGetBlock2CoAP(path, query []string, block uint32, mid uint16, token, srcEUI, dstEUI []byte) ([]byte, error) {
	return buildGetCoAP(path, query, []CoAPOption{{Num: 23, Val: encodeCoAPUint(block)}}, mid, token, srcEUI, dstEUI)
}

func buildGetCoAP(path, query []string, extra []CoAPOption, mid uint16, token, srcEUI, dstEUI []byte) ([]byte, error) {
	options := make([]CoAPOption, 0, 1+len(extra))
	options = append(options, optUriPort)
	options = append(options, extra...)
	return BuildCoAP(CoAPReq{
		Type:    "CON",
		Code:    "0.01",
		MID:     mid,
		Token:   token,
		Path:    path,
		Query:   query,
		Options: options,
		SrcEUI:  srcEUI,
		DstEUI:  dstEUI,
		SrcPort: bridgeCoAPPort,
		DstPort: coapPort,
	})
}

// configTLVMaxLen bounds every value in a d/config body. The device reassembles
// each record into a fixed buffer whose capacity config_report_response_handle@0x1fb70
// passes as a literal 8 to coap_tlv_stream_parse@0x105f4; a record declaring more
// returns -5, which aborts the rest of the body. One oversized value would
// therefore silently discard every tag after it.
const configTLVMaxLen = 8

// BuildConfigWriteCoAP builds a d/config store write carrying the given TLVs, as
// the cloud pushes to a device. The method is CoAP 0.03 (PUT), the same framing
// the other d/* writes use. The path carries no query: every accepted write on
// the air addresses the bare d/config. The resolved valve store tags are encoded
// by ConfigWrite; a caller may also pass arbitrary TLVs, which is the way to
// reach the tags ConfigWrite deliberately leaves out.
//
// A zero-length value is not rejected but never reaches the device's per-record
// dispatcher (the fire condition requires len != 0), so it is a silent no-op.
func BuildConfigWriteCoAP(tlvs []TLV, mid uint16, token, srcEUI, dstEUI []byte) ([]byte, error) {
	if len(tlvs) == 0 {
		return nil, errors.New("tado: config write carries no TLV")
	}
	var body []byte
	for _, t := range tlvs {
		if len(t.Val) > configTLVMaxLen {
			return nil, errors.New("tado: config TLV " + t.Tag + " carries " +
				strconv.Itoa(len(t.Val)) + " bytes, the device drops every tag after a value longer than " +
				strconv.Itoa(configTLVMaxLen))
		}
		body = append(body, t.Encode()...)
	}
	return deviceWrite(deviceReq{
		Path:    []string{"d", "config"},
		Options: []CoAPOption{optUriPort, optOctets},
		Payload: body,
		MID:     mid, Token: token, SrcEUI: srcEUI, DstEUI: dstEUI,
	})
}

// BuildConfigRereadCoAP builds an empty PUT d/config, the wall thermostat's
// re-read trigger. Its handler (thermostat 0x1e230) ignores the payload: it
// writes ff..ff to 015a and flags config_request_build@0x1e64c to GET d/config
// at once instead of on the hourly timer. It sends 2.04 and does not echo
// Block1, so coap_server_request_process@0x21150 turns the last block of a
// Block1 upload into 5.01. This request carries no Block1 and gets the 2.04.
func BuildConfigRereadCoAP(mid uint16, token, srcEUI, dstEUI []byte) ([]byte, error) {
	return deviceWrite(deviceReq{
		Path:    []string{"d", "config"},
		Options: []CoAPOption{optUriPort, optOctets},
		MID:     mid, Token: token, SrcEUI: srcEUI, DstEUI: dstEUI,
	})
}

// configConst021a is tag 021a, which every captured d/config document carries
// with this value. The device's dispatcher accepts the record and no store
// backend claims it, so the coordinator sends the constant and nothing reads it.
var configConst021a = TLV{Tag: "021a", Val: []byte{0x00, 0x00}}

// configDocOrder is the tag order of the d/config document the bridge writes.
// The device dispatches per record, so only the repeated 015e depends on the
// order, but a document that differs from the bridge's own is a shape no
// capture supports. Tag 015a closes the document and the caller appends it.
//
// No capture carries 013f or 02b2, so their slots are a choice: they sit
// between the last attested tag before them and 021a.
var configDocOrder = []string{
	"0143", "0140", "015d", "015c", "02b3", "013f", "02b2", "021a", "0149", "015e", "0158",
}

// configDocDefaults are the tags every captured document carries whatever the
// operator asked for. The bridge writes the whole document, so a field the
// coordinator has never seen goes out with the captured value. Tag 015d has no
// default: its value depends on the device kind (installConfigWrite). Tag 0140
// has none either: the valve stores only the tags a document carries
// (config_doc_tlv_cb@0x1f9a4), so a document without it keeps the offset the
// valve holds.
func configDocDefaults() []TLV {
	return []TLV{
		{Tag: "0143", Val: []byte{0x00}}, // trace off
		{Tag: "02b3", Val: []byte{0x00}}, // captured; mergeDeviceConfig always sets it
		configConst021a,
		{Tag: "0149", Val: []byte{0x00}},       // horizontal
		{Tag: "0158", Val: []byte{0x02, 0x00}}, // notify on a remote change
	}
}

// thermostatConfigDocDefaults are the always-present tags of the wall
// thermostat's document. Its captured document carries no 021a and no 0149,
// and 0158 = 0000 (capture-app-20260907.log line 70943,
// ota-watch-20260924-1410.log line 5199). The thermostat refuses every d/config
// write, so no operator value replaces them.
func thermostatConfigDocDefaults() []TLV {
	return []TLV{
		{Tag: "0143", Val: []byte{0x00}},
		{Tag: "02b3", Val: []byte{0x00}},
		{Tag: changeNotifyTag, Val: []byte{0x00, 0x00}},
	}
}

// configDocument renders the merged store TLVs as the whole document the bridge
// puts on the wire: every always-present tag filled from the captured defaults,
// every tag in the captured order. A tag the order does not name keeps its
// position after the named ones. The 015a ETag is the caller's to append.
// thermostat selects the wall thermostat's document, which drops a 021a, 0149
// or 0158 that merged holds.
func configDocument(merged []TLV, thermostat bool) []TLV {
	defaults := configDocDefaults()
	if thermostat {
		defaults = thermostatConfigDocDefaults()
		merged = withoutTag(withoutTag(withoutTag(merged, configConst021a.Tag), orientationTag), changeNotifyTag)
	}
	byTag := map[string][]TLV{}
	var extra []string
	for _, tlv := range merged {
		if _, seen := byTag[tlv.Tag]; !seen && !inConfigDocOrder(tlv.Tag) {
			extra = append(extra, tlv.Tag)
		}
		byTag[tlv.Tag] = append(byTag[tlv.Tag], tlv)
	}
	for _, def := range defaults {
		if _, seen := byTag[def.Tag]; !seen {
			byTag[def.Tag] = []TLV{def}
		}
	}
	out := make([]TLV, 0, len(merged)+len(configDocOrder))
	for _, tag := range configDocOrder {
		out = append(out, byTag[tag]...)
	}
	for _, tag := range extra {
		out = append(out, byTag[tag]...)
	}
	return out
}

func inConfigDocOrder(tag string) bool {
	for _, t := range configDocOrder {
		if t == tag {
			return true
		}
	}
	return false
}

// NodeTableRec is one record of the 015e node table. Both bytes go out in this
// order and the device keys them by the record's ordinal in the body: Node lands
// in the slot array at 0x6300+i, Role in the one at 0x6340+i.
type NodeTableRec struct {
	// Role is the role bitmask. app_node_find_by_zone_kind@0x29672 selects on
	// Role & 0x0d. On air: 0x0d, 0x0b, 0x02.
	Role uint8
	// Node is the node/zone id the record binds, 0x7f meaning none.
	Node uint8
}

// ConfigWrite is the resolved subset of the valve d/config store to push. A nil
// pointer, or an empty slice, leaves that field untouched.
//
// The tags the device's dispatcher accepts but nothing in the image reads
// (020f 0217 0218 0219 0221 0286), and 016a which commits to the provisioning
// flash page with no reader found, have no typed field on purpose.
// BuildConfigWriteCoAP still takes arbitrary TLVs for a caller who needs one.
// Tag 021a belongs to the same family but travels in every document, so it is a
// constant of the document (configConst021a) rather than a field.
type ConfigWrite struct {
	// TempUnitF is tag 013f, which no capture carries: the document holds it only
	// when a caller sets it. true selects Fahrenheit. ui_gva_attr_write_handler@0x2610e
	// rebuilds the setpoint limits from it (77/41 F, else 25/5 C). No store
	// backend claims 013f, so the device drops the value as soon as it has acted
	// on it: push it again on every config sync.
	TempUnitF *bool
	// CalibOffset is tag 0140, the temperature calibration offset in
	// centi-degrees, s16 big-endian.
	CalibOffset *int
	// TraceEnable is tag 0143, the diagnostics-module flag. The dispatcher hands
	// the raw byte to the setter at 0xfcb8 without normalizing it to 0/1, so the
	// field is a byte: a bool would claim a guarantee the device does not give.
	TraceEnable *uint8
	// Orientation is tag 0149, the display orientation: 0 horizontal, 1
	// vertical. ui_gva_attr_write_handler@0x2617e hands a changed value to
	// disp_lcd_screen_config@0x30f44, which asserts on any other value.
	Orientation *uint8
	// ChangeNotify is tag 0158, the change-notification flags decoded by
	// app_state_decode_7db@0x2507c. Bit 9 (0x0200) shows the notification on a
	// remote overlay change, bit 10 (0x0400) on any zone change; the device
	// ignores the other bits and reads 0x7fff as off. On air 0200 and 0000.
	ChangeNotify *uint16
	// Etag is tag 015a, the opaque config-document version, 1..8 bytes (always 8
	// on air). The device rejects it while one of its own Block1 uploads is
	// pending and answers by invalidating the etag
	// (config_etag_invalidate_report@0x1f8ce), so it must not be pushed during
	// an upload. EmuState.EnqueueConfig stamps a 015a of its own: setting this
	// field there puts two 015a records in the body, of which the last wins.
	Etag []byte
	// HomeID is tag 015c, the home id every h/<home>/... path carries. It gates
	// the device's apply path: config_report_response_handle@0x1fb70 reads it
	// back and on 0 logs diag 0x3b/0x100 and applies nothing.
	HomeID *uint32
	// InterfaceModuleID is tag 015d, the id of the device's interface module,
	// u16 big-endian. A valve holds 112. The wall thermostat reflashes its STM32
	// with the firmware overlay this id selects, so only the value the thermostat
	// itself reported may ever reach it.
	InterfaceModuleID *uint16
	// NodeTable is tag 015e, one 2-byte record per entry, sent in this order.
	// The device derives each record's slot from its ordinal in the body and
	// resets that ordinal only at Block2 offset 0, so the whole table has to go
	// out in one exchange starting at the first block. A partial table does not
	// patch the device's: it rewrites the slots from 0.
	NodeTable []NodeTableRec
	// UTCOffsetMin is tag 02b2, which no capture carries: the document holds it
	// only when a caller sets it. Minutes east of UTC. The device holds no DST
	// rules, so whoever serves the config pushes the new value twice a year
	// (CET 60, CEST 120); left unset the on-device weekly schedules run on UTC.
	UTCOffsetMin *int
	// OfflineScheduleEnabled is tag 02b3. On true the device GETs
	// h/<home>/z/<N>/sched at boot, 24 h after a 2.05 or 2.03 and 10 min after an
	// error, and after 25 min without coordinator answers it runs that weekly
	// program on its heating zone (6020 = 1). On false it never fetches. The
	// commercial coordinator writes false. Open window detection is zone tag
	// 60e0, not this one.
	OfflineScheduleEnabled *bool
	// Resend writes the document even when no field of it changes. The capture
	// uses this write as a poke: the device answers 2.04 and reads back both
	// d/<serial>/config and h/<home>/z/N/config, which is how a new zone
	// configuration document reaches it.
	Resend bool
}

const (
	// utcOffsetMinAbs bounds tag 02b2. A real zone offset stays inside a day, and
	// the check also keeps a caller from writing 0x7fff, the store's own "unset"
	// sentinel, which the device would read back as "no offset configured".
	utcOffsetMinAbs = 1440
	// calibOffsetAbs bounds tag 0140 at +-10.0 C, tado's own UI range; every
	// on-air value is a multiple of 10 inside it.
	calibOffsetAbs = 1000
	// nodeTableMax bounds the 015e records: the device counts them in a byte
	// (published as attr 0x17d), so a 256th record wraps the count to zero.
	nodeTableMax = 255
)

// Validate rejects field values the device would misread. TLVs() has no error
// return, so the check lives here and EnqueueConfig runs it before encoding.
func (w ConfigWrite) Validate() error {
	if w.CalibOffset != nil && (*w.CalibOffset < -calibOffsetAbs || *w.CalibOffset > calibOffsetAbs) {
		return errors.New("tado: calibration offset out of range -" +
			strconv.Itoa(calibOffsetAbs) + ".." + strconv.Itoa(calibOffsetAbs) + " centi-degrees")
	}
	if w.Orientation != nil && *w.Orientation > orientationMax {
		return errors.New("tado: orientation must be 0 (horizontal) or 1 (vertical)")
	}
	if w.Etag != nil && (len(w.Etag) == 0 || len(w.Etag) > configTLVMaxLen) {
		return errors.New("tado: config etag must be 1.." + strconv.Itoa(configTLVMaxLen) + " bytes")
	}
	if w.HomeID != nil && *w.HomeID == 0 {
		return errors.New("tado: home id must be non-zero: the device blocks its config apply path on a zero home id")
	}
	if w.InterfaceModuleID != nil {
		if err := validInterfaceModule(*w.InterfaceModuleID); err != nil {
			return err
		}
	}
	if len(w.NodeTable) > nodeTableMax {
		return errors.New("tado: node table holds at most " + strconv.Itoa(nodeTableMax) + " records")
	}
	if w.UTCOffsetMin != nil && (*w.UTCOffsetMin < -utcOffsetMinAbs || *w.UTCOffsetMin > utcOffsetMinAbs) {
		return errors.New("tado: UTC offset out of range -" +
			strconv.Itoa(utcOffsetMinAbs) + ".." + strconv.Itoa(utcOffsetMinAbs) + " minutes")
	}
	return nil
}

// TLVs renders the set fields as the store TLVs, in ascending tag order. The
// device's dispatcher keys on the tag, so only the 015e records depend on the
// order, and they keep the order they were given.
func (w ConfigWrite) TLVs() []TLV {
	var out []TLV
	if w.TempUnitF != nil {
		out = append(out, TLV{Tag: "013f", Val: []byte{boolByte(*w.TempUnitF)}})
	}
	if w.CalibOffset != nil {
		out = append(out, TLV{Tag: "0140", Val: be16(uint16(int16(*w.CalibOffset)))})
	}
	if w.TraceEnable != nil {
		out = append(out, TLV{Tag: "0143", Val: []byte{*w.TraceEnable}})
	}
	if w.Orientation != nil {
		out = append(out, TLV{Tag: "0149", Val: []byte{*w.Orientation}})
	}
	if w.ChangeNotify != nil {
		out = append(out, TLV{Tag: "0158", Val: be16(*w.ChangeNotify)})
	}
	if len(w.Etag) > 0 {
		out = append(out, TLV{Tag: configEtagTag, Val: append([]byte(nil), w.Etag...)})
	}
	if w.HomeID != nil {
		v := make([]byte, 4)
		binary.BigEndian.PutUint32(v, *w.HomeID)
		out = append(out, TLV{Tag: "015c", Val: v})
	}
	if w.InterfaceModuleID != nil {
		out = append(out, TLV{Tag: interfaceModuleTag, Val: be16(*w.InterfaceModuleID)})
	}
	for _, r := range w.NodeTable {
		out = append(out, TLV{Tag: "015e", Val: []byte{r.Role, r.Node}})
	}
	if w.UTCOffsetMin != nil {
		out = append(out, TLV{Tag: "02b2", Val: be16(uint16(int16(*w.UTCOffsetMin)))})
	}
	if w.OfflineScheduleEnabled != nil {
		out = append(out, TLV{Tag: "02b3", Val: []byte{boolByte(*w.OfflineScheduleEnabled)}})
	}
	return out
}

func be16(v uint16) []byte { return []byte{byte(v >> 8), byte(v)} }

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

const (
	// pairPlainKeyTag holds the network key in the clear. The coordinator has a
	// second form, tag 0x07, that wraps the key with a per-device KEK. freedo
	// owns no KEK, so it always sends the plaintext tag.
	pairPlainKeyTag = 0x12
	// pairSessionLen is the length of the single-use value in option 2048. The
	// value is new on each request and freedo keeps no copy of it.
	pairSessionLen = 8
)

// BuildPairCoAP builds the d/pair commission write that hands the 16-byte
// plaintext network key to a device, as tag8 0x12. A joining device reads it
// under PairingKey only; EmuState.EnqueuePair picks the key. The coordinator
// sends a CON POST with an empty token, Content-Format 42 and option 2048, and
// with no Uri-Port. The device answers 2.04 under the new network key. There is
// no id query.
func BuildPairCoAP(networkKey []byte, mid uint16, srcEUI, dstEUI []byte) ([]byte, error) {
	if len(networkKey) != 16 {
		return nil, errors.New("tado: network key must be 16 bytes")
	}
	session, err := randBytes(pairSessionLen)
	if err != nil {
		return nil, err
	}
	// Every captured d/pair goes from UDP 5683 to 5683, not from the bridge's
	// request port 4005 (coap_sniff_1786619225, three requests).
	return BuildCoAP(CoAPReq{
		Type: "CON", Code: "0.02", MID: mid, Token: []byte{},
		Path:    []string{"d", "pair"},
		Options: []CoAPOption{optOctets, {Num: coapSessionOption, Val: session}},
		Payload: encodeTLV8(pairPlainKeyTag, networkKey),
		SrcEUI:  srcEUI, DstEUI: dstEUI, SrcPort: coapPort, DstPort: coapPort,
	})
}

const BoostTemperatureC = 25.0

// BoostCoAP describes the captured heating boost overlay and its restore.
type BoostCoAP struct {
	ID      int
	Enabled bool
	// Home, SchedC, NoSchedC, ProgramOff and NewOverlay carry the state of the
	// whole home, the program block and the overlay marker, as in SetpointCoAP.
	Home       bool
	SchedC     float64
	NoSchedC   bool
	ProgramOff bool
	NewOverlay bool
	OpenWindow byte // 62e0
	MID        uint16
	Token      []byte
	SrcEUI     []byte
	DstEUI     []byte
}

// BuildBoostCoAP builds the official PUT z/s?id=N. The coordinator owns the
// timer. The restore uses overlay auto and omits the enabled and setpoint tags.
func BuildBoostCoAP(c BoostCoAP) ([]byte, error) {
	body := boostHeatingBody(c)
	return deviceWrite(deviceReq{
		Path:    []string{"z", "s"},
		Query:   []string{idQuery(c.ID)},
		Options: []CoAPOption{optUriPort, optOctets},
		Payload: body,
		MID:     c.MID, Token: c.Token, SrcEUI: c.SrcEUI, DstEUI: c.DstEUI,
	})
}

func boostHeatingBody(c BoostCoAP) []byte {
	z := zoneWrite{
		home: c.Home, programOff: c.ProgramOff, schedC: c.SchedC, noSchedC: c.NoSchedC, kind: roomZoneKind,
		overlay: overlayAuto, openWindow: c.OpenWindow, newOverlay: c.NewOverlay,
	}
	if c.Enabled {
		z.overlay, z.enabled, z.setpoint = 3, overlayEnabled(), tempC(BoostTemperatureC)
	}
	return encodeZoneWrite(z)
}

// CH flow (heating-circuit) setpoint bounds. The wall thermostat applies the
// value as the boiler OpenTherm CH setpoint (MaxTSet, data-id 57) and the boiler
// reports its own hard upper bound back as tag 0471 (60.00 C in the reference
// install). The window below stays inside a condensing boiler's usable range; the
// firmware re-clamps regardless.
const (
	FlowMinC = 30.0
	FlowMaxC = 75.0
)

// flowTag is the circuit-config field that carries the CH flow setpoint ("mandata"),
// a u16 big-endian centi-degC. Confirmed on air on "h/<home>/c/<id>/config": the
// cloud answers the wall thermostat's GET of that resource with 2040=<centi>
// (12c0 = 48.00 C in every capture). The STM32 writes it as OpenTherm MaxTSet
// (id 57) at HVAC init, and only for the circuit its 015e role selects (see
// initialNodeTable). The boiler's read-back arrives as tag 046f on "hvac/config".
const flowTag = "2040"

// flowInRange is the single bounds check shared by the builder and the emulator.
func flowInRange(tempC float64) error {
	if tempC < FlowMinC || tempC > FlowMaxC {
		return errors.New("tado: CH flow setpoint out of range " +
			strconv.FormatFloat(FlowMinC, 'f', -1, 64) + ".." +
			strconv.FormatFloat(FlowMaxC, 'f', -1, 64) + " C")
	}
	return nil
}

// BuildFlowSetpointCoAP answers the thermostat circuit-config GET with tag 2040.
// Captures contain this pull response and no coordinator push. The captured
// 2.05 carries ETag and Block2 0/0/128 and no Content-Format.
func BuildFlowSetpointCoAP(id int, tempC float64, mid uint16, token, srcEUI, dstEUI []byte) ([]byte, error) {
	if err := flowInRange(tempC); err != nil {
		return nil, err
	}
	_ = id // documents the target circuit; the 2.05 answer is matched by MID
	payload := flowSetpointPayload(tempC)
	if token == nil {
		token = []byte{}
	}
	return BuildCoAP(CoAPReq{
		Type:  "ACK",
		Code:  "2.05",
		MID:   mid,
		Token: token,
		Options: []CoAPOption{
			{Num: 4, Val: FlowSetpointETag(payload)},
			{Num: 23, Val: []byte{0x03}},
		},
		Payload: payload,
		SrcEUI:  srcEUI,
		DstEUI:  dstEUI,
		SrcPort: bridgeCoAPPort,
		DstPort: coapPort,
	})
}

func flowSetpointPayload(tempC float64) []byte {
	centi := uint16(tempC*100 + 0.5)
	return TLV{Tag: flowTag, Val: []byte{byte(centi >> 8), byte(centi)}}.Encode()
}

// FlowSetpointETag hashes the representation into an opaque 8-byte value.
// The thermostat compares ETags only for equality.
func FlowSetpointETag(payload []byte) []byte {
	sum := sha256.Sum256(payload)
	return append([]byte(nil), sum[:8]...)
}

// timeFixed32Key is the only field key of the "time" response body: protobuf
// field 1, wire type 5 (fixed32), i.e. (1<<3)|5. The valve nanopb descriptor at
// flash 0x00038d9c carries field-info word 0x40001404 = tag 1 /
// PB_LTYPE_FIXED32 / size 4. This resource is the one place the protocol answers
// in protobuf instead of the TLV encoding every other resource uses.
const timeFixed32Key = 0x0d

// EncodeTimePayload renders the "time" response body: the nanopb field key
// followed by the Unix epoch in seconds, little-endian. The epoch is the
// standard 1970-01-01 UTC one (gmtime_r@0x367e0, +719468 day shift) and the
// device turns the value into its millisecond clock at 0x101dc. Captured cloud
// answer: 0df7a57d6a = 1786619383.
func EncodeTimePayload(now time.Time) []byte {
	out := make([]byte, 5)
	out[0] = timeFixed32Key
	binary.LittleEndian.PutUint32(out[1:], uint32(now.Unix()))
	return out
}

// BuildTimeCoAP builds the answer the bridge serves when a device GETs "time": a
// piggybacked ACK 2.05 Content echoing the request's MID and token, carrying no
// options at all - not even Content-Format, which is why this cannot go through
// BuildCoAPAck. That is the cloud's wire form, captured decrypted.
//
// The answer has to be produced synchronously. coap_rx_timed_response_dispatch@0x22748
// discards a response arriving more than 5 s after the device built the request
// (cmp r2,#4; bls accept), so it must never be queued for a later rendezvous.
func BuildTimeCoAP(now time.Time, mid uint16, token, srcEUI, dstEUI []byte, srcPort, dstPort uint16) ([]byte, error) {
	if token == nil {
		token = []byte{} // the request carries TKL=0; echo it rather than mint one
	}
	return BuildCoAP(CoAPReq{
		Type:    "ACK",
		Code:    "2.05",
		MID:     mid,
		Token:   token,
		Payload: EncodeTimePayload(now),
		SrcEUI:  srcEUI,
		DstEUI:  dstEUI,
		SrcPort: srcPort,
		DstPort: dstPort,
	})
}

// Session TLVs of the auth/token answer, read off the valve TLV-response
// callback at 0x1e5b8.
const (
	// sessionIDTag carries the session id. The branch at 0x1e5e2 accepts a
	// length of 4..8 and stores the value; coap_opt_set_0x800@0x247c4 then emits
	// it verbatim as CoAP option 2048 on every uplink the device sends. A
	// zero-length answer leaves the device without that option entirely.
	sessionIDTag = "025e"
	// sessionLifetimeTag carries the session lifetime in minutes, u16 big-endian
	// (byte-swapped at 0x1e620). The device arms its auth refresh at
	// value*60000-120000 ms, two minutes before expiry.
	sessionLifetimeTag = "025f"
	// sessionLifetimeMin mirrors the real cloud's value (05a0 = 24 h).
	sessionLifetimeMin = 1440
)

// BuildAuthTokenBody builds the auth/token 2.05 body the cloud answers with: the
// session id and the session lifetime.
//
// TLV 0261 is deliberately absent. Its branch (0x1e65c, length 16) decrypts the
// value in place with the device's keystore id2 (aes128_decrypt_block@0x1c674)
// and persists the plaintext as keystore id7, the permanent auth token
// (cfg_store_set@0xfda0). Without that device's real id2, id7 becomes garbage.
func BuildAuthTokenBody(sessionID []byte) ([]byte, error) {
	if len(sessionID) < 4 || len(sessionID) > 8 {
		return nil, errors.New("tado: session id must be 4..8 bytes")
	}
	return concat(
		TLV{Tag: sessionIDTag, Val: sessionID}.Encode(),
		TLV{Tag: sessionLifetimeTag, Val: []byte{sessionLifetimeMin >> 8, sessionLifetimeMin & 0xff}}.Encode(),
	), nil
}

// ScheduleEntry is one switch point of a day program.
type ScheduleEntry struct {
	Minute        int // minute of day
	SetpointCenti int // degrees C * 100
	// Enabled is entry field 3. The device copies it verbatim to zone tag 61e0
	// (power on), and false selects the 5 C fallback.
	Enabled bool
}

// EncodeScheduleEntry serializes one entry as a protobuf message body:
// field1 minute (uvarint), field2 setpoint centi-degrees (signed varint, two's
// complement, not zigzag), field3 enabled. Field3 is always emitted, so the
// encoding is deterministic.
//
// The three field types match the valve nanopb descriptor at flash 0x382f0:
// uint32, int32 (PB_LTYPE_VARINT, so two's complement), bool.
func EncodeScheduleEntry(minute int, setpointCenti int, enabled bool) []byte {
	out := appendUvarint([]byte{0x08}, uint64(minute))
	out = appendUvarint(append(out, 0x10), uint64(int64(setpointCenti)))
	return append(out, 0x18, boolByte(enabled))
}

// EncodeScheduleDayProgram serializes a DayProgram: field1 day_mask (uvarint of
// the weekday bitmap the program covers) then each entry on field2 as a
// length-delimited submessage [0x12][len uvarint][entry]. Entry minutes must be
// strictly increasing within a program (caller contract, not enforced here).
//
// The layout matches the valve nanopb descriptor at flash 0x38294: day_mask on
// field 1, the entry list on field 2.
func EncodeScheduleDayProgram(dayMask byte, entries ...ScheduleEntry) []byte {
	out := appendUvarint([]byte{0x08}, uint64(dayMask))
	for _, e := range entries {
		b := EncodeScheduleEntry(e.Minute, e.SetpointCenti, e.Enabled)
		out = appendUvarint(append(out, 0x12), uint64(len(b)))
		out = append(out, b...)
	}
	return out
}

// EncodeSchedule serializes a Schedule: each DayProgram on field2 as a
// length-delimited submessage [0x12][len uvarint][program]. Each argument is a
// blob from EncodeScheduleDayProgram (descriptor 0x382c0).
func EncodeSchedule(programs ...[]byte) []byte {
	var out []byte
	for _, p := range programs {
		out = appendUvarint(append(out, 0x12), uint64(len(p)))
		out = append(out, p...)
	}
	return out
}

// ScheduleProgram is one day program of the weekly schedule: the weekday bitmap
// it applies to and its switch points.
//
// Bit 0 is Monday and bit 6 is Sunday: the valve evaluates the mask with
// (tm_wday+6)%7 (sched_eval_setpoint_and_next@0x216c0).
type ScheduleProgram struct {
	DayMask byte
	Entries []ScheduleEntry
}

// EncodeSchedulePrograms serializes a whole weekly schedule from its day
// programs.
func EncodeSchedulePrograms(programs []ScheduleProgram) []byte {
	blobs := make([][]byte, 0, len(programs))
	for _, p := range programs {
		blobs = append(blobs, EncodeScheduleDayProgram(p.DayMask, p.Entries...))
	}
	return EncodeSchedule(blobs...)
}

// DecodeSchedule reads back a body EncodeSchedulePrograms built. The API serves
// the stored programs out of the stored document, so an unknown field or a
// truncated value is an error and never a skipped byte.
func DecodeSchedule(body []byte) ([]ScheduleProgram, error) {
	return decodeSchedule(body, false)
}

// ReencodeLegacySchedule rewrites a document whose entries carry field 3 as an
// "off" flag, absent meaning enabled, into the current encoding with the same
// enabled values. It returns the new body and its ETag. The state database
// migration runs it on every stored schedule.
func ReencodeLegacySchedule(body []byte) ([]byte, []byte, error) {
	programs, err := decodeSchedule(body, true)
	if err != nil {
		return nil, nil, err
	}
	out := EncodeSchedulePrograms(programs)
	return out, configEtag(out), nil
}

func decodeSchedule(body []byte, legacyOff bool) ([]ScheduleProgram, error) {
	var out []ScheduleProgram
	for len(body) > 0 {
		f, err := nextScheduleField(body)
		if err != nil {
			return nil, err
		}
		if f.number != 2 {
			return nil, fmt.Errorf("tado: schedule holds unknown field %d", f.number)
		}
		program, err := decodeScheduleDayProgram(f.blob, legacyOff)
		if err != nil {
			return nil, err
		}
		out = append(out, program)
		body = f.rest
	}
	return out, nil
}

func decodeScheduleDayProgram(in []byte, legacyOff bool) (ScheduleProgram, error) {
	var out ScheduleProgram
	for len(in) > 0 {
		f, err := nextScheduleField(in)
		if err != nil {
			return ScheduleProgram{}, err
		}
		switch f.number {
		case 1:
			if f.value > 0xff {
				return ScheduleProgram{}, errors.New("tado: schedule day mask is out of range")
			}
			out.DayMask = byte(f.value)
		case 2:
			entry, err := decodeScheduleEntry(f.blob, legacyOff)
			if err != nil {
				return ScheduleProgram{}, err
			}
			out.Entries = append(out.Entries, entry)
		default:
			return ScheduleProgram{}, fmt.Errorf("tado: schedule day program holds unknown field %d", f.number)
		}
		in = f.rest
	}
	return out, nil
}

// decodeScheduleEntry reads field 3 as enabled. An absent field 3 is the
// protobuf default, false. With legacyOff it reads the retired encoding: field
// 3 is "off", and an absent field 3 means enabled.
func decodeScheduleEntry(in []byte, legacyOff bool) (ScheduleEntry, error) {
	out := ScheduleEntry{Enabled: legacyOff}
	for len(in) > 0 {
		f, err := nextScheduleField(in)
		if err != nil {
			return ScheduleEntry{}, err
		}
		switch f.number {
		case 1:
			if f.value > 0x7fffffff {
				return ScheduleEntry{}, errors.New("tado: schedule minute is out of range")
			}
			out.Minute = int(f.value)
		case 2:
			out.SetpointCenti = int(int64(f.value))
		case 3:
			out.Enabled = (f.value != 0) != legacyOff
		default:
			return ScheduleEntry{}, fmt.Errorf("tado: schedule entry holds unknown field %d", f.number)
		}
		in = f.rest
	}
	return out, nil
}

// scheduleField is one protobuf field of the schedule messages: the varint value
// for wire type 0, the bytes for wire type 2, and what follows the field.
type scheduleField struct {
	number int
	value  uint64
	blob   []byte
	rest   []byte
}

func nextScheduleField(in []byte) (scheduleField, error) {
	key, n := binary.Uvarint(in)
	if n <= 0 {
		return scheduleField{}, errors.New("tado: schedule holds a truncated field key")
	}
	in = in[n:]
	number := int(key >> 3)
	switch key & 7 {
	case 0:
		value, used := binary.Uvarint(in)
		if used <= 0 {
			return scheduleField{}, errors.New("tado: schedule holds a truncated varint")
		}
		return scheduleField{number: number, value: value, rest: in[used:]}, nil
	case 2:
		length, used := binary.Uvarint(in)
		if used <= 0 || length > uint64(len(in)-used) {
			return scheduleField{}, errors.New("tado: schedule holds a truncated submessage")
		}
		return scheduleField{number: number, blob: in[used : used+int(length)], rest: in[used+int(length):]}, nil
	}
	return scheduleField{}, fmt.Errorf("tado: schedule field %d uses an unsupported wire type", number)
}

// appendUvarint appends v in protobuf base-128 varint form.
func appendUvarint(out []byte, v uint64) []byte {
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

func idQuery(id int) string { return "id=" + strconv.Itoa(id) }

// deviceReq is the common shape of a resource write.
type deviceReq struct {
	Path    []string
	Query   []string
	Options []CoAPOption
	Payload []byte
	MID     uint16
	Token   []byte
	SrcEUI  []byte
	DstEUI  []byte
}

func deviceWrite(d deviceReq) ([]byte, error) {
	return deviceRequest(d, "0.03")
}

func devicePost(d deviceReq) ([]byte, error) {
	return deviceRequest(d, "0.02")
}

func deviceRequest(d deviceReq, code string) ([]byte, error) {
	return BuildCoAP(CoAPReq{
		Type:    "CON",
		Code:    code,
		MID:     d.MID,
		Token:   d.Token,
		Path:    d.Path,
		Query:   d.Query,
		Options: d.Options,
		Payload: d.Payload,
		SrcEUI:  d.SrcEUI,
		DstEUI:  d.DstEUI,
		SrcPort: bridgeCoAPPort,
		DstPort: coapPort,
	})
}
