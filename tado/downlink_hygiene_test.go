package tado

import (
	"bytes"
	"encoding/hex"
	"io"
	"testing"
	"time"
)

var answerCtr uint32 = 1 << 20

// answerQueued puts the head of dev's queue on the air and answers it from dev
// with code, under the MID and token of that attempt. It returns what the
// emulator sends back.
func answerQueued(t *testing.T, e *EmuState, dev []byte, code string) []Action {
	t.Helper()
	key := hexEUI(dev)
	if _, ok, err := e.NextQueuedDownlink(key); err != nil || !ok {
		t.Fatalf("no queued downlink for %s: ok=%v err=%v", key, ok, err)
	}
	d := e.queue[key][0]
	ack, err := BuildCoAPAck(code, d.mid, d.responseToken, nil, dev, e.selfEUI, coapPort, bridgeCoAPPort)
	if err != nil {
		t.Fatal(err)
	}
	answerCtr++
	f, err := BuildData(e.key, byte(answerCtr), e.selfEUI, dev[:5], WrapAppPayload(ack, answerCtr, nil))
	if err != nil {
		t.Fatal(err)
	}
	return e.OnFrame(f.Bytes())
}

// headRequest returns the CoAP request the head of dev's queue carries.
func headRequest(t *testing.T, e *EmuState, dev []byte) *CoAPMessage {
	t.Helper()
	q := e.queue[hexEUI(dev)]
	if len(q) == 0 {
		t.Fatalf("no queued downlink for %x", dev)
	}
	m, err := parseCoAPCore(q[0].coap)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestDbg2PatchesTheDocumentOnDelivery: the served d/config takes a d/dbg2
// record under the held ETag only when the valve answers 2.04. A 4.xx answer or
// a write the retry limit drops leaves the document as it was, so the valve's
// conditional GET, which earns 2.03, never hides a value it lacks.
func TestDbg2PatchesTheDocumentOnDelivery(t *testing.T) {
	e := displayEmu(t)
	camera := mustHex(cameraEUIHex)
	before := e.cfgDoc[cameraEUIHex]
	served0140 := func() string { return tlvValue(parseTLVList(e.cfgDoc[cameraEUIHex].body), "0140") }
	was := served0140()

	if err := e.EnqueueCalibOffsetDbg2(cameraEUIHex, 10, -70); err != nil {
		t.Fatal(err)
	}
	if got := served0140(); got != was {
		t.Errorf("queued dbg2: served 0140 = %q, want %q until the valve takes it", got, was)
	}
	answerQueued(t, e, camera, "4.04")
	if n := e.Pending(cameraEUIHex); n != 0 {
		t.Fatalf("4.04 left %d writes queued", n)
	}
	if got := e.cfgDoc[cameraEUIHex]; !bytes.Equal(got.body, before.body) || !bytes.Equal(got.etag, before.etag) {
		t.Errorf("refused dbg2: document %x etag %x, want %x %x", got.body, got.etag, before.body, before.etag)
	}

	orientation := OrientationVertical
	if err := e.EnqueueDisplaySettings(cameraEUIHex, 10, DisplaySettingsWrite{Orientation: &orientation, Via: OffsetModeDbg2}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= maxDownlinkTries; i++ {
		e.NextQueuedDownlink(cameraEUIHex)
	}
	if n := e.Pending(cameraEUIHex); n != 0 {
		t.Fatalf("retry limit left %d writes queued", n)
	}
	if got, _ := e.DisplaySettings(cameraEUIHex); got.Orientation != OrientationHorizontal {
		t.Errorf("dropped dbg2: display %+v, want the horizontal the valve holds", got)
	}
	if got := e.cfgDoc[cameraEUIHex]; !bytes.Equal(got.body, before.body) {
		t.Errorf("dropped dbg2: document %x, want %x", got.body, before.body)
	}

	if err := e.EnqueueCalibOffsetDbg2(cameraEUIHex, 10, -70); err != nil {
		t.Fatal(err)
	}
	answerQueued(t, e, camera, "2.04")
	if got := served0140(); got != "ffba" {
		t.Errorf("delivered dbg2: served 0140 = %q, want ffba", got)
	}
	if !bytes.Equal(e.cfgDoc[cameraEUIHex].etag, before.etag) {
		t.Errorf("delivered dbg2: etag %x, want the held %x", e.cfgDoc[cameraEUIHex].etag, before.etag)
	}
	if !e.TakeDeliveredChanged() {
		t.Error("the delivered dbg2 record is not marked for the owner to persist")
	}
}

// TestConfigWriteKeepsQueuedDbg2: a d/config write queued behind a d/dbg2 write
// carries its record, and the dbg2 delivery does not put back a value the
// d/config write replaces.
func TestConfigWriteKeepsQueuedDbg2(t *testing.T) {
	e := displayEmu(t)
	camera := mustHex(cameraEUIHex)
	if err := e.EnqueueCalibOffsetDbg2(cameraEUIHex, 10, -70); err != nil {
		t.Fatal(err)
	}
	orientation := uint8(1)
	if err := e.EnqueueConfig(cameraEUIHex, 10, ConfigWrite{Orientation: &orientation}); err != nil {
		t.Fatal(err)
	}
	doc := parseTLVList(configWriteDocument(t, e.queue[cameraEUIHex][1]))
	if tlvValue(doc, "0140") != "ffba" || tlvValue(doc, "0149") != "01" {
		t.Fatalf("config write behind dbg2: 0140=%s 0149=%s, want ffba 01", tlvValue(doc, "0140"), tlvValue(doc, "0149"))
	}
	calib := 30
	if err := e.EnqueueConfig(cameraEUIHex, 10, ConfigWrite{CalibOffset: &calib}); err != nil {
		t.Fatal(err)
	}
	answerQueued(t, e, camera, "2.04")
	if got := tlvValue(parseTLVList(e.cfgDoc[cameraEUIHex].body), "0140"); got != "001e" {
		t.Errorf("dbg2 delivered ahead of a d/config write: served 0140 = %s, want the 001e the write carries", got)
	}
}

// TestSilentDeviceKeepsOneZoneWrite: a valve that stays off the air gets 100
// zone rewrites and keeps the newest alone. A queued write that marks an
// overlay (6440) stays ahead of it, a write on the air stays, and writes that
// must all go out are never merged.
func TestSilentDeviceKeepsOneZoneWrite(t *testing.T) {
	in := synthInstall(t)
	e := NewEmu(in.EmuConfig(), NewUnhandledLogger(io.Discard))
	e.SetHomeState(true, map[int]float64{0: 55, 4: 20.5})
	va := mustHex("3000000000c51b00")
	ru := mustHex("2000000000c51b00")
	e.node(va).raw = map[string][]byte{"6280": {0x08, 0x34}, "6240": {0x02}, "62e0": {0x00}}
	e.node(va).refreshLastKnown()

	for i := 0; i < 100; i++ {
		e.SetHomeState(i%2 == 0, map[int]float64{0: 55, 4: 20.5})
		if err := e.EnqueueZoneRewrite("VA0000000001", 4); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.Pending("VA0000000001"); n != 1 {
		t.Fatalf("100 rewrites to a silent valve left %d queued, want 1", n)
	}
	if h := e.QueueHistory("VA0000000001"); len(h) == 0 || h[0].Result != "superseded" {
		t.Errorf("history = %+v, want the superseded rewrites", h)
	}

	if err := e.EnqueueSetpoint("VA0000000001", SetpointParams{ID: 4, Setpoint: 22, Sched: 20.5, Overlay: 2}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := e.EnqueueZoneRewrite("VA0000000001", 4); err != nil {
			t.Fatal(err)
		}
	}
	q := e.queue[hexEUI(va)]
	if len(q) != 2 || q[0].kind != "setpoint" || q[1].kind != "rewrite" {
		t.Fatalf("queue = %v, want the setpoint with its 6440 and the newest rewrite", e.Queue("VA0000000001"))
	}
	if got := ZoneStateSummary(mustPayload(t, q[1].coap)); !bytes.Contains([]byte(got), []byte("6280=22.00")) {
		t.Errorf("the rewrite behind a setpoint restates %s, want its 22.00", got)
	}
	if err := e.EnqueueSetpoint("VA0000000001", SetpointParams{ID: 4, Setpoint: 23, Sched: 20.5, Overlay: 2}); err != nil {
		t.Fatal(err)
	}
	if n := e.Pending("VA0000000001"); n != 1 {
		t.Fatalf("a new setpoint left %d queued, want it alone", n)
	}

	e.NextQueuedDownlink("VA0000000001")
	for i := 0; i < 3; i++ {
		if err := e.EnqueueZoneRewrite("VA0000000001", 4); err != nil {
			t.Fatal(err)
		}
	}
	if q := e.queue[hexEUI(va)]; len(q) != 2 || !q[0].inFlight || q[0].kind != "setpoint" {
		t.Fatalf("queue = %v, want the setpoint on the air and one rewrite", e.Queue("VA0000000001"))
	}

	for i := 0; i < 3; i++ {
		if err := e.EnqueueChildLock("VA0000000001", 4, i%2 == 0); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.Pending("VA0000000001"); n != 5 {
		t.Errorf("child locks: %d queued, want each of the three behind the two zone writes", n)
	}

	for i := 0; i < 50; i++ {
		if err := e.EnqueueZoneRewrite("RU0000000001", 0); err != nil {
			t.Fatal(err)
		}
		if err := e.EnqueueDHWOverlay("RU0000000001", 0, 50, true, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	if q := e.queue[hexEUI(ru)]; len(q) != 1 || q[0].kind != "dhw" {
		t.Fatalf("hot water queue = %v, want the newest overlay alone", e.Queue("RU0000000001"))
	}
}

// TestZoneWriteDeliveredFollowsMarks: a write superseded while it waits is
// confirmed by the delivery of the newer one. A write still queued, a 4.xx
// answer and a write to another zone confirm nothing.
func TestZoneWriteDeliveredFollowsMarks(t *testing.T) {
	in := synthInstall(t)
	e := NewEmu(in.EmuConfig(), NewUnhandledLogger(io.Discard))
	e.SetHomeState(true, map[int]float64{0: 55, 4: 20.5})
	va := mustHex("3000000000c51b00")
	const target = "VA0000000001"
	if err := e.EnqueueZoneRewrite(target, 4); err != nil {
		t.Fatal(err)
	}
	first, ok := e.ZoneWriteMark(target, 4)
	if !ok || e.ZoneWriteDelivered(target, 4, first) {
		t.Fatalf("mark=%d ok=%t, delivered before any answer", first, ok)
	}
	if err := e.EnqueueZoneRewrite(target, 4); err != nil {
		t.Fatal(err)
	}
	second, _ := e.ZoneWriteMark(target, 4)
	if second <= first || e.Pending(target) != 1 {
		t.Fatalf("second mark %d after %d with %d queued, want a newer mark on one write", second, first, e.Pending(target))
	}
	answerQueued(t, e, va, "4.00")
	if e.ZoneWriteDelivered(target, 4, first) {
		t.Fatal("a 4.00 answer confirmed the write")
	}
	if err := e.EnqueueZoneRewrite(target, 4); err != nil {
		t.Fatal(err)
	}
	answerQueued(t, e, va, "2.04")
	if !e.ZoneWriteDelivered(target, 4, first) || !e.ZoneWriteDelivered(target, 4, second) {
		t.Error("the delivered write does not confirm the ones queued before it")
	}
	if e.ZoneWriteDelivered(target, 5, first) {
		t.Error("a write to zone 4 confirmed zone 5")
	}
}

func mustPayload(t *testing.T, coap []byte) []byte {
	t.Helper()
	m, err := parseCoAPCore(coap)
	if err != nil {
		t.Fatal(err)
	}
	return m.Payload
}

// TestZoneReadsRestateTheHomeState: a zone read served from the cache, on the
// zone plane or after a proxied read times out, restates the home state as the
// direct read does. A delivered write stores the head it carried, 6200 after
// 61e0.
func TestZoneReadsRestateTheHomeState(t *testing.T) {
	in := synthInstall(t)
	va, err := in.Resolve("VA0000000001")
	if err != nil {
		t.Fatal(err)
	}
	e := NewEmu(in.EmuConfig(), NewUnhandledLogger(io.Discard))
	e.SetHomeState(true, map[int]float64{0: 55, 4: 20.5})
	// The cache as a write made away left it.
	e.zoneState[zoneKey(4, "z/s")] = encodeTLVs([]TLV{
		{Tag: "6160", Val: []byte{0x02}}, {Tag: "6180", Val: []byte{0x00}}, {Tag: "6020", Val: []byte{0x01}},
		{Tag: "61e0", Val: []byte{0x00}}, {Tag: "6240", Val: []byte{0x02}}, {Tag: "6260", Val: []byte{0x01}},
		{Tag: "6280", Val: []byte{0x07, 0xd0}}, {Tag: "62e0", Val: []byte{0x00}},
	})
	want := "61600101" + "61800100" + "60200101" + "61e00101" + "6200020802" +
		"62400102" + "62600101" + "62800207d0" + "62e00100"

	get := synthZoneUplink(t, in, va.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7501, Token: []byte{0x31},
		Path: []string{"z", "s"}, Query: []string{"lid=4"},
	})
	if m := synthAnswer(t, in, e.OnFrame(get), 0x7501); hex.EncodeToString(m.Payload) != want {
		t.Errorf("zone-plane read = %x, want %s", m.Payload, want)
	}
	a, ok := e.answerExpiredProxy(&zoneProxyWait{
		requester: va.WireEUI(), zone: 4, resource: "z/s", uri: "z/s?lid=4",
		mid: 0x7502, token: []byte{0x32}, srcPort: coapPort, dstPort: coapPort,
	})
	if !ok {
		t.Fatal("no answer to the expired proxy")
	}
	if m := synthAnswer(t, in, []Action{a}, 0x7502); hex.EncodeToString(m.Payload) != want {
		t.Errorf("expired proxy answer = %x, want %s", m.Payload, want)
	}

	e.zoneState = map[string][]byte{}
	if err := e.EnqueueSetpoint("VA0000000001", SetpointParams{ID: 4, Setpoint: 21, Sched: 20.5, Overlay: 2}); err != nil {
		t.Fatal(err)
	}
	answerQueued(t, e, va.WireEUI(), "2.04")
	wantHeating := "61600101" + "61800100" + "60200101" + "61e00101" + "6200020802" +
		"62400102" + "62600101" + "6280020834" + "62e00100"
	if got := hex.EncodeToString(e.zoneState[zoneKey(4, "z/s")]); got != wantHeating {
		t.Errorf("cached heating block = %s, want %s", got, wantHeating)
	}

	ru, err := in.Resolve("RU0000000001")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueDHWOverlay("RU0000000001", 0, 50, true, 0, ""); err != nil {
		t.Fatal(err)
	}
	answerQueued(t, e, ru.WireEUI(), "2.04")
	wantDHW := "61600101" + "61800100" + "60200102" + "61e00101" + "620002157c" +
		"62400102" + "62600101" + "6280021388" + "62e00100"
	if got := hex.EncodeToString(e.zoneState[zoneKey(0, "z/s")]); got != wantDHW {
		t.Errorf("cached hot-water block = %s, want %s", got, wantDHW)
	}
}

// TestChildLockRecordedOnDelivery: the served d/lock state follows what the
// valve took, not what is queued to it.
func TestChildLockRecordedOnDelivery(t *testing.T) {
	e, _ := fixtureEmu(t)
	camera := mustHex(cameraEUIHex)
	if err := e.EnqueueChildLock(cameraEUIHex, 10, true); err != nil {
		t.Fatal(err)
	}
	if e.childLock[cameraEUIHex] {
		t.Fatal("the child lock is recorded before the valve takes it")
	}
	actions, ok, err := e.NextQueuedDownlink(cameraEUIHex)
	if err != nil || !ok {
		t.Fatalf("no queued downlink: ok=%v err=%v", ok, err)
	}
	df, err := ParseData(e.key, actions[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	e.OnFrame(BuildEnhAck(df.Seq, e.selfEUI, camera, 0))
	if !e.childLock[cameraEUIHex] || !e.TakeDeliveredChanged() {
		t.Error("the delivered child lock is not recorded for the owner to persist")
	}
}

// TestConfigWritesCoalesce: the UTC offset push while an operator write is
// still queued leaves one d/config write, the whole newest document: each one
// homes the valve's motor.
func TestConfigWritesCoalesce(t *testing.T) {
	e := displayEmu(t)
	calib := 20
	if err := e.EnqueueConfig(cameraEUIHex, 10, ConfigWrite{CalibOffset: &calib}); err != nil {
		t.Fatal(err)
	}
	v := 120
	e.cfg.UTCOffsetMin = &v
	now := time.Now()
	if !e.SyncUTCOffset(now) {
		t.Fatal("SyncUTCOffset missed the changed offset")
	}
	if got := e.PushUTCOffset(now); got != cameraEUIHex {
		t.Fatalf("PushUTCOffset = %q, want the camera valve", got)
	}
	q := e.queue[cameraEUIHex]
	if len(q) != 1 {
		t.Fatalf("%d d/config writes queued, want the newest document alone", len(q))
	}
	doc := parseTLVList(configWriteDocument(t, q[0]))
	if tlvValue(doc, "0140") != "0014" || tlvValue(doc, "02b2") != "0078" {
		t.Errorf("coalesced write: 0140=%s 02b2=%s, want 0014 0078", tlvValue(doc, "0140"), tlvValue(doc, "02b2"))
	}
	if !bytes.Equal(configWriteDocument(t, q[0]), e.cfgDoc[cameraEUIHex].body) {
		t.Error("the coalesced write is not the served document")
	}
}

// TestServerErrorRetryMintsNewIDs: a 5.xx closes the CoAP exchange, so the
// retry of a write goes out under a new MID and token. The old ones no longer
// close it; the new ones do.
func TestServerErrorRetryMintsNewIDs(t *testing.T) {
	e, now := clockedFixtureEmu(t)
	camera := mustHex(cameraEUIHex)
	if err := e.EnqueueSetpoint(cameraEUIHex, SetpointParams{ID: 10, Setpoint: 21, Sched: 20, Overlay: 2}); err != nil {
		t.Fatal(err)
	}
	first := headRequest(t, e, camera)
	answerQueued(t, e, camera, "5.03")
	if n := e.Pending(cameraEUIHex); n != 1 {
		t.Fatalf("5.03 left %d writes queued, want the write for a retry", n)
	}
	*now = now.Add(serverErrorRetryBackoff)
	second := headRequest(t, e, camera)
	if second.MID == first.MID || bytes.Equal(second.Token, first.Token) {
		t.Fatalf("retry MID/token = %04x/%x, want new ones after %04x/%x", second.MID, second.Token, first.MID, first.Token)
	}
	if !bytes.Equal(second.Payload, first.Payload) || second.URI() != first.URI() {
		t.Errorf("retry = %s %x, want the same request %s %x", second.URI(), second.Payload, first.URI(), first.Payload)
	}
	if ck, ok := second.RecomputeChecksum(e.selfEUI, camera); !ok || ck != second.Checksum {
		t.Errorf("retry checksum = %04x, want %04x", second.Checksum, ck)
	}
	e.confirmByMID(camera, &CoAPMessage{Type: "ACK", Code: "2.04", MID: first.MID, Token: first.Token})
	if n := e.Pending(cameraEUIHex); n != 1 {
		t.Fatal("the old MID closed the re-minted write")
	}
	answerQueued(t, e, camera, "2.04")
	if n := e.Pending(cameraEUIHex); n != 0 {
		t.Fatalf("2.04 under the new MID left %d queued", n)
	}
}

// clockedFixtureEmu is fixtureEmu on a clock the test moves.
func clockedFixtureEmu(t *testing.T) (*EmuState, *time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	cfg := fixtureInstall(t).EmuConfig()
	cfg.Now = func() time.Time { return now }
	return NewEmu(cfg, NewUnhandledLogger(io.Discard)), &now
}

// TestServerErrorWaitsForTheRetry: a 5.xx answer costs its own attempt only.
// The retry does not go out in the rendezvous that carried the answer, where
// the busy device would answer 5.xx again, but after the backoff.
func TestServerErrorWaitsForTheRetry(t *testing.T) {
	e, now := clockedFixtureEmu(t)
	camera := mustHex(cameraEUIHex)
	if err := e.EnqueueSetpoint(cameraEUIHex, SetpointParams{ID: 10, Setpoint: 21, Sched: 20, Overlay: 2}); err != nil {
		t.Fatal(err)
	}
	for _, a := range answerQueued(t, e, camera, "5.03") {
		if a.QueueAttempt {
			t.Errorf("the 5.03 answer sent %q in the same rendezvous", a.Reason)
		}
	}
	if q := e.Queue(cameraEUIHex); len(q) != 1 || q[0].Tries != 1 {
		t.Fatalf("queue after 5.03 = %+v, want the write after one try", q)
	}
	if _, ok, _ := e.NextQueuedDownlink(cameraEUIHex); ok {
		t.Fatal("the retry went out before the backoff")
	}
	*now = now.Add(serverErrorRetryBackoff)
	if _, ok, _ := e.NextQueuedDownlink(cameraEUIHex); !ok {
		t.Fatal("no retry after the backoff")
	}
	if q := e.Queue(cameraEUIHex); len(q) != 1 || q[0].Tries != 2 {
		t.Errorf("queue after the retry = %+v, want the second try", q)
	}
}

// TestConfigWriteReportsCoalescedWrite: a d/config write that replaces one
// queued ahead of it is still a new write, for the operator's clear of 02b3
// and for a reload.
func TestConfigWriteReportsCoalescedWrite(t *testing.T) {
	e, _ := fixtureEmu(t)
	flipped := !e.inst.Options.OfflineScheduleEnabled()
	if err := e.EnqueueConfig(cameraEUIHex, 10, ConfigWrite{OfflineScheduleEnabled: &flipped}); err != nil {
		t.Fatal(err)
	}
	written, err := e.ClearOfflineSchedule(cameraEUIHex, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n := e.Pending(cameraEUIHex); !written || n != 1 {
		t.Errorf("clear behind a queued write: written=%t with %d queued, want true and the one coalesced write", written, n)
	}

	orientation := uint8(1)
	if err := e.EnqueueConfig(cameraEUIHex, 10, ConfigWrite{Orientation: &orientation}); err != nil {
		t.Fatal(err)
	}
	e.node(mustHex(cameraEUIHex)).LastID = 10
	e.inst.HomeID++
	if written := e.refreshConfigDocuments(); !written[cameraEUIHex] {
		t.Errorf("reload behind a queued write: written=%v, want %s", written, cameraEUIHex)
	}
}
