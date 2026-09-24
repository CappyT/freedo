package tado

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"
)

// RFC 4944 size and offsets refer to the uncompressed IPv6 datagram. FRAG1
// carries the compressed IPv6 and UDP headers, which stand for the 48 bytes of
// the uncompressed ones, and then the first CoAP bytes. A FRAGN at offset o
// carries the CoAP bytes from o-48. The CoAP message is parsed once, over the
// whole datagram: an option can cross the end of FRAG1.

const defaultFragmentReassemblyTimeout = 60 * time.Second

type reasmKey struct {
	src  string
	dst  string
	size int
	tag  uint16
}

type reasmBuf struct {
	frag1   []byte // FRAG1 body: compressed headers and the first CoAP bytes
	coap1   int    // CoAP bytes in frag1
	pieces  []reasmPiece
	frames  int
	started time.Time
}

type reasmPiece struct {
	off  int
	data []byte
}

// Reassembler collects 6LoWPAN fragments. It is single-goroutine state.
type Reassembler struct {
	m       map[reasmKey]*reasmBuf
	now     func() time.Time
	timeout time.Duration
}

// NewReassembler uses the RFC 4944 60-second timeout.
func NewReassembler() *Reassembler {
	return NewReassemblerWithClock(time.Now, defaultFragmentReassemblyTimeout)
}

// NewReassemblerWithClock permits deterministic time. Nil and non-positive values use RFC defaults.
func NewReassemblerWithClock(now func() time.Time, timeout time.Duration) *Reassembler {
	if now == nil {
		now = time.Now
	}
	if timeout <= 0 {
		timeout = defaultFragmentReassemblyTimeout
	}
	return &Reassembler{m: map[reasmKey]*reasmBuf{}, now: now, timeout: timeout}
}

// IsFragment reports a 6LoWPAN fragment in decrypted app plaintext.
func IsFragment(pt []byte) bool {
	if len(pt) < 9 {
		return false
	}
	d := pt[8]
	return d&0xF8 == 0xC0 || d&0xF8 == 0xE0
}

// Fragment is the RFC 4944 fragmentation header of one frame.
type Fragment struct {
	First  bool   // FRAG1, the head of the datagram; a FRAGN carries a later part
	Size   int    // total size of the uncompressed IPv6 datagram, bytes
	Tag    uint16 // datagram tag, one per datagram and per source
	Offset int    // where this fragment starts in the datagram; 0 for FRAG1
}

// String renders the header for a log line.
func (fr Fragment) String() string {
	name := "FRAGN"
	if fr.First {
		name = "FRAG1"
	}
	return fmt.Sprintf("%s tag=%04x size=%d off=%d", name, fr.Tag, fr.Size, fr.Offset)
}

// ParseFragment reads the fragmentation header out of decrypted app plaintext.
// It reports false when the plaintext holds no readable fragment.
func ParseFragment(pt []byte) (Fragment, bool) {
	if len(pt) < 11 || !IsFragment(pt) {
		return Fragment{}, false
	}
	body := pt[8 : len(pt)-2]
	if len(body) < lowpanFrag1HeaderLen {
		return Fragment{}, false
	}
	fr := Fragment{
		First: body[0]&0xF8 == 0xC0,
		Size:  int(binary.BigEndian.Uint16(body[0:2]) & 0x07FF),
		Tag:   binary.BigEndian.Uint16(body[2:4]),
	}
	if !fr.First {
		if len(body) < lowpanFragNHeaderLen {
			return Fragment{}, false
		}
		fr.Offset = int(body[4]) * 8
	}
	return fr, true
}

// Reassembly reports what one fragment did to the datagram it belongs to.
type Reassembly struct {
	Fragment Fragment
	Frames   int          // fragments held for this datagram, this one included
	Held     int          // datagram bytes held
	Message  *CoAPMessage // the whole message, set by the fragment that completes it
}

// Incomplete counts the datagrams the reassembler still holds pieces of.
func (r *Reassembler) Incomplete() int { return len(r.m) }

// Expire removes timed-out incomplete datagrams.
func (r *Reassembler) Expire() int {
	now := r.now()
	n := 0
	for k, buf := range r.m {
		if !now.Before(buf.started.Add(r.timeout)) {
			delete(r.m, k)
			n++
		}
	}
	return n
}

// Add returns a complete reassembled CoAP message when this frame finishes it.
func (r *Reassembler) Add(f *Frame) (*CoAPMessage, bool) {
	rs, ok := r.AddFrame(f)
	if !ok || rs.Message == nil {
		return nil, false
	}
	return rs.Message, true
}

// AddFrame files one fragment and reports the state of its datagram, so a
// sniffer can show what it holds while the datagram is still incomplete. It
// reports false when the frame carries no usable fragment, which includes an
// overlap: an overlap drops the whole datagram. Reassembly.Fragment still holds
// the header of a fragment the reassembler refused.
func (r *Reassembler) AddFrame(f *Frame) (Reassembly, bool) {
	r.Expire()
	pt := f.Plaintext
	fr, ok := ParseFragment(pt)
	if !ok {
		return Reassembly{}, false
	}
	rs := Reassembly{Fragment: fr}
	if fr.Size < ipv6UDPHeaderLen || fr.Size > maxFragmentIPv6Datagram {
		return rs, false
	}
	body := pt[8 : len(pt)-2] // strip the Tado shim and this fragment's 2-byte trailer
	k := reasmKey{src: hexEUI(f.SrcEUI()), dst: hexEUI(f.DstEUI), size: fr.Size, tag: fr.Tag}
	now := r.now()

	switch {
	case fr.First:
		frag1 := append([]byte{}, body[lowpanFrag1HeaderLen:]...)
		hdr, err := coapOffset(frag1)
		coap1 := len(frag1) - hdr
		if err != nil || coap1+ipv6UDPHeaderLen > fr.Size {
			return rs, false
		}
		buf := r.m[k]
		if buf == nil {
			buf = &reasmBuf{started: now}
			r.m[k] = buf
		} else if buf.frag1 != nil && !bytes.Equal(buf.frag1, frag1) {
			// A different first fragment reuses the tag for a new datagram.
			buf = &reasmBuf{started: now}
			r.m[k] = buf
		}
		buf.frag1, buf.coap1 = frag1, coap1
		if firstFragmentOverlaps(buf) {
			delete(r.m, k)
			return rs, false
		}
	default:
		data := append([]byte{}, body[lowpanFragNHeaderLen:]...)
		if fr.Offset < ipv6UDPHeaderLen || fr.Offset+len(data) > fr.Size {
			return rs, false
		}
		buf := r.m[k]
		if buf == nil {
			buf = &reasmBuf{started: now}
			r.m[k] = buf
		}
		if addPiece(buf, fr.Offset, data) {
			delete(r.m, k)
			return rs, false
		}
	}

	buf := r.m[k]
	if buf == nil {
		return rs, false
	}
	buf.frames++
	rs.Frames, rs.Held = buf.frames, heldBytes(buf)
	if buf.frag1 != nil && reassemblyComplete(k, buf) {
		delete(r.m, k)
		if rs.Message = assemble(buf); rs.Message == nil {
			return rs, false
		}
	}
	return rs, true
}

// heldBytes counts the datagram bytes a buffer holds. The first fragment stands
// for the whole uncompressed IPv6 and UDP header, not for the bytes on air.
func heldBytes(buf *reasmBuf) int {
	n := 0
	if buf.frag1 != nil {
		n = ipv6UDPHeaderLen + buf.coap1
	}
	for _, p := range buf.pieces {
		n += len(p.data)
	}
	return n
}

// addPiece accepts exact retransmissions and rejects other overlaps.
func addPiece(buf *reasmBuf, off int, data []byte) bool {
	end := off + len(data)
	for i := range buf.pieces {
		p := buf.pieces[i]
		pEnd := p.off + len(p.data)
		if off == p.off && bytes.Equal(data, p.data) {
			return false
		}
		if off < pEnd && end > p.off {
			return true
		}
	}
	buf.pieces = append(buf.pieces, reasmPiece{off: off, data: data})
	return false
}

func firstFragmentOverlaps(buf *reasmBuf) bool {
	end := ipv6UDPHeaderLen + buf.coap1
	for _, p := range buf.pieces {
		if p.off < end {
			return true
		}
	}
	return false
}

// reassemblyComplete requires a contiguous CoAP tail.
func reassemblyComplete(k reasmKey, buf *reasmBuf) bool {
	sortPieces(buf.pieces)
	want := buf.coap1
	for _, p := range buf.pieces {
		if p.off-ipv6UDPHeaderLen != want {
			return false
		}
		want += len(p.data)
	}
	return want == k.size-ipv6UDPHeaderLen
}

func sortPieces(pieces []reasmPiece) {
	for i := 1; i < len(pieces); i++ {
		for j := i; j > 0 && pieces[j-1].off > pieces[j].off; j-- {
			pieces[j-1], pieces[j] = pieces[j], pieces[j-1]
		}
	}
}

// assemble parses the whole datagram, or returns nil when it holds no CoAP
// message.
func assemble(buf *reasmBuf) *CoAPMessage {
	core := append([]byte{}, buf.frag1...)
	for _, p := range buf.pieces {
		core = append(core, p.data...)
	}
	m, err := parseCoAPCore(core)
	if err != nil {
		return nil
	}
	return m
}

// coapOffset returns where the CoAP message starts in a datagram: the length
// of its compressed IPv6 and UDP headers.
func coapOffset(core []byte) (int, error) {
	i, err := skipIPHC(core)
	if err != nil {
		return 0, err
	}
	_, i, err = parseUDPNHC(core, i)
	return i, err
}

func hexEUI(b []byte) string {
	const hexd = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexd[c>>4]
		out[i*2+1] = hexd[c&0xf]
	}
	return string(out)
}
