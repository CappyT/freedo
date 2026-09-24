package tado

import (
	"encoding/hex"
	"strings"
	"testing"
)

// TestDeviceOverlayAnsweredAndTaken: a device overlay PUT gets a 2.05 with no
// body, reaches the owner once and moves the zone block to the overlay. The
// captured body carries the new setpoint in 6280 and no 6200, so the node keeps
// the scheduled setpoint it last reported.
func TestDeviceOverlayAnsweredAndTaken(t *testing.T) {
	in := synthInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	va, err := in.Resolve("VA0000000001")
	if err != nil {
		t.Fatal(err)
	}
	ns := e.node(va.WireEUI())
	ns.raw["6200"] = tempC(20)
	ns.raw["6280"] = tempC(20)
	ns.refreshLastKnown()
	raw := synthDeviceUplink(t, in, va.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7401, Token: []byte{0x21},
		Path: homePath(in, "z", "4", "overlay"), Options: []CoAPOption{optOctets},
		Payload: encodeTLVs([]TLV{
			{Tag: "6280", Val: []byte{0x08, 0x98}}, {Tag: "6240", Val: []byte{0x02}}, {Tag: "6260", Val: []byte{0x01}},
		}),
	})
	if m := synthAnswer(t, in, e.OnFrame(raw), 0x7401); m.Code != "2.05" || len(m.Payload) != 0 {
		t.Fatalf("answer=%s payload=%x, want 2.05 with no body", m.Code, m.Payload)
	}
	want := OverlayReport{Zone: 4, Device: hex.EncodeToString(va.WireEUI())}
	if got := e.TakeOverlayReports(); len(got) != 1 || got[0] != want {
		t.Fatalf("reports=%+v, want [%+v]", got, want)
	}
	if got := e.TakeOverlayReports(); len(got) != 0 {
		t.Errorf("a second take returned %+v, want none", got)
	}
	block, ok := e.ZoneBlock(4)
	if !ok || block.Overlay != 2 || block.SetpointC == nil || *block.SetpointC != 22 {
		t.Errorf("zone block = %+v (known=%t), want a manual overlay at 22 C", block, ok)
	}
	if ns.LastSetpointC != 22 || ns.LastSchedC != 20 {
		t.Errorf("node setpoint=%v sched=%v, want the overlay 22 and the scheduled 20", ns.LastSetpointC, ns.LastSchedC)
	}
	if v := e.valveView(ns); v.SetpointC != 22 || v.SchedSetpointC != 20 {
		t.Errorf("node view SetpointC=%v SchedSetpointC=%v, want 22 and 20", v.SetpointC, v.SchedSetpointC)
	}
}

// TestDeviceOverlayEndValidated: the config refuses a rule on a zone that is
// missing or not a heating zone, and a bad rule; a zone with no rule takes the
// default.
func TestDeviceOverlayEndValidated(t *testing.T) {
	in := synthInstall(t)
	if got := in.Options.DeviceOverlayEnd(4); got != DefaultDeviceOverlayEnd {
		t.Errorf("zone 4 with no rule = %+v, want %+v", got, DefaultDeviceOverlayEnd)
	}
	in.Options.DeviceOverlayEnds = map[int]OverlayEnd{4: {Mode: OverlayEndTimer, DurationSec: 900}}
	if err := in.Validate(); err != nil {
		t.Fatalf("a timer rule on a heating zone: %v", err)
	}
	for zone, end := range map[int]OverlayEnd{
		0: {Mode: OverlayEndManual},
		9: {Mode: OverlayEndManual},
		4: {Mode: OverlayEndTimer},
	} {
		in.Options.DeviceOverlayEnds = map[int]OverlayEnd{zone: end}
		if err := in.Validate(); err == nil || !strings.Contains(err.Error(), "device_overlay_end") {
			t.Errorf("zone %d rule %+v: err = %v, want a device_overlay_end error", zone, end, err)
		}
	}
}

func TestOverlayEndCheck(t *testing.T) {
	for _, tc := range []struct {
		end  OverlayEnd
		want string
	}{
		{OverlayEnd{Mode: OverlayEndManual}, ""},
		{OverlayEnd{Mode: OverlayEndUntilNext}, ""},
		{OverlayEnd{Mode: OverlayEndTimer, DurationSec: 3600}, ""},
		{OverlayEnd{Mode: OverlayEndTimer}, "needs a duration_sec"},
		{OverlayEnd{Mode: OverlayEndTimer, DurationSec: 8 * 24 * 3600}, "needs a duration_sec"},
		{OverlayEnd{Mode: OverlayEndManual, DurationSec: 60}, "needs mode timer"},
		{OverlayEnd{Mode: "forever"}, "bad mode"},
	} {
		got := tc.end.Check()
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%+v: Check() = %q, want %q", tc.end, got, tc.want)
		}
	}
}
