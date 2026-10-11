package tado

import (
	"encoding/binary"
	"errors"
	"math"
)

// SetpointCmd describes a downlink "set temperature" command to one valve.
//
// Meta is the 6LoWPAN(IPHC+UDP-NHC)+CoAP header between the counter and the
// payload TLVs (the compressed IPv6/UDP header plus the CoAP request line, e.g.
// PUT z/s?id=N). Copying it verbatim from a real downlink to the SAME valve and
// swapping the setpoint keeps the outer CCM* MIC valid, but leaves a stale UDP
// checksum inside Meta (it was computed over the original payload). A valve with a
// real UDP stack may drop that; a correct rebuild recomputes the UDP checksum over
// the new payload (or clears it via UDP-NHC C=1). See ParseCoAP / README "Live inject".
type SetpointCmd struct {
	DstEUI     []byte  // 8-byte wire EUI of the target valve
	SrcPrefix  []byte  // 5-byte src prefix; nil => bridge (spoof a downlink)
	Seq        byte    // MAC sequence number
	Counter    uint32  // must be bridge_counter+1 to beat anti-replay
	Setpoint   float64 // active setpoint, degrees C; sent only with a manual overlay
	Meta       []byte  // copied from a real capture to the same valve
	Overlay    byte    // 0=auto, 1=until the next automatic change, 2=manual, 3=timer or until_next
	OpenWindow byte    // 62e0 value
	// Home, SchedC and NewOverlay carry the state of the whole home and the
	// overlay marker, as in SetpointCoAP.
	Home       bool
	SchedC     float64
	NewOverlay bool
	Trailer    []byte // reserved 2-byte MAC FCS; BuildData replaces it
}

// BuildSetpoint assembles the plaintext for a setpoint command and frames it.
// Returns the on-air Frame and the plaintext that was encrypted.
func BuildSetpoint(key []byte, c SetpointCmd) (*Frame, []byte, error) {
	if len(c.DstEUI) != 8 {
		return nil, nil, errors.New("tado: DstEUI must be 8 bytes")
	}
	src := c.SrcPrefix
	if src == nil {
		src = BridgeEUI[:5]
	}
	trailer := c.Trailer
	if trailer == nil {
		trailer = []byte{0x00, 0x00}
	}
	if len(trailer) != 2 {
		return nil, nil, errors.New("tado: Trailer must be 2 bytes")
	}

	z := zoneWrite{
		home: c.Home, schedC: c.SchedC, kind: roomZoneKind,
		overlay: c.Overlay, openWindow: c.OpenWindow, newOverlay: c.NewOverlay,
	}
	if c.Overlay != overlayAuto {
		z.enabled, z.setpoint = overlayEnabled(), tempC(c.Setpoint)
	}
	body := encodeZoneWrite(z)

	ctr := make([]byte, 4)
	binary.LittleEndian.PutUint32(ctr, c.Counter)
	pt := concat(EUISuffix, []byte{0x04}, ctr, c.Meta, body, trailer)

	f, err := BuildData(key, c.Seq, c.DstEUI, src, pt)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Plaintext, nil
}

// tempC encodes a temperature in degrees C as a 2-byte big-endian value*100.
func tempC(c float64) []byte {
	v := int(math.Round(c * 100))
	return []byte{byte(v >> 8), byte(v)}
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
