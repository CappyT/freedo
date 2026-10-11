package tado

import (
	"bytes"
	"encoding/hex"
	"strconv"
	"strings"
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

// TestServedFlowWithoutLimit: a circuit nothing sets is served no 2040 while
// options.flow_limit_c is unset, and the option once it is set.
func TestServedFlowWithoutLimit(t *testing.T) {
	in := synthInstall(t)
	ru, _ := synthDevices(t, in)
	e := NewEmu(in.EmuConfig(), nil)
	m := flowGET(t, in, e, ru, 0x7401, nil)
	if m.Code != "2.05" || len(m.Payload) != 0 {
		t.Fatalf("c/1/config = %s %x, want an empty 2.05", m.Code, m.Payload)
	}
	if _, served, _ := e.ServedFlow(1); served {
		t.Error("a circuit without a limit reads as served")
	}

	limit := 55
	next := synthInstall(t)
	next.Options.FlowLimitC = &limit
	if _, _, err := e.SetInstall(next); err != nil {
		t.Fatal(err)
	}
	if _, served, fetched := e.ServedFlow(1); !served || fetched {
		t.Errorf("a new default reads served=%v fetched=%v, want true and false", served, fetched)
	}
	m = flowGET(t, in, e, ru, 0x7402, nil)
	if got := hex.EncodeToString(m.Payload); got != "204002157c" {
		t.Errorf("c/1/config after flow_limit_c=55 = %s, want 2040 02 157c", got)
	}
	if _, _, fetched := e.ServedFlow(1); !fetched {
		t.Error("the GET did not mark the value fetched")
	}

	if _, _, err := e.SetInstall(synthInstall(t)); err != nil {
		t.Fatal(err)
	}
	if m = flowGET(t, in, e, ru, 0x7403, nil); len(m.Payload) != 0 {
		t.Errorf("c/1/config after the limit left = %x, want an empty 2.05", m.Payload)
	}
	if n := e.FlowPending(); n != 0 {
		t.Errorf("FlowPending = %d with no limit served, want 0", n)
	}
}

// TestThermostatConfigETagWithoutFlowLimit: with options.flow_limit_c unset the
// ETag hashes no default. A circuit value moves it, clearing the value restores
// it and leaves no pending fetch, and a reload that sets the limit moves it.
func TestThermostatConfigETagWithoutFlowLimit(t *testing.T) {
	in := synthInstall(t)
	ru, _ := synthDevices(t, in)
	e := NewEmu(in.EmuConfig(), nil)
	e.OnFrame(moduleReport(t, in, ru, 71, 0x7601))
	etag := func() []byte { return append([]byte(nil), e.cfgDoc[hexEUI(ru)].etag...) }
	base := etag()
	if len(base) == 0 {
		t.Fatal("no thermostat document after the module report")
	}

	if err := e.EnqueueFlowSetpoint(hexEUI(ru), 1, DefaultFlowLimitC); err != nil {
		t.Fatal(err)
	}
	moved := etag()
	if bytes.Equal(moved, base) {
		t.Error("a first circuit value left the ETag")
	}
	e.ClearFlowSetpoint(1)
	if !bytes.Equal(etag(), base) {
		t.Error("after the clear, the ETag differs from the first one")
	}
	if n := e.FlowPending(); n != 0 {
		t.Errorf("FlowPending = %d after the clear, want 0", n)
	}

	limit := DefaultFlowLimitC
	next := synthInstall(t)
	next.Options.FlowLimitC = &limit
	if _, _, err := e.SetInstall(next); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(etag(), base) {
		t.Error("a reload that sets flow_limit_c left the ETag")
	}
}

// TestFlowBoundsFollowBoiler: the top of the served flow limit is the CH
// setpoint bound the heat source reports in tag 0471. A bound outside the
// window is ignored, a served value above the bound is cut to it, and the
// thermostat ETag and the pending fetch follow. Tag 046f, the MaxTSet read
// back, bounds nothing.
func TestFlowBoundsFollowBoiler(t *testing.T) {
	in := synthInstall(t)
	limit := 70
	in.Options.FlowLimitC = &limit
	ru, _ := synthDevices(t, in)
	e := NewEmu(in.EmuConfig(), nil)
	e.OnFrame(moduleReport(t, in, ru, 71, 0x7701))
	report := func(tag string, centi uint16) {
		t.Helper()
		e.cacheTLVs(e.node(ru), []TLV{{Tag: tag, Val: []byte{byte(centi >> 8), byte(centi)}}},
			"h/7654321/hvac/config", ClassData, "", "", "")
	}
	etag := func() []byte { return append([]byte(nil), e.cfgDoc[hexEUI(ru)].etag...) }
	want := func(step string, minC, maxC int, served float64) {
		t.Helper()
		if lo, hi := e.FlowBounds(1); lo != minC || hi != maxC {
			t.Errorf("%s: bounds %d..%d, want %d..%d", step, lo, hi, minC, maxC)
		}
		if got, ok, _ := e.ServedFlow(1); !ok || got != served {
			t.Errorf("%s: served %g %v, want %g", step, got, ok, served)
		}
	}
	want("before a report", 30, 75, 70)
	base := etag()

	report("046f", 4801)
	want("after the MaxTSet read-back", 30, 75, 70)
	if !bytes.Equal(etag(), base) {
		t.Error("the MaxTSet read-back moved the ETag")
	}

	report(maxChBoundsTag, 6000)
	want("after a bound of 60", 30, 60, 60)
	if bytes.Equal(etag(), base) {
		t.Error("a bound under the served limit left the ETag")
	}
	if n := e.FlowPending(); n != 1 {
		t.Errorf("FlowPending = %d after the bound, want 1", n)
	}
	if m := flowGET(t, in, e, ru, 0x7702, nil); hex.EncodeToString(m.Payload) != "2040021770" {
		t.Errorf("c/1/config = %x, want 2040 02 1770", m.Payload)
	}

	for _, centi := range []uint16{2500, 9000} {
		report(maxChBoundsTag, centi)
		want("a bound of "+strconv.Itoa(int(centi)/100), 30, 75, 70)
		if !bytes.Equal(etag(), base) {
			t.Errorf("a bound of %d outside the window moved the ETag", centi/100)
		}
	}

	report(maxChBoundsTag, 7200)
	want("a bound above the limit", 30, 72, 70)
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

// TestDefaultFlowAt: the default curve gives the manual maximum minus a
// reduction of 3.2 + 0.33 per degree outdoors, rounded and clamped to 0..8 C.
func TestDefaultFlowAt(t *testing.T) {
	for _, tc := range []struct {
		limit   int
		outdoor float64
		want    int
	}{
		{56, -60, 56}, // the line is negative, no reduction
		{56, -10, 56}, // 0.1, rounds to 0
		{56, -9.7, 56},
		{56, -5, 54}, // 1.55 rounds to 2
		{56, 0, 53},  // 3.2 rounds to 3
		{56, 3, 52},  // 4.19 rounds to 4
		{56, 10, 49}, // 6.5 rounds up to 7
		{56, 10.6, 49},
		{56, 14, 48}, // 7.82 rounds to 8, the cap
		{56, 15.6, 48},
		{56, 16, 48},
		{56, 20, 48}, // 9.8, capped at 8
		{56, 60, 48},
		{60, 3, 56},
		{33, 20, 30}, // 25 is under the window, clamped to its lowest value
		{30, 20, 30},
	} {
		if got := DefaultFlowAt(tc.limit, tc.outdoor); got != tc.want {
			t.Errorf("DefaultFlowAt(%d, %g) = %d, want %d", tc.limit, tc.outdoor, got, tc.want)
		}
	}
	for limit := int(FlowMinC); limit <= int(FlowMaxC); limit++ {
		for outdoor := -60.0; outdoor <= 60; outdoor += 0.25 {
			got := DefaultFlowAt(limit, outdoor)
			if lowest := max(limit-defaultCurveMaxReductionC, int(FlowMinC)); got > limit || got < lowest {
				t.Fatalf("DefaultFlowAt(%d, %g) = %d, want %d..%d", limit, outdoor, got, lowest, limit)
			}
		}
	}
}

// TestFlowDefaultCurveNeedsTheManualMaximum: the switch is refused without
// options.flow_limit_c, and it does nothing while that is unset.
func TestFlowDefaultCurveNeedsTheManualMaximum(t *testing.T) {
	on, off, limit := true, false, 56
	in := synthInstall(t)
	in.Options.FlowDefaultCurve = &on
	if err := in.Validate(); err == nil || !strings.Contains(err.Error(), "options.flow_default_curve") {
		t.Errorf("the default curve without flow_limit_c: %v, want an error that names it", err)
	}
	if in.Options.FlowDefaultCurveEnabled() {
		t.Error("the default curve is enabled without flow_limit_c")
	}
	in.Options.FlowLimitC = &limit
	if err := in.Validate(); err != nil {
		t.Errorf("the default curve with flow_limit_c: %v", err)
	}
	if !in.Options.FlowDefaultCurveEnabled() {
		t.Error("the default curve is not enabled")
	}
	in.Options.FlowDefaultCurve = &off
	if in.Options.FlowDefaultCurveEnabled() || (Options{}).FlowDefaultCurveEnabled() {
		t.Error("the default curve is enabled while off or unset")
	}
	in.Options.FlowLimitC = nil
	if err := in.Validate(); err != nil {
		t.Errorf("the switch off without flow_limit_c: %v", err)
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
	def := 48
	in := install(&def)
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
	if _, _, err := e.SetInstall(install(&def)); err != nil {
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
