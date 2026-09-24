package tado

import (
	"bytes"
	"crypto/aes"
	"encoding/hex"
	"testing"
)

// TestBuildWakeUpVectors rebuilds real captured wake-up frames byte for byte.
func TestBuildWakeUpVectors(t *testing.T) {
	cases := []struct {
		seq    byte
		dst    []byte
		rendez uint16
		want   string
	}{
		{0x0a, []byte{0x0a, 0x06}, 1191, "250acdab0a06820ea704803f"},
		{0x8c, []byte{0xa2, 0x05}, 248, "258ccdaba205820ef800803f"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(BuildWakeUp(c.seq, c.dst, c.rendez))
		if got != c.want {
			t.Errorf("BuildWakeUp(%#x) = %s, want %s", c.seq, got, c.want)
		}
	}
}

func TestCapturedUnicastWakeUpCountdown(t *testing.T) {
	values := [...]uint16{248, 235, 223, 210, 198, 186, 173, 161, 148, 136, 124, 111, 99, 86, 74, 62, 49, 37, 24, 12, 0}
	for i, rendez := range values {
		got := BuildWakeUp(0x56, []byte{0x0a, 0x01}, rendez)
		want := []byte{0x25, 0x56, 0xcd, 0xab, 0x0a, 0x01, 0x82, 0x0e, byte(rendez), byte(rendez >> 8), 0x80, 0x3f}
		if !bytes.Equal(got, want) {
			t.Fatalf("wake %d=%x, want=%x", i, got, want)
		}
	}
}

func TestBuildRouterAdvertWakeUpBurstNominal(t *testing.T) {
	burst := BuildRouterAdvertWakeUpBurst(0x42)
	if len(burst) != RouterAdvertWakeUpCount {
		t.Fatalf("burst length=%d, want %d", len(burst), RouterAdvertWakeUpCount)
	}
	want := [...]uint16{
		3125, 3112, 3100, 3087, 3075, 3063, 3050, 3038, 3025, 3013, 3001, 2988, 2976, 2963, 2951, 2939,
		2926, 2914, 2901, 2889, 2877, 2864, 2852, 2839, 2827, 2815, 2802, 2790, 2777, 2765, 2753, 2740,
		2728, 2715, 2703, 2691, 2678, 2666, 2653, 2641, 2629, 2616, 2604, 2591, 2579, 2567, 2554, 2542,
		2529, 2517, 2505, 2492, 2480, 2467, 2455, 2443, 2430, 2418, 2405, 2393, 2381, 2368, 2356, 2344,
		2331, 2319, 2306, 2294, 2282, 2269, 2257, 2244, 2232, 2220, 2207, 2195, 2182, 2170, 2158, 2145,
		2133, 2120, 2108, 2096, 2083, 2071, 2058, 2046, 2034, 2021, 2009, 1996, 1984, 1972, 1959, 1947,
		1934, 1922, 1910, 1897, 1885, 1872, 1860, 1848, 1835, 1823, 1810, 1798, 1786, 1773, 1761, 1748,
		1736, 1724, 1711, 1699, 1686, 1674, 1662, 1649, 1637, 1624, 1612, 1600, 1587, 1575, 1563, 1550,
		1538, 1525, 1513, 1501, 1488, 1476, 1463, 1451, 1439, 1426, 1414, 1401, 1389, 1377, 1364, 1352,
		1339, 1327, 1315, 1302, 1290, 1277, 1265, 1253, 1240, 1228, 1215, 1203, 1191, 1178, 1166, 1153,
		1141, 1129, 1116, 1104, 1091, 1079, 1067, 1054, 1042, 1029, 1017, 1005, 992, 980, 967, 955,
		943, 930, 918, 905, 893, 881, 868, 856, 843, 831, 819, 806, 794, 782, 769, 757,
		744, 732, 720, 707, 695, 682, 670, 658, 645, 633, 620, 608, 596, 583, 571, 558,
		546, 534, 521, 509, 496, 484, 472, 459, 447, 434, 422, 410, 397, 385, 372, 360,
		348, 335, 323, 310, 298, 286, 273, 261, 248, 236, 224, 211, 199, 186, 174, 162,
		149, 137, 124, 112, 100, 87, 75, 62, 50, 38, 25, 13, 1, 0,
	}
	if len(want) != RouterAdvertWakeUpCount {
		t.Fatalf("golden length=%d, want %d", len(want), RouterAdvertWakeUpCount)
	}
	for i, rendez := range want {
		expected := BuildWakeUp(0x42, []byte{0xff, 0xff}, rendez)
		if got := burst[i]; !bytes.Equal(got, expected) {
			t.Errorf("frame %d=%x, want %x", i, got, expected)
		}
		w, err := ParseWakeUp(burst[i])
		if err != nil || w.Seq != 0x42 || w.DstShort != "ffff" || w.RendezvousTime != rendez {
			t.Errorf("frame %d=%+v err=%v", i, w, err)
		}
	}
}

// realEnhAcks are captured Enhanced ACKs, used both as build vectors and as FCS
// vectors.
var realEnhAcks = []struct {
	seq   byte
	dst   string
	src   string
	phase uint16
	want  string
}{
	{0x00, "010acefa07c51b00", "09a1cefa07c51b00", 0x34b7,
		"42ee00010acefa07c51b0009a1cefa07c51b00040db734350c803f5af5"},
	{0x4b, "05a2cafa07c51b00", "09a1cefa07c51b00", 0x57d6,
		"42ee4b05a2cafa07c51b0009a1cefa07c51b00040dd657350c803fe72d"},
	{0x00, "02a6cefa07c51b00", "09a1cefa07c51b00", 0x9edc,
		"42ee0002a6cefa07c51b0009a1cefa07c51b00040ddc9e350c803fe32d"},
	{0x01, "010acefa07c51b00", "09a1cefa07c51b00", 0x89f6,
		"42ee01010acefa07c51b0009a1cefa07c51b00040df689350c803f2bd5"},
}

// TestBuildEnhAckVectors rebuilds real captured Enhanced ACKs byte for byte,
// FCS included.
func TestBuildEnhAckVectors(t *testing.T) {
	for _, c := range realEnhAcks {
		got := hex.EncodeToString(BuildEnhAck(c.seq, mustB(c.dst), mustB(c.src), c.phase))
		if got != c.want {
			t.Errorf("BuildEnhAck(%#x) = %s, want %s", c.seq, got, c.want)
		}
	}
}

// TestMACFCS checks the FCS routine against the captured Enhanced ACK trailers.
func TestMACFCS(t *testing.T) {
	for _, c := range realEnhAcks {
		frame := mustB(c.want)
		got := hex.EncodeToString(macFCS(frame[0:27]))
		if want := hex.EncodeToString(frame[27:29]); got != want {
			t.Errorf("macFCS(%s...) = %s, want %s", c.want[:6], got, want)
		}
	}
}

// TestBuildWakeUpRoundTrip checks ParseWakeUp recovers every built field.
func TestBuildWakeUpRoundTrip(t *testing.T) {
	cases := []struct {
		seq    byte
		dst    []byte
		rendez uint16
	}{
		{0x00, []byte{0x0a, 0x06}, 0},
		{0x0a, []byte{0xa2, 0x05}, 1191},
		{0x7f, []byte{0xa6, 0x02}, 0x0c35},
		{0xff, []byte{0xff, 0xff}, 0xffff},
	}
	for _, c := range cases {
		frame := BuildWakeUp(c.seq, c.dst, c.rendez)
		if len(frame) != 12 {
			t.Fatalf("frame len=%d want 12", len(frame))
		}
		if Classify(frame) != ClassWakeUp {
			t.Errorf("Classify=%v want ClassWakeUp", Classify(frame))
		}
		w, err := ParseWakeUp(frame)
		if err != nil {
			t.Fatal(err)
		}
		if w.Seq != c.seq {
			t.Errorf("Seq=%#x want %#x", w.Seq, c.seq)
		}
		if w.PANID != PANID {
			t.Errorf("PANID=%#x want %#x", w.PANID, PANID)
		}
		if w.DstShort != hex.EncodeToString(c.dst) {
			t.Errorf("DstShort=%s want %s", w.DstShort, hex.EncodeToString(c.dst))
		}
		if w.RendezvousTime != c.rendez {
			t.Errorf("RendezvousTime=%d want %d", w.RendezvousTime, c.rendez)
		}
	}
}

// TestBuildEnhAckRoundTrip checks ParseEnhAck recovers every built field.
func TestBuildEnhAckRoundTrip(t *testing.T) {
	cases := []struct {
		seq   byte
		dst   string
		src   string
		phase uint16
	}{
		{0x00, "010acefa07c51b00", "09a1cefa07c51b00", 0x34b7},
		{0x4b, "05a2cafa07c51b00", "09a1cefa07c51b00", 0x57d6},
		{0xc3, "08a4defa07c51b00", "09a1cefa07c51b00", 0xffff},
	}
	for _, c := range cases {
		frame := BuildEnhAck(c.seq, mustB(c.dst), mustB(c.src), c.phase)
		if len(frame) != 29 {
			t.Fatalf("frame len=%d want 29", len(frame))
		}
		if Classify(frame) != ClassEnhAck {
			t.Errorf("Classify=%v want ClassEnhAck", Classify(frame))
		}
		a, err := ParseEnhAck(frame)
		if err != nil {
			t.Fatal(err)
		}
		if a.Seq != c.seq {
			t.Errorf("Seq=%#x want %#x", a.Seq, c.seq)
		}
		if hex.EncodeToString(a.DstEUI) != c.dst {
			t.Errorf("DstEUI=%s want %s", hex.EncodeToString(a.DstEUI), c.dst)
		}
		if hex.EncodeToString(a.SrcEUI) != c.src {
			t.Errorf("SrcEUI=%s want %s", hex.EncodeToString(a.SrcEUI), c.src)
		}
		if a.CSLPhase != c.phase {
			t.Errorf("CSLPhase=%#x want %#x", a.CSLPhase, c.phase)
		}
		if a.CSLPeriod != CSLPeriodUnits {
			t.Errorf("CSLPeriod=%#x want %#x", a.CSLPeriod, CSLPeriodUnits)
		}
		if want := hex.EncodeToString(macFCS(frame[0:27])); hex.EncodeToString(a.Trailer) != want {
			t.Errorf("Trailer=%s want %s", hex.EncodeToString(a.Trailer), want)
		}
	}
}

// TestBuildWakeUpTrain checks the burst keeps seq/dst constant and clamps the
// Rendezvous Time at 0 instead of wrapping the uint16.
func TestBuildWakeUpTrain(t *testing.T) {
	cases := []struct {
		count int
		start uint16
		step  uint16
		want  []uint16
	}{
		{4, 1200, 300, []uint16{1200, 900, 600, 300}},
		{6, 1000, 300, []uint16{1000, 700, 400, 100, 0, 0}}, // step*count passes 0
		{3, 5, 100, []uint16{5, 0, 0}},
	}
	dst := []byte{0x0a, 0x06}
	const seq = 0x0a
	for _, c := range cases {
		train := BuildWakeUpTrain(seq, dst, c.count, c.start, c.step)
		if len(train) != c.count {
			t.Fatalf("train len=%d want %d", len(train), c.count)
		}
		for i, frame := range train {
			w, err := ParseWakeUp(frame)
			if err != nil {
				t.Fatal(err)
			}
			if w.Seq != seq {
				t.Errorf("frame %d Seq=%#x want %#x", i, w.Seq, seq)
			}
			if w.DstShort != "0a06" {
				t.Errorf("frame %d DstShort=%s want 0a06", i, w.DstShort)
			}
			if w.RendezvousTime != c.want[i] {
				t.Errorf("frame %d RendezvousTime=%d want %d", i, w.RendezvousTime, c.want[i])
			}
		}
	}
	if BuildWakeUpTrain(seq, dst, 0, 100, 10) != nil {
		t.Error("count=0 should return nil")
	}
}

// TestBuildBroadcastRoundTrip decrypts a built broadcast with the same slices
// the receiver uses: nonce = frame[0:13], aad = frame[0:16].
func TestBuildBroadcastRoundTrip(t *testing.T) {
	plaintext := mustB("c51b0000112233445566778899aabbcc")
	frame, err := BuildBroadcast(NetworkKey, 0x2d, BridgeEUI, 0x1234, 0x0d, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if frame[0] != 0x49 || frame[1] != 0xe8 {
		t.Fatalf("FCF=%02x%02x want 49e8", frame[0], frame[1])
	}
	if Classify(frame) != ClassBroadcast {
		t.Errorf("Classify=%v want ClassBroadcast", Classify(frame))
	}
	if len(frame) != headerLen+len(plaintext)+micLen {
		t.Fatalf("frame len=%d want %d", len(frame), headerLen+len(plaintext)+micLen)
	}
	block, err := aes.NewCipher(NetworkKey)
	if err != nil {
		t.Fatal(err)
	}
	pt, ok := ccmDecrypt(block, frame[0:nonceLen], frame[0:headerLen],
		frame[headerLen:len(frame)-micLen], frame[len(frame)-micLen:])
	if !ok {
		t.Fatal("MIC verification failed")
	}
	want, err := setMACFCS(frame[:headerLen], plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(pt) != hex.EncodeToString(want) {
		t.Errorf("plaintext=%s want %s", hex.EncodeToString(pt), hex.EncodeToString(want))
	}
	if _, err := BuildBroadcast(NetworkKey, 0x00, mustB("0011"), 0, 0x0d, plaintext); err == nil {
		t.Error("short srcEUI should error")
	}
}
