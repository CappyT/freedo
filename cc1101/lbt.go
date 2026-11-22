package cc1101

import (
	"bytes"
	"errors"
	"fmt"
)

// ErrLBTUnsupported means the bridge firmware does not know CMD_LBT.
var ErrLBTUnsupported = errors.New("bridge firmware has no listen-before-talk switch")

// LBT is the listen-before-talk setting of the firmware. With Enabled, the
// firmware listens before the first frame of a CSL job and sends nothing while
// the level is above ThresholdDBm. It answers such a job with the status
// ST_CSL_TX_BUSY, which is ErrCSLTxBusy.
type LBT struct {
	Enabled      bool
	ThresholdDBm int8
}

// SetLBT sets the switch and the threshold. The request is [enable 0|1,
// threshold int8 dBm] and the firmware echoes both bytes. It accepts the
// request while the chip is unconfigured.
func (b *Bridge) SetLBT(l LBT) error {
	payload := []byte{0, byte(l.ThresholdDBm)}
	if l.Enabled {
		payload[0] = 1
	}
	status, data, err := b.req(cmdLBT, payload, func(status byte, data []byte) bool {
		return (status == 0 && len(data) == 2) || parserReject(status)
	})
	if err != nil {
		return err
	}
	switch {
	case status == statusBadCmd:
		return ErrLBTUnsupported
	case status != 0:
		return fmt.Errorf("LBT status 0x%02x", status)
	case !bytes.Equal(data, payload):
		return fmt.Errorf("LBT reply %x does not echo the request %x", data, payload)
	}
	return nil
}
