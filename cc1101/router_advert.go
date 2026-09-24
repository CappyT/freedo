package cc1101

import (
	"fmt"
	"time"
)

const (
	routerAdvertBurstMinSent = 3
	routerAdvertBurstMaxSent = 255
	routerAdvertBurstInput   = 0x14
	routerAdvertBurstTXFail  = 0x15
)

// SendRouterAdvertBurst sends a timed wake-up burst and the Router Advertisement on-chip.
func (b *Bridge) SendRouterAdvertBurst(frame []byte, timeout time.Duration) (int, error) {
	if len(frame) < 3 || len(frame) > maxTrainFrame {
		return 0, fmt.Errorf("router advertisement frame length %d out of range", len(frame))
	}
	if frame[0] != 0x49 || frame[1] != 0xe8 {
		return 0, fmt.Errorf("router advertisement frame is not 0x49e8")
	}
	savedTimeout := b.timeout
	if timeout > b.timeout {
		b.timeout = timeout
	}
	defer func() { b.timeout = savedTimeout }()
	status, data, err := b.req(cmdRABurst, frame, func(_ byte, data []byte) bool { return len(data) == 1 })
	if err != nil {
		return 0, err
	}
	if len(data) != 1 {
		return 0, fmt.Errorf("RABURST response has %d-byte payload", len(data))
	}
	sent := int(data[0])
	switch status {
	case 0x00:
		if sent < routerAdvertBurstMinSent || sent > routerAdvertBurstMaxSent {
			return sent, fmt.Errorf("RABURST returned invalid frame count %d", sent)
		}
		return sent, nil
	case routerAdvertBurstInput:
		return sent, fmt.Errorf("RABURST rejected input")
	case routerAdvertBurstTXFail:
		return sent, fmt.Errorf("RABURST transmit failed after %d frames", sent)
	default:
		return sent, fmt.Errorf("RABURST status 0x%02x", status)
	}
}
