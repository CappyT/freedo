package daemon

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"freedo/cc1101"
	"freedo/internal/radio"
	"freedo/tado"
)

// holdClock is a clock that a test moves by hand.
type holdClock struct{ now time.Time }

func (c *holdClock) read() time.Time         { return c.now }
func (c *holdClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// holdingRadio holds the echo of one device with the error given, and takes every
// other action.
type holdingRadio struct {
	*immediateTestRadio
	heldShort byte
	hold      error
	asked     map[string]int
}

func (r *holdingRadio) SendImmediate(actions []tado.Action, auto bool) (radio.ImmediateResult, error) {
	if len(actions) == 1 && len(actions[0].Bytes) >= 11 {
		r.asked[hex.EncodeToString(actions[0].Bytes[3:11])]++
		if actions[0].Bytes[3] == r.heldShort {
			return radio.ImmediateResult{}, r.hold
		}
	}
	return r.immediateTestRadio.SendImmediate(actions, auto)
}

// The pace holds the echo of the first device that is due. The echo of the next
// device goes in the same tick, and the held one keeps its probe: it goes out when
// the pace lets it.
func TestHeldEchoDoesNotStopTheOtherDevices(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	a, err := defaultInstall.Resolve("a408")
	if err != nil {
		t.Fatal(err)
	}
	b, err := defaultInstall.Resolve("0a01")
	if err != nil {
		t.Fatal(err)
	}
	ka, kb := hex.EncodeToString(a.WireEUI()), hex.EncodeToString(b.WireEUI())
	live := &holdingRadio{
		immediateTestRadio: newImmediateTestRadio(true, nil), heldShort: a.WireEUI()[0], asked: map[string]int{},
		hold: &radio.BootstrapPaced{RetryAfter: 52 * time.Second, Waiting: 1, Cause: radio.PaceLimit},
	}
	d := newDaemon(defaultInstall, emu, live, false, "live")
	clock := &holdClock{now: time.Unix(1_700_000_000, 0)}
	d.retryNow = clock.read
	d.linkMaintTick = linkMaintStartupTicks
	d.linkMaint = map[string]linkMaintEntry{ka: {state: 1, retry: 0}, kb: {state: 1, retry: 0}}
	d.linkMaintOrder = []string{ka, kb}
	d.linkMaintenanceTick(emu)
	if live.asked[kb] != 1 {
		t.Fatalf("the echo of the second device was asked %d times in the tick that the first was held, want 1", live.asked[kb])
	}
	if d.linkMaint[ka].retry != 0 || d.linkMaint[kb].retry != 1 {
		t.Fatalf("entries %+v %+v, want the held echo to keep its probe and the sent one to spend one", d.linkMaint[ka], d.linkMaint[kb])
	}
	if d.linkMaintCursor != 0 {
		t.Fatalf("cursor %d, want it past the sent echo", d.linkMaintCursor)
	}
	// The pace lets the echo go: it spends its probe.
	live.hold = nil
	clock.advance(53 * time.Second)
	for range linkMaintPostSendTicks + 1 {
		d.linkMaintenanceTick(emu)
	}
	if d.linkMaint[ka].retry != 1 {
		t.Fatalf("entry %+v, want the held echo to go once the pace lets it", d.linkMaint[ka])
	}
}

// A hold of the radio other than the pace (the airtime deferral at 90 %, the
// synthesizer, a scan step) keeps the probe too. Only an echo that goes on the air
// spends one, so the probes of a device do not run out while the radio defers.
func TestEchoHeldByTheRadioKeepsItsProbes(t *testing.T) {
	for _, hold := range []error{
		fmt.Errorf("%w: deferred while the share is high", cc1101.ErrAirtimeBudget),
		cc1101.ErrAirtimeBudget,
	} {
		emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
		dev, err := defaultInstall.Resolve("a408")
		if err != nil {
			t.Fatal(err)
		}
		key := hex.EncodeToString(dev.WireEUI())
		live := &holdingRadio{immediateTestRadio: newImmediateTestRadio(true, nil), heldShort: dev.WireEUI()[0], asked: map[string]int{}, hold: hold}
		d := newDaemon(defaultInstall, emu, live, false, "live")
		clock := &holdClock{now: time.Unix(1_700_000_000, 0)}
		d.retryNow = clock.read
		d.linkMaintTick = linkMaintStartupTicks
		d.linkMaint = map[string]linkMaintEntry{key: {state: 1, retry: 0}}
		d.linkMaintOrder = []string{key}
		for range 200 {
			d.linkMaintenanceTick(emu)
			clock.advance(128 * time.Millisecond)
		}
		if got := d.linkMaint[key]; got.retry != 0 || got.state != 1 {
			t.Fatalf("hold %v: entry %+v after 200 held ticks, want the probes kept", hold, got)
		}
	}
}

// holdAllRadio holds every echo with the error given: always, or while the radio
// defers long transmissions. It says whether the radio still defers them.
type holdAllRadio struct {
	*immediateTestRadio
	deferred atomic.Bool
	always   bool
	hold     error
	asked    atomic.Int32
}

func (r *holdAllRadio) LongTXDeferred() bool { return r.deferred.Load() }
func (r *holdAllRadio) SendImmediate(actions []tado.Action, auto bool) (radio.ImmediateResult, error) {
	if !r.always && !r.deferred.Load() {
		return r.immediateTestRadio.SendImmediate(actions, auto)
	}
	r.asked.Add(1)
	return radio.ImmediateResult{}, r.hold
}

func dueDevices(t *testing.T) []string {
	t.Helper()
	var keys []string
	for _, dev := range defaultInstall.Devices {
		keys = append(keys, hex.EncodeToString(dev.WireEUI()))
	}
	if len(keys) < 3 {
		t.Fatalf("%d devices in the test installation", len(keys))
	}
	return keys
}

// The pace holds the echoes of every device that is due. A held echo is not asked
// again at each 128 ms tick: it waits the retry that the pace named, and at least
// 2 s. Without the wait every held device builds a frame and a counter at each
// tick for the whole hold.
func TestHeldEchoesAreAskedAtMostOnceEveryTwoSeconds(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	keys := dueDevices(t)
	paced := &radio.BootstrapPaced{RetryAfter: time.Second, Waiting: 1, Cause: radio.PaceCooldown}
	live := &holdAllRadio{immediateTestRadio: newImmediateTestRadio(true, nil), always: true, hold: paced}
	d := newDaemon(defaultInstall, emu, live, false, "live")
	clock := &holdClock{now: time.Unix(1_700_000_000, 0)}
	d.retryNow = clock.read
	d.linkMaintTick = linkMaintStartupTicks
	d.linkMaint = map[string]linkMaintEntry{}
	for _, key := range keys {
		d.linkMaint[key] = linkMaintEntry{state: 1, retry: 0}
	}
	d.linkMaintOrder = keys
	const ticks = 100 // 12.8 s of 128 ms ticks
	for range ticks {
		d.linkMaintenanceTick(emu)
		clock.advance(128 * time.Millisecond)
	}
	// Each device asks at 0 s, 2 s, 4 s ... : 7 times in 12.8 s at the most.
	if limit := int32(len(keys) * 7); live.asked.Load() > limit {
		t.Fatalf("%d echoes asked for %d held devices in 12.8 s, want at most %d", live.asked.Load(), len(keys), limit)
	}
	if live.asked.Load() < int32(len(keys)) {
		t.Fatalf("%d echoes asked, want every device asked at least once", live.asked.Load())
	}
}

// While the radio itself defers (synthesizer, scan, airtime), a held echo waits
// until the deferral ends, and at most 60 s. It is not asked again every 2 s.
func TestEchoHeldByTheRadioWaitsForTheHoldToEnd(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	keys := dueDevices(t)[:1]
	live := &holdAllRadio{immediateTestRadio: newImmediateTestRadio(true, nil), hold: fmt.Errorf("%w: held while the synthesizer is not locked", cc1101.ErrAirtimeBudget)}
	live.deferred.Store(true)
	d := newDaemon(defaultInstall, emu, live, false, "live")
	clock := &holdClock{now: time.Unix(1_700_000_000, 0)}
	d.retryNow = clock.read
	d.linkMaintTick = linkMaintStartupTicks
	d.linkMaint = map[string]linkMaintEntry{keys[0]: {state: 1, retry: 0}}
	d.linkMaintOrder = keys
	run := func(span time.Duration) {
		for end := clock.now.Add(span); clock.now.Before(end); clock.advance(128 * time.Millisecond) {
			d.linkMaintenanceTick(emu)
		}
	}
	run(40 * time.Second)
	if live.asked.Load() != 1 {
		t.Fatalf("%d echoes asked in 40 s of a hold, want one", live.asked.Load())
	}
	// The hold ends: the echo goes within a few seconds, and spends its probe.
	live.deferred.Store(false)
	run(2500 * time.Millisecond)
	if len(live.batches) == 0 {
		t.Fatalf("no echo went within 2.5 s of the end of the hold, entry %+v", d.linkMaint[keys[0]])
	}
}

// A refusal with no deferral to poll (the limit off and an old bridge that
// refuses above its cap) has no end that the daemon can see: the echo waits the
// longest hold, 60 s, and not 2 s. The echoes of 9 devices are asked once a minute.
func TestRefusalWithNoDeferralHoldsTheEchoForAMinute(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	keys := dueDevices(t)
	live := &holdAllRadio{immediateTestRadio: newImmediateTestRadio(true, nil), always: true, hold: fmt.Errorf("%w", cc1101.ErrAirtimeBudget)}
	d := newDaemon(defaultInstall, emu, live, false, "live")
	clock := &holdClock{now: time.Unix(1_700_000_000, 0)}
	d.retryNow = clock.read
	d.linkMaintTick = linkMaintStartupTicks
	d.linkMaint = map[string]linkMaintEntry{}
	for _, key := range keys {
		d.linkMaint[key] = linkMaintEntry{state: 1, retry: 0}
	}
	d.linkMaintOrder = keys
	for end := clock.now.Add(130 * time.Second); clock.now.Before(end); clock.advance(128 * time.Millisecond) {
		d.linkMaintenanceTick(emu)
	}
	// Each device asks at 0 s, 60 s and 120 s.
	if limit := int32(len(keys) * 3); live.asked.Load() > limit || live.asked.Load() < int32(len(keys)) {
		t.Fatalf("%d echoes asked for %d devices in 130 s, want between %d and %d", live.asked.Load(), len(keys), len(keys), limit)
	}
	for _, key := range keys {
		if got := d.linkMaint[key]; got.retry != 0 {
			t.Fatalf("entry %+v spent a probe on a refused echo", got)
		}
	}
}

// A pace hold waits the retry that the pace named, not 2 s: the echo is asked once
// in the first 50 s of a 52 s hold, and again once it ends.
func TestPaceHoldOfTheEchoWaitsItsRetryAfter(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	keys := dueDevices(t)[:1]
	paced := &radio.BootstrapPaced{RetryAfter: 52 * time.Second, Waiting: 1, Cause: radio.PaceLimit}
	live := &holdAllRadio{immediateTestRadio: newImmediateTestRadio(true, nil), always: true, hold: paced}
	d := newDaemon(defaultInstall, emu, live, false, "live")
	clock := &holdClock{now: time.Unix(1_700_000_000, 0)}
	d.retryNow = clock.read
	d.linkMaintTick = linkMaintStartupTicks
	d.linkMaint = map[string]linkMaintEntry{keys[0]: {state: 1, retry: 0}}
	d.linkMaintOrder = keys
	run := func(span time.Duration) {
		for end := clock.now.Add(span); clock.now.Before(end); clock.advance(128 * time.Millisecond) {
			d.linkMaintenanceTick(emu)
		}
	}
	run(50 * time.Second)
	if live.asked.Load() != 1 {
		t.Fatalf("%d echoes asked in 50 s of a 52 s hold, want 1", live.asked.Load())
	}
	run(4 * time.Second)
	if live.asked.Load() != 2 {
		t.Fatalf("%d echoes asked after the hold, want 2", live.asked.Load())
	}
}

// A hold that lasts is asked again after 60 s at the latest.
func TestEchoHeldByTheRadioIsAskedAgainAfterAMinute(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	keys := dueDevices(t)[:1]
	live := &holdAllRadio{immediateTestRadio: newImmediateTestRadio(true, nil), hold: fmt.Errorf("%w: deferred", cc1101.ErrAirtimeBudget)}
	live.deferred.Store(true)
	d := newDaemon(defaultInstall, emu, live, false, "live")
	clock := &holdClock{now: time.Unix(1_700_000_000, 0)}
	d.retryNow = clock.read
	d.linkMaintTick = linkMaintStartupTicks
	d.linkMaint = map[string]linkMaintEntry{keys[0]: {state: 1, retry: 0}}
	d.linkMaintOrder = keys
	for end := clock.now.Add(70 * time.Second); clock.now.Before(end); clock.advance(128 * time.Millisecond) {
		d.linkMaintenanceTick(emu)
	}
	if live.asked.Load() != 2 {
		t.Fatalf("%d echoes asked in 70 s of a hold, want 2: the first and the one after 60 s", live.asked.Load())
	}
}

// An error that is not a pace wait does not hide the next pace line for a minute:
// only a pace wait starts the interval of the limit.
func TestOnlyAPaceWaitStartsTheLogInterval(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if err := emu.EnqueueIdentify(device.Serial, 3); err != nil {
		t.Fatal(err)
	}
	live := newImmediateTestRadio(false, errors.New("link fault"))
	d := newDaemon(defaultInstall, emu, live, false, "live")
	now := time.Now()
	d.retryNow = func() time.Time { return now }
	d.retryAfter = func(time.Duration) <-chan time.Time { return nil }
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	d.handleFrame(deviceRequestUplink(t, device, displaySettingsGet(0x7a40), 0x31), true)
	live.err = &radio.BootstrapPaced{RetryAfter: 17 * time.Second, Waiting: 1, Cause: radio.PaceCooldown}
	now = now.Add(time.Second)
	d.handleFrame(deviceRequestUplink(t, device, displaySettingsGet(0x7a41), 0x32), true)
	if got := strings.Count(logs.String(), "immediate tx"); got != 2 {
		t.Fatalf("%d lines, want the link fault and the first pace wait: %q", got, logs.String())
	}
}

// The cursor moves past a held echo as it does past one that went out, so the
// next tick tries the other devices before the held one again.
func TestCursorMovesPastAHeldEcho(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	a, err := defaultInstall.Resolve("a408")
	if err != nil {
		t.Fatal(err)
	}
	b, err := defaultInstall.Resolve("0a01")
	if err != nil {
		t.Fatal(err)
	}
	ka, kb := hex.EncodeToString(a.WireEUI()), hex.EncodeToString(b.WireEUI())
	live := &holdingRadio{
		immediateTestRadio: newImmediateTestRadio(true, nil), heldShort: a.WireEUI()[0], asked: map[string]int{},
		hold: &radio.BootstrapPaced{RetryAfter: 52 * time.Second, Waiting: 1, Cause: radio.PaceLimit},
	}
	d := newDaemon(defaultInstall, emu, live, false, "live")
	d.linkMaintTick = linkMaintStartupTicks
	// The second device is not due: it waits after an echo.
	d.linkMaint = map[string]linkMaintEntry{ka: {state: 1, retry: 0}, kb: {state: 1, retry: 1, wait: 5}}
	d.linkMaintOrder = []string{ka, kb}
	d.linkMaintenanceTick(emu)
	if d.linkMaintCursor != 1 {
		t.Fatalf("cursor %d, want 1: past the held echo", d.linkMaintCursor)
	}
}
