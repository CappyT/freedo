package tado

import "strings"

// learnSerial notes the serial that a declared device without one names in its
// own d/<serial>/... request. A device declared after a pairing carries no
// serial until then. The prefix must fit the kind: VA for a valve, RU for a
// wall thermostat. Each device is noted once per run.
func (e *EmuState) learnSerial(m *CoAPMessage, src []byte) {
	if e.inst == nil || !isRequestCode(m.Code) {
		return
	}
	first, rest, ok := strings.Cut(m.Path, "/")
	if !ok || first != "d" {
		return
	}
	serial, _, _ := strings.Cut(rest, "/")
	key := hexEUI(src)
	if _, seen := e.serialSeen[key]; seen {
		return
	}
	dev, err := e.inst.Resolve(key)
	if err != nil || dev.Serial != "" || !serialFitsKind(serial, dev.Kind) {
		return
	}
	e.serialSeen[key] = serial
	e.serialLearned[key] = serial
}

func serialFitsKind(serial string, kind DeviceKind) bool {
	if reSerial.FindString(serial) != serial {
		return false
	}
	switch kind {
	case KindValve:
		return strings.HasPrefix(serial, "VA")
	case KindThermostat:
		return strings.HasPrefix(serial, "RU")
	}
	return false
}

// TakeLearnedSerials returns the serials learnSerial noted since the last take,
// keyed by wire EUI hex, for the owner to write into the installation.
func (e *EmuState) TakeLearnedSerials() map[string]string {
	if len(e.serialLearned) == 0 {
		return nil
	}
	out := e.serialLearned
	e.serialLearned = map[string]string{}
	return out
}
