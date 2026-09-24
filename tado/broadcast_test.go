package tado

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestBuildICMPv6EchoRequest(t *testing.T) {
	src := mustHex("c51b0004dbd30000")
	dst := mustHex("05a2cafa07c51b00")
	got, err := BuildICMPv6EchoRequest(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 || !bytes.Equal(got[:5], []byte{0x7a, 0x33, nhICMPv6, icmpEchoRequest, 0}) {
		t.Fatalf("Echo Request=%x, want 7a333a8000<checksum>", got)
	}
	msg := append([]byte(nil), got[3:]...)
	checksum := binary.BigEndian.Uint16(msg[2:4])
	msg[2], msg[3] = 0, 0
	if want := icmpChecksumV6(linkLocalFromWire(src), linkLocalFromWire(dst), msg); checksum != want {
		t.Fatalf("Echo checksum=%04x, want %04x", checksum, want)
	}
}
