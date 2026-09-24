package daemon

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"freedo/internal/radio"
	"freedo/tado"
)

// fwDevice is a fake valve that answers d/fw per the firmware's block handler:
// the next block 2.31 with Block1 echoed, the last one 2.04, a repeat of the
// previous block re-acked without a write, any other gap 4.08, a size past the
// slot 4.13. On 2.04 it reboots: it PUTs its fw/state, unconfirmed until the
// coordinator answers that PUT with a 2.xx.
type fwDevice struct {
	frames chan []byte
	wire   []byte
	coord  []byte
	key    []byte

	mu        sync.Mutex
	reasm     *tado.Reassembler
	ctr       uint32
	running   tado.FwVersion
	confirmed bool
	putMID    uint16
	slotMax   int
	next      int // next block expected
	prev      int // last block taken
	lastCode  string
	size      int
	image     []byte
	pending   tado.FwVersion
	crc       uint16
	writes    int
	aborted   int
	requests  []string
	// dropAnswer swallows that many answers of a block: the block is taken,
	// its answer never arrives.
	dropAnswer map[int]int
	// code forces the answer to a block.
	code map[int]string
	// gapAfter makes the device expect a block further than the one it took.
	gapAfter int
}

func newFwDevice(t *testing.T, running tado.FwVersion, confirmed bool) *fwDevice {
	t.Helper()
	dev, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	return &fwDevice{
		frames: make(chan []byte, 16), wire: dev.WireEUI(), coord: defaultInstall.CoordinatorEUI(),
		key: defaultInstall.NetworkKey(), reasm: tado.NewReassembler(), running: running, confirmed: confirmed,
		slotMax: tado.OTASlotSize, gapAfter: -1, prev: -1, dropAnswer: map[int]int{}, code: map[int]string{},
	}
}

func (f *fwDevice) Frames() <-chan []byte { return f.frames }
func (f *fwDevice) Send([]byte) error     { return nil }
func (f *fwDevice) Close() error          { return nil }

func (f *fwDevice) SendImmediate(actions []tado.Action, _ bool) (radio.ImmediateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := radio.ImmediateResult{Submitted: append([]tado.Action(nil), actions...)}
	for _, a := range actions {
		frame, err := tado.ParseData(f.key, a.Bytes)
		if err != nil || !bytes.Equal(frame.DstEUI, f.wire) {
			continue
		}
		var m *tado.CoAPMessage
		if tado.IsFragment(frame.Plaintext) {
			if msg, done := f.reasm.Add(frame); done {
				m = msg
			}
		} else if app := tado.ParseApp(frame.Plaintext); app != nil {
			m = app.CoAP
		}
		if m == nil {
			continue
		}
		if ack := f.handle(m); ack != nil {
			result.Acks = append(result.Acks, ack)
		}
	}
	return result, nil
}

func (f *fwDevice) handle(m *tado.CoAPMessage) []byte {
	if m.Type == "ACK" {
		if m.MID == f.putMID && strings.HasPrefix(m.Code, "2.") {
			f.confirmed = true
		}
		return nil
	}
	switch {
	case m.Code == "0.02" && m.Path == "d/fw":
		return f.block(m)
	case m.Code == "0.04" && m.Path == "d/fw":
		f.requests = append(f.requests, "abort")
		f.aborted++
		f.next, f.prev, f.lastCode, f.image = 0, -1, "", nil
		return f.answer(m, "2.02", nil)
	case m.Code == "0.01" && m.Path == "d/fw/state":
		f.requests = append(f.requests, "get fw/state")
		return f.answer(m, "2.05", f.state())
	}
	f.requests = append(f.requests, m.Path)
	return f.answer(m, "2.04", nil)
}

func (f *fwDevice) block(m *tado.CoAPMessage) []byte {
	o, _ := m.Option(27)
	var v uint32
	for _, b := range o.Val {
		v = v<<8 | uint32(b)
	}
	num, more := int(v>>4), v&8 != 0
	f.requests = append(f.requests, "fw block")
	echo := tado.CoAPOption{Num: 27, Val: o.Val}
	code := ""
	switch {
	case f.code[num] != "":
		code = f.code[num]
		delete(f.code, num)
	case num == f.prev && f.lastCode != "":
		code = f.lastCode
	case num != f.next:
		code = "4.08"
	case num == 0 && int(binary.BigEndian.Uint32(m.Payload[2:6])) > f.slotMax:
		code = "4.13"
	default:
		data := m.Payload
		if num == 0 {
			f.size = int(binary.BigEndian.Uint32(data[2:6]))
			f.crc = binary.BigEndian.Uint16(data[13:15])
			f.pending = tado.FwVersion(binary.BigEndian.Uint16(data[18:20]))
			f.image = nil
			data = data[tado.OTAHeaderLen:]
		}
		f.image = append(f.image, data...)
		f.writes++
		f.prev, f.next = num, num+1
		if num == f.gapAfter {
			f.next = num + 2
		}
		code = "2.31"
		if !more {
			code = "5.00"
			if len(f.image) == f.size && tado.OTASlotCRC(f.image) == f.crc {
				code = "2.04"
			}
		}
		f.lastCode = code
	}
	if code == "2.04" && f.running != f.pending {
		f.reboot()
	}
	if f.dropAnswer[num] > 0 {
		f.dropAnswer[num]--
		return nil
	}
	return f.answer(m, code, nil, echo)
}

// reboot runs the new image and PUTs its fw/state, as the device does at boot.
func (f *fwDevice) reboot() {
	f.running, f.confirmed = f.pending, false
	f.putMID = 0x7e01
	f.ctr++
	pkt, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: f.putMID, Token: []byte{7},
		Path: []string{"d", "VA1000000005", "fw", "state"}, Payload: f.state(),
		SrcEUI: f.wire, DstEUI: f.coord, SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		panic(err)
	}
	frame, err := tado.BuildData(f.key, byte(f.ctr), f.coord, f.wire[:5], tado.WrapAppPayload(pkt, 500000+f.ctr, nil))
	if err != nil {
		panic(err)
	}
	f.frames <- frame.Bytes()
}

func (f *fwDevice) state() []byte {
	flags := tado.FwSlotHasImage | tado.FwSlotActive
	if f.confirmed {
		flags |= tado.FwSlotConfirmed
	}
	b := tado.TLV{Tag: "01a0", Val: []byte{tado.OTADeviceType}}.Encode()
	b = append(b, tado.TLV{Tag: "003a", Val: []byte{byte(f.running >> 8), byte(f.running)}}.Encode()...)
	return append(b, tado.TLV{Tag: "003b", Val: []byte{byte(flags)}}.Encode()...)
}

func (f *fwDevice) answer(m *tado.CoAPMessage, code string, body []byte, opts ...tado.CoAPOption) []byte {
	ack, err := tado.BuildCoAPAck(code, m.MID, m.Token, body, f.wire, f.coord, 5683, 4005, opts...)
	if err != nil {
		panic(err)
	}
	f.ctr++
	frame, err := tado.BuildData(f.key, byte(f.ctr), f.coord, f.wire[:5], tado.WrapAppPayload(ack, 500000+f.ctr, nil))
	if err != nil {
		panic(err)
	}
	return frame.Bytes()
}

func (f *fwDevice) snapshot() (writes, aborted int, image []byte, requests []string, confirmed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes, f.aborted, append([]byte(nil), f.image...), append([]string(nil), f.requests...), f.confirmed
}

var testFwTiming = fwTiming{
	block: 300 * time.Millisecond, retries: 2, abort: time.Second,
	reboot: 3 * time.Second, read: 3 * time.Second, reread: 200 * time.Millisecond,
	poll: 2 * time.Millisecond,
}

func slowFwTiming() fwTiming {
	timing := testFwTiming
	timing.block = 5 * time.Second
	return timing
}

func fwImage(n int) []byte {
	img := make([]byte, n)
	for i := range img {
		img[i] = byte(i*13 + 5)
	}
	binary.LittleEndian.PutUint32(img[0:], 0x2000fff0)
	binary.LittleEndian.PutUint32(img[4:], 0x0000b001)
	return img
}

func mustFwVersion(t *testing.T, s string) tado.FwVersion {
	t.Helper()
	v, err := tado.ParseFwVersion(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// startFwDaemon starts a daemon on dev with the given transfer timing.
func startFwDaemon(t *testing.T, dev *fwDevice, now func() time.Time, timing fwTiming, dryRun bool) string {
	t.Helper()
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000, NoAutoQueue: true, Now: now}, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, dev, dryRun, "live")
	d.fwTiming = timing
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	return srv.URL
}

// newFwDaemon starts a daemon on dev and reads the device's fw/state once, as
// an operator does before a transfer.
func newFwDaemon(t *testing.T, dev *fwDevice, now func() time.Time, timing fwTiming) string {
	t.Helper()
	url := startFwDaemon(t, dev, now, timing, false)
	var out map[string]any
	if code := getJSON(t, url+"/api/nodes/VA1000000005/fw", &out); code != http.StatusOK {
		t.Fatalf("fw read: %d %v", code, out)
	}
	return url
}

func fwPost(t *testing.T, url string, body map[string]any) (int, map[string]any) {
	t.Helper()
	full := map[string]any{"confirm": true, "version": "215.2", "imageKind": "valve", "image": fwImage(1000)}
	for k, v := range body {
		if v == nil {
			delete(full, k)
			continue
		}
		full[k] = v
	}
	var out map[string]any
	code := postJSON(t, http.MethodPost, url+"/api/nodes/VA1000000005/fw", full, &out)
	return code, out
}

type fwView struct {
	Transfer *fwStatus `json:"transfer"`
}

func waitFw(t *testing.T, url string, done func(*fwStatus) bool) *fwStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var v fwView
		getJSON(t, url+"/api/nodes/VA1000000005/fw", &v)
		if v.Transfer != nil && done(v.Transfer) {
			return v.Transfer
		}
		time.Sleep(5 * time.Millisecond)
	}
	var v fwView
	getJSON(t, url+"/api/nodes/VA1000000005/fw", &v)
	t.Fatalf("transfer did not reach the state: %+v", v.Transfer)
	return nil
}

func finished(s *fwStatus) bool { return s.Finished != nil }

func startFwTransfer(t *testing.T, url string) {
	t.Helper()
	if code, out := fwPost(t, url, nil); code != http.StatusAccepted {
		t.Fatalf("start: %d %v", code, out)
	}
}

func TestFwTransferSucceedsAndConfirmsAfterReboot(t *testing.T) {
	dev := newFwDevice(t, mustFwVersion(t, "215.1"), true)
	url := newFwDaemon(t, dev, nil, testFwTiming)
	startFwTransfer(t, url)
	st := waitFw(t, url, finished)
	if st.State != fwDone || st.Error != "" || st.Acked != 8 || st.Blocks != 8 || st.Resends != 0 {
		t.Fatalf("status %+v", st)
	}
	if st.Reported == nil || st.Reported.Running.Version != "215.2" || st.Reported.Confirmed {
		t.Fatalf("reported %+v, want the unconfirmed 215.2 of the reboot PUT", st.Reported)
	}
	if st.Result == nil || st.Result.Running.Version != "215.2" || !st.Result.Confirmed {
		t.Fatalf("result %+v, want 215.2 confirmed", st.Result)
	}
	writes, aborted, image, _, confirmed := dev.snapshot()
	if writes != 8 || aborted != 0 || !bytes.Equal(image, fwImage(1000)) || !confirmed {
		t.Fatalf("device writes %d aborted %d image ok %t confirmed %t", writes, aborted, bytes.Equal(image, fwImage(1000)), confirmed)
	}
}

func TestFwTransferResendsABlockWhoseAnswerIsLost(t *testing.T) {
	dev := newFwDevice(t, mustFwVersion(t, "215.1"), true)
	dev.dropAnswer[2] = 1
	url := newFwDaemon(t, dev, nil, testFwTiming)
	startFwTransfer(t, url)
	st := waitFw(t, url, finished)
	if st.State != fwDone || st.Resends != 1 {
		t.Fatalf("status %+v", st)
	}
	if writes, _, image, _, _ := dev.snapshot(); writes != 8 || !bytes.Equal(image, fwImage(1000)) {
		t.Fatalf("device writes %d, want each block once", writes)
	}
}

func TestFwTransferLastBlockAnswerLostVerifiesByFwState(t *testing.T) {
	dev := newFwDevice(t, mustFwVersion(t, "215.1"), true)
	dev.dropAnswer[7] = 100
	url := newFwDaemon(t, dev, nil, testFwTiming)
	startFwTransfer(t, url)
	st := waitFw(t, url, finished)
	if st.State != fwDone || st.Note == "" || st.Result == nil || !st.Result.Confirmed {
		t.Fatalf("status %+v", st)
	}
	if _, aborted, _, _, _ := dev.snapshot(); aborted != 0 {
		t.Fatalf("a lost last answer sent DELETE d/fw %d times", aborted)
	}
}

func TestFwTransferAbortsOnDeviceErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(*fwDevice)
		want  string
	}{
		"gap 4.08":  {func(f *fwDevice) { f.gapAfter = 1 }, "block 2 answered 4.08"},
		"size 4.13": {func(f *fwDevice) { f.slotMax = 500 }, "block 0 answered 4.13"},
		"fail 5.00": {func(f *fwDevice) { f.code[3] = "5.00" }, "block 3 answered 5.00"},
		"no answer": {func(f *fwDevice) { f.dropAnswer[4] = 100 }, "block 4: no response after 3 tries"},
	} {
		t.Run(name, func(t *testing.T) {
			dev := newFwDevice(t, mustFwVersion(t, "215.1"), true)
			tc.setup(dev)
			url := newFwDaemon(t, dev, nil, testFwTiming)
			startFwTransfer(t, url)
			st := waitFw(t, url, finished)
			if st.State != fwFailed || !strings.Contains(st.Error, tc.want) || st.LastCode != "2.02" {
				t.Fatalf("status %+v, want failed with %q and the 2.02 of DELETE d/fw", st, tc.want)
			}
			if _, aborted, _, _, _ := dev.snapshot(); aborted != 1 {
				t.Fatalf("DELETE d/fw sent %d times", aborted)
			}
		})
	}
}

func TestFwTransferCancelSendsDelete(t *testing.T) {
	dev := newFwDevice(t, mustFwVersion(t, "215.1"), true)
	dev.dropAnswer[3] = 100
	url := newFwDaemon(t, dev, nil, slowFwTiming())
	startFwTransfer(t, url)
	waitFw(t, url, func(s *fwStatus) bool { return s.Acked == 3 })
	var out map[string]any
	if code := postJSON(t, http.MethodDelete, url+"/api/nodes/VA1000000005/fw", nil, &out); code != http.StatusAccepted {
		t.Fatalf("cancel: %d %v", code, out)
	}
	st := waitFw(t, url, finished)
	if st.State != fwCancelled {
		t.Fatalf("status %+v", st)
	}
	if _, aborted, _, _, _ := dev.snapshot(); aborted != 1 {
		t.Fatalf("DELETE d/fw sent %d times", aborted)
	}
	if code := postJSON(t, http.MethodDelete, url+"/api/nodes/VA1000000005/fw", nil, &out); code != http.StatusNotFound {
		t.Fatalf("cancel with nothing running: %d %v", code, out)
	}
}

func TestFwTransferHoldsOtherDownlinks(t *testing.T) {
	dev := newFwDevice(t, mustFwVersion(t, "215.1"), true)
	dev.dropAnswer[2] = 1
	url := newFwDaemon(t, dev, nil, testFwTiming)
	startFwTransfer(t, url)
	waitFw(t, url, func(s *fwStatus) bool { return s.Acked >= 1 })
	var out map[string]any
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/VA1000000005/identify", map[string]any{"id": 10}, &out); code != http.StatusOK {
		t.Fatalf("identify: %d %v", code, out)
	}
	waitFw(t, url, finished)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, _, _, requests, _ := dev.snapshot()
		last := strings.LastIndex(strings.Join(requests, ","), "fw block")
		at := strings.Index(strings.Join(requests, ","), "d/identify")
		if at >= 0 {
			if at < last {
				t.Fatalf("identify interleaved with the blocks: %v", requests)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("identify never went out after the transfer: %v", requests)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFwTransferRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		running   string
		confirmed bool
		body      map[string]any
		status    int
		want      string
	}{
		"no confirm":    {"215.1", true, map[string]any{"confirm": false}, http.StatusBadRequest, `{"confirm":true}`},
		"bad version":   {"215.1", true, map[string]any{"version": "215"}, http.StatusBadRequest, "major.minor"},
		"downgrade":     {"215.3", true, nil, http.StatusBadRequest, "below running"},
		"unconfirmed":   {"215.1", false, nil, http.StatusBadRequest, "not confirmed"},
		"kind mismatch": {"215.1", true, map[string]any{"imageKind": "thermostat"}, http.StatusBadRequest, "image is for a thermostat"},
		"bad image":     {"215.1", true, map[string]any{"image": fwImage(1000)[:6]}, http.StatusBadRequest, "too short"},
		"no image":      {"215.1", true, map[string]any{"image": nil}, http.StatusBadRequest, "ota image is 0 bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			dev := newFwDevice(t, mustFwVersion(t, tc.running), tc.confirmed)
			url := newFwDaemon(t, dev, nil, testFwTiming)
			code, out := fwPost(t, url, tc.body)
			if code != tc.status || !strings.Contains(out["error"].(string), tc.want) {
				t.Fatalf("got %d %v, want %d %q", code, out, tc.status, tc.want)
			}
			if _, _, _, requests, _ := dev.snapshot(); strings.Contains(strings.Join(requests, ","), "fw block") {
				t.Fatalf("a refused transfer sent a block: %v", requests)
			}
		})
	}

	t.Run("stale fw/state", func(t *testing.T) {
		clock := newTestClock(time.Now())
		dev := newFwDevice(t, mustFwVersion(t, "215.1"), true)
		url := newFwDaemon(t, dev, clock.now, testFwTiming)
		clock.advance(fwStateMaxAge + time.Minute)
		if code, out := fwPost(t, url, nil); code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "old") {
			t.Fatalf("got %d %v", code, out)
		}
	})
	t.Run("no fw/state", func(t *testing.T) {
		dev := newFwDevice(t, mustFwVersion(t, "215.1"), true)
		url := startFwDaemon(t, dev, nil, testFwTiming, false)
		if code, out := fwPost(t, url, nil); code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "no fw/state") {
			t.Fatalf("got %d %v", code, out)
		}
	})
	t.Run("dry-run", func(t *testing.T) {
		dev := newFwDevice(t, mustFwVersion(t, "215.1"), true)
		url := startFwDaemon(t, dev, nil, testFwTiming, true)
		if code, out := fwPost(t, url, nil); code != http.StatusConflict {
			t.Fatalf("got %d %v", code, out)
		}
	})
	t.Run("busy", func(t *testing.T) {
		dev := newFwDevice(t, mustFwVersion(t, "215.1"), true)
		dev.dropAnswer[1] = 100
		url := newFwDaemon(t, dev, nil, slowFwTiming())
		startFwTransfer(t, url)
		if code, out := fwPost(t, url, nil); code != http.StatusConflict || !strings.Contains(out["error"].(string), "already running") {
			t.Fatalf("got %d %v", code, out)
		}
		var out map[string]any
		postJSON(t, http.MethodDelete, url+"/api/nodes/VA1000000005/fw", nil, &out)
		waitFw(t, url, finished)
	})
	t.Run("oversized body", func(t *testing.T) {
		dev := newFwDevice(t, mustFwVersion(t, "215.1"), true)
		url := newFwDaemon(t, dev, nil, testFwTiming)
		resp, err := http.Post(url+"/api/nodes/VA1000000005/fw", "application/json",
			io.MultiReader(strings.NewReader(`{"image":"`), strings.NewReader(strings.Repeat("A", fwUploadMaxBody)), strings.NewReader(`"}`)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("got %d", resp.StatusCode)
		}
	})
}
