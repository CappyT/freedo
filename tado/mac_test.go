package tado

import (
	"encoding/hex"
	"strings"
	"testing"
)

func mustB(s string) []byte { b, _ := hex.DecodeString(s); return b }

// TestClassify covers every frame class seen on air.
func TestClassify(t *testing.T) {
	cases := []struct {
		hex  string
		want FrameClass
	}{
		{"258ccdaba205820ef800803f", ClassWakeUp},
		{"42ee4b05a2cafa07c51b0009a1cefa07c51b00040dd657350c803fe72d", ClassEnhAck},
		{"69ec0009a1cefa07c51b00010acefa070d4eef31", ClassData},
		{"79ec0009a1cefa07c51b00010acefa077cc6176e", ClassDataPending},
		{"49e8ff09a1cefa07c51b0000112233", ClassBroadcast},
	}
	for _, c := range cases {
		if got := Classify(mustB(c.hex)); got != c.want {
			t.Errorf("Classify(%s) = %v, want %v", c.hex[:8], got, c.want)
		}
	}
}

// TestDissectPrintsCoAPMetadata: the dissection of a Data frame carries the
// fields a capture needs to pair a request with its response.
func TestDissectPrintsCoAPMetadata(t *testing.T) {
	s := Dissect(NetworkKey, mustB(capturedDHWSetpoint))
	for _, want := range []string{"CoAP CON mid=0x", "tok=", "port=65535", "cf=42"} {
		if !strings.Contains(s, want) {
			t.Errorf("dissection %q has no %q", s, want)
		}
	}
}

// TestDissectorJoinsFragments: the stateful dissector holds the fragments of the
// zone configuration answer and prints the whole CoAP message on the last one.
func TestDissectorJoinsFragments(t *testing.T) {
	frames := loadFrames(t, "testdata/frames_zone_config.txt")
	d := NewDissector(NetworkKey)
	first := d.Dissect(frames[0])
	if !strings.Contains(first, "FRAG1 tag=0041 size=193 off=0") || !strings.Contains(first, "held=128/193 frames=1") {
		t.Errorf("first fragment = %q", first)
	}
	if strings.Contains(first, "CoAP") {
		t.Errorf("an incomplete datagram must not print a CoAP message: %q", first)
	}
	last := d.Dissect(frames[1])
	for _, want := range []string{
		"FRAGN tag=0041 size=193 off=128",
		"REASSEMBLED frames=2 bytes=193",
		"CoAP ACK mid=0x5b68 tok=- 2.05",
		"etag=6ddcb9dc8da3b865",
		"blk2=0/1/128",
	} {
		if !strings.Contains(last, want) {
			t.Errorf("completed datagram %q has no %q", last, want)
		}
	}
	// The stateless dissection names the fragment and reassembles nothing.
	if s := Dissect(NetworkKey, frames[0]); !strings.Contains(s, "FRAG1 tag=0041") || strings.Contains(s, "held=") {
		t.Errorf("stateless dissection = %q", s)
	}
}

// TestParseWakeUp decodes a real CSL wake-up frame.
func TestParseWakeUp(t *testing.T) {
	w, err := ParseWakeUp(mustB("258ccdaba205820ef800803f"))
	if err != nil {
		t.Fatal(err)
	}
	if w.PANID != PANID {
		t.Errorf("PANID=%#x want %#x", w.PANID, PANID)
	}
	if w.DstShort != "a205" {
		t.Errorf("DstShort=%s want a205", w.DstShort)
	}
	if w.RendezvousTime != 0x00f8 {
		t.Errorf("RendezvousTime=%d want 248", w.RendezvousTime)
	}
}

// TestParseEnhAck decodes a real Enhanced ACK and confirms the CSL Period.
func TestParseEnhAck(t *testing.T) {
	a, err := ParseEnhAck(mustB("42ee4b05a2cafa07c51b0009a1cefa07c51b00040dd657350c803fe72d"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Seq != 0x4b {
		t.Errorf("Seq=%#x want 0x4b", a.Seq)
	}
	if a.CSLPeriod != CSLPeriodUnits {
		t.Errorf("CSLPeriod=%#x want %#x", a.CSLPeriod, CSLPeriodUnits)
	}
	if hex.EncodeToString(a.SrcEUI) != "09a1cefa07c51b00" {
		t.Errorf("SrcEUI=%s", hex.EncodeToString(a.SrcEUI))
	}
	if hex.EncodeToString(a.Trailer) != "e72d" {
		t.Errorf("Trailer=%s want e72d", hex.EncodeToString(a.Trailer))
	}
}
