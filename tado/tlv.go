package tado

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The decrypted app plaintext of a Data frame:
//
//	[c51b00][04][ctr:4 LE][meta ... 0xff][TLV list][trailer:2]
//	TLV = [tag:2 BE][len:1][value:len]
//
// Tag semantics were confirmed by controlled HA experiments. The tag space is
// shared end-to-end (device<->bridge<->cloud).

// TLV is one tag/length/value triple.
type TLV struct {
	Tag string // 2-byte tag, hex (e.g. "6280")
	Len int
	Val []byte
}

// Encode serializes the TLV as [tag:2 BE][len:1][value].
func (t TLV) Encode() []byte {
	tag, _ := hex.DecodeString(t.Tag)
	out := append([]byte{}, tag...)
	out = append(out, byte(len(t.Val)))
	return append(out, t.Val...)
}

// encodeTLVs concatenates the wire encoding of each TLV, in order.
func encodeTLVs(tlvs []TLV) []byte {
	var out []byte
	for _, t := range tlvs {
		out = append(out, t.Encode()...)
	}
	return out
}

// AppPayload is a parsed app plaintext.
type AppPayload struct {
	Type    byte   // version/type octet (0x04 in captures)
	Counter uint32 // internal per-source anti-replay counter (LE, offset 4:8)
	Meta    []byte // the 6LoWPAN(IPHC+UDP-NHC)+CoAP header before the payload TLVs
	TLVs    []TLV
	Trailer []byte // 2-byte app checksum (opaque; CCM* already authenticates)
	Serial  string // device serial (VA/RU...) if present in cleartext
	ID      string // logical id / link id if present
	// CoAP is the decoded 6LoWPAN/UDP/CoAP message. Its Payload is the TLV list
	// (same bytes ParseApp also exposes as TLVs). Nil when the body is not a
	// recognizable CoAP packet (e.g. a synthetic placeholder meta).
	CoAP *CoAPMessage
}

var (
	reSerial = regexp.MustCompile(`(VA|RU)[0-9A-Z]{9,11}`)
	reID     = regexp.MustCompile(`l?id=([0-9]+)`)
)

// TLV returns the first TLV with the given tag, if present.
func (p *AppPayload) TLV(tag string) (TLV, bool) {
	for _, t := range p.TLVs {
		if t.Tag == tag {
			return t, true
		}
	}
	return TLV{}, false
}

// ParseApp parses a decrypted plaintext into its structure. It decodes the
// 6LoWPAN/UDP/CoAP packet and exposes the CoAP payload as the TLV list; every
// captured frame parses this way (including no-payload ACKs). It falls back to a
// TLV-suffix heuristic when the body is not a CoAP packet (a synthetic meta), and
// returns nil only when neither path finds a structure.
func ParseApp(pt []byte) *AppPayload {
	if len(pt) < 8 {
		return nil
	}
	if c, err := ParseCoAP(pt[8:]); err == nil {
		p := &AppPayload{
			Type:    pt[3],
			Counter: binary.LittleEndian.Uint32(pt[4:8]),
			CoAP:    c,
			TLVs:    parseTLVList(c.Payload),
		}
		// meta = the 6LoWPAN/UDP/CoAP header before the payload TLVs; trailer = last 2.
		p.Trailer = pt[len(pt)-2:]
		if end := len(pt) - 2 - len(c.Payload); end >= 8 {
			p.Meta = pt[8:end]
		}
		fillSerialID(p, pt)
		return p
	}
	off, leftover, items := parseTLVSuffix(pt)
	if items == nil {
		return nil
	}
	p := &AppPayload{
		Type:    pt[3],
		Counter: binary.LittleEndian.Uint32(pt[4:8]),
		Meta:    pt[8:off],
		TLVs:    items,
	}
	if leftover == 2 {
		p.Trailer = pt[len(pt)-2:]
	}
	fillSerialID(p, pt)
	return p
}

func fillSerialID(p *AppPayload, pt []byte) {
	if m := reSerial.Find(pt); m != nil {
		p.Serial = string(m)
	}
	if m := reID.FindSubmatch(pt); m != nil {
		p.ID = string(m[1])
	}
}

// ParseTLVs reads the TLV list a CoAP payload carries. Use it on a reassembled
// message, which ParseApp never sees because the fragments hold no whole packet.
func ParseTLVs(b []byte) []TLV { return parseTLVList(b) }

// parseTLVList reads a run of [tag:2 BE][len:1][value] triples until the bytes
// are exhausted. Trailing bytes that cannot form a full TLV are ignored.
func parseTLVList(b []byte) []TLV {
	var out []TLV
	for i := 0; i+3 <= len(b); {
		ln := int(b[i+2])
		if i+3+ln > len(b) {
			break
		}
		out = append(out, TLV{Tag: hex.EncodeToString(b[i : i+2]), Len: ln, Val: b[i+3 : i+3+ln]})
		i += 3 + ln
	}
	return out
}

// parseTLVSuffix finds the longest run of TLVs [tag2][len1<=8][val] that starts
// somewhere at/after offset 8 and consumes to the end or end-2 (a 2-byte
// trailer). Mirrors tado_app.parse_tlv_suffix.
func parseTLVSuffix(pt []byte) (off, leftover int, best []TLV) {
	for start := 8; start < len(pt)-2; start++ {
		i := start
		var items []TLV
		ok := true
		for i+3 <= len(pt) {
			ln := int(pt[i+2])
			if ln > 8 || i+3+ln > len(pt) {
				ok = false
				break
			}
			items = append(items, TLV{Tag: hex.EncodeToString(pt[i : i+2]), Len: ln, Val: pt[i+3 : i+3+ln]})
			i += 3 + ln
		}
		lo := len(pt) - i
		if ok && (lo == 0 || lo == 2) && len(items) >= 2 {
			if best == nil || len(items) > len(best) {
				off, leftover, best = start, lo, items
			}
		}
	}
	return off, leftover, best
}

// --- tag catalog ---

type tagInfo struct {
	name   string
	decode func([]byte) string
}

func be(v []byte) uint64 {
	var n uint64
	for _, b := range v {
		n = n<<8 | uint64(b)
	}
	return n
}

func div(scale float64, unit string) func([]byte) string {
	return func(v []byte) string { return fmt.Sprintf("%.2f%s", float64(be(v))/scale, unit) }
}
func rawDec(v []byte) string { return fmt.Sprintf("%d", be(v)) }

// sdec decodes a big-endian two's-complement integer of any width.
func sdec(v []byte) string {
	if len(v) == 0 || len(v) >= 8 {
		return rawDec(v)
	}
	n := int64(be(v))
	if bits := uint(len(v) * 8); n >= 1<<(bits-1) {
		n -= 1 << bits
	}
	return strconv.FormatInt(n, 10)
}

// asciiVal renders printable ASCII verbatim (a serial or a build hash) and falls
// back to hex for anything else.
func asciiVal(v []byte) string {
	for _, b := range v {
		if b < 0x20 || b >= 0x7f {
			return hex.EncodeToString(v)
		}
	}
	return string(v)
}

func hexVal(v []byte) string { return hex.EncodeToString(v) }

var overlayName = map[uint64]string{0: "auto", 1: OverlayEndUntilAutoChange, 2: "manual", 3: "until_next"}

var zoneKindName = map[uint64]string{1: "heating", 2: "hot_water"}

var tags = withHVACTags(map[string]tagInfo{
	// telemetry (device -> bridge), all confirmed vs concurrent HA sensors
	"012d": {"temp_C", div(100, "C")},      // calibrated temperature = HA current_temperature
	"012e": {"temp_raw_C", div(100, "C")},  // raw sensor; 012d-012e = calibration offset
	"01c8": {"temp_aux_C", div(100, "C")},  // secondary temperature
	"4060": {"temp_aux2_C", div(100, "C")}, // another temperature (matched HA temp)
	"0135": {"humidity_pct", div(10, "%")}, // = HA current_humidity
	"0162": {"battery_mV", rawDec},         // 2467..2555 mV (valve = 2xAA)
	"0136": {"flag0136", rawDec}, "0137": {"flag0137", rawDec}, "027a": {"flag027a", rawDec},
	"0161": {"sen_0161", rawDec}, // one sample of 6205 in a thermostat d/<serial>/sen
	// motor telemetry (d/<serial>/act), u16 each. 0265 and 0294 are two counters
	// of the same unit that follow the valve travel; the capture does not
	// separate an absolute position from the length of the last stroke.
	"0265": {"valve_travel_0265", rawDec},
	"0294": {"valve_travel_0294", rawDec},
	// s16 end-stop position error: 0294 before the end-stop latch minus the stored
	// end-stop, below -100 an overshoot fault (motor_endstop_overshoot_cb@0xf4f6).
	"028d": {"valve_travel_delta", sdec},
	"0273": {"valve_ctr_0273", rawDec},   // moves with 027c, observed 2278..2347
	"027c": {"valve_ctr_027c", rawDec},   // stays above 0273, observed 2300..2480
	"0280": {"valve_const_0280", rawDec}, // near constant, observed 1786..1794
	// fault report (d/<serial>/err), u32. The capture holds no non-zero sample.
	// The bits the valve firmware sets are the Fault* constants below.
	"01a3": {"device_fault_word", rawDec}, // 0 = no fault
	// circuit lead zone (c/N/act): the wall thermostat publishes the target and the
	// temperature of the zone that drives the circuit, not its own. In the capture
	// 4000 follows the zone 10 setpoint (5.00, 25.00, 24.00, 5.00) and 4040 follows
	// the zone 10 temperature (17.48, 26.74), while the thermostat reports 24.95 C
	// for itself in d/RU1000000001/sen over the same window.
	"4000": {"lead_zone_target_C", div(100, "C")},
	"4040": {"lead_zone_temp_C", div(100, "C")},
	// circuit config (c/N/config): the CH flow setpoint the cloud
	// serves and the thermostat applies as the boiler OpenTherm CH setpoint
	"2040": {"ch_flow_setpoint_C", div(100, "C")},
	// circuit call-for-heat (c/N/act); 0 with the boiler off (summer)
	"2090": {"heat_active", rawDec}, // STM32 master heat-active flag
	// The circuit's aggregated heat demand, 0..100: the max over the zones bound
	// to the circuit, written by zone_to_circuit_demand_update@0x2debc into
	// circuit-table byte +3 and published from there by
	// zone_circuit_publish_dirty@0x2db5c.
	"4080": {"circuit_demand_pct", rawDec},
	// The single zone's heat demand, 0..100, produced by
	// zone_heat_demand_report@0x2b69c on the valve; the thermostat folds it into
	// 4080 by taking the max over the circuit's zones (zone_to_circuit_demand_update@0x2debc).
	"40a0": {"zone_demand_pct", rawDec},
	"0164": {"relay_state", rawDec}, // nRF latching relay (P0.25 set / P0.26 reset)
	"0167": {"bus_detect", rawDec},  // hvac/det/bus detect flag, sent by eBus B only
	"0460": {"fault_code", rawDec},  // hvac/codes, one TLV per active fault code
	"0481": {"unsupported", func(v []byte) string { return strings.Join(unsupportedTags(v), ",") }},
	// join / auth (auth/token), confirmed on the captured cloud handshake
	"0007": {"auth_token", hexVal},      // 16-byte session token
	"0260": {"serial_id", asciiVal},     // device serial string ("IB.../VA.../RU...")
	"0210": {"fw_build_hash", asciiVal}, // d/fw/state ASCII build hash (e.g. "73b9b52")
	// valve d/config store, mapped from the firmware dispatcher at 0x1f9a5
	// temperature display unit; ui_gva_attr_write_handler@0x2610e rebuilds the
	// setpoint limits from it (77/41 F on 1, else 25/5 C). No store backend
	// claims it, so the device keeps it only until the next reboot.
	"013f": {"temp_unit", func(v []byte) string {
		if be(v) == 1 {
			return "F"
		}
		return "C"
	}},
	"0140": {"calib_offset", sdec},   // calibration offset, signed centi-degrees
	"0143": {"trace_enable", rawDec}, // diagnostics-module flag, raw byte
	// u16 in every captured d/config document and in its 2.05 echo, always 0000.
	// The dispatcher accepts it and no store backend reads it.
	"021a": {"dcfg_const_021a", rawDec},
	// node-table record, one per entry: [role bitmask][node/zone id], keyed by
	// the record's ordinal in the body (0x7f id = none).
	"015e": {"zone_table", hexVal},
	// UTC-to-local offset in minutes east of UTC, signed s16 big-endian, applied
	// by app_net_provision_init@0xadf0. 0x7fff is the store's "unset" sentinel
	// (cfg_get_u16@0xfe5e miss): the device then runs its schedules on UTC.
	"02b2": {"utc_offset_min", sdec},
	// offline schedule: on 1 the device fetches its zone schedule and runs it
	// alone after 25 min without coordinator answers; on 0 it never fetches.
	"02b3": {"offline_schedule", rawDec},
	// child lock (d/lock), same tag the bridge writes in BuildChildLockCoAP
	"0290": {"child_lock", rawDec},
	// heating control block (bridge <-> device), confirmed via mode-sweep + HA
	"6280": {"setpoint_C", div(100, "C")},       // active setpoint
	"6200": {"setpoint_sched_C", div(100, "C")}, // scheduled setpoint
	"6240": {"overlay", func(v []byte) string {
		if s, ok := overlayName[be(v)]; ok {
			return s
		}
		return rawDec(v)
	}},
	"62e0": {"open_window", rawDec}, // 1 = window open
	// z/p answer flag, 1B: a zone member answers a z/p PUT with 2.05 62a0=00.
	// Bit0 set makes the valve force its PID demand 40a0 to 0.
	"62a0": {"zmeas_ack", rawDec},
	// open-window detection reported by the valve: 021c at device level, which
	// the translator at 0x292c8 republishes per zone as 62c0.
	"021c": {"open_window_detected", rawDec},
	"62c0": {"open_window_zone", rawDec}, // on the zone plane only; see zoneConfigTags
	"6440": {"new_overlay", rawDec},      // downlink-only marker, on every z/s write
	// zone kind, 1B: every room zone reads 1 and the hot water zone reads 2, on
	// the z/s block and in the zone configuration document alike.
	"6020": {"zone_kind", func(v []byte) string {
		if s, ok := zoneKindName[be(v)]; ok {
			return s
		}
		return rawDec(v)
	}},
	"6260": {"zone_channel_enable", rawDec}, // 1B, every captured block carries 1
	// control flags of the z/s block, 1B each. The capture reads 6160=2, 6180=0
	// and 61e0=0 on every zone, so it separates none of the three roles.
	"6160": {"zone_ctl_6160", rawDec},
	"6180": {"zone_ctl_6180", rawDec},
	"61e0": {"zone_ctl_61e0", rawDec},
	// device management attrs (d/* control paths)
	// device fallback mode of d/<serial>/fallback, 1B: valves send 0, 0x7f = unset
	"0182": {"fallback_state", rawDec},
	"015c": {"home_id", rawDec}, // echoed home id in d/config writes, 4B
	// interface module id, u16, in d/config and in the thermostat's hvac/codes
	// report. A valve holds 112 (0 reads as 112), and 0x7fff tears its
	// controller down. On the wall thermostat it selects the STM32 firmware
	// overlay and so the boiler bus family. Not a fault code, those are 0460.
	"015d": {"interface_module_id", rawDec},
	// zone fallback mode of h/<home>/z/N/fallback, 1B: 0 none, 1 last demand
	// with the local program, 2 fixed 80 % at 25 C, 3 frost 5 C
	"6460": {"zone_fallback_mode", rawDec},
	// 8-byte CoAP ETag of the d/config document, cloud-assigned. The device stores
	// the ETag from the config response and re-sends it on the next GET; it changes
	// whenever any config field changes, so it covaries with edits like the display
	// orientation. ff..ff = invalid, forces a full resync. Confirmed in valve fw
	// config_request_build / config_report_response_handle.
	"015a": {"config_etag", hexVal},
	"0148": {"reboot_attr", rawDec},    // attr queued by d/reboot, u16
	"01c7": {"identify_attr", rawDec},  // attr queued by d/identify
	"01c6": {"calibrate_attr", rawDec}, // attr queued by d/calibrate
	// firmware state, served by d/fw/state and repeated in the info report
	// (d/<serial>/s, coap_info_report_serialize); see fwstate.go. Versions are
	// major<<6 | minor, slot flags the pending/image/active/confirmed nibble.
	"01a0": {"fw_device_type", rawDec},
	"003a": {"fw_run_version", fwVersionVal}, "003b": {"fw_run_flags", fwFlagsVal},
	"0035": {"fw_ext10_version", fwVersionVal}, "0036": {"fw_ext10_flags", fwFlagsVal},
	"0039": {"fw_ext11_version", fwVersionVal}, "003c": {"fw_ext11_flags", fwFlagsVal},
	"0285": {"fw_timestamp", rawDec},
	"0180": {"info_180", rawDec}, "014c": {"info_14c", rawDec},
	// control-report family (coap_ctrl_report_serialize), pushed as PUT
	// d/<serial>/mnt. valve_app_fsm_event writes the motor calibration outcome:
	// 01fa 0 none, 1 started, 2 done, 3 failed; 01fb the failure code; 01b5 and
	// 01b6 the travel bounds on success.
	"01b5": {"ctrl_1b5", rawDec}, "01b6": {"ctrl_1b6", rawDec}, "01fa": {"calib_state", rawDec},
	"01fb": {"calib_error", rawDec}, "01b8": {"ctrl_1b8", rawDec},
	// display fields, carried in the d/config store write and consumed by the
	// valve UI handlers (ui_gva_attr_write_handler). 0149 is the display
	// orientation, 0 horizontal, 1 vertical (disp_lcd_screen_config@0x30f44).
	// 0158 holds the change-notification flags app_state_decode_7db@0x2507c
	// reads: bit 9 notifies a remote overlay change, bit 10 any zone change.
	"0149": {"display_orientation", rawDec}, "0158": {"change_notify", hexVal},
	// zone/heat config attrs (z/cpe, 0x6xxx zone-config class)
	"4160": {"heat_pct", rawDec}, // percent-of-heat attr 0x4160 (0..100, deadband 5)
	// external UI binding (z/extui): the two ASCII CoAP endpoint URIs the device
	// pushes its zone state to, "[<ipv6>]/z/s" and "[<ipv6>]/z/p".
	"01d4": {"extui_uri_s", asciiVal}, "01d5": {"extui_uri_p", asciiVal},
	// legionella protection state (z/legionella), 1 byte
	"63c0": {"legionella_state", rawDec},
	// zone configuration document (h/<home>/z/N/config), the body
	// BuildZoneConfigDocument emits. Every URI tag holds the ASCII form
	// "coap://[<ipv6>]/z/<resource>" of one member endpoint. The 0x8xxx tags are
	// indexed containers: one TLV per zone member, in membership order.
	"63e0": {"zone_cfg_state", rawDec},       // 1B, the document always carries 1
	"63a0": {"zone_owner_uri_s", asciiVal},   // zone owner, the elected demand reporter
	"8000": {"zone_member_uri_s", asciiVal},  // one entry per zone member
	"8400": {"zone_report_uri_p", asciiVal},  // where a member reports its zone state
	"8200": {"zone_cpe_uri", asciiVal},       // where a valve takes the heat percent (4160)
	"6040": {"zone_measure_uri_p", asciiVal}, // the measuring endpoint of the zone
	"6060": {"zone_circuit", rawDec},         // zone to circuit binding, 1B
	"60e0": {"zone_cfg_enable", rawDec},      // 1B, the document always carries 1
	"6380": {"zone_refresh_ivl_sec", rawDec}, // u16 seconds
	// PID and interval constants of the heating branch, u16 each. The document
	// carries 1000 / 500 / 1500; which gain each tag holds stays unconfirmed.
	"6080": {"zone_cfg_6080", rawDec},
	"60a0": {"zone_cfg_60a0", rawDec},
	"60c0": {"zone_cfg_60c0", rawDec},
	// diagnostics / cloud auth
	"0168": {"selftest_diag", hexVal}, // d/<serial>/selftest/result diagnostic, 4B
	// auth/token response: the session id the device echoes back as CoAP option
	// 2048 on every uplink, and the session lifetime it refreshes the auth on.
	"025e": {"cloud_session_id", hexVal},      // session id, 4..8B -> option 2048
	"025f": {"cloud_session_minutes", rawDec}, // session lifetime, minutes (u16 BE)
	"0261": {"challenge_key_rotate", hexVal},  // new challenge key id2, 16B
})

// Bits of the fault word, tag 01a3 of d/<serial>/err, as app_state_flags_recompute
// (valve firmware, @0x2e0fc) sets them. Both describe the radio link, not the
// device: a missed answer clears itself at the next one.
const (
	// FaultNoNeighbour: no IPv6 source address, or no neighbour, at the 60 s check.
	FaultNoNeighbour uint32 = 0x02
	// FaultNoConfigAnswer: no answer to the valve's own d/config GET for 90 min
	// (5400 s timer, cleared by any answer code), and from boot until the first answer.
	FaultNoConfigAnswer uint32 = 0x80
)

// DecodeFaultWord splits a fault word into the communication warnings it
// carries, by bit order, and whether any other bit is set. A bit this table does
// not know counts as a device fault.
func DecodeFaultWord(w uint32) (warnings []string, fault bool) {
	if w&FaultNoNeighbour != 0 {
		warnings = append(warnings, "noNeighbour")
	}
	if w&FaultNoConfigAnswer != 0 {
		warnings = append(warnings, "noConfigAnswer")
	}
	return warnings, w&^(FaultNoNeighbour|FaultNoConfigAnswer) != 0
}

// The boiler bus resources of the wall thermostat.
const (
	resHVACMon    = "hvac/mon"
	resHVACDHW    = "hvac/mon/dhw"
	resHVACHP     = "hvac/mon/hp"
	resHVACHC     = "hvac/mon/hc"
	resHVACMaint  = "hvac/maint"
	resHVACConfig = "hvac/config"
	resHVACCodes  = "hvac/codes"
)

// hvacTag is one attribute of the boiler tag catalogue. The thermostat reports
// one catalogue for every boiler bus and each bus fills a subset: tag 0481 of a
// resource lists the attributes the boiler does not support. A value of the
// width's signed maximum (0x7f, 0x7fff, 0x7fffffff) means unset.
type hvacTag struct {
	tag, res, name string
	size           int     // value width in bytes
	scale          float64 // divisor from the wire integer to the unit
	unit           string  // "C" values are signed centi-degrees
}

func hvacRaw(tag, res, prefix string, size int) hvacTag {
	return hvacTag{tag: tag, res: res, name: prefix + "_" + tag, size: size, scale: 1}
}

func hvacNamed(tag, res, name string, size int) hvacTag {
	return hvacTag{tag: tag, res: res, name: name, size: size, scale: 1}
}

func hvacC(tag, res, name string) hvacTag {
	return hvacTag{tag: tag, res: res, name: name, size: 2, scale: 100, unit: "C"}
}

var hvacTags = []hvacTag{
	hvacC("044c", resHVACMon, "boiler_flow_C"),
	hvacC("044d", resHVACMon, "return_water_C"),
	hvacC("044e", resHVACMon, "exhaust_C"),
	hvacC("044f", resHVACMon, "outside_C"),
	hvacC("0450", resHVACMon, "control_setpoint_C"),
	hvacRaw("0451", resHVACMon, "boiler", 2),
	hvacNamed("0452", resHVACMon, "boiler_mod", 2),
	hvacRaw("0453", resHVACMon, "boiler", 1),
	hvacRaw("0454", resHVACMon, "boiler", 2),
	hvacRaw("0455", resHVACMon, "boiler", 1),
	hvacRaw("0456", resHVACMon, "boiler", 1),
	hvacNamed("0457", resHVACMon, "boiler_flag", 1),
	hvacRaw("0458", resHVACMon, "boiler", 2),
	hvacRaw("0459", resHVACMon, "boiler", 1),
	hvacRaw("048a", resHVACMon, "boiler", 2),
	hvacRaw("048b", resHVACMon, "boiler", 2),
	hvacRaw("048c", resHVACMon, "boiler", 2),
	hvacRaw("048d", resHVACMon, "boiler", 2),
	hvacRaw("048e", resHVACMon, "boiler", 1),
	hvacRaw("0490", resHVACMon, "boiler", 1),
	hvacRaw("0498", resHVACMon, "boiler", 4),
	hvacRaw("0499", resHVACMon, "boiler", 4),

	hvacC("045a", resHVACDHW, "dhw_temp_C"),
	hvacRaw("045b", resHVACDHW, "boiler", 2),
	hvacRaw("045c", resHVACDHW, "dhw", 1),
	hvacRaw("045d", resHVACDHW, "dhw", 1),
	{tag: "045e", res: resHVACDHW, name: "dhw_flow_lmin", size: 1, scale: 10, unit: "l/min"},
	hvacRaw("045f", resHVACDHW, "dhw", 1),
	hvacRaw("048f", resHVACDHW, "dhw", 2),
	hvacRaw("049a", resHVACDHW, "dhw", 2),

	hvacRaw("0482", resHVACHP, "hp", 4),
	hvacRaw("0483", resHVACHP, "hp", 4),
	hvacRaw("0484", resHVACHP, "hp", 4),
	hvacRaw("0485", resHVACHP, "hp", 4),
	hvacRaw("0486", resHVACHP, "hp", 4),
	hvacRaw("0487", resHVACHP, "hp", 4),
	hvacRaw("0488", resHVACHP, "hp", 2),
	hvacRaw("0489", resHVACHP, "hp", 2),
	hvacRaw("049b", resHVACHP, "hp", 4),
	hvacRaw("049c", resHVACHP, "hp", 4),

	hvacRaw("0491", resHVACHC, "hc", 2),
	hvacRaw("0492", resHVACHC, "hc", 2),
	hvacRaw("0493", resHVACHC, "hc", 1),
	hvacRaw("0494", resHVACHC, "hc", 4),
	hvacRaw("0495", resHVACHC, "hc", 2),
	hvacRaw("0496", resHVACHC, "hc", 2),
	hvacRaw("0497", resHVACHC, "hc", 1),
	hvacRaw("049d", resHVACHC, "hc", 4),
	hvacRaw("049e", resHVACHC, "hc", 2),

	{tag: "0462", res: resHVACMaint, name: "ch_pressure_bar", size: 1, scale: 10, unit: "bar"},
	hvacNamed("0463", resHVACMaint, "burner_starts", 4),
	hvacNamed("0464", resHVACMaint, "dhw_burner_starts", 4),
	hvacNamed("0465", resHVACMaint, "ch_burner_starts", 4),
	hvacNamed("0466", resHVACMaint, "burner_hours_min", 4), // minutes = hours*60
	hvacNamed("0467", resHVACMaint, "ch_burner_min", 4),
	hvacNamed("0468", resHVACMaint, "dhw_burner_hours_min", 4),

	hvacRaw("0469", resHVACConfig, "boiler", 1),
	hvacRaw("046a", resHVACConfig, "boiler", 1),
	hvacRaw("046b", resHVACConfig, "boiler", 1),
	hvacNamed("046c", resHVACConfig, "slave_config", 1),
	hvacNamed("046d", resHVACConfig, "slave_config_compl", 1),
	hvacRaw("046e", resHVACConfig, "boiler", 2),
	// MaxTSet as the boiler reads it back, stored by
	// hvac_ot_read_sensors_pt@0x0800c230. hvac_ot_f88_to_centi adds 1, so 48 C
	// reads 48.01 and a boiler that answers 0 reads 0.01.
	hvacC("046f", resHVACConfig, "max_ch_setpoint_C"),
	hvacRaw("0470", resHVACConfig, "boiler", 2),
	hvacC("0471", resHVACConfig, "max_ch_bounds_C"),
}

var hvacTagByTag = func() map[string]hvacTag {
	out := make(map[string]hvacTag, len(hvacTags))
	for _, h := range hvacTags {
		out[h.tag] = h
	}
	return out
}()

func withHVACTags(m map[string]tagInfo) map[string]tagInfo {
	for _, h := range hvacTags {
		m[h.tag] = tagInfo{h.name, h.label}
	}
	return m
}

// value decodes the attribute. It reports false for the unset sentinel and for
// a width over 4 bytes.
func (h hvacTag) value(v []byte) (float64, bool) {
	if len(v) == 0 || len(v) > 4 {
		return 0, false
	}
	n, half := be(v), uint64(1)<<(8*len(v)-1)
	if n == half-1 {
		return 0, false
	}
	x := float64(n)
	if h.unit == "C" && n >= half {
		x -= float64(half << 1)
	}
	return x / h.scale, true
}

func (h hvacTag) label(v []byte) string {
	x, ok := h.value(v)
	switch {
	case !ok:
		return "unset"
	case h.unit == "":
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprintf("%.2f%s", x, h.unit)
}

// unsupportedTags reads tag 0481: 3-byte entries {tag BE, 0x02}, one per
// attribute the boiler does not support.
func unsupportedTags(v []byte) []string {
	var out []string
	for i := 0; i+3 <= len(v); i += 3 {
		if v[i+2] == 0x02 {
			out = append(out, hex.EncodeToString(v[i:i+2]))
		}
	}
	return out
}

// --- resource context ---

// TLVResource names the resource a TLV body was read from. The tag space is
// shared across resources, so a tag can carry one attribute on one resource and
// another elsewhere.
type TLVResource uint8

const (
	ResDefault    TLVResource = iota // every resource the catalog covers directly
	ResZoneConfig                    // the h/<home>/z/N/config document
)

// zoneConfigTags override the catalog inside the zone configuration document.
// 62c0 is the open-window state of the zone plane and an interval here.
var zoneConfigTags = map[string]tagInfo{
	"62c0": {"zone_open_ivl_sec", rawDec},
}

// zoneConfigMarks are tags the zone configuration document alone carries. A CoAP
// response repeats no Uri-Path, so a served document identifies itself by these.
var zoneConfigMarks = map[string]bool{
	"63a0": true, "63e0": true, "6040": true, "6380": true,
}

var reZoneConfigPath = regexp.MustCompile(`(^|/)z/[0-9]+/config(\?|$)`)

// ResourceOf classifies one TLV body. It reads the request URI when the message
// carries one, else the tags of the body itself.
func ResourceOf(uri string, ts []TLV) TLVResource {
	if reZoneConfigPath.MatchString(uri) {
		return ResZoneConfig
	}
	for _, t := range ts {
		if zoneConfigMarks[t.Tag] {
			return ResZoneConfig
		}
	}
	return ResDefault
}

// tagFor resolves one tag inside the resource that carried it.
func tagFor(tag string, res TLVResource) (tagInfo, bool) {
	if res == ResZoneConfig {
		if info, ok := zoneConfigTags[tag]; ok {
			return info, true
		}
	}
	info, ok := tags[tag]
	return info, ok
}

// TagLabel returns "name=value" for a TLV using the catalog, or "tag=hex" when
// the tag is unknown.
func (t TLV) TagLabel() string { return t.TagLabelIn(ResDefault) }

// TagLabelIn is TagLabel for a TLV read from a known resource.
func (t TLV) TagLabelIn(res TLVResource) string {
	if info, ok := tagFor(t.Tag, res); ok {
		return info.name + "=" + info.decode(t.Val)
	}
	return t.Tag + "=" + hex.EncodeToString(t.Val)
}
