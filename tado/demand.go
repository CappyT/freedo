package tado

import (
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Heat demand, as the air shows it. The coordinator observes and acknowledges
// the whole chain; it originates none of it.
//
//	valve       PUT h/<home>/z/N/act   40a0            zone demand, 0..100
//	valve       2.05 to the thermostat 4060 40a0 0135  same demand, relayed by us
//	thermostat  PUT h/<home>/c/C/act   4080 2090       circuit demand, heat active
//	thermostat  PUT z/cpe?lid=N        4160            circuit heat percent, per zone
//
// The role that elects the publishing device travels in d/config tag 015e as
// [role bitmask][zone id]. Tags 6340, 6060, 6400 and 2040 never appear.

// zoneDemandTags and circuitDemandTags gate the merge: a report on one of these
// resources carries other attributes too, and only these belong to the chain.
var (
	zoneDemandTags    = map[string]bool{"40a0": true, "4160": true, "4060": true}
	circuitDemandTags = map[string]bool{"4080": true, "2090": true}
)

// demandReport is the last known value of one zone or one circuit. Every report
// resource sends only the attributes that changed, so a frame merges into this
// and never replaces it.
type demandReport struct {
	raw     map[string][]byte
	source  string // short address of the device that last published into it
	updated time.Time
}

func (r *demandReport) merge(tlvs []TLV, want map[string]bool) bool {
	changed := false
	for _, t := range tlvs {
		if !want[t.Tag] || len(t.Val) == 0 {
			continue
		}
		r.raw[t.Tag] = append([]byte(nil), t.Val...)
		changed = true
	}
	return changed
}

// observeReports files one observed CoAP message into the demand chain and the
// role table. It runs on every message we parse, relayed traffic included: the
// thermostat addresses z/cpe to a zone peer and we only route it, so the
// ingestion cannot sit behind the "addressed to us" test.
func (e *EmuState) observeReports(m *CoAPMessage, ns *NodeState) {
	if len(m.Payload) == 0 {
		return
	}
	tlvs := parseTLVList(m.Payload)
	if len(tlvs) == 0 {
		return
	}
	if id, ok := circuitReportPath(m.Path); ok {
		e.mergeDemand(e.circuitRep, id, tlvs, circuitDemandTags, ns.Short)
	}
	if id, ok := e.demandZone(m, ns); ok {
		e.mergeDemand(e.zoneRep, id, tlvs, zoneDemandTags, ns.Short)
	}
	e.ingestNodeRoles(tlvs, ns)
	e.observeFallback(m, tlvs, ns)
}

func (e *EmuState) mergeDemand(store map[int]*demandReport, id int, tlvs []TLV, want map[string]bool, source string) {
	rep := store[id]
	if rep == nil {
		rep = &demandReport{raw: map[string][]byte{}}
	}
	if !rep.merge(tlvs, want) {
		return
	}
	rep.source, rep.updated = source, e.cfg.Now()
	store[id] = rep
}

// ZoneReport returns the zone temperature (tag 4060) and demand (tag 40a0)
// the zone last reported, and when its report last changed. ok is false until
// the zone reports a 4060.
func (e *EmuState) ZoneReport(zone int) (tempC float64, demandPct int, updated time.Time, ok bool) {
	rep := e.zoneRep[zone]
	if rep == nil || len(rep.raw["4060"]) == 0 {
		return 0, 0, time.Time{}, false
	}
	return centiC(rep.raw["4060"]), rawInt(rep.raw["40a0"]), rep.updated, true
}

// ZoneChain returns what ZoneReport does plus tag 4160, the heat percent the
// thermostat publishes for the zone. demandPct and heatPct are nil until the
// zone reports that tag.
func (e *EmuState) ZoneChain(zone int) (tempC float64, demandPct, heatPct *int, updated time.Time, ok bool) {
	rep := e.zoneRep[zone]
	if rep == nil || len(rep.raw["4060"]) == 0 {
		return 0, nil, nil, time.Time{}, false
	}
	pct := func(tag string) *int {
		if len(rep.raw[tag]) == 0 {
			return nil
		}
		v := rawInt(rep.raw[tag])
		return &v
	}
	return centiC(rep.raw["4060"]), pct("40a0"), pct("4160"), rep.updated, true
}

// demandZone names the zone a payload belongs to. The relayed 2.05 carries
// neither a path nor a query, so the publisher's own zone answers for it.
func (e *EmuState) demandZone(m *CoAPMessage, ns *NodeState) (int, bool) {
	if id, ok := zoneReportPath(m.Path); ok {
		return id, true
	}
	if id, ok := queryID(m.Query); ok {
		return id, true
	}
	if e.inst != nil {
		if d, err := e.inst.Resolve(ns.Short); err == nil {
			if d.Zone != nil {
				return *d.Zone, true
			}
			if zones := e.inst.zonesOf(d); len(zones) == 1 {
				return zones[0], true
			}
		}
	}
	if ns.LastID > 0 {
		return ns.LastID, true
	}
	return 0, false
}

// zoneReportPath reads N from a home-plane "h/<home>/z/N/..." path. The device
// plane spells the zone in the query instead, so "z/s" and "z/cpe" miss here.
func zoneReportPath(path string) (int, bool) {
	segs := strings.Split(path, "/")
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] != "z" {
			continue
		}
		if n, err := strconv.Atoi(segs[i+1]); err == nil {
			return n, true
		}
	}
	return 0, false
}

// circuitReportPath reads C from "h/<home>/c/C/act". Only the activity resource
// counts: "c/C/config" is the flow-setpoint document the bridge serves.
func circuitReportPath(path string) (int, bool) {
	segs := strings.Split(path, "/")
	for i := 0; i+2 < len(segs); i++ {
		if segs[i] != "c" || segs[i+2] != "act" {
			continue
		}
		if n, err := strconv.Atoi(segs[i+1]); err == nil {
			return n, true
		}
	}
	return 0, false
}

// ingestNodeRoles files the 015e records of an observed d/config document
// against the node that published it. Tag 015e is the only place the role
// bitmask reaches a device, so an observed document supersedes the one we hold.
func (e *EmuState) ingestNodeRoles(tlvs []TLV, ns *NodeState) {
	recs := nodeTableRecs(tlvs)
	if len(recs) == 0 {
		return
	}
	e.obsRoles[ns.WireLE] = recs
}

// nodeTableRecs collects the 015e records of a config document, in wire order.
func nodeTableRecs(tlvs []TLV) []NodeTableRec {
	var out []NodeTableRec
	for _, t := range tlvs {
		if t.Tag == "015e" && len(t.Val) == 2 {
			out = append(out, NodeTableRec{Role: t.Val[0], Node: t.Val[1]})
		}
	}
	return out
}

// roleTables returns the 015e records per node (wire EUI hex): what a device
// published about itself, else what our own d/config document holds for it.
func (e *EmuState) roleTables() map[string][]NodeTableRec {
	out := make(map[string][]NodeTableRec, len(e.cfgDoc)+len(e.obsRoles))
	for key, snap := range e.cfgDoc {
		if recs := nodeTableRecs(parseTLVList(snap.body)); len(recs) > 0 {
			out[key] = recs
		}
	}
	for key, recs := range e.obsRoles {
		out[key] = recs
	}
	return out
}

// NodeRoles returns the 015e role records the coordinator holds for one target.
func (e *EmuState) NodeRoles(target string) ([]NodeTableRec, error) {
	wire, _, err := e.resolve(target)
	if err != nil {
		return nil, err
	}
	return e.roleTables()[hexEUI(wire)], nil
}

// roleElected is the 015e bit that separates the elected member from a plain
// one. The coordinator writes 0x0d for the elected member and 0x05 for the
// others (initialNodeTable), and the capture's publishing valve reads 0d0a, so
// bit 3 is the difference. It is not tag 6340's election mask, which reads
// (flags & 0x0c) == 4 and would answer the other way round.
const roleElected = 0x08

// zoneReporters maps a zone number to the short address of the device whose
// 015e record for that zone elects it. Ties resolve on the short address so the
// answer does not depend on map order.
func (e *EmuState) zoneReporters() map[int]string {
	out := map[int]string{}
	for key, recs := range e.roleTables() {
		short := shortFromHexEUI(key)
		if short == "" {
			continue
		}
		for _, r := range recs {
			if r.Role&roleElected == 0 {
				continue
			}
			zone := int(r.Node)
			if cur, ok := out[zone]; !ok || short < cur {
				out[zone] = short
			}
		}
	}
	return out
}

func shortFromHexEUI(key string) string {
	wire, err := hex.DecodeString(key)
	if err != nil {
		return ""
	}
	return ShortFromWire(wire)
}

// demandZones returns every zone number the state knows: the declared ones plus
// any the air reported, sorted.
func (e *EmuState) demandZones() []int {
	seen := map[int]bool{}
	var out []int
	add := func(n int) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if e.inst != nil {
		for i := range e.inst.Zones {
			add(e.inst.Zones[i].Number)
		}
	}
	for n := range e.zoneRep {
		add(n)
	}
	for n := range e.zoneFallback {
		add(n)
	}
	sort.Ints(out)
	return out
}

// undeclaredCircuits returns the circuit ids the air reported that the
// installation does not declare, sorted.
func (e *EmuState) undeclaredCircuits() []int {
	declared := map[int]bool{}
	if e.inst != nil {
		for i := range e.inst.Circuits {
			declared[e.inst.Circuits[i].ID] = true
		}
	}
	var out []int
	for id := range e.circuitRep {
		if !declared[id] {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}
