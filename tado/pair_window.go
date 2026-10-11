package tado

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// The commercial bridge keeps a pair window open while it waits for a device
// to join. In that window csl_rx_frame_dispatch@0x36088 retries every frame
// that fails the MIC under the network key with the pairing key (flag
// 0x20006e14, key copy 0x20006e04, "tado pairing key" at 0x5a884). A frame that
// verifies there tags its sender with key index 2, and csl_tx_statemachine
// secures the frames to that sender with the pairing key. A joining valve sends
// its Enhanced Beacon Requests and Router Solicitations under the pairing key
// (coap_sniff_1786619225), so the window learns its address before any d/pair.
//
// freedo keeps the same fallback, bounded to a window the operator opens. It
// records the sender. It answers only the two frames a joiner needs before
// d/pair: the Enhanced Beacon Request with an Enhanced Beacon (beacon.go) and
// the Router Solicitation with the doubled advertisement (BeaconBurst). Bridge
// firmware v2.9 answers the Beacon Request itself, 4 ms after it, which the
// host cannot do: the window then tells the radio to send the advertisement
// with Frame Pending too.

// MaxPairWindow bounds a pair window. The valve's own commissioning window
// lasts two minutes.
const MaxPairWindow = 10 * time.Minute

// ErrPairTarget means POST d/pair has no single device to target.
var ErrPairTarget = errors.New("tado: no single joining device: name the target")

// PairJoiner is a device heard under PairingKey while the pair window was open.
type PairJoiner struct {
	EUI       string    `json:"eui"` // wire EUI hex, a valid d/pair target
	Short     string    `json:"short"`
	Label     string    `json:"label"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
	Frames    int       `json:"frames"`
	LastClass string    `json:"lastClass"` // frame class of the last one: Data or Broadcast
}

// PairWindowView is the state of the pair window.
type PairWindowView struct {
	Open   bool      `json:"open"`
	Opened time.Time `json:"opened,omitzero"`
	Until  time.Time `json:"until,omitzero"`
	// Responder says that the radio answers the Enhanced Beacon Requests of
	// this window and sends the doubled advertisement.
	Responder bool         `json:"responder"`
	Joiners   []PairJoiner `json:"joiners"`
}

type pairWindow struct {
	opened, until time.Time
	joiners       map[string]*PairJoiner // wire EUI hex
	// netHeard holds, per wire EUI hex, the last frame that verified under the
	// network key while the window was open.
	netHeard  map[string]time.Time
	responder bool
}

// OpenPairWindow opens the pair window for d, capped at MaxPairWindow, and
// forgets the devices of the previous window. A zero or negative d closes it.
func (e *EmuState) OpenPairWindow(d time.Duration) {
	if d <= 0 {
		e.ClosePairWindow()
		return
	}
	now := e.cfg.Now()
	e.pair = pairWindow{
		opened: now, until: now.Add(min(d, MaxPairWindow)),
		joiners: map[string]*PairJoiner{}, netHeard: map[string]time.Time{},
	}
}

// ClosePairWindow closes the pair window. The devices it learned stay visible.
func (e *EmuState) ClosePairWindow() {
	if e.pairWindowOpen() {
		e.pair.until = e.cfg.Now()
	}
	e.pair.responder = false
}

func (e *EmuState) pairWindowOpen() bool {
	return !e.pair.until.IsZero() && e.cfg.Now().Before(e.pair.until)
}

// SetPairResponder records whether the radio answers the Enhanced Beacon
// Requests of the open window and takes the Frame Pending advertisement
// (bridge firmware v2.9). With it the host sends no beacon and doubles the
// advertisement; without it the host sends the beacon itself, late, and the
// advertisement stays single.
func (e *EmuState) SetPairResponder(on bool) {
	e.pair.responder = on && e.pairWindowOpen()
}

// ReservePairBeacons takes n MAC seq and n frame counter values for the
// beacons the radio sends on its own, and returns the first of each. No host
// frame reuses them.
func (e *EmuState) ReservePairBeacons(n int) (byte, uint32, error) {
	if err := e.reserveTx(n); err != nil {
		return 0, 0, err
	}
	seq, ctr := e.seq+1, e.tx+1
	e.seq += byte(n)
	e.tx += uint32(n)
	return seq, ctr, nil
}

// PairWindow returns the window state and the devices it learned, oldest first.
func (e *EmuState) PairWindow() PairWindowView {
	open := e.pairWindowOpen()
	v := PairWindowView{
		Open: open, Opened: e.pair.opened, Until: e.pair.until,
		Responder: open && e.pair.responder, Joiners: []PairJoiner{},
	}
	for _, j := range e.pair.joiners {
		v.Joiners = append(v.Joiners, *j)
	}
	sort.Slice(v.Joiners, func(i, j int) bool {
		if !v.Joiners[i].FirstSeen.Equal(v.Joiners[j].FirstSeen) {
			return v.Joiners[i].FirstSeen.Before(v.Joiners[j].FirstSeen)
		}
		return v.Joiners[i].EUI < v.Joiners[j].EUI
	})
	return v
}

// PairAutoTarget returns the wire EUI hex of the one device the open window
// learned. It fails when the window is closed or learned none or several.
func (e *EmuState) PairAutoTarget() (string, error) {
	if !e.pairWindowOpen() || len(e.pair.joiners) != 1 {
		return "", ErrPairTarget
	}
	for k := range e.pair.joiners {
		return k, nil
	}
	return "", ErrPairTarget
}

// PairJoinerTarget returns the wire EUI hex of the device of the open window
// that ref names, by wire EUI hex or short address.
func (e *EmuState) PairJoinerTarget(ref string) (string, bool) {
	if !e.pairWindowOpen() {
		return "", false
	}
	ref = strings.ToLower(strings.TrimSpace(ref))
	for k, j := range e.pair.joiners {
		if ref == k || ref == j.Short {
			return k, true
		}
	}
	return "", false
}

// notePairingFrame records the sender of a frame that verified under
// PairingKey. Our own source prefix is the commercial bridge, not a device.
func (e *EmuState) notePairingFrame(class string, src []byte) {
	if bytes.Equal(src[:5], e.selfEUI[:5]) {
		return
	}
	key := hexEUI(src)
	now := e.cfg.Now()
	j, ok := e.pair.joiners[key]
	if !ok {
		j = &PairJoiner{EUI: key, Short: ShortFromWire(src), FirstSeen: now}
		e.pair.joiners[key] = j
	}
	j.Label, j.LastSeen, j.LastClass = e.name(src), now, class
	j.Frames++
	e.stats.PairingKeyFrames++
}

// notePairingData retries a Data frame that failed the MIC under the network
// key with PairingKey, while the pair window is open. It reports whether the
// frame verified there.
func (e *EmuState) notePairingData(class FrameClass, raw []byte) bool {
	if !e.pairWindowOpen() {
		return false
	}
	f, err := ParseData(PairingKey, raw)
	if err != nil {
		return false
	}
	e.notePairingFrame(class.String(), f.SrcEUI())
	e.log.Log(class.String(), e.name(f.SrcEUI()), e.name(f.DstEUI), rsnPairingKey, loggedFrameHex(raw))
	return true
}

// notePairingBroadcast is notePairingData for a 0x49e8 broadcast.
func (e *EmuState) notePairingBroadcast(raw []byte) (*BroadcastMsg, bool) {
	if !e.pairWindowOpen() {
		return nil, false
	}
	b, err := ParseBroadcast(PairingKey, raw)
	if err != nil {
		return nil, false
	}
	e.notePairingFrame("Broadcast", b.SrcEUI)
	e.log.Log("Broadcast", e.name(b.SrcEUI), "ffff", rsnPairingKey, hex.EncodeToString(raw))
	return b, true
}

// answerPairingSolicit answers a Router Solicitation under the pairing key with
// the doubled advertisement, as the commercial bridge does
// (coap_sniff_1786619225 lines 3823, 35818, 37195, each answered within 1.4 s).
// A single advertisement carries only the network key, which the joiner cannot
// read, so without the doubling there is nothing to answer with.
func (e *EmuState) answerPairingSolicit(b *BroadcastMsg) []Action {
	if b.ICMPType != icmpRouterSolicit || !e.pair.responder || e.BridgeSeen() || !e.armed() {
		return nil
	}
	actions, due, err := e.beaconIfDue(routerSolicitAnswerWindow)
	if err != nil {
		e.log.Log("Broadcast", e.name(b.SrcEUI), "ffff", rsnBuildFail, "")
		return nil
	}
	e.stats.RouterSolicitationsAnswered++
	if !due {
		return nil
	}
	return actions
}

// onBeaconRequest takes an Enhanced Beacon Request. Only a device that looks
// for a coordinator sends one, under the pairing key, so outside the window
// the request is dropped. In the window it names a joiner, and it gets the
// Enhanced Beacon from the host when the radio does not answer it itself.
func (e *EmuState) onBeaconRequest(raw []byte) []Action {
	src := ""
	if len(raw) == beaconRequestLen {
		src = e.name(raw[7:15])
	}
	if !e.pairWindowOpen() {
		e.log.Log(ClassBeaconRequest.String(), src, "ffff", rsnPairWindowClosed, hex.EncodeToString(raw))
		return nil
	}
	r, err := ParseBeaconRequest(PairingKey, raw)
	if err != nil {
		reason := rsnMalformed
		if errors.Is(err, ErrMICFail) {
			reason = rsnMICFail
		}
		e.log.Log(ClassBeaconRequest.String(), src, "ffff", reason, hex.EncodeToString(raw))
		return nil
	}
	if bytes.Equal(r.SrcEUI[:5], e.selfEUI[:5]) {
		return nil
	}
	e.notePairingFrame(ClassBeaconRequest.String(), r.SrcEUI)
	e.log.Log(ClassBeaconRequest.String(), src, "ffff", rsnPairingKey, hex.EncodeToString(raw))
	if e.pair.responder || e.BridgeSeen() {
		return nil
	}
	if !e.armed() {
		e.log.Log(ClassBeaconRequest.String(), src, "ffff", rsnPairingKey+" suppressed=tx-not-armed", "")
		return nil
	}
	ctr, err := e.nextTx()
	if err != nil {
		e.log.Log(ClassBeaconRequest.String(), src, "ffff", rsnBuildFail, err.Error())
		return nil
	}
	seq := e.nextSeq()
	eb, err := BuildEnhancedBeacon(PairingKey, seq, r.SrcEUI, e.selfEUI, ctr)
	if err != nil {
		e.log.Log(ClassBeaconRequest.String(), src, "ffff", rsnBuildFail, err.Error())
		return nil
	}
	return []Action{{
		Kind:   ActBeacon,
		Bytes:  eb,
		Reason: fmt.Sprintf("enhanced-beacon seq=%02x ctr=%d", seq, ctr),
		Node:   src,
	}}
}

// noteNetworkKeyFrame records a frame from src that verified under the
// network key while the pair window is open.
func (e *EmuState) noteNetworkKeyFrame(src []byte) {
	if e.pairWindowOpen() {
		e.pair.netHeard[hexEUI(src)] = e.cfg.Now()
	}
}

// pairFirst moves a queued d/pair of key to the head, ahead of every downlink
// the radio does not hold. The commercial bridge sends d/pair ahead of the
// frames it holds for the joiner: in coap_sniff_1786619225 the d/config MID
// efaf still waits for its CoAP answer after the Enhanced ACK of line 4102, and
// d/pair goes on line 4634 without it. An attempt the radio holds
// (awaitingCSL) cannot be recalled, so it keeps the head until it completes.
// Any other attempt in flight waits behind d/pair with its tries, MID and MAC
// seq. Its answer no longer closes it, and its next attempt asks again under
// the same MID. A firmware upload owns the queue, so d/pair waits for it.
func (e *EmuState) pairFirst(key string) {
	q := e.queue[key]
	if len(q) < 2 || q[0].kind == "pair" || q[0].awaitingCSL || e.fwLock[key] {
		return
	}
	i := slices.IndexFunc(q, func(d *queuedDownlink) bool { return d.kind == "pair" })
	if i < 0 {
		return
	}
	head, pair := q[0], q[i]
	head.inFlight, head.retryAt, head.deferred = false, time.Time{}, false
	copy(q[1:i+1], q[:i])
	q[0] = pair
}

// pairKeyFor returns the key that secures a d/pair attempt to wire. The
// commercial bridge sends it under the network key to a device that already
// talks the network key (coap_sniff_1786619225, answered 2.03), and under the
// pairing key to one that joins. freedo takes the network key only when the
// open window heard the device under it after its last pairing-key frame:
// without that evidence a device waiting to join cannot read anything else.
func (e *EmuState) pairKeyFor(wire []byte) []byte {
	if !e.pairWindowOpen() {
		return PairingKey
	}
	key := hexEUI(wire)
	net, ok := e.pair.netHeard[key]
	if !ok {
		return PairingKey
	}
	if j, ok := e.pair.joiners[key]; ok && !net.After(j.LastSeen) {
		return PairingKey
	}
	return e.key
}
