package radio

import (
	"errors"
	"sort"
	"testing"
	"time"

	"freedo/cc1101"
	"freedo/tado"
)

func paceFrame(short byte) []byte { return []byte{0x69, 0xec, 0, short, 0x04} }

func paceAction(short byte, origin tado.Origin) tado.Action {
	return tado.Action{Bytes: paceFrame(short), CSLTx: true, QueueAttempt: true, QueueMID: uint16(short), QueueTry: 1, Origin: origin}
}

// TestBootstrapPaceNineDeviceRestart models the bench: after a restart nine
// devices have no anchor and each job asks for a train, all at once. The jobs
// that the cooldown of the last train holds back wait in the pace like the ones
// over the limit. The trains spread over minutes, the two user commands go
// before the refreshes that wait, and the radio refuses nothing for good.
func TestBootstrapPaceNineDeviceRestart(t *testing.T) {
	base := time.Unix(1000, 0)
	now := base
	bridge := &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}
	radio := &Live{
		br: bridge, clock: func() time.Time { return now }, airtimeLimit: true,
		cslPending: make(map[uint16]liveCSLPending), cslGroups: make(map[uint16]*liveCSLGroup),
	}
	type job struct {
		action  tado.Action
		arrives time.Duration
		next    time.Duration
		done    bool
	}
	var jobs []*job
	for short := byte(1); short <= 7; short++ {
		jobs = append(jobs, &job{action: paceAction(short, tado.OriginRefresh)})
	}
	// The user commands arrive one second after the restart.
	for short := byte(8); short <= 9; short++ {
		jobs = append(jobs, &job{action: paceAction(short, tado.OriginUser), arrives: time.Second, next: time.Second})
	}
	var grants []time.Duration
	var order []byte
	sawWaiting := 0
	for step := time.Duration(0); step <= 5*time.Minute; step += 100 * time.Millisecond {
		now = base.Add(step)
		for _, j := range jobs {
			if j.done || step < j.next {
				continue
			}
			result, err := radio.sendActions([]tado.Action{j.action})
			var paced *BootstrapPaced
			switch {
			case errors.As(err, &paced):
				if len(result.Accepted) != 0 {
					t.Fatalf("a paced job was accepted: %+v", result)
				}
				j.next = step + paced.RetryAfter
			case err != nil:
				t.Fatalf("job %x: %v", j.action.Bytes[3], err)
			default:
				j.done = true
				grants = append(grants, step)
				order = append(order, j.action.Bytes[3])
			}
		}
		if step == 1500*time.Millisecond {
			sawWaiting = radio.BootstrapWaiting()
		}
	}
	if len(order) != 9 {
		t.Fatalf("%d of 9 jobs sent a train, order %v: a job was dropped", len(order), order)
	}
	for _, c := range bridge.queued {
		if !c.bootstrap {
			t.Fatalf("job %x went out without its train", c.frame[3])
		}
	}
	for i := range grants {
		n := 0
		for _, g := range grants {
			if g >= grants[i] && g < grants[i]+time.Minute {
				n++
			}
		}
		if n > liveBootstrapPaceMax {
			t.Fatalf("%d trains in the minute from %s, want at most %d: %v", n, grants[i], liveBootstrapPaceMax, grants)
		}
	}
	if span := grants[len(grants)-1] - grants[0]; span < time.Minute {
		t.Fatalf("the trains span %s, want them spread over minutes", span)
	}
	// One train started at once. The other eight jobs wait: the cooldown of the
	// last train holds them, and then the limit of the minute.
	if sawWaiting != 8 {
		t.Fatalf("health showed %d waiting trains after one second, want 8", sawWaiting)
	}
	for i := 1; i < len(grants); i++ {
		if grants[i]-grants[i-1] < liveCSLBootstrapGlobalCooldown {
			t.Fatalf("trains at %s and %s are closer than the global cooldown", grants[i-1], grants[i])
		}
	}
	if left := radio.BootstrapWaiting(); left != 0 {
		t.Fatalf("%d trains still wait after every job went out", left)
	}
	// Of the jobs that waited, the user commands (8 and 9) go before the refreshes (5, 6, 7).
	pos := map[byte]int{}
	for i, short := range order {
		pos[short] = i
	}
	late := []int{pos[5], pos[6], pos[7]}
	sort.Ints(late)
	if pos[8] > late[0] || pos[9] > late[0] {
		t.Fatalf("order %v: a refresh went before a user command", order)
	}
}

// TestBootstrapPaceRefundsAJobThatNeverFlew: a job that the firmware turned away
// before its first frame did not use a train, so its slot is free again.
func TestBootstrapPaceRefundsAJobThatNeverFlew(t *testing.T) {
	now := time.Unix(1000, 0)
	radio := &Live{clock: func() time.Time { return now }, airtimeLimit: true}
	for short := byte(1); short <= liveBootstrapPaceMax; short++ {
		if paced := radio.paceBootstrap(string([]byte{short}), false, true); paced != nil {
			t.Fatalf("train %d paced: %v", short, paced)
		}
	}
	if radio.paceBootstrap("x", false, true) == nil {
		t.Fatal("a fifth train in the minute was not paced")
	}
	radio.settleBootstrapSlot(string([]byte{2}), &cc1101.CSLTxResult{})
	if paced := radio.paceBootstrap("x", false, true); paced != nil {
		t.Fatalf("the slot of a busy channel did not return: %v", paced)
	}
	radio.confirmCSLBootstrap(paceFrame(2), true, &cc1101.CSLTxResult{}, cc1101.ErrCSLTxBusy)
	if got := radio.CSLBusy(); got != 1 {
		t.Fatalf("busy count %d, want 1", got)
	}
}

// TestBootstrapPaceResponseGoesOutWithoutATrain: a response has no retry of its
// own, so over the limit it goes out on the anchor it has, and it does not wait.
func TestBootstrapPaceResponseGoesOutWithoutATrain(t *testing.T) {
	now := time.Unix(1000, 0)
	bridge := &orderedLiveBridge{cslResult: &cc1101.CSLTxResult{Sent: 22, Ack: &cc1101.RxFrame{}}}
	radio := &Live{br: bridge, clock: func() time.Time { return now }, airtimeLimit: true}
	for short := byte(1); short <= liveBootstrapPaceMax; short++ {
		radio.paceBootstrap(string([]byte{short}), false, true)
	}
	action := tado.Action{Bytes: paceFrame(9), CSLTx: true}
	if _, err := radio.sendActions([]tado.Action{action}); err != nil {
		t.Fatal(err)
	}
	if len(bridge.cslBootstrap) != 1 || bridge.cslBootstrap[0] {
		t.Fatalf("bootstrap flags %v, want one send without a train", bridge.cslBootstrap)
	}
	if radio.BootstrapWaiting() != 0 {
		t.Fatal("a response joined the waiting trains")
	}
}
