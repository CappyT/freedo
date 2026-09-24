package tado

import (
	"encoding/binary"
	"errors"
	"sort"
	"strconv"
)

const (
	interfaceModuleTag = "015d"
	// valveInterfaceModule is the tag 015d every valve holds. Any other value
	// leaves the valve without its controller after the next restart.
	valveInterfaceModule uint16 = 112
	// interfaceModuleTeardown is the 015d value on which the device tears its
	// controller down.
	interfaceModuleTeardown uint16 = 0x7fff
	// stubModule selects the STM32 image that drives no boiler bus.
	stubModule    uint16 = 202
	familyUnknown        = "unknown"
)

// moduleFamily maps the interface module ids a wall thermostat STM32 overlay
// answers to the boiler bus family it drives (catalogue 0x08007f28).
var moduleFamily = map[uint16]string{
	71: "opentherm",
	0:  "ebus_a", 1: "ebus_a", 5: "ebus_a", 6: "ebus_a", 7: "ebus_a", 9: "ebus_a",
	80: "ebus_a", 83: "ebus_a", 200: "ebus_a",
	121: "ebus_b",
	4:   "ebus_c", 122: "ebus_c",
	20: "km", 22: "km", 23: "km", 24: "km", 50: "km",
	41: "ht_bus", 43: "ht_bus", 44: "ht_bus",
	120: "ems",
	100: "bsb", 110: "bsb", 111: "bsb", 123: "bsb",
	31: "scom", 32: "scom",
	3: "analog", 10: "analog", 11: "analog", 12: "analog", 30: "analog",
	40: "analog", 42: "analog", 60: "analog", 61: "analog",
	stubModule: "stub",
}

// InterfaceModuleFamily names the boiler bus family an interface module id
// selects, "unknown" for an id no overlay answers.
func InterfaceModuleFamily(v uint16) string {
	if f, ok := moduleFamily[v]; ok {
		return f
	}
	return familyUnknown
}

// relayModule reports the eBus A ids that also drive the nRF relay.
func relayModule(v uint16) bool {
	switch v {
	case 1, 5, 6, 7, 9:
		return true
	}
	return false
}

// InterfaceModuleIssue is a wall thermostat whose reported interface module id
// was refused, or learned with a warning.
type InterfaceModuleIssue struct {
	Device  string `json:"device"`
	Module  uint16 `json:"module"`
	Family  string `json:"family"`
	Refused bool   `json:"refused"`
	Reason  string `json:"reason"`
}

// validInterfaceModule rejects the 015d values no device may be given. The
// device reads 0 as its own default, which on the wall thermostat is a different
// STM32 image.
func validInterfaceModule(v uint16) error {
	switch v {
	case 0:
		return errors.New("tado: interface module id 0 selects the firmware default image, not a module")
	case interfaceModuleTeardown:
		return errors.New("tado: interface module id 0x7fff tears the device's controller down")
	}
	return nil
}

// validThermostatModule is validInterfaceModule for the wall thermostat. 112 is
// the valve's module: no thermostat STM32 image matches it, and serving it stops
// the boiler interface.
func validThermostatModule(v uint16) error {
	if err := validInterfaceModule(v); err != nil {
		return err
	}
	if v == valveInterfaceModule {
		return errors.New("tado: interface module id 112 is the valve's, no wall thermostat image matches it")
	}
	if _, ok := moduleFamily[v]; !ok {
		return errors.New("tado: interface module id " + strconv.Itoa(int(v)) + " selects no known wall thermostat image")
	}
	return nil
}

// thermostatModule returns the interface module id of a declared wall
// thermostat. An id the thermostat never reported is looked up in the d/config
// document cached for it. With neither, the thermostat has no document to be
// served, so the cached one is dropped.
func (e *EmuState) thermostatModule(dev *Device) (uint16, bool) {
	key := hexEUI(dev.WireEUI())
	if v, ok := e.moduleID[key]; ok {
		return v, true
	}
	if v, ok := tlvU16(parseTLVList(e.cfgDoc[key].body), interfaceModuleTag); ok {
		if e.learnModuleID(dev, v, "cached d/config") {
			return v, true
		}
	}
	delete(e.cfgDoc, key)
	return 0, false
}

// noteConfigWithheld logs, once per device until a thermostat's module id is
// learned, a d/config GET answered without the held document: what names the
// answer.
func (e *EmuState) noteConfigWithheld(dst []byte, srcLbl, what string) {
	key := hexEUI(dst)
	if e.withheldLogged[key] {
		return
	}
	e.withheldLogged[key] = true
	e.log.Log("Data", srcLbl, "self", rsnConfigWithheld+"="+what, "")
}

// learnModuleID records the interface module id a wall thermostat reported. It
// reports whether the value is held: one no thermostat may carry is refused.
// The stub image, and a relay id after a report of another family, are held
// with a warning.
func (e *EmuState) learnModuleID(dev *Device, v uint16, source string) bool {
	key := hexEUI(dev.WireEUI())
	if err := validThermostatModule(v); err != nil {
		e.flagModule(dev, v, true, err.Error())
		e.log.Log("Config", dev.Ref(), "", rsnModuleRefused+"="+strconv.Itoa(int(v))+" from "+source, "")
		return false
	}
	old, known := e.moduleID[key]
	switch issue, flagged := e.moduleIssue[key]; {
	case v == stubModule:
		e.flagModule(dev, v, false, "the stub image drives no boiler bus")
	case relayModule(v) && known && InterfaceModuleFamily(old) != InterfaceModuleFamily(v):
		e.flagModule(dev, v, false, "the id drives the relay, the thermostat reported "+
			InterfaceModuleFamily(old)+" ("+strconv.Itoa(int(old))+") before")
	case flagged && (issue.Refused || issue.Module != v):
		delete(e.moduleIssue, key)
	}
	if known && old == v {
		return true
	}
	e.moduleID[key] = v
	e.moduleLearned = true
	delete(e.withheldLogged, key)
	e.log.Log("Config", dev.Ref(), "", rsnModuleLearned+"="+strconv.Itoa(int(v))+" from "+source, "")
	return true
}

// flagModule records a refused or suspicious interface module id and logs the
// warning once per change.
func (e *EmuState) flagModule(dev *Device, v uint16, refused bool, reason string) {
	key := hexEUI(dev.WireEUI())
	issue := InterfaceModuleIssue{Device: dev.Ref(), Module: v, Family: InterfaceModuleFamily(v), Refused: refused, Reason: reason}
	if old, ok := e.moduleIssue[key]; ok && old == issue {
		return
	}
	e.moduleIssue[key] = issue
	if !refused {
		e.log.Log("Config", dev.Ref(), "", rsnModuleWarning+"="+strconv.Itoa(int(v))+" "+reason, "")
	}
}

// InterfaceModuleIssues lists the wall thermostats whose last reported
// interface module id was refused or learned with a warning, and the stored ids
// a restore dropped, by device.
func (e *EmuState) InterfaceModuleIssues() []InterfaceModuleIssue {
	out := make([]InterfaceModuleIssue, 0, len(e.moduleIssue))
	for key, issue := range e.moduleIssue {
		if issue.Device == "" {
			issue.Device = key
			if e.inst != nil {
				if dev, err := e.inst.Resolve(key); err == nil {
					issue.Device = dev.Ref()
				}
			}
		}
		out = append(out, issue)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out
}

// observeModuleID learns the interface module id a wall thermostat carries in
// its hvac/codes report, in a d/config document it sends, or in a response.
func (e *EmuState) observeModuleID(wire []byte, m *CoAPMessage, tlvs []TLV) {
	if e.inst == nil {
		return
	}
	res := normalizeResource(m.Path)
	if res != "hvac/codes" && res != "d/config" && !isResponseCode(m.Code) {
		return
	}
	v, ok := tlvU16(tlvs, interfaceModuleTag)
	if !ok {
		return
	}
	dev, err := e.inst.Resolve(hexEUI(wire))
	if err != nil || dev.Kind != KindThermostat {
		return
	}
	source := res
	if source == "" {
		source = "response " + m.Code
	}
	if e.learnModuleID(dev, v, source) {
		e.refreshThermostatDocument(dev)
	}
}

// refreshThermostatDocument rebuilds the document served to a wall thermostat
// on the interface module id it reported, or drops it when there is none.
func (e *EmuState) refreshThermostatDocument(dev *Device) {
	key := hexEUI(dev.WireEUI())
	w, ok := e.installConfigWrite(dev)
	if !ok {
		return
	}
	_, snap, err := e.mergeDeviceConfig(key, w)
	if err != nil {
		delete(e.cfgDoc, key)
		e.log.Log("Config", dev.Ref(), "", rsnBuildFail+" d/config "+err.Error(), "")
		return
	}
	e.cfgDoc[key] = snap
}

// refreshThermostatDocuments runs refreshThermostatDocument on every declared
// wall thermostat, so a restored document never outlives the id it must carry.
func (e *EmuState) refreshThermostatDocuments() {
	if e.inst == nil {
		return
	}
	for i := range e.inst.Devices {
		if dev := &e.inst.Devices[i]; dev.Kind == KindThermostat {
			e.refreshThermostatDocument(dev)
		}
	}
}

func tlvU16(tlvs []TLV, tag string) (uint16, bool) {
	for _, t := range tlvs {
		if t.Tag == tag && len(t.Val) == 2 {
			return binary.BigEndian.Uint16(t.Val), true
		}
	}
	return 0, false
}

// TakeInterfaceModuleLearned reports whether a thermostat's interface module id
// was learned since the last call, so the owner persists it.
func (e *EmuState) TakeInterfaceModuleLearned() bool {
	learned := e.moduleLearned
	e.moduleLearned = false
	return learned
}

// UnknownInterfaceModules names the declared wall thermostats whose interface
// module id is not known yet. None of them is served a d/config document.
func (e *EmuState) UnknownInterfaceModules() []string {
	if e.inst == nil {
		return nil
	}
	var out []string
	for i := range e.inst.Devices {
		dev := &e.inst.Devices[i]
		if dev.Kind != KindThermostat {
			continue
		}
		if _, ok := e.moduleID[hexEUI(dev.WireEUI())]; !ok {
			out = append(out, dev.Ref())
		}
	}
	sort.Strings(out)
	return out
}
