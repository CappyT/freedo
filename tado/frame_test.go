package tado

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"encoding/hex"
	"errors"
	"os"
	"testing"
)

// loadGolden reads the captured 0x69ec frames (hex per line).
func loadGolden(t *testing.T) [][]byte {
	t.Helper()
	f, err := os.Open("testdata/frames_69ec.txt")
	if err != nil {
		t.Fatalf("open golden: %v", err)
	}
	defer f.Close()
	var frames [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<16)
	for sc.Scan() {
		b, err := hex.DecodeString(sc.Text())
		if err != nil {
			t.Fatalf("bad hex line: %v", err)
		}
		frames = append(frames, b)
	}
	if len(frames) == 0 {
		t.Fatal("no golden frames loaded")
	}
	return frames
}

// TestDecryptGolden: every captured Data frame decrypts with a valid MIC. A MIC
// pass proves key, framing, and CCM* are all correct.
func TestDecryptGolden(t *testing.T) {
	frames := loadGolden(t)
	for i, fr := range frames {
		if _, err := ParseData(NetworkKey, fr); err != nil {
			t.Errorf("frame %d (%s...): %v", i, hex.EncodeToString(fr[:16]), err)
		}
	}
	t.Logf("decrypted %d/%d frames, all MIC valid", len(frames), len(frames))
}

// TestRoundTrip: decrypt then re-encrypt reproduces each frame byte-for-byte.
// This is the encoder-correctness proof (the Go port of tado_build selftest).
func TestRoundTrip(t *testing.T) {
	frames := loadGolden(t)
	for i, fr := range frames {
		p, err := ParseData(NetworkKey, fr)
		if err != nil {
			t.Fatalf("frame %d decrypt: %v", i, err)
		}
		rebuilt, err := BuildData(NetworkKey, p.Seq, p.DstEUI, p.SrcPrefix, p.Plaintext)
		if err != nil {
			t.Fatalf("frame %d rebuild: %v", i, err)
		}
		if !bytes.Equal(rebuilt.Bytes(), fr) {
			t.Errorf("frame %d mismatch\n orig=%s\n got =%s", i, hex.EncodeToString(fr), rebuilt.Hex())
		}
	}
	t.Logf("round-trip re-encoded %d/%d frames byte-for-byte", len(frames), len(frames))
}

func TestCapturedMACFCS(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		parse func([]byte) ([]byte, error)
	}{
		{"data", capturedDHWSetpoint, func(frame []byte) ([]byte, error) {
			p, err := ParseData(NetworkKey, frame)
			if err != nil {
				return nil, err
			}
			return p.Plaintext, nil
		}},
		{"data-pending", capturedFlowConfig, func(frame []byte) ([]byte, error) {
			p, err := ParseData(NetworkKey, frame)
			if err != nil {
				return nil, err
			}
			return p.Plaintext, nil
		}},
		{"router-advertisement", capturedRA, func(frame []byte) ([]byte, error) {
			p, err := ParseBroadcast(NetworkKey, frame)
			if err != nil {
				return nil, err
			}
			return p.Plaintext, nil
		}},
		{"router-solicitation", capturedRS, func(frame []byte) ([]byte, error) {
			p, err := ParseBroadcast(NetworkKey, frame)
			if err != nil {
				return nil, err
			}
			return p.Plaintext, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame := mustB(tc.frame)
			plaintext, err := tc.parse(frame)
			if err != nil {
				t.Fatal(err)
			}
			if len(plaintext) < 2 {
				t.Fatal("plaintext has no MAC FCS")
			}
			want := macFCS(append(append([]byte{}, frame[:headerLen]...), plaintext[:len(plaintext)-2]...))
			if !bytes.Equal(plaintext[len(plaintext)-2:], want) {
				t.Fatalf("FCS=%x, want %x", plaintext[len(plaintext)-2:], want)
			}
		})
	}
}

func TestParseDataRejectsValidMICWithBadMACFCS(t *testing.T) {
	raw := mustB(capturedDHWSetpoint)
	p, err := ParseData(NetworkKey, raw)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := append([]byte(nil), p.Plaintext...)
	plaintext[len(plaintext)-1] ^= 0x01
	block, err := aes.NewCipher(NetworkKey)
	if err != nil {
		t.Fatal(err)
	}
	ct, tag := ccmEncrypt(block, raw[:nonceLen], raw[:headerLen], plaintext)
	bad := append(append(append([]byte{}, raw[:headerLen]...), ct...), tag...)
	if _, err := ParseData(NetworkKey, bad); !errors.Is(err, ErrMACFCS) {
		t.Fatalf("error=%v, want ErrMACFCS", err)
	}
}

func TestFrameBuildersWriteMACFCS(t *testing.T) {
	plaintext := WrapAppPayload([]byte{0x60, 0x00}, 1, nil)
	cases := []struct {
		name  string
		frame []byte
		parse func([]byte) ([]byte, error)
	}{
		{"data", mustBuildData(t, fcfData, plaintext), func(frame []byte) ([]byte, error) {
			p, err := ParseData(NetworkKey, frame)
			if err != nil {
				return nil, err
			}
			return p.Plaintext, nil
		}},
		{"data-pending", mustBuildData(t, fcfDataPending, plaintext), func(frame []byte) ([]byte, error) {
			p, err := ParseData(NetworkKey, frame)
			if err != nil {
				return nil, err
			}
			return p.Plaintext, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plaintext, err := tc.parse(tc.frame)
			if err != nil {
				t.Fatal(err)
			}
			want := macFCS(append(append([]byte{}, tc.frame[:headerLen]...), plaintext[:len(plaintext)-2]...))
			if !bytes.Equal(plaintext[len(plaintext)-2:], want) {
				t.Fatalf("FCS=%x, want %x", plaintext[len(plaintext)-2:], want)
			}
		})
	}
}

func mustBuildData(t *testing.T, fcf [2]byte, plaintext []byte) []byte {
	t.Helper()
	var (
		frame *Frame
		err   error
	)
	if fcf == fcfData {
		frame, err = BuildData(NetworkKey, 0x42, BridgeEUI, BridgeEUI[:5], plaintext)
	} else {
		frame, err = BuildDataPending(NetworkKey, 0x42, BridgeEUI, BridgeEUI[:5], plaintext)
	}
	if err != nil {
		t.Fatal(err)
	}
	return frame.Bytes()
}

// TestWrongKeyFails: a bad key must fail the MIC, not silently return garbage.
func TestWrongKeyFails(t *testing.T) {
	fr := loadGolden(t)[0]
	bad := append([]byte{}, NetworkKey...)
	bad[0] ^= 0xff
	if _, err := ParseData(bad, fr); err != ErrMICFail {
		t.Fatalf("want ErrMICFail with wrong key, got %v", err)
	}
}

// TestCorruptRejected: frames that pass CRC16 on air but are corrupt must fail
// the CCM* MIC. These two came from real captures; the MIC is the strong check
// that CRC16 alone misses.
func TestCorruptRejected(t *testing.T) {
	f, err := os.Open("testdata/frames_corrupt.txt")
	if err != nil {
		t.Skipf("no corrupt fixture: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		fr, err := hex.DecodeString(line)
		if err != nil {
			t.Fatalf("bad hex: %v", err)
		}
		if _, err := ParseData(NetworkKey, fr); err != ErrMICFail {
			t.Errorf("corrupt frame %s... should fail MIC, got %v", line[:16], err)
		}
		n++
	}
	t.Logf("rejected %d corrupt frames via MIC", n)
}

func TestBuildDataRejectsOversizedFrame(t *testing.T) {
	if _, err := BuildData(NetworkKey, 1, BridgeEUI, BridgeEUI[:5], make([]byte, 108)); err == nil {
		t.Fatal("BuildData accepted a frame above the 127-byte PHY limit")
	}
	if frame, err := BuildData(NetworkKey, 1, BridgeEUI, BridgeEUI[:5], make([]byte, 107)); err != nil || len(frame.Bytes()) != 127 {
		t.Errorf("127-byte boundary frame = %v, %v", frame, err)
	}
}
