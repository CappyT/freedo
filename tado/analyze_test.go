package tado

import (
	"encoding/hex"
	"strings"
	"testing"
)

// The analyzer is exercised on the same recorded fixtures as the emulator
// (loadFrames lives in emu_test.go). No hardware is involved.

func analyzeFile(t *testing.T, path string) (*Analyzer, string) {
	t.Helper()
	a := NewAnalyzer(nil)
	for _, fr := range loadFrames(t, path) {
		a.Add(fr)
	}
	return a, a.Report()
}

// TestAnalyzerBothDirections: the analyzer decodes the bridge-sourced downlinks
// the emulator drops, so the golden capture yields commands as well as telemetry.
func TestAnalyzerBothDirections(t *testing.T) {
	a, rep := analyzeFile(t, "testdata/frames_69ec.txt")
	s := a.Stats()

	if s.Frames != 370 || s.Data != 370 {
		t.Errorf("frames=%d data=%d, want 370/370", s.Frames, s.Data)
	}
	if s.Downlinks == 0 || s.Uplinks == 0 {
		t.Fatalf("downlinks=%d uplinks=%d, want both > 0 (bridge-sourced frames must not be dropped)",
			s.Downlinks, s.Uplinks)
	}
	if s.Downlinks+s.Uplinks != s.Data-s.MICFails-s.Malformed {
		t.Errorf("direction split %d+%d does not account for %d data frames (mic-fail=%d malformed=%d)",
			s.Downlinks, s.Uplinks, s.Data, s.MICFails, s.Malformed)
	}
	if s.MICFails != 0 || s.Malformed != 0 || s.AppParseFail != 0 {
		t.Errorf("golden capture should decode cleanly: mic-fail=%d malformed=%d app-parse-fail=%d",
			s.MICFails, s.Malformed, s.AppParseFail)
	}

	for _, want := range []string{
		"downlink commands (bridge -> device)",
		"uplink traffic (device -> bridge)",
		"z/s?id=10",   // per-zone setpoint write
		"setpoint_C=", // decoded control TLV
		"setpoint_C=21.00C",
		"overlay=until_next",
		"new_overlay=1",
		"battery_mV=", // uplink telemetry
		"humidity_pct=",
		"devices: 9",
		"camera",
	} {
		if !strings.Contains(rep, want) {
			t.Errorf("report lacks %q", want)
		}
	}

	// The z/s?id=10 group must sit in the downlink section, not the uplink one.
	dl := strings.Index(rep, "downlink commands (bridge -> device)")
	ul := strings.Index(rep, "uplink traffic (device -> bridge)")
	cmd := strings.Index(rep, "z/s?id=10")
	if !(dl < cmd && cmd < ul) {
		t.Errorf("z/s?id=10 at %d is outside the downlink section (%d..%d)", cmd, dl, ul)
	}
}

// TestAnalyzerReassembly: fragmented datagrams reassemble in both directions and
// their decoded payload reaches the report. A fragment plaintext must never be
// mined for TLVs by the ParseApp heuristic.
func TestAnalyzerReassembly(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		wantDown int
		wantUp   int
		wantDir  string
	}{
		{"downlink", "testdata/frames_frag.txt", 2, 0, "DL"},
		{"uplink", "testdata/frames_frag_uplink.txt", 0, 2, "UL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, rep := analyzeFile(t, c.path)
			s := a.Stats()
			if s.Fragments != 2 {
				t.Errorf("fragments=%d, want 2", s.Fragments)
			}
			if s.Reassembled < 1 {
				t.Fatalf("reassembled=%d, want >= 1", s.Reassembled)
			}
			if s.Downlinks != c.wantDown || s.Uplinks != c.wantUp {
				t.Errorf("downlinks=%d uplinks=%d, want %d/%d", s.Downlinks, s.Uplinks, c.wantDown, c.wantUp)
			}
			for _, want := range []string{
				"reassembled datagrams: 1",
				c.wantDir + " ",
				"coap://", // the reassembled resource listing
				"ACK 2.05 x1",
				"ufficio",
			} {
				if !strings.Contains(rep, want) {
					t.Errorf("report lacks %q:\n%s", want, rep)
				}
			}
			// Two fragment frames, one datagram: the FRAG1 frame must not have
			// produced a second, heuristically decoded message.
			if n := strings.Count(rep, "1 msg"); n != 1 {
				t.Errorf("want exactly one 1-message group, got %d:\n%s", n, rep)
			}
		})
	}
}

// TestAnalyzerNonData: frames that are not Data are counted, never decoded.
func TestAnalyzerNonData(t *testing.T) {
	a := NewAnalyzer(nil)
	for _, h := range []string{
		"25bdcdaba205820ef800803f",                                   // wake-up
		"42ee3009a1cefa07c51b0008a4defa07c51b00040d0000350c803f0000", // enhanced ack
		"0102", // unclassifiable
	} {
		fr, err := hex.DecodeString(h)
		if err != nil {
			t.Fatalf("bad hex %s: %v", h, err)
		}
		a.Add(fr)
	}
	s := a.Stats()
	if s.WakeUps != 1 || s.EnhAcks != 1 || s.Unknown != 1 || s.Other() != 3 {
		t.Errorf("got wakeup=%d enh-ack=%d unknown=%d other=%d, want 1/1/1/3",
			s.WakeUps, s.EnhAcks, s.Unknown, s.Other())
	}
	if s.Data != 0 || s.Downlinks != 0 || s.Uplinks != 0 {
		t.Errorf("non-Data frames were decoded: data=%d dl=%d ul=%d", s.Data, s.Downlinks, s.Uplinks)
	}
}

// TestAnalyzerCorrupt: a frame that passes CRC16 but fails the MIC is counted,
// not reported as traffic.
func TestAnalyzerCorrupt(t *testing.T) {
	a, rep := analyzeFile(t, "testdata/frames_corrupt.txt")
	s := a.Stats()
	if s.MICFails != 2 {
		t.Errorf("mic-fails=%d, want 2", s.MICFails)
	}
	if s.Downlinks != 0 || s.Uplinks != 0 {
		t.Errorf("corrupt frames must not reach the groups: dl=%d ul=%d", s.Downlinks, s.Uplinks)
	}
	if !strings.Contains(rep, "mic-fail=2") {
		t.Errorf("report lacks the mic-fail count:\n%s", rep)
	}
}

// TestAnalyzerUnhandled: a TLV tag outside the catalog lands in the shared
// unhandled log and in the report. The tag is synthesized: every tag the golden
// capture carries is catalogued, which the second half asserts.
func TestAnalyzerUnhandled(t *testing.T) {
	log := NewUnhandledLogger(nil)
	a := NewAnalyzer(log)
	valve := mustHex(NodeByAny("camera").WireLE)
	a.Add(uplinkFrame(t, valve, "0.03", []string{"d", "VA1000000005", "config"}, []string{"id=10"},
		TLV{Tag: "9999", Val: []byte{0x01}}.Encode()))

	c := log.Counts()
	if c["unknown-tlv-tag=9999"] == 0 {
		t.Errorf("unknown tag 9999 not logged, counts=%v", c)
	}
	if log.Total() == 0 {
		t.Error("no unhandled events logged")
	}
	if !strings.Contains(a.Report(), "unknown-tlv-tag=9999") {
		t.Error("report does not carry the unhandled summary")
	}

	goldLog := NewUnhandledLogger(nil)
	gold := NewAnalyzer(goldLog)
	for _, fr := range loadFrames(t, "testdata/frames_69ec.txt") {
		gold.Add(fr)
	}
	if n := countPrefix(goldLog.Counts(), rsnUnknownTag); n != 0 {
		t.Errorf("%d unknown tags in the golden capture:\n%s", n, goldLog.Summary())
	}
}
