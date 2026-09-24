package tado

import (
	"encoding/hex"
	"errors"
	"strconv"
	"time"
)

// RFC 7959 splits a representation on a fixed byte boundary, so every block past
// block 0 can begin in the middle of a TLV. This layer collects the blocks of one
// response and hands the whole body to the TLV parser once.
//
// It is independent of the 6LoWPAN reassembler: one Block2 block can itself
// travel as several 6LoWPAN fragments, which Reassembler joins first.

const (
	block2MaxBody = 4096             // bytes of one transfer
	block2MaxOpen = 8                // concurrent transfers
	block2Timeout = 60 * time.Second // idle life of one transfer
)

var (
	errBlock2NoTransfer = errors.New("no-open-transfer")
	errBlock2OutOfOrder = errors.New("out-of-order")
	errBlock2SizeChange = errors.New("block-size-changed")
	errBlock2TooLarge   = errors.New("over-size-limit")
)

type block2Buf struct {
	next    uint32 // block number the transfer expects next
	szx     byte
	body    []byte
	started time.Time
}

// block2Reasm collects block-wise response bodies. It is single-goroutine state.
type block2Reasm struct {
	m   map[string]*block2Buf
	now func() time.Time
}

func newBlock2Reasm(now func() time.Time) *block2Reasm {
	if now == nil {
		now = time.Now
	}
	return &block2Reasm{m: map[string]*block2Buf{}, now: now}
}

// block2Key identifies one transfer: the peer, the token that pairs the blocks
// and the resource. The message id changes at every block of a driven GET, so it
// serves only the messages that carry no token.
func block2Key(srcEUI []byte, m *CoAPMessage) string {
	id := "t" + hex.EncodeToString(m.Token)
	if len(m.Token) == 0 {
		id = "m" + strconv.FormatUint(uint64(m.MID), 16)
	}
	return hexEUI(srcEUI) + "|" + id + "|" + m.URI()
}

// Add takes one block of a response and returns the reassembled body once the
// last block lands. It returns ok=false and a nil error while the transfer stays
// open, and an error for a block that joins no transfer.
func (r *block2Reasm) Add(key string, num uint32, more bool, szx byte, payload []byte) ([]byte, bool, error) {
	r.expire()
	if num == 0 {
		delete(r.m, key)
		if !more {
			return payload, true, nil
		}
		if len(payload) > block2MaxBody {
			return nil, false, errBlock2TooLarge
		}
		r.makeRoom()
		r.m[key] = &block2Buf{next: 1, szx: szx, body: append([]byte(nil), payload...), started: r.now()}
		return nil, false, nil
	}
	b, ok := r.m[key]
	if !ok {
		return nil, false, errBlock2NoTransfer
	}
	if num != b.next {
		delete(r.m, key)
		return nil, false, errBlock2OutOfOrder
	}
	if szx != b.szx {
		delete(r.m, key)
		return nil, false, errBlock2SizeChange
	}
	if len(b.body)+len(payload) > block2MaxBody {
		delete(r.m, key)
		return nil, false, errBlock2TooLarge
	}
	b.body = append(b.body, payload...)
	if more {
		b.next = num + 1
		return nil, false, nil
	}
	delete(r.m, key)
	return b.body, true, nil
}

// Open reports how many transfers are buffered.
func (r *block2Reasm) Open() int { return len(r.m) }

func (r *block2Reasm) expire() {
	deadline := r.now().Add(-block2Timeout)
	for k, b := range r.m {
		if b.started.Before(deadline) {
			delete(r.m, k)
		}
	}
}

// makeRoom drops the oldest transfers until one slot is free.
func (r *block2Reasm) makeRoom() {
	for len(r.m) >= block2MaxOpen {
		oldest := ""
		for k, b := range r.m {
			if oldest == "" || b.started.Before(r.m[oldest].started) {
				oldest = k
			}
		}
		delete(r.m, oldest)
	}
}
