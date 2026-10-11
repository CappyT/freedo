package cc1101

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.bug.st/serial"
)

// linkPort is a serial port with a scripted firmware behind it. An idle read
// takes a few milliseconds and returns no byte, like the read timeout of a real
// port, so a reply that stops short ends by its stall and not by an EOF.
type linkPort struct {
	mu    sync.Mutex
	rx    []byte
	baud  int
	reply func(p *linkPort, request []byte)
}

func (p *linkPort) feed(data []byte) {
	p.mu.Lock()
	p.rx = append(p.rx, data...)
	p.mu.Unlock()
}

func (p *linkPort) Read(dst []byte) (int, error) {
	p.mu.Lock()
	n := copy(dst, p.rx)
	p.rx = p.rx[n:]
	p.mu.Unlock()
	if n == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	return n, nil
}

func (p *linkPort) Write(data []byte) (int, error) {
	if len(data) >= 4 && data[0] == reqMagic && p.reply != nil {
		p.reply(p, data)
	}
	return len(data), nil
}

func (p *linkPort) SetMode(m *serial.Mode) error {
	p.mu.Lock()
	p.baud = m.BaudRate
	p.mu.Unlock()
	return nil
}

func (p *linkPort) currentBaud() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.baud
}

func (p *linkPort) Drain() error                                         { return nil }
func (p *linkPort) ResetInputBuffer() error                              { return nil }
func (p *linkPort) ResetOutputBuffer() error                             { return nil }
func (p *linkPort) SetDTR(bool) error                                    { return nil }
func (p *linkPort) SetRTS(bool) error                                    { return nil }
func (p *linkPort) GetModemStatusBits() (*serial.ModemStatusBits, error) { return nil, nil }
func (p *linkPort) SetReadTimeout(time.Duration) error                   { return nil }
func (p *linkPort) Close() error                                         { return nil }
func (p *linkPort) Break(time.Duration) error                            { return nil }

// A reply of an event that stops after n bytes is discarded, and the next poll
// reads its own reply. The lost event stays lost and the counter shows it.
func TestTruncatedReplyDoesNotDesyncTheNextRequest(t *testing.T) {
	event := make([]byte, 100)
	event[0] = 1
	full := autoAckResponse(0, event)
	for _, cut := range []int{1, 2, 4, 5, 60, len(full) - 1} {
		t.Run(fmt.Sprintf("cut%d", cut), func(t *testing.T) {
			t.Parallel()
			polls := 0
			p := &linkPort{reply: func(p *linkPort, _ []byte) {
				polls++
				if polls == 1 {
					p.feed(full[:cut])
					return
				}
				p.feed(autoAckResponse(0, nil))
			}}
			b := &Bridge{port: p, timeout: time.Second}
			started := time.Now()
			_, err := b.PollCSLQueueWithin(2 * time.Second)
			if !errors.Is(err, ErrLinkFault) {
				t.Fatalf("truncated poll: %v, want a link fault", err)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("truncated poll took %s, want the stall bound and not the deadline", elapsed)
			}
			if len(b.owed) != 0 {
				t.Fatalf("owed=%d replies, a truncated reply is not owed", len(b.owed))
			}
			event, err := b.PollCSLQueueWithin(2 * time.Second)
			if err != nil || event != nil {
				t.Fatalf("next poll: event=%+v err=%v, want its own empty reply", event, err)
			}
			if got := b.SerialErrors(); got != 1 {
				t.Fatalf("serial errors=%d, want 1", got)
			}
		})
	}
}

// A length past the payload limit is a false frame start: the bytes are
// discarded and the next request is clean.
func TestReplyWithImpossibleLengthIsALinkFault(t *testing.T) {
	polls := 0
	p := &linkPort{reply: func(p *linkPort, _ []byte) {
		polls++
		if polls == 1 {
			p.feed([]byte{rspMagic, 0, 0xff, 0xff, 1, 2, 3})
			return
		}
		p.feed(autoAckResponse(0, nil))
	}}
	b := &Bridge{port: p, timeout: time.Second}
	if _, err := b.PollCSLQueueWithin(2 * time.Second); !errors.Is(err, ErrLinkFault) {
		t.Fatalf("poll: %v, want a link fault", err)
	}
	if event, err := b.PollCSLQueueWithin(2 * time.Second); err != nil || event != nil {
		t.Fatalf("next poll: event=%+v err=%v", event, err)
	}
	if got := b.SerialErrors(); got != 1 {
		t.Fatalf("serial errors=%d, want 1", got)
	}
}

// A bridge that stays silent leaves its reply owed, as before, and the miss
// counts as a serial error.
func TestSilentBridgeCountsASerialError(t *testing.T) {
	b := &Bridge{port: &linkPort{}, timeout: time.Millisecond}
	if _, err := b.PollCSLQueueWithin(20 * time.Millisecond); !errors.Is(err, ErrLinkFault) || !errors.Is(err, ErrCSLPollTimeout) {
		t.Fatalf("poll: %v, want a link fault and a poll timeout", err)
	}
	if len(b.owed) != 1 || b.SerialErrors() != 1 {
		t.Fatalf("owed=%d serial errors=%d, want 1 and 1", len(b.owed), b.SerialErrors())
	}
}
