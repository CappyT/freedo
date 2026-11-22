package tado

import "testing"

// TestDownlinkOrigin: a user write marks the whole queue of the device as user
// traffic, because the bootstrap train that opens the link serves every job
// behind it. A read stays a refresh.
func TestDownlinkOrigin(t *testing.T) {
	e, _ := fixtureEmu(t)
	if err := e.EnqueueGet(bedroomEUIHex, "d/fw/state", []string{"d", "fw", "state"}, nil); err != nil {
		t.Fatal(err)
	}
	actions, ok, err := e.NextQueuedDownlink(bedroomEUIHex)
	if err != nil || !ok {
		t.Fatalf("no queued downlink: ok=%v err=%v", ok, err)
	}
	if actions[0].Origin != OriginRefresh {
		t.Fatalf("a read has origin %d, want refresh", actions[0].Origin)
	}
	if err := e.EnqueueChildLock(bedroomEUIHex, 10, true); err != nil {
		t.Fatal(err)
	}
	q := e.queue[bedroomEUIHex]
	if q[0].origin != OriginRefresh || q[1].origin != OriginUser {
		t.Fatalf("origins %d, %d, want refresh then user", q[0].origin, q[1].origin)
	}
	if got := e.queueOrigin(bedroomEUIHex); got != OriginUser {
		t.Fatalf("queue origin %d, want user: a user command waits behind the read", got)
	}
}
