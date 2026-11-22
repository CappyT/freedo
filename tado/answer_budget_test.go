package tado

import "testing"

// conCopies builds copies of one CON of a device: the same frame again (a MAC
// retry, same seq) and a new frame with the same CoAP message (a CoAP
// retransmission, next seq and frame counter).
type conCopies struct {
	t   *testing.T
	in  *Install
	dev []byte
	pkt []byte
}

func newConCopies(t *testing.T, in *Install, r CoAPReq) *conCopies {
	va, err := in.Resolve("VA0000000001")
	if err != nil {
		t.Fatal(err)
	}
	dev := va.WireEUI()
	r.SrcEUI, r.DstEUI = dev, in.CoordinatorEUI()
	r.SrcPort, r.DstPort = coapPort, bridgeCoAPPort
	pkt, err := BuildCoAP(r)
	if err != nil {
		t.Fatal(err)
	}
	return &conCopies{t: t, in: in, dev: dev, pkt: pkt}
}

func (c *conCopies) frame(seq byte, ctr uint32) []byte {
	f, err := BuildData(c.in.NetworkKey(), seq, c.in.CoordinatorEUI(), c.dev[:5], WrapAppPayload(c.pkt, ctr, nil))
	if err != nil {
		c.t.Fatal(err)
	}
	return f.Bytes()
}

// answerOf returns the answer frame among the actions, or nil.
func answerOf(actions []Action) *Action {
	for i := range actions {
		if actions[i].Kind == ActData && actions[i].CSLTx {
			return &actions[i]
		}
	}
	return nil
}

// acked records that the device acknowledged an answer to the CON.
func (c *conCopies) acked(e *EmuState, mid uint16) {
	e.NoteAnswerSent(c.dev, mid)
	e.NoteAnswerEnded(c.dev, mid, true)
}

// The device repeats a CON at the MAC layer when it misses our Enhanced ACK, and
// at the CoAP layer when it gets no answer. A MAC retry does not use the re-ack.
// The re-ack is spent on answers that the device acknowledged, and it never uses
// the train of one period once an answer counts. A CON whose answers the device
// never acknowledged is answered again, with the train allowed.
func TestReAckBudgetIsSpentOnAnsweredFrames(t *testing.T) {
	in := synthInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	c := newConCopies(t, in, CoAPReq{Type: "CON", Code: "0.03", MID: 0x7406, Token: []byte{0x7a}, Path: homePath(in, "z", "4", "ow"), Options: []CoAPOption{optOctets}})
	first := c.frame(0x41, 700)

	a := answerOf(e.OnFrame(first))
	if a == nil || !a.AnswerCounts || a.AnswerMID != 0x7406 || a.NoBootstrap {
		t.Fatalf("first answer = %+v, want a counted answer that may use the train", a)
	}
	// The answer ended with no anchor: nobody acknowledged it. The device missed
	// our Enhanced ACK and repeats the frame.
	a = answerOf(e.OnFrame(first))
	if a == nil || a.AnswerCounts || a.NoBootstrap {
		t.Fatalf("MAC retry answer = %+v, want an answer that does not count and may use the train", a)
	}
	// It then retransmits the CON: still no answer acknowledged, so it is
	// answered again and the train is allowed.
	a = answerOf(e.OnFrame(c.frame(0x42, 701)))
	if a == nil || !a.AnswerCounts || a.NoBootstrap {
		t.Fatalf("first CoAP retransmission = %+v, want a counted answer that may use the train", a)
	}
	// An answer is acknowledged. The next copy is a re-ack with no train.
	c.acked(e, 0x7406)
	second := c.frame(0x43, 702)
	a = answerOf(e.OnFrame(second))
	if a == nil || !a.AnswerCounts || !a.NoBootstrap {
		t.Fatalf("second CoAP retransmission = %+v, want a counted re-ack with no train", a)
	}
	// A MAC retry of that frame does not use the re-ack.
	if a = answerOf(e.OnFrame(second)); a == nil || a.AnswerCounts || !a.NoBootstrap {
		t.Fatalf("MAC retry after an answer = %+v, want an answer with no train that does not count", a)
	}
	// The re-ack is acknowledged: one answer and one re-ack are the whole budget.
	c.acked(e, 0x7406)
	third := c.frame(0x44, 703)
	if a = answerOf(e.OnFrame(third)); a != nil {
		t.Fatalf("third CoAP retransmission got %q", a.Reason)
	}
	// A MAC retry of a copy that got no answer gets none either.
	for i := 0; i < 3; i++ {
		if a = answerOf(e.OnFrame(third)); a != nil {
			t.Fatalf("MAC retry %d of the copy over the budget got %q", i+1, a.Reason)
		}
	}
	if got := e.TakeOpenWindowReports(); len(got) != 1 {
		t.Errorf("open window reports=%d, want 1: a copy must not be processed again", len(got))
	}
}

// An answer in the radio queue counts against the budget at once, and leaves it
// when its completion says that the device did not acknowledge it. An answer on
// a stale anchor (sent, no ACK) does not spend the budget.
func TestReAckBudgetCountsAnswersInFlightAndForgetsUnackedOnes(t *testing.T) {
	in := synthInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	c := newConCopies(t, in, CoAPReq{Type: "CON", Code: "0.03", MID: 0x7408, Token: []byte{0x7d}, Path: homePath(in, "z", "4", "ow"), Options: []CoAPOption{optOctets}})
	if answerOf(e.OnFrame(c.frame(0x51, 800))) == nil {
		t.Fatal("no first answer")
	}
	e.NoteAnswerSent(c.dev, 0x7408) // answer 1 is in the queue
	a := answerOf(e.OnFrame(c.frame(0x52, 801)))
	if a == nil || !a.NoBootstrap {
		t.Fatalf("copy while an answer is in flight = %+v, want a re-ack with no train", a)
	}
	e.NoteAnswerSent(c.dev, 0x7408) // the re-ack is in the queue too
	if a = answerOf(e.OnFrame(c.frame(0x53, 802))); a != nil {
		t.Fatalf("copy with two answers in flight got %q", a.Reason)
	}
	// Both complete with no ACK: the device never heard them.
	e.NoteAnswerEnded(c.dev, 0x7408, false)
	e.NoteAnswerEnded(c.dev, 0x7408, false)
	a = answerOf(e.OnFrame(c.frame(0x54, 803)))
	if a == nil || a.NoBootstrap {
		t.Fatalf("copy after two answers with no ACK = %+v, want an answer that may use the train", a)
	}
}

// The same bytes again and again are MAC retries of one answered frame: none of
// them uses the re-ack, and none gets the train once an answer counts.
func TestMACRetriesDoNotUseTheReAck(t *testing.T) {
	in := synthInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	c := newConCopies(t, in, CoAPReq{Type: "CON", Code: "0.03", MID: 0x7407, Token: []byte{0x7c}, Path: homePath(in, "z", "4", "ow"), Options: []CoAPOption{optOctets}})
	first := c.frame(0x51, 800)
	if answerOf(e.OnFrame(first)) == nil {
		t.Fatal("no first answer")
	}
	c.acked(e, 0x7407)
	for i := 0; i < 3; i++ {
		a := answerOf(e.OnFrame(first))
		if a == nil || a.AnswerCounts || !a.NoBootstrap {
			t.Fatalf("MAC retry %d = %+v", i+1, a)
		}
	}
	second := c.frame(0x52, 801)
	if a := answerOf(e.OnFrame(second)); a == nil || !a.NoBootstrap {
		t.Fatalf("the CoAP retransmission after the MAC retries = %+v, want a re-ack with no train", a)
	}
	// The budget is spent. A MAC retry of the answered frame still gets its answer,
	// which does not count.
	c.acked(e, 0x7407)
	if a := answerOf(e.OnFrame(second)); a == nil || a.AnswerCounts || !a.NoBootstrap {
		t.Fatalf("MAC retry after the budget = %+v, want an answer that does not count", a)
	}
}

// The clock request is exempt: each copy gets the clock again, with the train
// allowed, as before.
func TestTimeGetStaysExemptFromTheReAckBudget(t *testing.T) {
	in := synthInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	c := newConCopies(t, in, CoAPReq{Type: "CON", Code: "0.01", MID: 0x7501, Token: []byte{0x7b}, Path: []string{"time"}})
	for i := 0; i < 4; i++ {
		a := answerOf(e.OnFrame(c.frame(byte(0x61+i), uint32(900+i))))
		if a == nil || a.NoBootstrap {
			t.Fatalf("copy %d = %+v, want an answer that may use the train", i+1, a)
		}
	}
}
