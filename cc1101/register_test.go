package cc1101

import (
	"errors"
	"testing"
	"time"
)

// simPort answers the requests Autotune sends: a stream of frames that all read
// one FREQEST value, the SPI transfers and the strobes. It records the FSCTRL0
// writes.
type simPort struct {
	fakePort
	freqEst byte
	fsctrl0 []byte
}

func (p *simPort) Write(data []byte) (int, error) {
	p.fakePort.Write(data)
	if len(data) < 5 || data[0] != reqMagic {
		return len(data), nil
	}
	switch data[1] {
	case cmdStream:
		payload := make([]byte, 20)
		payload[0], payload[1] = 0x69, 0xec
		copy(payload[11:16], "abcde")
		blob := append([]byte{byte(len(payload))}, payload...)
		blob = append(blob, 0x10, 0x80|0x20)
		x := p.freqEst
		for _, c := range blob {
			x ^= c
		}
		for range TuneWant {
			p.read = append(p.read, rspMagic, streamFrame, byte(len(blob)))
			p.read = append(p.read, blob...)
			p.read = append(p.read, p.freqEst, x)
		}
		p.read = append(p.read, rspMagic, streamEnd, 0x00)
	case cmdXfer:
		body := data[4 : len(data)-1]
		if len(body) == 2 && body[0] == FSCTRL0 {
			p.fsctrl0 = append(p.fsctrl0, body[1])
		}
		p.read = append(p.read, autoAckResponse(0, make([]byte, len(body)))...)
	case cmdStrobe:
		p.read = append(p.read, autoAckResponse(0, []byte{0x00})...)
	}
	return len(data), nil
}

// FSCTRL0 is a signed byte. From 0x70 a reading of +19 must not wrap to 0x83,
// which the chip reads as -125: the carrier would jump 376 kHz the wrong way.
func TestAutotuneSaturatesAtTheInt8Limit(t *testing.T) {
	p := &simPort{freqEst: 19}
	b := &Bridge{port: p, timeout: 50 * time.Millisecond, curFSCTRL0: 0x70}
	reg, _, err := b.Autotune()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.fsctrl0) == 0 {
		t.Fatal("no FSCTRL0 write")
	}
	for _, w := range p.fsctrl0 {
		if int8(w) < 0x70 {
			t.Fatalf("FSCTRL0 writes %x: %#02x wrapped below the start", p.fsctrl0, w)
		}
	}
	if reg != 0x7f {
		t.Fatalf("Autotune register %#02x, want 0x7f", reg)
	}
}

func TestTuneTargetSaturates(t *testing.T) {
	for _, tc := range []struct {
		reg  byte
		corr int
		want byte
	}{
		{0x10, -0x20, 0xf0},
		{0x7c, 10, 0x7f},
		{0x84, -10, 0x80},
		{0x34, -13, 0x27},
	} {
		if got := TuneTarget(tc.reg, tc.corr); got != tc.want {
			t.Errorf("TuneTarget(%#02x, %d) = %#02x, want %#02x", tc.reg, tc.corr, got, tc.want)
		}
	}
}

// regPort records the register writes and answers FSCAL1 with the scripted
// values, one per read, the last one for good.
type regPort struct {
	fakePort
	regs   map[byte]byte
	fscal1 []byte
	scal   int
}

func (p *regPort) Write(data []byte) (int, error) {
	p.fakePort.Write(data)
	var reply []byte
	switch data[1] {
	case cmdXfer:
		body := data[4 : len(data)-1]
		reply = make([]byte, len(body))
		switch {
		case len(body) == 2 && body[0]&readSingle != 0:
			if body[0]&^readSingle == FSCAL1 && len(p.fscal1) > 0 {
				reply[1] = p.fscal1[0]
				if len(p.fscal1) > 1 {
					p.fscal1 = p.fscal1[1:]
				}
			}
		case len(body) == 2:
			if p.regs == nil {
				p.regs = map[byte]byte{}
			}
			p.regs[body[0]] = body[1]
		}
	case cmdStrobe:
		if data[4] == SCAL {
			p.scal++
		}
		reply = []byte{0x0f}
	}
	p.read = append(p.read, autoAckResponse(0, reply)...)
	return len(data), nil
}

// Configure loads the default calibration and the firmware frame limit, and
// leaves the register the host tracks on the same value.
func TestConfigureLoadsTheTunedRegisters(t *testing.T) {
	p := &regPort{}
	b := &Bridge{port: p, timeout: time.Second}
	if err := b.Configure(); err != nil {
		t.Fatal(err)
	}
	if got := p.regs[FSCTRL0]; got != TuneDefault || b.FSCTRL0() != TuneDefault {
		t.Errorf("FSCTRL0 loaded %#02x, tracked %#02x, want %#02x", got, b.FSCTRL0(), TuneDefault)
	}
	if got := p.regs[PKTLEN]; got != maxTrainFrame {
		t.Errorf("PKTLEN %#02x, want the firmware frame limit %#02x", got, maxTrainFrame)
	}
}

// FSCAL1 reads 0x3F when the calibration did not lock. A second SCAL is tried,
// and the error follows after the third.
func TestCalibrateSynthChecksTheLock(t *testing.T) {
	p := &regPort{fscal1: []byte{0x3f, 0x20}}
	b := &Bridge{port: p, timeout: time.Second}
	if err := b.CalibrateSynth(); err != nil || p.scal != 2 {
		t.Fatalf("err=%v after %d SCAL, want a lock on the second", err, p.scal)
	}
	p = &regPort{fscal1: []byte{0x3f}}
	b = &Bridge{port: p, timeout: time.Second}
	if err := b.CalibrateSynth(); !errors.Is(err, ErrNoSynthLock) || p.scal != synthCalAttempts {
		t.Fatalf("err=%v after %d SCAL, want ErrNoSynthLock after %d", err, p.scal, synthCalAttempts)
	}
}

// A register change that finds no lock still leaves the register written and
// tracked, so the caller knows what the chip holds.
func TestSetFSCTRL0ReportsNoLockAfterWriting(t *testing.T) {
	p := &regPort{fscal1: []byte{0x3f}}
	b := &Bridge{port: p, timeout: time.Second}
	if err := b.SetFSCTRL0(0x2f); !errors.Is(err, ErrNoSynthLock) {
		t.Fatalf("err = %v", err)
	}
	if p.regs[FSCTRL0] != 0x2f || b.FSCTRL0() != 0x2f {
		t.Fatalf("written %#02x, tracked %#02x, want 0x2f", p.regs[FSCTRL0], b.FSCTRL0())
	}
}

// A chip whose synthesizer does not lock after its retries cannot hold the
// carrier. Configure fails and never sends CMD_CONFIGURED, so the firmware keeps
// refusing every transmit command.
func TestConfigureWithoutSynthLockFailsAndStaysUnconfigured(t *testing.T) {
	p := &regPort{fscal1: []byte{0x3f}}
	b := &Bridge{port: p, timeout: time.Second}
	err := b.Configure()
	if !errors.Is(err, ErrNoSynthLock) {
		t.Fatalf("Configure err = %v, want ErrNoSynthLock", err)
	}
	if p.scal != synthCalAttempts {
		t.Fatalf("%d SCAL strobes, want %d retries", p.scal, synthCalAttempts)
	}
	for _, w := range p.writes {
		if w[1] == cmdConfigured {
			t.Fatalf("CONFIGURED sent to a chip without lock: %x", w)
		}
	}
}

// The airtime preload goes out once, after the register load and before
// CMD_CONFIGURED: the load overwrites every bucket, so a later one would erase
// the time on air of the session.
func TestConfigurePreloadsAirtimeBeforeConfigured(t *testing.T) {
	p := &firmwarePort{}
	b := &Bridge{port: p, timeout: time.Second}
	var load [AirtimeBuckets]uint16
	load[60] = 777
	if err := b.Configure(WithAirtime(load)); err != nil {
		t.Fatal(err)
	}
	var airtime []int
	for i, cmd := range p.cmds {
		if cmd == cmdAirtime {
			airtime = append(airtime, i)
		}
	}
	last := len(p.cmds) - 1
	if len(airtime) != 1 || airtime[0] != last-1 || p.cmds[last] != cmdConfigured {
		t.Fatalf("commands %x: want one AIRTIME right before the final CONFIGURED", p.cmds)
	}
	var req []byte
	for _, w := range p.writes {
		if w[1] == cmdAirtime {
			req = w
		}
	}
	if payload := req[4 : len(req)-1]; len(payload) != 2*AirtimeBuckets || payload[120] != 0x03 || payload[121] != 0x09 {
		t.Fatalf("preload payload %x: want 122 bytes ending 0309", payload)
	}
}
