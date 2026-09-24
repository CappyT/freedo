package tado

import (
	"errors"
	"fmt"
	"strings"
)

// d/fw/state, the firmware state a valve or a thermostat nRF52 serves on GET and
// PUTs to d/<serial>/fw/state at boot and on every slot change. The same
// attributes travel in the d/<serial>/s info report (coap_info_report_serialize).
// Layout read in coap_fw_state_serialize@0x20fa4 of the valve firmware.
const (
	fwTagDeviceType = "01a0"
	fwTagRunVersion = "003a" // internal slot 0, the running image
	fwTagRunFlags   = "003b"
	fwTagExt10Ver   = "0035" // external slot 0x10
	fwTagExt10Flags = "0036"
	fwTagExt11Ver   = "0039" // external slot 0x11
	fwTagExt11Flags = "003c"
	fwTagBuildHash  = "0210"
	fwTagTimestamp  = "0285"
)

// FwVersion is the u16 image version, major<<6 | minor.
type FwVersion uint16

// NewFwVersion packs major.minor; minor is 6 bits wide.
func NewFwVersion(major, minor int) (FwVersion, error) {
	if major < 0 || major > 0x3ff || minor < 0 || minor > 0x3f {
		return 0, fmt.Errorf("tado: fw version %d.%d out of range", major, minor)
	}
	return FwVersion(major<<6 | minor), nil
}

// ParseFwVersion reads "major.minor".
func ParseFwVersion(s string) (FwVersion, error) {
	var major, minor int
	if n, err := fmt.Sscanf(s, "%d.%d", &major, &minor); err != nil || n != 2 || fmt.Sprintf("%d.%d", major, minor) != s {
		return 0, fmt.Errorf("tado: fw version %q is not major.minor", s)
	}
	return NewFwVersion(major, minor)
}

func (v FwVersion) Major() int { return int(v >> 6) }
func (v FwVersion) Minor() int { return int(v & 0x3f) }

func (v FwVersion) String() string { return fmt.Sprintf("%d.%d", v.Major(), v.Minor()) }

// FwSlotFlags is the state nibble of one image slot.
type FwSlotFlags uint8

const (
	FwSlotPending   FwSlotFlags = 1 << 0
	FwSlotHasImage  FwSlotFlags = 1 << 1
	FwSlotActive    FwSlotFlags = 1 << 2
	FwSlotConfirmed FwSlotFlags = 1 << 3
)

func (f FwSlotFlags) String() string {
	var parts []string
	for _, b := range []struct {
		bit  FwSlotFlags
		name string
	}{{FwSlotPending, "pending"}, {FwSlotHasImage, "image"}, {FwSlotActive, "active"}, {FwSlotConfirmed, "confirmed"}} {
		if f&b.bit != 0 {
			parts = append(parts, b.name)
		}
	}
	if len(parts) == 0 {
		return "empty"
	}
	return strings.Join(parts, "+")
}

// FwSlot is one image slot as d/fw/state reports it.
type FwSlot struct {
	Present bool
	Version FwVersion
	Flags   FwSlotFlags
}

// FwState is the decoded d/fw/state document.
type FwState struct {
	DeviceType uint64 // 01a0, 8 on the valve
	Running    FwSlot // internal slot 0
	Ext10      FwSlot // external slot 0x10
	Ext11      FwSlot // external slot 0x11
	BuildHash  string // 0210, the 7-character git hash of the running build
	Timestamp  uint64 // 0285
}

// ParseFwState decodes a d/fw/state body. The running slot is mandatory.
func ParseFwState(body []byte) (*FwState, error) {
	s := &FwState{}
	var haveType, haveVer, haveFlags bool
	for _, t := range ParseTLVs(body) {
		switch t.Tag {
		case fwTagDeviceType:
			s.DeviceType, haveType = be(t.Val), true
		case fwTagRunVersion:
			s.Running.Version, haveVer = FwVersion(be(t.Val)), true
		case fwTagRunFlags:
			s.Running.Flags, haveFlags = FwSlotFlags(be(t.Val)), true
		case fwTagExt10Ver:
			s.Ext10.Version, s.Ext10.Present = FwVersion(be(t.Val)), true
		case fwTagExt10Flags:
			s.Ext10.Flags, s.Ext10.Present = FwSlotFlags(be(t.Val)), true
		case fwTagExt11Ver:
			s.Ext11.Version, s.Ext11.Present = FwVersion(be(t.Val)), true
		case fwTagExt11Flags:
			s.Ext11.Flags, s.Ext11.Present = FwSlotFlags(be(t.Val)), true
		case fwTagBuildHash:
			s.BuildHash = asciiVal(t.Val)
		case fwTagTimestamp:
			s.Timestamp = be(t.Val)
		}
	}
	if !haveType || !haveVer || !haveFlags {
		return nil, errors.New("tado: fw/state: device type or running slot missing")
	}
	s.Running.Present = true
	return s, nil
}

// Confirmed reports whether the running image is confirmed. The device refuses
// to begin an upload otherwise.
func (s *FwState) Confirmed() bool { return s.Running.Flags&FwSlotConfirmed != 0 }

// FwSlotView is the JSON form of one slot.
type FwSlotView struct {
	Version    string `json:"version"`
	VersionRaw uint16 `json:"version_raw"`
	Flags      uint8  `json:"flags"`
	State      string `json:"state"`
}

// FwStateView is the JSON form of FwState.
type FwStateView struct {
	DeviceType uint64      `json:"device_type"`
	Running    FwSlotView  `json:"running"`
	Ext10      *FwSlotView `json:"ext_slot_10,omitempty"`
	Ext11      *FwSlotView `json:"ext_slot_11,omitempty"`
	Confirmed  bool        `json:"confirmed"`
	BuildHash  string      `json:"build_hash"`
	Timestamp  uint64      `json:"timestamp"`
}

func (sl FwSlot) view() FwSlotView {
	return FwSlotView{Version: sl.Version.String(), VersionRaw: uint16(sl.Version), Flags: uint8(sl.Flags), State: sl.Flags.String()}
}

// View returns the JSON form.
func (s *FwState) View() FwStateView {
	v := FwStateView{
		DeviceType: s.DeviceType, Running: s.Running.view(), Confirmed: s.Confirmed(),
		BuildHash: s.BuildHash, Timestamp: s.Timestamp,
	}
	if s.Ext10.Present {
		x := s.Ext10.view()
		v.Ext10 = &x
	}
	if s.Ext11.Present {
		x := s.Ext11.view()
		v.Ext11 = &x
	}
	return v
}

func fwVersionVal(v []byte) string { return FwVersion(be(v)).String() }
func fwFlagsVal(v []byte) string   { return FwSlotFlags(be(v)).String() }
