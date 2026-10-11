package main

import (
	"encoding/hex"
	"flag"
	"io"
	"testing"
	"time"

	"freedo/cc1101"
	"freedo/internal/radio"
	"freedo/tado"
)

type fakePassiveRadio struct {
	frames [][]byte
	calls  int
}

func (r *fakePassiveRadio) Stream(_ time.Duration, cb func(*cc1101.RxFrame)) error {
	r.calls++
	for _, frame := range r.frames {
		cb(&cc1101.RxFrame{Payload: frame, CRCOK: true})
	}
	r.frames = nil
	return nil
}

type fakeCSLRadio struct {
	actions     [][]tado.Action
	result      radio.ImmediateResult
	err         error
	accept      bool
	completions chan radio.CSLCompletion
	onSubmit    func(tado.Action) *radio.CSLCompletion
}

func (r *fakeCSLRadio) SendImmediate(actions []tado.Action, _ bool) (radio.ImmediateResult, error) {
	r.actions = append(r.actions, append([]tado.Action(nil), actions...))
	if len(actions) == 1 && r.onSubmit != nil {
		if completion := r.onSubmit(actions[0]); completion != nil {
			r.completions <- *completion
		}
	}
	result := r.result
	if r.accept {
		result.Accepted = append([]tado.Action(nil), actions...)
	}
	return result, r.err
}

func coldStartFixture(t *testing.T) (*coldStartRun, *tado.EmuState, *fakeCSLRadio) {
	t.Helper()
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	tx := &fakeCSLRadio{completions: make(chan radio.CSLCompletion, 1)}
	tx.result = radio.ImmediateResult{CSLQueue: true}
	r := &coldStartRun{
		emu: emu, passive: &fakePassiveRadio{}, tx: tx, completions: tx.completions,
		in: fixtureInstall, doTx: true, target: "bath", listen: 20 * time.Millisecond,
	}
	return r, emu, tx
}

func queueOne(t *testing.T, emu *tado.EmuState) {
	t.Helper()
	if err := emu.EnqueueSetpoint("bath", tado.SetpointParams{
		ID: 3, Setpoint: 5.0, Sched: 5.0, Overlay: 2,
	}); err != nil {
		t.Fatal(err)
	}
}

func acceptedResult(action tado.Action) radio.ImmediateResult {
	return radio.ImmediateResult{Accepted: []tado.Action{action}, CSLQueue: true}
}

func matchingAck(t *testing.T, action tado.Action) *radio.CSLCompletion {
	t.Helper()
	data, err := tado.ParseData(tado.NetworkKey, action.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	bath := tado.NodeByAny("bath")
	wire := mustHexBytes(t, bath.WireLE)
	ack := tado.BuildEnhAck(data.Seq, tado.BridgeEUI, wire, 0)
	return &radio.CSLCompletion{
		Action: action,
		Result: &cc1101.CSLTxResult{Sent: 40, Ack: &cc1101.RxFrame{Payload: ack, CRCOK: true}},
		Frames: [][]byte{ack},
	}
}

func matchingAckAndCoAP204(t *testing.T, action tado.Action) *radio.CSLCompletion {
	t.Helper()
	completion := matchingAck(t, action)
	data, err := tado.ParseData(tado.NetworkKey, action.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	app := tado.ParseApp(data.Plaintext)
	if app == nil || app.CoAP == nil {
		t.Fatal("queued setpoint has no CoAP request")
	}
	bath := tado.NodeByAny("bath")
	wire := mustHexBytes(t, bath.WireLE)
	response, err := tado.BuildCoAPAck("2.04", app.CoAP.MID, app.CoAP.Token, nil, wire, tado.BridgeEUI, 5683, 4005)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.BuildData(tado.NetworkKey, 0x61, tado.BridgeEUI, wire[:5], tado.WrapAppPayload(response, 100001, nil))
	if err != nil {
		t.Fatal(err)
	}
	completion.Frames = append(completion.Frames, frame.Bytes())
	return completion
}

func TestColdStartAttemptDeliveredByCSLCompletion(t *testing.T) {
	r, emu, tx := coldStartFixture(t)
	queueOne(t, emu)
	tx.onSubmit = func(action tado.Action) *radio.CSLCompletion {
		tx.result = acceptedResult(action)
		return matchingAckAndCoAP204(t, action)
	}

	delivered, err := r.attempt(1)
	if err != nil {
		t.Fatal(err)
	}
	if !delivered {
		t.Fatal("matching scheduler ACK was not counted as a delivery")
	}
	if len(tx.actions) != 1 || len(tx.actions[0]) != 1 {
		t.Fatalf("CSL submissions=%d, want one Data job", len(tx.actions))
	}
	action := tx.actions[0][0]
	if !action.CSLTx || !action.QueueAttempt {
		t.Fatalf("action=%+v, want queued CSL Data", action)
	}
	data, err := tado.ParseData(tado.NetworkKey, action.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	app := tado.ParseApp(data.Plaintext)
	if app == nil || app.CoAP == nil || app.CoAP.URI() != "z/s?id=3" {
		t.Fatalf("H14 job does not contain the requested setpoint: %+v", app)
	}
	if emu.Pending("bath") != 0 {
		t.Fatalf("%d queued downlinks remain after delivery", emu.Pending("bath"))
	}
}

func TestColdStartDoesNotCreditUncorrelatedACK(t *testing.T) {
	r, emu, tx := coldStartFixture(t)
	queueOne(t, emu)
	tx.onSubmit = func(action tado.Action) *radio.CSLCompletion {
		tx.result = acceptedResult(action)
		completion := matchingAck(t, action)
		completion.Frames[0][2]++
		return completion
	}

	delivered, err := r.attempt(1)
	if err != nil {
		t.Fatal(err)
	}
	if delivered {
		t.Fatal("unrelated ACK counted as cold-start delivery")
	}
	if emu.Pending("bath") != 0 {
		t.Fatalf("%d queued downlinks remain after an unconfirmed attempt", emu.Pending("bath"))
	}
}

func TestColdStartDryRunPlansNoSchedulerJob(t *testing.T) {
	r, emu, tx := coldStartFixture(t)
	r.doTx = false
	queueOne(t, emu)

	delivered, err := r.attempt(1)
	if err != nil {
		t.Fatal(err)
	}
	if delivered || len(tx.actions) != 0 {
		t.Fatalf("dry-run delivered=%t submissions=%d", delivered, len(tx.actions))
	}
}

func TestColdStartPreflightLatchesBridge(t *testing.T) {
	frames := loadFrames(t, "../../tado/testdata/frames_69ec.txt", 400)
	r, emu, _ := coldStartFixture(t)
	r.passive = &fakePassiveRadio{frames: frames}
	if err := r.preflight(time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !emu.BridgeSeen() {
		t.Skip("no bridge-sourced frame in corpus slice")
	}
}

func TestColdStartRefusesBridgeSeenAfterPreflight(t *testing.T) {
	frames := loadFrames(t, "../../tado/testdata/frames_69ec.txt", 400)
	r, emu, tx := coldStartFixture(t)
	queueOne(t, emu)
	updates := make(chan []byte, len(frames))
	for _, frame := range frames {
		updates <- frame
	}
	r.frames = updates
	if _, err := r.attempt(1); err == nil {
		t.Fatal("submitted an explicit CSL transaction after bridge detection")
	}
	if !emu.BridgeSeen() {
		t.Skip("no bridge-sourced frame in corpus slice")
	}
	if len(tx.actions) != 0 {
		t.Fatalf("submitted %d CSL jobs after bridge detection", len(tx.actions))
	}
}

func TestColdStartFailsWhenSchedulerStops(t *testing.T) {
	r, emu, tx := coldStartFixture(t)
	queueOne(t, emu)
	close(tx.completions)
	tx.accept = true
	tx.onSubmit = func(action tado.Action) *radio.CSLCompletion {
		tx.result = acceptedResult(action)
		return nil
	}
	if _, err := r.attempt(1); err == nil {
		t.Fatal("closed scheduler completion channel was accepted")
	}
}

func TestPortFlagAcceptsLongAlias(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	portFlag(fs)
	if err := fs.Parse([]string{"--port", "/dev/test-radio"}); err != nil {
		t.Fatal(err)
	}
	if got := fs.Lookup("p").Value.String(); got != "/dev/test-radio" {
		t.Fatalf("-p=%q, want long alias value", got)
	}
}

func TestColdStartPreflightMinimum(t *testing.T) {
	if err := validateColdStartPreflight(coldStartPreflightMin - time.Millisecond); err == nil {
		t.Fatal("accepted a preflight shorter than 75s")
	}
	if err := validateColdStartPreflight(coldStartPreflightMin); err != nil {
		t.Fatal(err)
	}
}

func mustHexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
