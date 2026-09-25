package cc1101

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// TestSetPairWindow pins the CMD_PAIRWIN request of firmware v2.9 and its
// reply: the number of Enhanced Beacons the window that the request ends sent.
func TestSetPairWindow(t *testing.T) {
	key := []byte("0123456789abcdef")
	coordinator := []byte{0x09, 0xa1, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00}

	p := &fakePort{read: autoAckResponse(0x00, []byte{0})}
	b := &Bridge{port: p, timeout: time.Millisecond}
	sent, err := b.SetPairWindow(PairWindow{
		Duration: 2 * time.Minute, Key: key, Coordinator: coordinator,
		Seq: 0x59, Counter: 0x01020304, Count: 64,
	})
	if err != nil || sent != 0 {
		t.Fatalf("open: sent=%d err=%v", sent, err)
	}
	want := []byte{reqMagic, cmdPairWindow, 0x00, 0x20, 0x00, 0x78}
	want = append(want, key...)
	want = append(want, coordinator...)
	want = append(want, 0x59, 0x04, 0x03, 0x02, 0x01, 0x40)
	if len(p.writes) != 1 || !bytes.Equal(p.writes[0], want) {
		t.Fatalf("open request=%x, want %x", p.writes, want)
	}

	p = &fakePort{read: autoAckResponse(0x00, []byte{3})}
	b = &Bridge{port: p, timeout: time.Millisecond}
	if sent, err := b.SetPairWindow(PairWindow{}); err != nil || sent != 3 {
		t.Fatalf("close: sent=%d err=%v, want 3", sent, err)
	}
	if want := []byte{reqMagic, cmdPairWindow, 0x00, 0x02, 0x00, 0x00}; !bytes.Equal(p.writes[0], want) {
		t.Fatalf("close request=%x, want %x", p.writes[0], want)
	}

	// Firmware before v2.9 does not know the command.
	b = &Bridge{port: &fakePort{read: autoAckResponse(0xe0, nil)}, timeout: time.Millisecond}
	if _, err := b.SetPairWindow(PairWindow{}); !errors.Is(err, ErrPairWindowUnsupported) {
		t.Fatalf("v2.8 firmware: %v, want ErrPairWindowUnsupported", err)
	}

	for _, w := range []PairWindow{
		{Duration: time.Minute, Key: key[:15], Coordinator: coordinator, Count: 1},
		{Duration: time.Minute, Key: key, Coordinator: coordinator[:7], Count: 1},
		{Duration: time.Minute, Key: key, Coordinator: coordinator},
		{Duration: 1000 * time.Minute, Key: key, Coordinator: coordinator, Count: 1},
	} {
		p := &fakePort{}
		b := &Bridge{port: p, timeout: time.Millisecond}
		if _, err := b.SetPairWindow(w); err == nil || len(p.writes) != 0 {
			t.Errorf("invalid window %+v accepted", w)
		}
	}
}

// TestSendRouterAdvertBurstFramePending: the first advertisement of a pair
// window carries Frame Pending (FCF 59e8), which firmware v2.9 accepts.
func TestSendRouterAdvertBurstFramePending(t *testing.T) {
	p := &fakePort{read: autoAckResponse(0x00, []byte{254})}
	b := &Bridge{port: p, timeout: time.Millisecond}
	if sent, err := b.SendRouterAdvertBurst([]byte{0x59, 0xe8, 0x42}, time.Second); err != nil || sent != 254 {
		t.Fatalf("sent=%d err=%v", sent, err)
	}
	if want := []byte{reqMagic, cmdRABurst, 0x00, 0x03, 0x59, 0xe8, 0x42}; !bytes.Equal(p.writes[0], want) {
		t.Fatalf("request=%x, want %x", p.writes[0], want)
	}
}
