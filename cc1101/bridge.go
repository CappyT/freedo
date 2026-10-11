// Package cc1101 drives a CC1101 radio over the thin ESP32 serial<->SPI bridge.
// All CC1101 semantics (registers, strobes, FIFO) live here on the host; the
// ESP32 only frames raw SPI transactions. See firmware/cc1101_bridge for the
// firmware and docs "CC1101 Bridge - Driver e Setup".
package cc1101

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go.bug.st/serial"
)

var (
	errSerialDeadline  = errors.New("serial response deadline exceeded")
	errUnexpectedReply = errors.New("serial reply does not answer the request")
)

// serialStaleMax bounds the replies one request reads past before its own. A
// reply carries no command echo, so a reply of the wrong shape is the late
// reply of an earlier request, and the reply to this request follows it.
const serialStaleMax = 2

// serialDrainLimit bounds a drain: input that never goes quiet is not the tail
// of a stream.
const serialDrainLimit = 3 * time.Second

// bannerPrefix starts the PING banner of every firmware version.
const bannerPrefix = "CC1101BRIDGE"

// replyCheck reports whether a reply has the shape of the answer to a request.
type replyCheck func(status byte, data []byte) bool

// lateReply takes the reply of a request that missed its deadline, once the
// reply arrives. A nil lateReply discards it.
type lateReply func(status byte, data []byte)

// Serial framing (matches firmware/cc1101_bridge/cc1101_bridge.ino @ 921600 8N1):
//
//	request : 0xA5 cmd lenHi lenLo payload
//	response: 0x5A status lenHi lenLo payload
const (
	reqMagic = 0xA5
	rspMagic = 0x5A

	cmdPing     = 0x01
	cmdXfer     = 0x02 // full-duplex, CS-framed
	cmdGDO      = 0x03
	cmdReset    = 0x04
	cmdStrobe   = 0x05
	cmdRecv     = 0x06 // receive one packet, drained on-chip
	cmdStream   = 0x07 // continuous RX; streams frames until a duration elapses
	cmdTxTrain  = 0x08 // back-to-back TX burst, inter-frame gap set on-device
	cmdRespond  = 0x09 // continuous RX with on-device auto-response to a match
	cmdAutoAck  = 0x0A
	cmdRABurst  = 0x0B
	cmdCSLTx    = 0x0C
	cmdCSLQueue = 0x0D

	streamFrame = 0xF1 // 5A F1 <n> <n bytes> <freqest> <xor8>
	streamEnd   = 0xF2 // 5A F2 00
	streamAlive = 0xF3 // 5A F3 00 heartbeat: link live but quiet (keeps read timer fed)
)

// Bridge is a connection to the ESP32<->CC1101 bridge.
type Bridge struct {
	port       serial.Port
	timeout    time.Duration
	curFSCTRL0 byte // current frequency-offset register
	// owed has one entry per request that missed its deadline before its reply
	// arrived, in request order. The next request reads those replies first.
	owed []lateReply
	// late holds the CSL queue events that arrived as the reply of a poll that
	// missed its deadline. The next polls return them first.
	late []*CSLQueueEvent
}

// Open connects to the ESP32 bridge on the given serial port. The ESP32
// auto-resets when the port opens, so this waits for it to boot.
func Open(portName string) (*Bridge, error) {
	p, err := serial.Open(portName, &serial.Mode{BaudRate: 921600})
	if err != nil {
		return nil, err
	}
	if err := p.SetReadTimeout(100 * time.Millisecond); err != nil {
		p.Close()
		return nil, err
	}
	time.Sleep(2 * time.Second) // ESP32 boot if DTR reset fired
	b := &Bridge{port: p, timeout: 4 * time.Second}
	// The DTR reset is unreliable on the CH340, so a stream left running by a
	// previous (killed) session may still be flooding the port. Send a byte to
	// abort any running stream (firmware breaks on Serial.available()), then drain
	// everything until the port goes quiet, so we start from a clean command loop.
	p.Write([]byte{0x00})
	if err := b.drain(400*time.Millisecond, serialDrainLimit); err != nil {
		p.Close()
		return nil, err
	}
	return b, nil
}

// Close releases the serial port.
func (b *Bridge) Close() error { return b.port.Close() }

// drain discards all input until the port is quiet for `quiet`: a leftover
// stream at startup, or the tail of an aborted stream. Input that still
// arrives after limit is an error.
func (b *Bridge) drain(quiet, limit time.Duration) error {
	b.port.SetReadTimeout(quiet)
	defer b.port.SetReadTimeout(100 * time.Millisecond)
	buf := make([]byte, 512)
	deadline := time.Now().Add(limit)
	for {
		n, err := b.port.Read(buf)
		if err != nil || n == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("serial input still busy after %s", limit)
		}
	}
}

func (b *Bridge) readExact(n int) ([]byte, error) {
	return b.readExactUntil(n, time.Now().Add(b.timeout+time.Second))
}

func (b *Bridge) readExactUntil(n int, deadline time.Time) ([]byte, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w after %d/%d bytes", errSerialDeadline, len(out), n)
		}
		m, err := b.port.Read(buf[:n-len(out)])
		if err != nil {
			return nil, err
		}
		if m > 0 {
			out = append(out, buf[:m]...)
			continue
		}
	}
	return out, nil
}

// req sends a request and returns (status, payload). It resyncs on the response
// magic byte, tolerating stray boot/noise bytes, and reads past a reply that
// valid rejects.
func (b *Bridge) req(cmd byte, payload []byte, valid replyCheck) (byte, []byte, error) {
	return b.reqUntil(cmd, payload, time.Now().Add(b.timeout+time.Second), valid, nil)
}

// reqUntil sends a request and reads its reply by deadline. The firmware
// answers every request once and in request order, so a reply that valid
// rejects is the late reply of an earlier request, and the reply to this
// request follows it. A request that misses its deadline leaves its reply
// owed: the next request resyncs first and hands that reply to late.
func (b *Bridge) reqUntil(cmd byte, payload []byte, deadline time.Time, valid replyCheck, late lateReply) (byte, []byte, error) {
	if err := b.resync(deadline); err != nil {
		return 0, nil, err
	}
	if err := b.send(cmd, payload); err != nil {
		return 0, nil, err
	}
	for skipped := 0; ; skipped++ {
		status, data, err := b.readReply(deadline)
		if err != nil {
			b.owed = append(b.owed, late)
			return 0, nil, err
		}
		if parserReject(status) || valid(status, data) {
			return status, data, nil
		}
		if skipped == serialStaleMax {
			b.owed = append(b.owed, late)
			return 0, nil, fmt.Errorf("%w: status 0x%02x with %d bytes", errUnexpectedReply, status, len(data))
		}
	}
}

// resync reads the replies still owed to requests that missed their deadline.
// A PING goes out behind them. Every reply before its banner is a late reply,
// in request order, and the banner shows that nothing else is in flight. A
// bridge that rebooted lost the owed replies, and the banner comes first.
func (b *Bridge) resync(deadline time.Time) error {
	if len(b.owed) == 0 {
		return nil
	}
	if err := b.send(cmdPing, nil); err != nil {
		return err
	}
	b.owed = append(b.owed, nil)
	for extra := 0; ; {
		status, data, err := b.readReply(deadline)
		if err != nil {
			return fmt.Errorf("serial resync: %w", err)
		}
		if isBanner(status, data) {
			b.owed = nil
			return nil
		}
		if len(b.owed) > 1 {
			late := b.owed[0]
			b.owed = b.owed[1:]
			if late != nil {
				late(status, data)
			}
			continue
		}
		if extra++; extra > serialStaleMax {
			return fmt.Errorf("serial resync: %w: status 0x%02x with %d bytes", errUnexpectedReply, status, len(data))
		}
	}
}

func (b *Bridge) send(cmd byte, payload []byte) error {
	n := len(payload)
	hdr := []byte{reqMagic, cmd, byte(n >> 8), byte(n)}
	_, err := b.port.Write(append(hdr, payload...))
	return err
}

func (b *Bridge) readReply(deadline time.Time) (byte, []byte, error) {
	for {
		m, err := b.readExactUntil(1, deadline)
		if err != nil {
			return 0, nil, err
		}
		if m[0] == rspMagic {
			break
		}
	}
	head, err := b.readExactUntil(3, deadline)
	if err != nil {
		return 0, nil, err
	}
	status := head[0]
	rlen := int(head[1])<<8 | int(head[2])
	if err := responsePayloadLength(rlen); err != nil {
		return 0, nil, err
	}
	var data []byte
	if rlen > 0 {
		if data, err = b.readExactUntil(rlen, deadline); err != nil {
			return 0, nil, err
		}
	}
	return status, data, nil
}

// parserReject reports the status the firmware parser answers a request it
// could not read with, ST_BAD_CMD to ST_BAD_ARGS. It answers any request.
func parserReject(status byte) bool {
	return status >= 0xe0 && status <= 0xe3
}

func isBanner(status byte, data []byte) bool {
	return status == 0 && strings.HasPrefix(string(data), bannerPrefix)
}

// okLen accepts a reply with status 0 and n payload bytes.
func okLen(n int) replyCheck {
	return func(status byte, data []byte) bool { return status == 0 && len(data) == n }
}

func responsePayloadLength(n int) error {
	if n > maxReqPayload {
		return fmt.Errorf("serial response length %d exceeds %d", n, maxReqPayload)
	}
	return nil
}

// --- primitives ---

// Ping returns the firmware banner.
func (b *Bridge) Ping() (string, error) {
	_, d, err := b.req(cmdPing, nil, isBanner)
	return string(d), err
}

// Reset issues a manual CC1101 power-on reset.
func (b *Bridge) Reset() error { _, _, err := b.req(cmdReset, nil, okLen(0)); return err }

// Xfer does a full-duplex SPI transfer, returning the MISO bytes.
func (b *Bridge) Xfer(data []byte) ([]byte, error) {
	_, d, err := b.req(cmdXfer, data, okLen(len(data)))
	return d, err
}
