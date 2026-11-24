package tado

import (
	"bytes"
	"encoding/hex"
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

func TestSensorDeviceOverlayPreservedInZoneRead(t *testing.T) {
	in := synthSensorInstall(t, true)
	su := in.Devices[0].WireEUI()
	e := NewEmu(in.EmuConfig(), nil)
	e.SetHomeState(true, map[int]float64{11: 18.0})

	// Sensor sends PUT h/<home>/z/11/overlay with 6280=21.00C, 6240=2, 6260=1.
	overlayBody := encodeTLVs([]TLV{
		{Tag: "6280", Val: []byte{0x08, 0x34}},
		{Tag: "6240", Val: []byte{0x02}},
		{Tag: "6260", Val: []byte{0x01}},
	})
	rawPut := synthDeviceUplink(t, in, su, CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x4301, Token: []byte{0x41},
		Path:    homePath(in, "z", "11", "overlay"),
		Payload: overlayBody,
	})
	putAns := synthAnswer(t, in, e.OnFrame(rawPut), 0x4301)
	if putAns.Code != "2.05" {
		t.Fatalf("overlay PUT code = %s, want 2.05", putAns.Code)
	}

	// Sensor re-reads h/<home>/z/11/s: must see 6240=2, 6260=1, 6280=2100.
	rawGet := synthDeviceUplink(t, in, su, CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x4302, Token: []byte{0x42},
		Path:    homePath(in, "z", "11", "s"),
		Options: []CoAPOption{{Num: 23, Val: []byte{0x03}}},
	})
	getAns := synthAnswer(t, in, e.OnFrame(rawGet), 0x4302)
	if getAns.Code != "2.05" {
		t.Fatalf("z/11/s GET code = %s, want 2.05", getAns.Code)
	}
	got := map[string]string{}
	for _, tlv := range parseTLVList(getAns.Payload) {
		got[tlv.Tag] = hex.EncodeToString(tlv.Val)
	}
	if got["6240"] != "02" || got["6260"] != "01" || got["6280"] != "0834" {
		t.Errorf("zone read after overlay = %v, want 6240=02 6260=01 6280=0834", got)
	}
}
