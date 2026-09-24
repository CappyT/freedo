// Package radio holds the frame sources and sinks the daemon engine drives: the
// live CC1101 bridge, a capture replay and an in-memory mock.
package radio

import (
	"bufio"
	"errors"
	"log"
	"os"
	"strings"
	"sync"

	"freedo/internal/cli"
)

var (
	ErrStopped        = errors.New("daemon stopped")
	ErrTXBlocked      = errors.New("live tx blocked after bridge detection")
	errLiveBusy       = errors.New("live tx busy handling receive batch")
	errLiveEnded      = errors.New("live radio owner ended")
	errCSLPollStalled = errors.New("CSL queue poll stalled")
	// errCSLCompletionLost fails a queued CSL job the bridge never completed.
	errCSLCompletionLost = errors.New("CSL queue completion lost")
	// errBridgeUnconfigured ends the owner of a chip on reset defaults.
	errBridgeUnconfigured = errors.New("CC1101 lost its register set: the bridge or the chip reset")
)

// Radio is the frame source and sink the engine drives.
type Radio interface {
	Frames() <-chan []byte
	Send(frame []byte) error
	Close() error
}

// Replay feeds a capture file, then closes the frame channel.
type Replay struct {
	frames chan []byte
	stop   chan struct{}
	once   sync.Once
}

func NewReplay(path string) (*Replay, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := &Replay{frames: make(chan []byte, 64), stop: make(chan struct{})}
	go r.feed(f)
	return r, nil
}

func (r *Replay) feed(f *os.File) {
	defer f.Close()
	defer close(r.frames)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<16)
	var frames, skipped int
	for sc.Scan() {
		fr, ok := cli.ReplayFrame(sc.Text())
		if !ok {
			if strings.TrimSpace(sc.Text()) != "" {
				skipped++
			}
			continue
		}
		frames++
		select {
		case r.frames <- fr:
		case <-r.stop:
			return
		}
	}
	if err := sc.Err(); err != nil {
		log.Printf("replay: read error after %d frames: %v", frames, err)
	}
	log.Printf("replay: %d frames, %d lines skipped", frames, skipped)
}

func (r *Replay) Frames() <-chan []byte { return r.frames }

func (r *Replay) Send(frame []byte) error {
	log.Printf("replay: no radio, dropping %d B TX", len(frame))
	return nil
}

func (r *Replay) Close() error {
	r.once.Do(func() { close(r.stop) })
	return nil
}

// Mock replays injected frames and records what the engine transmits.
type Mock struct {
	frames chan []byte
	mu     sync.Mutex
	sent   [][]byte
}

func NewMock(in [][]byte) *Mock {
	ch := make(chan []byte, len(in)+1)
	for _, f := range in {
		ch <- f
	}
	close(ch)
	return &Mock{frames: ch}
}

func (m *Mock) Frames() <-chan []byte { return m.frames }

func (m *Mock) Send(frame []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, append([]byte(nil), frame...))
	return nil
}

func (m *Mock) Sent() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]byte(nil), m.sent...)
}

func (m *Mock) Close() error { return nil }
