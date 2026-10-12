package tado

import (
	"encoding/hex"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// installConfigWrite is the part of a device's d/config the installation owns.
// Everything else in the document (an operator's write, a restored desired
// state) survives a reload because the write is merged, never substituted.
// Tag 02b3 is not in it: every merge resolves that tag (mergeDeviceConfig).
// ok=false names a wall thermostat whose interface module id is not known:
// it holds no document, since the only safe 015d is the one it reported. Its
// d/config GET is served withheldModuleDocument.
func (e *EmuState) installConfigWrite(dev *Device) (ConfigWrite, bool) {
	w := e.installConfigBase(dev)
	switch dev.Kind {
	case KindValve:
		module := valveInterfaceModule
		w.InterfaceModuleID = &module
	case KindThermostat:
		module, ok := e.thermostatModule(dev)
		if !ok {
			return ConfigWrite{}, false
		}
		w.InterfaceModuleID = &module
	}
	return w, true
}

// installConfigBase is installConfigWrite without tag 015d.
func (e *EmuState) installConfigBase(dev *Device) ConfigWrite {
	home := e.inst.HomeID
	w := ConfigWrite{
		HomeID:    &home,
		NodeTable: initialNodeTable(e.inst, dev),
	}
	// A valve that already holds a document takes a new offset from
	// PushUTCOffset, one valve at a time: each write homes its motor.
	if _, held := e.cfgDoc[hexEUI(dev.WireEUI())]; !held || dev.Kind == KindThermostat {
		w.UTCOffsetMin = e.UTCOffset()
	}
	return w
}

// withheldModuleDocument is the d/config document served to a declared wall
// thermostat whose interface module id is not known: the installation's
// document without tag 015d. Every uplink after d/config waits for a 2.05 or
// 2.03 of it (thermostat 0x1e6dc, response handler 0x1e4b4), and hvac/codes,
// which carries 015d, is one of them: an error answer would leave the id
// unknown for good. The tag handler (0x1e2e8) writes only the tags a body
// carries, so the thermostat keeps its own 015d. The document is not held:
// once the id is known, the held one replaces it and its ETag differs.
func (e *EmuState) withheldModuleDocument(dst []byte) (cfgSnapshot, bool) {
	if e.inst == nil {
		return cfgSnapshot{}, false
	}
	key := hexEUI(dst)
	dev, err := e.inst.Resolve(key)
	if err != nil || dev.Kind != KindThermostat {
		return cfgSnapshot{}, false
	}
	_, snap, err := e.mergeDeviceConfigOn(cfgSnapshot{}, key, e.installConfigBase(dev))
	if err != nil {
		e.log.Log("Config", dev.Ref(), "", rsnBuildFail+" d/config "+err.Error(), "")
		return cfgSnapshot{}, false
	}
	return snap, true
}

// SetInstall replaces the installation the emulator coordinates and returns the
// wire EUIs (hex, sorted) of the devices that appeared and of those that
// disappeared.
//
// The caller owns validation: in must already have passed Validate or
// ValidateCommissionable, because the network key and the coordinator address
// are taken as given here. Only the installation-derived state moves. The frame
// counters, the MAC sequence, the datagram tag, the clock, the CSL phase and
// the zone-proxy settings are bootstrap or live state and keep their values,
// and no frame builder is invoked.
//
// Calling it twice with the same installation changes nothing: the derived
// d/config documents merge to the same bytes, so no second config write is
// queued and no ETag moves.
func (e *EmuState) SetInstall(in *Install) (added, removed []string, err error) {
	if in == nil {
		return nil, nil, errors.New("tado: SetInstall: nil installation")
	}
	key, self := in.NetworkKey(), in.CoordinatorEUI()
	if len(key) != 16 {
		return nil, nil, errors.New("tado: SetInstall: no 16-byte network key resolved, validate the installation first")
	}
	if len(self) != 8 {
		return nil, nil, errors.New("tado: SetInstall: no usable coordinator EUI, validate the installation first")
	}

	before, after := declaredEUIs(e.inst), declaredEUIs(in)
	added, removed = missingFrom(after, before), missingFrom(before, after)
	poke := openWindowChanges(e.inst, in)
	oldZones := declaredZones(e.inst)
	newMembers := in.EmuZones()
	newZones := zoneSets(newMembers)

	for _, key := range removed {
		e.dropDevice(key)
	}
	e.rehomeScheduleDocuments(e.inst, in)

	oldFlow, oldSet := e.defaultFlowC()
	e.inst = in
	if flow, set := e.defaultFlowC(); set && (flow != oldFlow || !oldSet) {
		for i := range in.Circuits {
			if _, own := e.flowC[in.Circuits[i].ID]; !own {
				e.flowFresh[in.Circuits[i].ID] = true
			}
		}
	}
	e.cfg.Install = in
	e.cfg.Key = key
	e.cfg.SelfEUI = self
	e.cfg.Zones = newMembers
	e.cfg.UTCOffsetMin = in.Options.UTCOffsetMin
	e.loadUTCZone()
	e.utcOffset = e.resolveUTCOffset(e.cfg.Now())
	e.key = key
	e.selfEUI = self

	e.rebuildZones(oldZones, newZones)
	e.refreshLowBattery()
	written := e.refreshConfigDocuments()
	e.pokeZoneConfigReaders(poke, written)
	return added, removed, nil
}

// openWindowChanges returns the zones whose open window detection the new
// installation moves. A zone the new installation does not declare is left out:
// its members lose the zone, not a setting. Only a heating document carries the
// two tags, so a hot water zone reaches no member with them.
func openWindowChanges(before, after *Install) []int {
	if before == nil {
		return nil
	}
	var out []int
	for i := range after.Zones {
		z := &after.Zones[i]
		if z.Kind != ZoneHeating {
			continue
		}
		old, err := before.ZoneByNumber(z.Number)
		if err != nil {
			continue
		}
		if old.OpenWindowEnabled() != z.OpenWindowEnabled() ||
			old.OpenWindowSeconds() != z.OpenWindowSeconds() {
			out = append(out, z.Number)
		}
	}
	return out
}

// pokeZoneConfigReaders writes every member of a changed zone its own d/config
// document, unchanged. The capture shows this write after an open window
// detection change: the device answers 2.04 and then reads d/<serial>/config
// and h/<home>/z/N/config again, which is how the new zone document reaches it.
// written names the members this reload already wrote, so no device gets two.
func (e *EmuState) pokeZoneConfigReaders(zones []int, written map[string]bool) {
	for _, zone := range zones {
		members, err := e.inst.ZoneMembers(zone)
		if err != nil {
			continue
		}
		for _, dev := range members {
			wire := dev.WireEUI()
			key := hexEUI(wire)
			if written[key] || e.refusesConfigWrite(wire) {
				continue
			}
			id, ok := e.deviceID(wire, e.nodes[key])
			if !ok {
				continue
			}
			written[key] = true
			if err := e.EnqueueConfig(key, id, ConfigWrite{Resend: true}); err != nil {
				e.log.Log("Config", dev.Ref(), "", rsnBuildFail+" d/config poke "+err.Error(), "")
			}
		}
	}
}

// rebuildZones re-derives the zone routing table from the new installation. A
// zone that survives keeps the members we learned from the traffic we routed,
// which are the ones no declaration ever put there; a zone that left the
// installation loses its entry and its cached state.
func (e *EmuState) rebuildZones(oldZones, newZones map[int]map[string]bool) {
	next := make(map[int]map[string]bool, len(e.zones))
	for zone, members := range e.zones {
		_, wasDeclared := oldZones[zone]
		if _, isDeclared := newZones[zone]; wasDeclared && !isDeclared {
			continue
		}
		learned := map[string]bool{}
		for member := range members {
			if !oldZones[zone][member] {
				learned[member] = true
			}
		}
		if len(learned) > 0 {
			next[zone] = learned
		}
	}
	e.zones = next
	for zone, members := range newZones {
		for member := range members {
			wire, err := hex.DecodeString(member)
			if err != nil {
				continue
			}
			e.addZoneEUI(zone, wire)
		}
	}
	for zone := range oldZones {
		if _, ok := newZones[zone]; !ok {
			e.dropZoneState(zone)
		}
	}
}

func (e *EmuState) dropZoneState(zone int) {
	for key, timer := range e.boostTimers {
		if timer.id == zone {
			delete(e.boostTimers, key)
		}
	}
	delete(e.zoneRep, zone)
	delete(e.zoneFallback, zone)
	prefix := strconv.Itoa(zone) + "|"
	for key := range e.zoneState {
		if strings.HasPrefix(key, prefix) {
			delete(e.zoneState, key)
		}
	}
}

// dropDevice forgets everything the emulator holds about one wire EUI except
// its NodeState: a node that leaves the installation may still be on the air,
// and hiding it would hide the very thing an operator has to see to re-declare
// or replace it. A learned interface module id stays too: it is a property of
// the hardware, and a re-declared thermostat must not wait to report it again.
func (e *EmuState) dropDevice(key string) {
	delete(e.cfgDoc, key)
	delete(e.schedDoc, key)
	delete(e.childLock, key)
	delete(e.tempUnitF, key)
	delete(e.offlineSched, key)
	delete(e.utcSettled, key)
	delete(e.sessions, key)
	delete(e.obsRoles, key)
	delete(e.queue, key)
	delete(e.history, key)
	delete(e.fwLock, key)
	delete(e.fwReply, key)
	delete(e.fwPut, key)
	delete(e.fwGet, key)
	delete(e.fwReads, key)
	delete(e.serialSeen, key)
	delete(e.serialLearned, key)
	prefix := key + "|"
	for k := range e.boostTimers {
		if strings.HasPrefix(k, prefix) {
			delete(e.boostTimers, k)
		}
	}
	for k := range e.getRes {
		if strings.HasPrefix(k, prefix) {
			delete(e.getRes, k)
		}
	}
	for k := range e.getCode {
		if strings.HasPrefix(k, prefix) {
			delete(e.getCode, k)
		}
	}
}

// rehomeScheduleDocuments gives each device whose heating zones the reload
// changed the sched document of its new zone. The document is keyed by device,
// so without this a moved member keeps serving the program of the zone it left,
// and the new zone reads that program back from it. The new program is the one
// the first member that stayed in the zone holds; a zone with none leaves the
// device without a document. With no earlier installation there is no move to
// see, and the documents a restore brought back stay.
func (e *EmuState) rehomeScheduleDocuments(before, after *Install) {
	if before == nil {
		return
	}
	oldZones, newZones := heatingZonesByDevice(before), heatingZonesByDevice(after)
	changed := map[string]bool{}
	for key, zones := range newZones {
		if !slices.Equal(zones, oldZones[key]) {
			changed[key] = true
		}
	}
	for key := range oldZones {
		if _, ok := newZones[key]; !ok {
			changed[key] = true
		}
	}
	next := make(map[string]cfgSnapshot, len(changed))
	for key := range changed {
		zones := newZones[key]
		if len(zones) == 0 {
			continue
		}
		members, err := after.ZoneMembers(zones[0])
		if err != nil {
			continue
		}
		for _, m := range members {
			if changed[m.wireHex()] {
				continue
			}
			if snap, ok := e.schedDoc[m.wireHex()]; ok {
				next[key] = snap
				break
			}
		}
	}
	for key := range changed {
		if snap, ok := next[key]; ok {
			e.schedDoc[key] = snap
		} else {
			delete(e.schedDoc, key)
		}
	}
}

// heatingZonesByDevice maps the wire EUI hex of each member of a heating zone
// to the heating zones that list it, in declaration order.
func heatingZonesByDevice(in *Install) map[string][]int {
	out := map[string][]int{}
	if in == nil {
		return out
	}
	for i := range in.Zones {
		z := &in.Zones[i]
		if z.Kind != ZoneHeating {
			continue
		}
		for _, ref := range z.Members {
			d, err := in.Resolve(ref)
			if err != nil {
				continue
			}
			out[d.wireHex()] = append(out[d.wireHex()], z.Number)
		}
	}
	return out
}

// refreshConfigDocuments merges the installation-derived part of d/config into
// every declared device's document, through the same path an operator's config
// write takes, so a changed 015e election reaches the device as a write. It
// returns the devices the merge put a write on the queue for.
func (e *EmuState) refreshConfigDocuments() map[string]bool {
	written := map[string]bool{}
	for i := range e.inst.Devices {
		dev := &e.inst.Devices[i]
		wire := dev.WireEUI()
		if len(wire) != 8 {
			continue
		}
		key := hexEUI(wire)
		w, ok := e.installConfigWrite(dev)
		if !ok {
			continue
		}
		if id, ok := e.deviceID(wire, e.nodes[key]); ok && !e.refusesConfigWrite(wire) {
			queued, err := e.enqueueConfig(key, id, w)
			if err != nil {
				e.log.Log("Config", dev.Ref(), "", rsnBuildFail+" d/config "+err.Error(), "")
			}
			written[key] = queued
			continue
		}
		// No logical id yet, or a device that refuses the write: keep the served
		// document current and leave the push to the first uplink that names one.
		_, snap, err := e.mergeDeviceConfig(key, w)
		if err != nil {
			e.log.Log("Config", dev.Ref(), "", rsnBuildFail+" d/config "+err.Error(), "")
			continue
		}
		e.setConfigDocument(key, snap)
	}
	return written
}

func declaredEUIs(in *Install) map[string]bool {
	out := map[string]bool{}
	if in == nil {
		return out
	}
	for i := range in.Devices {
		if wire := in.Devices[i].WireEUI(); len(wire) == 8 {
			out[hexEUI(wire)] = true
		}
	}
	return out
}

func declaredZones(in *Install) map[int]map[string]bool {
	if in == nil {
		return map[int]map[string]bool{}
	}
	return zoneSets(in.EmuZones())
}

func zoneSets(members map[int][]string) map[int]map[string]bool {
	out := make(map[int]map[string]bool, len(members))
	for zone, list := range members {
		set := make(map[string]bool, len(list))
		for _, m := range list {
			set[m] = true
		}
		out[zone] = set
	}
	return out
}

// missingFrom returns the keys of a that b does not hold, sorted.
func missingFrom(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
