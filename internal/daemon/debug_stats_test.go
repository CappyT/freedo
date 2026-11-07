package daemon

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"freedo/tado"
)

type dbgStatsView struct {
	F          int                   `json:"f"`
	Pending    bool                  `json:"pending"`
	Code       string                `json:"code"`
	ReceivedAt *time.Time            `json:"received_at"`
	Stats      []tado.DebugStat      `json:"stats"`
	Share      *tado.DebugStatsShare `json:"share"`
	Error      string                `json:"error"`
}

func TestDaemonDebugStatsRefusesBadRequests(t *testing.T) {
	radio := newImmediateTestRadio(true, nil)
	_, url := newImmediateDaemon(t, tado.EmuConfig{TxCounter: 100000, NoAutoQueue: true}, radio, false)
	for _, path := range []string{
		"/api/nodes/0a06/dbg/stats",
		"/api/nodes/0a06/dbg/stats?f=3",
		"/api/nodes/0a06/dbg/stats?f=12",
		"/api/nodes/0a06/dbg/stats?f=10&f=11",
		"/api/nodes/0a06/dbg/stats?f=10&adr=0",
		"/api/nodes/RU1000000001/dbg/stats?f=11",
	} {
		var v dbgStatsView
		if code := getJSON(t, url+path, &v); code != http.StatusBadRequest || v.Error == "" {
			t.Errorf("GET %s: %d %+v, want 400", path, code, v)
		}
		if code := postJSON(t, http.MethodPost, url+path, nil, &v); code != http.StatusBadRequest || v.Error == "" {
			t.Errorf("POST %s: %d %+v, want 400", path, code, v)
		}
	}
	var v dbgStatsView
	if code := getJSON(t, url+"/api/nodes/nosuch/dbg/stats?f=10", &v); code != http.StatusNotFound {
		t.Errorf("unknown node: %d, want 404", code)
	}
	if got := radio.queuedAttempts(); len(got) != 0 {
		t.Fatalf("a refused request sent %d frames", len(got))
	}
}

func TestDaemonDebugStatsReadsOnPostOnly(t *testing.T) {
	radio := newImmediateTestRadio(true, nil)
	// The daemon binds the installation at start (SetInstall in Main): the
	// statistics read takes the device kind from it.
	// The emulator stamps each answer with its clock: the share compares the ticks with it.
	var clock atomic.Int64
	clock.Store(time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC).Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0) }
	_, url := newImmediateDaemonWithRetry(t, tado.EmuConfig{TxCounter: 100000, NoAutoQueue: true, Now: now}, radio, false, nil, func(d *daemon) {
		if _, _, err := d.emu.SetInstall(defaultInstall); err != nil {
			t.Fatal(err)
		}
	})
	const statsURL = "/api/nodes/0a06/dbg/stats?f=10"

	var v dbgStatsView
	if code := getJSON(t, url+statsURL, &v); code != http.StatusOK || v.F != 10 || v.Pending || v.Stats != nil || v.Code != "" {
		t.Fatalf("GET before any read: %d %+v", code, v)
	}
	if got := radio.queuedAttempts(); len(got) != 0 {
		t.Fatalf("GET sent %d frames", len(got))
	}

	device, err := defaultInstall.Resolve("0a06")
	if err != nil {
		t.Fatal(err)
	}
	answer := func(attempt int, body []byte, counter uint32) {
		t.Helper()
		attempts := waitForQueuedAttempts(t, radio, attempt+1)
		if len(attempts) != attempt+1 {
			t.Fatalf("attempts=%d, want %d", len(attempts), attempt+1)
		}
		req := queuedCoAP(t, attempts[attempt].Bytes)
		if req.Code != "0.02" || req.URI() != "d/dbg/m?f=10" || len(req.Payload) != 0 {
			t.Fatalf("request = %s %q %x", req.Code, req.URI(), req.Payload)
		}
		resp, err := tado.BuildCoAPAck("2.05", req.MID, req.Token, body, device.WireEUI(), defaultInstall.CoordinatorEUI(), 5683, 4005)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := tado.BuildData(defaultInstall.NetworkKey(), byte(counter), defaultInstall.CoordinatorEUI(), device.WireEUI()[:5], tado.WrapAppPayload(resp, counter, nil))
		if err != nil {
			t.Fatal(err)
		}
		radio.frames <- frame.Bytes()
	}
	waitAnswered := func(want int) dbgStatsView {
		t.Helper()
		var v dbgStatsView
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			v = dbgStatsView{}
			if code := getJSON(t, url+statsURL, &v); code == http.StatusOK && !v.Pending && len(v.Stats) > 0 && v.Stats[0].Value == int64(want) {
				return v
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("no answer with awake_ticks %d: %+v", want, v)
		return v
	}
	body := func(awake, sleep, tx uint32) []byte {
		u32 := func(x uint32) []byte { return []byte{byte(x >> 24), byte(x >> 16), byte(x >> 8), byte(x)} }
		var b []byte
		for _, t := range []tado.TLV{{Tag: "0223", Val: u32(awake)}, {Tag: "0224", Val: u32(sleep)}, {Tag: "0226", Val: u32(tx)}} {
			b = append(b, t.Encode()...)
		}
		return b
	}

	var q struct {
		Queued  bool   `json:"queued"`
		Error   string `json:"error"`
		Pending int    `json:"pending"`
	}
	if code := postJSON(t, http.MethodPost, url+statsURL, nil, &q); code != http.StatusOK || !q.Queued || q.Pending != 1 {
		t.Fatalf("POST: %d %+v", code, q)
	}
	if code := getJSON(t, url+statsURL, &v); code != http.StatusOK || !v.Pending {
		t.Fatalf("GET while pending: %d %+v", code, v)
	}
	const hz = tado.DebugStatsTickHz
	answer(0, body(1000*hz, 3000*hz, 0), 100001)
	first := waitAnswered(1000 * hz)
	if first.Code != "2.05" || first.ReceivedAt == nil || first.Share != nil || first.Stats[0].Name != "awake_ticks" {
		t.Fatalf("first answer: %+v", first)
	}

	if code := postJSON(t, http.MethodPost, url+statsURL, nil, &q); code != http.StatusOK || !q.Queued {
		t.Fatalf("second POST: %d %+v", code, q)
	}
	// 100 s later: 25 s awake, 75 s asleep, 1 s transmitting.
	clock.Add(100)
	answer(1, body(1025*hz, 3075*hz, hz), 100002)
	second := waitAnswered(1025 * hz)
	if second.Share == nil || second.Share.AwakePct != 25 || second.Share.TxPct != 1 {
		t.Fatalf("share after two answers: %+v", second.Share)
	}
}
