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
	"freedo/internal/radio"
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

// A stored calibration holds for the whole session: the daemon writes it once,
// at start, and a long run of off-center FREQEST readings leaves the register
// and the store alone.
func TestLiveRadioHoldsTheStoredFSCTRL0(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "state.db"))
	if err := store.SetFSCTRL0(context.Background(), 0x33); err != nil {
		t.Fatal(err)
	}
	bridge := &offCenterBridge{tunableLiveBridge: newTunableBridge(0x40)}
	logs := captureLog(t)

	r, err := startSeededRadio(store, bridge, defaultInstall.CoordinatorEUI(), 0, false, true)
	if err != nil {
		t.Fatal(err)
	}
	want := 3 * cc1101.TuneWant
	deadline := time.Now().Add(5 * time.Second)
	for bridge.received() < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	state := r.Calibration().State
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if got := bridge.received(); got < want {
		t.Fatalf("the loop received %d frames, want at least %d", got, want)
	}
	if !bytes.Equal(bridge.writes, []byte{0x33}) {
		t.Fatalf("FSCTRL0 writes=%x, want 33: the register moved after start", bridge.writes)
	}
	if got := r.FSCTRL0(); got != 0x33 {
		t.Errorf("FSCTRL0()=%#02x want 0x33", got)
	}
	if state != radio.CalibrationStored {
		t.Errorf("calibration state=%s, want stored", state)
	}
	if !strings.Contains(logs.String(), "tune frame src=6162636465 path=recv") {
		t.Errorf("log=%q, want the FREQEST debug line", logs.String())
	}
}

// Without a stored calibration the radio measures one from the frames it
// hears, and the daemon stores the result for the next start.
func TestLiveRadioMeasuresAndTheDaemonStores(t *testing.T) {
	store := newTestStore(t, filepath.Join(t.TempDir(), "state.db"))
	bridge := &offCenterBridge{tunableLiveBridge: newTunableBridge(0x40)}
	captureLog(t)

	r, err := startSeededRadio(store, bridge, defaultInstall.CoordinatorEUI(), 0, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	d := newDaemon(defaultInstall, nil, r, true, "live")
	if err := d.attachState(store); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.storeCalibrations(ctx)

	// The bridge reads +15 whatever the register holds, so the radio writes
	// TuneRounds corrections and takes the last one.
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, ok, err := store.FSCTRL0(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			if v != 0x6d {
				t.Fatalf("stored FSCTRL0=%#02x, want 0x6d", v)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing stored, calibration %+v", r.Calibration())
		}
		time.Sleep(time.Millisecond)
	}
	if c := r.Calibration(); c.State != radio.CalibrationMeasured {
		t.Errorf("calibration=%+v, want measured", c)
	}
}
