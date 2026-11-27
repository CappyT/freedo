package tado

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func synthSensorInstall(t *testing.T, withSerial bool) *Install {
	t.Helper()
	serial := ""
	if withSerial {
		serial = "SU1000000001"
	}
	noBoiler := true
	offSched := false
	in := &Install{
		Version:       ConfigVersion,
		HomeID:        123456789,
		NetworkKeyHex: "00112233445566778899aabbccddeeff",
		Coordinator:   Coordinator{EUI: "a1b2c37520c51b00"},
		Devices: []Device{
			{Kind: KindSensor, EUI: "1a2b3c3107c51b00", Serial: serial, Label: "hallway"},
		},
		Zones: []Zone{
			{
				Number:   11,
				Name:     "hallway",
				Kind:     ZoneHeating,
				Members:  []string{"1a2b3c3107c51b00"},
				Reporter: refList{"1a2b3c3107c51b00"},
			},
		},
		Options: Options{
			NoBoiler:        &noBoiler,
			OfflineSchedule: &offSched,
			Timezone:        "UTC",
		},
	}
	if err := in.Validate(); err != nil {
		t.Fatalf("synthSensorInstall: %v", err)
	}
	return in
}

func TestSensorConfigAndBattery(t *testing.T) {
	in := synthSensorInstall(t, true)
	if got := in.Options.LowBatteryMV(KindSensor); got != 3300 {
		t.Errorf("LowBatteryMV(KindSensor) = %d, want 3300", got)
	}
	if got := in.Options.BatteryPct(KindSensor, 3900); got != 50 {
		t.Errorf("BatteryPct(KindSensor, 3900) = %d, want 50", got)
	}

	// A sensor cannot be a circuit heat source.
	bad := synthSensorInstall(t, true)
	bad.Circuits = []Circuit{{ID: 1, Name: "ch", HeatSource: "SU1000000001"}}
	if err := bad.Validate(); err == nil {
		t.Error("circuit with KindSensor heat_source validated; want error")
	}
}

func TestSensorSerialLearn(t *testing.T) {
	in := synthSensorInstall(t, false)
	su := in.Devices[0].WireEUI()
	e := NewEmu(in.EmuConfig(), nil)

	raw := synthDeviceUplink(t, in, su, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x3101, Token: []byte{0x11},
		Path:    []string{"d", "SU1000000001", "fw", "state"},
		Payload: TLV{Tag: "01a0", Val: []byte{0x08}}.Encode(),
	})
	_ = e.OnFrame(raw)
	learned := e.TakeLearnedSerials()
	if got := learned[hexEUI(su)]; got != "SU1000000001" {
		t.Fatalf("learned serial = %q, want SU1000000001", got)
	}
}

func TestSensorConfigDocumentShape(t *testing.T) {
	in := synthSensorInstall(t, true)
	su := in.Devices[0].WireEUI()
	e := NewEmu(in.EmuConfig(), nil)

	// Sensor GETs d/<serial>/config with Block2 0/0/128.
	raw := synthDeviceUplink(t, in, su, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x4101, Token: []byte{0x21},
		Path:    []string{"d", "SU1000000001", "config"},
		Options: []CoAPOption{{Num: 23, Val: []byte{0x03}}},
	})
	ans := synthAnswer(t, in, e.OnFrame(raw), 0x4101)
	if ans.Code != "2.05" {
		t.Fatalf("d/config code = %s, want 2.05", ans.Code)
	}
	if len(ans.Payload) != 30 {
		t.Fatalf("d/config length = %d, want 30 (%x)", len(ans.Payload), ans.Payload)
	}
	tlvs := parseTLVList(ans.Payload)
	want := []struct {
		tag string
		val string
	}{
		{"0143", "00"},
		{"015c", "075bcd15"}, // 123456789
		{"02b3", "00"},
		{"02b2", "0000"},
		{"015e", "0d0b"},
		{"0158", "0200"},
	}
	if len(tlvs) != len(want) {
		t.Fatalf("d/config tlvs = %v, want %d tags", tlvs, len(want))
	}
	for i, w := range want {
		if tlvs[i].Tag != w.tag || hex.EncodeToString(tlvs[i].Val) != w.val {
			t.Errorf("tlv[%d] = %s:%x, want %s:%s", i, tlvs[i].Tag, tlvs[i].Val, w.tag, w.val)
		}
	}
	// Block2 must be 0/0/64 (szx=2, more=0, num=0 -> 0x02).
	if o, ok := ans.Option(23); !ok || decodeCoAPUint(o.Val) != 0x02 {
		t.Errorf("d/config Block2 = %v (ok=%v), want 0x02 (0/0/64)", o.Val, ok)
	}
	etagOpt, ok := ans.Option(4)
	if !ok || len(etagOpt.Val) != 8 {
		t.Fatalf("d/config missing 8-byte ETag")
	}

	// Conditional GET with the served ETag returns 2.03 Valid.
	raw2 := synthDeviceUplink(t, in, su, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x4102, Token: []byte{0x22},
		Path: []string{"d", "SU1000000001", "config"},
		Options: []CoAPOption{
			{Num: 4, Val: etagOpt.Val},
			{Num: 23, Val: []byte{0x03}},
		},
	})
	ans2 := synthAnswer(t, in, e.OnFrame(raw2), 0x4102)
	if ans2.Code != "2.03" {
		t.Errorf("conditional d/config code = %s, want 2.03", ans2.Code)
	}

	// Child lock is refused on KindSensor.
	if err := e.EnqueueChildLock(hexEUI(su), 11, true); err == nil {
		t.Error("EnqueueChildLock succeeded on KindSensor; want error")
	}
}

func TestSensorZoneConfigBlock2Unscaled64(t *testing.T) {
	in := synthSensorInstall(t, true)
	su := in.Devices[0].WireEUI()
	e := NewEmu(in.EmuConfig(), nil)

	wantDoc, err := BuildZoneConfigDocument(in, 11)
	if err != nil {
		t.Fatal(err)
	}
	if len(wantDoc.Body) != 217 {
		t.Fatalf("zone config length = %d, want 217", len(wantDoc.Body))
	}

	// The sensor requests SZX 3 (128) on every block while counting NUM in
	// the 64-byte blocks served to it: 0/0/128, 1/0/128, 2/0/128, 3/0/128.
	wantBlocks := []struct {
		reqBlock  byte
		respBlock uint32
		size      int
	}{
		{0x03, 0x0a, 64}, // 0/1/64
		{0x13, 0x1a, 64}, // 1/1/64
		{0x23, 0x2a, 64}, // 2/1/64
		{0x33, 0x32, 25}, // 3/0/64
	}
	var got []byte
	for i, wb := range wantBlocks {
		mid := uint16(0x4200 + i)
		raw := synthDeviceUplink(t, in, su, CoAPReq{
			Type: "CON", Code: "0.01", MID: mid, Token: []byte{0x31},
			Path:    homePath(in, "z", "11", "config"),
			Options: []CoAPOption{{Num: 23, Val: []byte{wb.reqBlock}}},
		})
		ans := synthAnswer(t, in, e.OnFrame(raw), mid)
		if ans.Code != "2.05" {
			t.Fatalf("block %d code = %s, want 2.05", i, ans.Code)
		}
		if o, ok := ans.Option(23); !ok || decodeCoAPUint(o.Val) != wb.respBlock {
			t.Fatalf("block %d Block2 = %x (ok=%v), want %x", i, o.Val, ok, wb.respBlock)
		}
		if len(ans.Payload) != wb.size {
			t.Fatalf("block %d len = %d, want %d", i, len(ans.Payload), wb.size)
		}
		got = append(got, ans.Payload...)
	}
	if !bytes.Equal(got, wantDoc.Body) {
		t.Errorf("reassembled zone doc does not match BuildZoneConfigDocument")
	}
}

func TestSensorReporterWarning(t *testing.T) {
	noBoiler := true
	in := &Install{
		Version:       ConfigVersion,
		HomeID:        123456789,
		NetworkKeyHex: "00112233445566778899aabbccddeeff",
		Coordinator:   Coordinator{EUI: "a1b2c37520c51b00"},
		Devices: []Device{
			{Kind: KindSensor, EUI: "1a2b3c3107c51b00", Serial: "SU1000000001", Label: "hallway"},
			{Kind: KindValve, EUI: "2b3c4d3107c51b00", Serial: "VA1000000002", Label: "rad"},
		},
		Zones: []Zone{
			{
				Number:   11,
				Name:     "hallway",
				Kind:     ZoneHeating,
				Members:  []string{"1a2b3c3107c51b00", "2b3c4d3107c51b00"},
				Reporter: refList{"2b3c4d3107c51b00"},
			},
		},
		Options: Options{NoBoiler: &noBoiler, Timezone: "UTC"},
	}
	// Case 1: sensor with a valve as reporter warns.
	var c issues
	in.checkZones(&c)
	hasReporterWarn := func(warns []string) bool {
		for _, w := range warns {
			if strings.Contains(w, "not its demand reporter") {
				return true
			}
		}
		return false
	}
	if !hasReporterWarn(c.warns) {
		t.Fatalf("expected warning when sensor is not zone reporter, got warnings=%v", c.warns)
	}

	// Case 2: sensor in a zone with no reporter warns.
	in.Zones[0].Reporter = nil
	c = issues{}
	in.checkZones(&c)
	if !hasReporterWarn(c.warns) {
		t.Fatalf("expected warning when sensor is in zone with no reporter, got warnings=%v", c.warns)
	}

	// Case 3: coordinator-owned zone with no reporter does not warn.
	in.Options.ZoneOwners = map[int]string{11: ZoneOwnerCoordinator}
	c = issues{}
	in.checkZones(&c)
	if hasReporterWarn(c.warns) {
		t.Fatalf("unexpected reporter warning in coordinator-owned zone: %v", c.warns)
	}

	// Case 4: sensor elected as reporter does not warn.
	in.Options.ZoneOwners = nil
	in.Zones[0].Reporter = refList{"1a2b3c3107c51b00"}
	c = issues{}
	in.checkZones(&c)
	if hasReporterWarn(c.warns) {
		t.Fatalf("unexpected reporter warning when sensor is elected: %v", c.warns)
	}
}

func TestSensorSafetyPaths(t *testing.T) {
	in := synthSensorInstall(t, true)
	su := in.Devices[0].WireEUI()
	key := hexEUI(su)
	e := NewEmu(in.EmuConfig(), nil)

	// 1. refusesConfigWrite includes KindSensor.
	if !e.ServesConfigOnRead("SU1000000001") {
		t.Fatal("ServesConfigOnRead(KindSensor) = false, want true")
	}

	// 2. EnqueueConfig serves on read, updates 0140, and queues an empty re-read trigger.
	before := e.cfgDoc[key]
	off := -100
	if err := e.EnqueueConfig("SU1000000001", 11, ConfigWrite{CalibOffset: &off}); err != nil {
		t.Fatalf("EnqueueConfig(CalibOffset): %v", err)
	}
	if !e.ConfigRereadQueued(key) {
		t.Error("ConfigRereadQueued(KindSensor) = false after 0140 change, want true")
	}
	after := e.cfgDoc[key]
	if bytes.Equal(before.etag, after.etag) {
		t.Error("d/config ETag did not change after CalibOffset update")
	}

	// Stripped tag (0149 Orientation) reports success and leaves the served document unchanged.
	orient := uint8(1)
	if err := e.EnqueueConfig("SU1000000001", 11, ConfigWrite{Orientation: &orient}); err != nil {
		t.Fatalf("EnqueueConfig(Orientation): %v", err)
	}
	if !bytes.Equal(after.etag, e.cfgDoc[key].etag) {
		t.Error("d/config ETag changed for stripped tag 0149")
	}

	// 3. d/dbg2/tlvs is refused for KindSensor.
	if err := e.EnqueueCalibOffsetDbg2("SU1000000001", 1, 50); err == nil {
		t.Error("EnqueueCalibOffsetDbg2 succeeded on KindSensor; want error")
	}

	// 4. installConfigBase keeps UTCOffsetMin for KindSensor even when cfgDoc is already held.
	base := e.installConfigBase(&in.Devices[0])
	if base.UTCOffsetMin == nil {
		t.Error("installConfigBase(KindSensor).UTCOffsetMin = nil, want non-nil")
	}
}
