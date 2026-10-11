package tado

import "time"

// What the boiler energy estimate reads of the engine: the two hvac/mon values
// that decide the heat output, and the time of the report that carried the
// last of them. The estimate itself is the daemon's (internal/daemon).

// BoilerMaxPowerLimitKW bounds options.boiler_max_power_kw: no residential
// boiler is above it, so a larger value is a typo in W.
const BoilerMaxPowerLimitKW = 500

// BoilerPower returns the output of the boiler at 100 % and at 0 % modulation,
// in kW. ok is false while no maximum is configured.
func (o Options) BoilerPower() (maxKW, minKW float64, ok bool) {
	if o.BoilerMaxPowerKW == nil {
		return 0, 0, false
	}
	if o.BoilerMinPowerKW != nil {
		minKW = *o.BoilerMinPowerKW
	}
	return *o.BoilerMaxPowerKW, minKW, true
}

func (in *Install) checkBoilerPower(c *issues) {
	o := in.Options
	if v := o.BoilerMaxPowerKW; v != nil && !(*v > 0 && *v <= BoilerMaxPowerLimitKW) {
		c.errf("options.boiler_max_power_kw", "%v is not a power in kW within 0..%d", *v, BoilerMaxPowerLimitKW)
	}
	if v := o.BoilerMinPowerKW; v != nil && !(*v >= 0 && *v <= BoilerMaxPowerLimitKW) {
		c.errf("options.boiler_min_power_kw", "%v is not a power in kW within 0..%d", *v, BoilerMaxPowerLimitKW)
	}
	if o.BoilerMaxPowerKW != nil && o.BoilerMinPowerKW != nil && *o.BoilerMinPowerKW >= *o.BoilerMaxPowerKW {
		c.errf("options.boiler_min_power_kw", "%v is not below boiler_max_power_kw %v", *o.BoilerMinPowerKW, *o.BoilerMaxPowerKW)
	}
}

// BoilerSample is the boiler state as of the last hvac/mon report: the relative
// modulation (tag 0452) and the burner flag (tag 0457). A report carries only
// the attributes that changed, so both are the last values reported. Each is
// nil while the boiler never reported it or lists it as unsupported. At is the
// time of the last hvac/mon report, zero before the first.
type BoilerSample struct {
	At            time.Time
	ModulationPct *int
	Burner        *bool
}

// BoilerSample returns the sample of the wall thermostat that reported hvac/mon
// last.
func (e *EmuState) BoilerSample() BoilerSample {
	var from *NodeState
	for _, ns := range e.nodes {
		if !ns.hvacMonAt.IsZero() && (from == nil || ns.hvacMonAt.After(from.hvacMonAt)) {
			from = ns
		}
	}
	if from == nil {
		return BoilerSample{}
	}
	var b BoilerView
	e.fillBoiler(&b, from)
	s := BoilerSample{At: from.hvacMonAt, ModulationPct: b.ModulationPct}
	if flag := b.Mon["boiler_flag"]; flag != nil {
		on := *flag != 0
		s.Burner = &on
	}
	return s
}
