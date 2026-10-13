package tado

import (
	"reflect"
	"testing"
)

// TestDecodeFaultWord: the word is flags | (state code << 11). The state code is
// one value, so 0x4000 is code 8 and never the single bit 0x4000.
func TestDecodeFaultWord(t *testing.T) {
	s := func(names ...string) []string { return names }
	for _, tc := range []struct {
		word                    uint32
		comms, warnings, faults []string
	}{
		{0, nil, nil, nil},
		{0x02, s("noNeighbour"), nil, nil},
		{0x80, s("noConfigAnswer"), nil, nil},
		{0x82, s("noNeighbour", "noConfigAnswer"), nil, nil},
		{0x04, nil, nil, s("storeWrite")},
		{0x08, nil, nil, s("configWrite")},
		{0x100000, nil, nil, s("stackLow")},
		{0x200000, nil, nil, nil},
		{1 << 11, nil, s("calibrationNoPinContact"), nil},
		{2 << 11, nil, nil, s("calibrationNoEndStop")},
		{3 << 11, nil, nil, s("calibrationTravel")},
		{4 << 11, nil, s("shortTravel"), nil},
		{5 << 11, nil, s("lowVoltage"), nil},
		{6 << 11, nil, nil, s("calibrationPower")},
		{7 << 11, nil, s("lowVoltage"), nil},
		{0x4000, nil, s("notMounted"), nil},
		{9 << 11, nil, nil, s("stall")},
		{10 << 11, nil, s("recalibrating"), nil},
		{11 << 11, nil, nil, s("unknownResult")},
		{12 << 11, nil, nil, s("state12")},
		{13 << 11, nil, s("lowVoltage"), nil},
		{14 << 11, nil, nil, s("earlyStall")},
		{15 << 11, nil, nil, s("state15")},
		{0x4000 | 0x80 | 0x200000, s("noConfigAnswer"), s("notMounted"), nil},
		{0x4800 | 0x04 | 0x02, s("noNeighbour"), nil, s("storeWrite", "stall")},
		{0x0100, nil, nil, s("unknown")},
		{0x4000 | 0x01, nil, s("notMounted"), s("unknown")},
	} {
		got := DecodeFaultWord(tc.word)
		want := FaultStatus{CommsWarnings: tc.comms, Warnings: tc.warnings, Faults: tc.faults}
		if !reflect.DeepEqual(got, want) || got.Fault() != (len(tc.faults) > 0) {
			t.Errorf("DecodeFaultWord(%#x) = %+v fault %v, want %+v", tc.word, got, got.Fault(), want)
		}
	}
}
