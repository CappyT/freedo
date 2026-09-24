package cc1101

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"freedo/tado"
)

func cslTxData() []byte {
	return []byte{
		0x69, 0xec, 0x56,
		0x01, 0x0a, 0xce, 0xfa, 0x07, 0xc5, 0x1b, 0x00,
		0x09, 0xa1, 0xce, 0xfa, 0x07,
	}
}

func TestSendCSLDataAckStatusContract(t *testing.T) {
	data := cslTxData()
	cases := []struct {
		name   string
		status byte
		want   string
	}{
		{"no frame", cslTxNoAck, ""},
		{"invalid frame", cslTxBadAck, "received invalid Enhanced ACK"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakePort{read: autoAckResponse(tc.status, []byte{23})}
			b := &Bridge{port: p, timeout: time.Millisecond}
			_, err := b.SendCSLData(data, []byte{0x0a, 0x01}, time.Millisecond, time.Second, false, false, 0)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("error=%v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}

func cslTxAck(data []byte) []byte {
	dst := append(append([]byte(nil), data[11:16]...), 0xc5, 0x1b, 0x00)
	ack := tado.BuildEnhAck(data[2], dst, data[3:11], 0)
	return append(append([]byte{byte(len(ack))}, ack...), 0, 0x80)
}

func TestSendCSLData(t *testing.T) {
	data := cslTxData()
	cases := []struct {
		name   string
		status byte
		body   []byte
		ack    bool
		err    bool
	}{
		{"ack", 0x00, append([]byte{23}, cslTxAck(data)...), true, false},
		{"no ack", cslTxNoAck, []byte{23}, false, false},
		{"input", cslTxInput, []byte{0}, false, true},
		{"tx fail", cslTxTXFail, []byte{4}, false, true},
		{"bad ack", cslTxBadAck, []byte{23}, false, true},
		{"no anchor", cslTxNoAnchor, []byte{0}, false, true},
		{"unknown", 0x99, []byte{23}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakePort{read: autoAckResponse(tc.status, tc.body)}
			b := &Bridge{port: p, timeout: time.Millisecond}
			got, err := b.SendCSLData(data, []byte{0x0a, 0x01}, 50*time.Millisecond, time.Second, true, false, 0)
			if (err != nil) != tc.err {
				t.Fatalf("error=%v, want error=%v", err, tc.err)
			}
			if got == nil || got.Sent != int(tc.body[0]) {
				t.Fatalf("result=%+v, want sent=%d", got, tc.body[0])
			}
			if (got.Ack != nil) != tc.ack {
				t.Fatalf("ack=%v, want=%v", got.Ack, tc.ack)
			}
			want := append([]byte{reqMagic, cmdCSLTx, 0, byte(len(data) + 5), 0, 50, 1, 0x0a, 0x01}, data...)
			if len(p.writes) != 1 || !bytes.Equal(p.writes[0], want) {
				t.Fatalf("request=%x, want=%x", p.writes, want)
			}
			if b.timeout != time.Millisecond {
				t.Fatalf("timeout=%s, want=%s", b.timeout, time.Millisecond)
			}
		})
	}
}

func TestSendCSLDataFollowResponse(t *testing.T) {
	data := cslTxData()
	ack := cslTxAck(data)
	follow := cslTxAck(data)
	follow2 := append([]byte(nil), follow...)
	body := append([]byte{23, byte(len(ack))}, ack...)
	body = append(body, 2, byte(len(follow)))
	body = append(body, follow...)
	body = append(body, byte(len(follow2)))
	body = append(body, follow2...)
	body = append(body, 0)
	p := &fakePort{read: autoAckResponse(0x00, body)}
	b := &Bridge{port: p, timeout: time.Millisecond}
	got, err := b.SendCSLData(data, []byte{0x0a, 0x01}, 50*time.Millisecond, time.Second, true, true, 161)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ack == nil || len(got.Follow) != 2 || got.Follow[0] == nil || got.Follow[1] == nil || got.Dropped != 0 {
		t.Fatalf("result=%+v", got)
	}
	want := append([]byte{reqMagic, cmdCSLTx, 0, byte(len(data) + 7), 0, 50, cslTxFlagBootstrap | cslTxFlagFollow, 0x0a, 0x01, 0xa1, 0x00}, data...)
	if len(p.writes) != 1 || !bytes.Equal(p.writes[0], want) {
		t.Fatalf("request=%x, want=%x", p.writes, want)
	}
}

func TestSendCSLDataRejectsMalformedFollowResponse(t *testing.T) {
	data := cslTxData()
	ack := cslTxAck(data)
	for _, body := range [][]byte{
		append([]byte{23, byte(len(ack))}, ack...),
		append(append([]byte{23, byte(len(ack))}, ack...), 7),
		append(append([]byte{23, byte(len(ack))}, ack...), 1, 32),
	} {
		p := &fakePort{read: autoAckResponse(0x00, body)}
		b := &Bridge{port: p, timeout: time.Millisecond}
		if _, err := b.SendCSLData(data, []byte{0x0a, 0x01}, time.Millisecond, time.Second, false, true, 0); err == nil {
			t.Fatalf("malformed follow response %x accepted", body)
		}
	}
}

func TestSendCSLDataRejectsMalformedResponse(t *testing.T) {
	data := cslTxData()
	for _, body := range [][]byte{
		nil,
		{23},
		append(append([]byte{23}, cslTxAck(data)...), 0),
		append([]byte{2}, cslTxAck(data)...),
	} {
		b := &Bridge{port: &fakePort{read: autoAckResponse(0x00, body)}, timeout: time.Millisecond}
		if _, err := b.SendCSLData(data, []byte{0x0a, 0x01}, time.Millisecond, time.Second, false, false, 0); err == nil {
			t.Fatalf("malformed response %x accepted", body)
		}
	}
}

func TestSendCSLDataRejectsInvalidInput(t *testing.T) {
	data := cslTxData()
	cases := []struct {
		frame []byte
		short []byte
		tmo   time.Duration
	}{
		{data[:15], []byte{0x0a, 0x01}, time.Millisecond},
		{data, []byte{0x01, 0x0a}, time.Millisecond},
		{data, []byte{0x04}, time.Millisecond},
		{data, []byte{0x0a, 0x01}, 0},
		{data, []byte{0x0a, 0x01}, 1001 * time.Millisecond},
	}
	for _, tc := range cases {
		p := &fakePort{}
		b := &Bridge{port: p, timeout: time.Millisecond}
		if _, err := b.SendCSLData(tc.frame, tc.short, tc.tmo, time.Second, false, false, 0); err == nil {
			t.Fatal("invalid CSLTX input accepted")
		}
		if len(p.writes) != 0 {
			t.Fatal("invalid CSLTX input reached the bridge")
		}
	}
}

func TestSendCSLDataRestoresHostTimeout(t *testing.T) {
	p := &fakePort{read: autoAckResponse(cslTxNoAck, []byte{23})}
	b := &Bridge{port: p, timeout: time.Millisecond}
	if _, err := b.SendCSLData(cslTxData(), []byte{0x0a, 0x01}, time.Millisecond, 6*time.Second, false, false, 0); err != nil {
		t.Fatal(err)
	}
	if b.timeout != time.Millisecond {
		t.Fatalf("timeout=%s, want=%s", b.timeout, time.Millisecond)
	}
}

func TestSendCSLDataBootstrapFrameCount(t *testing.T) {
	data := cslTxData()
	for _, tc := range []struct {
		status byte
		body   []byte
		err    bool
	}{
		{0x00, append([]byte{255}, cslTxAck(data)...), false},
		{cslTxNoAck, []byte{255}, false},
		{0x00, append([]byte{255}, cslTxAck(data)...), true},
	} {
		p := &fakePort{read: autoAckResponse(tc.status, tc.body)}
		b := &Bridge{port: p, timeout: time.Millisecond}
		bootstrap := !tc.err
		_, err := b.SendCSLData(data, []byte{0x0a, 0x01}, time.Millisecond, time.Second, bootstrap, false, 0)
		if (err != nil) != tc.err {
			t.Fatalf("status=%02x bootstrap=%t err=%v", tc.status, bootstrap, err)
		}
	}
}

func TestCSLTxAckPhaseWireOffset(t *testing.T) {
	data := cslTxData()
	dst := append(append([]byte(nil), data[11:16]...), 0xc5, 0x1b, 0x00)
	ack := tado.BuildEnhAck(data[2], dst, data[3:11], 161)
	raw := append([]byte{29}, ack...)
	if raw[22] != 0xa1 || raw[23] != 0x00 {
		t.Fatalf("phase wire bytes=%02x%02x, want a100", raw[22], raw[23])
	}
}
