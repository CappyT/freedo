package cc1101

import (
	"bytes"
	"testing"
	"time"
)

// A late reply of another request in front of the reply must not fail the
// request: AUTOACK and TXTRAIN read past it to their own reply.
func TestRequestsSkipLateReplyOfAnotherRequest(t *testing.T) {
	coordinator := []byte{0x09, 0xa1, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00}
	late := autoAckResponse(0, []byte{1}) // an enqueue admission

	t.Run("autoack", func(t *testing.T) {
		p := &fakePort{read: append(append([]byte(nil), late...), autoAckResponse(autoAckNoFrame, nil)...)}
		b := &Bridge{port: p, timeout: time.Millisecond}
		got, err := b.RecvAutoAck(250*time.Millisecond, coordinator, 0)
		if err != nil || got.Status != AutoAckNoFrame {
			t.Fatalf("result=%+v err=%v, want its own no-frame reply", got, err)
		}
	})
	t.Run("txtrain", func(t *testing.T) {
		p := &fakePort{read: append(autoAckResponse(0, nil), autoAckResponse(0, []byte{2})...)}
		b := &Bridge{port: p, timeout: time.Millisecond}
		sent, err := b.SendTrain([][]byte{{0x25, 0x01}, {0x25, 0x02}}, time.Second)
		if err != nil || sent != 2 {
			t.Fatalf("sent=%d err=%v, want its own reply", sent, err)
		}
	})
}

// firmwarePort answers each request the way the v2 firmware does and records
// the commands in request order. IOCFG0 reads iocfg0.
type firmwarePort struct {
	fakePort
	cmds   []byte
	subs   []byte
	iocfg0 byte
}

func (p *firmwarePort) Write(data []byte) (int, error) {
	p.fakePort.Write(data)
	cmd, payload := data[1], data[4:]
	p.cmds = append(p.cmds, cmd)
	sub := byte(0)
	if len(payload) > 0 {
		sub = payload[0]
	}
	p.subs = append(p.subs, sub)
	var reply []byte
	switch cmd {
	case cmdXfer:
		reply = append([]byte(nil), payload...)
		reply[0] = 0x0f
		if len(payload) == 2 && payload[0] == IOCFG0|readSingle {
			reply[1] = p.iocfg0
		}
	case cmdStrobe:
		reply = []byte{0x0f}
	case cmdPing:
		reply = []byte("CC1101BRIDGE v2.5")
	}
	p.read = append(p.read, autoAckResponse(0, reply)...)
	return len(data), nil
}

// Jobs a crashed session left in the bridge queue must not transmit while
// Configure rewrites the chip: the flush goes out before the reset.
func TestConfigureFlushesCSLQueueFirst(t *testing.T) {
	p := &firmwarePort{}
	b := &Bridge{port: p, timeout: time.Second}
	if err := b.Configure(); err != nil {
		t.Fatal(err)
	}
	if len(p.cmds) < 2 || p.cmds[0] != cmdCSLQueue || p.subs[0] != cslQueueFlush || p.cmds[1] != cmdReset {
		t.Fatalf("first commands=%x subs=%x, want CSLQUEUE FLUSH then RESET", p.cmds[:min(len(p.cmds), 4)], p.subs[:min(len(p.subs), 4)])
	}
}

// IOCFG0 tells the register set Configure loaded from the reset value the
// firmware writes after its own reboot.
func TestConfiguredReadsIOCFG0(t *testing.T) {
	for _, tc := range []struct {
		iocfg0 byte
		want   bool
	}{{0x07, true}, {0x2f, false}, {0x3f, false}} {
		p := &firmwarePort{iocfg0: tc.iocfg0}
		b := &Bridge{port: p, timeout: time.Second}
		got, err := b.Configured()
		if err != nil || got != tc.want {
			t.Fatalf("IOCFG0 %#02x: configured=%t err=%v, want %t", tc.iocfg0, got, err, tc.want)
		}
		if want := []byte{reqMagic, cmdXfer, 0, 2, IOCFG0 | readSingle, 0}; !bytes.Equal(p.writes[0], want) {
			t.Fatalf("request=%x, want %x", p.writes[0], want)
		}
	}
}

// busyPort never goes quiet, like a stream the abort byte did not stop.
type busyPort struct{ fakePort }

func (p *busyPort) Read(dst []byte) (int, error) {
	time.Sleep(time.Millisecond)
	dst[0] = 0x5a
	return 1, nil
}

func TestDrainGivesUpOnBusyPort(t *testing.T) {
	b := &Bridge{port: &busyPort{}, timeout: time.Second}
	start := time.Now()
	if err := b.drain(5*time.Millisecond, 50*time.Millisecond); err == nil {
		t.Fatal("drain returned no error on a port that never went quiet")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("drain ran for %s", elapsed)
	}
}
