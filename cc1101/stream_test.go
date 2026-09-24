package cc1101

import (
	"bytes"
	"testing"
	"time"
)

// streamPort plays the firmware stream: the frames of ahead, then after the
// abort byte the frames still in flight and STREAM_END, then a ping response.
type streamPort struct {
	fakePort
	inFlight []byte
}

func (p *streamPort) Write(data []byte) (int, error) {
	p.fakePort.Write(data)
	switch {
	case len(data) == 1 && data[0] == 0x00:
		p.read = append(p.read, p.inFlight...)
		p.read = append(p.read, rspMagic, streamEnd, 0x00)
	case len(data) >= 2 && data[0] == reqMagic && data[1] == cmdPing:
		p.read = append(p.read, autoAckResponse(0, []byte("CC1101BRIDGE v2.5"))...)
	}
	return len(data), nil
}

// streamFrameBytes is one 5A F1 record carrying payload with a CRC-OK status.
func streamFrameBytes(payload ...byte) []byte {
	blob := append([]byte{byte(len(payload))}, payload...)
	blob = append(blob, 0x10, 0x80|0x20)
	const freqEst = 0x03
	x := byte(freqEst)
	for _, c := range blob {
		x ^= c
	}
	out := []byte{rspMagic, streamFrame, byte(len(blob))}
	out = append(out, blob...)
	return append(out, freqEst, x)
}

// TestStreamUntilEndsAtStreamEnd: a stop from the callback aborts the firmware
// stream once, the reader consumes it up to STREAM_END, and the next request
// reads its own response instead of the stream's tail.
func TestStreamUntilEndsAtStreamEnd(t *testing.T) {
	p := &streamPort{inFlight: streamFrameBytes(0x02)}
	p.read = streamFrameBytes(0x01)
	b := &Bridge{port: p, timeout: time.Millisecond}
	var seen []byte
	err := b.StreamUntil(time.Second, func(f *RxFrame) bool {
		seen = append(seen, f.Payload...)
		return true
	})
	if err != nil {
		t.Fatalf("StreamUntil: %v", err)
	}
	if !bytes.Equal(seen, []byte{0x01, 0x02}) {
		t.Fatalf("frames seen %x, want 0102: the frame in flight before STREAM_END is dropped", seen)
	}
	aborts := 0
	for _, w := range p.writes[1:] {
		if bytes.Equal(w, []byte{0x00}) {
			aborts++
		}
	}
	if aborts != 1 || !bytes.Equal(p.writes[0][:2], []byte{reqMagic, cmdStream}) {
		t.Fatalf("writes %x, want the stream request and one abort byte", p.writes)
	}
	if got, err := b.Ping(); err != nil || got != "CC1101BRIDGE v2.5" {
		t.Fatalf("ping after the stream: %q %v, want the banner", got, err)
	}
}
