package cc1101

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"freedo/tado"
	"go.bug.st/serial"
)

type fakePort struct {
	read   []byte
	writes [][]byte
}

func (p *fakePort) SetMode(*serial.Mode) error { return nil }
func (p *fakePort) Read(dst []byte) (int, error) {
	if len(p.read) == 0 {
		return 0, io.EOF
	}
	n := copy(dst, p.read)
	p.read = p.read[n:]
	return n, nil
}
func (p *fakePort) Write(data []byte) (int, error) {
	p.writes = append(p.writes, append([]byte(nil), data...))
	return len(data), nil
}
func (p *fakePort) Drain() error                                         { return nil }
func (p *fakePort) ResetInputBuffer() error                              { return nil }
func (p *fakePort) ResetOutputBuffer() error                             { return nil }
func (p *fakePort) SetDTR(bool) error                                    { return nil }
func (p *fakePort) SetRTS(bool) error                                    { return nil }
func (p *fakePort) GetModemStatusBits() (*serial.ModemStatusBits, error) { return nil, nil }
func (p *fakePort) SetReadTimeout(time.Duration) error                   { return nil }
func (p *fakePort) Close() error                                         { return nil }
func (p *fakePort) Break(time.Duration) error                            { return nil }

func TestBuildAutoAckFrameMatchesBuilder(t *testing.T) {
	coordinator := []byte{0x09, 0xa1, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00}
	cases := []struct {
		name   string
		fcf    byte
		seq    byte
		prefix []byte
		phase  uint16
	}{
		{"data", 0x69, 0x00, []byte{0x01, 0x0a, 0xce, 0xfa, 0x07}, 0x34b7},
		{"data-pending", 0x79, 0x4b, []byte{0x05, 0xa2, 0xca, 0xfa, 0x07}, 0x57d6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := append([]byte{16, tc.fcf, 0xec, tc.seq}, coordinator...)
			raw = append(raw, tc.prefix...)
			raw = append(raw, 0x80, 0x80)
			got, ok := buildAutoAckFrame(raw, coordinator, tc.phase)
			if !ok {
				t.Fatal("buildAutoAckFrame rejected a valid uplink")
			}
			dst := append(append([]byte(nil), tc.prefix...), 0xc5, 0x1b, 0x00)
			want := tado.BuildEnhAck(tc.seq, dst, coordinator, tc.phase)
			if !bytes.Equal(got, want) {
				t.Fatalf("Enhanced ACK = %x, want %x", got, want)
			}
		})
	}
}

func TestBuildAutoAckFrameRejectsInvalidUplink(t *testing.T) {
	coordinator := []byte{0x09, 0xa1, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00}
	raw := append([]byte{16, 0x69, 0xec, 0x00}, coordinator...)
	raw = append(raw, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0x80, 0x00)
	if ack, ok := buildAutoAckFrame(raw, coordinator, 0); ok || ack != nil {
		t.Fatal("invalid CRC frame generated an ACK")
	}
}

func TestAutoAckRequest(t *testing.T) {
	coordinator := []byte{0x09, 0xa1, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00}
	got, err := autoAckRequest(0x1234*time.Millisecond, coordinator, 0x5678)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x12, 0x34, 0x09, 0xa1, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00, 0x78, 0x56}
	if !bytes.Equal(got, want) {
		t.Fatalf("request = %x, want %x", got, want)
	}
	if _, err := autoAckRequest(0, coordinator, 0); err == nil {
		t.Fatal("zero timeout accepted")
	}
	if _, err := autoAckRequest(time.Nanosecond, coordinator, 0); err == nil {
		t.Fatal("sub-millisecond timeout accepted")
	}
	if _, err := autoAckRequest(time.Second, coordinator[:7], 0); err == nil {
		t.Fatal("short coordinator EUI accepted")
	}
}

func TestResponsePayloadLength(t *testing.T) {
	for _, n := range []int{0, maxReqPayload} {
		if err := responsePayloadLength(n); err != nil {
			t.Fatalf("length %d rejected: %v", n, err)
		}
	}
	for _, n := range []int{maxReqPayload + 1, 0xffff} {
		if err := responsePayloadLength(n); err == nil {
			t.Fatalf("length %d accepted", n)
		}
	}
}

func TestReqRejectsOversizedResponse(t *testing.T) {
	p := &fakePort{read: []byte{rspMagic, 0x00, 0x04, 0x01}}
	b := &Bridge{port: p, timeout: time.Millisecond}
	if _, _, err := b.req(cmdPing, nil, isBanner); err == nil {
		t.Fatal("oversized response accepted")
	}
}

// txPort answers the SPI transactions Send makes: a strobe returns a chip status
// byte, and a status-register read returns the scripted MARCSTATE or TXBYTES.
type txPort struct {
	fakePort
	marcstate byte
	txbytes   byte
	marcReads int
}

func (p *txPort) Write(data []byte) (int, error) {
	p.fakePort.Write(data)
	payload := []byte{0x0f}
	if data[1] == cmdXfer {
		body := data[4:]
		payload = make([]byte, len(body))
		if len(body) == 2 {
			switch body[0] &^ readBurst {
			case MARCSTATE:
				p.marcReads++
				payload[1] = p.marcstate
			case TXBYTES:
				payload[1] = p.txbytes
			}
		}
	}
	p.read = append(p.read, rspMagic, 0x00, 0x00, byte(len(payload)))
	p.read = append(p.read, payload...)
	return len(data), nil
}

// MCSM1 TXOFF_MODE=RX leaves the chip in RX after a TX, so Send must not wait
// for IDLE: it burned the whole timeout and reported a failure on every frame.
func TestSendCompletesWithChipInRX(t *testing.T) {
	p := &txPort{marcstate: 0x0d}
	b := &Bridge{port: p, timeout: time.Millisecond}
	start := time.Now()
	ok, err := b.Send([]byte{0x01, 0x02, 0x03}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Send reported a failure although the frame went out")
	}
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Fatalf("Send took %s, want a prompt return", el)
	}
	if p.marcReads != 1 {
		t.Fatalf("MARCSTATE reads = %d, want 1", p.marcReads)
	}
}

func TestSendWaitsForTXToFinish(t *testing.T) {
	cases := []struct {
		name      string
		marcstate byte
		txbytes   byte
	}{
		{"in TX", 0x13, 0x00},
		{"in TX_END", 0x14, 0x00},
		{"FIFO not drained", 0x0d, 0x05},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &txPort{marcstate: tc.marcstate, txbytes: tc.txbytes}
			b := &Bridge{port: p, timeout: time.Millisecond}
			ok, err := b.Send([]byte{0x01}, 20*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				t.Fatal("Send reported success while the TX was still running")
			}
		})
	}
}

// countStrobes reports how many times Send strobed addr on the fake port.
func countStrobes(p *txPort, addr byte) int {
	n := 0
	for _, w := range p.writes {
		if len(w) == 5 && w[1] == cmdStrobe && w[4] == addr {
			n++
		}
	}
	return n
}

// A short frame fits the FIFO before STX, so the streaming loop never runs and
// only the completion poll can see the underflow. MARCSTATE 0x16 comes with a
// TXBYTES of 0x80: a zero count that the drained-FIFO test accepted as success.
func TestSendReportsTXFIFOUnderflow(t *testing.T) {
	cases := []struct {
		name      string
		marcstate byte
		txbytes   byte
	}{
		{"MARCSTATE underflow", 0x16, 0x80},
		{"TXBYTES underflow bit", 0x0d, 0x80},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &txPort{marcstate: tc.marcstate, txbytes: tc.txbytes}
			b := &Bridge{port: p, timeout: time.Millisecond}
			ok, err := b.Send([]byte{0x01, 0x02, 0x03}, time.Second)
			if ok {
				t.Fatal("Send reported success for a frame lost to an underflow")
			}
			if err == nil || !strings.Contains(err.Error(), "underflow") {
				t.Fatalf("error = %v, want one that names the underflow", err)
			}
			if n := countStrobes(p, SFTX); n != 2 {
				t.Fatalf("SFTX strobes = %d, want 2 (setup plus recovery)", n)
			}
		})
	}
}

func TestSendSucceedsOnNormalCompletion(t *testing.T) {
	p := &txPort{marcstate: 0x0d, txbytes: 0x00}
	b := &Bridge{port: p, timeout: time.Millisecond}
	ok, err := b.Send([]byte{0x01, 0x02, 0x03}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Send reported a failure although the frame went out")
	}
	if n := countStrobes(p, SFTX); n != 1 {
		t.Fatalf("SFTX strobes = %d, want 1 (setup only)", n)
	}
}

func TestRecvAutoAck(t *testing.T) {
	coordinator := []byte{0x09, 0xa1, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00}
	raw := append([]byte{16, 0x69, 0xec, 0x22}, coordinator...)
	raw = append(raw, 0x01, 0x0a, 0xce, 0xfa, 0x07, 0x80, 0x80)
	tuned := append(append([]byte(nil), raw...), 0xf3)
	cases := []struct {
		name    string
		status  byte
		raw     []byte
		want    AutoAckStatus
		freqEst int
		err     bool
	}{
		{name: "sent", status: 0x00, raw: raw, want: AutoAckSent},
		{name: "no-frame", status: autoAckNoFrame, want: AutoAckNoFrame},
		{name: "frame-no-ack", status: autoAckFrameNoAck, raw: raw, want: AutoAckFrameNoAck},
		{name: "transmit-fail", status: autoAckTransmitFail, raw: raw, want: AutoAckTransmitFail},
		{name: "freqest", status: 0x00, raw: tuned, want: AutoAckSent, freqEst: -13},
		{name: "malformed", status: 0x00, raw: raw[:len(raw)-1], err: true},
		{name: "residual", status: 0x00, raw: append(append([]byte(nil), tuned...), 0), err: true},
		{name: "unknown", status: 0x99, raw: raw, err: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakePort{read: autoAckResponse(tc.status, tc.raw)}
			b := &Bridge{port: p, timeout: time.Millisecond}
			got, err := b.RecvAutoAck(0x1234*time.Millisecond, coordinator, 0x5678)
			if tc.err {
				if err == nil {
					t.Fatal("RecvAutoAck accepted invalid response")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.want || !bytes.Equal(got.Raw, tc.raw) {
				t.Fatalf("result = %+v, want status=%s raw=%x", got, tc.want, tc.raw)
			}
			if tc.want == AutoAckNoFrame {
				if got.Frame != nil {
					t.Fatal("no-frame response decoded a frame")
				}
			} else if got.Frame == nil || !got.Frame.CRCOK {
				t.Fatalf("frame = %+v, want CRC-OK", got.Frame)
			} else if got.Frame.HasFreqEst != (tc.freqEst != 0) || got.Frame.FreqEst != tc.freqEst {
				t.Fatalf("freqEst = %d,%t, want %d", got.Frame.FreqEst, got.Frame.HasFreqEst, tc.freqEst)
			}
			want := append([]byte{reqMagic, cmdAutoAck, 0x00, 0x0c, 0x12, 0x34}, coordinator...)
			want = append(want, 0x78, 0x56)
			if len(p.writes) != 1 || !bytes.Equal(p.writes[0], want) {
				t.Fatalf("request = %x, want %x", p.writes, want)
			}
		})
	}
}

func TestRecvAutoAckRestoresHostTimeout(t *testing.T) {
	coordinator := []byte{0x09, 0xa1, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00}
	p := &fakePort{read: autoAckResponse(autoAckNoFrame, nil)}
	b := &Bridge{port: p, timeout: time.Millisecond}
	if _, err := b.RecvAutoAck(6*time.Second, coordinator, 0); err != nil {
		t.Fatal(err)
	}
	if b.timeout != time.Millisecond {
		t.Fatalf("host timeout = %s, want %s", b.timeout, time.Millisecond)
	}
	want := []byte{reqMagic, cmdAutoAck, 0x00, 0x0c, 0x17, 0x70}
	if !bytes.Equal(p.writes[0][:6], want) {
		t.Fatalf("request prefix = %x, want %x", p.writes[0][:6], want)
	}
}

func TestSendRouterAdvertBurst(t *testing.T) {
	frame := []byte{0x49, 0xe8, 0x42}
	cases := []struct {
		name   string
		status byte
		sent   byte
		want   int
		err    bool
	}{
		{"sent minimum", 0x00, routerAdvertBurstMinSent, routerAdvertBurstMinSent, false},
		{"sent variable", 0x00, 201, 201, false},
		{"sent maximum", 0x00, routerAdvertBurstMaxSent, routerAdvertBurstMaxSent, false},
		{"sent too few", 0x00, routerAdvertBurstMinSent - 1, routerAdvertBurstMinSent - 1, true},
		{"input", routerAdvertBurstInput, 0, 0, true},
		{"tx-fail", routerAdvertBurstTXFail, 254, 254, true},
		{"unknown", 0x99, 3, 3, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakePort{read: autoAckResponse(tc.status, []byte{tc.sent})}
			b := &Bridge{port: p, timeout: time.Millisecond}
			sent, err := b.SendRouterAdvertBurst(frame, 2*time.Second)
			if (err != nil) != tc.err || sent != tc.want {
				t.Fatalf("result sent=%d err=%v, want %d error=%v", sent, err, tc.want, tc.err)
			}
			want := []byte{reqMagic, cmdRABurst, 0x00, 0x03, 0x49, 0xe8, 0x42}
			if len(p.writes) != 1 || !bytes.Equal(p.writes[0], want) {
				t.Fatalf("request=%x, want %x", p.writes, want)
			}
			if b.timeout != time.Millisecond {
				t.Fatalf("timeout=%s, want %s", b.timeout, time.Millisecond)
			}
		})
	}
	for _, frame := range [][]byte{nil, {0x49, 0xe8}, {0x69, 0xec, 0x42}, make([]byte, maxTrainFrame+1)} {
		b := &Bridge{port: &fakePort{}, timeout: time.Millisecond}
		if _, err := b.SendRouterAdvertBurst(frame, time.Second); err == nil {
			t.Fatalf("invalid router advertisement %x accepted", frame)
		}
	}
}

func autoAckResponse(status byte, raw []byte) []byte {
	response := []byte{rspMagic, status, byte(len(raw) >> 8), byte(len(raw))}
	return append(response, raw...)
}
