package tado

import (
	"io"
	"testing"
)

// serialLessEmu returns an emulator whose installation declares bedroom without
// a serial, the way a device declared after a pairing starts.
func serialLessEmu(t *testing.T) *EmuState {
	t.Helper()
	in := fixtureInstall(t)
	dev, err := in.Resolve(bedroomEUIHex)
	if err != nil {
		t.Fatal(err)
	}
	dev.Serial = ""
	return NewEmu(in.EmuConfig(), NewUnhandledLogger(io.Discard))
}

// TestLearnSerialFromOwnRequest: a declared valve without a serial names it in
// its own d/<serial>/... request. The emulator notes it once, and a take empties
// the list.
func TestLearnSerialFromOwnRequest(t *testing.T) {
	e := serialLessEmu(t)
	bedroom := mustHex(bedroomEUIHex)
	for range 2 {
		e.OnFrame(uplinkFrame(t, bedroom, "0.03", []string{"d", "VA1000000005", "sen"}, nil, nil))
	}
	got := e.TakeLearnedSerials()
	if len(got) != 1 || got[bedroomEUIHex] != "VA1000000005" {
		t.Fatalf("learned %v, want only %s VA1000000005", got, bedroomEUIHex)
	}
	if again := e.TakeLearnedSerials(); len(again) != 0 {
		t.Fatalf("second take = %v, want none", again)
	}
}

// TestLearnSerialRefusesAMismatch: a serial whose prefix does not fit the kind,
// a path that is not a serial, a response and a zone-plane request teach
// nothing.
func TestLearnSerialRefusesAMismatch(t *testing.T) {
	bedroom := mustHex(bedroomEUIHex)
	for name, frame := range map[string]func(t *testing.T) []byte{
		"thermostat prefix on a valve": func(t *testing.T) []byte {
			return uplinkFrame(t, bedroom, "0.03", []string{"d", "RU1000000009", "sen"}, nil, nil)
		},
		"not a serial": func(t *testing.T) []byte {
			return uplinkFrame(t, bedroom, "0.03", []string{"d", "fw", "state"}, nil, nil)
		},
		"response": func(t *testing.T) []byte {
			return uplinkFrame(t, bedroom, "2.05", []string{"d", "VA1000000005", "sen"}, nil, nil)
		},
		"zone plane": func(t *testing.T) []byte {
			return uplinkFrame(t, bedroom, "0.01", []string{"z", "p"}, []string{"lid=1"}, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := serialLessEmu(t)
			e.OnFrame(frame(t))
			if got := e.TakeLearnedSerials(); len(got) != 0 {
				t.Fatalf("learned %v, want none", got)
			}
		})
	}
}

// TestLearnSerialKeepsADeclaredOne: a device that already has a serial never
// gets another from the air.
func TestLearnSerialKeepsADeclaredOne(t *testing.T) {
	e := NewEmu(fixtureInstall(t).EmuConfig(), NewUnhandledLogger(io.Discard))
	e.OnFrame(uplinkFrame(t, mustHex(bedroomEUIHex), "0.03", []string{"d", "VA1000000099", "sen"}, nil, nil))
	if got := e.TakeLearnedSerials(); len(got) != 0 {
		t.Fatalf("learned %v for a device with a serial, want none", got)
	}
}
