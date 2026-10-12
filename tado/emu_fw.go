package tado

import (
	"bytes"
	"encoding/hex"
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
	// fwReadSpacing is the shortest time between two automatic d/fw/state reads
	// after start.
	fwReadSpacing = 30 * time.Second
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
	e.fwChanged = true
}

// TakeFwStateChanged reports whether a device reported fw/state since the last
// take, so the owner persists the newest body.
func (e *EmuState) TakeFwStateChanged() bool {
	changed := e.fwChanged
	e.fwChanged = false
	return changed
}

// ReadFirmwareState queues a GET d/fw/state for the first declared valve or wall
// thermostat that holds no report yet, and returns its wire EUI hex, "" when it
// queues nothing. The device serves d/fw/state on GET, so the daemon reads it
// once after start for the devices that never PUT one, one read per device per
// run. Two reads stay fwReadSpacing apart, and an unarmed radio queues nothing.
// A failed or unanswered read is marked done and never retried.
func (e *EmuState) ReadFirmwareState(now time.Time) string {
	if !e.armed() || e.cfg.NoAutoQueue || e.inst == nil || e.fwReadAt.Add(fwReadSpacing).After(now) {
		return ""
	}
	for i := range e.inst.Devices {
		dev := &e.inst.Devices[i]
		if dev.Kind != KindValve && dev.Kind != KindThermostat {
			continue
		}
		wire := dev.WireEUI()
		if len(wire) != 8 {
			continue
		}
		key := hexEUI(wire)
		if e.fwReads[key] {
			continue
		}
		e.fwReads[key] = true
		if len(e.fwPut[key].Body) > 0 || len(e.fwGet[key].Body) > 0 || e.fwLock[key] {
			continue
		}
		e.fwReadAt = now
		if err := e.EnqueueGet(key, fwStateResource, []string{"d", "fw", "state"}, nil); err != nil {
			e.log.Log("Fw", dev.Ref(), "", rsnBuildFail+" fw/state read "+err.Error(), "")
			return ""
		}
		return key
	}
	return ""
}

// restoreFwState replaces the fw/state reports from a snapshot, newest body per
// device, and refills the node fw_* telemetry from it. The stored time keeps the
// report's age, so FreshFwState still refuses a stale one after a restart. The
// reports are a cache that the devices rebuild, so a bad entry is dropped
// instead of failing the restore of the whole desired state.
func (e *EmuState) restoreFwState(in map[string]DesiredFwState) {
	e.fwPut = make(map[string]FwStateReport, len(in))
	e.fwGet = map[string]FwStateReport{}
	e.fwSeq = 0
	for key, rep := range in {
		wire, err := hex.DecodeString(key)
		if err != nil || len(wire) != 8 || rep.At.IsZero() {
			continue
		}
		if _, err := ParseFwState(rep.Body); err != nil {
			continue
		}
		// A removal drops the report, but a stored state that outlives one would
		// otherwise bring its node back with no last seen, so restore only the
		// EUIs the installation still declares.
		if !declaredEUI(e.inst, key) {
			continue
		}
		e.fwSeq++
		e.fwPut[key] = FwStateReport{Body: append([]byte(nil), rep.Body...), At: rep.At, Seq: e.fwSeq}
		e.refillFwTelemetry(e.node(wire), rep.Body)
	}
}

// declaredEUI reports whether the installation declares the wire EUI, in the hex
// spelling of the fw/state keys.
func declaredEUI(in *Install, key string) bool {
	if in == nil {
		return false
	}
	for i := range in.Devices {
		if hexEUI(in.Devices[i].WireEUI()) == key {
			return true
		}
	}
	return false
}

// refillFwTelemetry writes the fw_* labels of a d/fw/state body into a node's
// telemetry, the way cacheTLVs reads them from the report on the air.
func (e *EmuState) refillFwTelemetry(ns *NodeState, body []byte) {
	for _, t := range ParseTLVs(body) {
		if info, ok := tagFor(t.Tag, ResDefault); ok {
			ns.Telemetry[info.name] = info.decode(t.Val)
		}
	}
}

// exportFwState returns the newest fw/state body of each device, nil when none
// reported.
func (e *EmuState) exportFwState() map[string]DesiredFwState {
	if len(e.fwPut) == 0 && len(e.fwGet) == 0 {
		return nil
	}
	out := make(map[string]DesiredFwState, len(e.fwPut)+len(e.fwGet))
	for key, put := range e.fwPut {
		if get, ok := e.fwGet[key]; ok && get.Seq > put.Seq {
			put = get
		}
		out[key] = DesiredFwState{Body: append([]byte(nil), put.Body...), At: put.At}
	}
	for key, get := range e.fwGet {
		if _, ok := e.fwPut[key]; ok {
			continue
		}
		out[key] = DesiredFwState{Body: append([]byte(nil), get.Body...), At: get.At}
	}
	return out
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
