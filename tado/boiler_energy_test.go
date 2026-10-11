package tado

import (
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
