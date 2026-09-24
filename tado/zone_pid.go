package tado

import (
	"maps"
	"slices"
	"strconv"
)

// ZonePID holds the gains of the zone controller, tags 6080 (Kp), 60a0 (Ki)
// and 60c0 (Kd) of h/<home>/z/N/config, each a uint16 on the wire.
//
// zone_ctrl_pid_init@0x2b5f8 of the valve reads the three tags with no clamp
// and zeroes the controller state, the integrator with it. The valve runs it
// after each 2.05 of the document (event 0x16d), and re-reads the document
// after each hourly d/config check, so a new ETag reaches it with no write.
// ctrl_pct_update@0x31970 computes, in centi-degrees and percent:
//
//	Kp*(SP-T)/2000 + Ki*integ/30000 - Kd*(T-Tref)/2000, clamped to 0..100
//
// with integ clamped to 0..3000000/Ki, and to 0 when Ki is 0.
type ZonePID struct {
	Kp int `yaml:"kp" json:"kp"`
	Ki int `yaml:"ki" json:"ki"`
	Kd int `yaml:"kd" json:"kd"`
}

// DefaultZonePID holds the gains the commercial coordinator serves.
var DefaultZonePID = ZonePID{Kp: 1000, Ki: 500, Kd: 1500}

// zonePIDGainMax bounds each gain, a uint16 on the wire.
const zonePIDGainMax = 0xffff

// ZonePID returns the controller gains of zone.
func (o Options) ZonePID(zone int) ZonePID {
	if pid, ok := o.ZonePIDs[zone]; ok {
		return pid
	}
	return DefaultZonePID
}

// Check reports what is wrong with the gains, or "".
func (p ZonePID) Check() string {
	for _, g := range []struct {
		name  string
		value int
	}{{"kp", p.Kp}, {"ki", p.Ki}, {"kd", p.Kd}} {
		if g.value < 0 || g.value > zonePIDGainMax {
			return g.name + " must be within 0.." + strconv.Itoa(zonePIDGainMax)
		}
	}
	// With no proportional and no integral term the output never rises above
	// 0 %, so the zone can never heat.
	if p.Kp == 0 && p.Ki == 0 {
		return "kp and ki cannot both be 0"
	}
	return ""
}

func (in *Install) checkZonePIDs(c *issues) {
	for _, zone := range slices.Sorted(maps.Keys(in.Options.ZonePIDs)) {
		path := "options.zone_pid." + strconv.Itoa(zone)
		switch z, err := in.ZoneByNumber(zone); {
		case err != nil:
			c.errf(path, "no zone %d is declared", zone)
		case z.Kind != ZoneHeating:
			c.errf(path, "zone %d is not a heating zone", zone)
		}
		if msg := in.Options.ZonePIDs[zone].Check(); msg != "" {
			c.errf(path, "%s", msg)
		}
	}
}
