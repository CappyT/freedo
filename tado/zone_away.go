package tado

import (
	"maps"
	"math"
	"slices"
	"strconv"
)

// The room setpoint range of the tado app. The captured writes span 5.00 C
// (away) to 25.00 C (boost).
const (
	RoomMinC = 5.0
	RoomMaxC = 25.0
)

// AwayTemperature returns the temperature zone keeps in away, and false when the
// zone is off in away: no value, or the room minimum. The cloud writes that
// default as 61e0=0 with no 6200 (capture-away-20261011); a higher value goes
// out as 61e0=1 and 6200=<value>.
func (o Options) AwayTemperature(zone int) (float64, bool) {
	if c := o.AwayTemperatures[zone]; c > RoomMinC {
		return c, true
	}
	return 0, false
}

func (in *Install) checkAwayTemperatures(c *issues) {
	for _, zone := range slices.Sorted(maps.Keys(in.Options.AwayTemperatures)) {
		path := "options.away_temperature_c." + strconv.Itoa(zone)
		switch z, err := in.ZoneByNumber(zone); {
		case err != nil:
			c.errf(path, "no zone %d is declared", zone)
		case z.Kind != ZoneHeating:
			c.errf(path, "zone %d is not a heating zone", zone)
		}
		if t := in.Options.AwayTemperatures[zone]; math.IsNaN(t) || t < RoomMinC || t > RoomMaxC {
			c.errf(path, "%g is out of range %g..%g C", t, RoomMinC, RoomMaxC)
		}
	}
}
