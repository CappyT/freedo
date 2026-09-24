package tado

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// syntheticImage is a fake application linked at 0xa000: a vector table with
// SP 0x20010000 and reset 0xa101, then a counting pattern.
func syntheticImage(n int) []byte {
	img := make([]byte, n)
	for i := range img {
		img[i] = byte(i * 7)
	}
	binary.LittleEndian.PutUint32(img[0:], 0x20010000)
	binary.LittleEndian.PutUint32(img[4:], 0x0000a101)
	return img
}

func TestCRC16CCITTFalseCheckValue(t *testing.T) {
	if got := CRC16CCITTFalse([]byte("123456789")); got != 0x29B1 {
		t.Fatalf("check value %#04x, want 0x29b1", got)
	}
}

func TestOTASlotCRCPadsWithErasedFlash(t *testing.T) {
	img := syntheticImage(1000)
	window := bytes.Repeat([]byte{0xFF}, OTASlotSize)
	copy(window, img)
	if got, want := OTASlotCRC(img), CRC16CCITTFalse(window); got != want {
		t.Fatalf("slot crc %#04x, want %#04x", got, want)
	}
}

func TestValidateOTAImage(t *testing.T) {
	good := syntheticImage(64)
	if err := ValidateOTAImage(good); err != nil {
		t.Fatal(err)
	}
	mutate := func(off int, v uint32) []byte {
		b := append([]byte(nil), good...)
		binary.LittleEndian.PutUint32(b[off:], v)
		return b
	}
	for name, img := range map[string][]byte{
		"empty":         nil,
		"too big":       syntheticImage(OTASlotSize + 1),
		"short":         good[:6],
		"sp in flash":   mutate(0, 0x0000a000),
		"sp past ram":   mutate(0, 0x20010004),
		"reset even":    mutate(4, 0x0000a100),
		"reset in boot": mutate(4, 0x00001001),
		"reset past":    mutate(4, 0x0007d001),
	} {
		if err := ValidateOTAImage(img); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ValidateOTAImage(syntheticImage(OTASlotSize)); err != nil {
		t.Errorf("full slot: %v", err)
	}
}

func TestOTAHeaderAndBlocks(t *testing.T) {
	v, _ := NewFwVersion(216, 0)
	img := syntheticImage(300)
	o, err := NewOTAImage(img, v)
	if err != nil {
		t.Fatal(err)
	}
	h := o.Header()
	if len(h) != OTAHeaderLen {
		t.Fatalf("header %d bytes, want %d", len(h), OTAHeaderLen)
	}
	want := []byte{
		0x01, 0xa8, 0x00, 0x00, 0x01, 0x2c, // size, no length byte
		0x01, 0xa9, 0x01, 0x00,
		0x01, 0xaa, 0x02, byte(o.CRC >> 8), byte(o.CRC),
		0x01, 0xab, 0x02, 0x36, 0x00, // 216.0 = 0x3600
	}
	if !bytes.Equal(h, want) {
		t.Fatalf("header % x\nwant   % x", h, want)
	}
	blocks := o.Blocks()
	if len(blocks) != 3 || len(blocks[0]) != 128 || len(blocks[1]) != 128 || len(blocks[2]) != 320-256 {
		t.Fatalf("blocks: %d", len(blocks))
	}
	if !bytes.Equal(bytes.Join(blocks, nil), append(want, img...)) {
		t.Fatal("blocks do not rebuild header+image")
	}
}

func TestPlanOTARefusals(t *testing.T) {
	run, _ := NewFwVersion(215, 1)
	confirmed := &FwState{DeviceType: 8, Running: FwSlot{Present: true, Version: run, Flags: FwSlotHasImage | FwSlotActive | FwSlotConfirmed}}
	unconfirmed := &FwState{DeviceType: 8, Running: FwSlot{Present: true, Version: run, Flags: FwSlotHasImage | FwSlotActive}}
	lower, _ := NewFwVersion(215, 0)
	img := syntheticImage(1000)

	if _, err := PlanOTA(unconfirmed, img, run); err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Errorf("unconfirmed: %v", err)
	}
	if _, err := PlanOTA(confirmed, img, lower); err == nil || !strings.Contains(err.Error(), "below running") {
		t.Errorf("downgrade: %v", err)
	}
	if _, err := PlanOTA(confirmed, img[:4], run); err == nil {
		t.Error("invalid image accepted")
	}
	p, err := PlanOTA(confirmed, img, run)
	if err != nil {
		t.Fatal(err)
	}
	if p.Blocks != 8 { // (20+1000)/128 rounded up
		t.Fatalf("blocks %d, want 8", p.Blocks)
	}
}

type fakeOTA struct {
	codes   map[int][]string // per block, one entry per try; "" = no response
	echo    map[int]int      // per block, the Block1 number the answer echoes
	tries   map[int]int
	sent    [][]byte
	more    []bool
	aborted bool
	onPost  func(num int)
}

func (f *fakeOTA) PostBlock(ctx context.Context, num int, more bool, data []byte) (OTAReply, error) {
	try := f.tries[num]
	f.tries[num]++
	f.sent = append(f.sent, data)
	f.more = append(f.more, more)
	if f.onPost != nil {
		f.onPost(num)
	}
	if err := ctx.Err(); err != nil {
		return OTAReply{}, err
	}
	code := ""
	if seq := f.codes[num]; try < len(seq) {
		code = seq[try]
	} else if more {
		code = "2.31"
	} else {
		code = "2.04"
	}
	if code == "" {
		return OTAReply{}, errors.New("timeout")
	}
	echo := num
	if e, ok := f.echo[num]; ok {
		echo = e
	}
	return OTAReply{Code: code, Block1: echo}, nil
}

func (f *fakeOTA) Abort() error { f.aborted = true; return nil }

func newFakeOTA(codes map[int][]string) *fakeOTA {
	return &fakeOTA{codes: codes, tries: map[int]int{}}
}

func threeBlocks() [][]byte {
	return [][]byte{bytes.Repeat([]byte{1}, 128), bytes.Repeat([]byte{2}, 128), {3, 3}}
}

func TestSendOTAHappyPathOneBlockInFlight(t *testing.T) {
	f := newFakeOTA(nil)
	var done []int
	if err := SendOTA(context.Background(), f, threeBlocks(), 2, func(n int) { done = append(done, n) }); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 3 || f.aborted || len(done) != 3 {
		t.Fatalf("sent %d aborted %v done %v", len(f.sent), f.aborted, done)
	}
	if !f.more[0] || !f.more[1] || f.more[2] {
		t.Fatalf("more flags %v", f.more)
	}
}

func TestSendOTARetriesTheSameBlock(t *testing.T) {
	f := newFakeOTA(map[int][]string{1: {"", "", "2.31"}})
	if err := SendOTA(context.Background(), f, threeBlocks(), 2, nil); err != nil {
		t.Fatal(err)
	}
	if f.tries[1] != 3 || len(f.sent) != 5 || !bytes.Equal(f.sent[1], f.sent[2]) || !bytes.Equal(f.sent[2], f.sent[3]) {
		t.Fatalf("tries %v sent %d", f.tries, len(f.sent))
	}
}

func TestSendOTAAbortsOnExhaustedRetries(t *testing.T) {
	f := newFakeOTA(map[int][]string{1: {"", "", ""}})
	err := SendOTA(context.Background(), f, threeBlocks(), 2, nil)
	if err == nil || !f.aborted || f.tries[2] != 0 {
		t.Fatalf("err %v aborted %v tries %v", err, f.aborted, f.tries)
	}
}

func TestSendOTAAbortsOnUnexpectedCode(t *testing.T) {
	for _, code := range []string{"4.08", "4.13", "5.00", "2.04", "RST"} {
		f := newFakeOTA(map[int][]string{0: {code}})
		if err := SendOTA(context.Background(), f, threeBlocks(), 2, nil); err == nil || !f.aborted || len(f.sent) != 1 {
			t.Errorf("%s: err %v aborted %v sent %d", code, err, f.aborted, len(f.sent))
		}
	}
	f := newFakeOTA(map[int][]string{2: {"2.31"}})
	if err := SendOTA(context.Background(), f, threeBlocks(), 2, nil); err == nil || !f.aborted {
		t.Errorf("last block 2.31: err %v aborted %v", err, f.aborted)
	}
	f = newFakeOTA(map[int][]string{2: {"4.12"}})
	if err := SendOTA(context.Background(), f, threeBlocks(), 2, nil); err == nil || errors.Is(err, ErrOTAUnverified) || !f.aborted {
		t.Errorf("last block 4.12 on the first try: err %v aborted %v", err, f.aborted)
	}
}

func TestSendOTAAbortsOnWrongBlock1Echo(t *testing.T) {
	f := newFakeOTA(nil)
	f.echo = map[int]int{1: 0}
	err := SendOTA(context.Background(), f, threeBlocks(), 2, nil)
	if err == nil || !strings.Contains(err.Error(), "echoing block 0") || !f.aborted || f.tries[2] != 0 {
		t.Fatalf("err %v aborted %v tries %v", err, f.aborted, f.tries)
	}
}

func TestSendOTALastBlockUnclearAnswerIsNotAborted(t *testing.T) {
	for name, codes := range map[string][]string{
		"4.12 on the resend": {"", "4.12"},
		"no answer at all":   {"", "", ""},
	} {
		f := newFakeOTA(map[int][]string{2: codes})
		err := SendOTA(context.Background(), f, threeBlocks(), 2, nil)
		if !errors.Is(err, ErrOTAUnverified) || f.aborted {
			t.Errorf("%s: err %v aborted %v", name, err, f.aborted)
		}
	}
}

func TestSendOTACancelAborts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := newFakeOTA(nil)
	f.onPost = func(num int) {
		if num == 1 {
			cancel()
		}
	}
	err := SendOTA(ctx, f, threeBlocks(), 2, nil)
	if !errors.Is(err, context.Canceled) || !f.aborted || f.tries[2] != 0 {
		t.Fatalf("err %v aborted %v tries %v", err, f.aborted, f.tries)
	}
	ctx, cancel = context.WithCancel(context.Background())
	f = newFakeOTA(nil)
	f.onPost = func(num int) {
		if num == 2 {
			cancel()
		}
	}
	if err := SendOTA(ctx, f, threeBlocks(), 2, nil); !errors.Is(err, context.Canceled) || !f.aborted {
		t.Fatalf("cancel on the last block: err %v aborted %v", err, f.aborted)
	}
}

func TestBuildFwBlockCoAP(t *testing.T) {
	dst := testDstEUI(t)
	data := bytes.Repeat([]byte{0xa5}, OTABlockSize)
	pkt, err := BuildFwBlockCoAP(5, true, data, 0x1234, []byte{1, 2, 3, 4}, BridgeEUI, dst)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseCoAP(append(pkt, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	b1, ok := m.Option(27)
	if m.Type != "CON" || m.Code != "0.02" || m.Path != "d/fw" || !ok || !bytes.Equal(b1.Val, []byte{0x5b}) || !bytes.Equal(m.Payload, data) {
		t.Fatalf("block: %s block1=%x payload %d", m.Summary(), b1.Val, len(m.Payload))
	}
	if block1Echo(m) != 5 {
		t.Fatalf("echo %d", block1Echo(m))
	}
	pkt, err = BuildFwBlockCoAP(300, false, data[:7], 0x1235, []byte{1}, BridgeEUI, dst)
	if err != nil {
		t.Fatal(err)
	}
	if m, err = ParseCoAP(append(pkt, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if b1, _ := m.Option(27); !bytes.Equal(b1.Val, []byte{0x12, 0xc3}) {
		t.Fatalf("last block1=%x, want 12c3", b1.Val)
	}
	if _, err := BuildFwBlockCoAP(0, true, data[:100], 1, []byte{1}, BridgeEUI, dst); err == nil {
		t.Error("short intermediate block accepted")
	}
	pkt, err = BuildFwAbortCoAP(0x1236, []byte{9}, BridgeEUI, dst)
	if err != nil {
		t.Fatal(err)
	}
	if m, err = ParseCoAP(append(pkt, 0, 0)); err != nil || m.Code != "0.04" || m.Path != "d/fw" {
		t.Fatalf("abort: %v %v", m, err)
	}
}

func TestPlanDeviceOTARefusals(t *testing.T) {
	run, _ := NewFwVersion(215, 1)
	state := &FwState{DeviceType: OTADeviceType, Running: FwSlot{Present: true, Version: run, Flags: FwSlotHasImage | FwSlotActive | FwSlotConfirmed}}
	valve := &Device{Kind: KindValve, Serial: "VA0000000001"}
	img := syntheticImage(1000)
	if _, err := PlanDeviceOTA(valve, "valve", state, img, run); err != nil {
		t.Fatal(err)
	}
	other := *state
	other.DeviceType = 9
	for name, tc := range map[string]struct {
		dev   *Device
		kind  string
		state *FwState
		want  string
	}{
		"kind mismatch": {valve, "thermostat", state, "image is for a thermostat"},
		"not a valve":   {&Device{Kind: "bridge"}, "bridge", state, "only valves and thermostats"},
		"no fw/state":   {valve, "valve", nil, "no fw/state"},
		"fw type":       {valve, "valve", &other, "device type 9"},
	} {
		if _, err := PlanDeviceOTA(tc.dev, tc.kind, tc.state, img, run); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", name, err, tc.want)
		}
	}
}

func TestVerifyOTA(t *testing.T) {
	want, _ := NewFwVersion(216, 0)
	old, _ := NewFwVersion(215, 1)
	ok := &FwState{Running: FwSlot{Present: true, Version: want, Flags: FwSlotHasImage | FwSlotActive | FwSlotConfirmed}}
	if err := VerifyOTA(ok, want); err != nil {
		t.Fatal(err)
	}
	if err := VerifyOTA(&FwState{Running: FwSlot{Version: old, Flags: FwSlotConfirmed}}, want); err == nil {
		t.Error("old version verified")
	}
	if err := VerifyOTA(&FwState{Running: FwSlot{Version: want, Flags: FwSlotActive}}, want); err == nil {
		t.Error("unconfirmed verified")
	}
}
