package tado

import (
	"encoding/hex"
	"strings"
)

// EUISuffix is the 3-byte tail shared by every node's wire address
// (00:1b:c5 reversed, without the last octet). It also opens the app plaintext.
var EUISuffix = mustHex("c51b00")

// NetworkKey, BridgeEUI and Inventory are the legacy single-install globals.
// They are EMPTY here and stay empty in every production path: an installation
// is described by an *Install (config.go), which carries the key, our
// coordinator EUI and the device list. They survive as package variables only
// so the capture-driven tests can point them at the network their frames came
// from; nothing outside a test may assume they are populated.
var (
	NetworkKey []byte
	BridgeEUI  []byte
	Inventory  []Node
)

// FCF octets for the three on-air frame classes (byte0 low, byte1 high).
var (
	fcfData        = [2]byte{0x69, 0xec} // Data, secured (CCM*), ack-req, PAN-id compression
	fcfDataPending = [2]byte{0x79, 0xec} // Data + Frame Pending: more frames queued for the peer
	fcfEnhAck      = [2]byte{0x42, 0xee} // Enhanced ACK, unsecured
	fcfShort       = byte(0x25)          // compact/multipurpose, unsecured (byte1 = 0xa8..0xaf)
)

// Node is one entry of the legacy Inventory. Device (config.go) replaces it.
type Node struct {
	WireLE string // 8-byte EUI as seen on air (little-endian), hex
	EUI    string // real EUI-64 (big-endian), colon form
	Short  string // 16-bit short address, hex
	Serial string
	Room   string
}

// NodeByAny resolves a node of the legacy Inventory by serial, short address,
// wire address, or room name (case-insensitive). Returns nil if unknown, which
// with an empty Inventory means always. Use (*Install).Resolve instead: it
// reports an ambiguous room label instead of returning the first match.
func NodeByAny(s string) *Node {
	s = strings.ToLower(strings.TrimSpace(s))
	for i := range Inventory {
		n := &Inventory[i]
		if strings.ToLower(n.Serial) == s || strings.ToLower(n.Short) == s ||
			strings.ToLower(n.WireLE) == s || (n.Room != "" && strings.ToLower(n.Room) == s) {
			return n
		}
	}
	return nil
}

// ShortFromWire returns the 16-bit short address (hex) for an 8-byte wire EUI:
// the little-endian first two bytes swapped. Example 02a6.. -> "a602".
func ShortFromWire(wire []byte) string {
	if len(wire) < 2 {
		return ""
	}
	return hex.EncodeToString([]byte{wire[1], wire[0]})
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic("tado: bad hex constant " + s)
	}
	return b
}
