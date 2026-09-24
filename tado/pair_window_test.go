package tado

import (
	"errors"
	"testing"
	"time"
)

// joiner is a device outside the installation that talks the pairing key.
var joinerEUI = mustHex("0bb1cefa07c51b00")

// pairingRS is a Router Solicitation from joinerEUI under PairingKey, as a
// joining valve sends it (coap_sniff_1786619225).
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
// heard under the network key after that (coap_sniff_1786619225: the second
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
