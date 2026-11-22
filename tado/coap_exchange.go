package tado

import (
	"bytes"
	"time"
)

// CoAP exchange state the emulator keeps per peer: the answers to the CONs it
// served, for deduplication, and the documents of the Block2 transfers in
// progress.

// exchangeLifetime is RFC 7252 EXCHANGE_LIFETIME with the default transmission
// parameters: how long a peer can still retransmit a CON.
const exchangeLifetime = 247 * time.Second

// maxAnswersPerSource bounds the answers held per device. A Contiki device
// keeps one CON outstanding (NSTART 1), so a few entries cover every
// retransmission.
const maxAnswersPerSource = 8

// maxBlock2Transfers bounds the Block2 transfers held at once.
const maxBlock2Transfers = 32

// midKey names a CoAP exchange: the Message ID space belongs to the endpoint
// that allocated the MID, so the MID alone collides across devices.
type midKey struct {
	eui string // wire EUI hex of the endpoint that allocated the MID
	mid uint16
}

// answeredCON is the answer to one CON the emulator served itself.
type answeredCON struct {
	request []byte // the CoAP message answered, token included
	pkt     []byte // the answer datagram
	dst     []byte
	code    string
	uri     string
	at      time.Time
	// acked counts the answers to this CON that the device acknowledged with an
	// Enhanced ACK: the first one, and the re-acks of CoAP retransmissions.
	// inflight counts the answers that the radio holds and has not completed: they
	// count against the budget until their completion says whether the device
	// heard them. An answer to a MAC retry of the same frame counts in neither.
	acked, inflight int
	// lastAnswered says that the last copy of the CON that was not a MAC retry
	// got an answer. A MAC retry repeats the answer only then.
	lastAnswered bool
}

// answers is the number of answers that count against the budget of re-acks.
func (a *answeredCON) answers() int { return a.acked + a.inflight }

// rememberAnswer files the answer to a CON, so a retransmission of it is
// answered again without being processed again (RFC 7252 4.5).
func (e *EmuState) rememberAnswer(dst []byte, m *CoAPMessage, pkt []byte, code string) {
	now := e.cfg.Now()
	if e.answered == nil {
		e.answered = map[midKey]*answeredCON{}
	}
	src := hexEUI(dst)
	var oldest midKey
	n := 0
	for k, a := range e.answered {
		if now.Sub(a.at) >= exchangeLifetime {
			delete(e.answered, k)
			continue
		}
		if k.eui != src {
			continue
		}
		if n == 0 || a.at.Before(e.answered[oldest].at) {
			oldest = k
		}
		n++
	}
	if n >= maxAnswersPerSource {
		delete(e.answered, oldest)
	}
	e.answered[midKey{src, m.MID}] = &answeredCON{
		request: append([]byte(nil), m.Raw...),
		pkt:     append([]byte(nil), pkt...),
		dst:     append([]byte(nil), dst...),
		code:    code,
		uri:     m.URI(),
		at:      now,

		lastAnswered: true,
	}
}

// NoteAnswerSent records that an answer to the CON of dst with message ID mid is
// in the radio queue. It counts against the budget of re-acks until
// NoteAnswerEnded. The daemon calls it for an action with AnswerCounts.
func (e *EmuState) NoteAnswerSent(dst []byte, mid uint16) {
	if a, ok := e.answered[midKey{hexEUI(dst), mid}]; ok {
		a.inflight++
	}
}

// NoteAnswerEnded records the end of an answer: the device acknowledged it with
// an Enhanced ACK, or not. An answer with no ACK (a stale anchor, a lost frame)
// does not spend the budget.
func (e *EmuState) NoteAnswerEnded(dst []byte, mid uint16, acked bool) {
	a, ok := e.answered[midKey{hexEUI(dst), mid}]
	if !ok {
		return
	}
	if a.inflight > 0 {
		a.inflight--
	}
	if acked {
		a.acked++
	}
}

// answerDuplicate answers a CON that the device sent again, as long as an answer
// is still due. mac says that the copy is a MAC retry of the same frame (same
// sequence number): the device missed the Enhanced ACK. It repeats the answer
// only when the copy it retries was answered, and the answer does not count
// against the budget. A CoAP retransmission (a new frame) gets one re-ack once an
// answer counts, and no more: each one would cost a train. An answer counts when
// the device acknowledged it, or while it is in the radio queue. A CON whose
// answers the device never acknowledged is answered again, with the train
// allowed, because the device still waits for it. Once an answer counts, a re-ack
// goes out on the anchor the bridge holds, never with a train of one period
// (Action.NoBootstrap). A CON with the same MID and other bytes is a new request,
// as after a device restart, and is processed as one.
func (e *EmuState) answerDuplicate(src []byte, m *CoAPMessage, srcLbl string, mac bool) ([]Action, bool) {
	if m == nil || m.Type != "CON" || !isRequestCode(m.Code) {
		return nil, false
	}
	a, ok := e.answered[midKey{hexEUI(src), m.MID}]
	if !ok || e.cfg.Now().Sub(a.at) >= exchangeLifetime || !bytes.Equal(a.request, m.Raw) {
		return nil, false
	}
	switch {
	case mac && !a.lastAnswered:
		return nil, true
	case !mac && a.answers() >= 2:
		a.lastAnswered = false
		return nil, true
	}
	actions, err := e.coapReplyActions(a.pkt, a.dst, a.code, a.uri, m.MID, srcLbl)
	if err != nil {
		return nil, false
	}
	for i := range actions {
		actions[i].Reason += " (duplicate)"
		actions[i].NoBootstrap = a.answers() >= 1
	}
	if !mac {
		a.lastAnswered = true
		if len(actions) > 0 {
			actions[0].AnswerCounts, actions[0].AnswerMID = true, m.MID
		}
	}
	return actions, true
}

// block2Transfer is the document a Block2 transfer serves, fixed at block 0.
type block2Transfer struct {
	body, etag []byte
	until      time.Time
}

// block2Document returns the document to serve one block from. Block 0 starts
// a transfer on the current document. A later block comes from the document of
// its transfer, so a document that changes in between never mixes into it.
// ok is false for a later block with no live transfer: the answer then refuses
// it, and the device restarts at block 0.
func (e *EmuState) block2Document(dst []byte, uri string, offset int, body, etag []byte) (docBody, docETag []byte, ok bool) {
	now := e.cfg.Now()
	key := hexEUI(dst) + "|" + uri
	if offset > 0 {
		t, ok := e.block2Tx[key]
		if !ok || !now.Before(t.until) {
			delete(e.block2Tx, key)
			return nil, nil, false
		}
		t.until = now.Add(exchangeLifetime)
		e.block2Tx[key] = t
		return t.body, t.etag, true
	}
	if e.block2Tx == nil {
		e.block2Tx = map[string]block2Transfer{}
	}
	oldest := ""
	for k, t := range e.block2Tx {
		if !now.Before(t.until) {
			delete(e.block2Tx, k)
			continue
		}
		if oldest == "" || t.until.Before(e.block2Tx[oldest].until) {
			oldest = k
		}
	}
	if _, held := e.block2Tx[key]; !held && len(e.block2Tx) >= maxBlock2Transfers {
		delete(e.block2Tx, oldest)
	}
	e.block2Tx[key] = block2Transfer{
		body:  append([]byte(nil), body...),
		etag:  append([]byte(nil), etag...),
		until: now.Add(exchangeLifetime),
	}
	return body, etag, true
}

// endBlock2Transfer drops a transfer whose last block went out.
func (e *EmuState) endBlock2Transfer(dst []byte, uri string) {
	delete(e.block2Tx, hexEUI(dst)+"|"+uri)
}
