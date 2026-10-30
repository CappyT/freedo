package tado

import (
	"errors"
	"maps"
	"slices"
	"strconv"
	"time"
)

// The modes of the room temperature correction of a heating zone.
//
// OffsetModeConfig writes tag 0140 in a full PUT d/config, as the commercial
// coordinator does. The valve re-reads d/config and h/<home>/z/N/config and
// homes its motor after each change (capture 2026-09-23).
//
// OffsetModeDbg2 writes the single record 0140 with PUT d/dbg2/tlvs. The valve
// serves that path with PUT only, on coap_res_tlv_write_handler@0x20118: each
// record goes to cfg_store_set and to the attribute report FIFO
// (coap_report_enqueue@0xd8b0), with no call to the d/config client. No capture
// shows the write on air.
const (
	OffsetModeConfig = "config"
	OffsetModeDbg2   = "dbg2"
)

// ZoneOffset is how a heating zone takes its room temperature correction.
type ZoneOffset struct {
	Mode string `yaml:"mode" json:"mode"`
}

// DefaultZoneOffset is the correction mode of a zone that states none.
var DefaultZoneOffset = ZoneOffset{Mode: OffsetModeConfig}

// ZoneOffset returns the correction mode of zone.
func (o Options) ZoneOffset(zone int) ZoneOffset {
	if off, ok := o.ZoneOffsets[zone]; ok {
		return off
	}
	return DefaultZoneOffset
}

// DefaultRoomTempMaxAgeSec is how long an external room temperature drives
// a zone. A room changes faster than the weather, so the limit is half the one
// of the outdoor temperature.
const DefaultRoomTempMaxAgeSec = 30 * 60

// RoomTempMaxAge is how long an external room temperature drives a zone
// without a new reading.
func (o Options) RoomTempMaxAge() time.Duration {
	return optSeconds(o.RoomTempMaxAgeSec, DefaultRoomTempMaxAgeSec)
}

// Check reports what is wrong with the mode, or "".
func (r ZoneOffset) Check() string {
	switch r.Mode {
	case OffsetModeConfig, OffsetModeDbg2:
		return ""
	}
	return "bad mode " + strconv.Quote(r.Mode) + ": want config or dbg2"
}

func (in *Install) checkZoneOffsets(c *issues) {
	for _, zone := range slices.Sorted(maps.Keys(in.Options.ZoneOffsets)) {
		path := "options.zone_offset." + strconv.Itoa(zone)
		switch z, err := in.ZoneByNumber(zone); {
		case err != nil:
			c.errf(path, "no zone %d is declared", zone)
		case z.Kind != ZoneHeating:
			c.errf(path, "zone %d is not a heating zone", zone)
		}
		if msg := in.Options.ZoneOffsets[zone].Check(); msg != "" {
			c.errf(path, "%s", msg)
		}
	}
}

// BuildDbg2TLVWriteCoAP builds PUT d/dbg2/tlvs, the valve's generic store
// write. The body is the d/config record form, [tag:2][len:1][value], which
// the handler parses with the same coap_tlv_stream_parse. No capture carries
// the request, so the options follow d/config.
func BuildDbg2TLVWriteCoAP(tlvs []TLV, mid uint16, token, srcEUI, dstEUI []byte) ([]byte, error) {
	if len(tlvs) == 0 {
		return nil, errors.New("tado: d/dbg2/tlvs write carries no TLV")
	}
	return deviceWrite(deviceReq{
		Path:    []string{"d", "dbg2", "tlvs"},
		Options: []CoAPOption{optUriPort, optOctets},
		Payload: encodeTLVs(tlvs),
		MID:     mid, Token: token, SrcEUI: srcEUI, DstEUI: dstEUI,
	})
}

// EnqueueCalibOffsetDbg2 queues the calibration offset (tag 0140, centi-degrees)
// as one PUT d/dbg2/tlvs record (enqueueDbg2). The id names the write in the log.
func (e *EmuState) EnqueueCalibOffsetDbg2(target string, id, centi int) error {
	w := ConfigWrite{CalibOffset: &centi}
	if err := w.Validate(); err != nil {
		return err
	}
	desc := "calib offset " + strconv.Itoa(centi) + " id=" + strconv.Itoa(id) + " via d/dbg2/tlvs"
	return e.enqueueDbg2(target, desc, w.TLVs())
}

// enqueueDbg2 queues tlvs as one PUT d/dbg2/tlvs. Its 2.04 puts them in the
// d/config document the device is served, under the ETag it already holds
// (applyDbg2). The valve presents that ETag on its hourly conditional GET
// (config_request_build@0x1fd08) and takes 2.03, so the store write alone
// reaches it. A new ETag would earn a 2.05 and the full config apply, which
// homes the motor. A later full read, with no ETag or an invalid one, finds the
// new values and not the old ones. A write the valve refuses or never takes
// leaves the document as it was: patched at enqueue, the document would claim
// a value the valve lacks, and the conditional GET would never bring it.
func (e *EmuState) enqueueDbg2(target, desc string, tlvs []TLV) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	if e.refusesConfigWrite(wire) {
		return errors.New("tado: d/dbg2/tlvs is a valve resource, the wall thermostat does not serve it")
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return err
	}
	pkt, err := BuildDbg2TLVWriteCoAP(tlvs, mid, token, e.selfEUI, wire)
	if err != nil {
		return err
	}
	if err := e.enqueue(target, "dbg2", desc, pkt, e.key, mid); err != nil {
		return err
	}
	q := e.queue[hexEUI(wire)]
	q[len(q)-1].requireCoAP = true
	q[len(q)-1].responseToken = append([]byte(nil), token...)
	q[len(q)-1].responseCode = "2.04"
	q[len(q)-1].dbg2 = slices.Clone(tlvs)
	return nil
}

// applyDbg2 patches the records of a delivered d/dbg2 write d into the served
// document. A d/config write queued after it already carries them
// (configBase), and a patch would put back a value that write replaces.
func (e *EmuState) applyDbg2(key string, d *queuedDownlink) {
	previous, ok := e.cfgDoc[key]
	if !ok || slices.ContainsFunc(e.queue[key], func(q *queuedDownlink) bool { return q.kind == "config" }) {
		return
	}
	e.setConfigDocument(key, patchConfigDocument(previous, d.dbg2))
	e.deliveredChanged = true
}

// configBase is the d/config document of key with the records of every queued
// d/dbg2 write patched in: the state the device reaches once they land. A new
// d/config write merges on it, so the whole document it carries keeps them.
func (e *EmuState) configBase(key string) cfgSnapshot {
	base := e.cfgDoc[key]
	for _, d := range e.queue[key] {
		if d.dbg2 != nil && base.body != nil {
			base = patchConfigDocument(base, d.dbg2)
		}
	}
	return base
}

// patchConfigDocument merges updates into the document and keeps its ETag.
func patchConfigDocument(previous cfgSnapshot, updates []TLV) cfgSnapshot {
	merged := configDocument(mergeConfigTLVs(withoutConfigETag(parseTLVList(previous.body)), updates), false)
	etag := append([]byte(nil), previous.etag...)
	document := append(merged, TLV{Tag: configEtagTag, Val: etag})
	return cfgSnapshot{body: encodeTLVs(document), etag: etag}
}

// DeviceTemp returns the calibrated temperature 012d that target last reported,
// which a write of its 0140 moves by the written delta (012d = 012e + 0140).
// reported is when that report arrived and seen when target last sent a frame.
// ok is false until target reports 012d. A device report carries only the tags
// that changed, so the value holds while the device sends frames.
func (e *EmuState) DeviceTemp(target string) (tempC float64, reported, seen time.Time, ok bool) {
	wire, _, err := e.resolve(target)
	if err != nil {
		return 0, time.Time{}, time.Time{}, false
	}
	ns, known := e.nodes[hexEUI(wire)]
	if !known || len(ns.raw["012d"]) == 0 {
		return 0, time.Time{}, time.Time{}, false
	}
	return centiC(ns.raw["012d"]), ns.tempAt, ns.LastSeen, true
}
