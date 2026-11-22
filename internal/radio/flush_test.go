package radio

import (
	"fmt"
	"testing"

	"freedo/cc1101"
	"freedo/tado"
)

// lostFlushBridge loses the reply of its first queue flush.
type lostFlushBridge struct {
	*queuedLiveBridge
	flushes int
}

func (b *lostFlushBridge) FlushCSLQueue() error {
	b.flushes++
	if b.flushes == 1 {
		return fmt.Errorf("%w: the firmware dropped the request (bad crc)", cc1101.ErrLinkFault)
	}
	return b.queuedLiveBridge.FlushCSLQueue()
}

// The flush after a bridge detection empties the firmware queue: until it works,
// the bridge starts a queued job between two receive windows. A flush that the
// link lost goes out again at the next pass, and the jobs fail once.
func TestLiveRadioSendsAgainAFlushAfterDetectionThatTheLinkLost(t *testing.T) {
	bridge := &lostFlushBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}}
	r := &Live{br: bridge, cslDone: make(chan CSLCompletion, 8)}
	frame := []byte{0x69, 0xec, 0x10, 0x06, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x09, 0xa1, 0xce, 0xfa, 0x07}
	for i := 0; i < 3; i++ {
		a := tado.Action{CSLTx: true, HasCSLBatch: true, CSLBatchTag: uint16(i + 1), Bytes: append([]byte(nil), frame...)}
		a.Bytes[2] = byte(0x10 + i)
		if _, err := r.sendActions([]tado.Action{a}); err != nil {
			t.Fatal(err)
		}
	}
	if queued, _, _ := bridge.snapshotQueue(); len(queued) != 3 {
		t.Fatalf("%d jobs in the bridge queue, want 3", len(queued))
	}
	r.txBlocked.Store(true)
	// what loop() does at the top of every iteration once txBlocked is set
	for range 10 {
		r.dropCSL(ErrTXBlocked)
	}
	if bridge.flushes != 2 {
		t.Fatalf("%d flush requests in 10 passes, want the lost one and one retry", bridge.flushes)
	}
	if _, _, flushed := bridge.snapshotQueue(); flushed != 1 {
		t.Fatalf("%d flushes reached the bridge queue, want the retry only", flushed)
	}
	if got := len(r.cslDone); got != 3 {
		t.Fatalf("%d completions, want one failure for each of the 3 jobs", got)
	}
}
