package tado

import (
	"maps"
	"math"
	"slices"
	"strconv"
)

// The accepted total radiator power of a room, in watts.
const (
	RadiatorMinW = 100.0
	RadiatorMaxW = 20000.0
)

// RadiatorW returns the total nominal radiator power of zone, and false when the
// zone states none.
func (o Options) RadiatorW(zone int) (float64, bool) {
	w, ok := o.RadiatorsW[zone]
	return w, ok
}

func (in *Install) checkRadiatorsW(c *issues) {
	for _, zone := range slices.Sorted(maps.Keys(in.Options.RadiatorsW)) {
		path := "options.radiator_w." + strconv.Itoa(zone)
		switch z, err := in.ZoneByNumber(zone); {
		case err != nil:
			c.errf(path, "no zone %d is declared", zone)
		case z.Kind != ZoneHeating:
			c.errf(path, "zone %d is not a heating zone", zone)
		}
		if w := in.Options.RadiatorsW[zone]; math.IsNaN(w) || w < RadiatorMinW || w > RadiatorMaxW {
			c.errf(path, "%g is out of range %g..%g W", w, RadiatorMinW, RadiatorMaxW)
		}
	}
}
