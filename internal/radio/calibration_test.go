package radio

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"freedo/cc1101"
)

// carrierBridge models a receiver whose carrier sits off the Tado carrier. On
// the stream path it hears frames of four sources, and each reads the offset
// left after the register: carrier - int8(FSCTRL0). With fixed set, every
// reading is that value whatever the register holds. With csl set, the frames
// come only from the CSL queue and the stream path hears nothing.
type carrierBridge struct {
	*orderedLiveBridge
	mu      sync.Mutex
	reg     byte
	writes  []byte
	carrier int
	fixed   *int
	csl     bool
	polls   int
	n       int
	quiet   bool
}

func newCarrierBridge(reg byte, carrier int) *carrierBridge {
	return &carrierBridge{orderedLiveBridge: &orderedLiveBridge{}, reg: reg, carrier: carrier}
}

func (b *carrierBridge) FSCTRL0() byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reg
}

func (b *carrierBridge) SetFSCTRL0(v byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reg = v
	b.writes = append(b.writes, v)
	return nil
}

func (b *carrierBridge) frameLocked() *cc1101.RxFrame {
	b.n++
	fe := b.carrier - int(int8(b.reg))
	if b.fixed != nil {
		fe = *b.fixed
	}
	return tuneDataFrame(fmt.Sprintf("src%02d", b.n%4), fe)
}

func (b *carrierBridge) RecvFW(time.Duration) (*cc1101.RxFrame, error) {
	time.Sleep(100 * time.Microsecond)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.csl || b.quiet {
		return nil, nil
	}
	return b.frameLocked(), nil
}

func (b *carrierBridge) EnqueueCSLBatch([]cc1101.CSLQueueRequest) error { return nil }
func (b *carrierBridge) FlushCSLQueue() error                           { return nil }

func (b *carrierBridge) PollCSLQueue() (*cc1101.CSLQueueEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.polls++
	if !b.csl {
		return nil, nil
	}
	return &cc1101.CSLQueueEvent{Inbound: b.frameLocked()}, nil
}

func (b *carrierBridge) snapshot() ([]byte, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.writes...), b.polls
}

func (b *carrierBridge) setQuiet(quiet bool) {
	b.mu.Lock()
	b.quiet = quiet
	b.mu.Unlock()
}

func startCarrier(t *testing.T, br *carrierBridge, stored bool) *Live {
	t.Helper()
	r, err := StartSeeded(br, defaultInstall.CoordinatorEUI(), 0, false, false, stored)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func awaitCalibration(t *testing.T, r *Live) CalibrationResult {
	t.Helper()
	select {
	case res := <-r.CalibrationResults():
		return res
	case <-time.After(5 * time.Second):
		t.Fatalf("no calibration result, state %+v", r.Calibration())
		return CalibrationResult{}
	}
}

// A -5 reading at the default register is inside the live band: the register
// stays and is the result. The on-air measurement reads about that far off the
// bench value.
func TestLiveRadioCalibrationInsideTheBandWritesNothing(t *testing.T) {
	br := newCarrierBridge(cc1101.TuneDefault, int(int8(cc1101.TuneDefault))-5)
	r := startCarrier(t, br, false)
	res := awaitCalibration(t, r)
	if writes, _ := br.snapshot(); len(writes) != 0 {
		t.Fatalf("FSCTRL0 writes=%x, want none", writes)
	}
	if res.FSCTRL0 != cc1101.TuneDefault || r.Calibration().State != CalibrationMeasured {
		t.Errorf("result=%+v state=%s, want 0x34 measured", res, r.Calibration().State)
	}
}

// A carrier that never answers the correction stops after TuneRounds writes, on
// the register as it stands.
func TestLiveRadioCalibrationStopsAfterTheRounds(t *testing.T) {
	fixed := 10
	br := newCarrierBridge(0x00, 0)
	br.fixed = &fixed
	r := startCarrier(t, br, false)
	res := awaitCalibration(t, r)
	writes, _ := br.snapshot()
	if !bytes.Equal(writes, []byte{0x0a, 0x14, 0x1e}) {
		t.Fatalf("FSCTRL0 writes=%x, want 0a 14 1e", writes)
	}
	if res.FSCTRL0 != 0x1e {
		t.Errorf("result=%+v, want FSCTRL0 0x1e", res)
	}
}

// CSL inbound frames follow the own transmission of the chip, so they never
// count as readings.
func TestLiveRadioCalibrationIgnoresCSLInbound(t *testing.T) {
	br := newCarrierBridge(0x00, -13)
	br.csl = true
	r := startCarrier(t, br, false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, polls := br.snapshot(); polls >= 4*cc1101.TuneWant {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the loop did not poll the CSL queue")
		}
		select {
		case <-r.Frames():
		case <-time.After(time.Millisecond):
		}
	}
	if c := r.Calibration(); c.State != CalibrationMeasuring || c.Readings != 0 {
		t.Fatalf("calibration=%+v, want measuring with no readings", c)
	}
	if writes, _ := br.snapshot(); len(writes) != 0 {
		t.Fatalf("FSCTRL0 writes=%x, want none", writes)
	}
}

// A module more than 20 units off pins its readings at the FOC limit. The round
// completes on the clipped readings and moves the register by the clip value;
// the next round reads -5, inside the band.
func TestLiveRadioCalibrationConvergesFromClippedReadings(t *testing.T) {
	br := newCarrierBridge(0x00, -25)
	r := startCarrier(t, br, false)
	res := awaitCalibration(t, r)
	writes, _ := br.snapshot()
	if !bytes.Equal(writes, []byte{0xec}) {
		t.Fatalf("FSCTRL0 writes=%x, want ec", writes)
	}
	if res.FSCTRL0 != 0xec {
		t.Errorf("result=%+v, want FSCTRL0 0xec", res)
	}
}

// Readings that clip on both sides never complete a round, and a source keeps
// a bounded number of them.
func TestLiveRadioCalibrationSamplesStayBounded(t *testing.T) {
	r := &Live{tuner: newCarrierBridge(0x00, 0), calResults: make(chan CalibrationResult, 1)}
	r.startMeasureLocked()
	for i := range 1000 {
		fe := 25
		if i%2 == 0 {
			fe = -25
		}
		r.collectTune(tuneDataFrame("abcde", fe))
	}
	r.collectTune(tuneDataFrame("abcde", 1))
	r.calMu.Lock()
	defer r.calMu.Unlock()
	if n := len(r.cal.samples["abcde"]); n != tuneKeep || r.cal.total != tuneKeep || r.cal.usable != 1 {
		t.Fatalf("samples=%d total=%d usable=%d, want %d, %d and 1", n, r.cal.total, r.cal.usable, tuneKeep, tuneKeep)
	}
	if r.cal.due {
		t.Fatal("a round of two-sided clipping completed")
	}
}

// FSCTRL0 is a signed byte: a correction saturates at 0x7f and 0x80, and a
// register at its limit is the result.
func TestLiveRadioCalibrationSaturatesTheRegister(t *testing.T) {
	for _, tc := range []struct {
		reg   byte
		fixed int
		want  byte
	}{
		{reg: 0x7c, fixed: 10, want: 0x7f},
		{reg: 0x84, fixed: -10, want: 0x80},
	} {
		fixed := tc.fixed
		br := newCarrierBridge(tc.reg, 0)
		br.fixed = &fixed
		r := startCarrier(t, br, false)
		res := awaitCalibration(t, r)
		if writes, _ := br.snapshot(); !bytes.Equal(writes, []byte{tc.want}) {
			t.Errorf("from %#02x: FSCTRL0 writes=%x, want %x", tc.reg, writes, tc.want)
		}
		if res.FSCTRL0 != tc.want {
			t.Errorf("from %#02x: result=%+v, want %#02x", tc.reg, res, tc.want)
		}
	}
	if got := tuneTarget(0x10, -0x20); got != 0xf0 {
		t.Errorf("tuneTarget(0x10, -32)=%#02x, want 0xf0", got)
	}
}

// A module at -13 from the default start register moves in one write; the next
// round confirms the register inside the band and reports it.
func TestLiveRadioCalibrationConvergesOnTheStream(t *testing.T) {
	logs := captureLog(t)
	br := newCarrierBridge(cc1101.TuneDefault, int(int8(cc1101.TuneDefault))-13)
	r := startCarrier(t, br, false)
	if c := r.Calibration(); c.State != CalibrationMeasuring || c.Want != cc1101.TuneWant {
		t.Fatalf("calibration at start=%+v, want measuring of %d", c, cc1101.TuneWant)
	}
	res := awaitCalibration(t, r)
	writes, _ := br.snapshot()
	if !bytes.Equal(writes, []byte{0x27}) {
		t.Fatalf("FSCTRL0 writes=%x, want 27", writes)
	}
	if res.FSCTRL0 != 0x27 || res.Frames != cc1101.TuneWant || res.Sources != 4 {
		t.Errorf("result=%+v, want FSCTRL0 0x27 on %d frames of 4 sources", res, cc1101.TuneWant)
	}
	if c := r.Calibration(); c.State != CalibrationMeasured || c.Readings != 0 || c.Want != 0 {
		t.Errorf("calibration=%+v, want measured without counts", c)
	}
	if got := r.FSCTRL0(); got != 0x27 {
		t.Errorf("FSCTRL0()=%#02x, want 0x27", got)
	}
	if !strings.Contains(logs.String(), "receiver calibration: round 1: correction -13 on 20 frames of 4 sources, FSCTRL0 0x34 -> 0x27") {
		t.Errorf("log=%q, want the round line", logs.String())
	}
}

// A stored calibration collects nothing. Recalibrate starts a new session: the
// loop writes the default register first, and only then samples.
func TestLiveRadioRecalibrateRestartsTheSampling(t *testing.T) {
	br := newCarrierBridge(0x2f, int(int8(cc1101.TuneDefault))-5)
	r := startCarrier(t, br, true)
	time.Sleep(20 * time.Millisecond)
	before := r.Calibration()
	if before.State != CalibrationStored || before.Readings != 0 {
		t.Fatalf("calibration=%+v, want stored", before)
	}
	c := r.Recalibrate()
	if c.State != CalibrationMeasuring || c.Session != before.Session+1 || c.Want != cc1101.TuneWant {
		t.Fatalf("Recalibrate=%+v, want a new measuring session", c)
	}
	res := awaitCalibration(t, r)
	if res.Session != c.Session || res.FSCTRL0 != cc1101.TuneDefault {
		t.Errorf("result=%+v, want 0x34 of session %d", res, c.Session)
	}
	if writes, _ := br.snapshot(); !bytes.Equal(writes, []byte{cc1101.TuneDefault}) {
		t.Errorf("FSCTRL0 writes=%x, want 34", writes)
	}

	br.setQuiet(true)
	partial := &Live{tuner: br, calResults: make(chan CalibrationResult, 1)}
	partial.startMeasureLocked()
	for range 5 {
		partial.collectTune(tuneDataFrame("abcde", -13))
	}
	if got := partial.Calibration().Readings; got != 5 {
		t.Fatalf("readings=%d, want 5", got)
	}
	if err := br.SetFSCTRL0(0x2f); err != nil {
		t.Fatal(err)
	}
	partial.fsctrl0.Store(0x2f)
	again := partial.Recalibrate()
	if again.Readings != 0 || again.State != CalibrationMeasuring {
		t.Errorf("Recalibrate=%+v, want measuring from 0", again)
	}
	partial.collectTune(tuneDataFrame("abcde", -13))
	if got := partial.Calibration().Readings; got != 0 {
		t.Fatalf("readings=%d before the reset write, want 0", got)
	}
	partial.tuneStep()
	if got := partial.FSCTRL0(); got != cc1101.TuneDefault || br.FSCTRL0() != cc1101.TuneDefault {
		t.Fatalf("FSCTRL0 after the reset=%#02x (bridge %#02x), want 0x34", got, br.FSCTRL0())
	}
	partial.collectTune(tuneDataFrame("abcde", -13))
	if got := partial.Calibration().Readings; got != 1 {
		t.Errorf("readings=%d after the reset write, want 1", got)
	}
}
