package tado

import "testing"

// A home or away switch is a write that the user asked for, and its queue entry
// has the kind "rewrite" like a schedule block change. The origin tells them
// apart: the radio serves the switch before a refresh.
func TestZoneSwitchIsAUserWrite(t *testing.T) {
	e, _ := fixtureEmu(t)
	if err := e.EnqueueZoneSwitch(bedroomEUIHex, 10); err != nil {
		t.Fatal(err)
	}
	q := e.queue[bedroomEUIHex]
	last := q[len(q)-1]
	if last.kind != "rewrite" || last.origin != OriginUser {
		t.Fatalf("kind=%q origin=%d, want a rewrite from the user", last.kind, last.origin)
	}
	if got := e.queueOrigin(bedroomEUIHex); got != OriginUser {
		t.Fatalf("queue origin=%d, want the user", got)
	}
}

// A schedule block change uses the same kind and stays a refresh.
func TestZoneRewriteIsARefresh(t *testing.T) {
	e, _ := fixtureEmu(t)
	if err := e.EnqueueZoneRewrite(bedroomEUIHex, 10); err != nil {
		t.Fatal(err)
	}
	q := e.queue[bedroomEUIHex]
	last := q[len(q)-1]
	if last.kind != "rewrite" || last.origin != OriginRefresh {
		t.Fatalf("kind=%q origin=%d, want a rewrite that is a refresh", last.kind, last.origin)
	}
}
