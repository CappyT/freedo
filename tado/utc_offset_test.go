package tado

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

const (
	thermostatEUIHex = "05a2cafa07c51b00"
	romeSpacing      = 120 * time.Second
)

// romeDSTEnd is the end of summer time in Europe/Rome in 2026: 03:00 CEST
// becomes 02:00 CET at 01:00 UTC on the last Sunday of October.
var romeDSTEnd = time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)

// fakeClock is the emulator clock a test moves by hand.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// romeEmu builds the fixture on Europe/Rome with no fixed offset, and gives the
// wall thermostat its interface module id so it is served a document.
func romeEmu(t *testing.T, clock *fakeClock) *EmuState {
	t.Helper()
	in := variantInstall(t, [2]string{"  utc_offset_min: 60\n", "  timezone: Europe/Rome\n  utc_push_spacing_sec: 120\n"})
	cfg := in.EmuConfig()
	cfg.Now = clock.Now
	e := NewEmu(cfg, NewUnhandledLogger(io.Discard))
	e.moduleID[thermostatEUIHex] = 71
	e.refreshThermostatDocuments()
	// The first document queues the thermostat's re-read trigger: the tests
	// start from an empty queue.
	delete(e.queue, thermostatEUIHex)
	// The fixture declares no logical id: each valve gets the one it would use
	// on air.
	for i, key := range declaredValves(e) {
		e.nodes[key] = &NodeState{LastID: i + 1}
	}
	return e
}

func declaredValves(e *EmuState) []string {
	var out []string
	for i := range e.inst.Devices {
		if dev := &e.inst.Devices[i]; dev.Kind == KindValve {
			out = append(out, hexEUI(dev.WireEUI()))
		}
	}
	return out
}

type utcPush struct {
	key string
	at  time.Time
}

// tickUTC runs the daemon's once-a-second 02b2 tick until the clock reaches end
// and returns the pushes in order.
func tickUTC(e *EmuState, clock *fakeClock, end time.Time) (pushes []utcPush, changes int) {
	for ; !clock.now.After(end); clock.now = clock.now.Add(time.Second) {
		if e.SyncUTCOffset(clock.now) {
			changes++
		}
		if key := e.PushUTCOffset(clock.now); key != "" {
			pushes = append(pushes, utcPush{key: key, at: clock.now})
		}
	}
	return pushes, changes
}

func checkPushes(t *testing.T, pushes []utcPush, want []string) {
	t.Helper()
	seen := map[string]int{}
	for i, p := range pushes {
		seen[p.key]++
		if i > 0 && p.at.Sub(pushes[i-1].at) < romeSpacing {
			t.Errorf("push %d to %s came %s after the previous one, want at least %s",
				i, p.key, p.at.Sub(pushes[i-1].at), romeSpacing)
		}
	}
	for _, key := range want {
		if seen[key] != 1 {
			t.Errorf("valve %s got %d pushes, want 1", key, seen[key])
		}
	}
	if len(pushes) != len(want) {
		t.Errorf("%d pushes, want %d", len(pushes), len(want))
	}
}

func docUTCOffset(t *testing.T, e *EmuState, key string) int {
	t.Helper()
	for _, tlv := range parseTLVList(e.cfgDoc[key].body) {
		if tlv.Tag == "02b2" && len(tlv.Val) == 2 {
			return int(int16(uint16(tlv.Val[0])<<8 | uint16(tlv.Val[1])))
		}
	}
	t.Fatalf("the document of %s carries no 02b2", key)
	return 0
}

// TestUTCOffsetFollowsDSTTransition: across the end of summer time every valve
// gets exactly one 02b2 write, the writes stay the spacing apart, and the wall
// thermostat gets a new served document, its re-read trigger and no write.
func TestUTCOffsetFollowsDSTTransition(t *testing.T) {
	clock := &fakeClock{now: romeDSTEnd.Add(-30 * time.Minute)}
	e := romeEmu(t, clock)
	valves := declaredValves(e)
	if got := e.UTCOffset(); got == nil || *got != 120 {
		t.Fatalf("offset before the transition = %v, want CEST 120", got)
	}
	for _, key := range append(valves, thermostatEUIHex) {
		if got := docUTCOffset(t, e, key); got != 120 {
			t.Fatalf("initial document of %s has 02b2 %d, want 120", key, got)
		}
	}
	thermoETag := append([]byte(nil), e.cfgDoc[thermostatEUIHex].etag...)

	pushes, changes := tickUTC(e, clock, romeDSTEnd.Add(-time.Second))
	if len(pushes) != 0 || changes != 0 || queuedDownlinks(e) != 0 {
		t.Fatalf("before the transition: %d pushes, %d changes, %d queued, want none", len(pushes), changes, queuedDownlinks(e))
	}

	pushes, changes = tickUTC(e, clock, romeDSTEnd.Add(2*time.Hour))
	if changes != 1 {
		t.Errorf("%d offset changes, want 1", changes)
	}
	if got := e.UTCOffset(); got == nil || *got != 60 {
		t.Fatalf("offset after the transition = %v, want CET 60", got)
	}
	checkPushes(t, pushes, valves)
	if len(pushes) > 0 && !pushes[0].at.Equal(romeDSTEnd) {
		t.Errorf("first push at %s, want the transition %s", pushes[0].at, romeDSTEnd)
	}
	for _, key := range valves {
		if got := docUTCOffset(t, e, key); got != 60 {
			t.Errorf("document of %s has 02b2 %d, want 60", key, got)
		}
		if n := len(e.queue[key]); n != 1 {
			t.Errorf("valve %s has %d queued writes, want 1", key, n)
		}
	}
	if got := docUTCOffset(t, e, thermostatEUIHex); got != 60 {
		t.Errorf("thermostat document has 02b2 %d, want 60", got)
	}
	if bytes.Equal(e.cfgDoc[thermostatEUIHex].etag, thermoETag) {
		t.Error("the thermostat ETag did not move: its conditional GET would not pick the offset up")
	}
	if q := e.queue[thermostatEUIHex]; len(q) != 1 || q[0].kind != configRereadKind {
		t.Errorf("thermostat has %d queued downlinks, want the one re-read trigger: it refuses d/config writes", len(q))
	}
}

// TestUTCOffsetNoPushWithoutChange: no transition, or a fixed offset that
// overrides the zone, queues nothing and moves no document.
func TestUTCOffsetNoPushWithoutChange(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	e := romeEmu(t, clock)
	etags := cfgEtags(e)
	pushes, changes := tickUTC(e, clock, clock.now.Add(2*time.Hour))
	if len(pushes) != 0 || changes != 0 || queuedDownlinks(e) != 0 {
		t.Errorf("no transition: %d pushes, %d changes, %d queued, want none", len(pushes), changes, queuedDownlinks(e))
	}
	if got := cfgEtags(e); len(got) != len(etags) {
		t.Errorf("documents = %d, want %d", len(got), len(etags))
	} else {
		for key, etag := range etags {
			if got[key] != etag {
				t.Errorf("ETag of %s moved", key)
			}
		}
	}

	in := variantInstall(t, [2]string{"  utc_offset_min: 60\n", "  utc_offset_min: 60\n  timezone: Europe/Rome\n"})
	cfg := in.EmuConfig()
	clock = &fakeClock{now: romeDSTEnd.Add(-time.Hour)}
	cfg.Now = clock.Now
	fixed := NewEmu(cfg, NewUnhandledLogger(io.Discard))
	pushes, changes = tickUTC(fixed, clock, romeDSTEnd.Add(time.Hour))
	if len(pushes) != 0 || changes != 0 || queuedDownlinks(fixed) != 0 {
		t.Errorf("fixed offset: %d pushes, %d changes, %d queued, want none", len(pushes), changes, queuedDownlinks(fixed))
	}
}

// restartEmu rebuilds the emulator at the clock's instant from what the daemon
// persisted, as a daemon restart does. The queue is not persisted.
func restartEmu(t *testing.T, e *EmuState, clock *fakeClock) *EmuState {
	t.Helper()
	state := e.ExportDesiredState()
	next := romeEmu(t, clock)
	if err := next.RestoreDesiredState(state); err != nil {
		t.Fatalf("RestoreDesiredState: %v", err)
	}
	return next
}

// TestUTCOffsetPushSurvivesRestart: a restart in the middle of the pushes
// neither repeats a valve already written nor skips one still behind, and a
// restart across the transition pushes every valve once.
func TestUTCOffsetPushSurvivesRestart(t *testing.T) {
	clock := &fakeClock{now: romeDSTEnd.Add(-time.Minute)}
	e := romeEmu(t, clock)
	valves := declaredValves(e)

	first, _ := tickUTC(e, clock, romeDSTEnd.Add(2*romeSpacing))
	if len(first) != 3 {
		t.Fatalf("%d pushes before the restart, want 3", len(first))
	}
	clock.now = clock.now.Add(10 * time.Minute)
	e = restartEmu(t, e, clock)
	if got := docUTCOffset(t, e, thermostatEUIHex); got != 60 {
		t.Errorf("thermostat document after the restart has 02b2 %d, want 60", got)
	}
	rest, changes := tickUTC(e, clock, clock.now.Add(time.Hour))
	if changes != 0 {
		t.Errorf("%d offset changes after the restart, want 0", changes)
	}
	checkPushes(t, append(first, rest...), valves)

	clock = &fakeClock{now: romeDSTEnd.Add(-time.Minute)}
	e = romeEmu(t, clock)
	if pushes, _ := tickUTC(e, clock, romeDSTEnd.Add(-time.Second)); len(pushes) != 0 {
		t.Fatalf("%d pushes before the transition, want none", len(pushes))
	}
	clock.now = romeDSTEnd.Add(5 * time.Minute)
	e = restartEmu(t, e, clock)
	pushes, _ := tickUTC(e, clock, clock.now.Add(time.Hour))
	checkPushes(t, pushes, valves)
}

// TestUTCOffsetOptionChangeIsSpaced: a changed utc_offset_min reaches the
// thermostat's document at once, with its re-read trigger, and the valves
// through the spaced push, not as one write per valve at reload.
func TestUTCOffsetOptionChangeIsSpaced(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	e := romeEmu(t, clock)
	next := variantInstall(t, [2]string{"  utc_offset_min: 60\n", "  utc_offset_min: 90\n  timezone: Europe/Rome\n  utc_push_spacing_sec: 120\n"})
	if _, _, err := e.SetInstall(next); err != nil {
		t.Fatalf("SetInstall: %v", err)
	}
	if q := e.queue[thermostatEUIHex]; len(q) != 1 || q[0].kind != configRereadKind {
		t.Fatalf("the reload queued the thermostat %d downlinks, want the one re-read trigger", len(q))
	}
	if n := queuedDownlinks(e) - 1; n != 0 {
		t.Fatalf("the reload queued %d writes, want none", n)
	}
	if got := docUTCOffset(t, e, thermostatEUIHex); got != 90 {
		t.Errorf("thermostat document has 02b2 %d, want 90", got)
	}
	pushes, _ := tickUTC(e, clock, clock.now.Add(time.Hour))
	checkPushes(t, pushes, declaredValves(e))
}

func TestTimezoneValidated(t *testing.T) {
	for _, option := range []string{
		"timezone: Europe/Nowhere", "timezone: Local", "timezone: ../etc/passwd", "utc_push_spacing_sec: 0",
	} {
		doc := strings.Replace(fixtureInstallYAML, "utc_offset_min: 60", option, 1)
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%q validated", option)
		}
	}
}
