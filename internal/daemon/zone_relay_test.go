package daemon

import (
	"errors"
	"io"
	"testing"
	"time"

	"freedo/cc1101"
	"freedo/internal/radio"
	"freedo/tado"
)

// zoneRelayFrame builds an uplink from sender whose IPv6 destination is target,
// so the coordinator relays it.
func zoneRelayFrame(t *testing.T, sender, target *tado.Device, req tado.CoAPReq, seq byte, ctr uint32) []byte {
	t.Helper()
	req.SrcEUI, req.DstEUI = sender.WireEUI(), target.WireEUI()
	msg, err := tado.BuildContextCoAP(req)
	if err != nil {
		t.Fatal(err)
	}
	wire := target.WireEUI()
	iid := make([]byte, len(wire))
	for i := range wire {
		iid[i] = wire[len(wire)-1-i]
	}
	iid[0] ^= 0x02
	pkt := append([]byte{0x7e, 0xf5, 0x00}, iid...)
	pkt = append(pkt, msg[3:]...)
	frame, err := tado.BuildData(defaultInstall.NetworkKey(), seq, defaultInstall.CoordinatorEUI(), sender.WireEUI()[:5], tado.WrapAppPayload(pkt, ctr, nil))
	if err != nil {
		t.Fatal(err)
	}
	return frame.Bytes()
}

func zonePush(mid uint16, zone string) tado.CoAPReq {
	return tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: mid, Token: []byte{byte(mid)},
		Path: []string{"z", "p"}, Query: []string{"lid=" + zone},
		Payload: tado.TLV{Tag: "40a0", Val: []byte{56}}.Encode(),
	}
}

func newZoneRelayDaemon(t *testing.T, now time.Time) (*daemon, *tado.EmuState, *immediateTestRadio, *manualRetryClock, *tado.Device, *tado.Device) {
	t.Helper()
	cfg := defaultInstall.EmuConfig()
	cfg.Now = func() time.Time { return now }
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	sender, err := defaultInstall.Resolve("0a01")
	if err != nil {
		t.Fatal(err)
	}
	target, err := defaultInstall.Resolve(dhwNode)
	if err != nil {
		t.Fatal(err)
	}
	radio := newImmediateTestRadio(false, nil)
	radio.cslQueue, radio.accept = true, true
	d := newDaemon(defaultInstall, emu, radio, false, "live")
	clock := newManualRetryClock()
	d.retryAfter, d.retryNow = clock.after, func() time.Time { return now }
	t.Cleanup(func() { close(d.done) })
	return d, emu, radio, clock, sender, target
}

// TestZoneRelayPiggybackHoldsAcceptedAttempt: an uplink that races a relay the
// ESP holds must not rebuild it under a new MAC seq, and a corrupted Enhanced
// ACK retries on the relay backoff, not after 30 s.
func TestZoneRelayPiggybackHoldsAcceptedAttempt(t *testing.T) {
	now := time.Date(2026, 9, 23, 14, 21, 29, 0, time.UTC)
	d, emu, immediate, clock, sender, target := newZoneRelayDaemon(t, now)
	emu.OnFrame(zoneRelayFrame(t, sender, target, zonePush(0x2201, "5"), 0x30, 800))
	if n := emu.Pending(dhwNode); n != 1 {
		t.Fatalf("pending=%d, want 1", n)
	}
	report := tado.CoAPReq{Type: "NON", Code: "0.03", MID: 0x7001, Path: []string{"d", "x", "sen"}}
	d.handleFrame(deviceRequestUplink(t, target, report, 0x40), true)
	report.MID++
	d.handleFrame(deviceRequestUplink(t, target, report, 0x41), true)
	attempts := immediate.queuedAttempts()
	if len(attempts) != 1 || !attempts[0].ZoneRelay {
		t.Fatalf("attempts=%+v, want one zone relay attempt", attempts)
	}
	clock.noTimer(t)

	d.handleCSLCompletion(radio.CSLCompletion{Action: attempts[0], Result: &cc1101.CSLTxResult{Sent: 23}, Err: errors.New("CSLTX received invalid Enhanced ACK")})
	if delay := clock.wait(t); delay != zoneRelayRetryBackoff {
		t.Fatalf("retry delay=%s, want %s", delay, zoneRelayRetryBackoff)
	}
}

// TestZoneRelayDeliveryChainsNextDispatch: the Enhanced ACK that closes one
// relay sends the next one for the same target at once.
func TestZoneRelayDeliveryChainsNextDispatch(t *testing.T) {
	now := time.Date(2026, 9, 23, 14, 39, 29, 0, time.UTC)
	d, emu, _, _, sender, target := newZoneRelayDaemon(t, now)
	emu.OnFrame(zoneRelayFrame(t, sender, target, zonePush(0x2201, "5"), 0x30, 800))
	emu.OnFrame(zoneRelayFrame(t, sender, target, zonePush(0x2202, "8"), 0x31, 801))
	first, ok, err := emu.NextQueuedDownlink(dhwNode)
	if err != nil || !ok || len(first) != 1 {
		t.Fatalf("first=%+v ok=%t err=%v", first, ok, err)
	}
	ack := tado.BuildEnhAck(first[0].Bytes[2], defaultInstall.CoordinatorEUI(), target.WireEUI(), 0)
	completion := radio.CSLCompletion{
		Action: first[0],
		Result: &cc1101.CSLTxResult{Sent: 23, Ack: &cc1101.RxFrame{Payload: ack, CRCOK: true}},
		Frames: [][]byte{ack},
	}
	d.handleCSLCompletion(completion)
	for _, frame := range completion.Frames {
		d.handleFrame(frame, false)
	}
	d.chainQueuedDispatch(completion)
	if n := emu.Pending(dhwNode); n != 1 {
		t.Fatalf("pending=%d, want the second relay", n)
	}
	if !d.dispatchDue[dhwNode] {
		t.Fatalf("dispatch due=%v, want the next relay dispatched now", d.dispatchDue)
	}
}
