package tado

import "time"

// BatteryWindow is how long a battery reading counts toward the level of a
// device. The level is the highest reading in the window: the captures hold one
// valve dipping 87 and 142 mV below its usual reading, once back 3 minutes later.
const BatteryWindow = 3 * time.Hour

type batteryReading struct {
	at time.Time
	mV int
}

// noteBattery files a tag 0162 reading and re-evaluates the low battery flag.
func (e *EmuState) noteBattery(ns *NodeState, mV int) {
	if mV <= 0 {
		return
	}
	now := e.cfg.Now()
	kept := make([]batteryReading, 0, len(ns.battery)+1)
	for _, r := range ns.battery {
		if now.Sub(r.at) < BatteryWindow {
			kept = append(kept, r)
		}
	}
	ns.battery = append(kept, batteryReading{at: now, mV: mV})
	e.evalLowBattery(ns)
}

// batteryLevel is the highest reading of the window, 0 when none is filed.
func (ns *NodeState) batteryLevel() int {
	level := 0
	for _, r := range ns.battery {
		level = max(level, r.mV)
	}
	return level
}

// evalLowBattery raises the flag when the level falls under the low battery
// voltage and clears it only when the level climbs past the hysteresis.
func (e *EmuState) evalLowBattery(ns *NodeState) {
	level := ns.batteryLevel()
	opts, kind := e.batteryModel(ns.Short)
	limit := opts.LowBatteryMV(kind)
	switch {
	case limit == 0 || level == 0:
		ns.lowBattery = false
	case ns.lowBattery:
		ns.lowBattery = level < opts.lowBatteryClearMV(kind)
	default:
		ns.lowBattery = level < limit
	}
}

// refreshLowBattery re-evaluates every node, so a changed threshold applies
// before the next reading.
func (e *EmuState) refreshLowBattery() {
	for _, ns := range e.nodes {
		e.evalLowBattery(ns)
	}
}

// batteryModel returns the thresholds and the kind that rate a node's battery.
// A node the installation does not declare has no kind, so no battery rating.
func (e *EmuState) batteryModel(short string) (Options, DeviceKind) {
	if e.inst == nil {
		return Options{}, ""
	}
	d, err := e.inst.Resolve(short)
	if err != nil {
		return e.inst.Options, ""
	}
	return e.inst.Options, d.Kind
}
