package tado

import (
	"io"
	"testing"
)

const fixtureOptions = "options:\n  utc_offset_min: 60\n"

// offlineScheduleInstall is the fixture with options.offline_schedule stated.
func offlineScheduleInstall(t *testing.T, enabled bool) *Install {
	t.Helper()
	value := "false"
	if enabled {
		value = "true"
	}
	return variantInstall(t, [2]string{fixtureOptions, fixtureOptions + "  offline_schedule: " + value + "\n"})
}

// served02b3 GETs d/config as the device does and returns the tag 02b3 byte of
// the served document.
func served02b3(t *testing.T, e *EmuState, euiHex string, mid uint16) byte {
	t.Helper()
	raw := deviceUplink(t, mustHex(euiHex), CoAPReq{
		Type: "CON", Code: "0.01", MID: mid, Token: []byte{0x0c},
		Path: []string{"d", "config"}, Options: []CoAPOption{{Num: 23, Val: []byte{0x03}}},
	})
	m := servedAnswer(t, e.OnFrame(raw), mid)
	if m.Code != "2.05" {
		t.Fatalf("%s d/config answer = %s, want 2.05", euiHex, m.Code)
	}
	for _, tlv := range parseTLVList(m.Payload) {
		if tlv.Tag == "02b3" {
			if len(tlv.Val) != 1 {
				t.Fatalf("%s 02b3 = %x, want one byte", euiHex, tlv.Val)
			}
			return tlv.Val[0]
		}
	}
	t.Fatalf("%s served d/config carries no 02b3", euiHex)
	return 0
}

// TestServedConfigOfflineScheduleFollowsInstall: every served d/config carries
// 02b3 = 1 when the installation states nothing, and 0 when it turns the
// offline schedule off.
func TestServedConfigOfflineScheduleFollowsInstall(t *testing.T) {
	e, _ := fixtureEmu(t)
	for i, eui := range []string{bedroomEUIHex, bathEUIHex} {
		if got := served02b3(t, e, eui, uint16(0xd100+i)); got != 1 {
			t.Errorf("default install: %s 02b3 = %d, want 1", eui, got)
		}
	}

	off := NewEmu(offlineScheduleInstall(t, false).EmuConfig(), NewUnhandledLogger(io.Discard))
	for i, eui := range []string{bedroomEUIHex, bathEUIHex} {
		if got := served02b3(t, off, eui, uint16(0xd200+i)); got != 0 {
			t.Errorf("offline_schedule false: %s 02b3 = %d, want 0", eui, got)
		}
	}
}

// TestOfflineSchedulePerDeviceWriteWins: a per-device 02b3 write outranks
// options.offline_schedule across installation changes, later writes that omit
// the field and a restart; the other devices follow the installation.
func TestOfflineSchedulePerDeviceWriteWins(t *testing.T) {
	e, _ := fixtureEmu(t)
	e.node(mustHex(bedroomEUIHex)).LastID = 10 // takes the EnqueueConfig path
	disabled := false
	if err := e.EnqueueConfig(bedroomEUIHex, 10, ConfigWrite{OfflineScheduleEnabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	// Each GET takes a new MID: a repeated one is a retransmission, answered
	// from the first answer.
	mid := uint16(0xd300)
	check := func(step string, e *EmuState, bedroom, bath byte) {
		t.Helper()
		mid += 2
		if got := served02b3(t, e, bedroomEUIHex, mid); got != bedroom {
			t.Errorf("%s: bedroom 02b3 = %d, want %d", step, got, bedroom)
		}
		if got := served02b3(t, e, bathEUIHex, mid+1); got != bath {
			t.Errorf("%s: bath 02b3 = %d, want %d", step, got, bath)
		}
	}
	check("after the device write", e, 0, 1)

	if _, _, err := e.SetInstall(offlineScheduleInstall(t, false)); err != nil {
		t.Fatal(err)
	}
	check("install off", e, 0, 0)

	on := offlineScheduleInstall(t, true)
	if _, _, err := e.SetInstall(on); err != nil {
		t.Fatal(err)
	}
	check("install on", e, 0, 1)

	calib := 50
	if err := e.EnqueueConfig(bedroomEUIHex, 10, ConfigWrite{CalibOffset: &calib}); err != nil {
		t.Fatal(err)
	}
	check("a write without 02b3", e, 0, 1)

	restarted, _ := fixtureEmu(t)
	if err := restarted.RestoreDesiredState(e.ExportDesiredState()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := restarted.SetInstall(on); err != nil {
		t.Fatal(err)
	}
	check("after a restart", restarted, 0, 1)
}

// A sched document on the hot-water thermostat is dropped only when no heating
// schedule explains it.
func TestDropHotWaterScheduleLeftovers(t *testing.T) {
	week := func(setpoints ...int) []ScheduleProgram {
		entries := make([]ScheduleEntry, len(setpoints))
		for i, c := range setpoints {
			entries[i] = ScheduleEntry{Minute: i * 360, SetpointCenti: c * 100, Enabled: true}
		}
		return []ScheduleProgram{{DayMask: 0x7f, Entries: entries}}
	}
	hotWater, heating := week(30, 55, 30), week(17, 21, 17)
	shared := "members: [RU1000000001, VA1000000003], reporter: RU1000000001"
	for _, c := range []struct {
		name          string
		heatingMember bool
		thermostat    []ScheduleProgram
		valve         []ScheduleProgram
		drop          bool
	}{
		{"hot-water setpoints, valve without a document", true, hotWater, nil, true},
		{"hot-water setpoints, valve with another document", true, hotWater, heating, true},
		{"the heating zone's document", true, heating, heating, false},
		{"hot-water setpoints on the heating zone's document", true, hotWater, hotWater, false},
		{"heating setpoints held by the thermostat alone", true, heating, nil, false},
		{"no heating zone", false, heating, nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := fixtureInstall(t)
			if !c.heatingMember {
				in = variantInstall(t, [2]string{shared, "members: [VA1000000003], reporter: VA1000000003"})
			}
			e := NewEmu(in.EmuConfig(), nil)
			if err := e.SetSchedule("RU1000000001", c.thermostat); err != nil {
				t.Fatal(err)
			}
			if c.valve != nil {
				if err := e.SetSchedule("VA1000000003", c.valve); err != nil {
					t.Fatal(err)
				}
			}
			dropped := e.DropHotWaterScheduleLeftovers()
			_, held, err := e.Schedule("RU1000000001")
			if err != nil {
				t.Fatal(err)
			}
			if c.drop != (len(dropped) == 1 && dropped[0] == "RU1000000001") || held == c.drop {
				t.Errorf("dropped %v, thermostat document held %v; want drop %v", dropped, held, c.drop)
			}
			if _, set, _ := e.Schedule("VA1000000003"); set != (c.valve != nil) {
				t.Errorf("valve document held %v, want %v", set, c.valve != nil)
			}
		})
	}
}

// schedFetchTriggers counts the d/sched fetch triggers queued for dev.
func schedFetchTriggers(e *EmuState, dev []byte) int {
	n := 0
	for _, d := range e.queue[hexEUI(dev)] {
		if d.kind == schedFetchKind {
			n++
		}
	}
	return n
}

// synthWeek is a one-program week whose night block holds c.
func synthWeek(c int) []ScheduleProgram {
	return []ScheduleProgram{{DayMask: 0x7f, Entries: []ScheduleEntry{
		{Minute: 0, SetpointCenti: c * 100, Enabled: true},
		{Minute: 420, SetpointCenti: 2100, Enabled: true},
	}}}
}

// TestScheduleEditQueuesOfflineFetch: with 02b3 on, a new schedule document
// queues one empty POST d/sched, the trigger that makes the device fetch it at
// once. A trigger that waits covers later documents, an unchanged document
// queues none, the device's 2.05 closes it, and a document that moves while it
// is on the air queues another.
func TestScheduleEditQueuesOfflineFetch(t *testing.T) {
	in := synthInstall(t)
	_, va := synthDevices(t, in)
	key := hexEUI(va)
	e := NewEmu(in.EmuConfig(), nil)

	if err := e.SetSchedule(key, synthWeek(17)); err != nil {
		t.Fatal(err)
	}
	if n := e.Pending(key); n != 1 || schedFetchTriggers(e, va) != 1 {
		t.Fatalf("pending=%d triggers=%d after the first document, want the one trigger", n, schedFetchTriggers(e, va))
	}
	trigger := headRequest(t, e, va)
	if trigger.Code != "0.02" || trigger.URI() != "d/sched" || len(trigger.Payload) != 0 {
		t.Errorf("trigger = %s %s %x, want an empty POST d/sched", trigger.Code, trigger.URI(), trigger.Payload)
	}

	for _, c := range []int{16, 18, 18} {
		if err := e.SetSchedule(key, synthWeek(c)); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.Pending(key); n != 1 {
		t.Errorf("pending=%d after three more documents, want the one waiting trigger", n)
	}
	answerQueued(t, e, va, "2.05")
	if n := e.Pending(key); n != 0 {
		t.Fatalf("pending=%d after the 2.05, want none", n)
	}

	if err := e.SetSchedule(key, synthWeek(18)); err != nil {
		t.Fatal(err)
	}
	if n := e.Pending(key); n != 0 {
		t.Errorf("pending=%d after the same document, want none: the served ETag did not move", n)
	}

	if err := e.SetSchedule(key, synthWeek(15)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := e.NextQueuedDownlink(key); err != nil || !ok {
		t.Fatalf("trigger not sent: ok=%v err=%v", ok, err)
	}
	if err := e.SetSchedule(key, synthWeek(14)); err != nil {
		t.Fatal(err)
	}
	if n := schedFetchTriggers(e, va); n != 2 {
		t.Errorf("triggers=%d after a document that moved while one was on the air, want 2", n)
	}
}

// TestScheduleEditFollowsOfflineScheduleFlag: with options.offline_schedule
// off a schedule document queues nothing; a per-device 02b3 on outranks it.
func TestScheduleEditFollowsOfflineScheduleFlag(t *testing.T) {
	in, err := Parse([]byte(synthInstallYAML + "options:\n  offline_schedule: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, va := synthDevices(t, in)
	key := hexEUI(va)
	e := NewEmu(in.EmuConfig(), nil)
	if err := e.SetSchedule(key, synthWeek(17)); err != nil {
		t.Fatal(err)
	}
	if n := e.Pending(key); n != 0 {
		t.Fatalf("pending=%d with 02b3 off, want none", n)
	}

	on := true
	if err := e.EnqueueConfig(key, 4, ConfigWrite{OfflineScheduleEnabled: &on}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetSchedule(key, synthWeek(16)); err != nil {
		t.Fatal(err)
	}
	if n := schedFetchTriggers(e, va); n != 1 {
		t.Errorf("triggers=%d with the per-device 02b3 on, want 1", n)
	}
}
