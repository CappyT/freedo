package tado

import (
	"encoding/hex"
	"maps"
	"slices"
	"time"
)

// HVACMonNode is the boiler bus state of one node that a daemon restart would
// lose: the thermostat sends hvac/mon as delta reports, so a value that does
// not change again stays unknown until the next change. Tags maps a tag hex to
// the hex of its last raw value.
type HVACMonNode struct {
	Node string // wire EUI hex
	At   time.Time
	Tags map[string]string
}

// ExportHVACMon lists, in short-address order, each node that sent hvac/mon,
// with the raw values of its boiler bus tags. unsupported is left out.
func (e *EmuState) ExportHVACMon() []HVACMonNode {
	var out []HVACMonNode
	for _, ns := range e.sortedNodes() {
		if ns.hvacMonAt.IsZero() {
			continue
		}
		tags := map[string]string{}
		for tag, val := range ns.raw {
			if _, ok := hvacTagByTag[tag]; ok && len(val) > 0 {
				tags[tag] = hex.EncodeToString(val)
			}
		}
		out = append(out, HVACMonNode{Node: ns.WireLE, At: ns.hvacMonAt, Tags: tags})
	}
	return out
}

// RestoreHVACMon puts back what ExportHVACMon listed. It takes only a node the
// installation declares, sets a tag only where the node has no value yet and
// the report time only while it is zero, so a report that arrived first wins.
// It returns the tags it set, or false when the node is not declared.
func (e *EmuState) RestoreHVACMon(s HVACMonNode) (int, bool) {
	wire, err := hex.DecodeString(s.Node)
	if err != nil || len(wire) != 8 || e.inst == nil {
		return 0, false
	}
	if _, err := e.inst.Resolve(s.Node); err != nil {
		return 0, false
	}
	ns := e.node(wire)
	set := 0
	for _, tag := range slices.Sorted(maps.Keys(s.Tags)) {
		val, err := hex.DecodeString(s.Tags[tag])
		if _, ok := hvacTagByTag[tag]; !ok || err != nil || len(val) == 0 || len(ns.raw[tag]) > 0 {
			continue
		}
		ns.raw[tag] = val
		set++
	}
	if ns.hvacMonAt.IsZero() {
		ns.hvacMonAt = s.At
	}
	return set, true
}
