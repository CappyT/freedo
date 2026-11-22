package radio

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"freedo/cc1101"
	"freedo/tado"
)

type LiveBridge interface {
	RecvAutoAck(time.Duration, []byte, uint16) (*cc1101.AutoAckResult, error)
	RecvFW(time.Duration) (*cc1101.RxFrame, error)
	SendTrain([][]byte, time.Duration) (int, error)
	SendCSLData([]byte, []byte, time.Duration, time.Duration, bool, bool, uint16) (*cc1101.CSLTxResult, error)
	Close() error
}

type queuedCSLBridge interface {
	EnqueueCSLBatch([]cc1101.CSLQueueRequest) error
	PollCSLQueue() (*cc1101.CSLQueueEvent, error)
	FlushCSLQueue() error
}

type boundedCSLBridge interface {
	PollCSLQueueWithin(time.Duration) (*cc1101.CSLQueueEvent, error)
}

type routerAdvertBurstBridge interface {
	SendRouterAdvertBurst([]byte, time.Duration) (int, error)
}

type pairWindowBridge interface {
	SetPairWindow(cc1101.PairWindow) (int, error)
}

// configuredBridge reads back whether the chip still holds its register set.
type configuredBridge interface {
	Configured() (bool, error)
}

// jobConfiguredBridge reads the same register after a CSL job that expired,
// with the longer reply wait of a command that can queue behind a transaction.
type jobConfiguredBridge interface {
	ConfiguredBehindJob() (bool, error)
}

// airtimeBridge reads the time on air of the last hour, and the hourly budget.
type airtimeBridge interface {
	Airtime() (cc1101.AirtimeReading, error)
}

// synthBridge runs the synthesizer calibration and checks that it locks.
type synthBridge interface {
	CalibrateSynth() error
}

// pinger is the one request a bridge with no CSL queue can send as a keepalive.
type pinger interface {
	Ping() (string, error)
}

// TunableBridge is the frequency-offset register of the radio. The daemon seeds
// it at start, and the live radio moves it while it measures a calibration.
type TunableBridge interface {
	FSCTRL0() byte
	SetFSCTRL0(byte) error
}

type Live struct {
	br                   LiveBridge
	coordinator          []byte
	phase                uint16
	autoAck              bool
	batch                bool
	frames               chan []byte
	immediate            chan liveBatch
	async                chan liveAsyncBatch
	pairCtl              chan livePairWindow
	deferred             []liveAsyncBatch
	bootstrapAttemptAt   map[string]time.Time
	bootstrapConfirmedAt map[string]time.Time
	lastBootstrapAttempt time.Time
	rxErrorLimit         int
	cslPending           map[uint16]liveCSLPending
	cslDeadline          time.Time // the bridge had the time to finish every pending job; zero: not armed
	cslGroups            map[uint16]*liveCSLGroup
	cslNextID            uint16
	cslFlushed           bool
	cslDone              chan CSLCompletion
	cslDeferred          []CSLCompletion
	frameDeferred        [][]byte
	tuneSeenAt           map[string]time.Time
	// tuneDebug logs the FREQEST reading of every received frame.
	tuneDebug bool
	txLastAt  time.Time
	// fsctrl0 mirrors the register: the loop goroutine writes it, health reads it.
	fsctrl0 atomic.Uint32
	// tuner is the register of a seeded radio; nil when the bridge has none.
	tuner           TunableBridge
	calMu           sync.Mutex
	cal             liveCalibration
	calResults      chan CalibrationResult
	clock           func() time.Time
	cslPollTimeout  time.Duration
	linkDeadAfter   time.Duration // zero: liveLinkDeadAfter
	pollOKAt        time.Time     // the last CSL queue poll that worked; the loop goroutine only
	rxOKAt          time.Time     // the last receive window that worked; the loop goroutine only
	txBlocked       atomic.Bool
	unconfigured    atomic.Bool // a request met ST_UNCONFIGURED: the loop ends
	bridgeClosed    atomic.Bool
	ownerEnding     atomic.Bool
	stop            chan struct{}
	gone            chan struct{}
	ended           chan struct{}
	goneOnce        sync.Once
	once            sync.Once
	bridgeCloseOnce sync.Once
	lifecycleMu     sync.RWMutex
	lifecycleErr    atomic.Value
	closeErr        error
	kindCounters
	// airtimeLimit is the airtime limit option: it never changes after the start.
	airtimeLimit bool
	// airtime is the last reading of the firmware budget. The loop goroutine
	// writes it, health reads it.
	airtimeMu sync.Mutex
	airtime   Airtime
	airtimeAt time.Time // the loop goroutine only
	// budgetWarned: the log said that the bridge has a budget with the limit off.
	budgetWarned bool      // the loop goroutine only
	synthAt      time.Time // the last synthesizer calibration; the loop goroutine only
	synthFailed  atomic.Bool
	synthTries   int                  // failed retries since the lock was lost; the loop goroutine only
	deferLogAt   map[string]time.Time // the loop goroutine only
	awaitErr     error                // why awaitImmediate gave up, nil for a stop; the loop goroutine only
	// pace holds the train starts of the last minute and the jobs that wait for a
	// slot. The loop goroutine writes it, health reads it.
	pace bootstrapPace
	// cslBusy counts the jobs and bursts that found the channel busy.
	cslBusy atomic.Uint64
}

// Option changes how a Live radio starts.
type Option func(*Live)

// WithAirtimeLimit turns the airtime limit on: the radio defers long
// transmissions near the budget that the bridge was given, and paces the CSL
// bootstrap trains. The caller gives the budget to the bridge in Configure.
func WithAirtimeLimit() Option { return func(r *Live) { r.airtimeLimit = true } }

// WithTXBlocked starts the radio with its transmitter latched off, as in a
// dry-run: no caller can arm it later.
func WithTXBlocked() Option { return func(r *Live) { r.txBlocked.Store(true) } }

type liveCSLPending struct {
	action    tado.Action
	bootstrap bool
	// slot says that this job booked a slot of the bootstrap pace for its batch.
	// Only the first job of a batch holds it.
	slot bool
	// lead says that this is the first job of its batch, or a job that has no batch.
	lead  bool
	group uint16
}

type liveCSLGroup struct {
	action  tado.Action
	remain  int
	anySent bool
	result  *cc1101.CSLTxResult
	err     error
	frames  [][]byte
}

type CSLCompletion struct {
	Action tado.Action
	Result *cc1101.CSLTxResult
	Err    error
	Frames [][]byte
}

type liveBatch struct {
	actions []tado.Action
	autoAck bool
	done    chan liveBatchResult
}

type liveBatchResult struct {
	result ImmediateResult
	err    error
}

type ImmediateResult struct {
	Acks      [][]byte
	Submitted []tado.Action
	Accepted  []tado.Action
	// Delivered lists the actions that the radio sent at once, with no queue, and
	// that the device acknowledged with an Enhanced ACK.
	Delivered []tado.Action
	CSLQueue  bool
}

type liveAsyncBatch struct {
	frames [][]byte
	done   chan error
}

type livePairWindow struct {
	window cc1101.PairWindow
	done   chan livePairResult
}

type livePairResult struct {
	sent int
	err  error
}

const liveReceiveWindow = 250 * time.Millisecond
const liveRXErrorBackoff = 100 * time.Millisecond
const liveRXErrorLimit = 10
const liveTrainChunk = 48
const liveTrainPayloadMax = 1024
const liveCSLTxAckTimeout = 50 * time.Millisecond
const liveCSLTxTimeout = time.Second

// liveCloseGrace is how long Close waits for the loop to stop before it closes
// the bridge under it: one receive window and the round trips around it, so that
// a stop inside a window still reads the airtime a last time.
const liveCloseGrace = 2 * liveReceiveWindow
const liveCSLPollTimeout = 2 * time.Second

// liveLinkDeadAfter is how long a kind of serial exchange, the queue poll or
// the receive window, may fail every time before the loop ends. A reply that the link cut is a routine fault: the loop discards it and
// goes on. A link that answers nothing for this long is dead, and the daemon
// restarts. It is far longer than a poll timeout and than the longest CSL job.
const liveLinkDeadAfter = 30 * time.Second
const liveCSLBootstrapGlobalCooldown = 2 * time.Second
const liveCSLBootstrapRetryCooldown = 30 * time.Second
const liveCSLBootstrapTargetCooldown = 10 * time.Minute

// liveKeepalive bounds the silence of the loop towards the bridge. The firmware
// drops its coordinator, pair window and queue after 5000 ms with no request.
const liveKeepalive = time.Second

// Airtime polling and the deferral it drives. The firmware counts the time on
// air of the last hour. With the airtime limit on, the loop defers the Router
// Advertisement bursts and the CSL bootstrap trains, the two long ones, from
// airtimeHold of the budget until the share falls under airtimeRelease.
const (
	liveAirtimePoll = 10 * time.Second
	airtimeHold     = 0.9
	airtimeRelease  = 0.8
)

// The synthesizer calibration runs once per hour from the idle step, and one
// minute after a calibration that did not lock. While it is not locked the radio
// transmits nothing. After liveSynthRetries failed retries in a row the loop
// ends, and the daemon restarts to open and configure the bridge again.
const (
	liveSynthEvery   = time.Hour
	liveSynthRetry   = time.Minute
	liveSynthRetries = 3
)

// liveConfigCheckWindows is the run of receive windows with no CRC-OK frame,
// about 10 s, after which the owner reads the register set back.
const liveConfigCheckWindows = 40

// liveCSLCompletionSlack covers the serial round trips between the end of a job
// on the bridge and the poll that reads its completion.
const liveCSLCompletionSlack = 500 * time.Millisecond

// SeededBridge is a live bridge with a frequency-offset register.
type SeededBridge interface {
	LiveBridge
	TunableBridge
}

// StartSeeded starts the receive loop on the frequency-offset register the
// bridge holds now: the caller seeds it first. stored says the register holds a
// stored calibration; without one the loop measures it from the frames it hears.
func StartSeeded(br SeededBridge, coordinator []byte, phase uint16, autoAck, tuneDebug, stored bool, opts ...Option) (*Live, error) {
	r, err := buildLiveRadio(br, coordinator, phase, autoAck, opts...)
	if err != nil {
		return nil, err
	}
	r.tuneDebug, r.tuner = tuneDebug, br
	r.fsctrl0.Store(uint32(br.FSCTRL0()))
	r.cal.quietSince = r.now()
	if stored {
		r.cal.state = CalibrationStored
		r.cal.stored, r.cal.hasStored = br.FSCTRL0(), true
	} else {
		r.startMeasureLocked()
	}
	go r.loop()
	return r, nil
}

func StartLive(br LiveBridge, coordinator []byte, phase uint16, autoAck bool, opts ...Option) (*Live, error) {
	r, err := buildLiveRadio(br, coordinator, phase, autoAck, opts...)
	if err != nil {
		return nil, err
	}
	go r.loop()
	return r, nil
}

func buildLiveRadio(br LiveBridge, coordinator []byte, phase uint16, autoAck bool, opts ...Option) (*Live, error) {
	if queue, ok := br.(queuedCSLBridge); ok {
		if err := queue.FlushCSLQueue(); err != nil {
			if errors.Is(err, cc1101.ErrUnconfigured) {
				err = fmt.Errorf("%w: %w", errBridgeUnconfigured, err)
			}
			return nil, err
		}
	}
	r := &Live{
		br:                   br,
		coordinator:          append([]byte(nil), coordinator...),
		phase:                phase,
		autoAck:              autoAck,
		batch:                autoAck,
		frames:               make(chan []byte, 1),
		immediate:            make(chan liveBatch, 1),
		async:                make(chan liveAsyncBatch, 4),
		pairCtl:              make(chan livePairWindow, 1),
		bootstrapAttemptAt:   make(map[string]time.Time),
		bootstrapConfirmedAt: make(map[string]time.Time),
		cslPending:           make(map[uint16]liveCSLPending),
		cslGroups:            make(map[uint16]*liveCSLGroup),
		cslDone:              make(chan CSLCompletion, 32),
		calResults:           make(chan CalibrationResult, 1),
		stop:                 make(chan struct{}),
		gone:                 make(chan struct{}),
		ended:                make(chan struct{}),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

func (r *Live) loop() {
	r.ownerGone()
	var loopErr error
	defer r.finishLoop(&loopErr)
	errs, quiet := 0, 0
	r.pollOKAt, r.rxOKAt, r.synthAt = r.now(), r.now(), r.now()
	for {
		select {
		case <-r.stop:
			return
		default:
		}
		if r.unconfigured.Load() {
			loopErr = errBridgeUnconfigured
			return
		}
		if r.txBlocked.Load() {
			r.autoAck = false
			r.dropAsync(ErrTXBlocked)
			r.dropCSL(ErrTXBlocked)
		}
		if r.synthFailed.Load() {
			// Jobs queued before the lock went must not start on a chip that
			// cannot hold the carrier.
			r.dropCSL(errSynthHeld)
		}
		r.drainFrames()
		r.drainCSLCompletions()
		if r.takeImmediate() || r.takePairWindow() {
			continue
		}
		drained, err := r.drainCSL()
		if err != nil {
			loopErr = fmt.Errorf("CSL queue poll: %w", err)
			return
		}
		if drained {
			continue
		}
		if len(r.cslPending) != 0 {
			if r.expireCSL() {
				// A lost completion is the first sign of a bridge that rebooted.
				if loopErr = r.checkConfigured(&errs, true); loopErr != nil {
					return
				}
				continue
			}
			select {
			case <-time.After(2 * time.Millisecond):
			case <-r.stop:
				return
			}
			continue
		}
		r.drainAsync()
		if err := r.idleStep(); err != nil {
			loopErr = err
			return
		}
		var frame *cc1101.RxFrame
		// The Enhanced ACK is a transmission: without a lock the radio only listens.
		if r.autoAck && !r.synthFailed.Load() {
			result, err := r.br.RecvAutoAck(liveReceiveWindow, r.coordinator, r.phase)
			if err != nil {
				if !r.rxError("autoack", err, &errs) {
					loopErr = fmt.Errorf("autoack receive: %w", err)
					return
				}
				continue
			}
			if result == nil {
				continue
			}
			if result.Status == cc1101.AutoAckTransmitFail {
				log.Printf("rx autoack: Enhanced ACK transmit failed")
			}
			frame = result.Frame
			r.tuneDebugFrame(frame, "autoack")
			r.collectTune(frame)
		} else {
			f, err := r.br.RecvFW(liveReceiveWindow)
			if err != nil {
				if !r.rxError("passive", err, &errs) {
					loopErr = fmt.Errorf("passive receive: %w", err)
					return
				}
				continue
			}
			frame = f
			r.tuneDebugFrame(frame, "recv")
			r.collectTune(frame)
		}
		errs = 0
		r.rxOKAt = r.now()
		if frame == nil || !frame.CRCOK {
			if quiet++; quiet >= liveConfigCheckWindows {
				quiet = 0
				if loopErr = r.checkConfigured(&errs, false); loopErr != nil {
					return
				}
			}
			continue
		}
		quiet = 0
		r.publishAcks([][]byte{frame.Payload})
		if r.batch && !r.awaitImmediate() {
			loopErr = r.awaitErr
			return
		}
	}
}

func (r *Live) finishLoop(loopErr *error) {
	err := ErrStopped
	if *loopErr != nil {
		if errors.Is(*loopErr, cc1101.ErrUnconfigured) && !errors.Is(*loopErr, errBridgeUnconfigured) {
			*loopErr = fmt.Errorf("%w: %w", errBridgeUnconfigured, *loopErr)
		}
		err = fmt.Errorf("%w: %w", errLiveEnded, *loopErr)
	}
	r.lifecycleErr.Store(err)
	r.ownerEnding.Store(true)
	close(r.ownerGone())
	r.lifecycleMu.Lock()
	r.dropAsync(err)
	r.dropImmediate(err)
	r.dropCSL(err)
	r.lifecycleMu.Unlock()
	close(r.frames)
	close(r.ended)
}

func (r *Live) ownerGone() chan struct{} {
	r.goneOnce.Do(func() {
		if r.gone == nil {
			r.gone = make(chan struct{})
		}
	})
	return r.gone
}

func (r *Live) endedErr() error {
	if !r.ownerEnding.Load() {
		return nil
	}
	if err, ok := r.lifecycleErr.Load().(error); ok {
		return err
	}
	return ErrStopped
}

func (r *Live) TXArmed() bool { return !r.ownerEnding.Load() }

// FSCTRL0 reports the frequency-offset register the radio holds now.
func (r *Live) FSCTRL0() byte { return byte(r.fsctrl0.Load()) }

// tuneDebugFrame logs the FREQEST reading of one received frame. The time since
// the previous frame of the same source and since our own last transmission
// tells a reading of the carrier from one the FOC loop has not settled yet.
// Only the loop goroutine calls it.
func (r *Live) tuneDebugFrame(frame *cc1101.RxFrame, path string) {
	if !r.tuneDebug || frame == nil {
		return
	}
	source, ok := cc1101.TuneSample(frame)
	if !ok {
		return
	}
	now := r.now()
	if r.tuneSeenAt == nil {
		r.tuneSeenAt = make(map[string]time.Time)
	}
	last := r.tuneSeenAt[source]
	r.tuneSeenAt[source] = now
	log.Printf("tune frame src=%s path=%s rssi=%.1f len=%d fe=%d since_src=%d since_tx=%d",
		cc1101.TuneLabel(source), path, frame.RSSIdBm, len(frame.Payload), frame.FreqEst,
		sinceMillis(now, last), sinceMillis(now, r.txLastAt))
}

// sinceMillis is the milliseconds between two instants. An instant that never
// happened gives -1.
func sinceMillis(now, then time.Time) int64 {
	if then.IsZero() {
		return -1
	}
	return now.Sub(then).Milliseconds()
}

// markTX records the instant the loop hands a transmission to the bridge.
func (r *Live) markTX() {
	r.txLastAt = r.now()
}

// SerialErrors counts the serial replies the link lost since start. A bridge
// that does not count reports zero.
func (r *Live) SerialErrors() uint64 {
	if br, ok := r.br.(interface{ SerialErrors() uint64 }); ok {
		return br.SerialErrors()
	}
	return 0
}

func (r *Live) RadioError() error {
	err := r.endedErr()
	if errors.Is(err, errLiveEnded) {
		return err
	}
	return nil
}

func (r *Live) rxError(mode string, err error, errs *int) bool {
	if errors.Is(err, cc1101.ErrUnconfigured) {
		log.Printf("rx %s: %v", mode, err)
		return false
	}
	if isLinkFault(err) {
		return r.survivesLinkFault("rx "+mode, &r.rxOKAt, err)
	}
	limit := r.receiveErrorLimit()
	*errs = *errs + 1
	if *errs == 1 || *errs == limit {
		log.Printf("rx %s: %v", mode, err)
	}
	if *errs >= limit {
		log.Printf("rx %s: stopping after %d consecutive errors", mode, *errs)
		return false
	}
	select {
	case <-time.After(liveRXErrorBackoff):
		return true
	case <-r.stop:
		return false
	}
}

// checkConfigured returns an error when the chip lost its register set. A
// bridge that rebooted with the USB link up answers every request, with empty
// polls and empty receive windows, so only the register read back shows it.
// The error ends the owner, and the daemon restarts and configures the chip.
// A serial error of the read back counts as a receive error. afterJob is true
// for the read after a CSL job that expired, which waits for a transaction.
func (r *Live) checkConfigured(errs *int, afterJob bool) error {
	br, ok := r.br.(configuredBridge)
	if !ok {
		return nil
	}
	read := br.Configured
	if jb, ok := r.br.(jobConfiguredBridge); ok && afterJob {
		read = jb.ConfiguredBehindJob
	}
	configured, err := read()
	if err != nil {
		if r.rxError("register check", err, errs) {
			return nil
		}
		return fmt.Errorf("register check: %w", err)
	}
	if !configured {
		return errBridgeUnconfigured
	}
	return nil
}

// isLinkFault tells a reply the serial link lost from an error of the port.
func isLinkFault(err error) bool {
	return errors.Is(err, cc1101.ErrLinkFault) || errors.Is(err, cc1101.ErrCSLPollTimeout)
}

// survivesLinkFault reports whether the loop goes on after a serial exchange
// lost its reply. The frame in that reply stays lost. The loop gives up when
// none of its kind worked since okAt for linkDeadAfter.
func (r *Live) survivesLinkFault(path string, okAt *time.Time, err error) bool {
	limit := r.linkDeadAfter
	if limit <= 0 {
		limit = liveLinkDeadAfter
	}
	if okAt.IsZero() {
		*okAt = r.now()
	}
	if silent := r.now().Sub(*okAt); silent >= limit {
		log.Printf("%s: %v: none worked for %s, stopping", path, err, silent.Round(time.Millisecond))
		return false
	}
	log.Printf("%s: %v", path, err)
	select {
	case <-time.After(liveRXErrorBackoff):
		return true
	case <-r.stop:
		return false
	}
}

func (r *Live) receiveErrorLimit() int {
	if r.rxErrorLimit > 0 {
		return r.rxErrorLimit
	}
	return liveRXErrorLimit
}

func (r *Live) drainAsync() {
	for len(r.deferred) > 0 {
		batch := r.deferred[0]
		r.deferred = r.deferred[1:]
		r.finishAsync(batch)
	}
	for {
		select {
		case batch := <-r.async:
			r.finishAsync(batch)
		default:
			return
		}
	}
}

func (r *Live) finishAsync(batch liveAsyncBatch) {
	if r.txBlocked.Load() {
		log.Printf("async tx: dropped %d frames after bridge detection", len(batch.frames))
		r.completeAsync(batch, ErrTXBlocked)
		return
	}
	acks, err := r.sendFrames(batch.frames)
	if err != nil {
		log.Printf("async tx: %v", err)
		r.noteBridgeErr(err)
	}
	r.completeAsync(batch, err)
	r.publishAcks(acks)
}

func (r *Live) publishAcks(acks [][]byte) {
	for _, ack := range acks {
		r.frameDeferred = append(r.frameDeferred, append([]byte(nil), ack...))
	}
	r.drainFrames()
}

func (r *Live) drainFrames() {
	for len(r.frameDeferred) != 0 {
		select {
		case r.frames <- r.frameDeferred[0]:
			r.frameDeferred[0] = nil
			r.frameDeferred = r.frameDeferred[1:]
		default:
			return
		}
	}
}

func (r *Live) completeAsync(batch liveAsyncBatch, err error) {
	if batch.done != nil {
		batch.done <- err
	}
}

// awaitImmediate waits for the engine to answer a received frame. It sends a
// keepalive request while it waits: the engine can take long, and the firmware
// drops its coordinator after 5000 ms of silence.
func (r *Live) awaitImmediate() bool {
	tick := time.NewTicker(liveKeepalive)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if err := r.keepalive(); err != nil {
				r.awaitErr = err
				return false
			}
		case batch := <-r.immediate:
			r.finishImmediate(batch)
			return true
		case batch := <-r.async:
			if batch.done != nil {
				r.completeAsync(batch, errLiveBusy)
			} else {
				r.deferred = append(r.deferred, batch)
			}
		case req := <-r.pairCtl:
			// The engine that owes the immediate batch may be the caller.
			r.finishPairWindow(req)
		case <-r.stop:
			return false
		}
	}
}

// keepalive sends one request that changes nothing: a queue poll, or a PING for
// a bridge with no queue.
func (r *Live) keepalive() error {
	if _, ok := r.br.(queuedCSLBridge); ok {
		if _, err := r.drainCSL(); err != nil {
			return fmt.Errorf("CSL queue poll: %w", err)
		}
		return nil
	}
	if br, ok := r.br.(pinger); ok {
		if _, err := br.Ping(); err != nil {
			if isLinkFault(err) && r.survivesLinkFault("keepalive", &r.pollOKAt, err) {
				return nil
			}
			return fmt.Errorf("keepalive: %w", err)
		}
		r.pollOKAt = r.now()
	}
	return nil
}

func (r *Live) takePairWindow() bool {
	select {
	case req := <-r.pairCtl:
		r.finishPairWindow(req)
		return true
	default:
		return false
	}
}

func (r *Live) finishPairWindow(req livePairWindow) {
	// A dry-run radio never opens the window. Closing one stays allowed.
	if r.txBlocked.Load() && req.window.Duration != 0 {
		req.done <- livePairResult{err: ErrTXBlocked}
		return
	}
	if r.synthFailed.Load() && req.window.Duration != 0 {
		req.done <- livePairResult{err: errSynthHeld}
		return
	}
	r.pairWindowChanged(req.window.Duration)
	if req.window.Duration != 0 && len(r.cslPending) == 0 && r.tuner != nil {
		// The beacons answer on the register the chip holds: send it home before
		// the window opens. With a job out the restore waits for the idle step.
		r.restoreIfDue()
		// The SCAL of the restore write can lose the lock. synthLost closed the
		// window then, and a PAIRWIN now would open it again.
		if r.synthFailed.Load() {
			req.done <- livePairResult{err: errSynthHeld}
			return
		}
	}
	br, ok := r.br.(pairWindowBridge)
	if !ok {
		req.done <- livePairResult{err: cc1101.ErrPairWindowUnsupported}
		return
	}
	sent, err := br.SetPairWindow(req.window)
	r.noteBridgeErr(err)
	req.done <- livePairResult{sent: sent, err: err}
}

// SetPairWindow opens or closes the Enhanced Beacon answer of the bridge on the
// owner loop. It returns the beacons the bridge sent in the window it ends.
func (r *Live) SetPairWindow(w cc1101.PairWindow) (int, error) {
	req := livePairWindow{window: w, done: make(chan livePairResult, 1)}
	if err := r.endedErr(); err != nil {
		return 0, err
	}
	r.lifecycleMu.RLock()
	if err := r.endedErr(); err != nil {
		r.lifecycleMu.RUnlock()
		return 0, err
	}
	select {
	case r.pairCtl <- req:
		r.lifecycleMu.RUnlock()
	case <-r.stop:
		r.lifecycleMu.RUnlock()
		return 0, ErrStopped
	case <-r.ownerGone():
		r.lifecycleMu.RUnlock()
		return 0, r.endedErr()
	}
	select {
	case res := <-req.done:
		return res.sent, res.err
	case <-r.stop:
		return 0, ErrStopped
	case <-r.ownerGone():
		return 0, r.endedErr()
	}
}

func (r *Live) takeImmediate() bool {
	select {
	case batch := <-r.immediate:
		r.finishImmediate(batch)
		return true
	default:
		return false
	}
}

func (r *Live) finishImmediate(batch liveBatch) {
	if r.txBlocked.Load() || !batch.autoAck {
		r.autoAck = false
		r.txBlocked.Store(true)
		r.dropAsync(ErrTXBlocked)
		if batch.done != nil {
			batch.done <- liveBatchResult{err: ErrTXBlocked}
		}
		return
	}
	r.autoAck = true
	at := r.now()
	result, err := r.sendActions(batch.actions)
	r.noteBridgeErr(err)
	r.holdCSLDeadline(at)
	if batch.done != nil {
		batch.done <- liveBatchResult{result: result, err: err}
	}
}

func (r *Live) dropAsync(err error) {
	for _, batch := range r.deferred {
		log.Printf("async tx: dropped %d frames after bridge detection", len(batch.frames))
		r.completeAsync(batch, err)
	}
	r.deferred = nil
	for {
		select {
		case batch := <-r.async:
			log.Printf("async tx: dropped %d frames after bridge detection", len(batch.frames))
			r.completeAsync(batch, err)
		default:
			return
		}
	}
}

func (r *Live) dropImmediate(err error) {
	for {
		select {
		case batch := <-r.immediate:
			if batch.done != nil {
				batch.done <- liveBatchResult{err: err}
			}
		default:
			return
		}
	}
}

// drainCSL takes one event from the bridge queue and reports whether it found
// one. It polls also with no job out: between transactions the bridge answers
// and stores the uplinks that land outside a receive window, and this poll is
// their only way to the emulator.
func (r *Live) drainCSL() (bool, error) {
	br, ok := r.br.(queuedCSLBridge)
	if !ok {
		return false, nil
	}
	event, err := r.pollCSLQueue(br)
	if err != nil {
		if isLinkFault(err) && r.survivesLinkFault("CSL queue poll", &r.pollOKAt, err) {
			return false, nil
		}
		return false, err
	}
	r.pollOKAt = r.now()
	if event == nil {
		return false, nil
	}
	if event.Inbound != nil {
		if !event.Inbound.CRCOK {
			LogCRCDrop("csl inbound", "", event.Inbound)
			return true, nil
		}
		// No calibration reading here: the chip receives these frames right
		// after its own TX, before the FOC loop converges, so their FREQEST is
		// not a measurement.
		r.tuneDebugFrame(event.Inbound, "csl")
		r.noteHeard(event.Inbound)
		r.publishAcks([][]byte{event.Inbound.Payload})
		return true, nil
	}
	completion := event.Completion
	if completion == nil {
		return false, nil
	}
	pending, ok := r.cslPending[completion.ID]
	if !ok {
		log.Printf("CSL queue completion for unknown id=%d", completion.ID)
		r.dropCSL(ErrTXBlocked)
		return true, nil
	}
	delete(r.cslPending, completion.ID)
	if errors.Is(completion.Err, cc1101.ErrAirtimeBudget) {
		r.noteAirtimeRefusal("CSL job")
	}
	r.confirmCSLBootstrap(pending.action.Bytes, pending.bootstrap, completion.Result, completion.Err)
	if pending.slot {
		r.settleBootstrapSlot(cslShort(pending.action.Bytes), completion.Result)
	} else if pending.bootstrap && pending.lead {
		r.noteUnbookedTrain(pending.action.Bytes, completion.Result)
	}
	r.countJob(pending.action, completion.Result)
	logCSLTx(pending.action, pending.bootstrap, completion.Result, completion.Err)
	if pending.group != 0 {
		group := r.cslGroups[pending.group]
		if group != nil {
			group.remain--
			if completion.Result != nil && completion.Result.Sent > 0 {
				group.anySent = true
				group.result = completion.Result
			}
			if completion.Err != nil {
				group.err = completion.Err
			}
			group.frames = append(group.frames, cslCompletionFrames(completion.Result)...)
			if group.remain == 0 {
				if group.result == nil && group.anySent {
					group.result = &cc1101.CSLTxResult{Sent: 1}
				}
				r.publishCSLCompletion(CSLCompletion{Action: group.action, Result: group.result, Err: group.err, Frames: group.frames})
				delete(r.cslGroups, pending.group)
			}
		}
	} else {
		r.publishCSLCompletion(CSLCompletion{Action: pending.action, Result: completion.Result, Err: completion.Err, Frames: cslCompletionFrames(completion.Result)})
	}
	return true, nil
}

func (r *Live) pollCSLQueue(br queuedCSLBridge) (*cc1101.CSLQueueEvent, error) {
	timeout := r.cslPollTimeout
	if timeout <= 0 {
		timeout = liveCSLPollTimeout
	}
	if bounded, ok := br.(boundedCSLBridge); ok {
		event, err := bounded.PollCSLQueueWithin(timeout)
		if errors.Is(err, cc1101.ErrCSLPollTimeout) {
			return nil, fmt.Errorf("%w: %w", errCSLPollStalled, err)
		}
		return event, err
	}
	return br.PollCSLQueue()
}

// armCSLDeadline adds the firmware guard of each new job to the time the bridge
// needs for the pending ones. The bridge runs one job at a time, so the sum
// bounds the last completion. Call it before the jobs join cslPending.
func (r *Live) armCSLDeadline(jobs []cc1101.CSLQueueRequest) {
	deadline := r.cslDeadline
	if len(r.cslPending) == 0 || deadline.IsZero() {
		deadline = r.now()
	}
	for _, job := range jobs {
		deadline = deadline.Add(cc1101.CSLQueueJobGuard(job))
	}
	r.cslDeadline = deadline
}

// holdCSLDeadline moves the deadline by the time an immediate batch held the
// loop. A train or a burst holds the bridge scheduler for as long.
func (r *Live) holdCSLDeadline(since time.Time) {
	if len(r.cslPending) == 0 || r.cslDeadline.IsZero() {
		return
	}
	r.cslDeadline = r.cslDeadline.Add(r.now().Sub(since))
}

// expireCSL fails every pending job once the bridge is past the time it needs
// to finish them. A bridge that rebooted after an enqueue lost its jobs and
// their completions, and a poll only ever answers empty. The flush brings the
// bridge back to a known queue, and the error completions let the daemon retry.
func (r *Live) expireCSL() bool {
	if r.cslDeadline.IsZero() || !r.now().After(r.cslDeadline.Add(liveCSLCompletionSlack)) {
		return false
	}
	log.Printf("CSL queue: %d jobs got no completion by their deadline", len(r.cslPending))
	r.dropCSL(errCSLCompletionLost)
	r.cslDeadline = time.Time{}
	return true
}

func cslCompletionFrames(result *cc1101.CSLTxResult) [][]byte {
	if result == nil {
		return nil
	}
	frames := make([][]byte, 0, 1+len(result.Follow))
	if result.Ack != nil {
		frames = append(frames, append([]byte(nil), result.Ack.Payload...))
	}
	for _, follow := range result.Follow {
		if follow.CRCOK {
			frames = append(frames, append([]byte(nil), follow.Payload...))
		}
	}
	return frames
}

// LogCRCDrop records a frame the chip received with a failed PHY CRC. The ESP
// hands such frames up on the CSL paths; decrypting one fails the MIC.
func LogCRCDrop(path, node string, frame *cc1101.RxFrame) {
	if node != "" {
		path += " node=" + node
	}
	log.Printf("%s: dropped CRC-failed frame len=%d rssi=%.1f lqi=%d head=%x",
		path, len(frame.Payload), frame.RSSIdBm, frame.LQI, frame.Payload[:min(len(frame.Payload), 3)])
}

func (r *Live) publishCSLCompletion(completion CSLCompletion) {
	if r.cslDone == nil {
		return
	}
	if len(r.cslDeferred) != 0 {
		r.cslDeferred = append(r.cslDeferred, completion)
		return
	}
	select {
	case r.cslDone <- completion:
	default:
		r.cslDeferred = append(r.cslDeferred, completion)
	}
}

func (r *Live) drainCSLCompletions() {
	for len(r.cslDeferred) != 0 {
		select {
		case r.cslDone <- r.cslDeferred[0]:
			r.cslDeferred = r.cslDeferred[1:]
		default:
			return
		}
	}
}

// dropCSL fails every pending job and flushes the bridge queue. The jobs fail
// once. A flush that the link lost is sent again at the next call: the bridge
// keeps its jobs until a flush works, and it starts one between two receive
// windows.
func (r *Live) dropCSL(err error) {
	if !r.cslFlushed {
		r.cslFlushed = r.flushCSL()
	}
	for _, group := range r.cslGroups {
		if group.err == nil {
			group.err = err
		}
		r.publishCSLCompletion(CSLCompletion{Action: group.action, Result: group.result, Err: group.err, Frames: group.frames})
	}
	for _, pending := range r.cslPending {
		if pending.group == 0 {
			r.publishCSLCompletion(CSLCompletion{Action: pending.action, Err: err})
		}
		// A job that the queue lost has no result: its slot of the pace returns.
		if pending.slot {
			r.settleBootstrapSlot(cslShort(pending.action.Bytes), nil)
		}
	}
	r.cslPending = make(map[uint16]liveCSLPending)
	r.cslGroups = make(map[uint16]*liveCSLGroup)
}

// flushCSL empties the bridge queue and reports whether nothing is left to
// flush: a bridge with no queue, a closed port and a bridge that reset need no
// flush.
func (r *Live) flushCSL() bool {
	br, ok := r.br.(queuedCSLBridge)
	if !ok || r.bridgeClosed.Load() {
		return true
	}
	err := br.FlushCSLQueue()
	if err != nil {
		log.Printf("CSL queue flush: %v", err)
		r.noteBridgeErr(err)
	}
	return err == nil || errors.Is(err, cc1101.ErrUnconfigured)
}

func (r *Live) sendTrain(frames [][]byte) error {
	_, err := r.sendFrames(frames)
	return err
}

func (r *Live) sendActions(actions []tado.Action) (ImmediateResult, error) {
	var result ImmediateResult
	if len(actions) != 0 {
		if err := r.synthHold("transmission"); err != nil {
			return result, err
		}
	}
	generic := make([][]byte, 0, len(actions))
	flush := func() error {
		if len(generic) == 0 {
			return nil
		}
		got, err := r.sendFrames(generic)
		result.Acks = append(result.Acks, got...)
		generic = generic[:0]
		return err
	}
	// prev and batchNoTrain follow the frames of a batch that the radio sends one by
	// one (a bridge with no queue): only the first frame books a slot and counts.
	var prev *tado.Action
	batchNoTrain := false
	for i := 0; i < len(actions); i++ {
		action := actions[i]
		if action.CSLTx {
			if err := flush(); err != nil {
				return result, err
			}
			if len(action.Bytes) < 5 {
				return result, fmt.Errorf("CSL Data frame is too short")
			}
			bootstrap := action.HasCSLBatch && !action.NoBootstrap
			held := r.longTXHold()
			stamp := false
			var cooldown time.Duration
			switch {
			case held != nil && (bootstrap || action.CSLBootstrapAlways):
				r.logDeferral("CSL bootstrap train", held)
				return result, held
			case held != nil:
				// The job runs on the anchor it has, and no train starts.
			case !bootstrap && !action.NoBootstrap:
				bootstrap, cooldown = r.cslBootstrapWanted(action)
				stamp = bootstrap
			}
			if cooldown > 0 && action.QueueAttempt {
				// The job wants a train but the last one just started. It waits in the
				// pace, not on a retry with no train, so that the pace orders it.
				paced := r.paceCooldown(cslShort(action.Bytes), action.Origin == tado.OriginUser, cooldown)
				r.logDeferral("CSL bootstrap train", paced)
				return result, paced
			}
			slot := false
			// noTrain says that the pace refused the train of a response: every frame
			// of its datagram goes on the anchor, none asks for a train.
			noTrain := false
			lead := prev == nil || !sameCSLBatch(*prev, action)
			first := action
			prev = &first
			if !lead {
				// A later frame of a batch: the first frame booked the slot and decided.
				noTrain = batchNoTrain
				bootstrap = bootstrap && !noTrain
				stamp = false
			} else if bootstrap && r.anchorKnown(action.Bytes) {
				// A train confirmed this device: it has an anchor, and the job needs no
				// train. The pace does not hold it and books no slot. The firmware still
				// decides on the air, and sends a train when the anchor missed: the
				// completion then counts that train (noteUnbookedTrain).
				stamp = false
				r.releaseBootstrapWait(cslShort(action.Bytes))
			} else if bootstrap {
				// A queued job and the link-maintenance echo ask again when they wait.
				// A response has no retry of its own.
				again := action.QueueAttempt || action.CSLBootstrapAlways
				paced := r.paceBootstrap(cslShort(action.Bytes), action.Origin == tado.OriginUser, again)
				switch {
				case paced == nil:
					slot = true
				case again:
					r.logDeferral("CSL bootstrap train", paced)
					return result, paced
				default:
					// A response goes out on the anchor it has, and the requester asks again.
					bootstrap, stamp, noTrain = false, false, true
				}
			}
			if lead {
				batchNoTrain = noTrain
			}
			if stamp {
				r.noteBootstrapAttempt(action.Bytes)
			}
			if !bootstrap {
				r.releaseBootstrapWait(cslShort(action.Bytes))
			}
			if br, ok := r.br.(queuedCSLBridge); ok {
				result.CSLQueue = true
				if r.cslPending == nil {
					r.cslPending = make(map[uint16]liveCSLPending)
				}
				jobs := []cc1101.CSLQueueRequest{{ID: r.nextCSLID(), Frame: action.Bytes, DstShort: []byte{action.Bytes[4], action.Bytes[3]}, AckTimeout: liveCSLTxAckTimeout, Bootstrap: bootstrap, WaitResponse: action.WaitResponse, CSLPhase: r.phase}}
				pending := []liveCSLPending{{action: action, bootstrap: bootstrap, slot: slot, lead: true}}
				for i+1 < len(actions) && sameCSLBatch(action, actions[i+1]) {
					i++
					next := actions[i]
					// A fragmented datagram needs every frame to have the same cold-start path.
					// The first frame can finish without creating an anchor for the next one.
					nextBootstrap := !noTrain && ((next.HasCSLBatch && !next.NoBootstrap) || bootstrap)
					jobs = append(jobs, cc1101.CSLQueueRequest{ID: r.nextCSLID(), Frame: next.Bytes, DstShort: []byte{next.Bytes[4], next.Bytes[3]}, AckTimeout: liveCSLTxAckTimeout, Bootstrap: nextBootstrap, WaitResponse: next.WaitResponse, CSLPhase: r.phase})
					pending = append(pending, liveCSLPending{action: next, bootstrap: nextBootstrap})
				}
				r.markTX()
				if err := br.EnqueueCSLBatch(jobs); err != nil {
					if slot {
						r.settleBootstrapSlot(cslShort(action.Bytes), nil)
					}
					return result, err
				}
				r.armCSLDeadline(jobs)
				r.cslFlushed = false
				for n, job := range jobs {
					if pending[n].action.QueueAttempt {
						pending[n].group = jobs[0].ID
					}
					r.cslPending[job.ID] = pending[n]
					result.Accepted = append(result.Accepted, pending[n].action)
				}
				if action.QueueAttempt {
					if r.cslGroups == nil {
						r.cslGroups = make(map[uint16]*liveCSLGroup)
					}
					r.cslGroups[jobs[0].ID] = &liveCSLGroup{action: pending[len(pending)-1].action, remain: len(jobs)}
				}
				continue
			}
			r.markTX()
			cslResult, err := r.br.SendCSLData(action.Bytes, []byte{action.Bytes[4], action.Bytes[3]}, liveCSLTxAckTimeout, liveCSLTxTimeout, bootstrap, action.WaitResponse, r.phase)
			if cslResult != nil && cslResult.Sent > 0 {
				result.Submitted = append(result.Submitted, action)
			}
			r.confirmCSLBootstrap(action.Bytes, bootstrap, cslResult, err)
			if slot {
				r.settleBootstrapSlot(cslShort(action.Bytes), cslResult)
			} else if bootstrap && lead {
				r.noteUnbookedTrain(action.Bytes, cslResult)
			}
			r.countJob(action, cslResult)
			logCSLTx(action, bootstrap, cslResult, err)
			if err != nil {
				return result, err
			}
			if cslResult.Ack != nil {
				result.Acks = append(result.Acks, append([]byte(nil), cslResult.Ack.Payload...))
				result.Delivered = append(result.Delivered, action)
			}
			for _, follow := range cslResult.Follow {
				if !follow.CRCOK {
					LogCRCDrop("csl follow", action.Node, follow)
					continue
				}
				result.Acks = append(result.Acks, append([]byte(nil), follow.Payload...))
			}
			continue
		}
		generic = append(generic, action.Bytes)
	}
	return result, flush()
}

func sameCSLBatch(first, next tado.Action) bool {
	if !next.CSLTx || len(next.Bytes) < 5 || next.Bytes[3] != first.Bytes[3] || next.Bytes[4] != first.Bytes[4] {
		return false
	}
	if first.HasCSLBatch || next.HasCSLBatch {
		return first.HasCSLBatch && next.HasCSLBatch && first.CSLBatchTag == next.CSLBatchTag
	}
	return first.QueueAttempt && next.QueueAttempt && first.QueueMID == next.QueueMID && first.QueueTry == next.QueueTry
}

func (r *Live) nextCSLID() uint16 {
	for {
		r.cslNextID++
		if r.cslNextID == 0 {
			continue
		}
		if _, exists := r.cslPending[r.cslNextID]; !exists {
			return r.cslNextID
		}
	}
}

func logCSLTx(action tado.Action, bootstrap bool, result *cc1101.CSLTxResult, err error) {
	log.Printf("csl tx: %s", formatCSLTx(action, bootstrap, result, err))
}

func formatCSLTx(action tado.Action, bootstrap bool, result *cc1101.CSLTxResult, err error) string {
	sent := 0
	ack := false
	follow, dropped := 0, 0
	if result != nil {
		sent = result.Sent
		ack = result.Ack != nil
		follow = len(result.Follow)
		dropped = result.Dropped
	}
	if err != nil {
		return fmt.Sprintf("node=%s short=%s queue=%t bootstrap=%t sent=%d ack=%t follow=%d dropped=%d reason=%q error=%v",
			action.Node, cslShort(action.Bytes), action.QueueAttempt, bootstrap, sent, ack, follow, dropped, action.Reason, err)
	}
	return fmt.Sprintf("node=%s short=%s queue=%t bootstrap=%t sent=%d ack=%t follow=%d dropped=%d reason=%q",
		action.Node, cslShort(action.Bytes), action.QueueAttempt, bootstrap, sent, ack, follow, dropped, action.Reason)
}

// cslBootstrapWanted applies the bootstrap cooldowns to one CSL action. An
// action that always bootstraps skips them. The caller records the attempt with
// noteBootstrapAttempt once the pace lets the train start. When only the global
// cooldown of the last train denies the train, wait is what is left of it.
func (r *Live) cslBootstrapWanted(action tado.Action) (wanted bool, wait time.Duration) {
	if len(action.Bytes) < 5 {
		return false, 0
	}
	if action.CSLBootstrapAlways {
		return true, 0
	}
	return r.bootstrapCooldown(action.Bytes, action.IdentifyCSLBootstrapRetry)
}

// anchorKnown says that a train confirmed the device, with its Enhanced ACK, less
// than liveCSLBootstrapTargetCooldown ago: the bridge holds an anchor for it.
func (r *Live) anchorKnown(frame []byte) bool {
	last := r.bootstrapConfirmedAt[cslShort(frame)]
	return !last.IsZero() && r.now().Sub(last) < liveCSLBootstrapTargetCooldown
}

// noteUnbookedTrain counts a train that a job sent without a slot: a job on a
// known anchor that the firmware had to send with a train, after the anchor
// missed. It acts only with the airtime limit on. The train takes its place in
// the window of the pace and starts the global cooldown, so that the pace holds
// the next train for it. It does not start the retry cooldown of the device. A
// job that ran on the anchor sent a few frames and counts for nothing, and the
// later frames of a batch are not counted: the first frame decides.
func (r *Live) noteUnbookedTrain(frame []byte, result *cc1101.CSLTxResult) {
	if !r.airtimeLimit || result == nil || result.Sent < liveBootstrapTrainFrames {
		return
	}
	p := &r.pace
	p.mu.Lock()
	now := r.now()
	p.prune(now)
	p.starts = append(p.starts, paceStart{at: now, short: cslShort(frame), settled: true})
	p.mu.Unlock()
	r.lastBootstrapAttempt = now
}

// noteBootstrapAttempt counts a train that starts, so the global cooldown keeps
// other targets off the air after it.
func (r *Live) noteBootstrapAttempt(frame []byte) {
	if r.bootstrapAttemptAt == nil {
		r.bootstrapAttemptAt = make(map[string]time.Time)
	}
	now := r.now()
	r.bootstrapAttemptAt[cslShort(frame)] = now
	r.lastBootstrapAttempt = now
}

func (r *Live) allowCSLBootstrap(frame []byte, identifyRetry bool) bool {
	if !r.bootstrapCooldownOK(frame, identifyRetry) {
		return false
	}
	r.noteBootstrapAttempt(frame)
	return true
}

func (r *Live) bootstrapCooldownOK(frame []byte, identifyRetry bool) bool {
	ok, _ := r.bootstrapCooldown(frame, identifyRetry)
	return ok
}

// bootstrapCooldown says whether a train may start for the frame. A denial with
// wait set comes only from the global cooldown of the last train, and it ends
// after wait. The cooldown of a confirmed target and the retry cooldown of a
// target end later and have no wait: the job goes on the anchor it has.
func (r *Live) bootstrapCooldown(frame []byte, identifyRetry bool) (ok bool, wait time.Duration) {
	if len(frame) < 5 {
		return false, 0
	}
	now := r.now()
	key := cslShort(frame)
	if last := r.bootstrapConfirmedAt[key]; !last.IsZero() && now.Sub(last) < liveCSLBootstrapTargetCooldown {
		return false, 0
	}
	if identifyRetry {
		return true, 0
	}
	if last := r.bootstrapAttemptAt[key]; !last.IsZero() && now.Sub(last) < liveCSLBootstrapRetryCooldown {
		return false, 0
	}
	if last := r.lastBootstrapAttempt; !last.IsZero() && now.Sub(last) < liveCSLBootstrapGlobalCooldown {
		return false, liveCSLBootstrapGlobalCooldown - now.Sub(last)
	}
	return true, 0
}

func (r *Live) confirmCSLBootstrap(frame []byte, bootstrap bool, result *cc1101.CSLTxResult, err error) {
	if len(frame) < 5 {
		return
	}
	r.noteCSLBusy(err)
	if errors.Is(err, cc1101.ErrCSLTxNoAnchor) {
		delete(r.bootstrapConfirmedAt, cslShort(frame))
		return
	}
	// A busy channel stopped the train before its first frame: the attempt
	// never reached the air, so it does not hold the next one back.
	if bootstrap && errors.Is(err, cc1101.ErrCSLTxBusy) {
		delete(r.bootstrapAttemptAt, cslShort(frame))
		return
	}
	// With the airtime limit on, a train of one period that the device did not
	// answer says that the anchor is gone: the device is not known any more, and
	// the pace holds its next job. With the limit off the next job uses the anchor.
	if r.airtimeLimit && result != nil && result.Sent >= liveBootstrapTrainFrames && result.Ack == nil {
		delete(r.bootstrapConfirmedAt, cslShort(frame))
	}
	// Any job that the device answers with an Enhanced ACK, with the bootstrap flag
	// or not, shows that the bridge holds its anchor, and renews the window.
	if err != nil || result == nil || result.Ack == nil {
		return
	}
	if r.bootstrapConfirmedAt == nil {
		r.bootstrapConfirmedAt = make(map[string]time.Time)
	}
	r.bootstrapConfirmedAt[cslShort(frame)] = r.now()
}

func (r *Live) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

func cslShort(frame []byte) string {
	if len(frame) < 5 {
		return "????"
	}
	return fmt.Sprintf("%02x%02x", frame[4], frame[3])
}

func (r *Live) sendFrames(frames [][]byte) ([][]byte, error) {
	if len(frames) != 0 {
		if err := r.synthHold("transmission"); err != nil {
			return nil, err
		}
	}
	if ras, ok := routerAdvertBursts(frames); ok {
		if held := r.longTXHold(); held != nil {
			r.logDeferral("router advertisement burst", held)
			return nil, held
		}
		if br, ok := r.br.(routerAdvertBurstBridge); ok {
			for _, ra := range ras {
				r.markTX()
				sent, err := br.SendRouterAdvertBurst(ra, time.Second)
				if err != nil {
					r.noteCSLBusy(err)
					return nil, err
				}
				if sent < 3 || sent > tado.RouterAdvertWakeUpCount+1 {
					return nil, fmt.Errorf("router advertisement burst returned invalid frame count %d", sent)
				}
				r.countKind("ra", jobAirtimeMS(len(ra), sent))
				log.Printf("router advertisement burst: sent %d frames", sent)
			}
			return nil, nil
		}
	}
	var acks [][]byte
	for start := 0; start < len(frames); {
		end, payload := start, 1
		for end < len(frames) && end-start < liveTrainChunk {
			next := 1 + len(frames[end])
			if payload+next > liveTrainPayloadMax {
				break
			}
			payload += next
			end++
		}
		if end == start {
			return acks, fmt.Errorf("frame %d exceeds serial train payload", start)
		}
		chunk := frames[start:end]
		r.markTX()
		sent, err := r.br.SendTrain(chunk, time.Second)
		if err != nil {
			r.noteCSLBusy(err)
			return acks, err
		}
		if sent != len(chunk) {
			return acks, fmt.Errorf("sent %d of %d frames", sent, len(chunk))
		}
		for _, f := range chunk {
			r.countKind("other", airFrameMS(len(f)))
		}
		start = end
	}
	return acks, nil
}

// routerAdvertBursts splits frames into whole advertisement bursts. A pair
// window sends two in a row.
func routerAdvertBursts(frames [][]byte) ([][]byte, bool) {
	n := tado.RouterAdvertWakeUpCount + 1
	if len(frames) == 0 || len(frames)%n != 0 {
		return nil, false
	}
	var ras [][]byte
	for i := 0; i < len(frames); i += n {
		ra, ok := routerAdvertBurst(frames[i : i+n])
		if !ok {
			return nil, false
		}
		ras = append(ras, ra)
	}
	return ras, true
}

func routerAdvertBurst(frames [][]byte) ([]byte, bool) {
	if len(frames) != tado.RouterAdvertWakeUpCount+1 {
		return nil, false
	}
	ra := frames[len(frames)-1]
	if len(ra) < 3 || tado.Classify(ra) != tado.ClassBroadcast {
		return nil, false
	}
	wakeups := tado.BuildRouterAdvertWakeUpBurst(ra[2])
	for i := range wakeups {
		if !bytes.Equal(frames[i], wakeups[i]) {
			return nil, false
		}
	}
	return ra, true
}

func (r *Live) Frames() <-chan []byte { return r.frames }

func (r *Live) CSLCompletions() <-chan CSLCompletion { return r.cslDone }

func (r *Live) Send(frame []byte) error {
	if r.txBlocked.Load() {
		return ErrTXBlocked
	}
	if err := r.endedErr(); err != nil {
		return err
	}
	r.lifecycleMu.RLock()
	defer r.lifecycleMu.RUnlock()
	if err := r.endedErr(); err != nil {
		return err
	}
	select {
	case r.async <- liveAsyncBatch{frames: copyFrames([][]byte{frame})}:
		return nil
	case <-r.stop:
		return ErrStopped
	case <-r.ownerGone():
		return r.endedErr()
	default:
		return errors.New("live tx queue full")
	}
}

func (r *Live) SendBatch(frames [][]byte) error {
	if r.txBlocked.Load() {
		return ErrTXBlocked
	}
	batch := liveAsyncBatch{frames: copyFrames(frames), done: make(chan error, 1)}
	if err := r.endedErr(); err != nil {
		return err
	}
	r.lifecycleMu.RLock()
	if err := r.endedErr(); err != nil {
		r.lifecycleMu.RUnlock()
		return err
	}
	select {
	case r.async <- batch:
		r.lifecycleMu.RUnlock()
	case <-r.stop:
		r.lifecycleMu.RUnlock()
		return ErrStopped
	case <-r.ownerGone():
		r.lifecycleMu.RUnlock()
		return r.endedErr()
	default:
		r.lifecycleMu.RUnlock()
		return errors.New("live tx queue full")
	}
	select {
	case err := <-batch.done:
		return err
	case <-r.stop:
		return ErrStopped
	case <-r.ownerGone():
		return r.endedErr()
	}
}

func (r *Live) SendImmediate(actions []tado.Action, autoAck bool) (ImmediateResult, error) {
	if len(actions) == 0 && !autoAck {
		if err := r.endedErr(); err != nil {
			return ImmediateResult{}, err
		}
		r.lifecycleMu.RLock()
		if err := r.endedErr(); err != nil {
			r.lifecycleMu.RUnlock()
			return ImmediateResult{}, err
		}
		r.txBlocked.Store(true)
		select {
		case r.immediate <- liveBatch{autoAck: false}:
		default:
		}
		r.lifecycleMu.RUnlock()
		return ImmediateResult{}, nil
	}
	batch := liveBatch{actions: append([]tado.Action(nil), actions...), autoAck: autoAck, done: make(chan liveBatchResult, 1)}
	if err := r.endedErr(); err != nil {
		return ImmediateResult{}, err
	}
	r.lifecycleMu.RLock()
	if err := r.endedErr(); err != nil {
		r.lifecycleMu.RUnlock()
		return ImmediateResult{}, err
	}
	select {
	case r.immediate <- batch:
		r.lifecycleMu.RUnlock()
	case <-r.stop:
		r.lifecycleMu.RUnlock()
		return ImmediateResult{}, ErrStopped
	case <-r.ownerGone():
		r.lifecycleMu.RUnlock()
		return ImmediateResult{}, r.endedErr()
	}
	select {
	case result := <-batch.done:
		return result.result, result.err
	case <-r.stop:
		return ImmediateResult{}, ErrStopped
	case <-r.ownerGone():
		return ImmediateResult{}, r.endedErr()
	}
}

func (r *Live) closeBridge() {
	r.bridgeClosed.Store(true)
	r.bridgeCloseOnce.Do(func() { r.closeErr = r.br.Close() })
}

func copyFrames(frames [][]byte) [][]byte {
	copyFrames := make([][]byte, len(frames))
	for i := range frames {
		copyFrames[i] = append([]byte(nil), frames[i]...)
	}
	return copyFrames
}

func (r *Live) Close() error {
	r.once.Do(func() {
		close(r.stop)
		select {
		case <-r.ownerGone():
			<-r.ended
			r.pollAirtimeFinal()
		case <-time.After(liveCloseGrace):
			r.closeBridge()
			<-r.ended
			return
		}
		r.closeBridge()
	})
	return r.closeErr
}
