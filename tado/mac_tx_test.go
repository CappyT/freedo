package tado

import (
	"encoding/binary"
	"io"
	"testing"
	"time"
)

func nextAttempt(t *testing.T, e *EmuState, target string) *Frame {
	t.Helper()
	actions, ok, err := e.NextQueuedDownlink(target)
	if err != nil || !ok || len(actions) != 1 {
		t.Fatalf("attempt actions=%d ok=%t err=%v", len(actions), ok, err)
	}
	f, err := ParseData(NetworkKey, actions[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func frameCounter(f *Frame) uint32 { return binary.LittleEndian.Uint32(f.Plaintext[4:8]) }

// A downlink the Enhanced ACK delivers retries as a MAC retransmission: the
// commercial coordinator sends it again under the same MAC seq and the next
// frame counter.
func TestMACRetransmissionKeepsSeq(t *testing.T) {
	e, _, _ := newTestEmu(t)
	if err := e.EnqueueChildLock("bedroom", 10, true); err != nil {
		t.Fatal(err)
	}
	first := nextAttempt(t, e, "bedroom")
	retry := nextAttempt(t, e, "bedroom")
	if retry.Seq != first.Seq {
		t.Errorf("retry seq=%02x, want %02x", retry.Seq, first.Seq)
	}
	if frameCounter(retry) == frameCounter(first) {
		t.Errorf("retry reused frame counter %d", frameCounter(first))
	}
	e.confirmBySeq(first.DstEUI, retry.Seq)
	if n := e.Pending("bedroom"); n != 0 {
		t.Errorf("pending=%d after the Enhanced ACK of the retry, want 0", n)
	}
}

// A downlink that waits for a CoAP answer retries as a new frame.
func TestCoAPTransactionRetryTakesNewSeq(t *testing.T) {
	e, _, _ := newTestEmu(t)
	if err := e.EnqueueIdentify("bedroom", 10); err != nil {
		t.Fatal(err)
	}
	first := nextAttempt(t, e, "bedroom")
	retry := nextAttempt(t, e, "bedroom")
	if retry.Seq == first.Seq {
		t.Errorf("CoAP retry kept seq %02x", first.Seq)
	}
}

// Frame Pending tells the device that another frame for it follows. It is set
// while another downlink waits behind the one on the air, and not on the last.
func TestFramePendingOnlyWhileMoreQueued(t *testing.T) {
	e, _, _ := newTestEmu(t)
	for _, on := range []bool{true, false} {
		if err := e.EnqueueChildLock("bedroom", 10, on); err != nil {
			t.Fatal(err)
		}
	}
	first := nextAttempt(t, e, "bedroom")
	if first.raw[0] != fcfDataPending[0] {
		t.Fatalf("first FCF=%02x, want %02x with a second downlink queued", first.raw[0], fcfDataPending[0])
	}
	e.confirmBySeq(first.DstEUI, first.Seq)
	second := nextAttempt(t, e, "bedroom")
	if second.raw[0] != fcfData[0] {
		t.Fatalf("last FCF=%02x, want %02x", second.raw[0], fcfData[0])
	}
}

func TestCoAPReplyFramePendingOnlyWithQueuedDownlink(t *testing.T) {
	e, _, _ := newTestEmu(t)
	wire, label, err := e.resolve("bedroom")
	if err != nil {
		t.Fatal(err)
	}
	pkt := []byte{0x7e, 0x33, 0xf0, 0x60, 0x44, 0x12, 0x34}
	for _, tc := range []struct {
		queued bool
		want   byte
	}{{false, fcfData[0]}, {true, fcfDataPending[0]}} {
		if tc.queued {
			if err := e.EnqueueChildLock("bedroom", 10, true); err != nil {
				t.Fatal(err)
			}
		}
		actions, err := e.coapReplyActions(pkt, wire, "2.04", "z/p", 0x1234, label)
		if err != nil || len(actions) != 1 {
			t.Fatalf("reply actions=%d err=%v", len(actions), err)
		}
		if got := actions[0].Bytes[0]; got != tc.want {
			t.Errorf("queued=%t FCF=%02x, want %02x", tc.queued, got, tc.want)
		}
	}
}

// The Router Advertisement takes the unicast frame counter, as on the
// commercial coordinator.
func TestBeaconSharesUnicastFrameCounter(t *testing.T) {
	e, _, _ := newTestEmu(t)
	if err := e.EnqueueChildLock("bedroom", 10, true); err != nil {
		t.Fatal(err)
	}
	down := nextAttempt(t, e, "bedroom")
	a, err := e.Beacon()
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseBroadcast(NetworkKey, a.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if m.FrameCounter != frameCounter(down)+1 {
		t.Errorf("broadcast counter=%d, want %d", m.FrameCounter, frameCounter(down)+1)
	}
	if e.TxCounter() != m.FrameCounter {
		t.Errorf("unicast counter=%d, want %d after the broadcast", e.TxCounter(), m.FrameCounter)
	}
}

// A restart continues the interval of the burst an earlier process sent: it is
// not due at once, it is due when the stored interval ends, and a stored instant
// or interval that cannot hold (a clock set back, a shorter --beacon) is cut.
func TestRestoreRouterAdvertContinuesTheInterval(t *testing.T) {
	now := time.Unix(1000, 0)
	newEmu := func() *EmuState {
		return NewEmu(EmuConfig{Now: func() time.Time { return now }, RouterAdvertInterval: 600 * time.Second},
			NewUnhandledLogger(io.Discard))
	}
	due := func(e *EmuState) bool {
		_, due, err := e.MaybeBeacon()
		if err != nil {
			t.Fatal(err)
		}
		e.CompleteBeacon(false)
		return due
	}
	tests := []struct {
		name   string
		last   time.Time
		next   time.Duration
		dueAt  time.Duration // after now
		notDue time.Duration // negative: due at once
	}{
		{"interval in progress", now.Add(-100 * time.Second), 300 * time.Second, 200 * time.Second, 199 * time.Second},
		{"interval over", now.Add(-400 * time.Second), 300 * time.Second, 0, -1},
		{"clock set back", now.Add(time.Hour), 300 * time.Second, 300 * time.Second, 299 * time.Second},
		{"shorter beacon option", now, time.Hour, 600 * time.Second, 599 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now = time.Unix(1000, 0)
			e := newEmu()
			e.RestoreRouterAdvert(tt.last, tt.next)
			if tt.notDue >= 0 {
				now = now.Add(tt.notDue)
				if due(e) {
					t.Fatalf("burst due %s after the restore, want it later", tt.notDue)
				}
			}
			now = time.Unix(1000, 0).Add(tt.dueAt)
			if !due(e) {
				t.Fatalf("burst not due %s after the restore", tt.dueAt)
			}
		})
	}
	t.Run("nothing restored", func(t *testing.T) {
		now = time.Unix(1000, 0)
		if !due(newEmu()) {
			t.Fatal("a first start sends no burst")
		}
	})
}

// CompleteBeacon says whether a burst went on air, so the daemon stores the
// instant of a real burst only.
func TestCompleteBeaconReportsABurstOnAir(t *testing.T) {
	e := NewEmu(EmuConfig{RouterAdvertInterval: 600 * time.Second}, NewUnhandledLogger(io.Discard))
	if e.CompleteBeacon(true) {
		t.Fatal("CompleteBeacon reported a burst that was never built")
	}
	if _, due, err := e.MaybeBeacon(); err != nil || !due {
		t.Fatalf("due=%v err=%v", due, err)
	}
	if e.CompleteBeacon(false) {
		t.Fatal("CompleteBeacon reported a burst that stayed off the air")
	}
	if _, _, ok := e.RouterAdvertState(); ok {
		t.Fatal("a burst that stayed off the air moved the interval")
	}
	if _, due, err := e.MaybeBeacon(); err != nil || !due {
		t.Fatalf("due=%v err=%v", due, err)
	}
	if !e.CompleteBeacon(true) {
		t.Fatal("CompleteBeacon did not report a burst on air")
	}
	if _, next, ok := e.RouterAdvertState(); !ok || next < 200*time.Second || next > 600*time.Second {
		t.Fatalf("next=%s ok=%v, want an interval of 200 to 600 s", next, ok)
	}
}

// Each periodic advertisement follows the last one after a random time
// between a third of the interval and all of it (RFC 4861, Contiki 200-600 s).
func TestRouterAdvertIntervalIsRandom(t *testing.T) {
	now := time.Unix(1000, 0)
	e := NewEmu(EmuConfig{Now: func() time.Time { return now }, RouterAdvertInterval: 600 * time.Second},
		NewUnhandledLogger(io.Discard))
	if _, due, err := e.MaybeBeacon(); err != nil || !due {
		t.Fatalf("first beacon due=%t err=%v", due, err)
	}
	e.CompleteBeacon(true)
	now = now.Add(199 * time.Second)
	if _, due, _ := e.MaybeBeacon(); due {
		t.Fatal("beacon due before a third of the interval")
	}
	now = now.Add(401 * time.Second)
	if _, due, _ := e.MaybeBeacon(); !due {
		t.Fatal("beacon not due at the whole interval")
	}
	lo, hi := time.Hour, time.Duration(0)
	for range 2000 {
		d := routerAdvertDelay(600 * time.Second)
		if d < 200*time.Second || d > 600*time.Second {
			t.Fatalf("delay %s outside 200..600 s", d)
		}
		lo, hi = min(lo, d), max(hi, d)
	}
	if lo > 250*time.Second || hi < 550*time.Second {
		t.Errorf("delays span %s..%s, want them spread over 200..600 s", lo, hi)
	}
}
