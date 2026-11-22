package main

import (
	"io"
	"testing"
	"time"

	"freedo/cc1101"
	"freedo/tado"
)

func TestEmulateDryRunReleasesRouterAdvert(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{RouterAdvertInterval: time.Minute}, tado.NewUnhandledLogger(io.Discard))
	actions, due, err := emu.MaybeBeacon()
	if err != nil || !due || len(actions) != tado.RouterAdvertWakeUpCount+1 {
		t.Fatalf("first dry-run beacon: actions=%d due=%v err=%v", len(actions), due, err)
	}
	completeDryRunBeacon(emu, false)
	actions, due, err = emu.MaybeBeacon()
	if err != nil || !due || len(actions) != tado.RouterAdvertWakeUpCount+1 {
		t.Fatalf("dry-run left beacon pending: actions=%d due=%v err=%v", len(actions), due, err)
	}
	emu.CompleteBeacon(false)
}

// streamLink replays frames as one firmware stream and records every send, and
// whether it came while the stream ran.
type streamLink struct {
	frames    [][]byte
	streaming bool
	stopped   bool
	inStream  int
	sent      int
}

func (l *streamLink) StreamUntil(_ time.Duration, cb func(*cc1101.RxFrame) bool) error {
	l.streaming = true
	defer func() { l.streaming = false }()
	for _, fr := range l.frames {
		if cb(&cc1101.RxFrame{Payload: fr, CRCOK: true}) {
			l.stopped = true
			return nil
		}
	}
	return nil
}

func (l *streamLink) Send([]byte, time.Duration) (bool, error) {
	if l.streaming {
		l.inStream++
	}
	l.sent++
	return true, nil
}

// TestEmulateTXSendsAfterTheStream: with TX armed, the frame that decides an
// action ends the stream, and the action goes out once the stream returned: a
// send inside it would end the firmware stream and misread its STREAM_END.
func TestEmulateTXSendsAfterTheStream(t *testing.T) {
	emu := tado.NewEmu(fixtureInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	link := &streamLink{frames: loadFrames(t, "../../tado/testdata/frames_69ec.txt", 400)}
	if err := emulateWindow(link, time.Second, emu, fixtureInstall.NewDissector(), nil, true); err != nil {
		t.Fatal(err)
	}
	if !link.stopped || link.sent == 0 {
		t.Fatalf("stopped=%t sent=%d, want the stream ended by a frame that decides an action", link.stopped, link.sent)
	}
	if link.inStream != 0 {
		t.Fatalf("%d sends inside the stream, want none", link.inStream)
	}
}

// TestEmitLiveStopsOnceTheBridgeIsHeard: an Enhanced ACK from our own EUI means
// the commercial bridge is on air, so no action goes out any more.
func TestEmitLiveStopsOnceTheBridgeIsHeard(t *testing.T) {
	cfg := fixtureInstall.EmuConfig()
	cfg.LiveRadio = true
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	link := &streamLink{}
	act := tado.Action{Bytes: []byte{1}}
	emitLive(link, emu, act, true)
	if link.sent != 1 {
		t.Fatalf("sent=%d before the bridge is heard, want 1", link.sent)
	}
	device := []byte{0x02, 0xa6, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00}
	emu.OnFrame(tado.BuildEnhAck(1, device, fixtureInstall.CoordinatorEUI(), 0x0100))
	if !emu.BridgeSeen() {
		t.Fatal("setup: the bridge ACK was not detected")
	}
	emitLive(link, emu, act, true)
	if link.sent != 1 {
		t.Fatalf("sent=%d after the bridge is heard, want no more", link.sent)
	}
}
