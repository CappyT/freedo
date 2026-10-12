package tado

// auto_offset: the automatic calibration of a heating zone's temperature
// correction (tag 0140 on the valve) from an external room temperature, at a
// settable interval. It applies in the modes config and dbg2, where the
// correction lives on the valve. Mode setpoint corrects continuously from the
// reading instead, so it refuses the option.

import (
	"maps"
	"slices"
	"strconv"
)

// AutoOffset is the automatic calibration of one heating zone. IntervalSec is
// how often at most the daemon writes a new correction.
type AutoOffset struct {
	Enabled     bool `yaml:"enabled" json:"enabled"`
	IntervalSec int  `yaml:"interval_sec,omitempty" json:"interval_sec,omitempty"`
}

const (
	// DefaultAutoOffsetIntervalSec is one calibration a day: a compensation
	// offset moves 4060 by the written delta, which the heat-up learner reads as
	// a step of the plant, so a slow interval bounds both the writes and the
	// disturbance of the learning.
	DefaultAutoOffsetIntervalSec = 86400
	// MinAutoOffsetIntervalSec is an hour, the shortest interval that keeps the
	// samples of the filter meaningful.
	MinAutoOffsetIntervalSec = 3600
	// MaxAutoOffsetIntervalSec is a week.
	MaxAutoOffsetIntervalSec = 604800
)

// DefaultAutoOffset is the calibration of a zone that states none.
var DefaultAutoOffset = AutoOffset{IntervalSec: DefaultAutoOffsetIntervalSec}

// AutoOffset returns the automatic calibration of zone.
func (o Options) AutoOffset(zone int) AutoOffset {
	if a, ok := o.AutoOffsets[zone]; ok {
		if a.IntervalSec == 0 {
			a.IntervalSec = DefaultAutoOffsetIntervalSec
		}
		return a
	}
	return DefaultAutoOffset
}

// Check reports what is wrong with the settings, or "".
func (a AutoOffset) Check() string {
	if a.IntervalSec < MinAutoOffsetIntervalSec || a.IntervalSec > MaxAutoOffsetIntervalSec {
		return "interval_sec must be within " + strconv.Itoa(MinAutoOffsetIntervalSec) + ".." +
			strconv.Itoa(MaxAutoOffsetIntervalSec)
	}
	return ""
}

// checkAutoOffsets reports an automatic calibration a zone cannot take: an
// unknown zone, the hot water zone, or a zone in offset mode setpoint, which
// already corrects continuously from the reading.
func (in *Install) checkAutoOffsets(c *issues) {
	for _, zone := range slices.Sorted(maps.Keys(in.Options.AutoOffsets)) {
		a := in.Options.AutoOffset(zone)
		path := "options.auto_offset." + strconv.Itoa(zone)
		if a.Enabled {
			switch z, err := in.ZoneByNumber(zone); {
			case err != nil:
				c.errf(path, "no zone %d is declared", zone)
			case z.Kind != ZoneHeating:
				c.errf(path, "zone %d is not a heating zone", zone)
			case in.Options.ZoneOffset(zone).Mode == OffsetModeSetpoint:
				c.errf(path, "zone %d is in offset mode setpoint: that mode corrects continuously from the reading", zone)
			}
		}
		if msg := a.Check(); msg != "" {
			c.errf(path, "%s", msg)
		}
	}
}
