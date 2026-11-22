package tado

import "testing"

// The health counts the downlink traffic by kind: the name comes from the queue
// kind and the reason of the action.
func TestActionJobKind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action Action
		want   string
	}{
		{"echo", Action{Reason: "link-maintenance echo"}, "echo"},
		{"router advertisement", Action{Reason: "beacon router-advert ctr=3"}, "ra"},
		{"router advertisement wake-up", Action{Reason: "beacon wake-up 1/3 seq=01"}, "ra"},
		{"reply", Action{Reason: "coap-ack 2.05 z/4/ow mid=7404"}, "reply"},
		{"proxy timeout", Action{Reason: "zone-proxy-timeout 2.05 z/p mid=7404"}, "reply"},
		{"measure", Action{Job: zoneMeasureKind, QueueAttempt: true, Reason: "downlink zone measure 19.50 C 42% lid=4 seq=01 try=1"}, "measure"},
		{"notify", Action{Job: "zone", QueueAttempt: true, Reason: "downlink zone notify z/p lid=4 obs=3 tok=a1b2 seq=01 try=1"}, "notify"},
		{"relay", Action{Job: "zone", QueueAttempt: true, Reason: "downlink zone-relay PUT z/s mid=7403 seq=01 try=1"}, "zone_relay"},
		{"firmware block", Action{Job: fwKind, QueueAttempt: true}, "fw"},
		{"firmware state", Action{Job: "get", QueueAttempt: true, Reason: "downlink get d/fw/state seq=01 try=1"}, "fw_state"},
		{"other get", Action{Job: "get", QueueAttempt: true, Reason: "downlink get d/stats seq=01 try=1"}, "get"},
		{"setpoint", Action{Job: "setpoint", QueueAttempt: true}, "command"},
		{"boost", Action{Job: "boost", QueueAttempt: true}, "command"},
		{"rewrite", Action{Job: "rewrite", QueueAttempt: true}, "rewrite"},
		{"identify", Action{Job: "identify", QueueAttempt: true}, "identify"},
		{"config", Action{Job: "config", QueueAttempt: true}, "config"},
		{"unknown", Action{Job: "icmp", QueueAttempt: true}, "other"},
		{"bare action", Action{Reason: "wake-up 1/3"}, "other"},
	} {
		if got := tc.action.JobKind(); got != tc.want {
			t.Errorf("%s: kind %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A queued downlink carries its queue kind into the action.
func TestQueuedActionCarriesItsJobKind(t *testing.T) {
	e, _ := fixtureEmu(t)
	if err := e.EnqueueZoneRewrite(bedroomEUIHex, 10); err != nil {
		t.Fatal(err)
	}
	actions, ok, err := e.NextQueuedDownlink(bedroomEUIHex)
	if err != nil || !ok || len(actions) == 0 {
		t.Fatalf("no queued downlink: ok=%t err=%v", ok, err)
	}
	if got := actions[0].JobKind(); got != "rewrite" {
		t.Fatalf("kind %q, want rewrite", got)
	}
}
