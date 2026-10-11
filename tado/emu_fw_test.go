package tado

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// runningFwState is a synthetic d/fw/state: type 8, running version v with the
// given flags.
func runningFwState(v FwVersion, flags FwSlotFlags) []byte {
	var b []byte
	b = append(b, TLV{Tag: "01a0", Val: []byte{OTADeviceType}}.Encode()...)
	b = append(b, TLV{Tag: "003a", Val: []byte{byte(v >> 8), byte(v)}}.Encode()...)
	return append(b, TLV{Tag: "003b", Val: []byte{byte(flags)}}.Encode()...)
}

// sendFw takes the next attempt for bedroom, reassembles it and returns the
// request.
func sendFw(t *testing.T, e *EmuState) *CoAPMessage {
	t.Helper()
	wire, label, err := e.resolve("bedroom")
	if err != nil {
		t.Fatal(err)
	}
	actions, ok := e.nextDownlink(wire, label)
	if !ok {
		t.Fatal("no attempt")
	}
	reasm := NewReassembler()
	for i, a := range actions {
		data, err := ParseData(NetworkKey, a.Bytes)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if m, done := reasm.Add(data); done {
			return m
		}
		if !IsFragment(data.Plaintext) {
			app := ParseApp(data.Plaintext)
			if app == nil || app.CoAP == nil {
				t.Fatalf("frame %d carries no CoAP", i)
			}
			return app.CoAP
		}
	}
	t.Fatal("attempt does not reassemble")
	return nil
}

func answerFw(t *testing.T, e *EmuState, req *CoAPMessage, code string, opts ...CoAPOption) {
	t.Helper()
	bedroom := mustHex(NodeByAny("bedroom").WireLE)
	ack, err := BuildCoAPAck(code, req.MID, req.Token, nil, bedroom, BridgeEUI, coapPort, bridgeCoAPPort, opts...)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := BuildData(NetworkKey, 0x42, BridgeEUI, bedroom[:5], WrapAppPayload(ack, 900, nil))
	if err != nil {
		t.Fatal(err)
	}
	e.OnFrame(frame.Bytes())
}

func TestFwBlockIsFragmentedAndFiledByAnswer(t *testing.T) {
	e, _, _ := newTestEmu(t)
	data := bytes.Repeat([]byte{0x5a}, OTABlockSize)
	mid, err := e.EnqueueFwBlock("bedroom", 3, true, data)
	if err != nil {
		t.Fatal(err)
	}
	wire, label, _ := e.resolve("bedroom")
	actions, ok := e.nextDownlink(wire, label)
	if !ok || len(actions) < 2 || !actions[0].HasCSLBatch || !actions[0].CoAPTransaction {
		t.Fatalf("fw block is not one fragmented CoAP exchange: %+v", actions)
	}
	e.RevertDownlinkAttempt(wire, actions[len(actions)-1].Bytes[2])
	req := sendFw(t, e)
	if req.MID != mid || req.Code != "0.02" || req.Path != "d/fw" || !bytes.Equal(req.Payload, data) {
		t.Fatalf("request %s", req.Summary())
	}
	if _, ok := e.FwReply("bedroom", mid); ok {
		t.Fatal("reply before the answer")
	}
	answerFw(t, e, req, "2.31", CoAPOption{Num: 27, Val: []byte{0x3b}})
	r, ok := e.FwReply("bedroom", mid)
	if !ok || r.Code != "2.31" || r.Block1 != 3 || e.Pending("bedroom") != 0 {
		t.Fatalf("reply %+v ok %t pending %d", r, ok, e.Pending("bedroom"))
	}
}

func TestFwErrorCodeClosesTheExchange(t *testing.T) {
	e, _, _ := newTestEmu(t)
	mid, err := e.EnqueueFwBlock("bedroom", 0, true, bytes.Repeat([]byte{1}, OTABlockSize))
	if err != nil {
		t.Fatal(err)
	}
	req := sendFw(t, e)
	answerFw(t, e, req, "4.08")
	if r, ok := e.FwReply("bedroom", mid); !ok || r.Code != "4.08" || r.Block1 != -1 || e.Pending("bedroom") != 0 {
		t.Fatalf("reply %+v ok %t pending %d", r, ok, e.Pending("bedroom"))
	}
	abort, err := e.EnqueueFwAbort("bedroom")
	if err != nil {
		t.Fatal(err)
	}
	req = sendFw(t, e)
	if req.Code != "0.04" || req.Path != "d/fw" {
		t.Fatalf("abort %s", req.Summary())
	}
	answerFw(t, e, req, "2.02")
	if r, ok := e.FwReply("bedroom", abort); !ok || r.Code != "2.02" {
		t.Fatalf("abort reply %+v", r)
	}
}

func TestFwLockHoldsOtherCommands(t *testing.T) {
	e, _, _ := newTestEmu(t)
	if err := e.LockFirmware("bedroom"); err != nil {
		t.Fatal(err)
	}
	if err := e.LockFirmware("bedroom"); err == nil {
		t.Fatal("second lock accepted")
	}
	if err := e.EnqueueIdentify("bedroom", 10); err != nil {
		t.Fatal(err)
	}
	wire, label, _ := e.resolve("bedroom")
	if _, ok := e.nextDownlink(wire, label); ok {
		t.Fatal("a locked queue sent identify")
	}
	mid, err := e.EnqueueFwBlock("bedroom", 0, true, bytes.Repeat([]byte{1}, OTABlockSize))
	if err != nil {
		t.Fatal(err)
	}
	req := sendFw(t, e)
	if req.MID != mid {
		t.Fatalf("head is %s, want the fw block", req.Summary())
	}
	answerFw(t, e, req, "2.31")
	if _, ok := e.nextDownlink(wire, label); ok {
		t.Fatal("identify went out between two blocks")
	}
	if !e.UnlockFirmware("bedroom") {
		t.Fatal("unlock reports no held command")
	}
	if req := sendFw(t, e); req.Path != "d/identify" {
		t.Fatalf("after unlock: %s", req.Summary())
	}
}

func TestFwWithdrawAndRetryLimitFileNoAnswer(t *testing.T) {
	e, _, _ := newTestEmu(t)
	mid, err := e.EnqueueFwBlock("bedroom", 0, true, bytes.Repeat([]byte{1}, OTABlockSize))
	if err != nil {
		t.Fatal(err)
	}
	if !e.WithdrawFw("bedroom", mid) || e.Pending("bedroom") != 0 {
		t.Fatal("withdraw")
	}
	if r, ok := e.FwReply("bedroom", mid); !ok || r.Code != "" || r.Result != "withdrawn" {
		t.Fatalf("reply %+v", r)
	}
	mid, err = e.EnqueueFwBlock("bedroom", 0, true, bytes.Repeat([]byte{1}, OTABlockSize))
	if err != nil {
		t.Fatal(err)
	}
	wire, label, _ := e.resolve("bedroom")
	for range maxDownlinkTries + 1 {
		e.nextDownlink(wire, label)
	}
	if r, ok := e.FwReply("bedroom", mid); !ok || r.Code != "" || r.Reason != "retry-limit" {
		t.Fatalf("reply %+v ok %t", r, ok)
	}
}

func TestFwStatePutIsAnsweredAndFiled(t *testing.T) {
	now := time.Unix(1000, 0)
	e := NewEmu(EmuConfig{TxCounter: 100000, Now: func() time.Time { return now }}, nil)
	if _, err := e.FreshFwState("bedroom", time.Minute); err == nil || !strings.Contains(err.Error(), "no fw/state") {
		t.Fatalf("fresh with nothing: %v", err)
	}
	v, _ := NewFwVersion(216, 0)
	bedroom := mustHex(NodeByAny("bedroom").WireLE)
	raw := uplinkFrame(t, bedroom, "0.03", []string{"d", "VA1000000005", "fw", "state"}, nil, runningFwState(v, FwSlotHasImage|FwSlotActive))
	actions := e.OnFrame(raw)
	var answered bool
	for _, a := range actions {
		if a.Kind != ActData {
			continue
		}
		data, err := ParseData(NetworkKey, a.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if m := ParseApp(data.Plaintext).CoAP; m != nil && m.Type == "ACK" && m.Code == "2.04" && m.MID == 0x4321 {
			answered = true
		}
	}
	if !answered {
		t.Fatalf("fw/state PUT not answered 2.04: %+v", actions)
	}
	put, get := e.FwStateReports("bedroom")
	if put.Seq == 0 || get.Seq != 0 {
		t.Fatalf("put %+v get %+v", put, get)
	}
	state, err := e.FreshFwState("bedroom", time.Minute)
	if err != nil || state.Running.Version != v || state.Confirmed() {
		t.Fatalf("fresh %+v %v", state, err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := e.FreshFwState("bedroom", time.Minute); err == nil || !strings.Contains(err.Error(), "old") {
		t.Fatalf("stale accepted: %v", err)
	}
}
