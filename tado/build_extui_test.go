package tado

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// No frame in the capture corpus carries z/extui or z/legionella, so neither
// resource has a golden vector. Every expectation below is therefore the wire
// form written out by hand from the device firmware contract, never a value the
// encoder produced: a test that only ran the codec against itself would agree
// with any bug the codec has.

// TestBuildExtuiBodyLiteralBytes: the served body is the two records and
// nothing else - tag big-endian, length = strlen, ASCII value with no NUL and
// no zero padding after the last record.
func TestBuildExtuiBodyLiteralBytes(t *testing.T) {
	state, meas := "[aaaa::21b:c507:face:a109]/z/s", "[aaaa::21b:c507:face:a109]/z/p"

	want := []byte{0x01, 0xd4, 0x1e}
	want = append(want, []byte("[aaaa::21b:c507:face:a109]/z/s")...)
	want = append(want, 0x01, 0xd5, 0x1e)
	want = append(want, []byte("[aaaa::21b:c507:face:a109]/z/p")...)

	got := BuildExtuiBody(state, meas)
	if !bytes.Equal(got, want) {
		t.Fatalf("body =\n%s\nwant\n%s", hex.EncodeToString(got), hex.EncodeToString(want))
	}
	if int(got[2]) != len(state) || len(state) != 0x1e {
		t.Errorf("length byte = %d, want strlen = %d", got[2], len(state))
	}
	if bytes.IndexByte(got, 0) >= 0 {
		t.Errorf("body carries a NUL: %s", hex.EncodeToString(got))
	}
}

// TestBuildExtuiBodyShortAddress: the same encoding with a short address, the
// 32-byte example the firmware analysis pins.
func TestBuildExtuiBodyShortAddress(t *testing.T) {
	const want = "01d40d5b666430303a3a315d2f7a2f73" + "01d50d5b666430303a3a315d2f7a2f70"
	got := hex.EncodeToString(BuildExtuiBody("[fd00::1]/z/s", "[fd00::1]/z/p"))
	if got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func TestBuildExtuiWriteCoAP(t *testing.T) {
	dst := testDstEUI(t)
	pkt, err := BuildExtuiWriteCoAP(ExtuiWriteCoAP{
		MID: 0x1234, Token: []byte{0xaa, 0xbb}, SrcEUI: BridgeEUI, DstEUI: dst,
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := parseCoAPMessage(pkt[9:])
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != "CON" || m.Code != "0.03" || m.URI() != "z/extui?lid=pzh" {
		t.Fatalf("request=%+v", m)
	}
	if _, ok := m.Option(7); ok {
		t.Fatal("Uri-Port is not in the bridge extui write")
	}
	if cf, ok := m.Option(12); !ok || !bytes.Equal(cf.Val, []byte{42}) {
		t.Fatalf("Content-Format=%+v", cf)
	}
	want := "01d51e5b616161613a3a3231623a633530373a666163653a613130395d2f7a2f70" +
		"01d41e5b616161613a3a3231623a633530373a666163653a613130395d2f7a2f73"
	if got := hex.EncodeToString(m.Payload); got != want {
		t.Fatalf("payload=%s want=%s", got, want)
	}
}

func TestExtuiKindOf(t *testing.T) {
	cases := []struct {
		query string
		kind  extuiKind
		ok    bool
	}{
		{"lid=pzh", extuiHeating, true},
		{"lid=pzw", extuiHotWater, true},
		{"id=3&lid=pzw", extuiHotWater, true},
		{"lid=5", 0, false}, // a zone number names no kind
		{"lid=pzx", 0, false},
		{"lid=pzhh", 0, false}, // longer than the device's 3-byte buffer
		{"lid=", 0, false},
		{"id=10", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		k, ok := extuiKindOf(c.query)
		if ok != c.ok || (ok && k != c.kind) {
			t.Errorf("extuiKindOf(%q) = %d, %v; want %d, %v", c.query, k, ok, c.kind, c.ok)
		}
	}
}

// TestParseAttrRecordsWireBody: an inbound body written out byte by byte parses
// into its records, values verbatim.
func TestParseAttrRecordsWireBody(t *testing.T) {
	body := mustHex("01d40d5b666430303a3a315d2f7a2f73" + "01d50d5b666430303a3a315d2f7a2f70")
	recs, err := parseAttrRecords(body, extuiMaxValue)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if recs[0].Tag != 0x01d4 || string(recs[0].Val) != "[fd00::1]/z/s" {
		t.Errorf("record 0 = %#x %q", recs[0].Tag, recs[0].Val)
	}
	if recs[1].Tag != 0x01d5 || string(recs[1].Val) != "[fd00::1]/z/p" {
		t.Errorf("record 1 = %#x %q", recs[1].Tag, recs[1].Val)
	}
}

// TestParseAttrRecordsTolerance: a device-built body is zero-filled to its
// buffer capacity and can end mid-record; neither is an error, while a value
// longer than the resource accepts aborts the parse and becomes a 4.00.
func TestParseAttrRecordsTolerance(t *testing.T) {
	padded := mustHex("000000" + "01d40d5b666430303a3a315d2f7a2f73" + "01d400" + "000000")
	recs, err := parseAttrRecords(padded, extuiMaxValue)
	if err != nil {
		t.Fatalf("padded body: %v", err)
	}
	if len(recs) != 1 || recs[0].Tag != 0x01d4 {
		t.Errorf("padded body gave %d records, want the one real record", len(recs))
	}

	truncated := mustHex("01d40d5b6664")
	if recs, err := parseAttrRecords(truncated, extuiMaxValue); err != nil || len(recs) != 0 {
		t.Errorf("truncated tail = %v, %v; want no record and no error", recs, err)
	}

	tooLong := append(mustHex("01d436"), bytes.Repeat([]byte{'a'}, 54)...)
	if _, err := parseAttrRecords(tooLong, extuiMaxValue); err == nil {
		t.Error("a 54-byte value was accepted on a resource that caps at 53")
	}
}

// TestParseLegionellaRecord: the lawful POST body and the 3-byte cap.
func TestParseLegionellaRecord(t *testing.T) {
	recs, err := parseAttrRecords(mustHex("63c00101"), legionellaMaxValue)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(recs) != 1 || recs[0].Tag != legionellaTag || !bytes.Equal(recs[0].Val, []byte{0x01}) {
		t.Errorf("records = %v, want one 63c0 record carrying 01", recs)
	}
	if _, err := parseAttrRecords(mustHex("63c00401020304"), legionellaMaxValue); err == nil {
		t.Error("a 4-byte value was accepted on a resource that caps at 3")
	}
}
