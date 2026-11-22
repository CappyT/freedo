package daemon

import (
	"io"
	"testing"

	"freedo/cc1101"
	"freedo/internal/radio"
	"freedo/tado"
)

// answers lists the answers to a CON that the radio got, in the order sent.
func answers(r *immediateTestRadio) []tado.Action {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []tado.Action
	for _, b := range r.batches {
		for _, a := range b {
			if a.CSLTx && a.AnswerCounts {
				out = append(out, a)
			}
		}
	}
	return out
}

// lastAnswer is the answer to a CON in the last batch the radio got.
func lastAnswer(t *testing.T, r *immediateTestRadio) tado.Action {
	t.Helper()
	all := answers(r)
	if len(all) == 0 {
		t.Fatal("no answer to a CON went to the radio")
	}
	return all[len(all)-1]
}

// answerRig is a daemon with a fake radio. submit: the radio sent the actions at
// once and the device did not answer (a stale anchor). deliver: it sent them and
// the device acknowledged. queue: it took them into its queue, and the test
// completes them.
func answerRig(t *testing.T, submit, deliver, queue bool) (*daemon, *immediateTestRadio, *tado.Device) {
	t.Helper()
	dev, err := defaultInstall.Resolve("0a01")
	if err != nil {
		t.Fatal(err)
	}
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	live := newImmediateTestRadio(submit || deliver, nil)
	live.deliver, live.accept, live.cslQueue = deliver, queue, queue
	return newDaemon(defaultInstall, emu, live, false, "live"), live, dev
}

func displaySettingsGet(mid uint16) tado.CoAPReq {
	return tado.CoAPReq{Type: "CON", Code: "0.01", MID: mid, Token: []byte{byte(mid)}, Path: []string{"d", "dispsettings"}}
}

func completeAnswer(d *daemon, a tado.Action, sent int, ack bool) {
	result := &cc1101.CSLTxResult{Sent: sent}
	if ack {
		result.Ack = &cc1101.RxFrame{}
	}
	d.handleCSLCompletion(radio.CSLCompletion{Action: a, Result: result})
}

// The re-ack budget of a CON is spent on answers that the device acknowledged. An
// answer that the radio sent and the device acknowledged counts: the next copy is
// a re-ack that may not use the train. An answer on a stale anchor (sent, no ACK)
// does not: the next copy may use the train.
func TestReAckBudgetCountsTheAnswerTheDeviceAcknowledged(t *testing.T) {
	for _, tc := range []struct {
		name            string
		submit, deliver bool
		noBootstrap     bool
	}{{"nothing sent", false, false, false}, {"sent, no ACK", true, false, false}, {"sent and acknowledged", false, true, true}} {
		d, live, dev := answerRig(t, tc.submit, tc.deliver, false)
		d.handleFrame(deviceRequestUplink(t, dev, displaySettingsGet(0x7a01), 0x31), true)
		if a := lastAnswer(t, live); a.NoBootstrap {
			t.Fatalf("%s: the first answer forbids the train", tc.name)
		}
		d.handleFrame(deviceRequestUplink(t, dev, displaySettingsGet(0x7a01), 0x32), true)
		if a := lastAnswer(t, live); a.NoBootstrap != tc.noBootstrap {
			t.Fatalf("%s: the re-ack has NoBootstrap=%t", tc.name, a.NoBootstrap)
		}
	}
}

// An answer in the bridge queue counts against the budget while it is in flight,
// and its completion says whether it stays: with the device's ACK it does, with
// no ACK or a queue that dropped it, it does not.
func TestReAckBudgetFollowsTheCompletionOfAnAnswer(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sent        int
		ack         bool
		noBootstrap bool
	}{{"acknowledged", 22, true, true}, {"no ACK", 22, false, false}, {"nothing sent", 0, false, false}} {
		d, live, dev := answerRig(t, false, false, true)
		d.handleFrame(deviceRequestUplink(t, dev, displaySettingsGet(0x7a02), 0x41), true)
		first := lastAnswer(t, live)
		// Before the completion the answer is in flight, and the next copy is a re-ack.
		d.handleFrame(deviceRequestUplink(t, dev, displaySettingsGet(0x7a02), 0x42), true)
		if a := lastAnswer(t, live); !a.NoBootstrap {
			t.Fatalf("%s: the copy during the flight may use the train", tc.name)
		}
		// Both answers complete.
		completeAnswer(d, first, tc.sent, tc.ack)
		completeAnswer(d, lastAnswer(t, live), tc.sent, tc.ack)
		d.handleFrame(deviceRequestUplink(t, dev, displaySettingsGet(0x7a02), 0x43), true)
		got := answers(live)
		if tc.ack {
			if len(got) != 2 {
				t.Fatalf("%s: %d answers reached the radio, want the answer and one re-ack", tc.name, len(got))
			}
			continue
		}
		if len(got) != 3 || got[2].NoBootstrap != tc.noBootstrap {
			t.Fatalf("%s: %d answers, last NoBootstrap=%t: the budget stayed spent", tc.name, len(got), got[len(got)-1].NoBootstrap)
		}
	}
}

// Copies that arrive before the completion of an answer see it in flight: the
// radio gets one answer and one re-ack for the CON, not one for each copy.
func TestAtMostTwoAnswersForOneConWhileTheyAreInFlight(t *testing.T) {
	d, live, dev := answerRig(t, false, false, true)
	for i := 0; i < 5; i++ {
		d.handleFrame(deviceRequestUplink(t, dev, displaySettingsGet(0x7b01), byte(0x31+i)), true)
	}
	if n := len(answers(live)); n != 2 {
		t.Fatalf("%d answers reached the radio queue for one CON, want 2", n)
	}
	for _, a := range answers(live) {
		completeAnswer(d, a, 22, true)
	}
	d.handleFrame(deviceRequestUplink(t, dev, displaySettingsGet(0x7b01), 0x40), true)
	if n := len(answers(live)); n != 2 {
		t.Fatalf("%d answers after both were acknowledged, want 2", n)
	}
}

// Only the first frame of an answer counts. The completion of another job to the
// same device, with an ACK, is not an answer: it does not spend the budget.
func TestCompletionOfAJobThatIsNotAnAnswerDoesNotSpendTheBudget(t *testing.T) {
	d, live, dev := answerRig(t, false, false, true)
	d.handleFrame(deviceRequestUplink(t, dev, displaySettingsGet(0), 0x31), true)
	first := lastAnswer(t, live)
	completeAnswer(d, first, 22, false)
	job := tado.Action{CSLTx: true, QueueAttempt: true, Bytes: first.Bytes}
	completeAnswer(d, job, 22, true)
	d.handleFrame(deviceRequestUplink(t, dev, displaySettingsGet(0), 0x32), true)
	if a := lastAnswer(t, live); a.NoBootstrap {
		t.Fatal("the ACK of a job that is not an answer spent the budget of the CON")
	}
}
