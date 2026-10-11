package tado

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// secretTags are the TLV tags whose value must not reach a log line: the device
// bearer token (0007), the device serial (0260), the session id (025e), the
// rotated challenge key (0261) and the challenge response (0262). The network
// key never travels as a 2-byte-tag TLV: it is tag8 0x12 of d/pair, inside a
// frame that loggedFrameHex cuts.
var secretTags = map[string]bool{
	"0007": true, "0260": true, "025e": true, "0261": true, "0262": true,
}

// redacted stands for a secret value in a log line: its length only.
func redacted(n int) string { return fmt.Sprintf("<%d bytes redacted>", n) }

// redactedPayload renders a CoAP payload for a log line. A TLV list keeps every
// tag id and shows the value, except for a secret tag, which shows its length.
// A payload that is no whole TLV list hides where a secret sits, so it shows
// its length only.
func redactedPayload(p []byte) string {
	tlvs, used := parseTLVList(p), 0
	for _, t := range tlvs {
		used += 3 + t.Len
	}
	if used != len(p) {
		return redacted(len(p))
	}
	parts := make([]string, len(tlvs))
	for i, t := range tlvs {
		v := hex.EncodeToString(t.Val)
		if secretTags[t.Tag] {
			v = redacted(t.Len)
		}
		parts[i] = t.Tag + "=" + v
	}
	return strings.Join(parts, " ")
}

// loggedFrameHex is the frame column of an unhandled-log line. A frame on air
// is encrypted under the network key, which only the database holds, so its
// bytes stay. The pairing key is public: the frame it opens, such as the d/pair
// write that carries the network key, keeps its 16-byte header only.
func loggedFrameHex(raw []byte) string {
	if _, err := ParseData(PairingKey, raw); err == nil {
		raw = raw[:headerLen]
	}
	return hex.EncodeToString(raw)
}
