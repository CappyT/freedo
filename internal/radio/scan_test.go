package radio

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"freedo/cc1101"
	"freedo/tado"
)

// testPlant is an installation with the devices that send the given sources.
type testPlant struct {
	mu      sync.Mutex
	sources []string
}

func (p *testPlant) add(source string) {
	p.mu.Lock()
	p.sources = append(p.sources, source)
	p.mu.Unlock()
}

func (p *testPlant) Devices() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sources)
}

func (p *testPlant) HasSource(source string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Contains(p.sources, source)
}

// scanPlant is a radio with one device, on a clock the test moves. Its first
// idle step starts the silence.
func scanPlant(t *testing.T) (*Live, *carrierBridge, *stepClock) {
	t.Helper()
	br := newCarrierBridge(cc1101.TuneDefault, 0)
	r, clock := scanLive(br)
	r.SetPlant(&testPlant{sources: []string{"devic"}})
	r.tuneStep()
	return r, br, clock
}

func writesOf(br *carrierBridge) []byte {
	writes, _ := br.snapshot()
	return writes
}

// The scan moves the transmit carrier as well as the receive one. It starts
// only when the plant has a device and no frame from a device arrived for 10
// minutes. A frame of another source, such as a neighbour network or the
// commercial coordinator, does not count.
func TestScanStartsAfterTenQuietMinutesOnAPlantWithADevice(t *testing.T) {
	r, br, clock := scanPlant(t)

	clock.advance(calibrationQuietBefore - time.Second)
	r.collectTune(tuneDataFrame("other", 0))
	r.tuneStep()
	if writes := writesOf(br); len(writes) != 0 {
		t.Fatalf("writes %x before the ten minutes", writes)
	}
	clock.advance(time.Second)
	r.tuneStep()
	first := scanRegisters(cc1101.TuneDefault)[0]
	if writes := writesOf(br); !bytes.Equal(writes, []byte{first}) {
		t.Fatalf("writes %x, want the first scan register %02x", writes, first)
	}
	if w := r.Warnings(); len(w) != 1 || !strings.Contains(w[0], "scanning FSCTRL0") {
		t.Fatalf("warnings while scanning: %q", w)
	}
}

func TestScanSilenceCountsFromTheLastFrameOfADevice(t *testing.T) {
	r, br, clock := scanPlant(t)
	clock.advance(9 * time.Minute)
	r.collectTune(tuneDataFrame("devic", 0))
	clock.advance(9 * time.Minute)
	r.tuneStep()
	if writes := writesOf(br); len(writes) != 0 {
		t.Fatalf("writes %x 9 minutes after a frame of a device", writes)
	}
	clock.advance(time.Minute)
	r.tuneStep()
	if writes := writesOf(br); len(writes) != 1 {
		t.Fatalf("writes %x 10 minutes after a frame of a device, want the first step", writes)
	}
}

// A frame of a device that arrives through the CSL queue ends the silence too:
// it carries no reading, but it shows the receiver decodes.
func TestScanSilenceEndsOnACSLInboundFrameOfADevice(t *testing.T) {
	r, br, clock := scanPlant(t)
	clock.advance(calibrationQuietBefore - time.Second)
	r.noteHeard(tuneDataFrame("devic", 0))
	clock.advance(time.Second)
	r.tuneStep()
	if writes := writesOf(br); len(writes) != 0 {
		t.Fatalf("writes %x after an inbound frame of a device", writes)
	}
}

// An empty plant has no device to hear: a pairing request every minute gives no
// reading and the register stays on the default for the whole hour, as does the
// register of a radio that was never told its plant. A plant that gets its first
// device counts the silence from then on.
func TestEmptyPlantStaysOnTheDefaultRegister(t *testing.T) {
	for name, plant := range map[string]*testPlant{"no plant": nil, "no device": {}} {
		t.Run(name, func(t *testing.T) {
			br := newCarrierBridge(cc1101.TuneDefault, 0)
			r, clock := scanLive(br)
			if plant != nil {
				r.SetPlant(plant)
			}
			request := &cc1101.RxFrame{CRCOK: true, HasFreqEst: true, Payload: append([]byte{0x0b, 0xe8}, make([]byte, 25)...)}
			for minute := 0; minute <= 90; minute++ {
				r.collectTune(request)
				r.tuneStep()
				if r.FSCTRL0() != cc1101.TuneDefault {
					t.Fatalf("minute %d: FSCTRL0 %#02x, want the default", minute, r.FSCTRL0())
				}
				clock.advance(time.Minute)
			}
			if writes := writesOf(br); len(writes) != 0 {
				t.Fatalf("writes %x on an empty plant", writes)
			}
			if plant == nil {
				return
			}
			plant.add("devic")
			r.tuneStep()
			clock.advance(calibrationQuietBefore - time.Second)
			r.tuneStep()
			if writes := writesOf(br); len(writes) != 0 {
				t.Fatalf("writes %x before 10 minutes with a device", writes)
			}
			clock.advance(time.Second)
			r.tuneStep()
			if writes := writesOf(br); len(writes) != 1 {
				t.Fatalf("writes %x, want the first scan step", writes)
			}
		})
	}
}

// A full pass steps the register through the whole list, 3 minutes each, then
// goes back to the home register and waits one hour for the next pass. Health
// shows a warning while it scans and after the failed pass. The register is on
// the default for more than half of the first hour.
func TestScanPassGoesBackHomeAndWaitsAnHour(t *testing.T) {
	r, br, clock := scanPlant(t)
	list := scanRegisters(cc1101.TuneDefault)
	home := 0
	var failedAt time.Duration
	for minute := 0; minute <= 60; minute++ {
		r.tuneStep()
		if r.FSCTRL0() == cc1101.TuneDefault {
			home++
		}
		if failedAt == 0 && len(writesOf(br)) == len(list)+1 {
			failedAt = time.Duration(minute) * time.Minute
		}
		clock.advance(time.Minute)
	}
	want := append(slices.Clone(list), cc1101.TuneDefault)
	if writes := writesOf(br); !bytes.Equal(writes, want) {
		t.Fatalf("writes %x, want the list and the home register %x", writes, want)
	}
	if home < 31 {
		t.Fatalf("%d minutes on the default register in the first hour, want more than half", home)
	}
	if failedAt == 0 {
		t.Fatal("the pass did not end")
	}
	w := r.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "decoded no frame from a device on any register") {
		t.Fatalf("warnings after a failed pass: %q", w)
	}
	// The next pass waits for the hour that follows the end of the pass.
	clock.advance(failedAt + calibrationScanHold - time.Duration(61)*time.Minute - time.Second)
	r.tuneStep()
	if writes := writesOf(br); len(writes) != len(want) {
		t.Fatalf("a new pass started inside the hour: %x", writes)
	}
	clock.advance(time.Second)
	r.tuneStep()
	if writes := writesOf(br); len(writes) != len(want)+1 || writes[len(want)] != list[0] {
		t.Fatalf("writes %x, want a new pass at %02x after the hour", writes, list[0])
	}
}

// The register goes back to the stored calibration, not to the default.
func TestScanPassGoesBackToTheStoredRegister(t *testing.T) {
	br := newCarrierBridge(0x2f, 0)
	r, clock := scanLive(br)
	r.cal.state, r.cal.stored, r.cal.hasStored = CalibrationStored, 0x2f, true
	r.SetPlant(&testPlant{sources: []string{"devic"}})
	r.tuneStep()
	clock.advance(calibrationQuietBefore)
	list := scanRegisters(0x2f)
	for range len(list) + 1 {
		r.tuneStep()
		clock.advance(calibrationScanDwell)
	}
	if got := r.FSCTRL0(); got != 0x2f {
		t.Fatalf("FSCTRL0 %#02x after the failed pass, want the stored 0x2f", got)
	}
	if writes := writesOf(br); len(writes) != len(list)+1 || writes[0] != list[0] {
		t.Fatalf("writes %x, want the list around the stored register and the stored register", writes)
	}
	if r.Calibration().State != CalibrationStored {
		t.Fatalf("state %s, want stored", r.Calibration().State)
	}
}

// A frame of a device on a scan step ends the scan and makes that step the
// candidate: the register stays, the normal rounds measure it, and health shows
// the candidate. The long transmissions stay deferred, because the register is
// not the home one.
func TestFrameOfADeviceOnAScanStepMakesThatStepTheCandidate(t *testing.T) {
	r, br, clock := scanPlant(t)
	clock.advance(calibrationQuietBefore)
	r.tuneStep()
	clock.advance(calibrationScanDwell)
	r.tuneStep()
	writes := writesOf(br)
	if len(writes) != 2 {
		t.Fatalf("writes %x, want two scan steps", writes)
	}
	r.collectTune(tuneDataFrame("devic", 7))
	w := r.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "a frame of a device decoded on FSCTRL0") || !strings.Contains(w[0], "stored only if the measurement settles") {
		t.Fatalf("warnings after a frame of a device: %q", w)
	}
	clock.advance(calibrationQuietBefore - time.Second)
	r.tuneStep()
	if after := writesOf(br); !bytes.Equal(after, writes) {
		t.Fatalf("the scan went on after a frame: %x -> %x", writes, after)
	}
	c := r.Calibration()
	if c.State != CalibrationMeasuring || c.Readings != 1 || !c.Candidate {
		t.Fatalf("calibration %+v, want a measuring candidate with the reading of that frame", c)
	}
	if !r.LongTXDeferred() {
		t.Fatal("long transmissions allowed on a candidate register")
	}
}

// The candidate becomes the stored register when the measurement settles inside
// the band. Nothing is deferred then: the register is the home value.
func TestCandidateSettlingInsideTheBandIsStored(t *testing.T) {
	r, br, clock := scanPlant(t)
	clock.advance(calibrationQuietBefore)
	r.tuneStep()
	candidate := r.FSCTRL0()
	for range cc1101.TuneWant {
		r.collectTune(tuneDataFrame("devic", 3))
	}
	r.tuneStep()
	select {
	case res := <-r.CalibrationResults():
		if res.FSCTRL0 != candidate {
			t.Fatalf("stored %#02x, want the candidate %#02x", res.FSCTRL0, candidate)
		}
	default:
		t.Fatal("no result for a candidate that settled")
	}
	c := r.Calibration()
	if c.State != CalibrationMeasured || c.Candidate {
		t.Fatalf("calibration %+v, want measured, no candidate", c)
	}
	if r.LongTXDeferred() {
		t.Fatal("still deferring on the register that is now the home value")
	}
	if writes := writesOf(br); len(writes) != 1 {
		t.Fatalf("writes %x, want only the scan step", writes)
	}
}

// A candidate whose rounds end outside the band is not kept and not stored: the
// register goes back to the stored value, the state is the one before the scan,
// and the long transmissions stay deferred until the write is done.
func TestCandidateSettlingOutsideTheBandGoesBackHome(t *testing.T) {
	br := newCarrierBridge(0x2f, 0)
	r, clock := scanLive(br)
	r.cal.state, r.cal.stored, r.cal.hasStored = CalibrationStored, 0x2f, true
	r.SetPlant(&testPlant{sources: []string{"devic"}})
	r.tuneStep()
	clock.advance(calibrationQuietBefore)
	r.tuneStep()
	r.collectTune(tuneDataFrame("devic", 15))
	for round := 1; round <= cc1101.TuneRounds+1; round++ {
		for range cc1101.TuneWant {
			r.collectTune(tuneDataFrame("devic", 15))
		}
		r.tuneStep()
	}
	if r.FSCTRL0() == 0x2f || !r.LongTXDeferred() {
		t.Fatalf("FSCTRL0 %#02x deferred=%v before the restore write, want the candidate register and the deferral", r.FSCTRL0(), r.LongTXDeferred())
	}
	r.tuneStep()
	if r.FSCTRL0() != 0x2f || r.LongTXDeferred() {
		t.Fatalf("FSCTRL0 %#02x deferred=%v, want home and no deferral", r.FSCTRL0(), r.LongTXDeferred())
	}
	if c := r.Calibration(); c.State != CalibrationStored || c.Candidate {
		t.Fatalf("calibration %+v, want stored, no candidate", c)
	}
	// The scan holds for an hour, as after a pass that decoded nothing.
	before := len(writesOf(br))
	clock.advance(calibrationScanHold - time.Second)
	r.tuneStep()
	if after := len(writesOf(br)); after != before {
		t.Fatalf("writes %d -> %d: a scan step started inside the hold", before, after)
	}
	select {
	case res := <-r.CalibrationResults():
		t.Fatalf("stored %+v for a candidate that did not settle", res)
	default:
	}
	if w := r.Warnings(); len(w) != 0 {
		t.Fatalf("warnings after the candidate ended: %q", w)
	}
}

// A candidate that no round settles in 30 minutes goes back home, whatever the
// frames did. The scan does not start again while it runs. It holds for an hour
// as after a pass that decoded nothing, and the long transmissions go out on the
// home register in the meantime.
func TestCandidateThatDoesNotSettleIn30MinutesGoesBackHome(t *testing.T) {
	r, br, clock := scanPlant(t)
	clock.advance(calibrationQuietBefore)
	r.tuneStep()
	r.collectTune(tuneDataFrame("devic", 7))
	writes := writesOf(br)
	clock.advance(calibrationCandidateMax - time.Second)
	r.tuneStep()
	if after := writesOf(br); !bytes.Equal(after, writes) || !r.Calibration().Candidate {
		t.Fatalf("writes %x -> %x, candidate %v inside 30 minutes", writes, after, r.Calibration().Candidate)
	}
	clock.advance(time.Second)
	r.tuneStep()
	if r.FSCTRL0() != cc1101.TuneDefault || r.LongTXDeferred() {
		t.Fatalf("FSCTRL0 %#02x deferred=%v after 30 minutes, want home", r.FSCTRL0(), r.LongTXDeferred())
	}
	if c := r.Calibration(); c.Candidate || c.State != CalibrationMeasuring || c.Readings != 0 {
		t.Fatalf("calibration %+v, want an empty measurement on the home register", c)
	}
	before := len(writesOf(br))
	clock.advance(calibrationQuietBefore)
	r.tuneStep()
	clock.advance(calibrationScanHold - calibrationQuietBefore - time.Second)
	r.tuneStep()
	if after := len(writesOf(br)); after != before || r.LongTXDeferred() {
		t.Fatalf("writes %d -> %d, deferred=%v: a scan step started inside the hold", before, after, r.LongTXDeferred())
	}
	clock.advance(time.Second)
	r.tuneStep()
	if after := len(writesOf(br)); after != before+1 {
		t.Fatalf("writes %d, want the scan to start again after the hold", after)
	}
}

// A pair window ends a candidate like it ends a scan: the beacons go out on the
// home register.
func TestPairWindowEndsTheCandidate(t *testing.T) {
	r, _, clock := scanPlant(t)
	r.br = &pairBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}}
	clock.advance(calibrationQuietBefore)
	r.tuneStep()
	r.collectTune(tuneDataFrame("devic", 7))
	req := livePairWindow{
		window: cc1101.PairWindow{Duration: 5 * time.Minute, Key: make([]byte, 16), Coordinator: make([]byte, 8), Count: 1},
		done:   make(chan livePairResult, 1),
	}
	r.finishPairWindow(req)
	if res := <-req.done; res.err != nil {
		t.Fatal(res.err)
	}
	r.tuneStep()
	if r.FSCTRL0() != cc1101.TuneDefault || r.Calibration().Candidate || r.LongTXDeferred() {
		t.Fatalf("FSCTRL0 %#02x candidate=%v deferred=%v, want the home register and no candidate", r.FSCTRL0(), r.Calibration().Candidate, r.LongTXDeferred())
	}
}

// A calibrated plant with one device that sends an uplink every 15 minutes, and
// a home register that is right. Model of the receiver, an assumption and not a
// measurement: a frame decodes when the register is within 30 units (48 kHz) of
// the carrier, and its FREQEST reads the offset, clipped at the FOC limit of 21
// units. The scan finds the device on a step next to home. Router Advertisements
// and bootstrap trains must never be allowed while the register is off home.
func TestSparseDeviceNeverLeavesLongTransmissionsOnAnOffHomeRegister(t *testing.T) {
	const home = 0x34
	br := newCarrierBridge(home, 0)
	clock := newStepClock()
	r := &Live{tuner: br, calResults: make(chan CalibrationResult, 1), clock: clock.read}
	r.fsctrl0.Store(uint32(home))
	r.cal.state, r.cal.stored, r.cal.hasStored = CalibrationStored, home, true
	r.cal.quietSince = clock.read()
	r.SetPlant(&testPlant{sources: []string{"devic"}})
	var atHome, deferred, offAllowed, heard int
	for minute := 0; minute < 360; minute++ {
		if minute%15 == 0 {
			offset := int(int8(home)) - int(int8(r.FSCTRL0()))
			if offset >= -30 && offset <= 30 {
				r.collectTune(tuneDataFrame("devic", max(min(offset, 21), -21)))
				heard++
			}
		}
		r.tuneStep()
		switch {
		case r.FSCTRL0() == home:
			atHome++
		case r.LongTXDeferred():
			deferred++
		default:
			offAllowed++
		}
		clock.advance(time.Minute)
	}
	t.Logf("uplinks heard %d; of 360 minutes: on home %d, off home deferred %d", heard, atHome, deferred)
	if offAllowed > 0 {
		t.Fatalf("%d of 360 minutes off the home register with long transmissions allowed", offAllowed)
	}
	if deferred == 0 {
		t.Fatal("the scan never left home: the model does not test the candidate")
	}
}

// Opening a pair window stops the scan and sends the register back to the home
// value, and no scan starts while the window is open or in the 10 minutes after.
func TestPairWindowStopsTheScanAndRestoresTheRegister(t *testing.T) {
	r, br, clock := scanPlant(t)
	r.br = &pairBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}}
	clock.advance(calibrationQuietBefore)
	r.tuneStep()
	if r.FSCTRL0() == cc1101.TuneDefault {
		t.Fatal("the scan did not start")
	}
	req := livePairWindow{
		window: cc1101.PairWindow{Duration: 5 * time.Minute, Key: make([]byte, 16), Coordinator: make([]byte, 8), Count: 1},
		done:   make(chan livePairResult, 1),
	}
	r.finishPairWindow(req)
	if res := <-req.done; res.err != nil {
		t.Fatal(res.err)
	}
	r.tuneStep()
	if got := r.FSCTRL0(); got != cc1101.TuneDefault {
		t.Fatalf("FSCTRL0 %#02x after the pair window opened, want the default", got)
	}
	if w := r.Warnings(); len(w) != 0 {
		t.Fatalf("warnings with the scan stopped: %q", w)
	}
	before := len(writesOf(br))
	clock.advance(5*time.Minute + calibrationQuietBefore - time.Second)
	r.tuneStep()
	if after := len(writesOf(br)); after != before {
		t.Fatalf("a scan step started before the silence that follows the window")
	}
	clock.advance(time.Second)
	r.tuneStep()
	if after := len(writesOf(br)); after != before+1 {
		t.Fatalf("writes %d, want one step after the window and the silence", after)
	}
}

// scanTXBridge is a bridge with a register that takes the transmissions of the
// airtime tests.
type scanTXBridge struct {
	*budgetBridge
	reg byte
}

func (b *scanTXBridge) FSCTRL0() byte { return b.reg }
func (b *scanTXBridge) SetFSCTRL0(v byte) error {
	b.reg = v
	return nil
}

// A scan step that holds another register moves the transmit carrier off the
// devices: no Router Advertisement burst and no bootstrap train goes out, the
// callers get a deferral, and an anchored job still goes. On the home register
// the same calls go out.
func TestScanDefersLongTransmissionsWhileOffTheHomeRegister(t *testing.T) {
	logs := captureLog(t)
	r, bridge, clock := newBudgetRadio(t)
	sb := &scanTXBridge{budgetBridge: bridge, reg: cc1101.TuneDefault}
	r.br, r.tuner = sb, sb
	r.fsctrl0.Store(uint32(cc1101.TuneDefault))
	r.SetPlant(&testPlant{sources: []string{"devic"}})
	r.tuneStep()
	clock.advance(calibrationQuietBefore)
	r.tuneStep()
	if !r.LongTXDeferred() {
		t.Fatal("not deferring on a scan step")
	}

	err := r.sendTrain(routerAdvertTestFrames())
	deferred := err
	if !errors.Is(err, errScanDeferred) || !errors.Is(err, cc1101.ErrAirtimeBudget) {
		t.Fatalf("router advertisement: err = %v, want a scan deferral", err)
	}
	frame := []byte{0x69, 0xec, 0x10, 0x06, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	if _, err := r.sendActions([]tado.Action{{CSLTx: true, HasCSLBatch: true, Bytes: frame}}); !errors.Is(err, errScanDeferred) {
		t.Fatalf("bootstrap fragment: err = %v, want a scan deferral", err)
	}
	if _, err := r.sendActions([]tado.Action{{CSLTx: true, CSLBootstrapAlways: true, Bytes: frame}}); !errors.Is(err, errScanDeferred) {
		t.Fatalf("bootstrap-always job: err = %v, want a scan deferral", err)
	}
	if _, err := r.sendActions([]tado.Action{{CSLTx: true, Bytes: frame}}); err != nil {
		t.Fatal(err)
	}
	if got := queuedBootstrap(bridge); !slices.Equal(got, []bool{false}) {
		t.Fatalf("anchored job bootstrap flags %v, want [false]", got)
	}
	if !strings.Contains(logs.String(), "router advertisement burst deferred while the FSCTRL0 scan") {
		t.Fatalf("log lacks the scan deferral: %q", logs.String())
	}
	r.noteBridgeErr(deferred)
	if a, _ := r.Airtime(); a.OverBudget {
		t.Fatal("a scan deferral set the over-budget flag")
	}

	r.pairWindowChanged(time.Minute)
	r.tuneStep()
	if r.LongTXDeferred() {
		t.Fatal("still deferring on the home register")
	}
	if err := r.sendTrain(routerAdvertTestFrames()); err != nil {
		t.Fatalf("router advertisement on the home register: %v", err)
	}
}

// failingCarrier refuses the register writes whose number is in fail (1-based),
// as a link that loses the request does. With landed set, a refused write still
// reaches the chip: the SCAL after it lost its reply.
type failingCarrier struct {
	*carrierBridge
	n      int
	fail   map[int]bool
	landed bool
}

func (b *failingCarrier) SetFSCTRL0(v byte) error {
	b.n++
	if !b.fail[b.n] {
		return b.carrierBridge.SetFSCTRL0(v)
	}
	if b.landed {
		_ = b.carrierBridge.SetFSCTRL0(v)
	}
	return fmt.Errorf("%w: reply lost", cc1101.ErrLinkFault)
}

// The write that sends the register back home after a failed pass can fail. The
// radio then holds the last scan register: it counts as off home, health says
// which register it holds, and the write is tried again until it works.
func TestFailedRestoreWriteAfterAPassIsRetriedUntilItWorks(t *testing.T) {
	br := newCarrierBridge(cc1101.TuneDefault, 0)
	list := scanRegisters(cc1101.TuneDefault)
	fb := &failingCarrier{carrierBridge: br, fail: map[int]bool{len(list) + 1: true, len(list) + 2: true}}
	clock := newStepClock()
	r := &Live{tuner: fb, calResults: make(chan CalibrationResult, 1), clock: clock.read}
	r.fsctrl0.Store(uint32(br.FSCTRL0()))
	r.startMeasureLocked()
	r.SetPlant(&testPlant{sources: []string{"devic"}})
	r.tuneStep()
	clock.advance(calibrationQuietBefore)
	for i := 0; i <= len(list); i++ {
		r.tuneStep()
		if i < len(list) {
			clock.advance(calibrationScanDwell)
		}
	}
	last := list[len(list)-1]
	if got := r.FSCTRL0(); got != last || !r.LongTXDeferred() {
		t.Fatalf("after the failed restore: FSCTRL0 %#02x deferred=%v, want the last scan register %#02x and the deferral", got, r.LongTXDeferred(), last)
	}
	w := strings.Join(r.Warnings(), "\n")
	for _, want := range []string{fmt.Sprintf("FSCTRL0 is %#02x, not the home value %#02x", last, cc1101.TuneDefault), "did not work"} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings lack %q: %q", want, w)
		}
	}
	if strings.Contains(w, "FSCTRL0 is back") {
		t.Errorf("warnings claim the register is home: %q", w)
	}

	tries := fb.n
	r.tuneStep()
	if fb.n != tries {
		t.Fatalf("the write was tried again inside %s", calibrationRestoreRetry)
	}
	clock.advance(calibrationRestoreRetry)
	r.tuneStep() // the second restore write fails too
	if fb.n != tries+1 || r.FSCTRL0() != last || !r.LongTXDeferred() {
		t.Fatalf("writes %d, FSCTRL0 %#02x deferred=%v after the second failure", fb.n, r.FSCTRL0(), r.LongTXDeferred())
	}
	clock.advance(calibrationRestoreRetry)
	r.tuneStep()
	if r.FSCTRL0() != cc1101.TuneDefault || r.LongTXDeferred() {
		t.Fatalf("FSCTRL0 %#02x deferred=%v after the third try, want home and no deferral", r.FSCTRL0(), r.LongTXDeferred())
	}
	if w := strings.Join(r.Warnings(), "\n"); strings.Contains(w, "did not work") {
		t.Errorf("warning about the restore write stays after it worked: %q", w)
	}
	clock.advance(calibrationRestoreRetry)
	r.tuneStep()
	if fb.n != tries+2 {
		t.Fatalf("writes %d, want no write after the register is home", fb.n)
	}
}

// Same for the restore that a pair window asks for: the window must not run on
// a scan register.
func TestFailedRestoreWriteAfterAPairWindowIsRetried(t *testing.T) {
	br := newCarrierBridge(cc1101.TuneDefault, 0)
	fb := &failingCarrier{carrierBridge: br, fail: map[int]bool{2: true}}
	clock := newStepClock()
	r := &Live{tuner: fb, calResults: make(chan CalibrationResult, 1), clock: clock.read}
	r.fsctrl0.Store(uint32(br.FSCTRL0()))
	r.startMeasureLocked()
	r.SetPlant(&testPlant{sources: []string{"devic"}})
	r.tuneStep()
	clock.advance(calibrationQuietBefore)
	r.tuneStep() // scan step 1
	r.pairWindowChanged(10 * time.Minute)
	r.tuneStep() // the restore write fails
	if r.FSCTRL0() == cc1101.TuneDefault || !r.LongTXDeferred() {
		t.Fatalf("FSCTRL0 %#02x deferred=%v, want the scan register and the deferral", r.FSCTRL0(), r.LongTXDeferred())
	}
	clock.advance(calibrationRestoreRetry)
	r.tuneStep()
	if r.FSCTRL0() != cc1101.TuneDefault || r.LongTXDeferred() {
		t.Fatalf("FSCTRL0 %#02x deferred=%v during the pair window, want home", r.FSCTRL0(), r.LongTXDeferred())
	}
}

// setFSCTRL0 writes the register, then runs SCAL. A SCAL whose reply the link
// lost leaves the new value in the chip, and the radio reads it back from the
// bridge instead of keeping the old one.
func TestRegisterWriteThatLosesItsSCALReplyKeepsTheChipValue(t *testing.T) {
	br := newCarrierBridge(cc1101.TuneDefault, 0)
	fb := &failingCarrier{carrierBridge: br, fail: map[int]bool{1: true}, landed: true}
	r, _ := scanLive(br)
	r.tuner = fb
	if err := r.setRegister(0x40); !errors.Is(err, cc1101.ErrLinkFault) {
		t.Fatalf("setRegister: %v, want the link fault", err)
	}
	if got := r.FSCTRL0(); got != 0x40 {
		t.Fatalf("FSCTRL0 %#02x, want the 0x40 the chip holds", got)
	}
}
