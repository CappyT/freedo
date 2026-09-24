package daemon

import (
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"freedo/tado"
)

// TestPairWindowAPI: POST /api/pair/window opens the window, a joining device
// heard under the pairing key shows in GET /api/pair/window, and POST /api/pair
// with no target queues d/pair to it.
func TestPairWindowAPI(t *testing.T) {
	clock := newTestClock(time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC))
	d, url := newHomeDaemon(t, clock)
	joiner := []byte{0x0b, 0xb1, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00}
	target := hex.EncodeToString(joiner)

	var res map[string]any
	if code := postJSON(t, http.MethodPost, url+"/api/pair", map[string]any{}, &res); code != http.StatusBadRequest {
		t.Fatalf("pair with no window: code=%d %v, want 400", code, res)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/pair/window", map[string]any{"seconds": 3600}, &res); code != http.StatusBadRequest {
		t.Fatalf("an hour-long window: code=%d %v, want 400", code, res)
	}
	var view tado.PairWindowView
	if code := postJSON(t, http.MethodPost, url+"/api/pair/window", map[string]any{"seconds": 120}, &view); code != http.StatusOK || !view.Open {
		t.Fatalf("open window: code=%d %+v", code, view)
	}

	rs := []byte{0x7b, 0x3b, 58, 0x02, 133, 0, 0, 0}
	frame, err := tado.BuildBroadcast(tado.PairingKey, 0x13, joiner, 25, 0x04, tado.WrapBroadcastPayload(rs, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.do(func(*tado.EmuState) (any, error) { d.handleFrame(frame, true); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if code := getJSON(t, url+"/api/pair/window", &view); code != http.StatusOK || len(view.Joiners) != 1 || view.Joiners[0].EUI != target {
		t.Fatalf("window view: code=%d %+v", code, view)
	}

	if code := postJSON(t, http.MethodPost, url+"/api/pair", map[string]any{}, &res); code != http.StatusOK || res["target"] != target {
		t.Fatalf("auto-targeted pair: code=%d %v", code, res)
	}
	if n := pending(t, d, target); n != 1 {
		t.Errorf("joiner holds %d queued writes, want the d/pair", n)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/pair", map[string]any{"target": "b10b"}, &res); code != http.StatusOK || res["target"] != target {
		t.Errorf("pair by the joiner's short address: code=%d %v", code, res)
	}

	if code := postJSON(t, http.MethodPost, url+"/api/pair/window", map[string]any{"seconds": 0}, &view); code != http.StatusOK || view.Open {
		t.Errorf("close window: code=%d %+v", code, view)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/pair", map[string]any{"target": "b10b"}, &res); code != http.StatusNotFound {
		t.Errorf("a joiner of a closed window: code=%d %v, want 404", code, res)
	}
}
