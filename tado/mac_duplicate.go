package tado

import (
	"bytes"
	"time"
)

// A device that misses our Enhanced ACK sends the frame again with the same MAC
// sequence number and datagram under the next frame counter, 1.5 to 3.6 s later
// (capture-app-20260907 and the coap_sniff logs). The window bounds the match in
// time, because a device restarts its sequence numbers after a reboot.
const macDuplicateWindow = 10 * time.Second

// macRxMark is the last Data frame taken from one source.
type macRxMark struct {
	seq byte
	pkt []byte // the datagram, without the counter and the app trailer
	at  time.Time
}

// macDuplicate reports whether f repeats the last frame taken from srcEUI: the
// same sequence number and the same datagram inside macDuplicateWindow. Only the
// frame counter and the app trailer differ. Other bytes under the same sequence
// number are a new frame, so a match cannot drop one.
func (e *EmuState) macDuplicate(srcEUI []byte, f *Frame) bool {
	if len(f.Plaintext) < 10 {
		return false
	}
	pkt := f.Plaintext[8 : len(f.Plaintext)-2]
	now := e.cfg.Now()
	key := hexEUI(srcEUI)
	last, ok := e.macRx[key]
	if ok && last.seq == f.Seq && now.Sub(last.at) < macDuplicateWindow && bytes.Equal(last.pkt, pkt) {
		return true
	}
	if e.macRx == nil {
		e.macRx = map[string]macRxMark{}
	}
	e.macRx[key] = macRxMark{seq: f.Seq, pkt: append([]byte(nil), pkt...), at: now}
	return false
}

// macDuplicateActions answers a MAC retransmission of a frame already taken. It
// gets the Enhanced ACK the device missed, a CON gets the answer it was served,
// and the device's rendezvous serves its queue. Nothing else runs again.
func (e *EmuState) macDuplicateActions(f *Frame, srcEUI []byte, srcLbl string) []Action {
	if !bytes.Equal(f.DstEUI, e.selfEUI) {
		return nil
	}
	actions := e.enhAckAction(f, srcEUI, srcLbl)
	if !IsFragment(f.Plaintext) {
		if dup, ok := e.answerDuplicate(srcEUI, ParseApp(f.Plaintext).coapOrNil(), srcLbl); ok {
			actions = append(actions, dup...)
		}
	}
	if a, ok := e.nextDownlink(srcEUI, srcLbl); ok {
		actions = append(actions, a...)
	}
	return actions
}
