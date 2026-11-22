package cc1101

import (
	"errors"
	"testing"
	"time"
)

// With the listen on and a busy channel, CMD_TXTRAIN answers ST_BUSY with one
// byte, the frames sent. The host reads it as a busy channel at once, not as a
// reply that never came.
func TestSendTrainMapsBusyChannel(t *testing.T) {
	p := &fakePort{read: autoAckResponse(cslTxBusy, []byte{0})}
	b := &Bridge{port: p, timeout: time.Second}
	sent, err := b.SendTrain([][]byte{{1, 2, 3}}, time.Second)
	if !errors.Is(err, ErrCSLTxBusy) || errors.Is(err, ErrLinkFault) || sent != 0 {
		t.Fatalf("sent=%d err=%v, want ErrCSLTxBusy and no link fault", sent, err)
	}
	if got := b.serialErrors.Load(); got != 0 || len(b.owed) != 0 {
		t.Fatalf("serialErrors=%d owed=%d, want a clean link", got, len(b.owed))
	}
}
