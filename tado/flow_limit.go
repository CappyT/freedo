package tado

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultFlowLimitC is the flow limit a circuit follows when a curve has no
// fresh outdoor reading or the adaptation starts, and options.flow_limit_c is
// unset. A circuit that nothing manages is served no 2040 then: the thermostat
// applies MaxTSet 70 C when its boiler interface starts.
const DefaultFlowLimitC = 48

const (
	DefaultOutdoorMaxAgeSec   = 60 * 60
	DefaultFlowHysteresisC    = 1
	DefaultFlowMinIntervalSec = 30 * 60
)

// maxChBoundsTag is the hvac/config tag in which the boiler reports the highest
// CH setpoint it accepts (OpenTherm id 49), in centi-degrees. Tag 046f is not
// that bound: it reads back the MaxTSet the thermostat applied.
const maxChBoundsTag = "0471"

// FlowBounds returns the lowest and highest flow limit, in whole degrees, that
// circuit id is served: the daemon window with its top cut to the CH setpoint
// bound the circuit's heat source reports, when that lies inside the window.
func (e *EmuState) FlowBounds(id int) (minC, maxC int) {
	minC, maxC = int(FlowMinC), int(FlowMaxC)
	if e.inst == nil {
		return minC, maxC
	}
	src, err := e.inst.HeatSource(id)
	if err != nil {
		return minC, maxC
	}
	if ns := e.nodes[hexEUI(src.WireEUI())]; ns != nil && len(ns.raw[maxChBoundsTag]) == 2 {
		if bound := int(be(ns.raw[maxChBoundsTag])) / 100; bound >= minC && bound < maxC {
			maxC = bound
		}
	}
	return minC, maxC
}

// flowCircuits returns the ids of the circuits that can be served a limit: the
// declared ones and the ones with a value of their own.
func (e *EmuState) flowCircuits() []int {
	ids := slices.Collect(maps.Keys(e.flowC))
	if e.inst != nil {
		for i := range e.inst.Circuits {
			ids = append(ids, e.inst.Circuits[i].ID)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// flowBoundsMoved marks the circuits whose served limit is not the one in
// before as not fetched, and moves the thermostat ETag with them.
func (e *EmuState) flowBoundsMoved(before map[int]float64) {
	moved := false
	for id, was := range before {
		if now, ok := e.servedFlowC(id); ok && now != was {
			e.flowFresh[id] = true
			moved = true
		}
	}
	if moved {
		e.refreshThermostatDocuments()
	}
}

// FlowCurve is the weather compensation curve of one heating circuit: the
// line through two points, clamped to MinC..MaxC and rounded to whole degrees,
// because the thermostat truncates the fraction of tag 2040.
type FlowCurve struct {
	Enabled bool              `yaml:"enabled" json:"enabled"`
	Points  [2]FlowCurvePoint `yaml:"points" json:"points"`
	MinC    int               `yaml:"min_c" json:"min_c"`
	MaxC    int               `yaml:"max_c" json:"max_c"`
}

// FlowCurvePoint maps one outdoor temperature to a flow limit.
type FlowCurvePoint struct {
	OutdoorC float64 `yaml:"outdoor_c" json:"outdoor_c"`
	FlowC    int     `yaml:"flow_c" json:"flow_c"`
}

// FlowAt returns the flow limit the curve gives at outdoorC.
func (c FlowCurve) FlowAt(outdoorC float64) int {
	p, q := c.Points[0], c.Points[1]
	slope := float64(q.FlowC-p.FlowC) / (q.OutdoorC - p.OutdoorC)
	v := int(math.Round(float64(p.FlowC) + slope*(outdoorC-p.OutdoorC)))
	return min(max(v, c.MinC), c.MaxC)
}

// Validate checks that the curve is a line and that every flow value it can
// give is one the circuit accepts.
func (c FlowCurve) Validate() error {
	if c.Points[0].OutdoorC == c.Points[1].OutdoorC {
		return errors.New("the two points have the same outdoor temperature")
	}
	for _, v := range []int{c.Points[0].FlowC, c.Points[1].FlowC, c.MinC, c.MaxC} {
		if err := flowInRange(float64(v)); err != nil {
			return errors.New(strings.TrimPrefix(err.Error(), "tado: "))
		}
	}
	if c.MinC > c.MaxC {
		return fmt.Errorf("min_c %d is above max_c %d", c.MinC, c.MaxC)
	}
	return nil
}

// The default weather curve gives a circuit the manual maximum minus a
// reduction that grows with the outdoor temperature:
// reduction = defaultCurveBaseC + defaultCurveSlopeCPerC x outdoor, clamped.
// It is a line fitted to the monthly means of one heating season on one plant,
// not the vendor algorithm.
const (
	// defaultCurveBaseC and defaultCurveSlopeCPerC are the least-squares line
	// through the seven monthly points.
	defaultCurveBaseC      = 3.2
	defaultCurveSlopeCPerC = 0.33
	// defaultCurveMinReductionC keeps the limit at or under the manual maximum.
	// The data stops at a monthly mean of 3 C and the line reaches 0 at -9.7 C,
	// so a colder day gets the full manual maximum.
	defaultCurveMinReductionC = 0
	// defaultCurveMaxReductionC is the plateau: 8 C at 19 to 21 C, and the
	// lower end of the 8 to 9 C seen from 13.6 C, so the limit stays higher.
	defaultCurveMaxReductionC = 8
)

// DefaultFlowAt returns the flow limit the default curve gives at outdoorC under
// the manual maximum limitC, in whole degrees. It stays inside the window a
// circuit accepts.
func DefaultFlowAt(limitC int, outdoorC float64) int {
	reduction := int(math.Round(defaultCurveBaseC + defaultCurveSlopeCPerC*outdoorC))
	reduction = min(max(reduction, defaultCurveMinReductionC), defaultCurveMaxReductionC)
	return max(limitC-reduction, int(FlowMinC))
}

// FlowLimit is the flow limit of a circuit that nothing else sets. ok is false
// while the option is unset: the circuit is then served no limit.
func (o Options) FlowLimit() (limit int, ok bool) {
	if o.FlowLimitC == nil {
		return 0, false
	}
	return *o.FlowLimitC, true
}

// EnabledFlowCurve returns the curve of circuit id when it is enabled.
func (o Options) EnabledFlowCurve(id int) (FlowCurve, bool) {
	c, ok := o.FlowCurves[id]
	return c, ok && c.Enabled
}

// FlowDefaultCurveEnabled reports whether the circuits without a curve of their
// own follow the default weather curve. The curve reduces options.flow_limit_c
// and does nothing while that is unset.
func (o Options) FlowDefaultCurveEnabled() bool {
	return o.FlowDefaultCurve != nil && *o.FlowDefaultCurve && o.FlowLimitC != nil
}

func (o Options) OutdoorMaxAge() time.Duration {
	return optSeconds(o.OutdoorMaxAgeSec, DefaultOutdoorMaxAgeSec)
}

func (o Options) FlowHysteresis() int {
	if o.FlowHysteresisC == nil {
		return DefaultFlowHysteresisC
	}
	return *o.FlowHysteresisC
}

// FlowAutoAdaptEnabled reports whether the flow limit adapts to the room
// feedback.
func (o Options) FlowAutoAdaptEnabled() bool {
	return o.FlowAutoAdapt != nil && *o.FlowAutoAdapt
}

func (o Options) FlowMinInterval() time.Duration {
	return optSeconds(o.FlowMinIntervalSec, DefaultFlowMinIntervalSec)
}

func optSeconds(v *int, def int) time.Duration {
	if v != nil {
		def = *v
	}
	return time.Duration(def) * time.Second
}

func (in *Install) checkFlowOptions(c *issues) {
	if o := in.Options.FlowLimitC; o != nil {
		if err := flowInRange(float64(*o)); err != nil {
			c.errf("options.flow_limit_c", "%s", strings.TrimPrefix(err.Error(), "tado: "))
		}
	}
	if o := in.Options; o.FlowDefaultCurve != nil && *o.FlowDefaultCurve && o.FlowLimitC == nil {
		c.errf("options.flow_default_curve", "needs options.flow_limit_c, the manual maximum the curve reduces")
	}
	for _, o := range []struct {
		path string
		v    *int
	}{
		{"options.outdoor_max_age_sec", in.Options.OutdoorMaxAgeSec},
		{"options.flow_hysteresis_c", in.Options.FlowHysteresisC},
		{"options.flow_min_interval_sec", in.Options.FlowMinIntervalSec},
	} {
		if o.v != nil && *o.v <= 0 {
			c.errf(o.path, "%d is not a positive number", *o.v)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(in.Options.FlowCurves)) {
		path := "options.flow_curves." + strconv.Itoa(id)
		if _, err := in.CircuitByID(id); err != nil {
			c.errf(path, "no circuit %d is declared", id)
		}
		if err := in.Options.FlowCurves[id].Validate(); err != nil {
			c.errf(path, "%v", err)
		}
	}
}
