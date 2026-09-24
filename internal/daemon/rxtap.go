package daemon

// rxtap: GET /api/debug/rx, a chunked NDJSON stream of every frame the engine
// receives. One line per frame, no framing protocol on top, so `curl` and `jq`
// read it as it arrives.

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"freedo/tado"
)

const (
	// tapBuffer is how far one client may fall behind before it loses frames.
	tapBuffer = 256
	// maxTaps bounds the copies publishRx makes on the radio path.
	maxTaps      = 4
	tapKeepalive = 30 * time.Second
	// tapWriteBudget bounds one write to a client. Without it a client that stops
	// reading blocks its handler until TCP gives up, holding one of maxTaps slots.
	tapWriteBudget = 10 * time.Second
)

type rxTap struct {
	c       chan []byte
	dropped atomic.Uint64
}

func (d *daemon) addTap() (*rxTap, bool) {
	d.tapsMu.Lock()
	defer d.tapsMu.Unlock()
	if len(d.taps) >= maxTaps {
		return nil, false
	}
	tap := &rxTap{c: make(chan []byte, tapBuffer)}
	d.taps[tap] = struct{}{}
	return tap, true
}

func (d *daemon) removeTap(tap *rxTap) {
	d.tapsMu.Lock()
	delete(d.taps, tap)
	d.tapsMu.Unlock()
}

// publishRx copies one received frame to every open tap. It runs on the engine
// goroutine, which owns the radio, so it must never block: a client that cannot
// keep up loses the frame and gets the running drop count instead.
//
// The tap channels are never closed. The publisher sends under tapsMu and
// removeTap deletes under the same mutex, so a send can never race a close, and
// leaving them open removes the need for a close protocol at all.
func (d *daemon) publishRx(frame []byte) {
	d.tapsMu.Lock()
	defer d.tapsMu.Unlock()
	if len(d.taps) == 0 {
		return
	}
	copied := append([]byte(nil), frame...)
	for tap := range d.taps {
		select {
		case tap.c <- copied:
		default:
			tap.dropped.Add(1)
		}
	}
}

// closeTaps releases every consumer. Without it one open tap would hold the
// whole HTTP drain budget on shutdown.
func (d *daemon) closeTaps() {
	d.tapStopOnce.Do(func() { close(d.tapStop) })
}

func (d *daemon) tapCount() int {
	d.tapsMu.Lock()
	defer d.tapsMu.Unlock()
	return len(d.taps)
}

func (d *daemon) getDebugRx(w http.ResponseWriter, r *http.Request) {
	tap, ok := d.addTap()
	if !ok {
		writeErr(w, http.StatusTooManyRequests, "too many open rx taps")
		return
	}
	defer d.removeTap(tap)

	// Per-client dissector on the HTTP goroutine: it joins the fragments of this
	// stream alone and touches neither the engine nor another client.
	var dis *tado.Dissector
	if inst := d.inst.Load(); r.URL.Query().Get("dissect") != "0" && inst != nil {
		dis = inst.NewDissector()
	}
	h := w.Header()
	h.Set("Content-Type", "application/x-ndjson")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	send := func(line map[string]any) bool {
		// A ResponseWriter without SetWriteDeadline keeps the old blocking write.
		if err := rc.SetWriteDeadline(time.Now().Add(tapWriteBudget)); err != nil &&
			!errors.Is(err, http.ErrNotSupported) {
			return false
		}
		if err := enc.Encode(line); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	// The hello line proves the stream is open before any frame arrives.
	if !send(map[string]any{"type": "hello", "mode": d.mode}) {
		return
	}

	keepalive := time.NewTicker(tapKeepalive)
	defer keepalive.Stop()
	for {
		select {
		case frame := <-tap.c:
			line := map[string]any{
				"type":    "frame",
				"t":       time.Now().Format(time.RFC3339Nano),
				"class":   tado.Classify(frame).String(),
				"hex":     hex.EncodeToString(frame),
				"dropped": tap.dropped.Load(),
			}
			if dis != nil {
				line["dissect"] = dis.Dissect(frame)
			}
			if !send(line) {
				return
			}
		case <-keepalive.C:
			if !send(map[string]any{"type": "keepalive", "t": time.Now().Format(time.RFC3339Nano)}) {
				return
			}
		case <-r.Context().Done():
			return
		case <-d.tapStop:
			return
		}
	}
}
