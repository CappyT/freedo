package tado

import (
	"bytes"
	"errors"
	"fmt"
	"time"
)

// Firmware upload plumbing of the downlink queue. The upload itself (block
// order, retries, abort) is SendOTA's; the queue carries each POST d/fw as one
// fragmented CON exchange and files the device's answer for the caller.

const (
	fwKind          = "fw"
	fwStateResource = "d/fw/state"
)

// FwReply is the terminal outcome of one queued d/fw exchange. Code is empty
// when no answer arrived: Result and Reason then say why the queue gave up.
type FwReply struct {
	MID    uint16
	Code   string // "2.31", "4.08", ... or "RST"
	Block1 int    // echoed Block1 number, -1 when absent
	Result string // delivered, dropped, withdrawn
	Reason string
}

// FwStateReport is one fw/state document a device sent. Seq orders every
// report of every device, so a caller can wait for one newer than it saw.
type FwStateReport struct {
	Body []byte
	At   time.Time
	Seq  uint64
}

// LockFirmware reserves target's downlink queue for a firmware upload: until
// UnlockFirmware only d/fw exchanges go out, the other commands stay queued.
func (e *EmuState) LockFirmware(target string) error {
	wire, _, err := e.resolve(target)
	if err != nil {
		return err
	}
	key := hexEUI(wire)
	if e.fwLock[key] {
		return errors.New("tado: a firmware upload already owns " + target)
	}
	e.fwLock[key] = true
	return nil
}

// UnlockFirmware releases target's queue. It reports whether commands wait.
func (e *EmuState) UnlockFirmware(target string) bool {
	wire, _, err := e.resolve(target)
	if err != nil {
		return false
	}
	key := hexEUI(wire)
	delete(e.fwLock, key)
	return len(e.queue[key]) > 0
}

// FirmwareLocked reports whether a firmware upload owns target's queue.
func (e *EmuState) FirmwareLocked(target string) bool {
	wire, _, err := e.resolve(target)
	return err == nil && e.fwLock[hexEUI(wire)]
}

// fwAtHead puts the first d/fw exchange at the head of a locked queue. An
// attempt already on the air stays first: it finishes as it started.
func (e *EmuState) fwAtHead(key string) bool {
	q := e.queue[key]
	if q[0].kind == fwKind || q[0].inFlight {
		return true
	}
	for i, d := range q {
		if d.kind == fwKind {
			copy(q[1:i+1], q[:i])
			q[0] = d
			return true
		}
	}
	return false
}

// EnqueueFwBlock queues one POST d/fw block ahead of every command not yet on
// the air and returns its MID.
func (e *EmuState) EnqueueFwBlock(target string, num int, more bool, data []byte) (uint16, error) {
	return e.enqueueFw(target, fmt.Sprintf("fw block %d", num), func(mid uint16, token, dst []byte) ([]byte, error) {
		return BuildFwBlockCoAP(num, more, data, mid, token, e.selfEUI, dst)
	})
}

// EnqueueFwAbort queues DELETE d/fw and returns its MID.
func (e *EmuState) EnqueueFwAbort(target string) (uint16, error) {
	return e.enqueueFw(target, "fw abort", func(mid uint16, token, dst []byte) ([]byte, error) {
		return BuildFwAbortCoAP(mid, token, e.selfEUI, dst)
	})
}

func (e *EmuState) enqueueFw(target, desc string, build func(mid uint16, token, dst []byte) ([]byte, error)) (uint16, error) {
	wire, _, err := e.resolve(target)
	if err != nil {
		return 0, err
	}
	mid, token, err := downlinkIDs()
	if err != nil {
		return 0, err
	}
	pkt, err := build(mid, token, wire)
	if err != nil {
		return 0, err
	}
	if err := e.enqueue(target, fwKind, desc, pkt, e.key, mid); err != nil {
		return 0, err
	}
	key := hexEUI(wire)
	q := e.queue[key]
	d := q[len(q)-1]
	d.requireCoAP = true
	d.responseToken = append([]byte(nil), token...)
	at := 0
	if q[0].inFlight && q[0] != d {
		at = 1
	}
	copy(q[at+1:], q[at:len(q)-1])
	q[at] = d
	delete(e.fwReply, key)
	return mid, nil
}

// confirmFw closes the in-flight d/fw exchange on any answer carrying its
// token, or on a reset: the upload decides what the code means.
func (e *EmuState) confirmFw(key string, d *queuedDownlink, m *CoAPMessage) {
	code := m.Code
	switch {
	case m.Type == "ACK" && isResponseCode(m.Code) && bytes.Equal(m.Token, d.responseToken):
	case m.Type == "RST":
		code = "RST"
	default:
		return
	}
	e.fwReply[key] = FwReply{MID: d.mid, Code: code, Block1: block1Echo(m), Result: "delivered", Reason: "coap-" + code}
	e.queue[key] = e.queue[key][1:]
	e.stats.DownlinksDelivered++
	e.recordDownlinkOutcome(key, d, "delivered", "coap-"+code)
}

// FwReply returns the outcome of target's d/fw exchange mid, once it has one.
func (e *EmuState) FwReply(target string, mid uint16) (FwReply, bool) {
	wire, _, err := e.resolve(target)
	if err != nil {
		return FwReply{}, false
	}
	r, ok := e.fwReply[hexEUI(wire)]
	if !ok || r.MID != mid {
		return FwReply{}, false
	}
	return r, true
}

// WithdrawFw drops target's d/fw exchange mid if it is still queued.
func (e *EmuState) WithdrawFw(target string, mid uint16) bool {
	wire, _, err := e.resolve(target)
	if err != nil {
		return false
	}
	key := hexEUI(wire)
	for i, d := range e.queue[key] {
		if d.kind == fwKind && d.mid == mid {
			e.queue[key] = append(e.queue[key][:i:i], e.queue[key][i+1:]...)
			e.recordDownlinkOutcome(key, d, "withdrawn", "fw-timeout")
			return true
		}
	}
	return false
}

// observeFwState files the fw/state a device PUTs on its own, at boot and on
// every slot change. The bridge answers it with the default 2.04; the first
// 2.xx is what makes a freshly booted image confirmed.
func (e *EmuState) observeFwState(m *CoAPMessage, src []byte) {
	if (m.Type != "CON" && m.Type != "NON") || (m.Code != "0.02" && m.Code != "0.03") {
		return
	}
	if normalizeResource(m.Path) != fwStateResource {
		return
	}
	if _, err := ParseFwState(m.Payload); err != nil {
		return
	}
	e.noteFwState(e.fwPut, hexEUI(src), m.Payload)
}

func (e *EmuState) noteFwState(store map[string]FwStateReport, key string, body []byte) {
	e.fwSeq++
	store[key] = FwStateReport{Body: append([]byte(nil), body...), At: e.cfg.Now(), Seq: e.fwSeq}
}

// FwStateReports returns the last fw/state target PUT and the last one it
// answered to a GET of d/fw/state.
func (e *EmuState) FwStateReports(target string) (put, get FwStateReport) {
	wire, _, err := e.resolve(target)
	if err != nil {
		return FwStateReport{}, FwStateReport{}
	}
	key := hexEUI(wire)
	return e.fwPut[key], e.fwGet[key]
}

// FreshFwState decodes the newest fw/state target reported and refuses one
// older than maxAge: an upload plans against what the device runs now.
func (e *EmuState) FreshFwState(target string, maxAge time.Duration) (*FwState, error) {
	put, get := e.FwStateReports(target)
	latest := get
	if put.Seq > get.Seq {
		latest = put
	}
	if latest.Seq == 0 {
		return nil, errors.New("tado: no fw/state from " + target + " yet: read GET /api/nodes/{id}/fw first")
	}
	if age := e.cfg.Now().Sub(latest.At); age > maxAge {
		return nil, fmt.Errorf("tado: the fw/state of %s is %s old, over %s: read it again", target, age.Round(time.Second), maxAge)
	}
	return ParseFwState(latest.Body)
}
