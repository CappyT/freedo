package tado

import (
	"strings"
	"testing"
	"time"
)

// Boiler energy input tests. Every body and time is synthetic.

func f64(v float64) *float64 { return &v }

// TestBoilerSample: the sample carries the last modulation and burner flag, and
// the time of the last hvac/mon report, whatever that report held.
func TestBoilerSample(t *testing.T) {
	now := time.Date(2026, 1, 5, 6, 0, 0, 0, time.UTC)
	e := NewEmu(EmuConfig{TxCounter: 100000, CSLPhase: 0x1234, Now: func() time.Time { return now }}, nil)
	if s := e.BoilerSample(); !s.At.IsZero() || s.ModulationPct != nil || s.Burner != nil {
		t.Fatalf("sample before any report = %+v, want the zero sample", s)
	}

	// A frame with neither attribute still shows the thermostat alive.
	hvacReport(e, resHVACMon, TLV{Tag: "0458", Val: []byte{0x15, 0x78}})
	first := now
	if s := e.BoilerSample(); !s.At.Equal(first) || s.ModulationPct != nil || s.Burner != nil {
		t.Errorf("sample after 0458 alone = %+v, want At=%v and no values", s, first)
	}

	now = now.Add(10 * time.Second)
	hvacReport(e, resHVACMon, TLV{Tag: "0452", Val: []byte{0x00, 0x35}}, TLV{Tag: "0457", Val: []byte{0x01}})
	s := e.BoilerSample()
	if !s.At.Equal(now) || !isPtr(s.ModulationPct, 53) || !isPtr(s.Burner, true) {
		t.Errorf("sample = %+v, want At=%v, modulation 53 and the burner on", s, now)
	}

	// A delta without the two attributes keeps them and moves the time.
	now = now.Add(10 * time.Second)
	hvacReport(e, resHVACMon, TLV{Tag: "0458", Val: []byte{0x02, 0xaa}})
	s = e.BoilerSample()
	if !s.At.Equal(now) || !isPtr(s.ModulationPct, 53) || !isPtr(s.Burner, true) {
		t.Errorf("sample after a delta = %+v, want At=%v with 53 and on kept", s, now)
	}

	// Other boiler bus resources do not move the time.
	stamp := now
	now = now.Add(time.Minute)
	hvacReport(e, resHVACMaint, TLV{Tag: "0463", Val: []byte{0, 0, 1, 0x2c}})
	hvacReport(e, resHVACDHW, TLV{Tag: "045a", Val: []byte{0x0d, 0xac}})
	if s := e.BoilerSample(); !s.At.Equal(stamp) {
		t.Errorf("At after hvac/maint and hvac/mon/dhw = %v, want %v", s.At, stamp)
	}

	// The burner flag is null while the boiler lists it unsupported.
	hvacReport(e, resHVACMon, TLV{Tag: "0481", Val: mustB("045702")})
	if s := e.BoilerSample(); s.Burner != nil || !isPtr(s.ModulationPct, 53) {
		t.Errorf("sample with 0457 unsupported = %+v, want no burner flag and 53 kept", s)
	}
}

// TestBoilerSampleFollowsOneHeatSource: two thermostats that report hvac/mon do
// not mix in the sample. It follows the heat source of the first circuit, and
// only the only reporting thermostat when no circuit names one.
func TestBoilerSampleFollowsOneHeatSource(t *testing.T) {
	in, err := Parse([]byte(`
version: 1
home_id: 7654321
network_key: "000102030405060708090a0b0c0d0e0f"
coordinator:
  eui: "1000000000c51b00"
devices:
  - {kind: thermostat, eui: "2000000000c51b00", serial: RU0000000001, label: hall}
  - {kind: thermostat, eui: "2100000000c51b00", serial: RU0000000002, label: loft}
  - {kind: valve, eui: "3000000000c51b00", serial: VA0000000001, label: study}
  - {kind: valve, eui: "3100000000c51b00", serial: VA0000000002, label: attic}
zones:
  - {number: 4, name: study, kind: heating, circuit: 1, members: [VA0000000001], reporter: VA0000000001}
  - {number: 5, name: attic, kind: heating, circuit: 2, members: [VA0000000002], reporter: VA0000000002}
circuits:
  - {id: 1, name: lower, heat_source: RU0000000001}
  - {id: 2, name: upper, heat_source: RU0000000002}
`))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 5, 6, 0, 0, 0, time.UTC)
	cfg := in.EmuConfig()
	cfg.Now = func() time.Time { return now }
	e := NewEmu(cfg, nil)
	hall, loft := mustB("2000000000c51b00"), mustB("2100000000c51b00")
	report := func(node []byte, modulation, burner byte) {
		t.Helper()
		e.cacheTLVs(e.node(node), []TLV{{Tag: "0452", Val: []byte{0, modulation}}, {Tag: "0457", Val: []byte{burner}}},
			"h/7654321/hvac/mon", ClassData, "", "", "")
	}

	report(loft, 80, 1)
	if s := e.BoilerSample(); !s.At.IsZero() || s.ModulationPct != nil {
		t.Errorf("only the second circuit's thermostat reported: sample %+v, want the zero sample", s)
	}
	now = now.Add(10 * time.Second)
	report(hall, 30, 1)
	first := now
	now = now.Add(10 * time.Second)
	report(loft, 90, 0)
	if s := e.BoilerSample(); !s.At.Equal(first) || !isPtr(s.ModulationPct, 30) || !isPtr(s.Burner, true) {
		t.Errorf("sample = %+v, want the first circuit's report at %v: 30 %% and burner on", s, first)
	}

	// No circuit names a heat source: one reporting thermostat is the source,
	// two are none.
	e.inst.Circuits = nil
	if s := e.BoilerSample(); !s.At.IsZero() {
		t.Errorf("sample with two reporting thermostats and no circuit = %+v, want the zero sample", s)
	}
	delete(e.nodes, hexEUI(hall))
	if s := e.BoilerSample(); !s.At.Equal(now) || !isPtr(s.ModulationPct, 90) || !isPtr(s.Burner, false) {
		t.Errorf("sample with one reporting thermostat = %+v, want its report at %v", s, now)
	}
}

// TestBoilerPowerOptions: the maximum output turns the estimate on, the minimum
// defaults to 0, and a value outside the limits is refused.
func TestBoilerPowerOptions(t *testing.T) {
	if _, _, ok := (Options{}).BoilerPower(); ok {
		t.Error("an installation without a maximum reports a boiler output")
	}
	if maxKW, minKW, ok := (Options{BoilerMaxPowerKW: f64(24)}).BoilerPower(); !ok || maxKW != 24 || minKW != 0 {
		t.Errorf("maximum alone = %v, %v, %v, want 24, 0, true", maxKW, minKW, ok)
	}
	if maxKW, minKW, ok := (Options{BoilerMaxPowerKW: f64(24), BoilerMinPowerKW: f64(2.5)}).BoilerPower(); !ok || maxKW != 24 || minKW != 2.5 {
		t.Errorf("both = %v, %v, %v, want 24, 2.5, true", maxKW, minKW, ok)
	}
	for name, edit := range map[string]func(*Options){
		"zero maximum":          func(o *Options) { o.BoilerMaxPowerKW = f64(0) },
		"negative maximum":      func(o *Options) { o.BoilerMaxPowerKW = f64(-3) },
		"maximum in watts":      func(o *Options) { o.BoilerMaxPowerKW = f64(24000) },
		"negative minimum":      func(o *Options) { o.BoilerMinPowerKW = f64(-1) },
		"minimum at maximum":    func(o *Options) { o.BoilerMaxPowerKW, o.BoilerMinPowerKW = f64(24), f64(24) },
		"minimum above maximum": func(o *Options) { o.BoilerMaxPowerKW, o.BoilerMinPowerKW = f64(24), f64(30) },
	} {
		in := synthInstall(t)
		edit(&in.Options)
		if err := in.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	in := synthInstall(t)
	in.Options.BoilerMaxPowerKW, in.Options.BoilerMinPowerKW = f64(24), f64(2.5)
	if err := in.Validate(); err != nil {
		t.Errorf("a valid output is refused: %v", err)
	}
}

// TestBoilerGasOptions: the gas flow turns the count on only with both values,
// and a value outside the limits is refused.
func TestBoilerGasOptions(t *testing.T) {
	if _, _, ok := (Options{}).BoilerGas(); ok {
		t.Error("an installation without a gas flow reports one")
	}
	if _, _, ok := (Options{BoilerMaxGasM3H: f64(2.7)}).BoilerGas(); ok {
		t.Error("a maximum alone turns the gas count on")
	}
	if _, _, ok := (Options{BoilerMinGasM3H: f64(0.3)}).BoilerGas(); ok {
		t.Error("a minimum alone turns the gas count on")
	}
	if maxM3H, minM3H, ok := (Options{BoilerMaxGasM3H: f64(2.7), BoilerMinGasM3H: f64(0)}).BoilerGas(); !ok || maxM3H != 2.7 || minM3H != 0 {
		t.Errorf("both = %v, %v, %v, want 2.7, 0, true", maxM3H, minM3H, ok)
	}
	for name, edit := range map[string]func(*Options){
		"zero maximum":          func(o *Options) { o.BoilerMaxGasM3H = f64(0) },
		"negative maximum":      func(o *Options) { o.BoilerMaxGasM3H = f64(-1) },
		"maximum in l/h":        func(o *Options) { o.BoilerMaxGasM3H = f64(2700) },
		"negative minimum":      func(o *Options) { o.BoilerMinGasM3H = f64(-0.1) },
		"minimum in l/h":        func(o *Options) { o.BoilerMinGasM3H = f64(300) },
		"minimum at maximum":    func(o *Options) { o.BoilerMaxGasM3H, o.BoilerMinGasM3H = f64(2.7), f64(2.7) },
		"minimum above maximum": func(o *Options) { o.BoilerMaxGasM3H, o.BoilerMinGasM3H = f64(2.7), f64(3) },
	} {
		in := synthInstall(t)
		edit(&in.Options)
		if err := in.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, edit := range map[string]func(*Options){
		"both":         func(o *Options) { o.BoilerMaxGasM3H, o.BoilerMinGasM3H = f64(2.7), f64(0.3) },
		"zero minimum": func(o *Options) { o.BoilerMaxGasM3H, o.BoilerMinGasM3H = f64(2.7), f64(0) },
		"at the limit": func(o *Options) { o.BoilerMaxGasM3H = f64(BoilerMaxGasLimitM3H) },
	} {
		in := synthInstall(t)
		edit(&in.Options)
		if err := in.Validate(); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

// TestFuelUnit: the unit reads m3 while unset, only m3, l and kg are valid, and
// the flow limit stays one value per hour in any of them.
func TestFuelUnit(t *testing.T) {
	if got := (Options{}).Fuel(); got != FuelM3 {
		t.Errorf("default unit = %q, want m3", got)
	}
	for _, unit := range []string{"m3", "l", "kg"} {
		in := synthInstall(t)
		in.Options.FuelUnit, in.Options.BoilerMaxGasM3H = unit, f64(BoilerMaxGasLimitM3H)
		if err := in.Validate(); err != nil {
			t.Errorf("%s: refused: %v", unit, err)
		}
	}
	for _, unit := range []string{"gallon", "L", "m^3", " kg"} {
		in := synthInstall(t)
		in.Options.FuelUnit = unit
		if err := in.Validate(); err == nil {
			t.Errorf("%q: accepted", unit)
		}
	}
	in := synthInstall(t)
	in.Options.FuelUnit, in.Options.BoilerMaxGasM3H = "kg", f64(BoilerMaxGasLimitM3H+1)
	if err := in.Validate(); err == nil || !strings.Contains(err.Error(), "kg/h") {
		t.Errorf("a flow over the limit: %v, want a refusal in kg/h", err)
	}
}
