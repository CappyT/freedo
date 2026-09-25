package cc1101

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// ErrPairWindowUnsupported means the bridge firmware predates v2.9: it answers
// no Enhanced Beacon Request and refuses the Frame Pending advertisement.
var ErrPairWindowUnsupported = errors.New("bridge firmware has no pair window (needs v2.9)")

const (
	cmdPairWindow = 0x0E
	statusBadCmd  = 0xE0
	pairWindowMax = 10 * time.Minute // the firmware refuses a longer window
)

// PairWindow arms the Enhanced Beacon answer of the bridge. Until Duration
// ends, the bridge answers an Enhanced Beacon Request that verifies under Key
// with an Enhanced Beacon from Coordinator, 4 ms after the request. The beacons
// take the MAC seq and the frame counter from Seq and Counter upward, Count
// beacons at most. A zero Duration closes the window.
type PairWindow struct {
	Duration    time.Duration
	Key         []byte // 16 bytes
	Coordinator []byte // 8-byte wire EUI
	Seq         byte
	Counter     uint32
	Count       byte
}

// SetPairWindow opens or closes the pair window of the bridge. It returns the
// number of Enhanced Beacons the bridge sent in the window the request ends.
//
//	request open : durHi durLo key[16] coordinator[8] seq counter[4 LE] count
//	request close: 00 00
//	reply        : sent[1]
func (b *Bridge) SetPairWindow(w PairWindow) (int, error) {
	payload := []byte{0, 0}
	if w.Duration != 0 {
		if w.Duration < time.Second || w.Duration > pairWindowMax || len(w.Key) != 16 || len(w.Coordinator) != 8 || w.Count == 0 {
			return 0, fmt.Errorf("pair window %v: needs 1 s to %v, a 16-byte key, an 8-byte coordinator and a count", w.Duration, pairWindowMax)
		}
		payload = binary.BigEndian.AppendUint16(payload[:0], uint16(w.Duration/time.Second))
		payload = append(payload, w.Key...)
		payload = append(payload, w.Coordinator...)
		payload = append(payload, w.Seq)
		payload = binary.LittleEndian.AppendUint32(payload, w.Counter)
		payload = append(payload, w.Count)
	}
	status, data, err := b.req(cmdPairWindow, payload, func(_ byte, data []byte) bool { return len(data) == 1 })
	if err != nil {
		return 0, err
	}
	switch {
	case status == statusBadCmd:
		return 0, ErrPairWindowUnsupported
	case status != 0:
		return 0, fmt.Errorf("PAIRWIN status 0x%02x", status)
	case len(data) != 1:
		return 0, fmt.Errorf("PAIRWIN reply has %d bytes", len(data))
	}
	return int(data[0]), nil
}
