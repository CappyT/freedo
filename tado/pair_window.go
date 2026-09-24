package tado

import (
	"bytes"
	"encoding/hex"
	"errors"
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
// records the sender and nothing else: it neither acknowledges nor processes
// the frame.

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
	Open    bool         `json:"open"`
	Opened  time.Time    `json:"opened,omitzero"`
	Until   time.Time    `json:"until,omitzero"`
	Joiners []PairJoiner `json:"joiners"`
}

type pairWindow struct {
	opened, until time.Time
	joiners       map[string]*PairJoiner // wire EUI hex
	// netHeard holds, per wire EUI hex, the last frame that verified under the
	// network key while the window was open.
	netHeard map[string]time.Time
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
}

func (e *EmuState) pairWindowOpen() bool {
	return !e.pair.until.IsZero() && e.cfg.Now().Before(e.pair.until)
}

// PairWindow returns the window state and the devices it learned, oldest first.
func (e *EmuState) PairWindow() PairWindowView {
	v := PairWindowView{Open: e.pairWindowOpen(), Opened: e.pair.opened, Until: e.pair.until, Joiners: []PairJoiner{}}
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
	e.log.Log(class.String(), e.name(f.SrcEUI()), e.name(f.DstEUI), rsnPairingKey, f.Hex())
	return true
}

// notePairingBroadcast is notePairingData for a 0x49e8 broadcast.
func (e *EmuState) notePairingBroadcast(raw []byte) bool {
	if !e.pairWindowOpen() {
		return false
	}
	b, err := ParseBroadcast(PairingKey, raw)
	if err != nil {
		return false
	}
	e.notePairingFrame("Broadcast", b.SrcEUI)
	e.log.Log("Broadcast", e.name(b.SrcEUI), "ffff", rsnPairingKey, hex.EncodeToString(raw))
	return true
}

// noteNetworkKeyFrame records a frame from src that verified under the
// network key while the pair window is open.
func (e *EmuState) noteNetworkKeyFrame(src []byte) {
	if e.pairWindowOpen() {
		e.pair.netHeard[hexEUI(src)] = e.cfg.Now()
	}
}

// pairKeyFor returns the key that secures a d/pair to wire. The commercial
// bridge sends it under the network key to a device that already talks the
// network key (coap_sniff_1786619225, answered 2.03), and under the pairing
// key to one that joins. freedo takes the network key only when the open window
// heard the device under it after its last pairing-key frame: without that
// evidence a device waiting to join cannot read anything else.
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
