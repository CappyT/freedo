package tado

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func TestBuildZoneConfigDocumentHeatingFixture(t *testing.T) {
	in := fixtureInstall(t)
	doc, err := BuildZoneConfigDocument(in, 3)
	if err != nil {
		t.Fatal(err)
	}

	const valve = "coap://[fe80:0:0:0:21b:c507:fade:a408]/z/"
	const ru = "coap://[fe80:0:0:0:21b:c507:faca:a205]/z/"
	want := zoneConfigTestTLVs(
		zoneConfigTestTLV(0x6020, []byte{1}),
		zoneConfigTestTLV(0x63e0, []byte{1}),
		zoneConfigTestTLV(0x63a0, []byte(valve+"s")),
		zoneConfigTestTLV(0x8200, []byte(valve+"cpe")),
		zoneConfigTestTLV(0x8400, []byte(valve+"p")),
		zoneConfigTestTLV(0x8000, []byte(ru+"s")),
		zoneConfigTestTLV(0x8000, []byte(valve+"s")),
		zoneConfigTestTLV(0x6060, []byte{1}),
		zoneConfigTestTLV(0x6040, []byte(valve+"p")),
		zoneConfigTestTLV(0x6080, []byte{0x03, 0xe8}),
		zoneConfigTestTLV(0x60a0, []byte{0x01, 0xf4}),
		zoneConfigTestTLV(0x60c0, []byte{0x05, 0xdc}),
		zoneConfigTestTLV(0x60e0, []byte{1}),
		zoneConfigTestTLV(0x62c0, []byte{0x03, 0x84}),
		zoneConfigTestTLV(0x6380, []byte{0x03, 0x84}),
	)
	if !bytes.Equal(doc.Body, want) {
		t.Errorf("body = %x\nwant   %x", doc.Body, want)
	}
	if len(doc.Body) != 313 {
		t.Errorf("body length = %d, want 313", len(doc.Body))
	}
	if len(doc.ETag) != 8 {
		t.Errorf("etag = %x, want a non-empty stable 8-byte ETag", doc.ETag)
	}

	again, err := BuildZoneConfigDocument(in, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(doc.ETag, again.ETag) {
		t.Errorf("ETag changed for the same body: %x -> %x", doc.ETag, again.ETag)
	}
}

func TestBuildZoneConfigDocumentZone0CapturedFixture(t *testing.T) {
	doc, err := BuildZoneConfigDocument(fixtureInstall(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	const ru = "coap://[fe80:0:0:0:21b:c507:faca:a205]/z/"
	want := zoneConfigTestTLVs(
		zoneConfigTestTLV(0x6020, []byte{2}),
		zoneConfigTestTLV(0x63e0, []byte{1}),
		zoneConfigTestTLV(0x63a0, []byte(ru+"s")),
		zoneConfigTestTLV(0x8400, []byte(ru+"p")),
		zoneConfigTestTLV(0x8400, []byte(ru+"p")),
		zoneConfigTestTLV(0x8000, []byte(ru+"s")),
	)
	if !bytes.Equal(doc.Body, want) {
		t.Errorf("body = %x\nwant   %x", doc.Body, want)
	}
	if len(doc.Body) != 188 {
		t.Errorf("body length = %d, want 188", len(doc.Body))
	}
	for _, tlv := range parseTLVList(doc.Body) {
		if tlv.Tag == "6060" || tlv.Tag == "6080" || tlv.Tag == "8200" {
			t.Errorf("hot-water document unexpectedly has TLV %s", tlv.Tag)
		}
	}
}

func TestBuildZoneConfigDocumentZone5CapturedFixture(t *testing.T) {
	doc, err := BuildZoneConfigDocument(fixtureInstall(t), 5)
	if err != nil {
		t.Fatal(err)
	}
	const ru = "coap://[fe80:0:0:0:21b:c507:faca:a205]/z/"
	const reporter = "coap://[fe80:0:0:0:21b:c507:face:a01]/z/"
	const member = "coap://[fe80:0:0:0:21b:c507:face:a06]/z/"
	want := zoneConfigTestTLVs(
		zoneConfigTestTLV(0x6020, []byte{1}),
		zoneConfigTestTLV(0x63e0, []byte{1}),
		zoneConfigTestTLV(0x63a0, []byte(reporter+"s")),
		zoneConfigTestTLV(0x8200, []byte(reporter+"cpe")),
		zoneConfigTestTLV(0x8200, []byte(member+"cpe")),
		zoneConfigTestTLV(0x8400, []byte(reporter+"p")),
		zoneConfigTestTLV(0x8400, []byte(member+"p")),
		zoneConfigTestTLV(0x8000, []byte(ru+"s")),
		zoneConfigTestTLV(0x8000, []byte(reporter+"s")),
		zoneConfigTestTLV(0x8000, []byte(member+"s")),
		zoneConfigTestTLV(0x6060, []byte{1}),
		zoneConfigTestTLV(0x6040, []byte(reporter+"p")),
		zoneConfigTestTLV(0x6080, []byte{0x03, 0xe8}),
		zoneConfigTestTLV(0x60a0, []byte{0x01, 0xf4}),
		zoneConfigTestTLV(0x60c0, []byte{0x05, 0xdc}),
		zoneConfigTestTLV(0x60e0, []byte{1}),
		zoneConfigTestTLV(0x62c0, []byte{0x03, 0x84}),
		zoneConfigTestTLV(0x6380, []byte{0x03, 0x84}),
	)
	if !bytes.Equal(doc.Body, want) {
		t.Errorf("body = %x\nwant   %x", doc.Body, want)
	}
	if len(doc.Body) != 442 {
		t.Errorf("body length = %d, want 442", len(doc.Body))
	}
}

// TestZoneConfigDocumentMatchesCapture pins the zone configuration document to
// the air. testdata/frames_zone_config.txt holds the five answer frames of one
// GET h/7654321/z/10/config burst: the coordinator serves the document as three
// RFC 7959 blocks of 128 bytes, the first two 6LoWPAN-fragmented over two Data
// frames each, the third whole. The 313 reassembled bytes are what
// BuildZoneConfigDocument emits for zone 10.
func TestZoneConfigDocumentMatchesCapture(t *testing.T) {
	frames := loadFrames(t, "testdata/frames_zone_config.txt")
	ra := NewReassembler()
	var doc []byte
	blocks := 0
	for i, raw := range frames {
		f, err := ParseData(NetworkKey, raw)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		var m *CoAPMessage
		if IsFragment(f.Plaintext) {
			rs, ok := ra.AddFrame(f)
			if !ok {
				t.Fatalf("frame %d: the reassembler refused the fragment", i)
			}
			if rs.Message == nil {
				continue
			}
			if rs.Frames != 2 || rs.Fragment.Size != 193 {
				t.Errorf("frame %d: %d frames of %d bytes, want 2 of 193", i, rs.Frames, rs.Fragment.Size)
			}
			m = rs.Message
		} else if app := ParseApp(f.Plaintext); app != nil {
			m = app.CoAP
		}
		if m == nil {
			t.Fatalf("frame %d carries no CoAP message", i)
		}
		block, ok := m.Option(23)
		if !ok {
			t.Fatalf("frame %d carries no Block2 option", i)
		}
		if got := int(block.Uint() >> 4); got != blocks {
			t.Fatalf("frame %d is block %d, want block %d", i, got, blocks)
		}
		doc = append(doc, m.Payload...)
		blocks++
	}
	if blocks != 3 {
		t.Fatalf("assembled %d blocks, want 3", blocks)
	}
	if n := ra.Incomplete(); n != 0 {
		t.Errorf("%d datagrams left incomplete", n)
	}

	want, err := BuildZoneConfigDocument(fixtureInstall(t), 10)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(doc, want.Body) {
		t.Fatalf("captured = %x\nbuilt    = %x", doc, want.Body)
	}
	// 6060 is the zone to circuit binding, the fact the fragmented document held.
	circuit, ok := zoneConfigTestFind(ParseTLVs(doc), "6060")
	if !ok || len(circuit.Val) != 1 || circuit.Val[0] != 1 {
		t.Errorf("6060 = %x, want the circuit id 01", circuit.Val)
	}
}

// TestZoneConfigEtagSurvivesRestart: h/<home>/z/N/config is derived from the
// installation on every GET, so its ETag needs no store. A coordinator that
// restarts on the same installation still answers 2.03 to the ETag the device
// cached from the run before.
func TestZoneConfigEtagSurvivesRestart(t *testing.T) {
	in := fixtureInstall(t)
	camera := mustHex("02a6cefa07c51b00")
	// One 16-byte block keeps the answer inside a single frame; every block
	// carries the ETag of the whole document.
	firstBlock := []CoAPOption{{Num: 23, Val: []byte{0x00}}}
	before := NewEmu(in.EmuConfig(), nil)
	raw := deviceUplink(t, camera, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0xd001, Token: []byte{0x0e},
		Path: []string{"h", "7654321", "z", "10", "config"}, Options: firstBlock,
	})
	m := servedAnswer(t, before.OnFrame(raw), 0xd001)
	if m.Code != "2.05" {
		t.Fatalf("first zone config GET = %s, want 2.05", m.Code)
	}
	etag, present := m.Option(4)
	if !present || len(etag.Val) == 0 {
		t.Fatal("the 2.05 carries no ETag option")
	}
	cached := append([]byte(nil), etag.Val...)

	after := NewEmu(in.EmuConfig(), nil)
	raw = deviceUplink(t, camera, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0xd002, Token: []byte{0x0f},
		Path:    []string{"h", "7654321", "z", "10", "config"},
		Options: []CoAPOption{{Num: 4, Val: cached}},
	})
	m = servedAnswer(t, after.OnFrame(raw), 0xd002)
	if m.Code != "2.03" {
		t.Errorf("cached ETag after a restart = %s, want 2.03", m.Code)
	}
	if len(m.Payload) != 0 {
		t.Errorf("2.03 Valid carries %d bytes, want none", len(m.Payload))
	}
}

// TestBuildZoneConfigDocumentWithoutCircuit: a heating zone bound to no circuit
// builds the document of a bound one without tag 6060 and without the heat
// source as a member.
func TestBuildZoneConfigDocumentWithoutCircuit(t *testing.T) {
	in := synthInstall(t)
	bound, err := BuildZoneConfigDocument(in, 4)
	if err != nil {
		t.Fatal(err)
	}
	in.Zones[1].Circuit = nil
	in.Circuits = nil
	if err := in.Validate(); err != nil {
		t.Fatal(err)
	}
	doc, err := BuildZoneConfigDocument(in, 4)
	if err != nil {
		t.Fatalf("a heating zone with no circuit: %v", err)
	}
	have := parseTLVList(doc.Body)
	if _, ok := zoneConfigTestFind(have, "6060"); ok {
		t.Error("the document carries the circuit tag 6060")
	}
	for _, tag := range []string{"6020", "63a0", "6040", "6080", "60e0", "6380"} {
		if _, ok := zoneConfigTestFind(have, tag); !ok {
			t.Errorf("the document lacks tag %s", tag)
		}
	}
	members := func(body []byte) int {
		n := 0
		for _, t := range parseTLVList(body) {
			if t.Tag == "8000" {
				n++
			}
		}
		return n
	}
	if got, want := members(doc.Body), members(bound.Body)-1; got != want {
		t.Errorf("member entries = %d, want %d: the heat source leaves the membership", got, want)
	}
	if bytes.Equal(doc.ETag, bound.ETag) {
		t.Error("the ETag of the two documents is the same")
	}
}

func TestBuildZoneConfigDocumentErrors(t *testing.T) {
	if _, err := BuildZoneConfigDocument(nil, 3); err == nil {
		t.Error("nil install accepted")
	}
	if _, err := BuildZoneConfigDocument(fixtureInstall(t), 4); err == nil {
		t.Error("unknown zone accepted")
	}
}

func zoneConfigTestFind(tlvs []TLV, tag string) (TLV, bool) {
	for _, t := range tlvs {
		if t.Tag == tag {
			return t, true
		}
	}
	return TLV{}, false
}

func zoneConfigTestTLV(tag uint16, val []byte) []byte {
	out := []byte{byte(tag >> 8), byte(tag), byte(len(val))}
	return append(out, val...)
}

func zoneConfigTestTLVs(records ...[]byte) []byte {
	var out []byte
	for _, record := range records {
		out = append(out, record...)
	}
	return out
}

// TestBuildZoneConfigDocumentOpenWindow: the zone carries the open window
// detection, so tags 60e0 and 62c0 follow the installation and the document
// ETag moves with them. The capture holds 1200 and 600 next to the 900 default.
func TestBuildZoneConfigDocumentOpenWindow(t *testing.T) {
	baseDoc, err := BuildZoneConfigDocument(fixtureInstall(t), 3)
	if err != nil {
		t.Fatal(err)
	}
	off, duration := false, 1200
	for _, tt := range []struct {
		name      string
		detection *bool
		duration  *int
		want      []string
	}{
		{"detection off", &off, nil, []string{"60e00100", "62c0020384"}},
		{"duration 1200", nil, &duration, []string{"60e00101", "62c00204b0"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := fixtureInstall(t)
			z, err := in.ZoneByNumber(3)
			if err != nil {
				t.Fatal(err)
			}
			z.OpenWindowDetection, z.OpenWindowDurationSec = tt.detection, tt.duration
			doc, err := BuildZoneConfigDocument(in, 3)
			if err != nil {
				t.Fatal(err)
			}
			body := hex.EncodeToString(doc.Body)
			for _, want := range tt.want {
				if !strings.Contains(body, want) {
					t.Errorf("body = %s\nwants %s", body, want)
				}
			}
			if len(doc.Body) != len(baseDoc.Body) {
				t.Errorf("body length = %d, want the %d of the default document", len(doc.Body), len(baseDoc.Body))
			}
			if bytes.Equal(doc.ETag, baseDoc.ETag) {
				t.Errorf("ETag = %x, want a value the default document does not have", doc.ETag)
			}
		})
	}
}
