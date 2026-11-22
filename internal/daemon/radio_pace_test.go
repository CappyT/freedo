package daemon

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"freedo/cc1101"
	"freedo/internal/radio"
	"freedo/tado"
)

// A job that the bootstrap pace holds stays queued and waits for the slot the
// radio names. It spends no try, and it does not wait the minute of an airtime
// refusal.
func TestBootstrapPaceHoldsAQueuedCommandUntilItsSlot(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if err := emu.EnqueueIdentify(device.Serial, 3); err != nil {
		t.Fatal(err)
	}
	paced := &radio.BootstrapPaced{RetryAfter: 17 * time.Second, Waiting: 3}
	d := newDaemon(defaultInstall, emu, newImmediateTestRadio(false, paced), false, "live")
	now := time.Now()
	d.retryNow = func() time.Time { return now }
	d.retryAfter = func(time.Duration) <-chan time.Time { return nil }

	d.dispatchQueued(emu, device.Serial)

	queue := emu.Queue(device.Serial)
	if len(queue) != 1 || queue[0].Tries != 0 {
		t.Fatalf("queue %+v, want the command queued with no try spent", queue)
	}
	if got, want := d.retryAt[device.Serial], now.Add(17*time.Second); !got.Equal(want) {
		t.Fatalf("retry at %v, want %v: the slot the radio named", got.Sub(now), want.Sub(now))
	}
}

// An uplink offers a queued job to the radio, and the pace defers it. No timer
// runs in that path, so the daemon sets one for the slot the radio names: the job
// asks again then, and does not wait for the next uplink.
func TestBootstrapPaceRetriesAJobThatAnUplinkOffered(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if err := emu.EnqueueIdentify(device.Serial, 3); err != nil {
		t.Fatal(err)
	}
	d := newDaemon(defaultInstall, emu, newImmediateTestRadio(false, nil), false, "live")
	now := time.Now()
	d.retryNow = func() time.Time { return now }
	d.retryAfter = func(time.Duration) <-chan time.Time { return nil }
	actions, ok, err := emu.NextQueuedDownlink(device.Serial)
	if err != nil || !ok {
		t.Fatalf("no queued downlink: ok=%t err=%v", ok, err)
	}
	d.revertUnsubmitted(actions, nil)

	paced := &radio.BootstrapPaced{RetryAfter: 17 * time.Second, Waiting: 3}
	d.retryPaced(actions, radio.ImmediateResult{}, paced)

	if got, want := d.retryAt[device.Serial], now.Add(17*time.Second); !got.Equal(want) {
		t.Fatalf("retry at %v, want %v", got.Sub(now), want.Sub(now))
	}
	delete(d.retryAt, device.Serial)
	d.retryPaced(actions, radio.ImmediateResult{}, errors.New("link fault"))
	if _, set := d.retryAt[device.Serial]; set {
		t.Fatal("an error other than the pace set a retry")
	}
}

// Health shows the bootstrap trains that wait, the pace limit, the LBT setting
// and the count of busy channels.
func TestHealthReportsTheBootstrapPaceAndTheLBTState(t *testing.T) {
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	live := &healthRadio{calRadio: newCalRadio(radio.Calibration{}), waiting: 5, limit: 4, busy: 2}
	d := newDaemon(defaultInstall, emu, live, true, "live")
	d.lbt = cc1101.LBT{Enabled: true, ThresholdDBm: -80}
	health, _ := d.healthBase()
	bootstrap, _ := health["bootstrap"].(map[string]any)
	if bootstrap["waiting"] != 5 || bootstrap["perMinute"] != 4 {
		t.Fatalf("bootstrap = %v", health["bootstrap"])
	}
	lbt, _ := health["lbt"].(map[string]any)
	if lbt["enabled"] != true || lbt["thresholdDbm"] != int8(-80) || lbt["busy"] != uint64(2) {
		t.Fatalf("lbt = %v", health["lbt"])
	}
}

// The uplink of a device offers it the queued job, and the radio defers the job
// for its pace. The daemon sets the retry in handleFrame, the call site of the
// timer, not only in the helper.
func TestUplinkThatOffersAPacedJobSetsItsRetry(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if err := emu.EnqueueIdentify(device.Serial, 3); err != nil {
		t.Fatal(err)
	}
	paced := &radio.BootstrapPaced{RetryAfter: 17 * time.Second, Waiting: 3}
	d := newDaemon(defaultInstall, emu, newImmediateTestRadio(false, paced), false, "live")
	now := time.Now()
	d.retryNow = func() time.Time { return now }
	d.retryAfter = func(time.Duration) <-chan time.Time { return nil }

	d.handleFrame(deviceRequestUplink(t, device, displaySettingsGet(0x7a10), 0x31), true)

	if got, want := d.retryAt[device.Serial], now.Add(17*time.Second); !got.Equal(want) {
		t.Fatalf("retry at %v, want %v: the slot the radio named", got.Sub(now), want.Sub(now))
	}
	if queue := emu.Queue(device.Serial); len(queue) != 1 || queue[0].Tries != 0 {
		t.Fatalf("queue %+v, want the job queued with no try spent", queue)
	}
}

// An uplink that is offered a paced job logs the pace at most once a minute: the
// radio asks again after each uplink, and the cooldown path makes it frequent.
func TestPacedImmediateTxLogsOnceAMinute(t *testing.T) {
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	if err := emu.EnqueueIdentify(device.Serial, 3); err != nil {
		t.Fatal(err)
	}
	paced := &radio.BootstrapPaced{RetryAfter: 17 * time.Second, Waiting: 3}
	d := newDaemon(defaultInstall, emu, newImmediateTestRadio(false, paced), false, "live")
	now := time.Now()
	d.retryNow = func() time.Time { return now }
	d.retryAfter = func(time.Duration) <-chan time.Time { return nil }
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	for i := 0; i < 3; i++ {
		d.handleFrame(deviceRequestUplink(t, device, displaySettingsGet(uint16(0x7a20+i)), byte(0x31+i)), true)
		now = now.Add(10 * time.Second)
	}
	if got := strings.Count(logs.String(), "immediate tx"); got != 1 {
		t.Fatalf("%d log lines for 3 paced uplinks in 30 s, want 1: %q", got, logs.String())
	}
	now = now.Add(time.Minute)
	d.handleFrame(deviceRequestUplink(t, device, displaySettingsGet(0x7a30), 0x40), true)
	if got := strings.Count(logs.String(), "immediate tx"); got != 2 {
		t.Fatalf("%d log lines after a minute, want 2", got)
	}
}

// Health reports the airtime limit, its budget and the count.
func TestHealthReportsTheAirtimeLimit(t *testing.T) {
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	for _, tc := range []struct {
		name  string
		a     radio.Airtime
		limit int
	}{
		{"off", radio.Airtime{UsedMS: 123000}, 0},
		{"on", radio.Airtime{UsedMS: 40000, BudgetMS: 36000, Share: 1.111, Limit: true, Deferring: true, OverBudget: true}, 4},
	} {
		live := &healthRadio{calRadio: newCalRadio(radio.Calibration{}), airtime: tc.a, known: true, limit: tc.limit}
		d := newDaemon(defaultInstall, emu, live, true, "live")
		health, _ := d.healthBase()
		got, _ := health["airtime"].(map[string]any)
		if got["usedMs"] != tc.a.UsedMS || got["budgetMs"] != tc.a.BudgetMS || got["limit"] != tc.a.Limit || got["deferring"] != tc.a.Deferring || got["overBudget"] != tc.a.OverBudget {
			t.Fatalf("%s: airtime = %v", tc.name, got)
		}
		bootstrap, _ := health["bootstrap"].(map[string]any)
		if bootstrap["perMinute"] != tc.limit {
			t.Fatalf("%s: bootstrap = %v, want perMinute %d", tc.name, bootstrap, tc.limit)
		}
	}
}

// A live radio that counts its downlink by kind has them in health, under
// downlink.kinds.
func TestHealthReportsTheDownlinkKinds(t *testing.T) {
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	live := &kindsRadio{healthRadio: &healthRadio{calRadio: newCalRadio(radio.Calibration{})}, kinds: map[string]radio.KindCount{
		"notify": {Attempts: 12, AirtimeMS: 1056.4}, "echo": {Attempts: 3, AirtimeMS: 2844},
	}}
	d := newDaemon(defaultInstall, emu, live, true, "live")
	health := d.healthEngine(emu, true)
	downlink, _ := health["downlink"].(map[string]any)
	kinds, _ := downlink["kinds"].(map[string]any)
	notify, _ := kinds["notify"].(map[string]any)
	if len(kinds) != 2 || notify["attempts"] != uint64(12) || notify["airtimeMs"] != 1056.0 {
		t.Fatalf("downlink = %v", downlink)
	}
	// A radio that does not count has no kinds, and the old counts stay.
	d = newDaemon(defaultInstall, emu, &healthRadio{calRadio: newCalRadio(radio.Calibration{})}, true, "live")
	health = d.healthEngine(emu, true)
	downlink, _ = health["downlink"].(map[string]any)
	if _, has := downlink["kinds"]; has || downlink["sent"] == nil {
		t.Fatalf("downlink = %v", downlink)
	}
}

type kindsRadio struct {
	*healthRadio
	kinds map[string]radio.KindCount
}

func (r *kindsRadio) DownlinkKinds() map[string]radio.KindCount { return r.kinds }
