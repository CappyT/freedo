package tado

import (
	"reflect"
	"testing"
	"time"
)

func TestDesiredStateRoundTrip(t *testing.T) {
	in := fixtureInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	bedroom, err := in.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	ru, err := in.Resolve("RU1000000001")
	if err != nil {
		t.Fatal(err)
	}
	bedroomKey := hexEUI(bedroom.WireEUI())
	if err := e.EnqueueChildLock(bedroomKey, 10, true); err != nil {
		t.Fatal(err)
	}
	fahrenheit := true
	if err := e.EnqueueConfig(bedroomKey, 10, ConfigWrite{TempUnitF: &fahrenheit}); err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueFlowSetpoint(hexEUI(ru.WireEUI()), 1, 48); err != nil {
		t.Fatal(err)
	}
	if err := e.SetSchedule(bedroomKey, []ScheduleProgram{{DayMask: 0x7f, Entries: []ScheduleEntry{{Minute: 0, SetpointCenti: 500, Enabled: true}, {Minute: 360, SetpointCenti: 700, Enabled: true}}}}); err != nil {
		t.Fatal(err)
	}
	e.zoneState[zoneKey(10, "z/s")] = []byte{1, 2, 3}
	e.utcSettled[bedroomKey] = 60
	if err := e.EnqueueBoost(bedroomKey, 10, true); err != nil {
		t.Fatal(err)
	}
	e.sessions[bedroomKey] = issuedSession{id: sessionID(bedroom.WireEUI()), at: time.Unix(1_700_000_000, 0).UTC()}

	want := e.ExportDesiredState()
	copyTest := e.ExportDesiredState()
	copyTest.ChildLock[bedroomKey] = false
	copyTest.Config[bedroomKey] = DesiredConfigDocument{Body: []byte{0}, ETag: []byte{0}}
	copyTest.Schedule[bedroomKey] = DesiredScheduleDocument{Body: []byte{0}, ETag: []byte{0}}
	copyTest.ZoneState[zoneKey(10, "z/s")][0] = 9
	copyTest.Boost[bedroomKey] = DesiredBoostTimer{ID: 9, ExpiresAt: time.Unix(1, 0)}
	copyTest.Session[bedroomKey].ID[0] ^= 0xff
	if got := e.ExportDesiredState(); !reflect.DeepEqual(got, want) {
		t.Fatal("export aliases emulator state")
	}

	restored := NewEmu(in.EmuConfig(), nil)
	if err := restored.RestoreDesiredState(want); err != nil {
		t.Fatal(err)
	}
	if got := restored.ExportDesiredState(); !reflect.DeepEqual(got, want) {
		t.Errorf("restored state differs\n got: %#v\nwant: %#v", got, want)
	}
	// A node first heard after the restore carries the served session id.
	if ns := restored.node(bedroom.WireEUI()); !reflect.DeepEqual(ns.SessionID, want.Session[bedroomKey].ID) {
		t.Errorf("restored node session = %x, want %x", ns.SessionID, want.Session[bedroomKey].ID)
	}
}

func TestRestoreDesiredStateRejectsInvalidSession(t *testing.T) {
	in := fixtureInstall(t)
	bedroom, err := in.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	key, id := hexEUI(bedroom.WireEUI()), sessionID(bedroom.WireEUI())
	for _, c := range []struct {
		name, key string
		session   DesiredSession
	}{
		{"short id", key, DesiredSession{ID: id[:3], IssuedAt: time.Unix(1, 0)}},
		{"long id", key, DesiredSession{ID: make([]byte, 9), IssuedAt: time.Unix(1, 0)}},
		{"no time", key, DesiredSession{ID: id}},
		{"no wire EUI", "bedroom", DesiredSession{ID: id, IssuedAt: time.Unix(1, 0)}},
	} {
		e := NewEmu(in.EmuConfig(), nil)
		bad := e.ExportDesiredState()
		bad.Session[c.key] = c.session
		if err := e.RestoreDesiredState(bad); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}

func TestRestoreDesiredStateRejectsInvalidState(t *testing.T) {
	e := NewEmu(fixtureInstall(t).EmuConfig(), nil)
	before := e.ExportDesiredState()
	bad := before
	bad.Version++
	if err := e.RestoreDesiredState(bad); err == nil {
		t.Fatal("invalid version accepted")
	}
	if got := e.ExportDesiredState(); !reflect.DeepEqual(got, before) {
		t.Fatal("invalid state changed emulator")
	}
}

// The zone resource keys are the last rule validateDesiredState applies, so a
// populated emulator proves that no earlier field is assigned before it runs.
func TestRestoreDesiredStateKeepsStateOnLateValidationFailure(t *testing.T) {
	in := fixtureInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	bedroom, err := in.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	ru, err := in.Resolve("RU1000000001")
	if err != nil {
		t.Fatal(err)
	}
	bedroomKey := hexEUI(bedroom.WireEUI())
	if err := e.EnqueueChildLock(bedroomKey, 10, true); err != nil {
		t.Fatal(err)
	}
	fahrenheit := true
	if err := e.EnqueueConfig(bedroomKey, 10, ConfigWrite{TempUnitF: &fahrenheit}); err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueFlowSetpoint(hexEUI(ru.WireEUI()), 1, 48); err != nil {
		t.Fatal(err)
	}
	if err := e.SetSchedule(bedroomKey, []ScheduleProgram{{DayMask: 0x7f, Entries: []ScheduleEntry{{Minute: 0, SetpointCenti: 500, Enabled: true}}}}); err != nil {
		t.Fatal(err)
	}
	e.zoneState[zoneKey(10, "z/s")] = []byte{1, 2, 3}
	e.utcSettled[bedroomKey] = 60
	if err := e.EnqueueBoost(bedroomKey, 10, true); err != nil {
		t.Fatal(err)
	}
	before := e.ExportDesiredState()

	bad := NewEmu(in.EmuConfig(), nil).ExportDesiredState()
	bad.FlowC["1"] = 55
	bad.ChildLock[bedroomKey] = false
	bad.UTCSettled[bedroomKey] = 120
	bad.ZoneState["not-a-zone-key"] = []byte{9}
	if err := e.RestoreDesiredState(bad); err == nil {
		t.Fatal("invalid zone resource key accepted")
	}
	if got := e.ExportDesiredState(); !reflect.DeepEqual(got, before) {
		t.Errorf("a rejected snapshot changed the emulator\n got: %#v\nwant: %#v", got, before)
	}
}
