package tado

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"
)

// TestZoneWriteRecordsOnlySentSched: a delivered zone write records the 6200
// it carried and nothing else. A write that leaves 6200 out keeps the 6200 the
// node and the zone cache held, absent or known: the device keeps its own.
func TestZoneWriteRecordsOnlySentSched(t *testing.T) {
	const ru = "RU1000000001" // hot water in zone 0, the writer of heating zone 1
	later := func(e *EmuState) time.Time { return e.cfg.Now().Add(2 * time.Hour) }
	paths := []struct {
		name    string
		zone    int
		enqueue func(*EmuState) error
	}{
		{"setpoint", 1, func(e *EmuState) error {
			return e.EnqueueSetpoint(ru, SetpointParams{ID: 1, Setpoint: 22, Sched: 21, Overlay: 2})
		}},
		{"off", 1, func(e *EmuState) error {
			return e.EnqueueSetpoint(ru, SetpointParams{ID: 1, Overlay: 2, Off: true})
		}},
		{"mode", 1, func(e *EmuState) error { return e.EnqueueMode(ru, 1, 3) }},
		{"return to schedule", 1, func(e *EmuState) error { return e.EnqueueSchedule(ru, 1) }},
		{"boost", 1, func(e *EmuState) error { return e.EnqueueBoost(ru, 1, true) }},
		{"timer expiry", 1, func(e *EmuState) error {
			if err := e.EnqueueSetpoint(ru, SetpointParams{ID: 1, Setpoint: 22, Sched: 21, Overlay: 3, Duration: time.Hour}); err != nil {
				return err
			}
			_, err := e.ExpireBoosts(later(e))
			return err
		}},
		{"rewrite", 1, func(e *EmuState) error { return e.EnqueueZoneRewrite(ru, 1) }},
		{"window", 1, func(e *EmuState) error { return e.EnqueueOpenWindow(ru, 1, true) }},
		{"hot water overlay", 0, func(e *EmuState) error { return e.EnqueueDHWOverlay(ru, 0, 48, true, 0) }},
		{"hot water off", 0, func(e *EmuState) error { return e.EnqueueDHWOverlay(ru, 0, 0, false, 0) }},
		{"hot water return to schedule", 0, func(e *EmuState) error { return e.EnqueueDHWSchedule(ru, 0) }},
		{"hot water timer expiry", 0, func(e *EmuState) error {
			if err := e.EnqueueDHWOverlay(ru, 0, 48, true, time.Hour); err != nil {
				return err
			}
			_, err := e.ExpireBoosts(later(e))
			return err
		}},
		{"hot water rewrite", 0, func(e *EmuState) error { return e.EnqueueZoneRewrite(ru, 0) }},
	}
	// Each case seeds the node with a device-reported heating block, with
	// nodeSched as its 6200 (0 for none), and the zone cache with cacheSched.
	cases := []struct {
		name       string
		home       bool
		program    bool // the owner pushed the program of the written zone: 19 C
		nodeSched  float64
		cacheSched float64
		// wantNode and wantCache are the 6200 after delivery, 0 for none.
		wantNode, wantCache float64
	}{
		{name: "unknown stays absent", home: true},
		{name: "reported stays", home: true, nodeSched: 55, wantNode: 55},
		{name: "away keeps both", nodeSched: 20.5, cacheSched: 20.5, wantNode: 20.5, wantCache: 20.5},
		{name: "known program recorded", home: true, program: true, nodeSched: 55, wantNode: 19, wantCache: 19},
	}
	for _, p := range paths {
		for _, tc := range cases {
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				e, in := fixtureEmu(t)
				dev, err := in.Resolve(ru)
				if err != nil {
					t.Fatal(err)
				}
				wire := dev.WireEUI()
				other := 1 - p.zone
				programs := map[int]float64{other: 55}
				if tc.program {
					programs[p.zone] = 19
				}
				if tc.home {
					e.SetHomeState(true, programs)
				}
				block := []TLV{
					{Tag: "6240", Val: []byte{0x00}}, {Tag: "6280", Val: tempC(20)}, {Tag: "62e0", Val: []byte{0x00}},
				}
				if tc.nodeSched != 0 {
					block = append(block, TLV{Tag: "6200", Val: tempC(tc.nodeSched)})
				}
				ns := e.node(wire)
				e.cacheTLVs(ns, block, "", ClassData, ru, "bridge", "")
				if tc.cacheSched != 0 {
					e.storeZoneState(p.zone, "z/s", encodeTLVs([]TLV{
						{Tag: "6160", Val: []byte{zoneCtlAway}}, {Tag: "61e0", Val: []byte{0x00}},
						{Tag: "6200", Val: tempC(tc.cacheSched)}, {Tag: "6240", Val: []byte{0x00}},
					}))
				}
				if err := p.enqueue(e); err != nil {
					t.Fatal(err)
				}
				label := ShortFromWire(wire)
				for n := 0; e.Pending(ru) > 0; n++ {
					if n > 4 {
						t.Fatalf("%d writes left undelivered", e.Pending(ru))
					}
					if _, ok := e.nextDownlink(wire, label); !ok {
						t.Fatal("queued write not sent")
					}
					head := e.queue[hexEUI(wire)][0]
					e.confirmByMID(wire, &CoAPMessage{Type: "ACK", Code: head.responseCode, MID: head.mid, Token: head.responseToken})
				}

				wantNode := tc.wantNode
				if p.zone == 0 && tc.program {
					// A hot-water write never moves the node's 6200.
					wantNode = tc.nodeSched
				}
				if got := ns.raw["6200"]; wantNode == 0 && got != nil || wantNode != 0 && !bytes.Equal(got, tempC(wantNode)) {
					t.Errorf("node raw 6200=%s, want %v", hex.EncodeToString(got), wantNode)
				}
				if ns.LastSchedC != wantNode {
					t.Errorf("node LastSchedC=%v, want %v", ns.LastSchedC, wantNode)
				}
				got, ok := e.ZoneSchedC(p.zone)
				if tc.wantCache == 0 && ok || tc.wantCache != 0 && got != tc.wantCache {
					t.Errorf("zone cache 6200=%v (held=%t), want %v", got, ok, tc.wantCache)
				}
			})
		}
	}
}
