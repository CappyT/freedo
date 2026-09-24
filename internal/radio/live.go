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

// configuredBridge reads back whether the chip still holds its register set.
type configuredBridge interface {
	Configured() (bool, error)
}

// TunableBridge is the frequency-offset register of the radio. The daemon writes
// it once, at start, and never while it runs.
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
	// fsctrl0 is set before the loop starts and never changes afterwards.
	fsctrl0         byte
	clock           func() time.Time
	cslPollTimeout  time.Duration
	txBlocked       atomic.Bool
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
}

type liveCSLPending struct {
	action    tado.Action
	bootstrap bool
	group     uint16
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
	CSLQueue  bool
}

type liveAsyncBatch struct {
	frames [][]byte
	done   chan error
}

const liveReceiveWindow = 250 * time.Millisecond
const liveRXErrorBackoff = 100 * time.Millisecond
const liveRXErrorLimit = 10
const liveTrainChunk = 48
const liveTrainPayloadMax = 1024
const liveCSLTxAckTimeout = 50 * time.Millisecond
const liveCSLTxTimeout = time.Second
const liveCloseGrace = 250 * time.Millisecond
const liveCSLPollTimeout = 2 * time.Second
const liveCSLBootstrapGlobalCooldown = 2 * time.Second
const liveCSLBootstrapRetryCooldown = 30 * time.Second
const liveCSLBootstrapTargetCooldown = 10 * time.Minute

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
// bridge holds now: the caller seeds it first.
func StartSeeded(br SeededBridge, coordinator []byte, phase uint16, autoAck, tuneDebug bool) (*Live, error) {
	r, err := buildLiveRadio(br, coordinator, phase, autoAck)
	if err != nil {
		return nil, err
	}
	r.tuneDebug, r.fsctrl0 = tuneDebug, br.FSCTRL0()
	go r.loop()
	return r, nil
}

func StartLive(br LiveBridge, coordinator []byte, phase uint16, autoAck bool) (*Live, error) {
	r, err := buildLiveRadio(br, coordinator, phase, autoAck)
	if err != nil {
		return nil, err
	}
	go r.loop()
	return r, nil
}

func buildLiveRadio(br LiveBridge, coordinator []byte, phase uint16, autoAck bool) (*Live, error) {
	if queue, ok := br.(queuedCSLBridge); ok {
		if err := queue.FlushCSLQueue(); err != nil {
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
		bootstrapAttemptAt:   make(map[string]time.Time),
		bootstrapConfirmedAt: make(map[string]time.Time),
		cslPending:           make(map[uint16]liveCSLPending),
		cslGroups:            make(map[uint16]*liveCSLGroup),
		cslDone:              make(chan CSLCompletion, 32),
		stop:                 make(chan struct{}),
		gone:                 make(chan struct{}),
		ended:                make(chan struct{}),
	}
	return r, nil
}

func (r *Live) loop() {
	r.ownerGone()
	var loopErr error
	defer r.finishLoop(&loopErr)
	errs, quiet := 0, 0
	for {
		select {
		case <-r.stop:
			return
		default:
		}
		if r.txBlocked.Load() {
			r.autoAck = false
			r.dropAsync(ErrTXBlocked)
			r.dropCSL(ErrTXBlocked)
		}
		r.drainFrames()
		r.drainCSLCompletions()
		if r.takeImmediate() {
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
				if loopErr = r.checkConfigured(&errs); loopErr != nil {
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
		var frame *cc1101.RxFrame
		if r.autoAck {
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
		}
		errs = 0
		if frame == nil || !frame.CRCOK {
			if quiet++; quiet >= liveConfigCheckWindows {
				quiet = 0
				if loopErr = r.checkConfigured(&errs); loopErr != nil {
					return
				}
			}
			continue
		}
		quiet = 0
		r.publishAcks([][]byte{frame.Payload})
		if r.batch && !r.awaitImmediate() {
			return
		}
	}
}

func (r *Live) finishLoop(loopErr *error) {
	err := ErrStopped
	if *loopErr != nil {
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

// FSCTRL0 reports the frequency-offset register the radio took at start.
func (r *Live) FSCTRL0() byte { return r.fsctrl0 }

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

func (r *Live) RadioError() error {
	err := r.endedErr()
	if errors.Is(err, errLiveEnded) {
		return err
	}
	return nil
}

func (r *Live) rxError(mode string, err error, errs *int) bool {
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
// A serial error of the read back counts as a receive error.
func (r *Live) checkConfigured(errs *int) error {
	br, ok := r.br.(configuredBridge)
	if !ok {
		return nil
	}
	configured, err := br.Configured()
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

func (r *Live) awaitImmediate() bool {
	for {
		select {
		case batch := <-r.immediate:
			r.finishImmediate(batch)
			return true
		case batch := <-r.async:
			if batch.done != nil {
				r.completeAsync(batch, errLiveBusy)
			} else {
				r.deferred = append(r.deferred, batch)
			}
		case <-r.stop:
			return false
		}
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
		return false, err
	}
	if event == nil {
		return false, nil
	}
	if event.Inbound != nil {
		if !event.Inbound.CRCOK {
			LogCRCDrop("csl inbound", "", event.Inbound)
			return true, nil
		}
		// No tune here: the chip receives these frames right after its own TX,
		// before the FOC loop converges, so their FREQEST is not a measurement.
		r.tuneDebugFrame(event.Inbound, "csl")
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
	r.confirmCSLBootstrap(pending.action.Bytes, pending.bootstrap, completion.Result, completion.Err)
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

// pollCSLQueue retries one failed poll. A late reply or a malformed event is a
// one-off after the bridge resyncs its input; a second failure in a row is not.
func (r *Live) pollCSLQueue(br queuedCSLBridge) (*cc1101.CSLQueueEvent, error) {
	event, err := r.pollCSLQueueOnce(br)
	if err == nil {
		return event, nil
	}
	log.Printf("CSL queue poll: %v, retrying once", err)
	select {
	case <-r.stop:
		return nil, err
	default:
	}
	return r.pollCSLQueueOnce(br)
}

func (r *Live) pollCSLQueueOnce(br queuedCSLBridge) (*cc1101.CSLQueueEvent, error) {
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

func (r *Live) dropCSL(err error) {
	if r.cslFlushed {
		return
	}
	if br, ok := r.br.(queuedCSLBridge); ok && !r.bridgeClosed.Load() {
		if err := br.FlushCSLQueue(); err != nil {
			log.Printf("CSL queue flush: %v", err)
		}
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
	}
	r.cslPending = make(map[uint16]liveCSLPending)
	r.cslGroups = make(map[uint16]*liveCSLGroup)
	r.cslFlushed = true
}

func (r *Live) sendTrain(frames [][]byte) error {
	_, err := r.sendFrames(frames)
	return err
}

func (r *Live) sendActions(actions []tado.Action) (ImmediateResult, error) {
	var result ImmediateResult
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
	for i := 0; i < len(actions); i++ {
		action := actions[i]
		if action.CSLTx {
			if err := flush(); err != nil {
				return result, err
			}
			if len(action.Bytes) < 5 {
				return result, fmt.Errorf("CSL Data frame is too short")
			}
			bootstrap := action.HasCSLBatch || r.cslBootstrap(action)
			if br, ok := r.br.(queuedCSLBridge); ok {
				result.CSLQueue = true
				if r.cslPending == nil {
					r.cslPending = make(map[uint16]liveCSLPending)
				}
				jobs := []cc1101.CSLQueueRequest{{ID: r.nextCSLID(), Frame: action.Bytes, DstShort: []byte{action.Bytes[4], action.Bytes[3]}, AckTimeout: liveCSLTxAckTimeout, Bootstrap: bootstrap, WaitResponse: action.WaitResponse, CSLPhase: r.phase}}
				pending := []liveCSLPending{{action: action, bootstrap: bootstrap}}
				for i+1 < len(actions) && sameCSLBatch(action, actions[i+1]) {
					i++
					next := actions[i]
					// A fragmented datagram needs every frame to have the same cold-start path.
					// The first frame can finish without creating an anchor for the next one.
					nextBootstrap := next.HasCSLBatch || bootstrap
					jobs = append(jobs, cc1101.CSLQueueRequest{ID: r.nextCSLID(), Frame: next.Bytes, DstShort: []byte{next.Bytes[4], next.Bytes[3]}, AckTimeout: liveCSLTxAckTimeout, Bootstrap: nextBootstrap, WaitResponse: next.WaitResponse, CSLPhase: r.phase})
					pending = append(pending, liveCSLPending{action: next, bootstrap: nextBootstrap})
				}
				r.markTX()
				if err := br.EnqueueCSLBatch(jobs); err != nil {
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
			logCSLTx(action, bootstrap, cslResult, err)
			if err != nil {
				return result, err
			}
			if cslResult.Ack != nil {
				result.Acks = append(result.Acks, append([]byte(nil), cslResult.Ack.Payload...))
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

// cslBootstrap applies the bootstrap cooldowns to one CSL action. An action
// that always bootstraps skips them but still counts as an attempt, so the
// global cooldown keeps other targets off the air after its train.
func (r *Live) cslBootstrap(action tado.Action) bool {
	if !action.CSLBootstrapAlways || len(action.Bytes) < 5 {
		return r.allowCSLBootstrap(action.Bytes, action.IdentifyCSLBootstrapRetry)
	}
	now := r.now()
	if r.bootstrapAttemptAt == nil {
		r.bootstrapAttemptAt = make(map[string]time.Time)
	}
	r.bootstrapAttemptAt[cslShort(action.Bytes)] = now
	r.lastBootstrapAttempt = now
	return true
}

func (r *Live) allowCSLBootstrap(frame []byte, identifyRetry bool) bool {
	if len(frame) < 5 {
		return false
	}
	now := r.now()
	key := cslShort(frame)
	if r.bootstrapAttemptAt == nil {
		r.bootstrapAttemptAt = make(map[string]time.Time)
	}
	if r.bootstrapConfirmedAt == nil {
		r.bootstrapConfirmedAt = make(map[string]time.Time)
	}
	if last := r.bootstrapConfirmedAt[key]; !last.IsZero() && now.Sub(last) < liveCSLBootstrapTargetCooldown {
		return false
	}
	if identifyRetry {
		r.bootstrapAttemptAt[key] = now
		r.lastBootstrapAttempt = now
		return true
	}
	if last := r.bootstrapAttemptAt[key]; !last.IsZero() && now.Sub(last) < liveCSLBootstrapRetryCooldown {
		return false
	}
	if !r.lastBootstrapAttempt.IsZero() && now.Sub(r.lastBootstrapAttempt) < liveCSLBootstrapGlobalCooldown {
		return false
	}
	r.bootstrapAttemptAt[key] = now
	r.lastBootstrapAttempt = now
	return true
}

func (r *Live) confirmCSLBootstrap(frame []byte, bootstrap bool, result *cc1101.CSLTxResult, err error) {
	if len(frame) < 5 {
		return
	}
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
	if !bootstrap || err != nil || result == nil || result.Ack == nil {
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
	if ra, ok := routerAdvertBurst(frames); ok {
		if br, ok := r.br.(routerAdvertBurstBridge); ok {
			r.markTX()
			sent, err := br.SendRouterAdvertBurst(ra, time.Second)
			if err != nil {
				return nil, err
			}
			if sent < 3 || sent > tado.RouterAdvertWakeUpCount+1 {
				return nil, fmt.Errorf("router advertisement burst returned invalid frame count %d", sent)
			}
			log.Printf("router advertisement burst: sent %d frames", sent)
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
			return acks, err
		}
		if sent != len(chunk) {
			return acks, fmt.Errorf("sent %d of %d frames", sent, len(chunk))
		}
		start = end
	}
	return acks, nil
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
		case <-time.After(liveCloseGrace):
			r.closeBridge()
			<-r.ended
			return
		}
		r.closeBridge()
	})
	return r.closeErr
}
