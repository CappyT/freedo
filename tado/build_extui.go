package tado

import (
	"errors"
	"net/netip"
	"strings"
)

// z/extui and z/legionella carry a record list of their own rather than the
// telemetry TLV list: [tag:2 BE][len:1][value:len], the value bare ASCII with no
// NUL terminator and len its byte count.
//
// z/extui is the resource that tells a device where to push its own reports:
// tag 01d4 is the zone-state endpoint, 01d5 the measurement one. The "lid="
// query of the resource names a zone kind, not a zone number, and each kind has
// its own pair of store slots.

const (
	extuiTagState = 0x01d4 // endpoint the device reports zone state (z/s) to
	extuiTagMeas  = 0x01d5 // endpoint the device reports measurements (z/p) to
	// extuiMaxValue and legionellaMaxValue are the per-record value limits the
	// device's own parser enforces. A longer record aborts the parse, so it is
	// a 4.00 and not a silent drop the way an unknown tag is.
	extuiMaxValue = 53

	legionellaTag      = 0x63c0
	legionellaMaxValue = 3
)

// extuiKind is the zone kind a z/extui request names.
type extuiKind int

const (
	extuiHeating  extuiKind = 0 // lid=pzh
	extuiHotWater extuiKind = 1 // lid=pzw
)

// extuiKindOf reads the zone kind out of a Uri-Query. Only the two literals
// count: a numeric lid names no kind, and neither does a missing one.
func extuiKindOf(query string) (extuiKind, bool) {
	for _, item := range strings.Split(query, "&") {
		v, ok := strings.CutPrefix(item, "lid=")
		if !ok {
			continue
		}
		switch v {
		case "pzh":
			return extuiHeating, true
		case "pzw":
			return extuiHotWater, true
		}
		return 0, false
	}
	return 0, false
}

// attrRecord is one record of a z/extui or z/legionella body.
type attrRecord struct {
	Tag int
	Val []byte
}

// BuildExtuiBody serializes a z/extui answer: the zone-state endpoint under tag
// 01d4, then the measurement endpoint under 01d5.
func BuildExtuiBody(state, meas string) []byte {
	return encodeAttrRecords(
		attrRecord{Tag: extuiTagState, Val: []byte(state)},
		attrRecord{Tag: extuiTagMeas, Val: []byte(meas)},
	)
}

// ExtuiWriteCoAP is the bridge's pzh external-UI binding write. Empty endpoint
// values use the coordinator context-0 address.
type ExtuiWriteCoAP struct {
	StateEndpoint string
	MeasEndpoint  string
	MID           uint16
	Token         []byte
	SrcEUI        []byte
	DstEUI        []byte
}

// BuildExtuiWriteCoAP builds the bridge's CON PUT z/extui?lid=pzh. The bridge
// emits 01d5 before 01d4 and sets only Content-Format 42.
func BuildExtuiWriteCoAP(c ExtuiWriteCoAP) ([]byte, error) {
	src := c.SrcEUI
	if src == nil {
		src = BridgeEUI
	}
	state, meas := c.StateEndpoint, c.MeasEndpoint
	if state == "" && meas == "" {
		var err error
		state, meas, err = extuiDefaultEndpoints(src)
		if err != nil {
			return nil, err
		}
	}
	if err := validExtuiEndpoint(state); err != nil {
		return nil, err
	}
	if err := validExtuiEndpoint(meas); err != nil {
		return nil, err
	}
	body := encodeAttrRecords(
		attrRecord{Tag: extuiTagMeas, Val: []byte(meas)},
		attrRecord{Tag: extuiTagState, Val: []byte(state)},
	)
	return deviceWrite(deviceReq{
		Path:    []string{"z", "extui"},
		Query:   []string{"lid=pzh"},
		Options: []CoAPOption{optOctets},
		Payload: body,
		MID:     c.MID,
		Token:   c.Token,
		SrcEUI:  src,
		DstEUI:  c.DstEUI,
	})
}

func extuiDefaultEndpoints(src []byte) (string, string, error) {
	if len(src) != 8 {
		return "", "", errors.New("tado: extui needs an 8-byte coordinator EUI")
	}
	addr, ok := netip.AddrFromSlice(contextAddr(src))
	if !ok {
		return "", "", errors.New("tado: extui cannot form coordinator address")
	}
	prefix := "[" + addr.String() + "]"
	return prefix + "/z/s", prefix + "/z/p", nil
}

func validExtuiEndpoint(v string) error {
	if v == "" || len(v) > extuiMaxValue || strings.IndexByte(v, 0) >= 0 {
		return errors.New("tado: extui endpoint must be 1..53 non-NUL bytes")
	}
	return nil
}

// encodeAttrRecords serializes records back to back and stops there: the device
// zero-fills its own bodies to the declared buffer capacity, we must not, since
// only the real records are ours to describe.
func encodeAttrRecords(recs ...attrRecord) []byte {
	var out []byte
	for _, r := range recs {
		out = append(out, byte(r.Tag>>8), byte(r.Tag), byte(len(r.Val)))
		out = append(out, r.Val...)
	}
	return out
}

// parseAttrRecords reads a record list the way the device's parser does. A
// value longer than max aborts it; a record with tag 0 or length 0 is the
// zero-fill of a device-built body and is skipped without firing anything; a
// truncated tail is no error, the parser just runs out of bytes mid-record.
func parseAttrRecords(b []byte, max int) ([]attrRecord, error) {
	var out []attrRecord
	for i := 0; i+3 <= len(b); {
		tag := int(b[i])<<8 | int(b[i+1])
		ln := int(b[i+2])
		if ln > max {
			return nil, errors.New("tado: attribute record value longer than the resource accepts")
		}
		if i+3+ln > len(b) {
			break
		}
		if tag != 0 && ln != 0 {
			out = append(out, attrRecord{Tag: tag, Val: b[i+3 : i+3+ln]})
		}
		i += 3 + ln
	}
	return out, nil
}
