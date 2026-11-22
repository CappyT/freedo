package daemon

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"freedo/cc1101"
	"freedo/internal/radio"
	"freedo/tado"
)

// An enqueue that the firmware refuses with ST_AIRTIME (0x1D) put nothing on the
// air. The command waits one minute, as OPERATION.md says, whether the refusal
// came from the firmware at the enqueue or from the radio as a deferral.
func TestAirtimeRefusalOfAQueuedCoAPTransactionRetriesAfterAMinute(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cslQueue bool
	}{
		{"refused by the firmware at the enqueue", true},
		{"deferred by the radio", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device, err := defaultInstall.Resolve("VA1000000005")
			if err != nil {
				t.Fatal(err)
			}
			emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
			if err := emu.EnqueueIdentify(device.Serial, 3); err != nil {
				t.Fatal(err)
			}
			refusing := newImmediateTestRadio(false, cc1101.ErrAirtimeBudget)
			refusing.cslQueue = tc.cslQueue
			d := newDaemon(defaultInstall, emu, refusing, false, "live")
			now := time.Now()
			d.retryNow = func() time.Time { return now }
			d.retryAfter = func(time.Duration) <-chan time.Time { return nil }

			d.dispatchQueued(emu, device.Serial)

			if emu.Pending(device.Serial) == 0 {
				t.Fatal("the refused command is gone")
			}
			if got, want := d.retryAt[device.Serial], now.Add(time.Minute); !got.Equal(want) {
				t.Fatalf("retry at %v, want %v: one minute after the refusal", got.Sub(now), want.Sub(now))
			}
			if d.retryDue[device.Serial] == 0 {
				t.Fatal("no retry is scheduled")
			}
		})
	}
}

// deferringRadio is a live radio whose long transmissions wait, and that counts
// the frames the daemon hands to it.
type deferringRadio struct {
	*immediateTestRadio
	deferring atomic.Bool
	sent      atomic.Int32
}

func (r *deferringRadio) LongTXDeferred() bool { return r.deferring.Load() }
func (r *deferringRadio) Send([]byte) error    { r.sent.Add(1); return nil }

// A Router Advertisement burst that the radio defers is not built: each build
// takes a frame counter, and the beacon tick runs every second. The log says it
// once for the period of deferral.
func TestBeaconTickBuildsNoBurstWhileTheRadioDefers(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	cfg := defaultInstall.EmuConfig()
	cfg.RouterAdvertInterval = 10 * time.Minute
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	live := &deferringRadio{immediateTestRadio: newImmediateTestRadio(false, nil)}
	live.deferring.Store(true)
	d := newDaemon(defaultInstall, emu, live, false, "live")

	for range 5 {
		d.beaconTick()
	}
	if got := live.sent.Load(); got != 0 {
		t.Fatalf("%d frames sent while the radio defers", got)
	}
	if got := strings.Count(logs.String(), "the Router Advertisement waits"); got != 1 {
		t.Fatalf("%d deferral lines in 5 ticks, want 1: %q", got, logs.String())
	}
	actions, due, err := emu.MaybeBeacon()
	if err != nil || !due || !strings.HasSuffix(actions[len(actions)-1].Reason, "ctr=1") {
		t.Fatalf("first burst after the deferral: due=%v err=%v reason=%q, want counter 1: nothing was consumed", due, err, actions[len(actions)-1].Reason)
	}
	emu.CompleteBeacon(false)

	live.deferring.Store(false)
	d.beaconTick()
	if live.sent.Load() == 0 {
		t.Fatal("no burst after the deferral ended")
	}
	live.deferring.Store(true)
	d.beaconTick()
	if got := strings.Count(logs.String(), "the Router Advertisement waits"); got != 2 {
		t.Fatalf("%d deferral lines after a second period, want 2", got)
	}
}

func TestShiftAirtimeMovesTheBucketsByTheWholeMinutesElapsed(t *testing.T) {
	var stored [cc1101.AirtimeBuckets]uint16
	for i := range stored {
		stored[i] = uint16(i + 1)
	}
	at := func(n int) uint16 { return uint16(n) }
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		first   uint16 // bucket 0 of the result
		last    uint16 // bucket 60 of the result
	}{
		{"no time", 0, at(1), at(61)},
		{"under a minute", 59 * time.Second, at(1), at(61)},
		{"three whole minutes", 3*time.Minute + 40*time.Second, at(4), 0},
		{"sixty minutes", 60 * time.Minute, at(61), 0},
		{"the whole window", 61 * time.Minute, 0, 0},
		{"a day", 24 * time.Hour, 0, 0},
		{"a clock that went back", -time.Hour, at(1), at(61)},
	} {
		got := shiftAirtime(stored[:], tc.elapsed)
		if got[0] != tc.first || got[60] != tc.last {
			t.Errorf("%s: first=%d last=%d, want %d and %d", tc.name, got[0], got[60], tc.first, tc.last)
		}
	}
	if got := shiftAirtime(stored[:3], 0); got != ([cc1101.AirtimeBuckets]uint16{}) {
		t.Errorf("a list of 3 buckets gave %v, want zeros", got)
	}
}

// A fresh database preloads zeros. A stored reading preloads the buckets moved
// forward by the minutes since it was read.
func TestStoredAirtimePreloadsTheShiftedCount(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := time.Unix(1_700_000_000, 0)
	if got := storedAirtime(store, now); got != ([cc1101.AirtimeBuckets]uint16{}) {
		t.Fatalf("a fresh database preloads %v, want zeros", got)
	}
	if got := storedAirtime(nil, now); got != ([cc1101.AirtimeBuckets]uint16{}) {
		t.Fatalf("no database preloads %v, want zeros", got)
	}
	buckets := make([]uint16, cc1101.AirtimeBuckets)
	buckets[58], buckets[59], buckets[60] = 100, 200, 300
	if err := store.SetAirtime(context.Background(), buckets, now.Add(-2*time.Minute-10*time.Second)); err != nil {
		t.Fatal(err)
	}
	got := storedAirtime(store, now)
	if got[56] != 100 || got[57] != 200 || got[58] != 300 || got[59] != 0 || got[60] != 0 {
		t.Fatalf("preload %v, want the buckets moved forward by 2 minutes", got[54:])
	}
}

// storingRadio reports a scripted airtime reading.
type storingRadio struct {
	*calRadio
	airtime radio.Airtime
	known   bool
}

func (r *storingRadio) Airtime() (radio.Airtime, bool) { return r.airtime, r.known }
func (r *storingRadio) Warnings() []string             { return nil }

func airtimeTestDaemon(t *testing.T) (*daemon, *storingRadio, *stateStore) {
	t.Helper()
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	lr := &storingRadio{calRadio: newCalRadio(radio.Calibration{})}
	return &daemon{radio: lr, state: store}, lr, store
}

// The daemon stores the first reading after the open, whatever it holds, and
// each later reading only when its buckets changed: a radio that sent nothing
// writes the database once.
func TestDaemonStoresTheFirstAirtimeReadingAndThenOnlyChanges(t *testing.T) {
	d, lr, store := airtimeTestDaemon(t)
	ctx := context.Background()

	d.syncAirtime(ctx, lr)
	if _, _, ok, _ := store.Airtime(ctx); ok {
		t.Fatal("stored a reading the radio has not made")
	}
	at := time.Unix(1_700_000_000, 0)
	lr.known = true
	lr.airtime = radio.Airtime{BudgetMS: 100000, At: at}
	d.syncAirtime(ctx, lr)
	if _, gotAt, ok, err := store.Airtime(ctx); err != nil || !ok || !gotAt.Equal(at) {
		t.Fatalf("the first reading of the run, all zeros, is not stored: at=%v ok=%v err=%v", gotAt, ok, err)
	}

	for range 3 {
		lr.airtime.At = lr.airtime.At.Add(10 * time.Second)
		d.syncAirtime(ctx, lr)
	}
	if _, gotAt, _, _ := store.Airtime(ctx); !gotAt.Equal(at) {
		t.Fatalf("an unchanged array was stored again, at=%v", gotAt)
	}

	lr.airtime.At = lr.airtime.At.Add(10 * time.Second)
	lr.airtime.Buckets[60] = 946
	d.syncAirtime(ctx, lr)
	buckets, gotAt, _, _ := store.Airtime(ctx)
	if !gotAt.Equal(lr.airtime.At) || buckets[60] != 946 {
		t.Fatalf("a changed array was not stored: at=%v bucket=%d", gotAt, buckets[60])
	}
	d.syncAirtime(ctx, lr)
	if _, again, _, _ := store.Airtime(ctx); !again.Equal(gotAt) {
		t.Fatal("the same reading was stored twice")
	}
}

// A run that ends before the first tick of the store still leaves its count: the
// radio reads the firmware when it closes, and the shutdown stores that reading.
func TestShutdownStoresTheAirtimeOfAShortRun(t *testing.T) {
	d, lr, store := airtimeTestDaemon(t)
	lr.known = true
	lr.airtime = radio.Airtime{BudgetMS: 100000, UsedMS: 9460, At: time.Now()}
	lr.airtime.Buckets[60] = 9460 // ten bootstrap trains right after the start
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.storeAirtime(ctx); close(done) }()
	cancel() // the daemon shuts down before the first tick
	<-done
	if _, _, ok, _ := store.Airtime(context.Background()); ok {
		t.Fatal("the ticker stored before its first tick")
	}
	d.storeAirtimeFinal()
	var sum int
	for _, v := range storedAirtime(store, time.Now()) {
		sum += int(v)
	}
	if sum != 9460 {
		t.Fatalf("the next start preloads %d ms, want the 9460 ms of the short run", sum)
	}
}

// An ESP32 that reset alone reports zeros. They must not erase the stored count,
// and the stored count ages as the minutes pass, also when a reading comes every
// 10 s and no store sees a whole minute.
func TestZeroReadingOfAResetBridgeKeepsTheStoredCount(t *testing.T) {
	d, lr, store := airtimeTestDaemon(t)
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0)
	lr.known = true
	lr.airtime = radio.Airtime{BudgetMS: 100000, UsedMS: 30000, At: at}
	lr.airtime.Buckets[60] = 30000
	d.syncAirtime(ctx, lr)

	for _, el := range []time.Duration{10 * time.Second, 70 * time.Second, 130 * time.Second, 190 * time.Second} {
		lr.airtime = radio.Airtime{BudgetMS: 100000, At: at.Add(el)} // the reset: all zeros
		lr.airtime.Buckets[60] = uint16(el/time.Second) / 10         // a little traffic after it
		d.syncAirtime(ctx, lr)
	}
	buckets, gotAt, _, _ := store.Airtime(ctx)
	if buckets[57] != 30000 || airtimeTotal(buckets) != 30000 {
		t.Fatalf("stored %v, want the count of 30000 ms moved 3 minutes and nothing else", buckets[55:])
	}
	if want := at.Add(3 * time.Minute); !gotAt.Equal(want) {
		t.Fatalf("stored at %v, want %v: the sub-minute rest must stay", gotAt, want)
	}
}

// The array with the larger total wins, whole. A bucket by bucket maximum would
// copy the busy minute of one array into the quiet minutes of the other.
func TestLargerAirtimeTotalWinsWhole(t *testing.T) {
	d, lr, store := airtimeTestDaemon(t)
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0)
	lr.known = true
	lr.airtime = radio.Airtime{BudgetMS: 100000, At: at}
	lr.airtime.Buckets[60] = 900
	d.syncAirtime(ctx, lr)

	// One minute later the reading has a bigger total in other buckets.
	lr.airtime = radio.Airtime{BudgetMS: 100000, At: at.Add(time.Minute)}
	lr.airtime.Buckets[59], lr.airtime.Buckets[60] = 500, 600
	d.syncAirtime(ctx, lr)
	buckets, gotAt, _, _ := store.Airtime(ctx)
	if buckets[59] != 500 || buckets[60] != 600 || buckets[59]+buckets[60] != 1100 || airtimeTotal(buckets) != 1100 || !gotAt.Equal(lr.airtime.At) {
		t.Fatalf("stored %v at %v, want the reading whole", buckets[57:], gotAt)
	}

	// Then a reading with a smaller total: the stored array wins whole.
	lr.airtime = radio.Airtime{BudgetMS: 100000, At: at.Add(2 * time.Minute)}
	lr.airtime.Buckets[60] = 1000
	d.syncAirtime(ctx, lr)
	buckets, gotAt, _, _ = store.Airtime(ctx)
	if buckets[58] != 500 || buckets[59] != 600 || buckets[60] != 0 || !gotAt.Equal(at.Add(2*time.Minute)) {
		t.Fatalf("stored %v at %v, want the old array moved one minute", buckets[57:], gotAt)
	}
}

// The radio tells devices from other sources by the 5-byte prefix of the wire
// address, which a Data frame carries as its source.
func TestInstallPlantKnowsTheDevicesByFrameSource(t *testing.T) {
	d := newDaemon(defaultInstall, nil, radio.NewMock(nil), true, "mock")
	plant := installPlant{d}
	if plant.Devices() != len(defaultInstall.Devices) || plant.Devices() == 0 {
		t.Fatalf("Devices() = %d, want %d", plant.Devices(), len(defaultInstall.Devices))
	}
	device := defaultInstall.Devices[0]
	if !plant.HasSource(string(device.WireEUI()[:5])) {
		t.Fatal("a device of the installation is not a source of the plant")
	}
	if plant.HasSource("other") || plant.HasSource(string(defaultInstall.CoordinatorEUI()[:5])) {
		t.Fatal("a stranger or the coordinator is a source of the plant")
	}
}
