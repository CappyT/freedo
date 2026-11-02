package daemon

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
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
	// TuneRounds corrections from the default and takes the last one.
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, ok, err := store.FSCTRL0(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			if v != 0x61 {
				t.Fatalf("stored FSCTRL0=%#02x, want 0x61", v)
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
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if want := []byte{cc1101.TuneDefault, 0x43, 0x52, 0x61}; !bytes.Equal(bridge.writes, want) {
		t.Errorf("FSCTRL0 writes=%x, want %x: the default seed, then the corrections", bridge.writes, want)
	}
}

// Without a stored calibration the daemon seeds the default register, the start
// of the measurement. A stored value wins.
func TestSeedFSCTRL0SeedsTheDefault(t *testing.T) {
	for _, tc := range []struct {
		name   string
		store  bool
		stored bool
		want   byte
	}{
		{name: "no database", want: cc1101.TuneDefault},
		{name: "nothing stored", store: true, want: cc1101.TuneDefault},
		{name: "stored calibration", store: true, stored: true, want: 0x2f},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var store *stateStore
			if tc.store {
				store = newTestStore(t, filepath.Join(t.TempDir(), "state.db"))
			}
			if tc.stored {
				if err := store.SetFSCTRL0(context.Background(), 0x2f); err != nil {
					t.Fatal(err)
				}
			}
			bridge := newTunableBridge(0x40)
			if got := seedFSCTRL0(store, bridge); got != tc.stored {
				t.Errorf("seedFSCTRL0=%t, want %t", got, tc.stored)
			}
			if !bytes.Equal(bridge.writes, []byte{tc.want}) {
				t.Errorf("FSCTRL0 writes=%x, want %x", bridge.writes, tc.want)
			}
		})
	}
}

// carrierLiveBridge hears frames whose FREQEST is the carrier offset left after
// the register: carrier - int8(FSCTRL0).
type carrierLiveBridge struct {
	*orderedLiveBridge
	mu      sync.Mutex
	reg     byte
	writes  []byte
	carrier int
}

func (b *carrierLiveBridge) FSCTRL0() byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reg
}

func (b *carrierLiveBridge) SetFSCTRL0(v byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reg = v
	b.writes = append(b.writes, v)
	return nil
}

func (b *carrierLiveBridge) RecvFW(time.Duration) (*cc1101.RxFrame, error) {
	time.Sleep(100 * time.Microsecond)
	b.mu.Lock()
	defer b.mu.Unlock()
	return tuneDataFrame("abcde", b.carrier-int(int8(b.reg))), nil
}

// The route forgets a stored 0x2f, writes the default register before the
// measurement samples, and the -5 readings at the default end it there.
func TestRadioCalibrateRouteResetsToTheDefault(t *testing.T) {
	logs := captureLog(t)
	store := newTestStore(t, filepath.Join(t.TempDir(), "state.db"))
	if err := store.SetFSCTRL0(context.Background(), 0x2f); err != nil {
		t.Fatal(err)
	}
	bridge := &carrierLiveBridge{
		orderedLiveBridge: &orderedLiveBridge{result: &cc1101.AutoAckResult{Status: cc1101.AutoAckNoFrame}},
		reg:               0x40,
		carrier:           int(int8(cc1101.TuneDefault)) - 5,
	}
	r, err := startSeededRadio(store, bridge, defaultInstall.CoordinatorEUI(), 0, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if c := r.Calibration(); c.State != radio.CalibrationStored || r.FSCTRL0() != 0x2f {
		t.Fatalf("calibration=%+v FSCTRL0=%#02x, want stored 0x2f", c, r.FSCTRL0())
	}
	d := newDaemon(defaultInstall, nil, r, true, "live")
	if err := d.attachState(store); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stored := make(chan struct{})
	go func() {
		d.storeCalibrations(ctx)
		close(stored)
	}()

	rec := httptest.NewRecorder()
	d.mux().ServeHTTP(rec, httptest.NewRequest("POST", "/api/radio/calibrate", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s, want 202", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, ok, err := store.FSCTRL0(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			if v != cc1101.TuneDefault {
				t.Fatalf("stored FSCTRL0=%#02x, want 0x34", v)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing stored, calibration %+v", r.Calibration())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-stored
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if want := []byte{0x2f, cc1101.TuneDefault}; !bytes.Equal(bridge.writes, want) {
		t.Errorf("FSCTRL0 writes=%x, want %x: the seed, then the reset", bridge.writes, want)
	}
	if want := "receiver calibration: stored value forgotten, FSCTRL0 0x34 (default), measuring again"; !strings.Contains(logs.String(), want) {
		t.Errorf("log=%q, want %q", logs.String(), want)
	}
}
