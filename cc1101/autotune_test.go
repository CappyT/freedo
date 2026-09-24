package cc1101

import (
	"bytes"
	"testing"
	"time"
)

// repeatInt returns n readings of the same value.
func repeatInt(n, value int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = value
	}
	return out
}

func TestTuneCorrection(t *testing.T) {
	tests := []struct {
		name        string
		samples     map[string][]int
		wantCorr    int
		wantUsable  int
		wantClipped bool
	}{
		{
			name:       "saturation dropped",
			samples:    map[string][]int{"a": {21, -21, 20, -20, 3, 3, 3}},
			wantCorr:   3,
			wantUsable: 3,
		},
		{
			name: "per source weighting",
			samples: map[string][]int{
				"a": {-12, -12, -12, -12, -12, -12, -12},
				"b": {-4},
				"c": {-3},
			},
			wantCorr:   -4,
			wantUsable: 9,
		},
		{
			// The wake-up train reads the offset, the coordinator Data frames
			// scatter after it. Two sources must not select the higher one.
			name: "two sources average",
			samples: map[string][]int{
				"coordinator": {0, 0, 0, 0, 0},
				"abcde":       {5, 5, 5, 5, 5},
			},
			wantCorr:   2,
			wantUsable: 10,
		},
		{
			name:       "even count per source",
			samples:    map[string][]int{"a": {-12, -10, -8, -6}},
			wantCorr:   -9,
			wantUsable: 4,
		},
		{
			name:       "single source",
			samples:    map[string][]int{"a": {-11, -9, -10, -10, -10}},
			wantCorr:   -10,
			wantUsable: 5,
		},
		{
			name:       "too few samples",
			samples:    map[string][]int{"a": {-8, -9}},
			wantCorr:   -8,
			wantUsable: 2,
		},
		{
			name:       "every reading clipped",
			samples:    map[string][]int{"a": {21, -21}, "b": {20}},
			wantCorr:   0,
			wantUsable: 0,
		},
		{name: "no sample", samples: map[string][]int{}, wantCorr: 0, wantUsable: 0},
		{
			// The register sits past the FOC limit: the readings that carry the
			// offset all pin at the limit and the survivors are noise near zero.
			name: "one sided clipping moves the register",
			samples: map[string][]int{
				"a": append(repeatInt(8, -20), -3, -2, -1, 0, 1, 2, 3, -1, 0, 1, -2, 2),
			},
			wantCorr:    -20,
			wantUsable:  12,
			wantClipped: true,
		},
		{
			name:        "clipping on both sides",
			samples:     map[string][]int{"a": repeatInt(8, -20), "b": repeatInt(4, 20)},
			wantCorr:    0,
			wantUsable:  0,
			wantClipped: false,
		},
		{
			name:        "few clipped readings",
			samples:     map[string][]int{"a": append(repeatInt(3, -20), repeatInt(17, -2)...)},
			wantCorr:    -2,
			wantUsable:  17,
			wantClipped: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			corr, usable, clipped := TuneCorrection(tt.samples)
			if corr != tt.wantCorr || usable != tt.wantUsable || clipped != tt.wantClipped {
				t.Errorf("select=%d,%d,%t want %d,%d,%t",
					corr, usable, clipped, tt.wantCorr, tt.wantUsable, tt.wantClipped)
			}
		})
	}
}

// dataFrame builds a synthetic Data frame of n payload bytes from one source.
func dataFrame(n int, source string) *RxFrame {
	payload := make([]byte, n)
	payload[0], payload[1] = 0x69, 0xec
	copy(payload[11:16], source)
	return &RxFrame{Payload: payload, CRCOK: true, HasFreqEst: true}
}

// broadcastFrame builds a synthetic Broadcast frame of n payload bytes.
func broadcastFrame(n int, source string) *RxFrame {
	payload := make([]byte, n)
	payload[0], payload[1] = 0x49, 0xe8
	copy(payload[5:13], source)
	return &RxFrame{Payload: payload, CRCOK: true, HasFreqEst: true}
}

// wakeUpFrame builds a synthetic 12-byte wake-up frame.
func wakeUpFrame() *RxFrame {
	payload := make([]byte, 12)
	payload[0] = 0x25
	return &RxFrame{Payload: payload, CRCOK: true, HasFreqEst: true}
}

func TestTuneSample(t *testing.T) {
	if source, ok := TuneSample(wakeUpFrame()); !ok || source != "coordinator" {
		t.Errorf("a WakeUp frame must feed the tune: source=%q,%t", source, ok)
	}
	if _, ok := TuneSample(dataFrame(64, "abcde")); !ok {
		t.Error("a 64-byte Data frame must feed the tune")
	}
	if _, ok := TuneSample(dataFrame(40, "abcde")); !ok {
		t.Error("a short Data frame must feed the tune")
	}
	if _, ok := TuneSample(broadcastFrame(120, "coordina")); !ok {
		t.Error("a Broadcast frame must feed the tune")
	}
	bad := dataFrame(64, "abcde")
	bad.CRCOK = false
	if _, ok := TuneSample(bad); ok {
		t.Error("a CRC-bad frame must not feed the tune")
	}
	ack := &RxFrame{Payload: make([]byte, 64), CRCOK: true, HasFreqEst: true}
	ack.Payload[0], ack.Payload[1] = 0x42, 0xee
	if _, ok := TuneSample(ack); ok {
		t.Error("an Enhanced ACK must not feed the tune")
	}
	blind := wakeUpFrame()
	blind.HasFreqEst = false
	if _, ok := TuneSample(blind); ok {
		t.Error("a frame without a FREQEST reading must not feed the tune")
	}
}

func TestFreqSource(t *testing.T) {
	data := make([]byte, 16)
	data[0], data[1] = 0x69, 0xec
	copy(data[11:], []byte{1, 2, 3, 4, 5})
	if got, ok := freqSource(data); !ok || got != string([]byte{1, 2, 3, 4, 5}) {
		t.Fatalf("data source=%x,%t", got, ok)
	}
	broadcast := make([]byte, 13)
	broadcast[0], broadcast[1] = 0x49, 0xe8
	copy(broadcast[5:], []byte{8, 7, 6, 5, 4, 3, 2, 1})
	if got, ok := freqSource(broadcast); !ok || got != string([]byte{8, 7, 6, 5, 4, 3, 2, 1}) {
		t.Fatalf("broadcast source=%x,%t", got, ok)
	}
	wakeUp := make([]byte, 12)
	wakeUp[0] = 0x25
	if got, ok := freqSource(wakeUp); !ok || got != "coordinator" {
		t.Fatalf("wake-up source=%q,%t", got, ok)
	}
	if _, ok := freqSource(wakeUp[:11]); ok {
		t.Fatal("a truncated wake-up frame must have no source")
	}
}

func TestParseRxBlobFreqEst(t *testing.T) {
	blob := []byte{0x02, 0x69, 0xec, 0xc0, 0xff}
	frame := parseRxBlob(blob)
	if frame == nil {
		t.Fatal("a well-formed blob must parse")
	}
	if frame.HasFreqEst || frame.FreqEst != 0 {
		t.Errorf("a v2 blob carries no FREQEST: freqEst=%d,%t", frame.FreqEst, frame.HasFreqEst)
	}
	if !frame.CRCOK || frame.LQI != 0x7f {
		t.Errorf("lqi/crc byte misread: crcOK=%t lqi=%#02x", frame.CRCOK, frame.LQI)
	}
	tuned := parseRxBlob(append(append([]byte(nil), blob...), 0xf3))
	if tuned == nil {
		t.Fatal("a blob with the trailing FREQEST byte must parse")
	}
	if !tuned.HasFreqEst || tuned.FreqEst != -13 {
		t.Errorf("trailing FREQEST misread: freqEst=%d,%t", tuned.FreqEst, tuned.HasFreqEst)
	}
	if len(tuned.Payload) != 2 || tuned.RSSIdBm != frame.RSSIdBm || tuned.LQI != frame.LQI {
		t.Errorf("the trailing byte moved the rest of the blob: %+v", tuned)
	}
	if parseRxBlob(blob[:3]) != nil {
		t.Error("a truncated blob must not parse")
	}
}

func TestParseAutoAckRXBlobFreqEst(t *testing.T) {
	blob := []byte{0x02, 0x69, 0xec, 0xc0, 0xff}
	if frame := parseAutoAckRXBlob(blob); frame == nil || frame.HasFreqEst {
		t.Errorf("a v2 AUTOACK blob must parse without a FREQEST reading: %+v", frame)
	}
	frame := parseAutoAckRXBlob(append(append([]byte(nil), blob...), 0xf3))
	if frame == nil || !frame.HasFreqEst || frame.FreqEst != -13 {
		t.Errorf("a v2.1 AUTOACK blob must carry FREQEST: %+v", frame)
	}
	if parseAutoAckRXBlob(append(append([]byte(nil), blob...), 0xf3, 0x00)) != nil {
		t.Error("an AUTOACK blob with two trailing bytes must not parse")
	}
}

// The v2.1 CMD_RECV response carries FREQEST too, so dry-run mode tunes on the
// frames it takes with RecvFW.
func TestRecvFWFreqEst(t *testing.T) {
	blob := []byte{0x02, 0x69, 0xec, 0xc0, 0xff}
	cases := []struct {
		name    string
		status  byte
		blob    []byte
		want    bool
		freqEst int
	}{
		{name: "v2.1", status: 0x00, blob: append(append([]byte(nil), blob...), 0xf3), want: true, freqEst: -13},
		{name: "v2", status: 0x00, blob: blob},
		{name: "no-frame", status: 0x11},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakePort{read: autoAckResponse(tc.status, tc.blob)}
			b := &Bridge{port: p, timeout: time.Millisecond}
			frame, err := b.RecvFW(2 * time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if tc.status != 0x00 {
				if frame != nil {
					t.Fatalf("a no-frame status decoded a frame: %+v", frame)
				}
				return
			}
			if frame == nil {
				t.Fatal("a well-formed response must decode a frame")
			}
			if frame.HasFreqEst != tc.want || frame.FreqEst != tc.freqEst {
				t.Errorf("freqEst = %d,%t, want %d,%t", frame.FreqEst, frame.HasFreqEst, tc.freqEst, tc.want)
			}
			if !bytes.Equal(frame.Payload, blob[1:3]) || !frame.CRCOK || frame.LQI != 0x7f {
				t.Errorf("the trailing byte moved the rest of the blob: %+v", frame)
			}
			want := []byte{reqMagic, cmdRecv, 0x00, 0x02, 0x07, 0xd0}
			if len(p.writes) != 1 || !bytes.Equal(p.writes[0], want) {
				t.Errorf("request = %x, want %x", p.writes, want)
			}
		})
	}
}
