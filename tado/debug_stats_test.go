package tado

import (
	"bytes"
	"encoding/hex"
	"io"
	"testing"
	"time"
)

func debugStatsBody(tlvs ...TLV) []byte { return encodeTLVs(tlvs) }

func u32(v uint32) []byte { return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)} }

func TestDecodeDebugStats(t *testing.T) {
	got, err := DecodeDebugStats(debugStatsBody(
		TLV{Tag: "0223", Val: u32(0x00010000)},
		TLV{Tag: "023d", Val: []byte{0xff, 0xb5}},
		TLV{Tag: "023e", Val: []byte{0x00, 0x30}},
		TLV{Tag: "0230", Val: []byte{0xff, 0xff}},
		TLV{Tag: "0241", Val: []byte{0x00, 0x01}},
	))
	if err != nil {
		t.Fatal(err)
	}
	want := []DebugStat{
		{Tag: "0223", Name: "awake_ticks", Value: 65536},
		{Tag: "023d", Name: "rssi_dBm", Value: -75},
		{Tag: "023e", Name: "lqi", Value: 48},
		{Tag: "0230", Name: "rx_ok", Value: 65535},
		{Tag: "0241", Value: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("stat %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if s, err := DecodeDebugStats(nil); err != nil || len(s) != 0 {
		t.Errorf("empty body = %+v, %v", s, err)
	}
	if _, err := DecodeDebugStats(debugStatsBody(TLV{Tag: "0230", Val: []byte{1, 2, 3}})); err == nil {
		t.Error("a 3-byte value decoded")
	}
	if _, err := DecodeDebugStats([]byte{0x02}); err == nil {
		t.Error("a body that is not a TLV stream decoded")
	}
	whole := debugStatsBody(TLV{Tag: "0230", Val: []byte{0, 1}}, TLV{Tag: "023b", Val: []byte{0, 2}})
	for name, body := range map[string][]byte{"truncated": whole[:len(whole)-1], "trailing byte": append(whole, 0)} {
		if s, err := DecodeDebugStats(body); err == nil {
			t.Errorf("%s body decoded to %+v", name, s)
		}
	}
}

func TestDebugStatsShareBetween(t *testing.T) {
	at := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	sample := func(d time.Duration, tlvs ...TLV) DebugStatsSample {
		return DebugStatsSample{At: at.Add(d), Body: debugStatsBody(tlvs...)}
	}
	const hz = DebugStatsTickHz
	// 100 s between the readings: 25 s awake, 75 s asleep, 1 s TX, 3 s RX.
	prev := sample(0, TLV{Tag: "0223", Val: u32(1000 * hz)}, TLV{Tag: "0224", Val: u32(3000 * hz)})
	cur := sample(100*time.Second,
		TLV{Tag: "0223", Val: u32(1025 * hz)}, TLV{Tag: "0224", Val: u32(3075 * hz)},
		TLV{Tag: "0226", Val: u32(1 * hz)}, TLV{Tag: "0227", Val: u32(3 * hz)})
	share, ok := DebugStatsShareBetween(prev, cur)
	if !ok || share.AwakePct != 25 || share.TxPct != 1 || share.RxPct != 3 || share.IntervalS != 100 {
		t.Fatalf("share = %+v, %t", share, ok)
	}
	// The answers reach the bridge with their own radio latency.
	if late, ok := DebugStatsShareBetween(prev, DebugStatsSample{At: cur.At.Add(5 * time.Second), Body: cur.Body}); !ok || late.AwakePct != 25 {
		t.Errorf("share with 5 s of latency = %+v, %t", late, ok)
	}

	// A u32 accumulator that wrapped once between the readings still gives its delta.
	wrapped, ok := DebugStatsShareBetween(
		sample(0, TLV{Tag: "0223", Val: u32(0xffff0000)}, TLV{Tag: "0224", Val: u32(0x10)}),
		sample(2*time.Second, TLV{Tag: "0223", Val: u32(0xffff0000 + hz)}, TLV{Tag: "0224", Val: u32(0x10 + hz)}))
	if !ok || wrapped.AwakePct != 50 {
		t.Errorf("wrapped share = %+v, %t, want awake 50%%", wrapped, ok)
	}

	// The valve restarted between the readings: its accumulators began again from 0.
	restarted := sample(time.Hour, TLV{Tag: "0223", Val: u32(300 * hz)}, TLV{Tag: "0224", Val: u32(900 * hz)})
	// Readings whose ticks run slower than the clock: the valve did not count part of the time.
	slow := sample(100*time.Second, TLV{Tag: "0223", Val: u32(1010 * hz)}, TLV{Tag: "0224", Val: u32(3030 * hz)})
	for name, pair := range map[string][2]DebugStatsSample{
		"one wrap apart":  {prev, sample(debugStatsWrap, TLV{Tag: "0223", Val: u32(2000)})},
		"same instant":    {prev, sample(0, TLV{Tag: "0223", Val: u32(2000)})},
		"out of order":    {cur, prev},
		"zero total":      {prev, sample(time.Minute, TLV{Tag: "0223", Val: u32(1000 * hz)}, TLV{Tag: "0224", Val: u32(3000 * hz)})},
		"bad body":        {prev, {At: at.Add(time.Minute), Body: []byte{0x02}}},
		"valve restarted": {prev, restarted},
		"ticks too few":   {prev, slow},
	} {
		if s, ok := DebugStatsShareBetween(pair[0], pair[1]); ok {
			t.Errorf("%s: share = %+v, want none", name, s)
		}
	}
}

func TestEnqueueDebugStatsRefuses(t *testing.T) {
	in := fixtureInstall(t)
	e := NewEmu(in.EmuConfig(), NewUnhandledLogger(io.Discard))
	for _, fn := range []int{3, 4, 7, 8, 9, 12, 0} {
		if err := e.EnqueueDebugStats("bedroom", fn); err == nil {
			t.Errorf("function %d was queued", fn)
		}
	}
	if err := e.EnqueueDebugStats("RU1000000001", DebugStatsCounters); err == nil {
		t.Error("the wall thermostat was queued")
	}
	if e.Pending("bedroom") != 0 || e.Pending("RU1000000001") != 0 {
		t.Fatal("a refused read left a queued downlink")
	}
	bare := NewEmu(EmuConfig{Key: NetworkKey, SelfEUI: BridgeEUI}, NewUnhandledLogger(io.Discard))
	if err := bare.EnqueueDebugStats("02a6cefa07c51b00", DebugStatsCounters); err == nil {
		t.Error("a read without an installation was queued")
	}
}

// debugStatsExchange queues a read of fn to the bedroom valve, lets it
// piggyback on an uplink and returns the request.
func debugStatsExchange(t *testing.T, e *EmuState, raw []byte, fn int) *CoAPMessage {
	t.Helper()
	if err := e.EnqueueDebugStats("bedroom", fn); err != nil {
		t.Fatal(err)
	}
	get, ok := findAction(e.OnFrame(raw), ActData, "downlink post d/dbg/m?f=")
	if !ok {
		t.Fatal("the queued read did not piggyback")
	}
	return activeGetMessage(t, get.Bytes)
}

func debugStatsAnswer(t *testing.T, e *EmuState, dev []byte, req *CoAPMessage, opts []CoAPOption, body []byte, ctr uint32) {
	t.Helper()
	pkt, err := BuildCoAP(CoAPReq{
		Type: "ACK", Code: "2.05", MID: req.MID, Token: req.Token, Options: opts, Payload: body,
		SrcEUI: dev, DstEUI: BridgeEUI, SrcPort: coapPort, DstPort: bridgeCoAPPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := BuildData(NetworkKey, byte(ctr), BridgeEUI, dev[:5], WrapAppPayload(pkt, ctr, nil))
	if err != nil {
		t.Fatal(err)
	}
	e.OnFrame(frame.Bytes())
}

func TestDebugStatsReadKeepsTwoAnswers(t *testing.T) {
	bedroom := mustHex(NodeByAny("bedroom").WireLE)
	raw, _, _ := firstUplink(t, func(fr *Frame, _ *AppPayload) bool {
		return hex.EncodeToString(fr.SrcEUI()) == hex.EncodeToString(bedroom)
	})
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	cfg := fixtureInstall(t).EmuConfig()
	cfg.Now = func() time.Time { return now }
	e := NewEmu(cfg, NewUnhandledLogger(io.Discard))

	req := debugStatsExchange(t, e, raw, DebugStatsStateTime)
	if req.Code != "0.02" || req.URI() != "d/dbg/m?f=10" || len(req.Payload) != 0 {
		t.Fatalf("request = %s %q payload %x, want 0.02 d/dbg/m?f=10 without payload", req.Code, req.URI(), req.Payload)
	}
	if err := e.EnqueueDebugStats("bedroom", DebugStatsStateTime); err != nil || e.Pending("bedroom") != 1 {
		t.Fatalf("second read while pending: %v, pending %d", err, e.Pending("bedroom"))
	}
	if s, pending, err := e.DebugStats("bedroom", DebugStatsStateTime); err != nil || !pending || len(s) != 0 {
		t.Fatalf("before the answer: %+v pending=%t %v", s, pending, err)
	}

	var bodies [][]byte
	for i := range 3 {
		if i > 0 {
			now = now.Add(time.Minute)
			req = debugStatsExchange(t, e, raw, DebugStatsStateTime)
		}
		body := debugStatsBody(TLV{Tag: "0223", Val: u32(uint32(1000 * (i + 1)))})
		bodies = append(bodies, body)
		debugStatsAnswer(t, e, bedroom, req, nil, body, uint32(900+i))
	}
	s, pending, err := e.DebugStats("bedroom", DebugStatsStateTime)
	if err != nil || pending || len(s) != 2 {
		t.Fatalf("after three answers: %+v pending=%t %v", s, pending, err)
	}
	if !bytes.Equal(s[0].Body, bodies[1]) || !bytes.Equal(s[1].Body, bodies[2]) || !s[1].At.Equal(now) || !s[0].At.Equal(now.Add(-time.Minute)) {
		t.Fatalf("samples = %+v, want the last two answers, newest last", s)
	}
	if other, _, _ := e.DebugStats("bedroom", DebugStatsCounters); len(other) != 0 {
		t.Fatalf("function 11 holds the answers of function 10: %+v", other)
	}
}

// TestDebugStatsRefusesMoreBlocks: a Block2 continuation of a POST would need a
// new POST, so an answer that announces more blocks ends the exchange under its
// own code and records no statistics. A single block without more is the whole
// answer.
func TestDebugStatsRefusesMoreBlocks(t *testing.T) {
	bedroom := mustHex(NodeByAny("bedroom").WireLE)
	raw, _, _ := firstUplink(t, func(fr *Frame, _ *AppPayload) bool {
		return hex.EncodeToString(fr.SrcEUI()) == hex.EncodeToString(bedroom)
	})
	e := NewEmu(fixtureInstall(t).EmuConfig(), NewUnhandledLogger(io.Discard))
	req := debugStatsExchange(t, e, raw, DebugStatsCounters)
	body := debugStatsBody(TLV{Tag: "0230", Val: []byte{0, 7}})

	debugStatsAnswer(t, e, bedroom, req, []CoAPOption{{Num: 23, Val: []byte{0x08}}}, body, 900)
	if s, pending, _ := e.DebugStats("bedroom", DebugStatsCounters); pending || len(s) != 0 {
		t.Fatalf("answer with more blocks: %+v pending=%t", s, pending)
	}
	if code, _, ok := e.ActiveGetResponseFor("bedroom", "d/dbg/m?f=11", nil); !ok || code != DebugStatsMoreBlocks {
		t.Fatalf("answer with more blocks recorded code %q, %t", code, ok)
	}
	if e.Pending("bedroom") != 0 {
		t.Fatalf("answer with more blocks left %d downlinks", e.Pending("bedroom"))
	}

	req = debugStatsExchange(t, e, raw, DebugStatsCounters)
	debugStatsAnswer(t, e, bedroom, req, []CoAPOption{{Num: 23, Val: []byte{0x00}}}, body, 901)
	if s, pending, _ := e.DebugStats("bedroom", DebugStatsCounters); pending || len(s) != 1 || !bytes.Equal(s[0].Body, body) {
		t.Fatalf("single-block answer: %+v pending=%t", s, pending)
	}
}
