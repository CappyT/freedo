package tado

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// joiner is a device outside the installation that talks the pairing key.
var joinerEUI = mustHex("0bb1cefa07c51b00")

// pairingRS is a Router Solicitation from joinerEUI under PairingKey, as a
// joining valve sends it (the pairing capture).
func pairingRS(t *testing.T) []byte {
	t.Helper()
	rs := []byte{iphcMcast0, iphcMcast1, nhICMPv6, 0x02, icmpRouterSolicit, 0, 0, 0}
	b, err := BuildBroadcast(PairingKey, 0x13, joinerEUI, 25, bcastSecCtrl, WrapBroadcastPayload(rs, 0))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pairEmu(t *testing.T, now *time.Time) *EmuState {
	t.Helper()
	return NewEmu(EmuConfig{TxCounter: 100000, CSLPhase: 0x1234, Now: func() time.Time { return *now }}, nil)
}

// TestPairWindowLearnsJoiningDevice: while the pair window is open, a frame that
// fails the network key and verifies under PairingKey names its sender. Outside
// the window it stays a MIC failure.
func TestPairWindowLearnsJoiningDevice(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	e := pairEmu(t, &now)

	if actions := e.OnFrame(pairingRS(t)); actions != nil {
		t.Fatalf("closed window: actions %v", reasons(actions))
	}
	if st := e.Stats(); st.MICFails != 1 || st.PairingKeyFrames != 0 {
		t.Fatalf("closed window stats %+v, want one MIC failure", st)
	}
	if _, err := e.PairAutoTarget(); !errors.Is(err, ErrPairTarget) {
		t.Fatalf("auto target with a closed window: %v", err)
	}

	e.OpenPairWindow(2 * time.Minute)
	if actions := e.OnFrame(pairingRS(t)); actions != nil {
		t.Fatalf("open window: actions %v, want none", reasons(actions))
	}
	data, err := BuildData(PairingKey, 0x21, BridgeEUI, joinerEUI[:5], WrapAppPayload([]byte{0x7a, 0x33, nhICMPv6, 0x80, 0, 0, 0}, 5, nil))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if actions := e.OnFrame(data.Bytes()); actions != nil {
		t.Fatalf("pairing-key Data: actions %v, want none", reasons(actions))
	}
	st := e.Stats()
	if st.MICFails != 1 || st.PairingKeyFrames != 2 {
		t.Errorf("open window stats %+v, want 2 pairing-key frames and no new MIC failure", st)
	}
	v := e.PairWindow()
	if !v.Open || len(v.Joiners) != 1 {
		t.Fatalf("window %+v, want open with one joiner", v)
	}
	j := v.Joiners[0]
	if j.EUI != hexEUI(joinerEUI) || j.Short != "b10b" || j.Frames != 2 || j.LastClass != "Data" {
		t.Errorf("joiner %+v", j)
	}
	target, err := e.PairAutoTarget()
	if err != nil || target != hexEUI(joinerEUI) {
		t.Fatalf("auto target %q %v, want %s", target, err, hexEUI(joinerEUI))
	}
	if got, ok := e.PairJoinerTarget("B10B"); !ok || got != target {
		t.Errorf("joiner by short: %q %v", got, ok)
	}

	now = now.Add(2 * time.Minute)
	if e.PairWindow().Open {
		t.Error("the window outlived its duration")
	}
	e.OnFrame(pairingRS(t))
	if st := e.Stats(); st.MICFails != 2 || st.PairingKeyFrames != 2 {
		t.Errorf("expired window stats %+v, want a MIC failure", st)
	}
	if _, err := e.PairAutoTarget(); !errors.Is(err, ErrPairTarget) {
		t.Errorf("auto target after the window: %v", err)
	}
	if len(e.PairWindow().Joiners) != 1 {
		t.Error("the joiners of a closed window must stay visible")
	}
}

// TestPairWindowIgnoresBridge: a pairing-key frame with the bridge's own source
// prefix is the commercial bridge, never a joining device.
func TestPairWindowIgnoresBridge(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	e := pairEmu(t, &now)
	e.OpenPairWindow(time.Minute)
	pkt, err := BuildPairCoAP(NetworkKey, 1, BridgeEUI, joinerEUI)
	if err != nil {
		t.Fatal(err)
	}
	f, err := BuildData(PairingKey, 0x21, joinerEUI, BridgeEUI[:5], WrapAppPayload(pkt, 5, nil))
	if err != nil {
		t.Fatal(err)
	}
	e.OnFrame(f.Bytes())
	if n := len(e.PairWindow().Joiners); n != 0 {
		t.Errorf("the bridge became a joiner: %d", n)
	}
}

// TestPairWindowBounded: a window is capped at MaxPairWindow and a zero
// duration closes it.
func TestPairWindowBounded(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	e := pairEmu(t, &now)
	e.OpenPairWindow(time.Hour)
	if got := e.PairWindow().Until.Sub(now); got != MaxPairWindow {
		t.Errorf("window lasts %v, want %v", got, MaxPairWindow)
	}
	e.OpenPairWindow(0)
	if e.PairWindow().Open {
		t.Error("a zero duration left the window open")
	}
}

// pairDownlink queues d/pair to target and returns its frame.
func pairDownlink(t *testing.T, e *EmuState, target string) []byte {
	t.Helper()
	if err := e.EnqueuePair(target, NetworkKey); err != nil {
		t.Fatal(err)
	}
	actions, ok, err := e.NextQueuedDownlink(target)
	if err != nil || !ok {
		t.Fatalf("no queued pair: %v %v", ok, err)
	}
	a, ok := findAction(actions, ActData, "downlink pair")
	if !ok {
		t.Fatalf("no pair downlink in %v", reasons(actions))
	}
	return a.Bytes
}

// TestPairKeyFollowsWindow: d/pair goes under PairingKey to a device the window
// heard under the pairing key, and under the network key to one the window
// heard under the network key after that (the pairing capture: the second
// d/pair to the joined valve, answered 2.03). It goes from UDP 5683 to 5683.
func TestPairKeyFollowsWindow(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	e := pairEmu(t, &now)
	e.OpenPairWindow(2 * time.Minute)
	e.OnFrame(pairingRS(t))
	target := hexEUI(joinerEUI)

	raw := pairDownlink(t, e, target)
	f, err := ParseData(PairingKey, raw)
	if err != nil {
		t.Fatalf("first d/pair is not under the pairing key: %v", err)
	}
	m := ParseApp(f.Plaintext).CoAP
	if m.SrcPort != coapPort || m.DstPort != coapPort {
		t.Errorf("ports %d->%d, want 5683->5683", m.SrcPort, m.DstPort)
	}

	// The device answers 2.04 under the network key: it joined.
	now = now.Add(time.Second)
	ack := deviceAck(t, joinerEUI, m.MID, "2.04")
	e.OnFrame(ack)
	if e.Pending(target) != 0 {
		t.Fatalf("the 2.04 left %d queued", e.Pending(target))
	}

	raw = pairDownlink(t, e, target)
	f, err = ParseData(NetworkKey, raw)
	if err != nil {
		t.Fatalf("second d/pair is not under the network key: %v", err)
	}
	m = ParseApp(f.Plaintext).CoAP
	now = now.Add(time.Second)
	e.OnFrame(deviceAck(t, joinerEUI, m.MID, "2.03"))
	if e.Pending(target) != 0 {
		t.Errorf("the 2.03 of a device that holds the key left %d queued", e.Pending(target))
	}

	// With the window closed, d/pair goes under the pairing key again.
	e.OpenPairWindow(0)
	if _, err := ParseData(PairingKey, pairDownlink(t, e, target)); err != nil {
		t.Errorf("d/pair with a closed window is not under the pairing key: %v", err)
	}
}

// deviceAck is a CoAP ACK with an empty token from dev under the network key.
func deviceAck(t *testing.T, dev []byte, mid uint16, code string) []byte {
	t.Helper()
	pkt, err := BuildCoAPAck(code, mid, []byte{}, nil, dev, BridgeEUI, coapPort, coapPort)
	if err != nil {
		t.Fatal(err)
	}
	f, err := BuildData(NetworkKey, byte(mid), BridgeEUI, dev[:5], WrapAppPayload(pkt, uint32(mid), nil))
	if err != nil {
		t.Fatal(err)
	}
	return f.Bytes()
}

// TestPairWindowAnswersBeaconRequest: an Enhanced Beacon Request under the
// pairing key names a joiner, and with an armed transmitter it gets the
// Enhanced Beacon from the host when the radio does not answer it itself.
// Outside the window it gets nothing.
func TestPairWindowAnswersBeaconRequest(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	e := pairEmu(t, &now)
	e.SetTXArmed(func() bool { return true })
	ebr := beaconRequest(t, PairingKey, 0x7b, joinerEUI, 16, 0x07)

	if actions := e.OnFrame(ebr); actions != nil {
		t.Fatalf("closed window: actions %v", reasons(actions))
	}
	if n := len(e.PairWindow().Joiners); n != 0 {
		t.Fatalf("closed window learned %d joiners", n)
	}

	e.OpenPairWindow(2 * time.Minute)
	actions := e.OnFrame(ebr)
	if len(actions) != 1 || actions[0].Kind != ActBeacon {
		t.Fatalf("open window: actions %v, want one Enhanced Beacon", reasons(actions))
	}
	eb := actions[0].Bytes
	if Classify(eb) != ClassBeacon || !bytes.Equal(eb[5:13], joinerEUI) {
		t.Fatalf("beacon %x", eb)
	}
	want, err := BuildEnhancedBeacon(PairingKey, eb[2], joinerEUI, BridgeEUI, 100001)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(eb, want) {
		t.Errorf("beacon %x, want %x: the next seq and frame counter of the bridge", eb, want)
	}
	v := e.PairWindow()
	if len(v.Joiners) != 1 || v.Joiners[0].EUI != hexEUI(joinerEUI) || v.Joiners[0].LastClass != "BeaconReq" {
		t.Fatalf("joiners %+v", v.Joiners)
	}

	// The radio answers on its own: the host sends nothing.
	e.SetPairResponder(true)
	if actions := e.OnFrame(ebr); actions != nil {
		t.Errorf("radio responder: actions %v, want none", reasons(actions))
	}
	e.SetPairResponder(false)
	e.SetTXArmed(func() bool { return false })
	if actions := e.OnFrame(ebr); actions != nil {
		t.Errorf("unarmed: actions %v, want none", reasons(actions))
	}
	if got := e.PairWindow().Joiners[0].Frames; got != 3 {
		t.Errorf("joiner frames %d, want 3", got)
	}
}

// TestReservePairBeacons: the block the radio answers with is taken from the
// bridge seq and frame counter, so no host frame reuses it.
func TestReservePairBeacons(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	e := pairEmu(t, &now)
	a, err := e.Beacon()
	if err != nil {
		t.Fatal(err)
	}
	seq, ctr, err := e.ReservePairBeacons(64)
	if err != nil {
		t.Fatal(err)
	}
	if seq != a.Bytes[2]+1 || ctr != 100002 {
		t.Fatalf("block seq=%02x ctr=%d, want %02x and 100002", seq, ctr, a.Bytes[2]+1)
	}
	b, err := e.Beacon()
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseBroadcast(NetworkKey, b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if b.Bytes[2] != seq+64 || m.FrameCounter != ctr+64 {
		t.Errorf("next frame seq=%02x ctr=%d, want %02x and %d", b.Bytes[2], m.FrameCounter, seq+64, ctr+64)
	}
}

// TestPairWindowDoublesRouterAdvert: while the window is open and the radio
// takes the Frame Pending advertisement, each advertisement goes out twice,
// each behind its own wake-up train: under the network key with Frame Pending
// set, then under the pairing key, on consecutive seq and frame counter
// (the pairing capture, four pairs).
func TestPairWindowDoublesRouterAdvert(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	e := pairEmu(t, &now)
	burst := RouterAdvertWakeUpCount + 1
	single := func(label string) {
		t.Helper()
		actions, err := e.BeaconBurst()
		e.CompleteBeacon(false)
		if err != nil || len(actions) != burst || actions[burst-1].Bytes[0] != 0x49 {
			t.Fatalf("%s: %d actions, err %v, want one plain burst", label, len(actions), err)
		}
	}
	single("closed window")
	e.OpenPairWindow(2 * time.Minute)
	single("no radio responder")

	e.SetPairResponder(true)
	actions, err := e.BeaconBurst()
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2*burst {
		t.Fatalf("%d actions, want two bursts of %d", len(actions), burst)
	}
	first, second := actions[burst-1], actions[2*burst-1]
	if first.Kind != ActBroadcast || second.Kind != ActBroadcast {
		t.Fatalf("kinds %v %v", first.Kind, second.Kind)
	}
	if first.Bytes[0] != 0x59 || second.Bytes[0] != 0x49 {
		t.Errorf("FCF %02x then %02x, want 59 (Frame Pending) then 49", first.Bytes[0], second.Bytes[0])
	}
	for i := 0; i < RouterAdvertWakeUpCount; i++ {
		if actions[i].Kind != ActWakeUp || actions[i].Bytes[1] != first.Bytes[2] ||
			actions[burst+i].Kind != ActWakeUp || actions[burst+i].Bytes[1] != second.Bytes[2] {
			t.Fatalf("wake-up %d does not share the seq of its advertisement", i)
		}
	}
	if second.Bytes[2] != first.Bytes[2]+1 {
		t.Errorf("seq %02x then %02x, want consecutive", first.Bytes[2], second.Bytes[2])
	}
	net, err := ParseBroadcast(NetworkKey, first.Bytes)
	if err != nil {
		t.Fatalf("first advertisement is not under the network key: %v", err)
	}
	pair, err := ParseBroadcast(PairingKey, second.Bytes)
	if err != nil {
		t.Fatalf("second advertisement is not under the pairing key: %v", err)
	}
	if net.RA == nil || pair.RA == nil || pair.FrameCounter != net.FrameCounter+1 {
		t.Errorf("counters %d then %d, want consecutive advertisements", net.FrameCounter, pair.FrameCounter)
	}
	if !bytes.Equal(net.Plaintext[:len(net.Plaintext)-2], pair.Plaintext[:len(pair.Plaintext)-2]) {
		t.Error("the two advertisements carry different payloads")
	}
}

// TestPairWindowAnswersPairingKeySolicitation: a Router Solicitation under the
// pairing key gets the doubled advertisement, as the commercial bridge answers
// it (the pairing capture: the advertisement follows the solicitation by 1.05 s).
func TestPairWindowAnswersPairingKeySolicitation(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	e := pairEmu(t, &now)
	e.SetTXArmed(func() bool { return true })
	e.OpenPairWindow(2 * time.Minute)
	if actions := e.OnFrame(pairingRS(t)); actions != nil {
		t.Fatalf("no radio responder: actions %d, want none", len(actions))
	}
	e.SetPairResponder(true)
	now = now.Add(time.Second)
	actions := e.OnFrame(pairingRS(t))
	if len(actions) != 2*(RouterAdvertWakeUpCount+1) {
		t.Fatalf("actions %d, want the doubled advertisement", len(actions))
	}
	if got := e.Stats().RouterSolicitationsAnswered; got != 1 {
		t.Errorf("answered %d, want 1", got)
	}
}

// queueKinds lists the kinds queued to target, head first.
func queueKinds(e *EmuState, target string) []string {
	var kinds []string
	for _, q := range e.Queue(target) {
		kinds = append(kinds, q.Kind)
	}
	return kinds
}

// queuedAttempt builds the next queued attempt to target and returns its frame
// action.
func queuedAttempt(t *testing.T, e *EmuState, target string) (Action, bool) {
	t.Helper()
	actions, ok, err := e.NextQueuedDownlink(target)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return Action{}, false
	}
	return actions[len(actions)-1], true
}

// TestPairGoesFirst: d/pair goes ahead of every downlink queued to the joiner,
// as the commercial bridge sends it ahead of the frames it holds
// (the pairing capture: a d/config waits for its CoAP answer after its
// Enhanced ACK, and d/pair goes on without it). An
// attempt the radio holds keeps the head until it completes; any other attempt
// in flight waits behind d/pair with its MID, and the other writes keep their
// order.
func TestPairGoesFirst(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	e := pairEmu(t, &now)
	e.OpenPairWindow(2 * time.Minute)
	e.OnFrame(pairingRS(t))
	target := hexEUI(joinerEUI)

	if err := e.EnqueueIdentify(target, 1); err != nil {
		t.Fatal(err)
	}
	identify, ok := queuedAttempt(t, e, target)
	if !ok || !strings.HasPrefix(identify.Reason, "downlink identify") {
		t.Fatalf("first attempt %q, want the identify", identify.Reason)
	}
	if !e.MarkQueuedDownlinkAwaitingCSL(joinerEUI, identify.Bytes[2]) {
		t.Fatal("the radio did not take the identify attempt")
	}
	if err := e.EnqueueNetcmd(target, 1); err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueChildLock(target, 1, true); err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueuePair(target, NetworkKey); err != nil {
		t.Fatal(err)
	}
	if got, want := queueKinds(e, target), []string{"identify", "pair", "netcmd", "childlock"}; !slices.Equal(got, want) {
		t.Fatalf("queue %v, want %v: only the attempt on the radio stays ahead of d/pair", got, want)
	}
	if a, ok := queuedAttempt(t, e, target); ok {
		t.Fatalf("built %q while the radio holds the identify attempt", a.Reason)
	}

	// The attempt ends without an answer. The identify now waits for its CoAP
	// answer, and d/pair does not wait for it.
	e.CompleteQueuedDownlinkAttempt(joinerEUI, identify.Bytes[2])
	pair, ok := queuedAttempt(t, e, target)
	if !ok || !strings.HasPrefix(pair.Reason, "downlink pair") {
		t.Fatalf("attempt after the identify %q, want d/pair", pair.Reason)
	}
	if got, want := queueKinds(e, target), []string{"pair", "identify", "netcmd", "childlock"}; !slices.Equal(got, want) {
		t.Fatalf("queue %v, want %v", got, want)
	}
	q := e.Queue(target)
	if q[1].InFlight || q[1].Tries != 1 {
		t.Errorf("identify behind d/pair: %+v, want waiting with its one try", q[1])
	}
	identifyMID := q[1].MID

	f, err := ParseData(PairingKey, pair.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	// The 2.04 closes d/pair, and the same rendezvous carries the identify
	// again.
	now = now.Add(time.Second)
	actions := e.OnFrame(deviceAck(t, joinerEUI, ParseApp(f.Plaintext).CoAP.MID, "2.04"))
	if got, want := queueKinds(e, target), []string{"identify", "netcmd", "childlock"}; !slices.Equal(got, want) {
		t.Fatalf("queue after the 2.04 %v, want %v", got, want)
	}
	again, ok := findAction(actions, ActData, "downlink identify")
	if !ok || again.QueueMID != identifyMID || again.QueueTry != 2 {
		t.Errorf("attempt after d/pair %q mid=%04x try=%d, want the identify mid=%04x try=2", again.Reason, again.QueueMID, again.QueueTry, identifyMID)
	}
}

// TestPairKeyChosenAtSend: the key of d/pair follows the window when the frame
// is built, not when it is queued. A joiner that the window hears under the
// network key after d/pair was queued takes it under the network key.
func TestPairKeyChosenAtSend(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	e := pairEmu(t, &now)
	e.OpenPairWindow(2 * time.Minute)
	e.OnFrame(pairingRS(t))
	target := hexEUI(joinerEUI)
	if err := e.EnqueuePair(target, NetworkKey); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	actions := e.OnFrame(deviceAck(t, joinerEUI, 0x7777, "2.04"))
	a, ok := findAction(actions, ActData, "downlink pair")
	if !ok {
		t.Fatalf("no d/pair on the uplink: %v", reasons(actions))
	}
	if _, err := ParseData(NetworkKey, a.Bytes); err != nil {
		t.Errorf("d/pair built after a network-key frame is not under the network key: %v", err)
	}
}

// TestPairAlwaysBootstraps: every d/pair attempt may take the bootstrap train.
// The joiner of the pairing capture restarted its frame counter before both
// windows (counter 1 in both). A reboot moves its CSL phase, so
// an anchor from before the reboot misses.
func TestPairAlwaysBootstraps(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	e := pairEmu(t, &now)
	e.OpenPairWindow(2 * time.Minute)
	e.OnFrame(pairingRS(t))
	target := hexEUI(joinerEUI)
	if err := e.EnqueuePair(target, NetworkKey); err != nil {
		t.Fatal(err)
	}
	for try := 1; try <= 2; try++ {
		a, ok := queuedAttempt(t, e, target)
		if !ok || !strings.HasPrefix(a.Reason, "downlink pair") {
			t.Fatalf("try %d: %q, want d/pair", try, a.Reason)
		}
		if !a.CSLBootstrapAlways {
			t.Errorf("try %d: d/pair without CSLBootstrapAlways", try)
		}
	}
}
