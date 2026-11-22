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

// firmwareHostTimeout is the silence after which the firmware drops its
// coordinator, its queue and its pair window.
const firmwareHostTimeout = 5 * time.Second

// A request whose reply is lost holds the host silent until its reply deadline.
// The deadline of a request that has no window of its own, and the one of the
// 250 ms receive window of the live loop, leave a wide margin under the firmware
// host timeout, so the timeout never fires while the daemon is alive.
func TestLostReplyKeepsTheHostSilentUnderTheFirmwareTimeout(t *testing.T) {
	const bound = 1500 * time.Millisecond
	for name, call := range map[string]func(*Bridge) error{
		"airtime": func(b *Bridge) error { _, err := b.Airtime(); return err },
		"autoack": func(b *Bridge) error {
			_, err := b.RecvAutoAck(250*time.Millisecond, make([]byte, 8), 0)
			return err
		},
		// The idle register check: the loop sends it with no job out.
		"configured": func(b *Bridge) error { _, err := b.Configured(); return err },
		"recv":       func(b *Bridge) error { _, err := b.RecvFW(250 * time.Millisecond); return err },
		"strobe":     func(b *Bridge) error { _, err := b.Strobe(SCAL); return err },
		"xfer":       func(b *Bridge) error { _, err := b.Xfer([]byte{0x80, 0}); return err },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, err := start(firmwareAt(DefaultBaud, "CC1101BRIDGE v2.11 (test)"), 0)
			if err != nil {
				t.Fatal(err)
			}
			b.port = &linkPort{} // from here on the firmware never answers
			begin := time.Now()
			err = call(b)
			silence := time.Since(begin)
			if !errors.Is(err, ErrLinkFault) {
				t.Fatalf("err = %v, want a link fault", err)
			}
			if silence > bound {
				t.Fatalf("a lost reply keeps the host silent for %s, want at most %s (firmware host timeout %s)", silence.Round(10*time.Millisecond), bound, firmwareHostTimeout)
			}
		})
	}
}

// The firmware runs a command that needs the radio when the CSL transaction in
// progress has ended, and the requests behind it wait in the same queue. A
// transaction lasts up to 3.9 s. The live loop can send PAIRWIN, a Router
// Advertisement burst, a train and the register check after an expired job with a job out, so these
// wait longer than the 1.25 s of an ordinary request, and a late reply is no
// link fault. Here the firmware answers 2.5 s after the request, as it does
// behind a transaction.
func TestCommandsThatNeedTheRadioWaitBehindATransaction(t *testing.T) {
	ra := make([]byte, 40)
	ra[0], ra[1] = 0x49, 0xe8
	for _, tc := range []struct {
		name  string
		cmd   byte
		reply []byte
		call  func(*Bridge) error
	}{
		{"pair window", cmdPairWindow, autoAckResponse(0, []byte{0}), func(b *Bridge) error {
			_, err := b.SetPairWindow(PairWindow{Duration: time.Minute, Key: make([]byte, 16), Coordinator: make([]byte, 8), Count: 1})
			return err
		}},
		{"router advertisement burst", cmdRABurst, autoAckResponse(0, []byte{20}), func(b *Bridge) error {
			_, err := b.SendRouterAdvertBurst(ra, time.Second)
			return err
		}},
		{"train", cmdTxTrain, autoAckResponse(0, []byte{1}), func(b *Bridge) error {
			_, err := b.SendTrain([][]byte{make([]byte, 12)}, time.Second)
			return err
		}},
		{"register check after a job", cmdXfer, autoAckResponse(0, []byte{0, 0x07}), func(b *Bridge) error {
			_, err := b.ConfiguredBehindJob()
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &linkPort{reply: func(p *linkPort, req []byte) {
				if req[1] != tc.cmd {
					return
				}
				go func() {
					time.Sleep(2500 * time.Millisecond)
					p.feed(tc.reply)
				}()
			}}
			b := &Bridge{port: p, timeout: requestWindow}
			if err := tc.call(b); err != nil {
				t.Fatalf("a reply 2.5 s after the request failed: %v", err)
			}
			if b.SerialErrors() != 0 || len(b.owed) != 0 {
				t.Fatalf("serialErrors=%d owed=%d, want none", b.SerialErrors(), len(b.owed))
			}
		})
	}
}

// The idle step sends XFER and STROBE with no job out, where no transaction can
// hold them back. A lost reply keeps the short window, so that two lost requests
// in a row cannot pass the 5 s of the firmware host timeout.
func TestIdleXferAndStrobeKeepTheShortWindow(t *testing.T) {
	for name, call := range map[string]func(*Bridge) error{
		"strobe": func(b *Bridge) error { _, err := b.Strobe(SCAL); return err },
		"xfer":   func(b *Bridge) error { _, err := b.Xfer([]byte{0x80, 0}); return err },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := &Bridge{port: &linkPort{reply: func(p *linkPort, req []byte) {}}, timeout: requestWindow}
			begin := time.Now()
			err := call(b)
			if silent := time.Since(begin); !errors.Is(err, ErrLinkFault) || silent > 2*time.Second {
				t.Fatalf("err=%v after %s, want a link fault after about 1.25 s", err, silent)
			}
		})
	}
}

// An ordinary request keeps its 1.25 s window: the keepalive rule of the idle
// loop depends on it.
func TestOrdinaryRequestKeepsItsShortWindow(t *testing.T) {
	p := &linkPort{reply: func(p *linkPort, req []byte) {}}
	b := &Bridge{port: p, timeout: requestWindow}
	begin := time.Now()
	_, err := b.Airtime()
	if !errors.Is(err, ErrLinkFault) {
		t.Fatalf("err=%v, want a link fault for a request that gets no reply", err)
	}
	if elapsed := time.Since(begin); elapsed > 2*time.Second {
		t.Fatalf("an ordinary request waited %s, want about 1.25 s", elapsed)
	}
}
