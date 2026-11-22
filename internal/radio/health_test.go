package radio

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"freedo/cc1101"
	"freedo/tado"
)

// stepClock is a clock the test moves by hand.
type stepClock struct{ now time.Time }

func newStepClock() *stepClock { return &stepClock{now: time.Unix(1_700_000_000, 0)} }

func (c *stepClock) read() time.Time         { return c.now }
func (c *stepClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// budgetBridge reports a scripted airtime and records the synthesizer
// calibrations.
type budgetBridge struct {
	*queuedLiveBridge
	bmu        sync.Mutex
	used       uint32
	budget     uint32
	polls      int
	synthErr   error
	synthCalls int
	buckets    [cc1101.AirtimeBuckets]uint16
	pairOpens  int
	pairCloses int
}

func (b *budgetBridge) SetPairWindow(w cc1101.PairWindow) (int, error) {
	b.bmu.Lock()
	defer b.bmu.Unlock()
	if w.Duration != 0 {
		b.pairOpens++
	} else {
		b.pairCloses++
	}
	return 0, nil
}

func (b *budgetBridge) Airtime() (cc1101.AirtimeReading, error) {
	b.bmu.Lock()
	defer b.bmu.Unlock()
	b.polls++
	return cc1101.AirtimeReading{UsedMS: b.used, BudgetMS: b.budget, Buckets: b.buckets}, nil
}

func (b *budgetBridge) CalibrateSynth() error {
	b.bmu.Lock()
	defer b.bmu.Unlock()
	b.synthCalls++
	return b.synthErr
}

func (b *budgetBridge) setUsed(used uint32) {
	b.bmu.Lock()
	b.used = used
	b.bmu.Unlock()
}

// newBudgetRadio is a radio with the airtime limit off: the firmware has no
// budget and reports 0.
func newBudgetRadio(t *testing.T) (*Live, *budgetBridge, *stepClock) {
	t.Helper()
	bridge := &budgetBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}}
	clock := newStepClock()
	return &Live{br: bridge, clock: clock.read}, bridge, clock
}

// newLimitedRadio is a radio with the airtime limit on and a budget of 100 s.
func newLimitedRadio(t *testing.T) (*Live, *budgetBridge, *stepClock) {
	t.Helper()
	r, bridge, clock := newBudgetRadio(t)
	bridge.budget = 100000
	r.airtimeLimit = true
	return r, bridge, clock
}

// queuedBootstrap lists the bootstrap flag of each job the bridge queue took.
func queuedBootstrap(b *budgetBridge) []bool {
	queued, _, _ := b.snapshotQueue()
	var out []bool
	for _, job := range queued {
		out = append(out, job.bootstrap)
	}
	return out
}

// The reading keeps the per-minute buckets and its instant: the daemon stores
// them, so that a restart of the bridge does not lose the count.
func TestAirtimePollKeepsTheBucketsAndTheirTime(t *testing.T) {
	r, bridge, clock := newBudgetRadio(t)
	bridge.buckets[cc1101.AirtimeBuckets-1] = 946
	bridge.buckets[0] = 12
	bridge.setUsed(958)
	r.pollAirtime()
	a, ok := r.Airtime()
	if !ok || a.Buckets != bridge.buckets || !a.At.Equal(clock.read()) {
		t.Fatalf("airtime = %+v ok=%v, want the buckets and the poll time", a, ok)
	}
}

// The loop reads the firmware about every 10 s, so a run that sends and stops
// within one period would report the count of its first idle step. Close reads
// the firmware once more after the loop ended, and a loop that ended on an
// error does not.
func TestCloseReadsTheAirtimeOnce(t *testing.T) {
	bridge := &budgetBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}, budget: 100000}
	r, err := StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := r.Airtime(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the loop did not read the airtime")
		}
		time.Sleep(time.Millisecond)
	}
	bridge.bmu.Lock()
	bridge.buckets[cc1101.AirtimeBuckets-1] = 9460 // ten bootstrap trains after the first read
	bridge.bmu.Unlock()
	if a, _ := r.Airtime(); a.Buckets[cc1101.AirtimeBuckets-1] != 0 {
		t.Fatal("the first reading holds the trains")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if a, _ := r.Airtime(); a.Buckets[cc1101.AirtimeBuckets-1] != 9460 {
		t.Fatalf("buckets after Close = %v, want the 9460 ms of the run", a.Buckets[cc1101.AirtimeBuckets-4:])
	}
}

// With the airtime limit off the firmware has no budget (it reports 0). The
// radio keeps the count and holds nothing back at any total: no deferral, no
// over-budget state, no warning and no log line.
func TestAirtimeLimitOffHoldsNothing(t *testing.T) {
	logs := captureLog(t)
	r, bridge, clock := newBudgetRadio(t)
	bridge.setUsed(123000)
	clock.advance(liveAirtimePoll)
	r.pollAirtime()
	a, ok := r.Airtime()
	if !ok || a.UsedMS != 123000 || a.Limit || a.Deferring || a.OverBudget || a.BudgetMS != 0 {
		t.Fatalf("airtime = %+v ok=%v, want the count with no limit state", a, ok)
	}
	if r.LongTXDeferred() {
		t.Fatal("the radio defers long transmissions with the limit off")
	}
	if err := r.sendTrain(routerAdvertTestFrames()); err != nil {
		t.Fatalf("router advertisement: %v", err)
	}
	frame := []byte{0x69, 0xec, 0x10, 0x06, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	for name, action := range map[string]tado.Action{
		"plain":    {CSLTx: true, Bytes: frame},
		"fragment": {CSLTx: true, HasCSLBatch: true, Bytes: frame},
		"always":   {CSLTx: true, CSLBootstrapAlways: true, Bytes: frame},
	} {
		if _, err := r.sendActions([]tado.Action{action}); err != nil {
			t.Fatalf("%s job: %v", name, err)
		}
	}
	if w := r.Warnings(); len(w) != 0 {
		t.Fatalf("warnings = %v", w)
	}
	if strings.Contains(logs.String(), "airtime") {
		t.Fatalf("log = %q", logs.String())
	}
}

// A refusal of the firmware cannot come with the limit off, since the firmware
// has no budget. If it comes, the host only logs it: it starts no deferral.
func TestAirtimeRefusalWithTheLimitOffOnlyLogs(t *testing.T) {
	logs := captureLog(t)
	r, _, _ := newBudgetRadio(t)
	r.noteBridgeErr(errors.Join(errors.New("enqueue"), cc1101.ErrAirtimeBudget))
	if r.LongTXDeferred() || r.airtimeDeferring() {
		t.Fatal("a refusal starts a deferral with the limit off")
	}
	if !strings.Contains(logs.String(), "airtime limit is off") {
		t.Fatalf("log = %q", logs.String())
	}
}

// With the limit on, the idle step reads the budget about every 10 s. The
// deferral starts at 90% and ends under 80%: a share between the two keeps the
// state it had.
func TestAirtimeDeferralHasAHysteresis(t *testing.T) {
	r, bridge, clock := newLimitedRadio(t)
	step := func(used uint32) Airtime {
		bridge.setUsed(used)
		clock.advance(liveAirtimePoll)
		r.pollAirtime()
		a, ok := r.Airtime()
		if !ok {
			t.Fatal("no airtime reading")
		}
		return a
	}
	if _, ok := r.Airtime(); ok {
		t.Fatal("airtime known before the first poll")
	}
	for _, tc := range []struct {
		used uint32
		want bool
	}{
		{20000, false}, // 20%
		{83000, false}, // 83%: above the release, under the hold
		{90000, true},  // 90%
		{83000, true},  // 83%: the hold stays
		{78000, false}, // 78%
		{83000, false}, // 83%: still off
	} {
		if a := step(tc.used); a.Deferring != tc.want || !a.Limit {
			t.Fatalf("used %d of %d: deferring=%t limit=%t, want %t", tc.used, a.BudgetMS, a.Deferring, a.Limit, tc.want)
		}
	}
	a, _ := r.Airtime()
	if a.UsedMS != 83000 || a.BudgetMS != 100000 || a.Share < 0.83 || a.Share > 0.84 {
		t.Fatalf("airtime = %+v", a)
	}
	// The poll honours its period.
	before := bridge.polls
	clock.advance(liveAirtimePoll / 2)
	r.pollAirtime()
	if bridge.polls != before {
		t.Fatal("polled again within the period")
	}
}

// With the limit on, the radio logs one warning for each crossing of the budget,
// not one for each reading, and a warning lists the hour while it is above it.
func TestAirtimeOverBudgetWarnsOncePerCrossing(t *testing.T) {
	logs := captureLog(t)
	r, bridge, clock := newLimitedRadio(t)
	step := func(used uint32) Airtime {
		bridge.setUsed(used)
		clock.advance(liveAirtimePoll)
		r.pollAirtime()
		a, _ := r.Airtime()
		return a
	}
	for _, tc := range []struct {
		used uint32
		want bool
	}{{20000, false}, {100000, false}, {101500, true}, {123000, true}, {99000, false}} {
		if a := step(tc.used); a.OverBudget != tc.want {
			t.Fatalf("used %d: overBudget=%t, want %t", tc.used, a.OverBudget, tc.want)
		}
		if tc.used == 123000 {
			if w := r.Warnings(); len(w) != 1 || !strings.Contains(w[0], "123000 ms") {
				t.Fatalf("warnings = %v", w)
			}
		}
	}
	if got := strings.Count(logs.String(), "above the budget"); got != 1 {
		t.Fatalf("%d over-budget warnings for one crossing, log: %q", got, logs.String())
	}
	if got := strings.Count(logs.String(), "back under the budget"); got != 1 {
		t.Fatalf("%d recoveries, log: %q", got, logs.String())
	}
	if got := strings.Count(logs.String(), "deferring router advertisements"); got != 1 {
		t.Fatalf("%d deferral warnings, log: %q", got, logs.String())
	}
}

// While the airtime is deferring, a Router Advertisement burst and a CSL
// bootstrap train stay off the air, and the caller gets an error that says
// "deferred". An anchored job still goes, without a train.
func TestAirtimeDeferralHoldsBurstsAndBootstrapTrains(t *testing.T) {
	logs := captureLog(t)
	r, bridge, clock := newLimitedRadio(t)
	bridge.setUsed(92000)
	clock.advance(liveAirtimePoll)
	r.pollAirtime()

	err := r.sendTrain(routerAdvertTestFrames())
	if !errors.Is(err, errAirtimeDeferred) || !errors.Is(err, cc1101.ErrAirtimeBudget) {
		t.Fatalf("router advertisement: err = %v, want a deferral", err)
	}
	frame := []byte{0x69, 0xec, 0x10, 0x06, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	fragment := tado.Action{CSLTx: true, HasCSLBatch: true, Bytes: frame}
	if _, err := r.sendActions([]tado.Action{fragment}); !errors.Is(err, errAirtimeDeferred) {
		t.Fatalf("bootstrap fragment: err = %v, want a deferral", err)
	}
	always := tado.Action{CSLTx: true, CSLBootstrapAlways: true, Bytes: frame}
	if _, err := r.sendActions([]tado.Action{always}); !errors.Is(err, errAirtimeDeferred) {
		t.Fatalf("bootstrap-always job: err = %v, want a deferral", err)
	}
	if events, _, _ := bridge.snapshot(); len(events) != 0 {
		t.Fatalf("bridge calls while deferring: %v", events)
	}
	plain := tado.Action{CSLTx: true, Bytes: frame}
	if _, err := r.sendActions([]tado.Action{plain}); err != nil {
		t.Fatal(err)
	}
	if got := queuedBootstrap(bridge); !slices.Equal(got, []bool{false}) {
		t.Fatalf("anchored job bootstrap flags %v, want [false]", got)
	}
	if !strings.Contains(logs.String(), "router advertisement burst deferred") || !strings.Contains(logs.String(), "CSL bootstrap train deferred") {
		t.Fatalf("log lacks the deferrals: %q", logs.String())
	}
	if w := r.Warnings(); len(w) != 1 || !strings.Contains(w[0], "airtime") {
		t.Fatalf("warnings = %v", w)
	}

	// Under 80% the same calls go out.
	bridge.setUsed(20000)
	clock.advance(liveAirtimePoll)
	r.pollAirtime()
	if err := r.sendTrain(routerAdvertTestFrames()); err != nil {
		t.Fatalf("router advertisement after the release: %v", err)
	}
	if _, err := r.sendActions([]tado.Action{fragment}); err != nil {
		t.Fatalf("bootstrap fragment after the release: %v", err)
	}
	if got := queuedBootstrap(bridge); !slices.Equal(got, []bool{false, true}) {
		t.Fatalf("bootstrap flags %v, want [false true]", got)
	}
	if w := r.Warnings(); len(w) != 0 {
		t.Fatalf("warnings = %v", w)
	}
}

// A refusal of the firmware (ST_AIRTIME) starts the deferral before the next
// reading, and the daemon gets the error to defer the job.
func TestAirtimeRefusalStartsTheDeferral(t *testing.T) {
	logs := captureLog(t)
	r, _, _ := newLimitedRadio(t)
	r.noteBridgeErr(errors.Join(errors.New("enqueue"), cc1101.ErrAirtimeBudget))
	if !r.airtimeDeferring() {
		t.Fatal("not deferring after a refusal")
	}
	if !strings.Contains(logs.String(), "budget is used up: deferred") {
		t.Fatalf("log = %q", logs.String())
	}
}

// The synthesizer is calibrated once per hour from the idle step. A calibration
// that does not lock raises a warning and runs again after a minute.
func TestSynthCalibrationRunsHourlyAndRetriesWithoutLock(t *testing.T) {
	logs := captureLog(t)
	r, bridge, clock := newBudgetRadio(t)
	r.synthAt = clock.now
	clock.advance(liveSynthEvery - time.Second)
	r.calibrateSynth()
	if bridge.synthCalls != 0 {
		t.Fatal("calibrated before the hour")
	}
	bridge.synthErr = cc1101.ErrNoSynthLock
	clock.advance(time.Second)
	r.calibrateSynth()
	if bridge.synthCalls != 1 || len(r.Warnings()) != 1 || !strings.Contains(r.Warnings()[0], "not locked") {
		t.Fatalf("calls=%d warnings=%v", bridge.synthCalls, r.Warnings())
	}
	clock.advance(liveSynthRetry - time.Second)
	r.calibrateSynth()
	if bridge.synthCalls != 1 {
		t.Fatal("retried before a minute")
	}
	bridge.synthErr = nil
	clock.advance(time.Second)
	r.calibrateSynth()
	if bridge.synthCalls != 2 || len(r.Warnings()) != 0 {
		t.Fatalf("calls=%d warnings=%v, want a lock and no warning", bridge.synthCalls, r.Warnings())
	}
	clock.advance(liveSynthRetry)
	r.calibrateSynth()
	if bridge.synthCalls != 2 {
		t.Fatal("a locked chip was calibrated again within the hour")
	}
	if !strings.Contains(logs.String(), "locked again") {
		t.Fatalf("log = %q", logs.String())
	}
}

// requestGaps records the instant of every request the loop sends.
type requestGaps struct {
	*queuedLiveBridge
	gmu sync.Mutex
	at  []time.Time
}

func (b *requestGaps) note() {
	b.gmu.Lock()
	b.at = append(b.at, time.Now())
	b.gmu.Unlock()
}

func (b *requestGaps) PollCSLQueue() (*cc1101.CSLQueueEvent, error) {
	b.note()
	return b.queuedLiveBridge.PollCSLQueue()
}

func (b *requestGaps) RecvAutoAck(wait time.Duration, c []byte, phase uint16) (*cc1101.AutoAckResult, error) {
	b.note()
	return b.queuedLiveBridge.RecvAutoAck(wait, c, phase)
}

func (b *requestGaps) EnqueueCSLBatch(jobs []cc1101.CSLQueueRequest) error {
	b.note()
	return b.queuedLiveBridge.EnqueueCSLBatch(jobs)
}

// longest is the longest silence towards the bridge between the first request
// and now.
func (b *requestGaps) longest() (time.Duration, int) {
	b.gmu.Lock()
	defer b.gmu.Unlock()
	var worst time.Duration
	for i := 1; i < len(b.at); i++ {
		worst = max(worst, b.at[i].Sub(b.at[i-1]))
	}
	if n := len(b.at); n > 0 {
		worst = max(worst, time.Since(b.at[n-1]))
	}
	return worst, len(b.at)
}

// The firmware drops its coordinator, pair window and queue after 5000 ms with
// no request. The loop sends one at least every 1.25 s in each state.
const keepaliveBound = 1250 * time.Millisecond

// A received frame holds the loop until the engine answers. The engine can take
// long, and the loop keeps the bridge alive meanwhile.
func TestLiveLoopKeepsTheBridgeAliveWhileWaitingForTheEngine(t *testing.T) {
	uplink := &cc1101.RxFrame{Payload: []byte{0x69, 0xec, 0x11}, CRCOK: true}
	bridge := &requestGaps{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{result: &cc1101.AutoAckResult{Status: cc1101.AutoAckSent, Frame: uplink}}}}
	radio, err := StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer radio.Close()
	select {
	case <-radio.Frames():
	case <-time.After(time.Second):
		t.Fatal("no uplink")
	}
	// No SendImmediate follows: the loop waits for the engine.
	time.Sleep(3 * time.Second)
	if worst, n := bridge.longest(); worst > keepaliveBound || n < 3 {
		t.Fatalf("longest silence %s over %d requests, want at most %s", worst, n, keepaliveBound)
	}
}

// With a CSL job out and no completion, the loop polls the queue.
func TestLiveLoopKeepsTheBridgeAliveWhileAJobIsOut(t *testing.T) {
	frame := []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	bridge := &requestGaps{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}}
	radio, err := StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer radio.Close()
	result, err := radio.SendImmediate([]tado.Action{{Node: "office", Bytes: frame, CSLTx: true, QueueAttempt: true}}, true)
	if err != nil || len(result.Accepted) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	time.Sleep(2 * time.Second)
	if worst, n := bridge.longest(); worst > keepaliveBound || n < 3 {
		t.Fatalf("longest silence %s over %d requests, want at most %s", worst, n, keepaliveBound)
	}
}

// unconfiguredBridge answers ST_UNCONFIGURED where it is told to.
type unconfiguredBridge struct {
	*queuedLiveBridge
	onRecv bool
}

func (b *unconfiguredBridge) RecvAutoAck(wait time.Duration, c []byte, phase uint16) (*cc1101.AutoAckResult, error) {
	if b.onRecv {
		return nil, cc1101.ErrUnconfigured
	}
	return b.queuedLiveBridge.RecvAutoAck(wait, c, phase)
}

func awaitEnd(t *testing.T, r *Live) error {
	t.Helper()
	select {
	case <-r.ended:
		return r.RadioError()
	case <-time.After(time.Second):
		t.Fatal("the loop kept running")
		return nil
	}
}

// ST_UNCONFIGURED on a receive ends the loop at once, with the error that makes
// the daemon restart and configure the chip.
func TestLiveLoopEndsOnUnconfiguredReceive(t *testing.T) {
	bridge := &unconfiguredBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}, onRecv: true}
	radio, err := StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer radio.Close()
	if err := awaitEnd(t, radio); !errors.Is(err, errBridgeUnconfigured) {
		t.Fatalf("radio error = %v, want errBridgeUnconfigured", err)
	}
}

// The same answer to a transmit request ends the loop after the batch fails.
func TestLiveLoopEndsOnUnconfiguredEnqueue(t *testing.T) {
	frame := []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	bridge := &unconfiguredBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}, enqueueErr: cc1101.ErrUnconfigured}}
	radio, err := StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer radio.Close()
	_, err = radio.SendImmediate([]tado.Action{{Node: "office", Bytes: frame, CSLTx: true, QueueAttempt: true}}, true)
	if !errors.Is(err, cc1101.ErrUnconfigured) {
		t.Fatalf("SendImmediate: %v", err)
	}
	if err := awaitEnd(t, radio); !errors.Is(err, errBridgeUnconfigured) {
		t.Fatalf("radio error = %v, want errBridgeUnconfigured", err)
	}
}

// A dry-run radio starts latched: no later call arms the transmitter, and the
// pair window does not open.
func TestLiveRadioStartedBlockedNeverTransmits(t *testing.T) {
	bridge := &pairBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}}
	radio, err := StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, false, WithTXBlocked())
	if err != nil {
		t.Fatal(err)
	}
	defer radio.Close()
	frame := []byte{0x69, 0xec, 0x10, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	if _, err := radio.SendImmediate([]tado.Action{{Node: "office", Bytes: frame, CSLTx: true, QueueAttempt: true}}, true); !errors.Is(err, ErrTXBlocked) {
		t.Fatalf("SendImmediate: %v, want ErrTXBlocked", err)
	}
	if err := radio.Send([]byte{0x01}); !errors.Is(err, ErrTXBlocked) {
		t.Fatalf("Send: %v, want ErrTXBlocked", err)
	}
	open := cc1101.PairWindow{Duration: time.Minute, Key: make([]byte, 16), Coordinator: make([]byte, 8), Count: 3}
	if _, err := radio.SetPairWindow(open); !errors.Is(err, ErrTXBlocked) {
		t.Fatalf("SetPairWindow: %v, want ErrTXBlocked", err)
	}
	if _, err := radio.SetPairWindow(cc1101.PairWindow{}); err != nil {
		t.Fatalf("closing the window: %v", err)
	}
	if bridge.opened() != 0 {
		t.Fatal("the bridge opened a pair window")
	}
	if got, _, _ := bridge.queuedLiveBridge.snapshotQueue(); len(got) != 0 {
		t.Fatalf("queued jobs %+v", got)
	}
}

type pairBridge struct {
	*queuedLiveBridge
	pmu   sync.Mutex
	opens int
}

func (b *pairBridge) SetPairWindow(w cc1101.PairWindow) (int, error) {
	b.pmu.Lock()
	defer b.pmu.Unlock()
	if w.Duration != 0 {
		b.opens++
	}
	return 0, nil
}

func (b *pairBridge) opened() int {
	b.pmu.Lock()
	defer b.pmu.Unlock()
	return b.opens
}

// The option turns the limit on: the reading says so. Without it the reading
// says the limit is off.
func TestWithAirtimeLimitTurnsTheLimitOn(t *testing.T) {
	for _, on := range []bool{false, true} {
		bridge := &budgetBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}, budget: 36000}
		var opts []Option
		if on {
			opts = append(opts, WithAirtimeLimit())
		}
		r, err := StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true, opts...)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		var a Airtime
		for ok := false; !ok; a, ok = r.Airtime() {
			if time.Now().After(deadline) {
				t.Fatal("the loop did not read the airtime")
			}
			time.Sleep(time.Millisecond)
		}
		if a.Limit != on || r.BootstrapLimit() == 0 == on {
			t.Fatalf("on=%t: limit=%t bootstrap limit=%d", on, a.Limit, r.BootstrapLimit())
		}
		r.Close()
	}
}

// A bridge that reports a budget with the airtime limit off runs an older v2.11
// firmware with a fixed cap. The log says so once. With the limit on a budget is
// expected, and the log says nothing of the kind.
func TestOldFirmwareBudgetWithTheLimitOffIsLoggedOnce(t *testing.T) {
	logs := captureLog(t)
	r, bridge, clock := newBudgetRadio(t)
	bridge.budget = 100000
	for range 3 {
		clock.advance(liveAirtimePoll)
		r.pollAirtime()
	}
	if got := strings.Count(logs.String(), "older v2.11 firmware"); got != 1 {
		t.Fatalf("%d warnings, want 1: %q", got, logs.String())
	}

	logs = captureLog(t)
	r, bridge, clock = newLimitedRadio(t)
	bridge.budget = 36000
	clock.advance(liveAirtimePoll)
	r.pollAirtime()
	if strings.Contains(logs.String(), "older v2.11 firmware") {
		t.Fatalf("a budget with the limit on warned: %q", logs.String())
	}
}

// A bridge that reports a budget with the airtime limit off starts no deferral
// and no over-budget state, whatever its count: the host holds nothing back for a
// cap that it did not ask for.
func TestBridgeBudgetWithTheLimitOffStartsNoDeferral(t *testing.T) {
	r, bridge, clock := newBudgetRadio(t)
	bridge.budget = 100000
	bridge.setUsed(120000)
	clock.advance(liveAirtimePoll)
	r.pollAirtime()
	a, ok := r.Airtime()
	if !ok || a.UsedMS != 120000 || a.BudgetMS != 100000 {
		t.Fatalf("airtime = %+v ok=%v, want the reading kept", a, ok)
	}
	if a.Limit || a.Deferring || a.OverBudget || r.LongTXDeferred() {
		t.Fatalf("airtime = %+v: the limit is off, nothing may defer", a)
	}
	if err := r.sendTrain(routerAdvertTestFrames()); err != nil {
		t.Fatalf("router advertisement held with the limit off: %v", err)
	}
}
