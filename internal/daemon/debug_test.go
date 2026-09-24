package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"freedo/internal/radio"
	"freedo/tado"
)

// debugFrameView mirrors one entry of the dissect answer.
type debugFrameView struct {
	Index     int    `json:"index"`
	Len       int    `json:"len"`
	Class     string `json:"class"`
	Dissect   string `json:"dissect"`
	Decrypted bool   `json:"decrypted"`
	Seq       int    `json:"seq"`
	Src       string `json:"src"`
	Dst       string `json:"dst"`
	Counter   uint32 `json:"counter"`
	Plaintext string `json:"plaintext"`
	Error     string `json:"error"`
	CoAP      *struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		MID     uint16 `json:"mid"`
		Token   string `json:"token"`
		URI     string `json:"uri"`
		Summary string `json:"summary"`
	} `json:"coap"`
	TLVs []struct {
		Tag   string `json:"tag"`
		Label string `json:"label"`
		Hex   string `json:"hex"`
	} `json:"tlvs"`
}

type debugDissectResp struct {
	Classes map[string]int   `json:"classes"`
	Frames  []debugFrameView `json:"frames"`
	Error   string           `json:"error"`
}

// newDebugServer serves the API of a daemon with no radio traffic: the debug
// endpoints never reach the engine, so the engine does not even have to run.
func newDebugServer(t *testing.T) string {
	t.Helper()
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	d.debugAPI = true
	srv := httptest.NewServer(d.mux())
	t.Cleanup(srv.Close)
	return srv.URL
}

// debugDataFrame builds one encrypted uplink carrying a CoAP request.
func debugDataFrame(t *testing.T, seq byte, counter uint32, payload []byte) []byte {
	t.Helper()
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	packet, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x4242, Token: []byte{0x7a},
		Path: []string{"d", "temp"}, Query: []string{"id=10"},
		Payload: payload,
		SrcEUI:  device.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(),
		SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.BuildData(defaultInstall.NetworkKey(), seq,
		defaultInstall.CoordinatorEUI(), device.WireEUI()[:5], tado.WrapAppPayload(packet, counter, nil))
	if err != nil {
		t.Fatal(err)
	}
	return frame.Bytes()
}

// debugFragmentFrames builds the two Data frames of one fragmented datagram.
func debugFragmentFrames(t *testing.T) [][]byte {
	t.Helper()
	device, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	packet, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x5150, Token: []byte{0x51},
		Path: []string{"d", "config"}, Query: []string{"id=10"},
		Payload: bytes.Repeat([]byte{0x51}, 110),
		SrcEUI:  device.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(),
		SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	bodies, err := tado.Fragment6LoWPANDatagram(packet, 0x5150)
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("the fixture datagram made %d fragments, want 2", len(bodies))
	}
	frames := make([][]byte, 0, len(bodies))
	for i, body := range bodies {
		frame, err := tado.BuildData(defaultInstall.NetworkKey(), byte(i),
			defaultInstall.CoordinatorEUI(), device.WireEUI()[:5], tado.WrapAppPayload(body, uint32(900+i), nil))
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame.Bytes())
	}
	return frames
}

func TestDebugDissectSingleFrame(t *testing.T) {
	url := newDebugServer(t)
	frame := debugDataFrame(t, 0x11, 4242, []byte{0x62, 0x80, 0x02, 0x07, 0xd0})

	var got debugDissectResp
	code := postJSON(t, http.MethodPost, url+"/api/debug/dissect",
		map[string]any{"hex": hex.EncodeToString(frame)}, &got)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%+v", code, got)
	}
	if len(got.Frames) != 1 {
		t.Fatalf("frames=%d", len(got.Frames))
	}
	view := got.Frames[0]
	if got.Classes["Data"] != 1 {
		t.Errorf("classes=%v", got.Classes)
	}
	if view.Index != 0 || view.Len != len(frame) || view.Class != "Data" || !view.Decrypted {
		t.Errorf("view=%+v", view)
	}
	if view.Seq != 0x11 || view.Counter != 4242 {
		t.Errorf("seq=%d counter=%d", view.Seq, view.Counter)
	}
	if view.Dst != hex.EncodeToString(defaultInstall.CoordinatorEUI()) {
		t.Errorf("dst=%s", view.Dst)
	}
	if view.Dissect == "" || !strings.Contains(view.Dissect, "CoAP") {
		t.Errorf("dissect=%q", view.Dissect)
	}
	if view.CoAP == nil {
		t.Fatalf("no coap in %+v", view)
	}
	if view.CoAP.Type != "CON" || view.CoAP.Code != "0.03" || view.CoAP.MID != 0x4242 {
		t.Errorf("coap=%+v", view.CoAP)
	}
	if view.CoAP.URI != "d/temp?id=10" {
		t.Errorf("uri=%q", view.CoAP.URI)
	}
	if len(view.TLVs) != 1 || view.TLVs[0].Tag != "6280" || view.TLVs[0].Hex != "07d0" {
		t.Errorf("tlvs=%+v", view.TLVs)
	}
}

func TestDebugDissectReassemblesWithinOneRequest(t *testing.T) {
	url := newDebugServer(t)
	frames := debugFragmentFrames(t)
	hexes := []string{hex.EncodeToString(frames[0]), hex.EncodeToString(frames[1])}

	var got debugDissectResp
	if code := postJSON(t, http.MethodPost, url+"/api/debug/dissect",
		map[string]any{"frames": hexes}, &got); code != http.StatusOK {
		t.Fatalf("status=%d body=%+v", code, got)
	}
	if len(got.Frames) != 2 {
		t.Fatalf("frames=%d", len(got.Frames))
	}
	if got.Frames[0].CoAP != nil {
		t.Errorf("the first fragment already carries a message: %+v", got.Frames[0])
	}
	last := got.Frames[1]
	if last.CoAP == nil {
		t.Fatalf("the last fragment did not reassemble: %+v", last)
	}
	if last.CoAP.URI != "d/config?id=10" || last.CoAP.MID != 0x5150 {
		t.Errorf("coap=%+v", last.CoAP)
	}
	if !strings.Contains(last.Dissect, "REASSEMBLED") {
		t.Errorf("dissect=%q", last.Dissect)
	}

	// Each request starts its own session: the first fragment posted alone must
	// not carry over to a second request that posts the rest of the datagram.
	for i, h := range hexes {
		var split debugDissectResp
		if code := postJSON(t, http.MethodPost, url+"/api/debug/dissect",
			map[string]any{"hex": h}, &split); code != http.StatusOK {
			t.Fatalf("fragment %d: status=%d", i, code)
		}
		if split.Frames[0].CoAP != nil {
			t.Fatalf("fragment %d reassembled across requests: %+v", i, split.Frames[0])
		}
	}

	// The same two fragments in one request do complete, so the split above proves
	// the session boundary and not a broken fixture.
	var together debugDissectResp
	if code := postJSON(t, http.MethodPost, url+"/api/debug/dissect",
		map[string]any{"frames": hexes}, &together); code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	if together.Frames[1].CoAP == nil {
		t.Fatalf("the fixture no longer reassembles: %+v", together.Frames[1])
	}
}

// Classify calls a bare 69ec a Data frame, so the MIC-fail branch of the
// dissector used to read a sequence byte past the end of the slice.
func TestDissectTwoByteDataFrameDoesNotPanic(t *testing.T) {
	line := defaultInstall.NewDissector().Dissect([]byte{0x69, 0xec})
	if !strings.Contains(line, "Data") {
		t.Fatalf("dissect=%q", line)
	}
}

func TestDebugDecryptFrame(t *testing.T) {
	url := newDebugServer(t)
	frame := debugDataFrame(t, 0x22, 77, []byte{0x62, 0x80, 0x02, 0x07, 0xd0})

	var got debugFrameView
	if code := postJSON(t, http.MethodPost, url+"/api/debug/decrypt",
		map[string]any{"hex": hex.EncodeToString(frame)}, &got); code != http.StatusOK {
		t.Fatalf("status=%d body=%+v", code, got)
	}
	if !got.Decrypted || got.Seq != 0x22 || got.Counter != 77 || got.Error != "" {
		t.Fatalf("view=%+v", got)
	}
	if got.Plaintext == "" || got.CoAP == nil || got.CoAP.URI != "d/temp?id=10" {
		t.Fatalf("view=%+v", got)
	}
}

func TestDebugDecryptMICFailureIsAnAnswer(t *testing.T) {
	url := newDebugServer(t)
	frame := debugDataFrame(t, 0x33, 1, nil)
	frame[len(frame)-1] ^= 0xff

	var got debugFrameView
	code := postJSON(t, http.MethodPost, url+"/api/debug/decrypt",
		map[string]any{"hex": hex.EncodeToString(frame)}, &got)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%+v", code, got)
	}
	if got.Decrypted || got.Class != "Data" || got.Error != debugMICFailure {
		t.Fatalf("view=%+v", got)
	}
}

func TestDebugRejectsBadInput(t *testing.T) {
	url := newDebugServer(t)
	tooMany := make([]string, debugMaxFrames+1)
	for i := range tooMany {
		tooMany[i] = "69ec"
	}
	cases := []struct {
		name string
		path string
		body map[string]any
		want string // substring of the error, "" accepts any
	}{
		{"bad hex", "/api/debug/dissect", map[string]any{"hex": "zzzz"}, "bad hex"},
		{"short frame", "/api/debug/dissect", map[string]any{"hex": "69"}, "shorter than 3 bytes"},
		// Classify calls a 2-byte 69ec a Data frame, so the dissectors would read a
		// sequence byte that is not there.
		{"two byte data frame", "/api/debug/dissect", map[string]any{"hex": "69ec"}, "shorter than 3 bytes"},
		{"two byte data decrypt", "/api/debug/decrypt", map[string]any{"hex": "69ec"}, "shorter than 3 bytes"},
		{"nothing", "/api/debug/dissect", map[string]any{}, "required"},
		{"over cap", "/api/debug/dissect", map[string]any{"frames": tooMany}, "too many frames"},
		// An even length keeps the bad-hex branch out of it, so only the cap answers.
		{"long hex", "/api/debug/dissect", map[string]any{"hex": strings.Repeat("a", debugMaxHex+2)},
			"longer than " + strconv.Itoa(debugMaxHex) + " hex characters"},
		{"decrypt bad hex", "/api/debug/decrypt", map[string]any{"hex": "zzzz"}, "bad hex"},
		{"decrypt empty", "/api/debug/decrypt", map[string]any{}, "required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got debugDissectResp
			if code := postJSON(t, http.MethodPost, url+tc.path, tc.body, &got); code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%+v", code, got)
			}
			if !strings.Contains(got.Error, tc.want) {
				t.Errorf("error=%q, want it to name %q", got.Error, tc.want)
			}
		})
	}
}

// The three debug routes are absent unless --debug-api asks for them: they
// decrypt any frame a caller sends and stream live traffic on an API that
// carries no authentication.
func TestDebugRoutesAbsentWithoutTheFlag(t *testing.T) {
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})

	// The mux answers an unregistered route in plain text, so these go past the
	// JSON helpers.
	client := &http.Client{Timeout: 5 * time.Second}
	status := func(method, path string) int {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(`{"hex":"69ec00"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	for _, path := range []string{"/api/debug/dissect", "/api/debug/decrypt"} {
		if code := status(http.MethodPost, path); code != http.StatusNotFound {
			t.Errorf("POST %s: status=%d, want 404", path, code)
		}
	}
	if code := status(http.MethodGet, "/api/debug/rx"); code != http.StatusNotFound {
		t.Errorf("GET /api/debug/rx: status=%d, want 404", code)
	}
	// Health is the container check: it stays unconditional.
	if code := status(http.MethodGet, "/api/health"); code != http.StatusOK {
		t.Errorf("GET /api/health: status=%d, want 200", code)
	}
}

// The debug endpoints must answer while the engine is busy: they read only the
// installation, which no goroutine writes after the config load.
func TestDebugEndpointsDoNotHopToTheEngine(t *testing.T) {
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	d.debugAPI = true
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})

	release := make(chan struct{})
	busy := make(chan struct{})
	go func() {
		_, _ = d.do(func(*tado.EmuState) (any, error) {
			close(busy)
			<-release
			return nil, nil
		})
	}()
	<-busy
	defer close(release)

	var got debugFrameView
	if code := postJSON(t, http.MethodPost, srv.URL+"/api/debug/decrypt",
		map[string]any{"hex": hex.EncodeToString(debugDataFrame(t, 1, 1, nil))}, &got); code != http.StatusOK {
		t.Fatalf("status=%d body=%+v", code, got)
	}
	if !got.Decrypted {
		t.Fatalf("view=%+v", got)
	}
}
