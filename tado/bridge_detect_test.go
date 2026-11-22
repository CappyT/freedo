package tado

import (
	"encoding/hex"
	"testing"
)

func liveEmu(t *testing.T, live bool) *EmuState {
	t.Helper()
	e, _, _ := newTestEmu(t)
	e.cfg.LiveRadio = live
	return e
}

func installationShort(t *testing.T) []byte {
	t.Helper()
	b, err := hex.DecodeString(Inventory[1].Short)
	if err != nil || len(b) != 2 {
		t.Fatalf("inventory short %q: %v", Inventory[1].Short, err)
	}
	return b
}

func feedOwnAcks(e *EmuState) {
	device := []byte{0x02, 0xa6, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00}
	for i := 0; i < 50; i++ {
		e.OnFrame(BuildEnhAck(byte(i), device, e.selfEUI, 0x0100))
	}
}

func feedTrain(e *EmuState, dst []byte) {
	for _, f := range BuildWakeUpTrain(1, dst, 21, 248, 12) {
		e.OnFrame(f)
	}
}

// A half-duplex radio never hears its own frame: an Enhanced ACK from our EUI is
// the commercial bridge.
func TestBridgeSeenOnEnhAckFromOurEUI(t *testing.T) {
	e := liveEmu(t, true)
	feedOwnAcks(e)
	if !e.BridgeSeen() {
		t.Fatal("BridgeSeen=false after Enhanced ACKs from our coordinator EUI on a live radio")
	}
}

// A capture of a second receiver holds our own frames: they prove nothing.
func TestBridgeNotSeenOnEnhAckFromOurEUIInReplay(t *testing.T) {
	e := liveEmu(t, false)
	feedOwnAcks(e)
	if e.BridgeSeen() {
		t.Fatal("BridgeSeen=true from our own Enhanced ACKs outside a live radio")
	}
}

func TestBridgeSeenOnWakeUpTrainToOurDevice(t *testing.T) {
	e := liveEmu(t, true)
	feedTrain(e, installationShort(t))
	if !e.BridgeSeen() {
		t.Fatal("BridgeSeen=false after a wake-up train to a device of this installation")
	}
}

func TestBridgeNotSeenOnWakeUpTrainInReplay(t *testing.T) {
	e := liveEmu(t, false)
	feedTrain(e, installationShort(t))
	if e.BridgeSeen() {
		t.Fatal("BridgeSeen=true from a wake-up train outside a live radio")
	}
}

// A neighbour network on the same PAN addresses its own devices.
func TestBridgeNotSeenOnWakeUpTrainToForeignShort(t *testing.T) {
	e := liveEmu(t, true)
	feedTrain(e, []byte{0x12, 0x34})
	if e.BridgeSeen() {
		t.Fatal("BridgeSeen=true from a wake-up train to a short address outside the installation")
	}
}
