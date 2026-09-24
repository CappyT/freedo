package cc1101

import (
	"encoding/binary"
	"fmt"
	"time"
)

const (
	autoAckNoFrame      = 0x11
	autoAckFrameNoAck   = 0x12
	autoAckTransmitFail = 0x13
)

var autoAckSuffix = [3]byte{0xc5, 0x1b, 0x00}

type AutoAckStatus string

const (
	AutoAckNoFrame      AutoAckStatus = "no-frame"
	AutoAckFrameNoAck   AutoAckStatus = "frame-no-ack"
	AutoAckSent         AutoAckStatus = "ack-sent"
	AutoAckTransmitFail AutoAckStatus = "ack-failed"
)

type AutoAckResult struct {
	Status AutoAckStatus
	Raw    []byte
	Frame  *RxFrame
}

func (b *Bridge) RecvAutoAck(timeout time.Duration, coordinatorEUI []byte, cslPhase uint16) (*AutoAckResult, error) {
	payload, err := autoAckRequest(timeout, coordinatorEUI, cslPhase)
	if err != nil {
		return nil, err
	}
	savedTimeout := b.timeout
	if timeout > b.timeout {
		b.timeout = timeout
	}
	defer func() { b.timeout = savedTimeout }()
	status, raw, err := b.req(cmdAutoAck, payload)
	if err != nil {
		return nil, err
	}
	result := &AutoAckResult{Raw: append([]byte(nil), raw...)}
	switch status {
	case autoAckNoFrame:
		if len(raw) != 0 {
			return nil, fmt.Errorf("AUTOACK no-frame response has %d bytes", len(raw))
		}
		result.Status = AutoAckNoFrame
	case 0x00:
		result.Status = AutoAckSent
	case autoAckFrameNoAck:
		result.Status = AutoAckFrameNoAck
	case autoAckTransmitFail:
		result.Status = AutoAckTransmitFail
	default:
		return nil, fmt.Errorf("AUTOACK status 0x%02x", status)
	}
	if result.Status != AutoAckNoFrame {
		result.Frame = parseAutoAckRXBlob(raw)
		if result.Frame == nil {
			return nil, fmt.Errorf("AUTOACK response has malformed RX blob")
		}
	}
	return result, nil
}

// parseAutoAckRXBlob accepts the blob with and without the trailing FREQEST byte
// firmware v2.1 appends, so the host runs on either firmware.
func parseAutoAckRXBlob(raw []byte) *RxFrame {
	if len(raw) < 3 {
		return nil
	}
	if want := 1 + int(raw[0]) + 2; len(raw) != want && len(raw) != want+1 {
		return nil
	}
	return parseRxBlob(raw)
}

func autoAckRequest(timeout time.Duration, coordinatorEUI []byte, cslPhase uint16) ([]byte, error) {
	ms := timeout.Milliseconds()
	if ms <= 0 {
		return nil, fmt.Errorf("AUTOACK timeout must be positive")
	}
	if len(coordinatorEUI) != 8 {
		return nil, fmt.Errorf("AUTOACK coordinator EUI is %d bytes, want 8", len(coordinatorEUI))
	}
	if ms > 0xffff {
		ms = 0xffff
	}
	payload := make([]byte, 12)
	binary.BigEndian.PutUint16(payload, uint16(ms))
	copy(payload[2:10], coordinatorEUI)
	binary.LittleEndian.PutUint16(payload[10:12], cslPhase)
	return payload, nil
}

func buildAutoAckFrame(raw, coordinatorEUI []byte, cslPhase uint16) ([]byte, bool) {
	if len(coordinatorEUI) != 8 || len(raw) < 19 || raw[len(raw)-1]&0x80 == 0 {
		return nil, false
	}
	length := int(raw[0])
	if length < 16 || len(raw) != 1+length+2 {
		return nil, false
	}
	frame := raw[1 : 1+length]
	if !((frame[0] == 0x69 || frame[0] == 0x79) && frame[1] == 0xec) || !sameBytes(frame[3:11], coordinatorEUI) {
		return nil, false
	}
	ack := make([]byte, 29)
	ack[0], ack[1], ack[2] = 0x42, 0xee, frame[2]
	copy(ack[3:8], frame[11:16])
	copy(ack[8:11], autoAckSuffix[:])
	copy(ack[11:19], coordinatorEUI)
	ack[19], ack[20] = 0x04, 0x0d
	binary.LittleEndian.PutUint16(ack[21:23], cslPhase)
	ack[23], ack[24], ack[25], ack[26] = 0x35, 0x0c, 0x80, 0x3f
	binary.LittleEndian.PutUint16(ack[27:29], autoAckFCS(ack[:27]))
	return ack, true
}

func autoAckFCS(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b)
		for bit := 0; bit < 8; bit++ {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0x8408
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

func sameBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
