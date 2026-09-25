package tado

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// Full MAC-layer dissection for every frame class on the Tado 868 MHz link.
// The link is an IEEE 802.15.4-2015 profile using the Low-Energy Coordinated
// Sampled Listening (CSL) MAC: the coordinator streams wake-up (Multipurpose)
// frames that count a Rendezvous Time down to zero, then exchanges a Data frame
// and an Enhanced ACK at the rendezvous. Frame/IE structure cross-checked against
// the sdegeorgio teardown wiki and confirmed on our decrypted captures; the
// security and app layers come from the extracted key + firmware (docs).
//
// Frames handled here are stored WITHOUT the on-air FCS (CRC-16/CMS 0x8005,
// which is the CC1101 hardware CRC and is stripped on receive).

// Network constants (confirmed on-air).
const (
	PANID          = 0xABCD // destination PAN ID in wake-up frames
	CSLPeriodUnits = 0x0C35 // channel-sample period: 3125 units of 320 us, exactly 1.000 s

	ieRendezvousTime = "820e" // Header IE: Rendezvous Time (element 0x1D, len 2)
	ieCSL            = "040d" // Header IE: CSL (element 0x1A, len 4)
	ieHeaderTerm     = "803f" // Header IE: Header Termination (element 0x7F, len 0)
)

// FrameClass identifies the MAC frame type by its Frame Control octets.
type FrameClass int

const (
	ClassUnknown       FrameClass = iota
	ClassData                     // 0x69ec unicast Data (secured, ack-req)
	ClassDataPending              // 0x79ec Data with Frame Pending set
	ClassBroadcast                // 0x49e8 broadcast Data (aux security header on wire), 0x59e8 with Frame Pending
	ClassEnhAck                   // 0x42ee Enhanced Acknowledgment (unsecured)
	ClassWakeUp                   // 0x25.. Multipurpose wake-up (CSL rendezvous)
	ClassBeaconRequest            // 0x0be8 MAC command, secured: Enhanced Beacon Request
	ClassBeacon                   // 0x18ec Enhanced Beacon, secured
)

func (c FrameClass) String() string {
	switch c {
	case ClassData:
		return "Data"
	case ClassDataPending:
		return "Data(pending)"
	case ClassBroadcast:
		return "Broadcast"
	case ClassEnhAck:
		return "EnhACK"
	case ClassWakeUp:
		return "WakeUp"
	case ClassBeaconRequest:
		return "BeaconReq"
	case ClassBeacon:
		return "EnhBeacon"
	}
	return "Unknown"
}

// Classify returns the MAC frame class from the leading Frame Control octets.
func Classify(frame []byte) FrameClass {
	if len(frame) < 2 {
		return ClassUnknown
	}
	switch {
	case frame[0] == 0x69 && frame[1] == 0xec:
		return ClassData
	case frame[0] == 0x79 && frame[1] == 0xec:
		return ClassDataPending
	case (frame[0] == 0x49 || frame[0] == 0x59) && frame[1] == 0xe8:
		return ClassBroadcast
	case frame[0] == fcfBeaconRequest[0] && frame[1] == fcfBeaconRequest[1]:
		return ClassBeaconRequest
	case frame[0] == fcfEnhBeacon[0] && frame[1] == fcfEnhBeacon[1]:
		return ClassBeacon
	case frame[0] == 0x42 && frame[1] == 0xee:
		return ClassEnhAck
	case frame[0] == 0x25:
		return ClassWakeUp
	}
	return ClassUnknown
}

// WakeUp is a decoded Multipurpose wake-up frame (CSL rendezvous beacon).
type WakeUp struct {
	Seq            byte
	PANID          uint16
	DstShort       string // target device 16-bit short address (hex, inventory form)
	RendezvousTime uint16 // CSL units remaining until the frame exchange (counts to 0)
}

// ParseWakeUp decodes a 12-byte wake-up frame:
//
//	MPFC(1) Seq(1) PANID(2 LE) DstAddr(2) RendezIE(2) RendezTime(2 LE) TermIE(2)
func ParseWakeUp(frame []byte) (*WakeUp, error) {
	if len(frame) < 12 || frame[0] != 0x25 {
		return nil, fmt.Errorf("tado: not a 12-byte wake-up frame")
	}
	return &WakeUp{
		Seq:            frame[1],
		PANID:          binary.LittleEndian.Uint16(frame[2:4]),
		DstShort:       hex.EncodeToString(frame[4:6]),
		RendezvousTime: binary.LittleEndian.Uint16(frame[8:10]),
	}, nil
}

// EnhAck is a decoded Enhanced Acknowledgment.
//
// CSL Phase runs opposite to IEEE 802.15.4-2015: both firmware builders emit
// (now - anchor)/10 and the receiver subtracts it, so it is time elapsed since
// the sender's last sample, never time to its next one. The capture agrees -
// phases of 20375..56721 units cannot fit inside one 3125-unit period.
type EnhAck struct {
	Seq       byte   // equals the Seq of the Data frame being acked
	DstEUI    []byte // 8-byte wire EUI
	SrcEUI    []byte // 8-byte wire EUI
	CSLPhase  uint16 // 320 us units elapsed since the sender's last CSL sample
	CSLPeriod uint16 // fixed 0x0C35
	Trailer   []byte // the 2-byte software MAC FCS over the 27 bytes before it (macFCS)
}

// ParseEnhAck decodes a 29-byte Enhanced ACK and verifies its MAC FCS:
//
//	FCF(2) Seq(1) Dst(8) Src(8) CSL-IE(2) Phase(2 LE) Period(2 LE) TermIE(2) FCS(2)
//
// The CC1101 strips only the PHY CRC, so the software FCS is the one check left
// on a frame whose sequence number closes a downlink.
func ParseEnhAck(frame []byte) (*EnhAck, error) {
	if len(frame) < 29 || frame[0] != 0x42 || frame[1] != 0xee {
		return nil, fmt.Errorf("tado: not a 29-byte Enhanced ACK")
	}
	if !bytes.Equal(frame[27:29], macFCS(frame[:27])) {
		return nil, ErrMACFCS
	}
	return &EnhAck{
		Seq:       frame[2],
		DstEUI:    append([]byte{}, frame[3:11]...),
		SrcEUI:    append([]byte{}, frame[11:19]...),
		CSLPhase:  binary.LittleEndian.Uint16(frame[21:23]),
		CSLPeriod: binary.LittleEndian.Uint16(frame[23:25]),
		Trailer:   append([]byte{}, frame[27:29]...),
	}, nil
}

// Dissect returns a one-line human description of any frame class, decrypting
// Data frames with key when possible, and naming the nodes of this
// installation. Used by the rx sniffer and the CLI.
//
// It holds no state, so a 6LoWPAN fragment reads as its fragmentation header
// alone. Use a Dissector to join the fragments and read the whole message.
func (in *Install) Dissect(frame []byte) string {
	return dissect(in, in.NetworkKey(), nil, frame)
}

// Dissect is Install.Dissect without a name directory: every node reads as its
// short address.
func Dissect(key, frame []byte) string { return dissect(nil, key, nil, frame) }

// Dissector renders frames the way Dissect does and, unlike it, joins 6LoWPAN
// fragments: it holds each datagram until the last fragment lands and then
// prints the whole CoAP message on that frame's line. A datagram that never
// completes expires after the RFC 4944 timeout and blocks nothing.
//
// It carries the reassembly table, so it is not safe for concurrent use. Drive
// it from one goroutine, the same contract as Reassembler.
type Dissector struct {
	in    *Install
	key   []byte
	reasm *Reassembler
}

// NewDissector returns a stateful dissector without a name directory.
func NewDissector(key []byte) *Dissector {
	return &Dissector{key: key, reasm: NewReassembler()}
}

// NewDissector returns a stateful dissector that names the nodes of this
// installation and decrypts with its key.
func (in *Install) NewDissector() *Dissector {
	return &Dissector{in: in, key: in.NetworkKey(), reasm: NewReassembler()}
}

// Dissect returns the one-line description of one frame.
func (d *Dissector) Dissect(frame []byte) string {
	return dissect(d.in, d.key, d.reasm, frame)
}

func dissect(in *Install, key []byte, ra *Reassembler, frame []byte) string {
	switch Classify(frame) {
	case ClassWakeUp:
		w, err := ParseWakeUp(frame)
		if err != nil {
			return "WakeUp (malformed)"
		}
		who := w.DstShort
		if in != nil {
			if d, err := in.Resolve(w.DstShort); err == nil {
				who = deviceLabel(d)
			}
		} else if node := NodeByAny(w.DstShort); node != nil {
			who = labelOf(node)
		}
		return fmt.Sprintf("WakeUp    seq=%02x -> %-14s rendezvous=%d", w.Seq, who, w.RendezvousTime)
	case ClassEnhAck:
		a, err := ParseEnhAck(frame)
		if err != nil {
			return "EnhACK (malformed)"
		}
		return fmt.Sprintf("EnhACK    seq=%02x %s -> %s  cslPhase=%d trailer=%s",
			a.Seq, nameOf(in, a.SrcEUI), nameOf(in, a.DstEUI), a.CSLPhase, hex.EncodeToString(a.Trailer))
	case ClassData, ClassDataPending:
		f, err := ParseData(key, frame)
		if err != nil {
			// Classify takes the 2-byte FCF alone, so the sequence byte may be absent.
			if len(frame) < 3 {
				return fmt.Sprintf("%-9s (malformed)", Classify(frame))
			}
			return fmt.Sprintf("%-9s seq=%02x (MIC fail / wrong key)", Classify(frame), frame[2])
		}
		s := fmt.Sprintf("%-9s seq=%02x %s -> %s", Classify(frame), f.Seq, nameOf(in, f.SrcEUI()), nameOf(in, f.DstEUI))
		if IsFragment(f.Plaintext) {
			return s + dissectFragment(ra, f)
		}
		if app := ParseApp(f.Plaintext); app != nil {
			s += fmt.Sprintf("  ctr=%d", app.Counter)
			if app.CoAP != nil {
				s += "  CoAP " + app.CoAP.Summary()
				return s + dissectPayload(app.CoAP)
			}
			res := ResourceOf("", app.TLVs)
			for _, t := range app.TLVs {
				s += "  " + t.TagLabelIn(res)
			}
		}
		return s
	case ClassBroadcast:
		// FCF(2) Seq(1) DstShort(2=ffff) Src(8) SecCtrl(1) FrameCounter(2 LE) ct...
		if len(frame) < 16 {
			return "Broadcast (malformed)"
		}
		pending := ""
		if frame[0] == 0x59 {
			pending = " pending"
		}
		return fmt.Sprintf("Broadcast seq=%02x src=%s secCtrl=%02x frameCtr=%d (aux-security)%s",
			frame[2], nameOf(in, frame[5:13]), frame[13], binary.LittleEndian.Uint16(frame[14:16]), pending)
	case ClassBeaconRequest:
		if len(frame) != beaconRequestLen {
			return "BeaconReq (malformed)"
		}
		return fmt.Sprintf("BeaconReq seq=%02x src=%s (pairing key)", frame[2], nameOf(in, frame[7:15]))
	case ClassBeacon:
		if len(frame) != enhBeaconLen {
			return "EnhBeacon (malformed)"
		}
		return fmt.Sprintf("EnhBeacon seq=%02x -> %s (pairing key)", frame[2], nameOf(in, frame[5:13]))
	}
	return "Unknown   " + hex.EncodeToString(frame[:min2(4, len(frame))])
}

// dissectFragment renders a 6LoWPAN fragment. With no reassembler it prints the
// fragmentation header alone. With one it also prints how much of the datagram
// is held, and the whole CoAP message on the frame that completes it.
func dissectFragment(ra *Reassembler, f *Frame) string {
	ctr := binary.LittleEndian.Uint32(f.Plaintext[4:8])
	fr, ok := ParseFragment(f.Plaintext)
	if !ok {
		return fmt.Sprintf("  ctr=%d  FRAG (malformed)", ctr)
	}
	s := fmt.Sprintf("  ctr=%d  %s", ctr, fr)
	if ra == nil {
		return s
	}
	rs, ok := ra.AddFrame(f)
	if !ok {
		return s + "  dropped"
	}
	if rs.Message == nil {
		return s + fmt.Sprintf("  held=%d/%d frames=%d", rs.Held, fr.Size, rs.Frames)
	}
	return s + fmt.Sprintf("  REASSEMBLED frames=%d bytes=%d  CoAP %s", rs.Frames, fr.Size, rs.Message.Summary()) +
		dissectPayload(rs.Message)
}

// dissectPayload renders the CoAP payload as TLV labels. A block-wise transfer
// past block 0 carries a slice of the document that cuts a TLV in half, so that
// one prints as raw bytes instead.
func dissectPayload(m *CoAPMessage) string {
	if len(m.Payload) == 0 {
		return ""
	}
	if !m.PayloadStartsDocument() {
		return "  payload=" + hex.EncodeToString(m.Payload)
	}
	ts := parseTLVList(m.Payload)
	res := ResourceOf(m.URI(), ts)
	s := ""
	for _, t := range ts {
		s += "  " + t.TagLabelIn(res)
	}
	return s
}

// nameOf names a node for a log line: its installation label when the address
// is known, else the short address. A nil install falls back to the legacy
// Inventory, which is empty outside the tests.
func nameOf(in *Install, wireEUI []byte) string {
	if len(wireEUI) < 8 {
		return hex.EncodeToString(wireEUI)
	}
	h := hex.EncodeToString(wireEUI)
	if in != nil {
		if bytes.Equal(wireEUI, in.CoordinatorEUI()) {
			return "bridge"
		}
		if d, err := in.Resolve(h); err == nil {
			return deviceLabel(d)
		}
	}
	if n := NodeByAny(h); n != nil {
		return labelOf(n)
	}
	return ShortFromWire(wireEUI)
}

func deviceLabel(d *Device) string {
	switch {
	case d.Label != "":
		return d.Label
	case d.Serial != "":
		return d.Serial
	}
	return d.Short()
}

func labelOf(n *Node) string {
	if n.Room != "" {
		return n.Room
	}
	return n.Serial
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}
