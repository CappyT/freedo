package tado

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

var (
	synthSerial = []byte("VA1111111111")
	synthToken  = bytes.Repeat([]byte{0xab}, 16)
	synthKey    = bytes.Repeat([]byte{0x5a}, 16)
)

func encTLV(tag string, val []byte) []byte { return TLV{Tag: tag, Val: val}.Encode() }

func TestRedactedPayload(t *testing.T) {
	auth := concat(encTLV("0260", synthSerial), encTLV("0007", synthToken))
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"auth token", auth, "0260=<12 bytes redacted> 0007=<16 bytes redacted>"},
		{"session and challenge", concat(encTLV("025e", []byte{1, 2, 3, 4}), encTLV("0261", synthToken), encTLV("0262", synthToken)),
			"025e=<4 bytes redacted> 0261=<16 bytes redacted> 0262=<16 bytes redacted>"},
		{"public tag keeps its value", concat(encTLV("0210", []byte("abc")), encTLV("0007", synthToken)),
			"0210=616263 0007=<16 bytes redacted>"},
		{"empty", nil, ""},
		{"not a TLV list", []byte{0x01, 0x02}, "<2 bytes redacted>"},
		{"trailing bytes after the list", concat(auth, []byte{0x99}), "<35 bytes redacted>"},
		{"length past the end", concat(encTLV("0210", []byte("abc")), []byte{0x00, 0x07, 0x10, 0xab}), "<10 bytes redacted>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactedPayload(tc.in); got != tc.want {
				t.Errorf("redactedPayload = %q, want %q", got, tc.want)
			}
		})
	}
}

// logHasNo fails the test when the log holds the value as hex or as text.
func logHasNo(t *testing.T, log string, what string, val []byte) {
	t.Helper()
	if strings.Contains(log, hex.EncodeToString(val)) || strings.Contains(log, string(val)) {
		t.Errorf("the log holds the %s:\n%s", what, log)
	}
}

// TestUnhandledLogHidesAuthSecrets: the log line of an auth/token request keeps
// the tag ids and the value lengths and drops the serial and the token. A
// request that carries another session id in option 2048 drops that id too.
func TestUnhandledLogHidesAuthSecrets(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	e, _, buf := newTestEmu(t)
	e.OnFrame(deviceUplink(t, dev, CoAPReq{
		Type: "CON", Code: "0.02", MID: 0x0301, Token: []byte{0x09},
		Path: []string{"auth", "token"}, Options: []CoAPOption{optOctets},
		Payload: concat(encTLV("0260", synthSerial), encTLV("0007", synthToken)),
	}))
	synthSession := mustHex("5e5500000000000a")
	e.OnFrame(deviceUplink(t, dev, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x0302, Token: []byte{0x0a},
		Path:    []string{"d", "sen"},
		Options: []CoAPOption{{Num: 2048, Val: synthSession}},
	}))

	log := buf.String()
	for _, want := range []string{
		"auth-accepted auth/token payload=0260=<12 bytes redacted> 0007=<16 bytes redacted>",
		"session-id-mismatch=<8 bytes redacted>",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %q:\n%s", want, log)
		}
	}
	logHasNo(t, log, "serial", synthSerial)
	logHasNo(t, log, "token", synthToken)
	logHasNo(t, log, "session id", synthSession)
}

// TestUnhandledLogCutsPairingKeyFrames: the pairing key is public, so a frame
// it opens, such as a d/pair write, never reaches the log in full, whether the
// pair window is open or not.
func TestUnhandledLogCutsPairingKeyFrames(t *testing.T) {
	pair, err := BuildPairCoAP(synthKey, 0x0401, BridgeEUI, joinerEUI)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := BuildData(PairingKey, 0x31, joinerEUI, BridgeEUI[:5], WrapAppPayload(pair, 9, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, open := range []bool{false, true} {
		now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
		buf := &bytes.Buffer{}
		e := NewEmu(EmuConfig{TxCounter: 100000, CSLPhase: 0x1234, Now: func() time.Time { return now }}, NewUnhandledLogger(buf))
		if open {
			e.OpenPairWindow(time.Minute)
		}
		e.OnFrame(frame.Bytes())

		line := strings.TrimSpace(buf.String())
		if line == "" {
			t.Fatalf("window open %t: the frame left no log line", open)
		}
		cols := strings.Split(line, "\t")
		got, err := hex.DecodeString(cols[len(cols)-1])
		if err != nil {
			t.Fatalf("window open %t: frame column %q: %v", open, cols[len(cols)-1], err)
		}
		if want := frame.Bytes()[:headerLen]; !bytes.Equal(got, want) {
			t.Errorf("window open %t: frame column %x, want the %d-byte header %x", open, got, headerLen, want)
		}
		logHasNo(t, line, "network key", synthKey)
	}
}

// TestUnhandledLogKeepsNetworkKeyFrames: a frame under the network key stays in
// full: it is opaque without the key, and the log is how a bug report shows it.
func TestUnhandledLogKeepsNetworkKeyFrames(t *testing.T) {
	frame, err := BuildData(NetworkKey, 0x31, BridgeEUI, mustHex("02a6cefa07")[:5], []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a})
	if err != nil {
		t.Fatal(err)
	}
	if got := loggedFrameHex(frame.Bytes()); got != hex.EncodeToString(frame.Bytes()) {
		t.Errorf("loggedFrameHex = %s, want the whole frame", got)
	}
}
