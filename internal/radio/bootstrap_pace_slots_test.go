package radio

import (
	"errors"
	"strings"
	"testing"
	"time"

	"freedo/cc1101"
	"freedo/tado"
)

// paceRig is a live radio with a queue bridge and a clock that the test moves.
// The airtime limit is on, unless the test turns it off.
type paceRig struct {
	t      *testing.T
	now    time.Time
	bridge *queuedLiveBridge
	radio  *Live
	done   int
}

func newPaceRig(t *testing.T) *paceRig {
	rig := &paceRig{t: t, now: time.Unix(1000, 0), bridge: &queuedLiveBridge{orderedLiveBridge: &orderedLiveBridge{}}}
	rig.radio = &Live{
		br: rig.bridge, clock: func() time.Time { return rig.now }, airtimeLimit: true,
		cslPending: make(map[uint16]liveCSLPending), cslGroups: make(map[uint16]*liveCSLGroup),
		cslDone: make(chan CSLCompletion, 1024),
	}
	return rig
}

// finish completes the jobs that the bridge queued since the last call. sent
// gives the wake frame count of each job, in the order of the queue.
func (g *paceRig) finish(sent ...int) { g.t.Helper(); g.finishAs(false, sent...) }

// finishAcked is finish with the Enhanced ACK of the device in each result: a
// train that the device answered confirms that the bridge holds its anchor.
func (g *paceRig) finishAcked(sent ...int) { g.t.Helper(); g.finishAs(true, sent...) }

func (g *paceRig) finishAs(ack bool, sent ...int) {
	g.t.Helper()
	queued, _, _ := g.bridge.snapshotQueue()
	g.bridge.mu.Lock()
	for i, call := range queued[g.done:] {
		result := &cc1101.CSLTxResult{Sent: sent[i]}
		if ack && sent[i] > 0 {
			result.Ack = &cc1101.RxFrame{}
		}
		g.bridge.completions = append(g.bridge.completions, &cc1101.CSLQueueCompletion{ID: call.id, Result: result})
	}
	g.done = len(queued)
	g.bridge.mu.Unlock()
	for {
		more, err := g.radio.drainCSL()
		if err != nil {
			g.t.Fatal(err)
		}
		if !more {
			return
		}
	}
}

func fragmentBlock(short byte, tag uint16, frames int) []tado.Action {
	var actions []tado.Action
	for i := 0; i < frames; i++ {
		actions = append(actions, tado.Action{Bytes: paceFrame(short), CSLTx: true, HasCSLBatch: true, CSLBatchTag: tag, QueueAttempt: true, QueueMID: tag, QueueTry: 1})
	}
	return actions
}

// A job that wants a train while the last train is inside its global cooldown
// waits in the pace. It does not go out with no train and come back 30 s later,
// so the pace orders the jobs: a user command goes before a refresh that asked
// first.
func TestBootstrapPaceCooldownWaitsInThePace(t *testing.T) {
	rig := newPaceRig(t)
	send := func(short byte, origin tado.Origin) error {
		_, err := rig.radio.sendActions([]tado.Action{paceAction(short, origin)})
		return err
	}
	if err := send(1, tado.OriginRefresh); err != nil {
		t.Fatal(err)
	}
	var refresh, user *BootstrapPaced
	if !errors.As(send(2, tado.OriginRefresh), &refresh) || !errors.As(send(3, tado.OriginUser), &user) {
		t.Fatal("a job inside the cooldown of the last train was not paced")
	}
	if refresh.RetryAfter < liveCSLBootstrapGlobalCooldown || user.RetryAfter < liveCSLBootstrapGlobalCooldown {
		t.Fatalf("retry in %s and %s, want the rest of the cooldown", refresh.RetryAfter, user.RetryAfter)
	}
	if queued, _, _ := rig.bridge.snapshotQueue(); len(queued) != 1 || !queued[0].bootstrap {
		t.Fatalf("queued %+v, want only the first train", queued)
	}
	// Both ask again at the same instant, the refresh first.
	rig.now = rig.now.Add(refresh.RetryAfter)
	if !errors.As(send(2, tado.OriginRefresh), &refresh) {
		t.Fatal("the refresh took the slot before the user command")
	}
	if err := send(3, tado.OriginUser); err != nil {
		t.Fatalf("the user command did not get the slot: %v", err)
	}
	if queued, _, _ := rig.bridge.snapshotQueue(); len(queued) != 2 || !queued[1].bootstrap || queued[1].frame[3] != 3 {
		t.Fatalf("queued %+v, want the user command second, with its train", queued)
	}
}

// A response has no retry, so the cooldown does not hold it: it goes out on the
// anchor it has.
func TestBootstrapPaceCooldownDoesNotHoldAResponse(t *testing.T) {
	rig := newPaceRig(t)
	if _, err := rig.radio.sendActions([]tado.Action{paceAction(1, tado.OriginRefresh)}); err != nil {
		t.Fatal(err)
	}
	direct := &orderedLiveBridge{cslResult: &cc1101.CSLTxResult{Sent: 22, Ack: &cc1101.RxFrame{}}}
	rig.radio.br = direct
	response := tado.Action{Bytes: paceFrame(2), CSLTx: true}
	if _, err := rig.radio.sendActions([]tado.Action{response}); err != nil {
		t.Fatal(err)
	}
	if len(direct.cslBootstrap) != 1 || direct.cslBootstrap[0] {
		t.Fatalf("bootstrap flags %v, want one send without a train", direct.cslBootstrap)
	}
}

// A firmware transfer sends one fragmented datagram for each block, and each one
// asks for a train. The device has an anchor, so the bridge sends about 22 wake
// frames and no train: the pace does not count it.
func TestBootstrapPaceBlocksOnAnAnchorKeepNoSlot(t *testing.T) {
	rig := newPaceRig(t)
	for block := uint16(1); block <= 3*liveBootstrapPaceMax; block++ {
		if _, err := rig.radio.sendActions(fragmentBlock(1, block, 3)); err != nil {
			t.Fatalf("block %d: %v", block, err)
		}
		rig.finish(22, 22, 22)
		rig.now = rig.now.Add(time.Second)
	}
	if n := len(rig.radio.pace.starts); n != 0 {
		t.Fatalf("%d slots held by blocks that ran on an anchor", n)
	}
}

// realEcho is the link-maintenance echo as tado builds it: not a queue attempt.
func realEcho(short byte) tado.Action {
	return tado.Action{Bytes: paceFrame(short), Reason: "link-maintenance echo", CSLTx: true, CSLBootstrapAlways: true, WaitResponse: true}
}

func lastQueued(g *paceRig) queuedCSLCall {
	q, _, _ := g.bridge.snapshotQueue()
	return q[len(q)-1]
}

// The link-maintenance echo always asks for a train. Echoes to devices that have
// an anchor leave the slots free for a job that needs a train.
func TestBootstrapPaceEchoesOnAnAnchorKeepNoSlot(t *testing.T) {
	rig := newPaceRig(t)
	for i := 0; i < 2*liveBootstrapPaceMax; i++ {
		echo := realEcho(byte(1 + i%3))
		if _, err := rig.radio.sendActions([]tado.Action{echo}); err != nil {
			t.Fatalf("echo %d: %v", i, err)
		}
		rig.finish(22)
		rig.now = rig.now.Add(time.Second)
	}
	rig.now = rig.now.Add(liveCSLBootstrapGlobalCooldown)
	if _, err := rig.radio.sendActions([]tado.Action{paceAction(9, tado.OriginUser)}); err != nil {
		t.Fatalf("a job that needs a train was paced behind the echoes: %v", err)
	}
}

// A job that sent a train of one period keeps its slot for the minute, so the
// fifth train waits for the oldest one to leave the window.
func TestBootstrapPaceTrainsKeepTheirSlots(t *testing.T) {
	rig := newPaceRig(t)
	for short := byte(1); short <= liveBootstrapPaceMax; short++ {
		if _, err := rig.radio.sendActions([]tado.Action{paceAction(short, tado.OriginRefresh)}); err != nil {
			t.Fatalf("train %d: %v", short, err)
		}
		rig.finish(252)
		rig.now = rig.now.Add(liveCSLBootstrapGlobalCooldown)
	}
	var paced *BootstrapPaced
	if _, err := rig.radio.sendActions([]tado.Action{paceAction(9, tado.OriginRefresh)}); !errors.As(err, &paced) {
		t.Fatalf("the fifth train in the minute was not paced: %v", err)
	}
	if paced.RetryAfter < 40*time.Second {
		t.Fatalf("retry in %s, want the time until the oldest train leaves the minute", paced.RetryAfter)
	}
}

// While jobs that have not completed hold the slots, a job that waits looks
// again soon: a job that ran on an anchor gives its slot back. It does not wait
// for the whole minute.
func TestBootstrapPaceWaitsForJobsInFlight(t *testing.T) {
	rig := newPaceRig(t)
	for short := byte(1); short <= liveBootstrapPaceMax; short++ {
		if paced := rig.radio.paceBootstrap(string([]byte{short}), false, true); paced != nil {
			t.Fatal(paced)
		}
	}
	paced := rig.radio.paceBootstrap("x", false, true)
	if paced == nil || paced.RetryAfter > 5*time.Second {
		t.Fatalf("paced=%v, want a retry in a few seconds while the four jobs fly", paced)
	}
}

// A batch of frames books one slot. When the batch never reaches the air, one
// slot returns, not one for each frame.
func TestBootstrapPaceBatchGivesBackOneSlot(t *testing.T) {
	rig := newPaceRig(t)
	short := cslShort(paceFrame(1))
	for i := 0; i < 2; i++ {
		if paced := rig.radio.paceBootstrap(short, false, true); paced != nil {
			t.Fatal(paced)
		}
	}
	if _, err := rig.radio.sendActions(fragmentBlock(1, 1, 3)); err != nil {
		t.Fatal(err)
	}
	if n := len(rig.radio.pace.starts); n != 3 {
		t.Fatalf("%d slots after the batch, want 3", n)
	}
	rig.finish(0, 0, 0)
	if n := len(rig.radio.pace.starts); n != 2 {
		t.Fatalf("%d slots after a batch that never flew, want 2", n)
	}
}

// The first frame of a batch sent a train and the second frame ended with sent=0.
// The batch used the air, so it keeps its slot.
func TestBootstrapPaceBatchKeepsItsSlotWhenTheFirstFrameFlew(t *testing.T) {
	rig := newPaceRig(t)
	if _, err := rig.radio.sendActions(fragmentBlock(1, 1, 2)); err != nil {
		t.Fatal(err)
	}
	rig.finish(252, 0)
	if n := len(rig.radio.pace.starts); n != 1 {
		t.Fatalf("%d slots, want the one of the train", n)
	}
}

// A user job that waits for a slot and never asks again (it left its queue, or
// nothing schedules its retry) holds the refreshes back until its own retry is
// late by a grace, not for a long fixed time.
func TestBootstrapPaceWaiterThatDoesNotReturnLetsRefreshesGo(t *testing.T) {
	rig := newPaceRig(t)
	start := rig.now
	for short := byte(1); short <= liveBootstrapPaceMax; short++ {
		if paced := rig.radio.paceBootstrap(string([]byte{short}), false, true); paced != nil {
			t.Fatal(paced)
		}
		rig.radio.settleBootstrapSlot(string([]byte{short}), &cc1101.CSLTxResult{Sent: 252})
	}
	user := rig.radio.paceBootstrap("user", true, true)
	if user == nil {
		t.Fatal("the user job was not paced")
	}
	var granted time.Duration
	for rig.now = start.Add(user.RetryAfter); ; rig.now = rig.now.Add(time.Second) {
		if rig.radio.paceBootstrap("refresh", false, true) == nil {
			granted = rig.now.Sub(start)
			break
		}
		if rig.now.Sub(start) > 3*time.Minute {
			t.Fatal("the refresh never got a slot")
		}
	}
	if limit := user.RetryAfter + 12*time.Second; granted > limit {
		t.Fatalf("the refresh got its slot at %s, want by %s", granted, limit)
	}
}

// With the airtime limit off there is no limit of trains in a minute: the jobs
// go out as fast as the cooldown of the last train allows, and no slot is held.
func TestBootstrapPaceLimitOffHasNoLimit(t *testing.T) {
	rig := newPaceRig(t)
	rig.radio.airtimeLimit = false
	for short := byte(1); short <= 3*liveBootstrapPaceMax; short++ {
		if _, err := rig.radio.sendActions([]tado.Action{paceAction(short, tado.OriginRefresh)}); err != nil {
			t.Fatalf("train %d: %v", short, err)
		}
		rig.finish(252)
		rig.now = rig.now.Add(liveCSLBootstrapGlobalCooldown)
	}
	if n := len(rig.radio.pace.starts); n != 0 {
		t.Fatalf("%d slots held with the limit off", n)
	}
	if got := rig.radio.BootstrapLimit(); got != 0 {
		t.Fatalf("limit %d, want 0 for no limit", got)
	}
	rig.radio.airtimeLimit = true
	if got := rig.radio.BootstrapLimit(); got != liveBootstrapPaceMax {
		t.Fatalf("limit %d, want %d", got, liveBootstrapPaceMax)
	}
}

// With the limit off a user command still goes before a refresh that waits.
func TestBootstrapPaceLimitOffKeepsTheUserPriority(t *testing.T) {
	rig := newPaceRig(t)
	rig.radio.airtimeLimit = false
	send := func(short byte, origin tado.Origin) error {
		_, err := rig.radio.sendActions([]tado.Action{paceAction(short, origin)})
		return err
	}
	if err := send(1, tado.OriginRefresh); err != nil {
		t.Fatal(err)
	}
	var refresh, user *BootstrapPaced
	if !errors.As(send(2, tado.OriginRefresh), &refresh) || !errors.As(send(3, tado.OriginUser), &user) {
		t.Fatal("the jobs inside the cooldown were not held")
	}
	rig.now = rig.now.Add(refresh.RetryAfter)
	if !errors.As(send(2, tado.OriginRefresh), &refresh) {
		t.Fatal("the refresh went before the user command")
	}
	if err := send(3, tado.OriginUser); err != nil {
		t.Fatalf("the user command did not go: %v", err)
	}
}

// An action that forbids the train goes out on the anchor the bridge holds, also
// when it is a fragmented datagram that asks for a train by default.
func TestNoBootstrapActionNeverAsksForATrain(t *testing.T) {
	rig := newPaceRig(t)
	plain := paceAction(1, tado.OriginRefresh)
	plain.NoBootstrap = true
	if _, err := rig.radio.sendActions([]tado.Action{plain}); err != nil {
		t.Fatal(err)
	}
	block := fragmentBlock(2, 1, 3)
	for i := range block {
		block[i].NoBootstrap = true
	}
	rig.now = rig.now.Add(liveCSLBootstrapGlobalCooldown)
	if _, err := rig.radio.sendActions(block); err != nil {
		t.Fatal(err)
	}
	queued, _, _ := rig.bridge.snapshotQueue()
	if len(queued) != 4 {
		t.Fatalf("%d jobs queued, want 4", len(queued))
	}
	for i, job := range queued {
		if job.bootstrap {
			t.Fatalf("job %d asks for a train", i)
		}
	}
	if n := len(rig.radio.pace.starts); n != 0 {
		t.Fatalf("%d slots booked by actions that forbid the train", n)
	}
}

// The radio counts the jobs that reached the air by kind, with an estimate of
// their time on air: 21 wake-up frames and a Data frame, or a train of one period.
func TestRadioCountsDownlinkByKind(t *testing.T) {
	rig := newPaceRig(t)
	rig.radio.airtimeLimit = false
	measure := paceAction(1, tado.OriginRefresh)
	measure.Job, measure.Reason = "measure", "downlink zone measure 19.50 C 42% lid=4 seq=01 try=1"
	notify := paceAction(2, tado.OriginRefresh)
	notify.Job, notify.Reason = "zone", "downlink zone notify z/p lid=4 obs=3 tok=a1b2 seq=01 try=1"
	never := paceAction(3, tado.OriginRefresh)
	never.Job = "identify"
	for _, a := range []tado.Action{measure, notify, never} {
		if _, err := rig.radio.sendActions([]tado.Action{a}); err != nil {
			t.Fatal(err)
		}
		rig.now = rig.now.Add(liveCSLBootstrapGlobalCooldown)
	}
	rig.finish(22, 252, 0)
	got := rig.radio.DownlinkKinds()
	if len(got) != 2 || got["measure"].Attempts != 1 || got["notify"].Attempts != 1 {
		t.Fatalf("kinds = %+v, want one measure and one notify, nothing for the job that sent nothing", got)
	}
	short, train := got["measure"].AirtimeMS, got["notify"].AirtimeMS
	if short < 70 || short > 100 {
		t.Fatalf("a short train is %.1f ms, want about 80 to 90", short)
	}
	if train < 900 || train > 1000 {
		t.Fatalf("a train of one period is %.1f ms, want about 950", train)
	}
	rig.radio.countKind("ra", 12)
	if k := rig.radio.DownlinkKinds(); k["ra"].Attempts != 1 || k["ra"].AirtimeMS != 12 {
		t.Fatalf("ra = %+v", k["ra"])
	}
}

// A job that does not need a train is never held by the pace, with the limit on or
// off: the devices got their train and answered it, so the bridge holds their
// anchors. The four slots of the minute are taken by trains.
func TestBootstrapPaceNeverHoldsAJobOnAKnownAnchor(t *testing.T) {
	for _, limit := range []bool{true, false} {
		rig := newPaceRig(t)
		rig.radio.airtimeLimit = limit
		for short := byte(1); short <= liveBootstrapPaceMax; short++ {
			if _, err := rig.radio.sendActions([]tado.Action{paceAction(short, tado.OriginRefresh)}); err != nil {
				t.Fatal(err)
			}
			rig.finishAcked(252)
			rig.now = rig.now.Add(liveCSLBootstrapGlobalCooldown)
		}
		for block := uint16(1); block <= 3; block++ {
			if _, err := rig.radio.sendActions(fragmentBlock(1, block, 3)); err != nil {
				t.Fatalf("limit=%t: a firmware block to a device with an anchor was held: %v", limit, err)
			}
			rig.finish(22, 22, 22)
		}
		if _, err := rig.radio.sendActions([]tado.Action{realEcho(2)}); err != nil {
			t.Fatalf("limit=%t: an echo to a device with an anchor was held: %v", limit, err)
		}
		if got := lastQueued(rig); !got.bootstrap {
			t.Fatalf("limit=%t: the echo lost its bootstrap flag", limit)
		}
		if n := len(rig.radio.pace.starts); limit && n != liveBootstrapPaceMax {
			t.Fatalf("limit=%t: %d slots, want the four trains only", limit, n)
		}
	}
}

// The echo keeps its train. Over the limit it waits like a queued job: the radio
// returns the pace error, nothing goes out with no train, and the echo goes out
// with its train when the slot comes.
func TestBootstrapPaceEchoWaitsForItsTrainOverTheLimit(t *testing.T) {
	rig := newPaceRig(t)
	for short := byte(1); short <= liveBootstrapPaceMax; short++ {
		if _, err := rig.radio.sendActions([]tado.Action{paceAction(short, tado.OriginRefresh)}); err != nil {
			t.Fatal(err)
		}
		rig.finish(252)
		rig.now = rig.now.Add(liveCSLBootstrapGlobalCooldown)
	}
	queued, _, _ := rig.bridge.snapshotQueue()
	var paced *BootstrapPaced
	if _, err := rig.radio.sendActions([]tado.Action{realEcho(9)}); !errors.As(err, &paced) {
		t.Fatalf("the echo over the limit was not held: %v", err)
	}
	if after, _, _ := rig.bridge.snapshotQueue(); len(after) != len(queued) {
		t.Fatal("the echo went out while the pace held it")
	}
	rig.now = rig.now.Add(paced.RetryAfter)
	if _, err := rig.radio.sendActions([]tado.Action{realEcho(9)}); err != nil {
		t.Fatalf("the echo did not go at its slot: %v", err)
	}
	if got := lastQueued(rig); got.frame[3] != 9 || !got.bootstrap {
		t.Fatalf("the echo went with bootstrap=%t", got.bootstrap)
	}
}

// With the limit off a user command that waits in the cooldown keeps the echo to
// another device back too: the echo waits, it does not go with no train.
func TestBootstrapPaceEchoWaitsBehindAUserCommandWithTheLimitOff(t *testing.T) {
	rig := newPaceRig(t)
	rig.radio.airtimeLimit = false
	if _, err := rig.radio.sendActions([]tado.Action{paceAction(1, tado.OriginRefresh)}); err != nil {
		t.Fatal(err)
	}
	var paced *BootstrapPaced
	if _, err := rig.radio.sendActions([]tado.Action{paceAction(2, tado.OriginUser)}); !errors.As(err, &paced) {
		t.Fatalf("the user command was not held: %v", err)
	}
	rig.now = rig.now.Add(time.Second)
	if _, err := rig.radio.sendActions([]tado.Action{realEcho(9)}); !errors.As(err, &paced) {
		t.Fatalf("the echo went past a waiting user command: %v", err)
	}
	if queued, _, _ := rig.bridge.snapshotQueue(); len(queued) != 1 {
		t.Fatalf("%d jobs queued, want only the first train", len(queued))
	}
}

// Jobs that hold a slot are dropped with the queue (completion lost, flush) and
// have no result: their slots return, and a user command gets its train at once.
func TestBootstrapPaceDroppedJobsReturnTheirSlots(t *testing.T) {
	rig := newPaceRig(t)
	for short := byte(1); short <= liveBootstrapPaceMax; short++ {
		if _, err := rig.radio.sendActions([]tado.Action{paceAction(short, tado.OriginRefresh)}); err != nil {
			t.Fatal(err)
		}
		rig.now = rig.now.Add(liveCSLBootstrapGlobalCooldown)
	}
	if n := len(rig.radio.pace.starts); n != liveBootstrapPaceMax {
		t.Fatalf("%d slots booked, want %d", n, liveBootstrapPaceMax)
	}
	rig.radio.dropCSL(errCSLCompletionLost)
	if n := len(rig.radio.pace.starts); n != 0 {
		t.Fatalf("%d slots stay booked after the drop", n)
	}
	if _, err := rig.radio.sendActions([]tado.Action{paceAction(9, tado.OriginUser)}); err != nil {
		t.Fatalf("a user command waits after the drop: %v", err)
	}
}

// A queue that refuses the batch put nothing on the air: the slot returns.
func TestBootstrapPaceEnqueueErrorReturnsTheSlot(t *testing.T) {
	rig := newPaceRig(t)
	rig.bridge.enqueueErr = cc1101.ErrCSLQueueFull
	if _, err := rig.radio.sendActions([]tado.Action{paceAction(1, tado.OriginRefresh)}); !errors.Is(err, cc1101.ErrCSLQueueFull) {
		t.Fatalf("err = %v", err)
	}
	if n := len(rig.radio.pace.starts); n != 0 {
		t.Fatalf("%d slots stay booked after an enqueue error", n)
	}
}

// A job that the radio sends at once, with no queue, settles its slot like a
// queued one: a short train gives it back, a train of one period keeps it.
func TestBootstrapPaceDirectPathSettlesTheSlot(t *testing.T) {
	for _, tc := range []struct {
		sent, slots int
	}{{22, 0}, {252, 1}} {
		now := time.Unix(1000, 0)
		bridge := &orderedLiveBridge{cslResult: &cc1101.CSLTxResult{Sent: tc.sent, Ack: &cc1101.RxFrame{}}}
		radio := &Live{br: bridge, clock: func() time.Time { return now }, airtimeLimit: true}
		if _, err := radio.sendActions([]tado.Action{paceAction(1, tado.OriginRefresh)}); err != nil {
			t.Fatal(err)
		}
		if n := len(radio.pace.starts); n != tc.slots {
			t.Fatalf("sent=%d: %d slots, want %d", tc.sent, n, tc.slots)
		}
	}
}

// The text of the pace names the real cause: the cooldown, the limit of the
// minute or a user command, and never "4 a minute" with the limit off.
func TestBootstrapPacedTextNamesTheCause(t *testing.T) {
	rig := newPaceRig(t)
	rig.radio.airtimeLimit = false
	if _, err := rig.radio.sendActions([]tado.Action{paceAction(1, tado.OriginRefresh)}); err != nil {
		t.Fatal(err)
	}
	var paced *BootstrapPaced
	if _, err := rig.radio.sendActions([]tado.Action{paceAction(2, tado.OriginRefresh)}); !errors.As(err, &paced) || paced.Cause != PaceCooldown {
		t.Fatalf("err = %v, want a cooldown wait", err)
	}
	if text := paced.Error(); strings.Contains(text, "a minute") || !strings.Contains(text, "last train started") {
		t.Fatalf("cooldown text %q", text)
	}
	limited := newPaceRig(t)
	for short := byte(1); short <= liveBootstrapPaceMax; short++ {
		limited.radio.paceBootstrap(string([]byte{short}), false, true)
		limited.radio.settleBootstrapSlot(string([]byte{short}), &cc1101.CSLTxResult{Sent: 252})
	}
	over := limited.radio.paceBootstrap("x", false, true)
	if over == nil || over.Cause != PaceLimit || !strings.Contains(over.Error(), "at most 4 a minute") {
		t.Fatalf("over the limit: %v", over)
	}
	yielded := newPaceRig(t)
	yielded.radio.pace.waiters = map[string]paceWaiter{"u": {user: true, until: yielded.now.Add(time.Minute)}}
	if y := yielded.radio.paceBootstrap("r", false, true); y == nil || y.Cause != PaceYield || !strings.Contains(y.Error(), "user command") {
		t.Fatalf("yield: %v", y)
	}
}

// A busy answer of CMD_TXTRAIN counts in lbt.busy, like a busy job.
func TestBusyTrainCountsAsABusyChannel(t *testing.T) {
	rig := newPaceRig(t)
	rig.bridge.orderedLiveBridge.trainErr = cc1101.ErrCSLTxBusy
	if err := rig.radio.sendTrain([][]byte{{1, 2, 3}}); !errors.Is(err, cc1101.ErrCSLTxBusy) {
		t.Fatalf("err = %v", err)
	}
	if got := rig.radio.CSLBusy(); got != 1 {
		t.Fatalf("busy count %d, want 1", got)
	}
}

// Every path to the air has its counter: a Router Advertisement burst, the other
// trains of frames and a job that the radio sends at once with no queue.
func TestRadioCountsEveryPathToTheAir(t *testing.T) {
	now := time.Unix(1000, 0)
	bridge := &orderedLiveBridge{cslResult: &cc1101.CSLTxResult{Sent: 22, Ack: &cc1101.RxFrame{}}}
	radio := &Live{br: bridge, clock: func() time.Time { return now }}
	if err := radio.sendTrain(routerAdvertTestFrames()); err != nil {
		t.Fatal(err)
	}
	if err := radio.sendTrain([][]byte{{1, 2, 3}, {4, 5}}); err != nil {
		t.Fatal(err)
	}
	direct := paceAction(1, tado.OriginRefresh)
	direct.Job, direct.Reason = "measure", "downlink zone measure 19.50 C 42% lid=4 seq=01 try=1"
	if _, err := radio.sendActions([]tado.Action{direct}); err != nil {
		t.Fatal(err)
	}
	got := radio.DownlinkKinds()
	if ra := got["ra"]; ra.Attempts != 1 || ra.AirtimeMS < 700 {
		t.Fatalf("ra = %+v, want one burst of about 730 ms", ra)
	}
	if other := got["other"]; other.Attempts != 2 || other.AirtimeMS <= 0 {
		t.Fatalf("other = %+v, want two frames", other)
	}
	if m := got["measure"]; m.Attempts != 1 || m.AirtimeMS < 70 || m.AirtimeMS > 100 {
		t.Fatalf("measure = %+v, want one short train", m)
	}
}

// confirmDevice sends a train to the device and completes it with the Enhanced
// ACK: the host then holds the anchor of the device as known.
func confirmDevice(t *testing.T, rig *paceRig, short byte) {
	t.Helper()
	if _, err := rig.radio.sendActions([]tado.Action{paceAction(short, tado.OriginRefresh)}); err != nil {
		t.Fatal(err)
	}
	rig.finishAcked(253)
}

// The firmware sends a train to a device with a known anchor when the anchor
// missed. The job had no slot, so the completion counts the train in the window
// of the minute. The first train that ends with no ACK ends the known anchor, and
// the next job to the device is paced. A burst of jobs to several known anchors
// that all miss is not held, so more than 4 trains can start in a minute: a
// limit of the alpha.
func TestBootstrapPaceCountsTheTrainsOfAKnownAnchor(t *testing.T) {
	rig := newPaceRig(t)
	confirmDevice(t, rig, 1)
	trains, start := 1, rig.now
	rig.now = rig.now.Add(3 * time.Second)
	// A job that misses on the anchor: 22 frames and no ACK.
	if _, err := rig.radio.sendActions(fragmentBlock(1, 100, 2)); err != nil {
		t.Fatal(err)
	}
	rig.finish(22, 22)
	held := 0
	for i := 0; i < 8; i++ {
		rig.now = rig.now.Add(3 * time.Second)
		var err error
		if i%2 == 0 {
			_, err = rig.radio.sendActions(fragmentBlock(1, uint16(101+i), 2))
		} else {
			_, err = rig.radio.sendActions([]tado.Action{realEcho(1)})
		}
		var paced *BootstrapPaced
		if errors.As(err, &paced) {
			held++
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			rig.finish(253, 22)
		} else {
			rig.finish(253)
		}
		trains++
	}
	if trains > liveBootstrapPaceMax {
		t.Fatalf("%d trains in %s with the limit on, the pace allows %d a minute", trains, rig.now.Sub(start), liveBootstrapPaceMax)
	}
	if held == 0 {
		t.Fatal("no job was held once the trains of the minute were used")
	}
}

// A train that a job on a known anchor sent starts the global cooldown when it
// completes (limit on), so that the train of another device waits. A job that ran
// on the anchor sent no train and starts nothing. With the limit off nothing is
// counted and nothing waits, as before.
func TestBootstrapPaceTrainOnAKnownAnchorStartsTheCooldown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit bool
		sent  int
		held  bool
	}{{"train", true, 253, true}, {"anchored", true, 22, false}, {"train, limit off", false, 253, false}} {
		rig := newPaceRig(t)
		rig.radio.airtimeLimit = tc.limit
		confirmDevice(t, rig, 1)
		rig.now = rig.now.Add(time.Minute)
		if _, err := rig.radio.sendActions([]tado.Action{realEcho(1)}); err != nil {
			t.Fatal(err)
		}
		rig.finish(tc.sent)
		rig.now = rig.now.Add(100 * time.Millisecond)
		_, err := rig.radio.sendActions([]tado.Action{paceAction(2, tado.OriginRefresh)})
		var paced *BootstrapPaced
		if got := errors.As(err, &paced); got != tc.held {
			t.Fatalf("%s: the job to another device was held=%t (%v), want %t", tc.name, got, err, tc.held)
		}
	}
}

// The known anchor lasts 10 minutes from the last Enhanced ACK of the device.
// Past that the pace holds a job to the device like any other. A job that the
// device answers renews the window, with the bootstrap flag or without it.
func TestBootstrapPaceKnownAnchorLastsTenMinutesFromTheLastAck(t *testing.T) {
	fill := func(rig *paceRig) {
		for short := byte(2); short <= 5; short++ {
			if _, err := rig.radio.sendActions([]tado.Action{paceAction(short, tado.OriginRefresh)}); err != nil {
				t.Fatal(err)
			}
			rig.finish(253)
			rig.now = rig.now.Add(liveCSLBootstrapGlobalCooldown)
		}
	}
	asks := func(rig *paceRig) error {
		_, err := rig.radio.sendActions(fragmentBlock(1, 7, 2))
		return err
	}
	var paced *BootstrapPaced

	rig := newPaceRig(t)
	confirmDevice(t, rig, 1)
	rig.now = rig.now.Add(liveCSLBootstrapTargetCooldown + time.Minute)
	fill(rig)
	if err := asks(rig); !errors.As(err, &paced) {
		t.Fatalf("11 minutes after the ACK the block was not held: %v", err)
	}

	rig = newPaceRig(t)
	confirmDevice(t, rig, 1)
	rig.now = rig.now.Add(liveCSLBootstrapTargetCooldown - time.Minute)
	fill(rig)
	if err := asks(rig); err != nil {
		t.Fatalf("9 minutes after the ACK the block was held: %v", err)
	}

	// A plain job that the device answers (no bootstrap flag) renews the window.
	rig = newPaceRig(t)
	confirmDevice(t, rig, 1)
	rig.now = rig.now.Add(6 * time.Minute)
	if _, err := rig.radio.sendActions([]tado.Action{paceAction(1, tado.OriginUser)}); err != nil {
		t.Fatal(err)
	}
	if got := lastQueued(rig); got.bootstrap {
		t.Fatal("a job to a confirmed device asks for a train")
	}
	rig.finishAcked(22)
	rig.now = rig.now.Add(liveCSLBootstrapTargetCooldown - time.Minute)
	fill(rig)
	if err := asks(rig); err != nil {
		t.Fatalf("the block was held 9 minutes after an ACK of a job with no flag: %v", err)
	}
}

// A fragmented answer that the pace refuses goes whole on the anchor: no fragment
// asks for a train. Fragment 1 with no flag and fragment 2 with it would take a
// train with no slot, after fragment 1 ended with no anchor.
func TestBootstrapPaceRefusedFragmentedAnswerGoesWholeWithoutATrain(t *testing.T) {
	rig := newPaceRig(t)
	for short := byte(1); short <= liveBootstrapPaceMax; short++ {
		if _, err := rig.radio.sendActions([]tado.Action{paceAction(short, tado.OriginRefresh)}); err != nil {
			t.Fatal(err)
		}
		rig.finish(253)
		rig.now = rig.now.Add(liveCSLBootstrapGlobalCooldown)
	}
	before, _, _ := rig.bridge.snapshotQueue()
	answer := []tado.Action{
		{Bytes: paceFrame(9), Reason: "coap-ack 2.05 fragment=1/2", CSLTx: true, HasCSLBatch: true, CSLBatchTag: 5},
		{Bytes: paceFrame(9), Reason: "coap-ack 2.05 fragment=2/2", CSLTx: true, HasCSLBatch: true, CSLBatchTag: 5},
	}
	if _, err := rig.radio.sendActions(answer); err != nil {
		t.Fatal(err)
	}
	queued, _, _ := rig.bridge.snapshotQueue()
	queued = queued[len(before):]
	if len(queued) != 2 {
		t.Fatalf("%d frames queued, want both fragments", len(queued))
	}
	for i, job := range queued {
		if job.bootstrap {
			t.Fatalf("fragment %d asks for a train the pace refused", i+1)
		}
	}
}

// The result lists the actions that the radio sent at once and that the device
// acknowledged: the daemon spends the re-ack budget of a CON on them.
func TestImmediateResultListsTheDeliveredActions(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, ack := range []bool{true, false} {
		result := &cc1101.CSLTxResult{Sent: 22}
		if ack {
			result.Ack = &cc1101.RxFrame{}
		}
		bridge := &orderedLiveBridge{cslResult: result}
		radio := &Live{br: bridge, clock: func() time.Time { return now }}
		got, err := radio.sendActions([]tado.Action{{Bytes: paceFrame(1), CSLTx: true, Reason: "coap-ack 2.05 z/4/ow mid=7406"}})
		if err != nil && ack {
			t.Fatal(err)
		}
		if delivered := len(got.Delivered) == 1; delivered != ack {
			t.Fatalf("ack=%t: %d delivered, submitted=%d", ack, len(got.Delivered), len(got.Submitted))
		}
	}
}

// A job that leaves the waiters when it goes on a known anchor: a user waiter of
// the device does not hold the refreshes back after its job went.
func TestBootstrapPaceKnownAnchorJobLeavesTheWaiters(t *testing.T) {
	rig := newPaceRig(t)
	confirmDevice(t, rig, 1)
	rig.now = rig.now.Add(time.Minute)
	rig.radio.pace.waiters = map[string]paceWaiter{cslShort(paceFrame(1)): {user: true, until: rig.now.Add(time.Minute)}}
	if _, err := rig.radio.sendActions(fragmentBlock(1, 7, 2)); err != nil {
		t.Fatal(err)
	}
	if n := rig.radio.BootstrapWaiting(); n != 0 {
		t.Fatalf("%d devices still wait after their job went on the anchor", n)
	}
}

// The direct path counts the train of a job on a known anchor like the queue
// path: a train that the device did not answer takes a place in the window and
// ends the known anchor.
func TestBootstrapPaceDirectPathCountsTheTrainOfAKnownAnchor(t *testing.T) {
	now := time.Unix(1000, 0)
	bridge := &orderedLiveBridge{cslResult: &cc1101.CSLTxResult{Sent: 253, Ack: &cc1101.RxFrame{}}}
	radio := &Live{br: bridge, clock: func() time.Time { return now }, airtimeLimit: true}
	radio.bootstrapConfirmedAt = map[string]time.Time{cslShort(paceFrame(1)): now}
	bridge.cslResult = &cc1101.CSLTxResult{Sent: 253}
	if _, err := radio.sendActions([]tado.Action{realEcho(1)}); err != nil {
		t.Fatal(err)
	}
	if n := len(radio.pace.starts); n != 1 {
		t.Fatalf("%d trains in the window, want the one of the echo", n)
	}
	if radio.anchorKnown(paceFrame(1)) {
		t.Fatal("a train with no ACK left the anchor known")
	}
}

// The train of a job on a known anchor starts the global cooldown and the window
// of the pace only. It does not start the retry cooldown of the device: a user
// command to it, once the 2 s are over, asks for its train.
func TestBootstrapPaceUnbookedTrainLeavesTheRetryCooldownOfTheDevice(t *testing.T) {
	rig := newPaceRig(t)
	confirmDevice(t, rig, 1)
	rig.now = rig.now.Add(time.Minute)
	if _, err := rig.radio.sendActions([]tado.Action{realEcho(1)}); err != nil {
		t.Fatal(err)
	}
	rig.finish(253)
	rig.now = rig.now.Add(liveCSLBootstrapGlobalCooldown + time.Second)
	if _, err := rig.radio.sendActions([]tado.Action{paceAction(1, tado.OriginUser)}); err != nil {
		t.Fatal(err)
	}
	if got := lastQueued(rig); !got.bootstrap {
		t.Fatal("the user command goes with no train: the device is in its retry cooldown")
	}
}

// The later frames of a batch are not counted: the first frame decides. A batch
// to a device with no anchor whose second frame sends a train books no train.
func TestBootstrapPaceLaterFragmentsAreNotCounted(t *testing.T) {
	rig := newPaceRig(t)
	if _, err := rig.radio.sendActions(fragmentBlock(1, 1, 2)); err != nil {
		t.Fatal(err)
	}
	rig.finish(22, 253)
	if n := len(rig.radio.pace.starts); n != 0 {
		t.Fatalf("%d trains in the window, want none: the second frame is not counted", n)
	}
}

// With the limit off a user command after a failed block behaves as before: the
// device has no known anchor, and the command asks for a train. Two trains with no
// ACK start no cooldown of the device.
func TestBootstrapPaceUserCommandAfterAFailedBlockWithTheLimitOff(t *testing.T) {
	rig := newPaceRig(t)
	rig.radio.airtimeLimit = false
	if _, err := rig.radio.sendActions(fragmentBlock(1, 1, 2)); err != nil {
		t.Fatal(err)
	}
	rig.finish(253, 253)
	rig.now = rig.now.Add(21 * time.Second)
	if _, err := rig.radio.sendActions([]tado.Action{paceAction(1, tado.OriginUser)}); err != nil {
		t.Fatal(err)
	}
	if got := lastQueued(rig); !got.bootstrap {
		t.Fatal("the user command goes with no train after a failed block")
	}
}

// The anchor-clear rule belongs to the limit. With it off a train with no ACK
// leaves the device known, and the next single-frame job uses the anchor. With
// it on the device is not known any more and the job asks for a train.
func TestBootstrapPaceTrainWithNoAckClearsTheAnchorOnlyWithTheLimit(t *testing.T) {
	for _, limit := range []bool{false, true} {
		rig := newPaceRig(t)
		rig.radio.airtimeLimit = limit
		confirmDevice(t, rig, 1)
		rig.now = rig.now.Add(time.Minute)
		if _, err := rig.radio.sendActions(fragmentBlock(1, 5, 2)); err != nil {
			t.Fatal(err)
		}
		rig.finish(253, 22)
		rig.now = rig.now.Add(liveCSLBootstrapGlobalCooldown + time.Second)
		if _, err := rig.radio.sendActions([]tado.Action{paceAction(1, tado.OriginUser)}); err != nil {
			t.Fatal(err)
		}
		if got := lastQueued(rig); got.bootstrap != limit {
			t.Fatalf("limit=%t: the next job has bootstrap=%t, want %t", limit, got.bootstrap, limit)
		}
	}
}

// On a bridge with no queue each frame is sent one by one. A batch still counts as
// one: the first frame books the slot or counts the train, the later frames do not.
func TestBootstrapPaceDirectPathCountsABatchOnce(t *testing.T) {
	now := time.Unix(1000, 0)
	bridge := &orderedLiveBridge{cslResult: &cc1101.CSLTxResult{Sent: 253}}
	radio := &Live{br: bridge, clock: func() time.Time { return now }, airtimeLimit: true}
	radio.bootstrapConfirmedAt = map[string]time.Time{cslShort(paceFrame(1)): now}
	if _, err := radio.sendActions(fragmentBlock(1, 1, 3)); err != nil {
		t.Fatal(err)
	}
	if n := len(radio.pace.starts); n != 1 {
		t.Fatalf("%d trains counted for one batch of 3 frames, want 1", n)
	}
	// A device with no anchor: the first frame books one slot, and the later
	// frames book none.
	radio = &Live{br: bridge, clock: func() time.Time { return now }, airtimeLimit: true}
	if _, err := radio.sendActions(fragmentBlock(2, 2, 3)); err != nil {
		t.Fatal(err)
	}
	if n := len(radio.pace.starts); n != 1 {
		t.Fatalf("%d slots for one batch of 3 frames with no anchor, want 1", n)
	}
}

// On a bridge with no queue a fragmented answer that the pace refuses goes whole
// with no train: the second frame follows the decision of the first.
func TestBootstrapPaceDirectPathRefusedAnswerGoesWholeWithoutATrain(t *testing.T) {
	now := time.Unix(1000, 0)
	bridge := &orderedLiveBridge{cslResult: &cc1101.CSLTxResult{Sent: 22, Ack: &cc1101.RxFrame{}}}
	radio := &Live{br: bridge, clock: func() time.Time { return now }, airtimeLimit: true}
	for short := byte(1); short <= liveBootstrapPaceMax; short++ {
		radio.paceBootstrap(string([]byte{short}), false, true)
		radio.settleBootstrapSlot(string([]byte{short}), &cc1101.CSLTxResult{Sent: 252})
	}
	answer := []tado.Action{
		{Bytes: paceFrame(9), Reason: "coap-ack 2.05 fragment=1/2", CSLTx: true, HasCSLBatch: true, CSLBatchTag: 5},
		{Bytes: paceFrame(9), Reason: "coap-ack 2.05 fragment=2/2", CSLTx: true, HasCSLBatch: true, CSLBatchTag: 5},
	}
	if _, err := radio.sendActions(answer); err != nil {
		t.Fatal(err)
	}
	if got := bridge.cslBootstrap; len(got) != 2 || got[0] || got[1] {
		t.Fatalf("bootstrap flags %v, want both fragments with no train", got)
	}
}
