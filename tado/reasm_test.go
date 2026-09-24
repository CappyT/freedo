package tado

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"os"
	"testing"
)

// TestReassembleGolden: a real FRAG1+FRAGN pair (a large CoAP resource-discovery
// response, split by 6LoWPAN fragmentation across two Data-pending 0x79ec frames)
// reassembles into one CoAP message whose payload is the device's coap:// resource list.
func TestReassembleGolden(t *testing.T) {
	f, err := os.Open("testdata/frames_frag.txt")
	if err != nil {
		t.Fatalf("open frag fixture: %v", err)
	}
	defer f.Close()
	ra := NewReassembler()
	var msg *CoAPMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<16)
	n := 0
	for sc.Scan() {
		fr, err := hex.DecodeString(sc.Text())
		if err != nil {
			t.Fatalf("bad hex: %v", err)
		}
		p, err := ParseData(NetworkKey, fr)
		if err != nil {
			t.Fatalf("fragment %d decrypt: %v", n, err)
		}
		if !IsFragment(p.Plaintext) {
			t.Fatalf("frame %d not detected as a 6LoWPAN fragment", n)
		}
		if m, done := ra.Add(p); done {
			msg = m
		}
		n++
	}
	if msg == nil {
		t.Fatal("fragments did not reassemble into a complete datagram")
	}
	if !bytes.Contains(msg.Payload, []byte("coap://")) {
		t.Errorf("reassembled payload lacks coap:// resource URIs: %x", msg.Payload)
	}
	t.Logf("reassembled %d-byte CoAP %s payload from %d fragments", len(msg.Payload), msg.Code, n)
}
