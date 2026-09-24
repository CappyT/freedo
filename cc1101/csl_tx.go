package cc1101

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

var ErrCSLTxNoAnchor = errors.New("CSLTX has no target sample anchor")

const (
	cslTxInput            = 0x14
	cslTxTXFail           = 0x15
	cslTxNoAck            = 0x16
	cslTxBadAck           = 0x17
	cslTxNoAnchor         = 0x18
	cslTxMinSent          = 3
	cslTxMaxSent          = 34
	cslTxBootstrapMaxSent = 255
	cslTxFlagBootstrap    = 1 << 0
	cslTxFlagFollow       = 1 << 1
	cslTxFollowMaxFrames  = 6
	cslTxMaxRawFrame      = 1 + maxTrainFrame + 2
)

// CSLTxResult is the on-chip CSL transmit outcome.
type CSLTxResult struct {
	Sent    int
	Ack     *RxFrame
	Follow  []*RxFrame
	Dropped int
}

// SendCSLData sends one Data frame in a timed unicast CSL transaction.
func (b *Bridge) SendCSLData(frame, dstShort []byte, ackTimeout, timeout time.Duration, bootstrap, waitResponse bool, cslPhase uint16) (*CSLTxResult, error) {
	payload, err := cslTxRequest(frame, dstShort, ackTimeout, bootstrap, waitResponse, cslPhase)
	if err != nil {
		return nil, err
	}
	savedTimeout := b.timeout
	if timeout > b.timeout {
		b.timeout = timeout
	}
	defer func() { b.timeout = savedTimeout }()
	status, data, err := b.req(cmdCSLTx, payload, func(status byte, data []byte) bool {
		if status == 0 {
			_, err := parseCSLTxResult(status, data, bootstrap, waitResponse)
			return err == nil
		}
		return len(data) == 1
	})
	if err != nil {
		return nil, err
	}
	return parseCSLTxResult(status, data, bootstrap, waitResponse)
}

func cslTxRequest(frame, dstShort []byte, ackTimeout time.Duration, bootstrap, waitResponse bool, cslPhase uint16) ([]byte, error) {
	if len(frame) < 16 || len(frame) > maxTrainFrame || (frame[0] != 0x69 && frame[0] != 0x79) || frame[1] != 0xec {
		return nil, fmt.Errorf("CSLTX frame is not a valid Data frame")
	}
	if len(dstShort) != 2 || dstShort[0] != frame[4] || dstShort[1] != frame[3] {
		return nil, fmt.Errorf("CSLTX destination short does not match Data frame")
	}
	ackMS := ackTimeout.Milliseconds()
	if ackMS <= 0 || ackMS > 1000 {
		return nil, fmt.Errorf("CSLTX ACK timeout %s out of range", ackTimeout)
	}
	headerLen := 5
	if waitResponse {
		headerLen += 2
	}
	payload := make([]byte, headerLen, headerLen+len(frame))
	binary.BigEndian.PutUint16(payload[:2], uint16(ackMS))
	if bootstrap {
		payload[2] |= cslTxFlagBootstrap
	}
	if waitResponse {
		payload[2] |= cslTxFlagFollow
	}
	copy(payload[3:5], dstShort)
	if waitResponse {
		binary.LittleEndian.PutUint16(payload[5:7], cslPhase)
	}
	payload = append(payload, frame...)
	return payload, nil
}

func parseCSLTxResult(status byte, data []byte, bootstrap, waitResponse bool) (*CSLTxResult, error) {
	if len(data) < 1 {
		return nil, fmt.Errorf("CSLTX response has no frame count")
	}
	result := &CSLTxResult{Sent: int(data[0])}
	maxSent := cslTxMaxSent
	if bootstrap {
		maxSent = cslTxBootstrapMaxSent
	}
	switch status {
	case 0x00:
		if result.Sent < cslTxMinSent || result.Sent > maxSent {
			return result, fmt.Errorf("CSLTX returned invalid frame count %d", result.Sent)
		}
		var ackRaw []byte
		if waitResponse {
			ackRaw, result.Follow, result.Dropped = parseCSLTxFollow(data[1:])
		} else {
			ackRaw = data[1:]
		}
		result.Ack = parseCSLTxAck(ackRaw)
		if result.Ack == nil {
			return result, fmt.Errorf("CSLTX response has malformed Enhanced ACK")
		}
		if waitResponse && ackRaw == nil {
			return result, fmt.Errorf("CSLTX follow response is malformed")
		}
		return result, nil
	case cslTxNoAck:
		if len(data) != 1 || result.Sent < cslTxMinSent || result.Sent > maxSent {
			return result, fmt.Errorf("CSLTX no-ACK response is malformed")
		}
		return result, nil
	case cslTxInput:
		return result, fmt.Errorf("CSLTX rejected input")
	case cslTxTXFail:
		return result, fmt.Errorf("CSLTX transmit failed after %d frames", result.Sent)
	case cslTxBadAck:
		return result, fmt.Errorf("CSLTX received invalid Enhanced ACK")
	case cslTxNoAnchor:
		return result, ErrCSLTxNoAnchor
	default:
		return result, fmt.Errorf("CSLTX status 0x%02x", status)
	}
}

func parseCSLTxFollow(data []byte) ([]byte, []*RxFrame, int) {
	if len(data) < 33 {
		return nil, nil, 0
	}
	ackLen := int(data[0])
	if ackLen != 32 || len(data) < 1+ackLen+2 {
		return nil, nil, 0
	}
	ack := data[1 : 1+ackLen]
	off := 1 + ackLen
	n := int(data[off])
	off++
	if n > cslTxFollowMaxFrames {
		return nil, nil, 0
	}
	follow := make([]*RxFrame, 0, n)
	for range n {
		if off >= len(data) {
			return nil, nil, 0
		}
		rawLen := int(data[off])
		off++
		if rawLen < 3 || rawLen > cslTxMaxRawFrame || off+rawLen > len(data) {
			return nil, nil, 0
		}
		frame := parseRxBlob(data[off : off+rawLen])
		if frame == nil {
			return nil, nil, 0
		}
		follow = append(follow, frame)
		off += rawLen
	}
	if off+1 != len(data) {
		return nil, nil, 0
	}
	return ack, follow, int(data[off])
}

func parseCSLTxAck(raw []byte) *RxFrame {
	if len(raw) != 32 || raw[0] != 29 {
		return nil
	}
	frame := parseRxBlob(raw)
	if frame == nil || len(frame.Payload) != 29 || frame.Payload[0] != 0x42 || frame.Payload[1] != 0xee {
		return nil
	}
	return frame
}
