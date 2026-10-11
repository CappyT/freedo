package cli

import (
	"encoding/hex"
	"testing"
)

// --- replay line parsing ---

func TestReplayFrameAcceptsLogFormats(t *testing.T) {
	const frame = "69ec7b05a2cafa07c51b0009a1cefa07eacdcd0a6da4daae26d678914521b94f"
	const wakeUp = "258ccdaba205820ef800803f"
	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{"bare hex", frame, frame},
		{"labelled", "hall: " + frame, frame},
		{"rx line", "2026-08-13T14:28:50+02:00 CRC-OK " + frame + " | Data hall", frame},
		{"unhandled log", "2026-08-13T14:28:50+02:00\tData\tbridge\thall\tbridge-sourced-data\t" + frame, frame},
		{"unhandled log utc", "2026-08-13T12:28:50Z\tData\tbridge\thall\tunknown-resource path=/z/s?id=1\t" + frame, frame},
		{"decode log", "2026-08-13T14:28:50+02:00\tData\t" + frame + "\tData hall -> bridge", frame},
		// The shortest frame on air: it pins the ten byte floor of isFrameHex.
		{"unhandled log wakeup", "2026-08-13T14:28:50+02:00\tWakeUp\t-\t820e\tunknown-wake-target short=820e\t" + wakeUp, wakeUp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr, ok := ReplayFrame(tc.line)
			if !ok {
				t.Fatalf("line rejected: %q", tc.line)
			}
			if got := hex.EncodeToString(fr); got != tc.want {
				t.Errorf("frame=%s, want %s", got, tc.want)
			}
		})
	}
}

func TestReplayFrameSkipsUnusableLines(t *testing.T) {
	const frame = "69ec7b05a2cafa07c51b0009a1cefa07eacdcd0a6da4daae26d678914521b94f"
	for _, tc := range []struct {
		name string
		line string
	}{
		{"garbage", "this line holds no frame at all"},
		{"crc bad", "2026-08-13T14:28:50+02:00 CRC-BAD " + frame},
		{"unhandled log without a frame", "2026-08-13T14:28:50+02:00\tConfig\tbedroom\t-\tzone-target zone=10\t-"},
		{"truncated hex", "hall: 69ec7b05a2cafa07c51b0009a1cefa07f"},
		// Classify calls a bare 69ec a Data frame, and every dissector then reads a
		// sequence byte that is not there.
		{"data prefix alone", "hall: 69ec"},
		{"comment", "# hall " + frame},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if fr, ok := ReplayFrame(tc.line); ok {
				t.Errorf("line accepted as %x: %q", fr, tc.line)
			}
		})
	}
}
