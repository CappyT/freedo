package cli

// replay: read the on-air frame hex out of a capture line.

import (
	"encoding/hex"
	"strings"
	"time"
)

// ReplayHex extracts the on-air frame hex from one capture line. It accepts a
// bare hex frame, a "label: hex" line, a `freedo-dev rx` line
// (<ts> [flag ...] <hex> | <dissection>): the dissection after " | " is dropped
// so the hex before it is taken. It also accepts the tab-separated log lines of
// tado.UnhandledLogger and of the decode log. Comment lines and CRC-BAD rx lines
// are skipped - the hardware CRC gates them before the MAC, so a real
// coordinator never sees them.
func ReplayHex(line string) (string, bool) {
	if strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
		return "", false
	}
	if strings.Contains(line, "CRC-BAD") {
		return "", false
	}
	if fields := logLineFields(line); fields != nil {
		return frameHexField(fields)
	}
	if i := strings.Index(line, " | "); i >= 0 {
		line = line[:i]
	}
	return lastToken(line), true
}

// ReplayFrame decodes the on-air frame from one capture line, or reports false
// when the line carries none.
func ReplayFrame(line string) ([]byte, bool) {
	h, ok := ReplayHex(line)
	if !ok {
		return nil, false
	}
	// Under three bytes there is no sequence number, which every dissector reads.
	fr, err := hex.DecodeString(h)
	if err != nil || len(fr) < 3 {
		return nil, false
	}
	return fr, true
}

// logLineFields splits a tab-separated log line and returns nil for every other
// line. tado.UnhandledLogger writes <ts> <class> <src> <dst> <reason> <hex> and
// the decode log writes <ts> <class> <hex> <dissection>. Both open with an
// RFC3339 timestamp whose colons defeat the "label: hex" split, so they need
// their own path.
func logLineFields(line string) []string {
	fields := strings.Split(strings.TrimSpace(line), "\t")
	if len(fields) < 3 {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, fields[0]); err != nil {
		return nil
	}
	return fields
}

// frameHexField takes the last field that holds a frame. The other fields carry
// labels, reasons and a dissection, and an event without a frame writes a dash.
// The backward scan holds only while no dissection is itself pure hex of at
// least ten bytes, which isFrameHex would take for a frame.
func frameHexField(fields []string) (string, bool) {
	for i := len(fields) - 1; i > 0; i-- {
		if isFrameHex(fields[i]) {
			return fields[i], true
		}
	}
	return "", false
}

// isFrameHex reports whether s is the hex of a frame. The ten byte floor keeps
// short hex fields, such as an EUI label, out of the replay.
func isFrameHex(s string) bool {
	if len(s) < 20 || len(s)%2 != 0 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
