package daemon

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"freedo/cc1101"
)

// tunableLiveBridge is a live bridge with a frequency-offset register, like the
// real one. It records every register write.
type tunableLiveBridge struct {
	*orderedLiveBridge
	reg    byte
	writes []byte
	err    error
}

func (b *tunableLiveBridge) FSCTRL0() byte { return b.reg }

func (b *tunableLiveBridge) SetFSCTRL0(value byte) error {
	if b.err != nil {
		return b.err
	}
	b.reg = value
	b.writes = append(b.writes, value)
	return nil
}

func newTunableBridge(reg byte) *tunableLiveBridge {
	return &tunableLiveBridge{
		orderedLiveBridge: &orderedLiveBridge{result: &cc1101.AutoAckResult{Status: cc1101.AutoAckNoFrame}},
		reg:               reg,
	}
}

// offCenterBridge receives an endless run of frames whose FREQEST sits far
// from zero, the input a runtime tune would fold back into FSCTRL0.
type offCenterBridge struct {
	*tunableLiveBridge
	mu   sync.Mutex
	recv int
}

func (b *offCenterBridge) RecvFW(time.Duration) (*cc1101.RxFrame, error) {
	b.mu.Lock()
	b.recv++
	b.mu.Unlock()
	time.Sleep(time.Millisecond)
	return tuneDataFrame("abcde", 15), nil
}

func (b *offCenterBridge) received() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.recv
}

// tuneDataFrame is a synthetic coordinator Data frame with one FREQEST reading.
// freqSource labels it by its source address.
func tuneDataFrame(source string, freqEst int) *cc1101.RxFrame {
	payload := make([]byte, 64)
	payload[0], payload[1] = 0x69, 0xec
	copy(payload[11:16], source)
	return &cc1101.RxFrame{Payload: payload, CRCOK: true, FreqEst: freqEst, HasFreqEst: true}
}

// captureLog sends the daemon log into a buffer for the length of one test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &logs
}

// The daemon writes FSCTRL0 once, at start, from the stored calibration. A long
// run of off-center FREQEST readings leaves the register and the store alone.
func TestLiveRadioHoldsTheSeededFSCTRL0(t *testing.T) {
	tests := []struct {
		name   string
		stored bool
		writes []byte
		reg    byte
	}{
		{name: "stored calibration", stored: true, writes: []byte{0x33}, reg: 0x33},
		{name: "no calibration", reg: 0x40},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t, filepath.Join(t.TempDir(), "state.db"))
			if tt.stored {
				if err := store.SetFSCTRL0(context.Background(), 0x33); err != nil {
					t.Fatal(err)
				}
			}
			bridge := &offCenterBridge{tunableLiveBridge: newTunableBridge(0x40)}
			logs := captureLog(t)

			radio, err := startSeededRadio(store, bridge, defaultInstall.CoordinatorEUI(), 0, false, true)
			if err != nil {
				t.Fatal(err)
			}
			want := 3 * cc1101.TuneWant
			deadline := time.Now().Add(5 * time.Second)
			for bridge.received() < want && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if err := radio.Close(); err != nil {
				t.Fatal(err)
			}
			if got := bridge.received(); got < want {
				t.Fatalf("the loop received %d frames, want at least %d", got, want)
			}

			if !bytes.Equal(bridge.writes, tt.writes) {
				t.Fatalf("FSCTRL0 writes=%x, want %x: the register moved after start", bridge.writes, tt.writes)
			}
			if got := radio.FSCTRL0(); got != tt.reg {
				t.Errorf("FSCTRL0()=%#02x want %#02x", got, tt.reg)
			}
			value, ok, err := store.FSCTRL0(context.Background())
			if err != nil || ok != tt.stored || (ok && value != 0x33) {
				t.Errorf("stored FSCTRL0=%#02x,%t,%v, want the calibration untouched", value, ok, err)
			}
			if !strings.Contains(logs.String(), "tune frame src=6162636465 path=recv") {
				t.Errorf("log=%q, want the FREQEST debug line", logs.String())
			}
		})
	}
}
