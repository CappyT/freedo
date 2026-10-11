package tado

import "time"

// What the boiler energy estimate reads of the engine: the two hvac/mon values
// that decide the heat output and the gas flow, and the time of the report that
// carried the last of them. The estimate itself is the daemon's (internal/daemon).

// BoilerMaxPowerLimitKW bounds options.boiler_max_power_kw: no residential
// boiler is above it, so a larger value is a typo in W.
const BoilerMaxPowerLimitKW = 500

// BoilerMaxGasLimitM3H bounds options.boiler_max_gas_m3h: 500 kW burns about
// 53 m3/h of natural gas, 50 l/h of heating oil and 40 kg/h of propane, so a
// larger value is a typo in a smaller unit.
const BoilerMaxGasLimitM3H = 60

// The units of options.fuel_unit.
const (
	FuelM3    = "m3"
	FuelLitre = "l"
	FuelKg    = "kg"
)

// Fuel returns the unit of the fuel flow and counter, m3 while the option is
// unset.
func (o Options) Fuel() string {
	if o.FuelUnit == "" {
		return FuelM3
	}
	return o.FuelUnit
}

func (in *Install) checkFuelUnit(c *issues) {
	switch in.Options.Fuel() {
	case FuelM3, FuelLitre, FuelKg:
	default:
		c.errf("options.fuel_unit", "%q is not one of %s, %s, %s", in.Options.FuelUnit, FuelM3, FuelLitre, FuelKg)
	}
}

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

// BoilerGas returns the fuel flow at the burner at 100 % and at 0 % modulation,
// in Fuel per hour. ok is false until both are configured: unlike the output, the
// minimum has no safe default, because a data sheet minimum is never 0.
func (o Options) BoilerGas() (maxM3H, minM3H float64, ok bool) {
	if o.BoilerMaxGasM3H == nil || o.BoilerMinGasM3H == nil {
		return 0, 0, false
	}
	return *o.BoilerMaxGasM3H, *o.BoilerMinGasM3H, true
}

func (in *Install) checkBoilerPower(c *issues) {
	o := in.Options
	checkBoilerRange(c, "power_kw", "a power in kW", o.BoilerMaxPowerKW, o.BoilerMinPowerKW, BoilerMaxPowerLimitKW)
	checkBoilerRange(c, "gas_m3h", "a fuel flow in "+o.Fuel()+"/h", o.BoilerMaxGasM3H, o.BoilerMinGasM3H, BoilerMaxGasLimitM3H)
}

// checkBoilerRange checks a maximum and a minimum option of the boiler: the
// maximum is above 0 and within limit, the minimum is within limit and below
// the maximum when both are set.
func checkBoilerRange(c *issues, name, what string, hi, lo *float64, limit float64) {
	maxKey, minKey := "boiler_max_"+name, "boiler_min_"+name
	if v := hi; v != nil && !(*v > 0 && *v <= limit) {
		c.errf("options."+maxKey, "%v is not %s within 0..%v", *v, what, limit)
	}
	if v := lo; v != nil && !(*v >= 0 && *v <= limit) {
		c.errf("options."+minKey, "%v is not %s within 0..%v", *v, what, limit)
	}
	if hi != nil && lo != nil && *lo >= *hi {
		c.errf("options."+minKey, "%v is not below %s %v", *lo, maxKey, *hi)
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
