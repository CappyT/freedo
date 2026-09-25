package tado

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func flowGET(t *testing.T, in *Install, e *EmuState, dev []byte, mid uint16, etag []byte) *CoAPMessage {
	t.Helper()
	r := CoAPReq{
		Type: "CON", Code: "0.01", MID: mid, Token: []byte{0x31},
		Path: homePath(in, "c", "1", "config"),
	}
	if etag != nil {
		r.Options = []CoAPOption{{Num: 4, Val: etag}}
	}
	return synthAnswer(t, in, e.OnFrame(synthDeviceUplink(t, in, dev, r)), mid)
}

// TestServedFlowDefault: a circuit nothing sets is served options.flow_limit_c,
// 48 C when unset, never an empty 2.05.
func TestServedFlowDefault(t *testing.T) {
	in := synthInstall(t)
	ru, _ := synthDevices(t, in)
	e := NewEmu(in.EmuConfig(), nil)
	m := flowGET(t, in, e, ru, 0x7401, nil)
	if got := hex.EncodeToString(m.Payload); m.Code != "2.05" || got != "20400212c0" {
		t.Fatalf("c/1/config = %s %s, want 2.05 20400212c0", m.Code, got)
	}

	limit := 55
	next := synthInstall(t)
	next.Options.FlowLimitC = &limit
	if _, _, err := e.SetInstall(next); err != nil {
		t.Fatal(err)
	}
	if _, fetched := e.ServedFlow(1); fetched {
		t.Error("a new default reads as fetched")
	}
	m = flowGET(t, in, e, ru, 0x7402, nil)
	if got := hex.EncodeToString(m.Payload); got != "204002157c" {
		t.Errorf("c/1/config after flow_limit_c=55 = %s, want 2040 02 157c", got)
	}
	if _, fetched := e.ServedFlow(1); !fetched {
		t.Error("the GET did not mark the value fetched")
	}
}

func TestFlowCurveFlowAt(t *testing.T) {
	c := FlowCurve{
		Enabled: true,
		Points:  [2]FlowCurvePoint{{OutdoorC: -5, FlowC: 60}, {OutdoorC: 15, FlowC: 35}},
		MinC:    38, MaxC: 55,
	}
	for _, tc := range []struct {
		outdoor float64
		want    int
	}{
		{-20, 55}, // extrapolated 78.75, clamped to max
		{-5, 55},  // 60, clamped to max
		{3, 50},   // 50
		{4.3, 48}, // 48.375 rounds down
		{4.7, 48}, // 47.875 rounds up
		{12, 39},  // 38.75 rounds up
		{15, 38},  // 35, clamped to min
		{30, 38},
	} {
		if got := c.FlowAt(tc.outdoor); got != tc.want {
			t.Errorf("FlowAt(%g) = %d, want %d", tc.outdoor, got, tc.want)
		}
	}
}

func TestFlowOptionsValidate(t *testing.T) {
	good := FlowCurve{Points: [2]FlowCurvePoint{{-5, 60}, {15, 35}}, MinC: 35, MaxC: 60}
	for name, edit := range map[string]func(*Options){
		"limit below range": func(o *Options) { v := 20; o.FlowLimitC = &v },
		"limit above range": func(o *Options) { v := 90; o.FlowLimitC = &v },
		"zero hysteresis":   func(o *Options) { v := 0; o.FlowHysteresisC = &v },
		"zero interval":     func(o *Options) { v := 0; o.FlowMinIntervalSec = &v },
		"zero max age":      func(o *Options) { v := 0; o.OutdoorMaxAgeSec = &v },
		"unknown circuit":   func(o *Options) { o.FlowCurves = map[int]FlowCurve{2: good} },
		"flat curve": func(o *Options) {
			c := good
			c.Points[1].OutdoorC = -5
			o.FlowCurves = map[int]FlowCurve{1: c}
		},
		"min above max": func(o *Options) {
			c := good
			c.MinC, c.MaxC = 50, 40
			o.FlowCurves = map[int]FlowCurve{1: c}
		},
		"point out of range": func(o *Options) {
			c := good
			c.Points[0].FlowC = 85
			o.FlowCurves = map[int]FlowCurve{1: c}
		},
	} {
		in := synthInstall(t)
		edit(&in.Options)
		if err := in.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	in := synthInstall(t)
	in.Options.FlowCurves = map[int]FlowCurve{1: good}
	if err := in.Validate(); err != nil {
		t.Errorf("a valid curve is refused: %v", err)
	}
}

// TestThermostatConfigETagFollowsFlowLimit: the thermostat's d/config ETag
// moves when a served flow limit moves and at no other time, so its next
// conditional GET gets 2.05. Each move queues it one re-read trigger; the valve
// document stays the same and no device gets a d/config write.
func TestThermostatConfigETagFollowsFlowLimit(t *testing.T) {
	// The valve declares its logical id, so a reload can address a write to it.
	install := func(limit *int) *Install {
		in := synthInstall(t)
		zone := 4
		in.Devices[1].Zone = &zone
		in.Options.FlowLimitC = limit
		return in
	}
	in := install(nil)
	ru, va := synthDevices(t, in)
	e := NewEmu(in.EmuConfig(), nil)
	e.OnFrame(moduleReport(t, in, ru, 71, 0x7501))
	drainQueue(t, e, ru)
	etag := func() []byte { return append([]byte(nil), e.cfgDoc[hexEUI(ru)].etag...) }
	valveDoc := e.cfgDoc[hexEUI(va)]
	noWrites := func(step string, rereads int) {
		t.Helper()
		if n := e.Pending(hexEUI(va)); n != 0 {
			t.Errorf("%s: %d downlinks queued to the valve, want none", step, n)
		}
		if total, n := rereadQueue(e, ru); total != rereads || n != rereads {
			t.Errorf("%s: thermostat queue = %d downlinks, %d re-read triggers, want %d of each", step, total, n, rereads)
		}
		if got := e.cfgDoc[hexEUI(va)]; !bytes.Equal(got.body, valveDoc.body) || !bytes.Equal(got.etag, valveDoc.etag) {
			t.Errorf("%s: the valve document moved", step)
		}
	}

	base := etag()
	if len(base) == 0 {
		t.Fatal("no thermostat document after the module report")
	}
	if err := e.EnqueueFlowSetpoint(hexEUI(ru), 1, 48); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(etag(), base) {
		t.Error("serving the default as an explicit value moved the ETag")
	}
	if _, _, err := e.SetInstall(install(nil)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(etag(), base) {
		t.Error("an unchanged reload moved the ETag")
	}
	noWrites("unchanged", 0)

	if err := e.EnqueueFlowSetpoint(hexEUI(ru), 1, 55); err != nil {
		t.Fatal(err)
	}
	moved := etag()
	if bytes.Equal(moved, base) {
		t.Fatal("a new flow limit left the ETag")
	}
	if m := configGET(t, in, e, ru, 0x7502); m.Code != "2.05" {
		t.Errorf("d/config GET = %s, want 2.05", m.Code)
	}
	cond := synthDeviceUplink(t, in, ru, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7503, Token: []byte{0x22},
		Path:    []string{"d", "RU0000000001", "config"},
		Options: []CoAPOption{{Num: 4, Val: base}},
	})
	if m := synthAnswer(t, in, e.OnFrame(cond), 0x7503); m.Code != "2.05" {
		t.Errorf("conditional GET with the old ETag = %s, want 2.05", m.Code)
	}
	cond = synthDeviceUplink(t, in, ru, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7504, Token: []byte{0x23},
		Path:    []string{"d", "RU0000000001", "config"},
		Options: []CoAPOption{{Num: 4, Val: moved}},
	})
	if m := synthAnswer(t, in, e.OnFrame(cond), 0x7504); m.Code != "2.03" {
		t.Errorf("conditional GET with the new ETag = %s, want 2.03", m.Code)
	}
	m := flowGET(t, in, e, ru, 0x7505, nil)
	if got := hex.EncodeToString(m.Payload); got != "204002157c" {
		t.Errorf("c/1/config = %s, want 2040 02 157c", got)
	}
	noWrites("changed", 1)
	drainQueue(t, e, ru)

	// The value survives a restart with the same ETag.
	restarted := NewEmu(in.EmuConfig(), nil)
	if err := restarted.RestoreDesiredState(e.ExportDesiredState()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restarted.cfgDoc[hexEUI(ru)].etag, moved) {
		t.Error("the restored ETag differs")
	}

	e.ClearFlowSetpoint(1)
	if !bytes.Equal(etag(), base) {
		t.Error("back on the default, the ETag differs from the first one")
	}
	limit := 50
	if _, _, err := e.SetInstall(install(&limit)); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(etag(), base) {
		t.Error("a new flow_limit_c left the ETag")
	}
	// The trigger the clear queued still waits, and covers the reload.
	noWrites("new default", 1)
}
