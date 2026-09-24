package cc1101

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

var (
	ErrCSLQueueFull         = errors.New("CSL queue for neighbor is full")
	ErrCSLQueueNeighborFull = errors.New("CSL scheduler neighbor table is full")
	ErrCSLPollTimeout       = errors.New("CSL queue poll timed out")
)

const (
	cslQueueEnqueue      = 0
	cslQueuePoll         = 1
	cslQueueFlush        = 2
	cslQueueFull         = 0x19
	cslQueueNeighborFull = 0x1a
)

// Firmware CSL timing (cc1101_bridge_v2.ino): CSL_PERIOD_US, CSL_SAMPLE_GUARD_US,
// CSL_BOOTSTRAP_US, CSL_FOLLOW_TMO_MS and CSL_GUARD_MARGIN_MS.
const (
	cslFirmwarePeriod      = time.Second
	cslFirmwareSampleGuard = 40 * time.Millisecond
	cslFirmwareBootstrap   = time.Second
	cslFirmwareFollow      = 500 * time.Millisecond
	cslFirmwareGuardMargin = 400 * time.Millisecond
)

// CSLQueueJobGuard is the longest time the firmware spends on one queued job
// once it schedules it. cslStart ends every job at its guard: wake window, ACK
// timeout, follow window and margin. An anchored job waits for the first sample
// of the target at least one sample guard away, up to one CSL period plus the
// guard, and its wake window ends one guard after that sample. A bootstrap job
// also takes that path when the target has an anchor with no miss, so the wake
// window is the longer of the two for every job.
func CSLQueueJobGuard(job CSLQueueRequest) time.Duration {
	wake := max(cslFirmwarePeriod+2*cslFirmwareSampleGuard, cslFirmwareBootstrap)
	guard := wake + job.AckTimeout + cslFirmwareGuardMargin
	if job.WaitResponse {
		guard += cslFirmwareFollow
	}
	return guard
}

// CSLQueueCompletion is one completed asynchronous CSL transaction.
type CSLQueueCompletion struct {
	ID     uint16
	Result *CSLTxResult
	Err    error
}

// CSLQueueRequest is one member of an all-or-nothing per-neighbor submission.
type CSLQueueRequest struct {
	ID           uint16
	Frame        []byte
	DstShort     []byte
	AckTimeout   time.Duration
	Bootstrap    bool
	WaitResponse bool
	CSLPhase     uint16
}

// CSLQueueEvent is either a completed transaction or an authenticated inbound
// frame captured while the on-chip scheduler owns the radio.
type CSLQueueEvent struct {
	Completion *CSLQueueCompletion
	Inbound    *RxFrame
}

// EnqueueCSLData submits one timed unicast to the on-chip per-neighbor FIFO.
func (b *Bridge) EnqueueCSLData(id uint16, frame, dstShort []byte, ackTimeout time.Duration, bootstrap, waitResponse bool, cslPhase uint16) error {
	return b.EnqueueCSLBatch([]CSLQueueRequest{{
		ID: id, Frame: frame, DstShort: dstShort, AckTimeout: ackTimeout,
		Bootstrap: bootstrap, WaitResponse: waitResponse, CSLPhase: cslPhase,
	}})
}

// EnqueueCSLBatch admits all jobs for one neighbor, or none of them.
func (b *Bridge) EnqueueCSLBatch(jobs []CSLQueueRequest) error {
	if len(jobs) == 0 || len(jobs) > 32 {
		return fmt.Errorf("CSL queue batch size %d out of range", len(jobs))
	}
	payload := []byte{cslQueueEnqueue, byte(len(jobs))}
	for _, job := range jobs {
		tx, err := cslTxRequest(job.Frame, job.DstShort, job.AckTimeout, job.Bootstrap, job.WaitResponse, job.CSLPhase)
		if err != nil {
			return err
		}
		if len(tx) > 0xffff {
			return fmt.Errorf("CSL queue job is too large")
		}
		record := make([]byte, 6, 6+len(tx))
		binary.BigEndian.PutUint16(record[:2], job.ID)
		binary.BigEndian.PutUint16(record[2:4], uint16(len(tx)))
		binary.LittleEndian.PutUint16(record[4:6], job.CSLPhase)
		payload = append(payload, record...)
		payload = append(payload, tx...)
	}
	if len(payload) > maxReqPayload {
		return fmt.Errorf("CSL queue batch exceeds serial payload")
	}
	status, data, err := b.req(cmdCSLQueue, payload)
	if err != nil {
		return err
	}
	if status == cslQueueFull {
		return ErrCSLQueueFull
	}
	if status == cslQueueNeighborFull {
		return ErrCSLQueueNeighborFull
	}
	if status != 0 || len(data) != 1 || int(data[0]) != len(jobs) {
		return fmt.Errorf("CSL queue enqueue status 0x%02x", status)
	}
	return nil
}

// PollCSLQueue returns the next completion or inbound frame, or nil when none is ready.
func (b *Bridge) PollCSLQueue() (*CSLQueueEvent, error) {
	return b.pollCSLQueueUntil(time.Now().Add(b.timeout + time.Second))
}

// PollCSLQueueWithin bounds one poll without introducing a second serial owner.
func (b *Bridge) PollCSLQueueWithin(timeout time.Duration) (*CSLQueueEvent, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("CSL queue poll timeout %s", timeout)
	}
	return b.pollCSLQueueUntil(time.Now().Add(timeout))
}

func (b *Bridge) pollCSLQueueUntil(deadline time.Time) (*CSLQueueEvent, error) {
	status, data, err := b.reqUntil(cmdCSLQueue, []byte{cslQueuePoll}, deadline)
	if err != nil {
		if errors.Is(err, errSerialDeadline) {
			return nil, fmt.Errorf("%w: %v", ErrCSLPollTimeout, err)
		}
		return nil, err
	}
	if len(data) == 0 && status == 0 {
		return nil, nil
	}
	// A malformed event is most likely the late reply of an earlier request, so
	// the reply to this poll is still on the wire: drop it before the next one.
	if len(data) < 1 {
		b.drain(serialResyncQuiet)
		return nil, fmt.Errorf("CSL queue event is malformed")
	}
	if data[0] == 1 {
		frame := parseRxBlob(data[1:])
		if frame == nil {
			b.drain(serialResyncQuiet)
			return nil, fmt.Errorf("CSL queue inbound frame is malformed")
		}
		return &CSLQueueEvent{Inbound: frame}, nil
	}
	if data[0] != 0 || len(data) < 5 {
		b.drain(serialResyncQuiet)
		return nil, fmt.Errorf("CSL queue completion is malformed")
	}
	completion := &CSLQueueCompletion{ID: binary.BigEndian.Uint16(data[1:3])}
	flags := data[3]
	completion.Result, completion.Err = parseCSLTxResult(status, data[4:], flags&cslTxFlagBootstrap != 0, flags&cslTxFlagFollow != 0)
	return &CSLQueueEvent{Completion: completion}, nil
}

// FlushCSLQueue cancels queued jobs and prevents a stale downlink after a gate change.
func (b *Bridge) FlushCSLQueue() error {
	status, data, err := b.req(cmdCSLQueue, []byte{cslQueueFlush})
	if err != nil {
		return err
	}
	if status != 0 || len(data) != 0 {
		return fmt.Errorf("CSL queue flush status 0x%02x", status)
	}
	return nil
}
