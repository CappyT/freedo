package tado

import (
	"bytes"
	"sort"
	"testing"
)

// TestCoAPGolden: every captured Data frame carries a decodable CoAP message on
// top of 6LoWPAN/UDP. Proves the app-layer transport is CoAP (RFC 7252) and that
// the header skip reaches a valid message for all address-compression modes.
func TestCoAPGolden(t *testing.T) {
	frames := loadGolden(t)
	total, withCoAP := 0, 0
	res := map[string]int{}
	for _, fr := range frames {
		p, err := ParseData(NetworkKey, fr)
		if err != nil {
			t.Fatal(err)
		}
		app := ParseApp(p.Plaintext)
		if app == nil {
			continue
		}
		total++
		if app.CoAP == nil {
			t.Errorf("frame with no CoAP: %s", p.Hex())
			continue
		}
		withCoAP++
		c := app.CoAP
		if c.DstPort != 5683 && c.SrcPort != 5683 {
			t.Errorf("neither port is CoAP 5683: %d->%d", c.SrcPort, c.DstPort)
		}
		key := c.Path
		if q := c.Query; q != "" {
			if i := indexByte(q, '='); i >= 0 {
				key += "?" + q[:i]
			}
		}
		res[key]++
	}
	if withCoAP != total {
		t.Fatalf("only %d/%d app payloads decoded as CoAP", withCoAP, total)
	}
	// The core resources must be present.
	for _, want := range []string{"z/s?id", "z/s?lid", "z/p?lid"} {
		if res[want] == 0 {
			t.Errorf("expected resource %q not seen", want)
		}
	}
	keys := make([]string, 0, len(res))
	for k := range res {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return res[keys[i]] > res[keys[j]] })
	t.Logf("decoded CoAP on %d/%d payloads; %d distinct resources", withCoAP, total, len(res))
	for _, k := range keys {
		t.Logf("  %-28s x%d", k, res[k])
	}
}

// A captured lid= frame per direction, on air, FCS excluded. Both are zone
// traffic, so both addresses are compressed against 6LoWPAN context 0 and the
// UDP checksum can only be checked once Context0Prefix is put back.
const (
	// thermostat -> bridge, CON 0.03 z/s?lid=1: source and destination IIDs both
	// elided, taken from the L2 header (IPHC 7ef7).
	lidUplinkHex = "69ec3509a1cefa07c51b0005a2cafa075fa8106b2b74b1ff891dfcaa8e1b074fd0737e" +
		"47851a3daaa7e9825b50653c14e05c06acd90412566ea124b8871c3318962e389d8c35200a9c" +
		"ca5276fadc2a98c747f129ff5c87f44a50e9d5aa1d454b8b52c0bbd9"
	// bridge -> thermostat, CON 0.03 z/s?lid=5, relayed for another zone: the source IID
	// travels inline because it is not the L2 sender's (IPHC 7cd7).
	lidDownlinkHex = "79ec9c05a2cafa07c51b0009a1cefa0776e0e13fb6220b3a347c1052a72407c072d81bdf" +
		"452670c0fcab833e4afa672a75ffddb8368e21e972f58072882647d1600fc209a3f5f12f185072577" +
		"6fce76433c8ccacab3247ae27ce33ce9ec08f2e2a84396065abfa04e9a11070c875"
)

// TestContext0PrefixVerifiesCapturedChecksums is the round trip that pins
// Context0Prefix: rebuild the full 128-bit source and destination of a captured
// zone frame from the prefix plus each node's EUI-64, recompute the UDP checksum
// over the IPv6 pseudo-header, and compare it to the one the frame carries. A
// wrong prefix cannot reproduce it.
func TestContext0PrefixVerifiesCapturedChecksums(t *testing.T) {
	for _, tc := range []struct {
		name, hex, uri, src, dst string
	}{
		{
			"uplink", lidUplinkHex, "z/s?lid=1",
			"aaaa000000000000021bc507facaa205", "aaaa000000000000021bc507facea109",
		},
		{
			"downlink", lidDownlinkHex, "z/s?lid=5",
			"aaaa000000000000021bc507face0a01", "aaaa000000000000021bc507facaa205",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := ParseData(NetworkKey, mustB(tc.hex))
			if err != nil {
				t.Fatal(err)
			}
			m, err := ParseCoAP(f.Plaintext[8:])
			if err != nil {
				t.Fatal(err)
			}
			if m.URI() != tc.uri {
				t.Fatalf("uri=%q, want %q", m.URI(), tc.uri)
			}
			if !m.Contexted() {
				t.Fatal("frame is not compressed against a 6LoWPAN context")
			}
			src, dst := m.Src.IPv6(f.SrcEUI()), m.Dst.IPv6(f.DstEUI)
			if got := hexEUI(src); got != tc.src {
				t.Errorf("src=%s, want %s", got, tc.src)
			}
			if got := hexEUI(dst); got != tc.dst {
				t.Errorf("dst=%s, want %s", got, tc.dst)
			}
			if m.Checksum == 0 {
				t.Fatal("the frame carries no inline UDP checksum")
			}
			got := udpChecksumV6(src, dst, m.SrcPort, m.DstPort, m.Raw)
			if got != m.Checksum {
				t.Errorf("recomputed checksum %04x, frame carries %04x", got, m.Checksum)
			}
		})
	}
}

// TestChecksumVerifiesEveryGoldenFrame: the same reconstruction holds for the
// whole capture, link-local and context-compressed frames alike. One prefix has
// to satisfy them all at once, which is what makes the value unique in practice.
func TestChecksumVerifiesEveryGoldenFrame(t *testing.T) {
	var checked, ctx int
	for _, raw := range loadGolden(t) {
		f, err := ParseData(NetworkKey, raw)
		if err != nil {
			t.Fatal(err)
		}
		if IsFragment(f.Plaintext) {
			continue // the checksum covers the whole datagram, not this fragment
		}
		m, err := ParseCoAP(f.Plaintext[8:])
		if err != nil || m.Checksum == 0 {
			continue
		}
		got, ok := m.RecomputeChecksum(f.SrcEUI(), f.DstEUI)
		if !ok {
			t.Errorf("cannot rebuild the addresses of %s", f.Hex())
			continue
		}
		checked++
		if m.Contexted() {
			ctx++
		}
		if got != m.Checksum {
			t.Errorf("checksum %04x, want %04x, on %s", got, m.Checksum, f.Hex())
		}
	}
	if ctx == 0 {
		t.Fatal("no context-compressed frame in the capture; the prefix went untested")
	}
	t.Logf("verified %d checksums, %d of them context-compressed", checked, ctx)
}

// TestBuildContextAckMatchesCapture: the answer the builder emits for a zone
// uplink is byte-identical to the one the real bridge sent, checksum included.
func TestBuildContextAckMatchesCapture(t *testing.T) {
	// bridge -> bedroom, the ACK 2.04 that closes a zone transaction, taken from
	// the capture with its Tado shim and app trailer stripped.
	const wantPkt = "7ef700f0163316336a9660442722c12a"

	bedroom := mustB(NodeByAny("bedroom").WireLE)
	got, err := BuildContextAck("2.04", 0x2722, nil, BridgeEUI, bedroom, 5683, 5683)
	if err != nil {
		t.Fatal(err)
	}
	if hexEUI(got) != wantPkt {
		t.Fatalf("built  %s\nwant   %s", hexEUI(got), wantPkt)
	}
	m, err := parseCoAPCore(got)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Contexted() || m.Type != "ACK" || m.Code != "2.04" || m.MID != 0x2722 {
		t.Errorf("parsed back as %s %s mid=%04x ctx=%v", m.Type, m.Code, m.MID, m.Contexted())
	}
	if ck, ok := m.RecomputeChecksum(BridgeEUI, bedroom); !ok || ck != m.Checksum {
		t.Errorf("built checksum %04x does not verify (recomputed %04x, ok=%v)", m.Checksum, ck, ok)
	}
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// TestCoAPZeroLengthObserve: the observe registration of the wall thermostat
// carries option 6 with no value, which is Observe 0. The parser keeps the
// option, and the serializer emits it empty and with a 24-bit value.
func TestCoAPZeroLengthObserve(t *testing.T) {
	raw := []byte{0x42, 0x01, 0x12, 0x34, 0xa1, 0xb2, 0x60, 0x51, 'z', 0x01, 'p'}
	m, err := parseCoAPMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	obs, ok := m.Option(6)
	if !ok || len(obs.Val) != 0 || obs.Uint() != 0 {
		t.Fatalf("Observe = %x present=%t, want present and empty", obs.Val, ok)
	}
	if m.Code != "0.01" || m.URI() != "z/p" || !bytes.Equal(m.Token, []byte{0xa1, 0xb2}) {
		t.Errorf("GET = %s %s tok=%x, want 0.01 z/p tok=a1b2", m.Code, m.URI(), m.Token)
	}
	for _, seq := range []uint32{0, 1, 0x1234, 0xffffff} {
		pkt, err := BuildContextCoAP(CoAPReq{
			Type: "NON", Code: "2.05", Token: []byte{0xa1}, Options: []CoAPOption{observeOption(seq)},
			SrcEUI: BridgeEUI, DstEUI: BridgeEUI,
		})
		if err != nil {
			t.Fatal(err)
		}
		back, err := parseCoAPCore(pkt)
		if err != nil {
			t.Fatal(err)
		}
		if o, ok := back.Option(6); !ok || o.Uint() != uint64(seq) || len(o.Val) != len(encodeCoAPUint(seq)) {
			t.Errorf("Observe %d serialized as %x present=%t", seq, o.Val, ok)
		}
	}
}
