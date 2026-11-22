package radio

import (
	"testing"
	"time"

	"freedo/cc1101"
)

// slowReceiveBridge holds each receive window for a full window and the round
// trips around it.
type slowReceiveBridge struct {
	*budgetBridge
	entered chan struct{}
}

func (b *slowReceiveBridge) RecvAutoAck(time.Duration, []byte, uint16) (*cc1101.AutoAckResult, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	time.Sleep(liveReceiveWindow + 100*time.Millisecond)
	return nil, nil
}

// A stop in the first milliseconds of a receive window still reads the airtime
// a last time: Close waits for the window to end.
func TestCloseInsideAReceiveWindowStillReadsTheAirtime(t *testing.T) {
	inner := &budgetBridge{queuedLiveBridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}, budget: 100000}
	bridge := &slowReceiveBridge{budgetBridge: inner, entered: make(chan struct{}, 1)}
	r, err := StartLive(bridge, defaultInstall.CoordinatorEUI(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-bridge.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not start a receive window")
	}
	inner.bmu.Lock()
	inner.buckets[cc1101.AirtimeBuckets-1] = 9460
	inner.bmu.Unlock()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if a, ok := r.Airtime(); !ok || a.Buckets[cc1101.AirtimeBuckets-1] != 9460 {
		t.Fatalf("airtime after Close = %+v ok=%v, want the buckets read after the stop", a, ok)
	}
}
