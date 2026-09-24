package tado

import (
	"encoding/hex"
	"errors"
	"strconv"
)

// Display orientations, the values of d/config tag 0149.
const (
	OrientationHorizontal = "horizontal"
	OrientationVertical   = "vertical"
)

const (
	orientationTag  = "0149"
	changeNotifyTag = "0158"
	// orientationMax bounds tag 0149: disp_lcd_screen_config@0x30f44 lays out
	// 0 and 1 only and logs an assert (diag 0x246/0x2ad) on any other value.
	orientationMax = 1
	// app_state_decode_7db@0x2507c reads bit 9 and bit 10 of tag 0158 and
	// ignores the others. Bit 9 shows the change notification when a zone
	// write carries 6440 bit 0 (a remote overlay change), bit 10 on any change
	// of the zone values, and wins when both are set. 0x7fff reads as off.
	changeNotifyRemote uint16 = 0x0200
	changeNotifyAny    uint16 = 0x0400
	changeNotifyUnset  uint16 = 0x7fff
)

// DisplaySettings is the display part of a valve's d/config document: the
// values freedo serves and writes, not a readback from the device.
type DisplaySettings struct {
	// Orientation is tag 0149: "horizontal" (0) or "vertical" (1).
	Orientation string `json:"orientation"`
	// Notification says the valve shows the change notification: bit 9 or
	// bit 10 of tag 0158.
	Notification bool `json:"notification"`
}

// DisplaySettingsWrite changes the display part of a valve's d/config. A nil
// field keeps its value. Via picks the write: OffsetModeConfig (or "") for the
// whole d/config write, OffsetModeDbg2 for PUT d/dbg2/tlvs.
type DisplaySettingsWrite struct {
	Orientation  *string
	Notification *bool
	Via          string
}

// DisplaySettings reads the display part of target's d/config document, the
// captured defaults when freedo holds no document for it yet.
func (e *EmuState) DisplaySettings(target string) (DisplaySettings, error) {
	key, err := e.displayValve(target)
	if err != nil {
		return DisplaySettings{}, err
	}
	orientation, notify := displayValues(parseTLVList(e.cfgDoc[key].body))
	out := DisplaySettings{Orientation: OrientationHorizontal, Notification: notifyOn(notify)}
	if orientation == 1 {
		out.Orientation = OrientationVertical
	} else if orientation != 0 {
		out.Orientation = "unknown"
	}
	return out, nil
}

// EnqueueDisplaySettings writes a changed display setting to target. Turning
// the notification on sets bit 9 when neither notification bit is set, turning
// it off clears both; every other bit of tag 0158 keeps its value. A write that
// changes nothing queues nothing.
//
// Via OffsetModeConfig writes the whole d/config document with a new ETag, as
// the commercial bridge does: the valve re-reads d/config and homes its motor.
// Via OffsetModeDbg2 writes only the changed 0149 and 0158 records with one PUT
// d/dbg2/tlvs (enqueueDbg2) and no d/config write. The valve UI handler
// (ui_gva_attr_write_handler@0x25e70) takes both from the attribute report.
func (e *EmuState) EnqueueDisplaySettings(target string, id int, w DisplaySettingsWrite) error {
	if w.Orientation == nil && w.Notification == nil {
		return errors.New("tado: display write has no fields")
	}
	switch w.Via {
	case "", OffsetModeConfig, OffsetModeDbg2:
	default:
		return errors.New("tado: bad display write via " + strconv.Quote(w.Via) + ": want config or dbg2")
	}
	key, err := e.displayValve(target)
	if err != nil {
		return err
	}
	orientation, notify := displayValues(parseTLVList(e.configBase(key).body))
	var cw ConfigWrite
	if w.Orientation != nil {
		v, err := parseOrientation(*w.Orientation)
		if err != nil {
			return err
		}
		cw.Orientation = &v
	}
	if w.Notification != nil {
		v := notify
		if v == changeNotifyUnset {
			v = 0
		}
		switch {
		case !*w.Notification:
			v &^= changeNotifyRemote | changeNotifyAny
		case !notifyOn(v):
			v |= changeNotifyRemote
		}
		cw.ChangeNotify = &v
	}
	if w.Via != OffsetModeDbg2 {
		return e.EnqueueConfig(key, id, cw)
	}
	if cw.Orientation != nil && *cw.Orientation == orientation {
		cw.Orientation = nil
	}
	if cw.ChangeNotify != nil && *cw.ChangeNotify == notify {
		cw.ChangeNotify = nil
	}
	tlvs := cw.TLVs()
	if len(tlvs) == 0 {
		return nil
	}
	desc := "display"
	for _, t := range tlvs {
		desc += " " + t.Tag + "=" + hex.EncodeToString(t.Val)
	}
	desc += " id=" + strconv.Itoa(id) + " via d/dbg2/tlvs"
	return e.enqueueDbg2(key, desc, tlvs)
}

func parseOrientation(s string) (uint8, error) {
	switch s {
	case OrientationHorizontal:
		return 0, nil
	case OrientationVertical:
		return 1, nil
	}
	return 0, errors.New(`tado: orientation must be "horizontal" or "vertical"`)
}

func notifyOn(v uint16) bool {
	return v != changeNotifyUnset && v&(changeNotifyRemote|changeNotifyAny) != 0
}

// displayValues reads tags 0149 and 0158 from a document, each defaulting to
// the value configDocDefaults serves.
func displayValues(doc []TLV) (uint8, uint16) {
	var orientation uint8
	var notify uint16
	for _, def := range configDocDefaults() {
		switch def.Tag {
		case orientationTag:
			orientation = def.Val[0]
		case changeNotifyTag:
			notify, _ = tlvU16([]TLV{def}, changeNotifyTag)
		}
	}
	for _, t := range doc {
		if t.Tag == orientationTag && len(t.Val) == 1 {
			orientation = t.Val[0]
		}
	}
	if v, ok := tlvU16(doc, changeNotifyTag); ok {
		notify = v
	}
	return orientation, notify
}

// displayValve resolves target to the wire key of a declared valve. The wall
// thermostat has no display orientation and refuses every d/config write.
func (e *EmuState) displayValve(target string) (string, error) {
	if e.inst == nil {
		return "", errors.New("tado: display settings need a declared valve")
	}
	dev, err := e.inst.Resolve(target)
	if err != nil {
		return "", err
	}
	if dev.Kind != KindValve {
		return "", errors.New("tado: display settings apply to a valve only")
	}
	wire := dev.WireEUI()
	if len(wire) != 8 {
		return "", errors.New("tado: unusable EUI for " + target)
	}
	return hexEUI(wire), nil
}
