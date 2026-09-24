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

// Firmware CSL timing (cc1101_bridge_v2.ino): CSL_PERIOD_US, CSL_GUARD_MAX_US,
// CCA_MAX_US, CSL_BOOTSTRAP_US, CSL_FOLLOW_TMO_MS and CSL_GUARD_MARGIN_MS.
const (
	cslFirmwarePeriod         = time.Second
	cslFirmwareMaxSampleGuard = 500 * time.Millisecond
	cslFirmwareMaxListen      = 7808 * time.Microsecond
	cslFirmwareBootstrap      = time.Second
	cslFirmwareFollow         = 500 * time.Millisecond
	cslFirmwareGuardMargin    = 400 * time.Millisecond
)

// CSLQueueJobGuard is the longest time the firmware spends on one queued job
// once it schedules it. cslStart ends every job at its guard: wake window, ACK
// timeout, follow window and margin. An anchored job waits for the first sample
// of the target at least one guard and the longest listen away, up to one CSL
// period more, and its wake window ends one guard after that sample. The guard
// grows with the age of the anchor up to half a period. A bootstrap job listens
// and then sends a train of one period, and it also takes the anchored path
// when the target has an anchor with no miss, so the wake window is the longer
// of the two for every job.
func CSLQueueJobGuard(job CSLQueueRequest) time.Duration {
	wake := max(cslFirmwarePeriod+2*cslFirmwareMaxSampleGuard, cslFirmwareBootstrap) + cslFirmwareMaxListen
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
	status, data, err := b.req(cmdCSLQueue, payload, func(status byte, data []byte) bool {
		switch status {
		case 0:
			return len(data) == 1 && int(data[0]) == len(jobs)
		case cslTxInput, cslQueueFull, cslQueueNeighborFull:
			return len(data) == 0
		}
		return false
	})
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
	if len(b.late) == 0 {
		status, data, err := b.reqUntil(cmdCSLQueue, []byte{cslQueuePoll}, deadline, validCSLQueueEvent, b.keepLateEvent)
		if err != nil {
			if errors.Is(err, errSerialDeadline) {
				return nil, fmt.Errorf("%w: %v", ErrCSLPollTimeout, err)
			}
			return nil, err
		}
		if parserReject(status) {
			return nil, fmt.Errorf("CSL queue poll status 0x%02x", status)
		}
		// The resync of this request can put late events ahead of this reply.
		b.keepLateEvent(status, data)
	}
	if len(b.late) == 0 {
		return nil, nil
	}
	event := b.late[0]
	b.late = b.late[1:]
	return event, nil
}

// keepLateEvent queues the event a poll reply carries for the next poll.
func (b *Bridge) keepLateEvent(status byte, data []byte) {
	if event, err := parseCSLQueueEvent(status, data); err == nil && event != nil {
		b.late = append(b.late, event)
	}
}

func validCSLQueueEvent(status byte, data []byte) bool {
	_, err := parseCSLQueueEvent(status, data)
	return err == nil
}

// parseCSLQueueEvent decodes a poll reply: nil for an empty queue, else one
// completion or one inbound frame.
func parseCSLQueueEvent(status byte, data []byte) (*CSLQueueEvent, error) {
	if len(data) == 0 && status == 0 {
		return nil, nil
	}
	if len(data) < 1 {
		return nil, fmt.Errorf("CSL queue event is malformed")
	}
	if data[0] == 1 {
		frame := parseRxBlobExact(data[1:])
		if status != 0 || frame == nil {
			return nil, fmt.Errorf("CSL queue inbound frame is malformed")
		}
		return &CSLQueueEvent{Inbound: frame}, nil
	}
	// The firmware appends the Enhanced ACK and the follow frames to an OK
	// completion only, and every other outcome is a CSLTX failure status.
	if data[0] != 0 || len(data) < 5 || (status != 0 && (len(data) != 5 || !cslTxFailure(status))) {
		return nil, fmt.Errorf("CSL queue completion is malformed")
	}
	completion := &CSLQueueCompletion{ID: binary.BigEndian.Uint16(data[1:3])}
	flags := data[3]
	completion.Result, completion.Err = parseCSLTxResult(status, data[4:], flags&cslTxFlagFollow != 0)
	if status == 0 && completion.Err != nil {
		return nil, fmt.Errorf("CSL queue completion is malformed: %w", completion.Err)
	}
	return &CSLQueueEvent{Completion: completion}, nil
}

// FlushCSLQueue cancels queued jobs and prevents a stale downlink after a gate change.
func (b *Bridge) FlushCSLQueue() error {
	status, data, err := b.req(cmdCSLQueue, []byte{cslQueueFlush}, okLen(0))
	if err != nil {
		return err
	}
	if status != 0 || len(data) != 0 {
		return fmt.Errorf("CSL queue flush status 0x%02x", status)
	}
	return nil
}
