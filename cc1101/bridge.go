// Package cc1101 drives a CC1101 radio over the thin ESP32 serial<->SPI bridge.
// All CC1101 semantics (registers, strobes, FIFO) live here on the host; the
// ESP32 only frames raw SPI transactions. See tado_capture/cc1101_bridge for the
// firmware and docs "CC1101 Bridge - Driver e Setup".
package cc1101

import (
	"errors"
	"fmt"
	"time"

	"go.bug.st/serial"
)

var errSerialDeadline = errors.New("serial response deadline exceeded")

// Serial framing (matches tado_capture/cc1101_bridge/firmware/cc1101_bridge_v2/cc1101_bridge_v2.ino @ 921600 8N1):
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
	b.drain(400 * time.Millisecond)
	return b, nil
}

// Close releases the serial port.
func (b *Bridge) Close() error { return b.port.Close() }

// drain discards all input until the port is quiet for `quiet`. Used at startup
// to flush a leftover stream from a previous session.
func (b *Bridge) drain(quiet time.Duration) {
	b.port.SetReadTimeout(quiet)
	buf := make([]byte, 512)
	for {
		n, err := b.port.Read(buf)
		if err != nil || n == 0 {
			break
		}
	}
	b.port.SetReadTimeout(100 * time.Millisecond)
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
// magic byte, tolerating stray boot/noise bytes.
func (b *Bridge) req(cmd byte, payload []byte) (byte, []byte, error) {
	return b.reqUntil(cmd, payload, time.Now().Add(b.timeout+time.Second))
}

func (b *Bridge) reqUntil(cmd byte, payload []byte, deadline time.Time) (byte, []byte, error) {
	n := len(payload)
	hdr := []byte{reqMagic, cmd, byte(n >> 8), byte(n)}
	if _, err := b.port.Write(append(hdr, payload...)); err != nil {
		return 0, nil, err
	}
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

func responsePayloadLength(n int) error {
	if n > maxReqPayload {
		return fmt.Errorf("serial response length %d exceeds %d", n, maxReqPayload)
	}
	return nil
}

// --- primitives ---

// Ping returns the firmware banner.
func (b *Bridge) Ping() (string, error) {
	_, d, err := b.req(cmdPing, nil)
	return string(d), err
}

// Reset issues a manual CC1101 power-on reset.
func (b *Bridge) Reset() error { _, _, err := b.req(cmdReset, nil); return err }

// Xfer does a full-duplex SPI transfer, returning the MISO bytes.
func (b *Bridge) Xfer(data []byte) ([]byte, error) {
	_, d, err := b.req(cmdXfer, data)
	return d, err
}

// GDO reads the GDO0/GDO2 pin state.
func (b *Bridge) GDO() (gdo0, gdo2 bool, err error) {
	_, d, err := b.req(cmdGDO, nil)
	if err != nil {
		return false, false, err
	}
	return d[0]&1 != 0, d[0]&2 != 0, nil
}
