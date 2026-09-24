package tado

import (
	"encoding/json"
	"strings"
	"testing"
)

// fwStateBody builds a synthetic d/fw/state body.
func fwStateBody(run, ext10, ext11 FwVersion, runFlags, ext10Flags, ext11Flags FwSlotFlags) []byte {
	u16 := func(v FwVersion) []byte { return []byte{byte(v >> 8), byte(v)} }
	return encodeTLVs([]TLV{
		{Tag: "01a0", Val: []byte{0x08}},
		{Tag: "003a", Val: u16(run)},
		{Tag: "003b", Val: []byte{byte(runFlags)}},
		{Tag: "0035", Val: u16(ext10)},
		{Tag: "0039", Val: u16(ext11)},
		{Tag: "0036", Val: []byte{byte(ext10Flags)}},
		{Tag: "003c", Val: []byte{byte(ext11Flags)}},
		{Tag: "0210", Val: []byte("abc1234")},
		{Tag: "0180", Val: []byte{0x04}},
		{Tag: "0285", Val: []byte{0x00, 0x01, 0x02, 0x03}},
	})
}

func TestParseFwState(t *testing.T) {
	run, _ := NewFwVersion(215, 1)
	old, _ := NewFwVersion(204, 1)
	body := fwStateBody(run, old, run,
		FwSlotHasImage|FwSlotActive|FwSlotConfirmed, FwSlotHasImage|FwSlotConfirmed, FwSlotHasImage|FwSlotActive|FwSlotConfirmed)
	s, err := ParseFwState(body)
	if err != nil {
		t.Fatal(err)
	}
	if s.DeviceType != 8 || s.Running.Version.String() != "215.1" || !s.Confirmed() {
		t.Fatalf("running: %+v", s)
	}
	if uint16(s.Running.Version) != 0x35c1 {
		t.Fatalf("215.1 packs to %#x, want 0x35c1", uint16(s.Running.Version))
	}
	if !s.Ext10.Present || s.Ext10.Version.String() != "204.1" || s.Ext10.Flags.String() != "image+confirmed" {
		t.Fatalf("ext10: %+v", s.Ext10)
	}
	if s.Ext11.Flags.String() != "image+active+confirmed" || s.BuildHash != "abc1234" || s.Timestamp != 0x010203 {
		t.Fatalf("ext11/hash/ts: %+v", s)
	}
	b, err := json.Marshal(s.View())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"version":"215.1"`, `"confirmed":true`, `"ext_slot_10":{"version":"204.1"`, `"build_hash":"abc1234"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("view %s lacks %s", b, want)
		}
	}
}

func TestParseFwStateUnconfirmedAndMissing(t *testing.T) {
	run, _ := NewFwVersion(215, 2)
	s, err := ParseFwState(fwStateBody(run, run, run, FwSlotHasImage|FwSlotActive, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if s.Confirmed() || s.Ext10.Flags.String() != "empty" {
		t.Fatalf("unconfirmed: %+v", s)
	}
	if _, err := ParseFwState(encodeTLVs([]TLV{{Tag: "01a0", Val: []byte{8}}})); err == nil {
		t.Fatal("a body without the running slot parsed")
	}
}

func TestFwVersionParse(t *testing.T) {
	v, err := ParseFwVersion("215.2")
	if err != nil || uint16(v) != 0x35c2 {
		t.Fatalf("215.2 -> %#x %v", uint16(v), err)
	}
	for _, bad := range []string{"215", "215.64", "a.b", "215.1x", "-1.0", "1024.0"} {
		if _, err := ParseFwVersion(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestFwStateTagLabels(t *testing.T) {
	for _, tc := range []struct{ tag, val, want string }{
		{"003a", "35c1", "fw_run_version=215.1"},
		{"003b", "0e", "fw_run_flags=image+active+confirmed"},
		{"0036", "0a", "fw_ext10_flags=image+confirmed"},
		{"01a0", "08", "fw_device_type=8"},
	} {
		if got := (TLV{Tag: tc.tag, Val: mustB(tc.val)}).TagLabel(); got != tc.want {
			t.Errorf("%s=%s -> %q, want %q", tc.tag, tc.val, got, tc.want)
		}
	}
}
