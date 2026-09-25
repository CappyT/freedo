package tado

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// rereadInstall is the synthetic installation on Europe/Rome with options
// added, and with src replaced by dst in its YAML when src is set.
func rereadInstall(t *testing.T, options, src, dst string) *Install {
	t.Helper()
	doc := synthInstallYAML + "options:\n  timezone: Europe/Rome\n" + options
	if src != "" {
		doc = strings.Replace(doc, src, dst, 1)
	}
	in, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// rereadQueue counts the downlinks queued to dev, and the re-read triggers
// among them.
func rereadQueue(e *EmuState, dev []byte) (total, rereads int) {
	for _, d := range e.queue[hexEUI(dev)] {
		total++
		if d.kind == configRereadKind {
			rereads++
		}
	}
	return total, rereads
}

// drainQueue answers 2.04 to every downlink queued to dev, sending first the
// ones not on the air yet.
func drainQueue(t *testing.T, e *EmuState, dev []byte) {
	t.Helper()
	key := hexEUI(dev)
	for i := 0; e.Pending(key) > 0; i++ {
		if i == 10 {
			t.Fatalf("the queue of %s does not drain", key)
		}
		if !e.queue[key][0].inFlight {
			if _, ok, err := e.NextQueuedDownlink(key); err != nil || !ok {
				t.Fatalf("no downlink sent to %s: ok=%v err=%v", key, ok, err)
			}
		}
		d := e.queue[key][0]
		ack, err := BuildCoAPAck("2.04", d.mid, d.responseToken, nil, dev, e.selfEUI, coapPort, bridgeCoAPPort)
		if err != nil {
			t.Fatal(err)
		}
		answerCtr++
		f, err := BuildData(e.key, byte(answerCtr), e.selfEUI, dev[:5], WrapAppPayload(ack, answerCtr, nil))
		if err != nil {
			t.Fatal(err)
		}
		e.OnFrame(f.Bytes())
	}
}

// TestThermostatRereadOnEveryETagChange: the wall thermostat takes d/config
// only on its own GET, so every change that moves the ETag of its served
// document queues exactly one re-read trigger, whatever the source. A change
// that keeps the ETag queues none, and a trigger already waiting covers the
// change.
func TestThermostatRereadOnEveryETagChange(t *testing.T) {
	const election = "members: [VA0000000001], reporter: VA0000000001"
	beforeDST := romeDSTEnd.Add(-30 * time.Minute)
	cases := []struct {
		name    string
		prepare func(t *testing.T, e *EmuState, key string)
		change  func(t *testing.T, e *EmuState, key string) error
		moves   bool
	}{
		{name: "flow limit set", moves: true, change: func(t *testing.T, e *EmuState, key string) error {
			return e.EnqueueFlowSetpoint(key, 1, 55)
		}},
		{name: "flow limit set to the default", change: func(t *testing.T, e *EmuState, key string) error {
			return e.EnqueueFlowSetpoint(key, 1, DefaultFlowLimitC)
		}},
		{name: "flow limit cleared", moves: true,
			prepare: func(t *testing.T, e *EmuState, key string) {
				if err := e.EnqueueFlowSetpoint(key, 1, 55); err != nil {
					t.Fatal(err)
				}
			},
			change: func(t *testing.T, e *EmuState, key string) error {
				e.ClearFlowSetpoint(1)
				return nil
			}},
		{name: "flow limit never set cleared", change: func(t *testing.T, e *EmuState, key string) error {
			e.ClearFlowSetpoint(1)
			return nil
		}},
		{name: "reload unchanged", change: func(t *testing.T, e *EmuState, key string) error {
			_, _, err := e.SetInstall(rereadInstall(t, "", "", ""))
			return err
		}},
		{name: "reload flow_limit_c", moves: true, change: func(t *testing.T, e *EmuState, key string) error {
			_, _, err := e.SetInstall(rereadInstall(t, "  flow_limit_c: 50\n", "", ""))
			return err
		}},
		{name: "reload election", moves: true, change: func(t *testing.T, e *EmuState, key string) error {
			_, _, err := e.SetInstall(rereadInstall(t, "", election,
				"members: [RU0000000001, VA0000000001], reporter: RU0000000001"))
			return err
		}},
		{name: "reload offline_schedule", moves: true, change: func(t *testing.T, e *EmuState, key string) error {
			_, _, err := e.SetInstall(rereadInstall(t, "  offline_schedule: false\n", "", ""))
			return err
		}},
		{name: "reload utc_offset_min", moves: true, change: func(t *testing.T, e *EmuState, key string) error {
			_, _, err := e.SetInstall(rereadInstall(t, "  utc_offset_min: 90\n", "", ""))
			return err
		}},
		{name: "dst transition", moves: true, change: func(t *testing.T, e *EmuState, key string) error {
			if !e.SyncUTCOffset(romeDSTEnd.Add(time.Hour)) {
				t.Error("no offset change across the transition")
			}
			return nil
		}},
		{name: "no dst transition", change: func(t *testing.T, e *EmuState, key string) error {
			e.SyncUTCOffset(romeDSTEnd.Add(-time.Second))
			return nil
		}},
		{name: "offline schedule cleared", moves: true,
			prepare: func(t *testing.T, e *EmuState, key string) {
				e.offlineSched[key] = false
				e.refreshThermostatDocuments()
			},
			change: func(t *testing.T, e *EmuState, key string) error {
				_, err := e.ClearOfflineSchedule(key, 0)
				return err
			}},
		{name: "offline schedule never set cleared", change: func(t *testing.T, e *EmuState, key string) error {
			_, err := e.ClearOfflineSchedule(key, 0)
			return err
		}},
		{name: "restore with a new flow limit", moves: true, change: func(t *testing.T, e *EmuState, key string) error {
			state := e.ExportDesiredState()
			state.FlowC = map[string]float64{"1": 55}
			return e.RestoreDesiredState(state)
		}},
		{name: "restore unchanged", change: func(t *testing.T, e *EmuState, key string) error {
			return e.RestoreDesiredState(e.ExportDesiredState())
		}},
		{name: "interface module id changed", moves: true, change: func(t *testing.T, e *EmuState, key string) error {
			e.OnFrame(moduleReport(t, e.inst, mustHex(key), 120, 0x7a11))
			return nil
		}},
		{name: "interface module id repeated", change: func(t *testing.T, e *EmuState, key string) error {
			e.OnFrame(moduleReport(t, e.inst, mustHex(key), 71, 0x7a12))
			return nil
		}},
	}
	for _, c := range cases {
		for _, waiting := range []bool{false, true} {
			name := c.name
			if waiting {
				name += " with a trigger waiting"
			}
			t.Run(name, func(t *testing.T) {
				in := rereadInstall(t, "", "", "")
				ru, _ := synthDevices(t, in)
				key := hexEUI(ru)
				cfg := in.EmuConfig()
				cfg.Now = func() time.Time { return beforeDST }
				e := NewEmu(cfg, NewUnhandledLogger(io.Discard))
				e.OnFrame(moduleReport(t, in, ru, 71, 0x7a01))
				if c.prepare != nil {
					c.prepare(t, e, key)
				}
				drainQueue(t, e, ru)
				if waiting {
					centi := -150
					if err := e.EnqueueConfig(key, 1, ConfigWrite{CalibOffset: &centi}); err != nil {
						t.Fatal(err)
					}
					if !e.ConfigRereadQueued(key) {
						t.Fatal("the calibOffset write queued no re-read trigger")
					}
				}
				before := append([]byte(nil), e.cfgDoc[key].etag...)
				if len(before) == 0 {
					t.Fatal("no thermostat document")
				}

				if err := c.change(t, e, key); err != nil {
					t.Fatal(err)
				}
				if moved := !bytes.Equal(e.cfgDoc[key].etag, before); moved != c.moves {
					t.Fatalf("ETag moved = %v, want %v", moved, c.moves)
				}
				want := 0
				if c.moves || waiting {
					want = 1
				}
				if total, rereads := rereadQueue(e, ru); total != want || rereads != want {
					t.Errorf("thermostat queue holds %d downlinks, %d re-read triggers, want %d of each",
						total, rereads, want)
				}
			})
		}
	}
}
