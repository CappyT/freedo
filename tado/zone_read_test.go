package tado

import (
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"testing"
)

// Zone read-path tests. They run on a synthetic installation, not on the
// recorded one: the assertions are wire shapes, so invented ids are enough.

const synthInstallYAML = `
version: 1
home_id: 7654321
network_key: "000102030405060708090a0b0c0d0e0f"
coordinator:
  eui: "1000000000c51b00"
  serial: IB0000000001
devices:
  - {kind: thermostat, eui: "2000000000c51b00", serial: RU0000000001, label: hall}
  - {kind: valve, eui: "3000000000c51b00", serial: VA0000000001, label: study}
zones:
  - {number: 0, name: water, kind: hotwater, members: [RU0000000001]}
  - {number: 4, name: study, kind: heating, circuit: 1, members: [VA0000000001], reporter: VA0000000001}
circuits:
  - {id: 1, name: house, heat_source: RU0000000001}
`

func synthInstall(t *testing.T) *Install {
	t.Helper()
	in, err := Parse([]byte(synthInstallYAML))
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// synthDeviceUplink is deviceUplink against the synthetic coordinator: a
// link-local request from dev to us.
func synthDeviceUplink(t *testing.T, in *Install, dev []byte, r CoAPReq) []byte {
	t.Helper()
	r.SrcEUI, r.DstEUI = dev, in.CoordinatorEUI()
	r.SrcPort, r.DstPort = coapPort, bridgeCoAPPort
	pkt, err := BuildCoAP(r)
	if err != nil {
		t.Fatal(err)
	}
	f, err := BuildData(in.NetworkKey(), 0x41, in.CoordinatorEUI(), dev[:5], WrapAppPayload(pkt, 700, nil))
	if err != nil {
		t.Fatal(err)
	}
	return f.Bytes()
}

// synthZoneUplink is a context-0 ("lid=") request from dev to the coordinator.
func synthZoneUplink(t *testing.T, in *Install, dev []byte, r CoAPReq) []byte {
	t.Helper()
	r.SrcEUI, r.DstEUI = dev, in.CoordinatorEUI()
	pkt, err := BuildContextCoAP(r)
	if err != nil {
		t.Fatal(err)
	}
	f, err := BuildData(in.NetworkKey(), 0x42, in.CoordinatorEUI(), dev[:5], WrapAppPayload(pkt, 800, nil))
	if err != nil {
		t.Fatal(err)
	}
	return f.Bytes()
}

// synthZonePush is a context-0 push from dev to a zone peer, which reaches the
// coordinator as an L2 hop only: the source stays elided and the destination is
// carried inline, as every zone publish in the capture does.
func synthZonePush(t *testing.T, in *Install, dev, peer []byte, r CoAPReq) []byte {
	t.Helper()
	r.SrcEUI, r.DstEUI = dev, peer
	msg, err := buildCoAPMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	out := append([]byte{0x7e, 0xf5, cidBothCtx0}, modifiedEUI64(peer)...)
	out = append(out, nhcUDPPortsIn)
	out = binary.BigEndian.AppendUint16(out, coapPort)
	out = binary.BigEndian.AppendUint16(out, coapPort)
	ck := udpChecksumV6(contextAddr(dev), contextAddr(peer), coapPort, coapPort, msg)
	pkt := append(binary.BigEndian.AppendUint16(out, ck), msg...)
	f, err := BuildData(in.NetworkKey(), 0x43, in.CoordinatorEUI(), dev[:5], WrapAppPayload(pkt, 900, nil))
	if err != nil {
		t.Fatal(err)
	}
	return f.Bytes()
}

// synthAnswer returns the emulator's CoAP answer carrying mid.
func synthAnswer(t *testing.T, in *Install, actions []Action, mid uint16) *CoAPMessage {
	t.Helper()
	for _, a := range actions {
		if a.Kind != ActData {
			continue
		}
		df, err := ParseData(in.NetworkKey(), a.Bytes)
		if err != nil {
			continue
		}
		if app := ParseApp(df.Plaintext); app != nil && app.CoAP != nil && app.CoAP.MID == mid {
			return app.CoAP
		}
	}
	t.Fatalf("no answer with mid=%04x among %v", mid, reasons(actions))
	return nil
}

func homePath(in *Install, rest ...string) []string {
	return append([]string{"h", strconv.FormatUint(uint64(in.HomeID), 10)}, rest...)
}

// TestDirectZoneReadMatchesCapturedShape: away, the thermostat's GET
// h/<home>/z/N/s gets the captured away block in the downlink order, whatever
// the cache holds. Byte-exact against "capture-analysis-20260827.md", section
// 2; the return to schedule matches the auto reads of the same capture.
func TestDirectZoneReadMatchesCapturedShape(t *testing.T) {
	in := synthInstall(t)
	ru, err := in.Resolve("RU0000000001")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		zone  int
		cache []TLV
		want  string
	}{{
		// The zone-plane echo the valve publishes, in its own tag order.
		name: "zone-plane echo",
		zone: 4,
		cache: []TLV{
			{Tag: "6280", Val: []byte{0x09, 0xc4}},
			{Tag: "6240", Val: []byte{0x02}},
			{Tag: "6260", Val: []byte{0x01}},
			{Tag: "61e0", Val: []byte{0x00}},
			{Tag: "6160", Val: []byte{0x02}},
			{Tag: "6180", Val: []byte{0x00}},
			{Tag: "6020", Val: []byte{0x01}},
			{Tag: "62e0", Val: []byte{0x00}},
		},
		want: "61600102" + "61800100" + "60200101" + "61e00100" +
			"62400102" + "62600101" + "62800209c4" + "62e00100",
	}, {
		// An auto read carries no 6260 and no 6280, and 62e0 reads back.
		name: "return to schedule",
		zone: 4,
		cache: []TLV{
			{Tag: "6240", Val: []byte{0x00}},
			{Tag: "6280", Val: []byte{0x09, 0x60}},
			{Tag: "61e0", Val: []byte{0x00}},
		},
		want: "61600102" + "61800100" + "60200101" + "61e00100" +
			"62400100" + "62e00100",
	}, {
		name: "hot water",
		zone: 0,
		cache: []TLV{
			{Tag: "6280", Val: []byte{0x12, 0xc0}},
			{Tag: "6240", Val: []byte{0x02}},
		},
		want: "61600102" + "61800100" + "60200102" + "61e00100" +
			"62400102" + "62600101" + "62800212c0" + "62e00100",
	}, {
		// 6200 and 6440 never reach a read, on any plane.
		name: "drops 6200 and 6440",
		zone: 4,
		cache: []TLV{
			{Tag: "6160", Val: []byte{0x02}},
			{Tag: "6200", Val: []byte{0x02, 0xbc}},
			{Tag: "6240", Val: []byte{0x02}},
			{Tag: "6260", Val: []byte{0x01}},
			{Tag: "6280", Val: []byte{0x01, 0xf4}},
			{Tag: "62e0", Val: []byte{0x00}},
			{Tag: "6440", Val: []byte{0x00, 0x01}},
		},
		want: "61600102" + "61800100" + "60200101" + "61e00100" +
			"62400102" + "62600101" + "62800201f4" + "62e00100",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEmu(in.EmuConfig(), nil)
			e.SetHomeState(false, map[int]float64{0: 55, 4: 20})
			e.zoneState[zoneKey(tc.zone, "z/s")] = encodeTLVs(tc.cache)
			raw := synthDeviceUplink(t, in, ru.WireEUI(), CoAPReq{
				Type: "CON", Code: "0.01", MID: 0x7101, Token: []byte{0x01},
				Path: homePath(in, "z", strconv.Itoa(tc.zone), "s"),
			})
			m := synthAnswer(t, in, e.OnFrame(raw), 0x7101)
			if got := hex.EncodeToString(m.Payload); m.Code != "2.05" || got != tc.want {
				t.Errorf("answer=%s body=%s, want 2.05 %s", m.Code, got, tc.want)
			}
		})
	}
}

// TestDirectZoneReadAtHome: at home the thermostat's read of a cached zone
// carries the home state and the program block in force, as the cloud answers
// in capture-app-20260907: 6160=1, 61e0=1 and 6200, then the overlay tags.
func TestDirectZoneReadAtHome(t *testing.T) {
	in := synthInstall(t)
	ru, err := in.Resolve("RU0000000001")
	if err != nil {
		t.Fatal(err)
	}
	manual := []TLV{
		{Tag: "6280", Val: []byte{0x01, 0xf4}},
		{Tag: "6240", Val: []byte{0x02}},
		{Tag: "6260", Val: []byte{0x01}},
		{Tag: "61e0", Val: []byte{0x00}},
		{Tag: "6160", Val: []byte{0x02}},
		{Tag: "6180", Val: []byte{0x00}},
		{Tag: "6020", Val: []byte{0x01}},
		{Tag: "62e0", Val: []byte{0x00}},
		{Tag: "6440", Val: []byte{0x00, 0x01}},
	}
	for _, tc := range []struct {
		name  string
		zone  int
		cache []TLV
		setup func(*EmuState)
		want  string
	}{{
		name:  "heating, manual overlay",
		zone:  4,
		cache: manual,
		setup: func(e *EmuState) { e.SetHomeState(true, map[int]float64{0: 55, 4: 20}) },
		want: "61600101" + "61800100" + "60200101" + "61e00101" + "62000207d0" +
			"62400102" + "62600101" + "62800201f4" + "62e00100",
	}, {
		name: "heating, auto",
		zone: 4,
		cache: []TLV{
			{Tag: "6200", Val: []byte{0x07, 0xd0}},
			{Tag: "6240", Val: []byte{0x00}},
			{Tag: "6260", Val: []byte{0x01}},
			{Tag: "6280", Val: []byte{0x07, 0xd0}},
			{Tag: "62e0", Val: []byte{0x00}},
		},
		setup: func(e *EmuState) { e.SetHomeState(true, map[int]float64{0: 55, 4: 20}) },
		want:  "61600101" + "61800100" + "60200101" + "61e00101" + "62000207d0" + "62400100" + "62e00100",
	}, {
		name: "hot water, manual overlay",
		zone: 0,
		cache: []TLV{
			{Tag: "6280", Val: []byte{0x12, 0xc0}},
			{Tag: "6240", Val: []byte{0x02}},
			{Tag: "6260", Val: []byte{0x01}},
			{Tag: "62e0", Val: []byte{0x00}},
		},
		setup: func(e *EmuState) { e.SetHomeState(true, map[int]float64{0: 55, 4: 20}) },
		want: "61600101" + "61800100" + "60200102" + "61e00101" + "620002157c" +
			"62400102" + "62600101" + "62800212c0" + "62e00100",
	}, {
		// The thermostat's own last 6200 may be the other zone's: the zone
		// cache is the fallback.
		name: "no program pushed, 6200 from the zone cache",
		zone: 4,
		cache: []TLV{
			{Tag: "6200", Val: []byte{0x08, 0x02}},
			{Tag: "6240", Val: []byte{0x00}},
		},
		setup: func(e *EmuState) { e.SetHomeState(true, nil) },
		want:  "61600101" + "61800100" + "60200101" + "61e00101" + "6200020802" + "62400100" + "62e00100",
	}, {
		name:  "no program known",
		zone:  4,
		cache: []TLV{{Tag: "6240", Val: []byte{0x00}}},
		setup: func(e *EmuState) { e.SetHomeState(true, nil) },
		want:  "61600101" + "61800100" + "60200101" + "61e00101" + "62400100" + "62e00100",
	}, {
		name:  "program block off",
		zone:  4,
		cache: []TLV{{Tag: "6240", Val: []byte{0x00}}},
		setup: func(e *EmuState) {
			e.SetHomeState(true, map[int]float64{4: 5})
			e.SetScheduleOff(map[int]bool{4: true})
		},
		want: "61600101" + "61800100" + "60200101" + "61e00100" + "62400100" + "62e00100",
	}, {
		name:  "window held open",
		zone:  4,
		cache: []TLV{{Tag: "6240", Val: []byte{0x00}}, {Tag: "62e0", Val: []byte{0x00}}},
		setup: func(e *EmuState) {
			e.SetHomeState(true, map[int]float64{4: 20})
			e.SetOpenWindows(map[int]bool{4: true})
		},
		want: "61600101" + "61800100" + "60200101" + "61e00101" + "62000207d0" + "62400100" + "62e00101",
	}, {
		// With no home state pushed the thermostat keeps its own.
		name:  "no home state pushed",
		zone:  4,
		cache: manual,
		setup: func(*EmuState) {},
		want:  "61800100" + "60200101" + "62400102" + "62600101" + "62800201f4" + "62e00100",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEmu(in.EmuConfig(), nil)
			tc.setup(e)
			e.zoneState[zoneKey(tc.zone, "z/s")] = encodeTLVs(tc.cache)
			raw := synthDeviceUplink(t, in, ru.WireEUI(), CoAPReq{
				Type: "CON", Code: "0.01", MID: 0x7102, Token: []byte{0x04},
				Path: homePath(in, "z", strconv.Itoa(tc.zone), "s"),
			})
			m := synthAnswer(t, in, e.OnFrame(raw), 0x7102)
			if got := hex.EncodeToString(m.Payload); m.Code != "2.05" || got != tc.want {
				t.Errorf("answer=%s body=%s, want 2.05 %s", m.Code, got, tc.want)
			}
		})
	}
}

// TestDirectZoneReadWithoutCache: a zone with no cached block reads the home
// state and the program block in force, then 6240=0 and 62e0=0, as every cloud
// read of capture-app-20260907 carries both: a thermostat that restarted holds
// no 6240 and skips the zone until one arrives. 6200 stays out when no program
// block is known. With no home state pushed the read stays an empty 2.05, which
// the device applies as no change.
func TestDirectZoneReadWithoutCache(t *testing.T) {
	in := synthInstall(t)
	for _, tc := range []struct {
		name  string
		dev   string
		zone  int
		setup func(*EmuState)
		want  string
	}{{
		name:  "no home state pushed",
		dev:   "VA0000000001",
		zone:  4,
		setup: func(*EmuState) {},
		want:  "",
	}, {
		name:  "home with the program block",
		dev:   "VA0000000001",
		zone:  4,
		setup: func(e *EmuState) { e.SetHomeState(true, map[int]float64{4: 20.5}) },
		want:  "61600101" + "61800100" + "60200101" + "61e00101" + "6200020802" + "62400100" + "62e00100",
	}, {
		name:  "home with no program block known",
		dev:   "VA0000000001",
		zone:  4,
		setup: func(e *EmuState) { e.SetHomeState(true, nil) },
		want:  "61600101" + "61800100" + "60200101" + "61e00101" + "62400100" + "62e00100",
	}, {
		name: "home with the program block off",
		dev:  "VA0000000001",
		zone: 4,
		setup: func(e *EmuState) {
			e.SetHomeState(true, map[int]float64{4: 5})
			e.SetScheduleOff(map[int]bool{4: true})
		},
		want: "61600101" + "61800100" + "60200101" + "61e00100" + "62400100" + "62e00100",
	}, {
		name:  "away",
		dev:   "VA0000000001",
		zone:  4,
		setup: func(e *EmuState) { e.SetHomeState(false, map[int]float64{4: 20.5}) },
		want:  "61600102" + "61800100" + "60200101" + "61e00100" + "62400100" + "62e00100",
	}, {
		name: "window held open",
		dev:  "VA0000000001",
		zone: 4,
		setup: func(e *EmuState) {
			e.SetHomeState(true, map[int]float64{4: 20.5})
			e.SetOpenWindows(map[int]bool{4: true})
		},
		want: "61600101" + "61800100" + "60200101" + "61e00101" + "6200020802" + "62400100" + "62e00101",
	}, {
		// See TestHotWaterReadWithoutProgramKeepsHotWaterOn.
		name:  "hot water",
		dev:   "RU0000000001",
		zone:  0,
		setup: func(e *EmuState) { e.SetHomeState(true, map[int]float64{0: 55}) },
		want:  "61600101" + "61800100" + "60200102" + "61e00101" + "620002157c" + "62400100" + "62e00100",
	}, {
		name:  "hot water, no home state pushed",
		dev:   "RU0000000001",
		zone:  0,
		setup: func(*EmuState) {},
		want:  "",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			dev, err := in.Resolve(tc.dev)
			if err != nil {
				t.Fatal(err)
			}
			e := NewEmu(in.EmuConfig(), nil)
			tc.setup(e)
			raw := synthDeviceUplink(t, in, dev.WireEUI(), CoAPReq{
				Type: "CON", Code: "0.01", MID: 0x7111, Token: []byte{0x02},
				Path: homePath(in, "z", strconv.Itoa(tc.zone), "s"),
			})
			m := synthAnswer(t, in, e.OnFrame(raw), 0x7111)
			if got := hex.EncodeToString(m.Payload); m.Code != "2.05" || got != tc.want {
				t.Errorf("answer=%s body=%s, want 2.05 %s", m.Code, got, tc.want)
			}
		})
	}
}

// thermostatDHWEnable models zone_state_timer_cb (thermostat 0x2d70c) for the
// hot-water zone: the value it publishes on 40e0, which the translator stores
// as the DHW enable (cfg 0x19a). published is false when the callback skips the
// zone because 6240 was never set, so the enable keeps its reset value.
func thermostatDHWEnable(state map[string]byte) (enable byte, published bool) {
	const unset = 0x7f
	get := func(tag string) byte {
		if v, ok := state[tag]; ok {
			return v
		}
		return unset
	}
	if get("62e0") != unset && get("62e0") != 0 {
		return 0, true
	}
	mode := get("6240")
	if mode == unset {
		return 0, false
	}
	flag := "61e0"
	if mode != overlayAuto {
		flag = "6260"
	}
	if get(flag) == unset {
		return 0, false
	}
	return get(flag), true
}

// TestHotWaterReadWithoutProgramKeepsHotWaterOn: at home, with no program for
// the hot-water zone and nothing cached for it, the thermostat's read of
// h/<home>/z/0/s must set 6240, as every cloud read of zone 0 does
// (capture-app-20260907). A thermostat that restarted holds no 6240, and
// zone_state_timer_cb then skips the zone: the DHW enable never goes out and
// the boiler keeps hot water off.
func TestHotWaterReadWithoutProgramKeepsHotWaterOn(t *testing.T) {
	in := synthInstall(t)
	ru, err := in.Resolve("RU0000000001")
	if err != nil {
		t.Fatal(err)
	}
	e := NewEmu(in.EmuConfig(), nil)
	e.SetHomeState(true, map[int]float64{4: 20})
	raw := synthDeviceUplink(t, in, ru.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7114, Token: []byte{0x06},
		Path: homePath(in, "z", "0", "s"),
	})
	m := synthAnswer(t, in, e.OnFrame(raw), 0x7114)
	want := "61600101" + "61800100" + "60200102" + "61e00101" + "62400100" + "62e00100"
	if got := hex.EncodeToString(m.Payload); m.Code != "2.05" || got != want {
		t.Errorf("answer=%s body=%s, want 2.05 %s", m.Code, got, want)
	}
	state := make(map[string]byte)
	for _, tlv := range parseTLVList(m.Payload) {
		if len(tlv.Val) == 1 {
			state[tlv.Tag] = tlv.Val[0]
		}
	}
	if enable, published := thermostatDHWEnable(state); !published || enable != 1 {
		t.Errorf("restarted thermostat after the read: DHW enable=%d published=%v, want 1 published", enable, published)
	}
}

// TestHotWaterFollowsAway: hot water follows the home state as the cloud does.
// Away, a zone on its program reads and is rewritten with 6160=2, 61e0=0 and
// 6240=0, so zone_state_timer_cb takes the enable from 61e0 and turns hot water
// off. A manual overlay keeps 6240=2 6260=1 6280 under the away head, as the
// cloud away read of zone 0 in capture-analysis-20260827 shows, so the enable
// comes from 6260 and hot water stays on at the overlay setpoint.
func TestHotWaterFollowsAway(t *testing.T) {
	in := synthInstall(t)
	ru, err := in.Resolve("RU0000000001")
	if err != nil {
		t.Fatal(err)
	}
	overlay48 := []TLV{
		{Tag: "6240", Val: []byte{0x02}},
		{Tag: "6260", Val: []byte{0x01}},
		{Tag: "6280", Val: []byte{0x12, 0xc0}},
		{Tag: "62e0", Val: []byte{0x00}},
	}
	program := []TLV{{Tag: "6240", Val: []byte{0x00}}, {Tag: "62e0", Val: []byte{0x00}}}
	for _, tc := range []struct {
		name       string
		home       bool
		cache      []TLV
		want       string
		wantEnable byte
	}{{
		name: "away, manual overlay", cache: overlay48,
		want: "61600102" + "61800100" + "60200102" + "61e00100" +
			"62400102" + "62600101" + "62800212c0" + "62e00100",
		wantEnable: 1,
	}, {
		name: "away, program", cache: program,
		want:       "61600102" + "61800100" + "60200102" + "61e00100" + "62400100" + "62e00100",
		wantEnable: 0,
	}, {
		name: "home, manual overlay", home: true, cache: overlay48,
		want: "61600101" + "61800100" + "60200102" + "61e00101" + "620002157c" +
			"62400102" + "62600101" + "62800212c0" + "62e00100",
		wantEnable: 1,
	}, {
		name: "home, program", home: true, cache: program,
		want:       "61600101" + "61800100" + "60200102" + "61e00101" + "620002157c" + "62400100" + "62e00100",
		wantEnable: 1,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEmu(in.EmuConfig(), nil)
			e.SetHomeState(tc.home, map[int]float64{0: 55, 4: 20})
			e.zoneState[zoneKey(0, "z/s")] = encodeTLVs(tc.cache)
			raw := synthDeviceUplink(t, in, ru.WireEUI(), CoAPReq{
				Type: "CON", Code: "0.01", MID: 0x7115, Token: []byte{0x07},
				Path: homePath(in, "z", "0", "s"),
			})
			read := synthAnswer(t, in, e.OnFrame(raw), 0x7115)
			if err := e.EnqueueZoneRewrite("RU0000000001", 0); err != nil {
				t.Fatal(err)
			}
			q := e.queue[hexEUI(ru.WireEUI())]
			rewrite := parseBuilt(t, q[len(q)-1].coap)
			for what, body := range map[string][]byte{"read": read.Payload, "rewrite": rewrite.Payload} {
				if got := hex.EncodeToString(body); got != tc.want {
					t.Errorf("%s body=%s, want %s", what, got, tc.want)
				}
				state := make(map[string]byte)
				for _, tlv := range parseTLVList(body) {
					if len(tlv.Val) == 1 {
						state[tlv.Tag] = tlv.Val[0]
					}
				}
				if enable, published := thermostatDHWEnable(state); !published || enable != tc.wantEnable {
					t.Errorf("%s: DHW enable=%d published=%v, want %d published", what, enable, published, tc.wantEnable)
				}
			}
		})
	}
}

// TestDirectZoneReadValveRebuilt: a valve reading a cached zone gets the block
// the thermostat gets, as the cloud answers every valve read of
// capture-app-20260907: the pushed home state and program block, the overlay
// tags of the zone, no 6440. The cache head is stale on purpose.
func TestDirectZoneReadValveRebuilt(t *testing.T) {
	in := synthInstall(t)
	va, err := in.Resolve("VA0000000001")
	if err != nil {
		t.Fatal(err)
	}
	manual := []TLV{
		{Tag: "6280", Val: []byte{0x01, 0xf4}},
		{Tag: "6240", Val: []byte{0x02}},
		{Tag: "6260", Val: []byte{0x01}},
		{Tag: "6200", Val: []byte{0x09, 0xc4}},
		{Tag: "61e0", Val: []byte{0x00}},
		{Tag: "6160", Val: []byte{0x02}},
		{Tag: "6180", Val: []byte{0x00}},
		{Tag: "6020", Val: []byte{0x01}},
		{Tag: "62e0", Val: []byte{0x00}},
		{Tag: "6440", Val: []byte{0x00, 0x01}},
	}
	// A return to schedule leaves the last overlay's 6260 and 6280 behind.
	auto := []TLV{
		{Tag: "6280", Val: []byte{0x09, 0xc4}},
		{Tag: "6240", Val: []byte{0x00}},
		{Tag: "6260", Val: []byte{0x01}},
		{Tag: "6200", Val: []byte{0x09, 0xc4}},
		{Tag: "61e0", Val: []byte{0x00}},
		{Tag: "6160", Val: []byte{0x02}},
		{Tag: "6180", Val: []byte{0x00}},
		{Tag: "6020", Val: []byte{0x01}},
		{Tag: "62e0", Val: []byte{0x00}},
	}
	for _, tc := range []struct {
		name  string
		cache []TLV
		home  bool
		want  string
	}{{
		name: "home, overlay", cache: manual, home: true,
		want: "61600101" + "61800100" + "60200101" + "61e00101" + "6200020802" +
			"62400102" + "62600101" + "62800201f4" + "62e00100",
	}, {
		name: "home, no overlay", cache: auto, home: true,
		want: "61600101" + "61800100" + "60200101" + "61e00101" + "6200020802" + "62400100" + "62e00100",
	}, {
		name: "away, overlay", cache: manual, home: false,
		want: "61600102" + "61800100" + "60200101" + "61e00100" +
			"62400102" + "62600101" + "62800201f4" + "62e00100",
	}, {
		name: "away, no overlay", cache: auto, home: false,
		want: "61600102" + "61800100" + "60200101" + "61e00100" + "62400100" + "62e00100",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEmu(in.EmuConfig(), nil)
			e.SetHomeState(tc.home, map[int]float64{4: 20.5})
			e.zoneState[zoneKey(4, "z/s")] = encodeTLVs(tc.cache)
			raw := synthDeviceUplink(t, in, va.WireEUI(), CoAPReq{
				Type: "CON", Code: "0.01", MID: 0x7112, Token: []byte{0x03},
				Path: homePath(in, "z", "4", "s"),
			})
			m := synthAnswer(t, in, e.OnFrame(raw), 0x7112)
			if got := hex.EncodeToString(m.Payload); m.Code != "2.05" || got != tc.want {
				t.Errorf("answer=%s body=%s, want 2.05 %s", m.Code, got, tc.want)
			}
		})
	}
}

// TestDirectZoneReadWithoutCacheThermostat: the thermostat sits in two zones,
// so its own last 6200 may be the other zone's. An uncached read takes 6200
// from the program pushed for the zone read, and leaves it out otherwise. It
// carries 6240=0 and 62e0=0 either way, so the restarted thermostat does not
// skip the zone.
func TestDirectZoneReadWithoutCacheThermostat(t *testing.T) {
	in := synthInstall(t)
	ru, err := in.Resolve("RU0000000001")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		schedC map[int]float64
		want   string
	}{{
		name:   "program pushed",
		schedC: map[int]float64{0: 55, 4: 20.5},
		want:   "61600101" + "61800100" + "60200101" + "61e00101" + "6200020802" + "62400100" + "62e00100",
	}, {
		name:   "no program pushed",
		schedC: map[int]float64{0: 55},
		want:   "61600101" + "61800100" + "60200101" + "61e00101" + "62400100" + "62e00100",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEmu(in.EmuConfig(), nil)
			e.SetHomeState(true, tc.schedC)
			e.nodes[hexEUI(ru.WireEUI())] = &NodeState{LastSchedC: 55}
			raw := synthDeviceUplink(t, in, ru.WireEUI(), CoAPReq{
				Type: "CON", Code: "0.01", MID: 0x7113, Token: []byte{0x05},
				Path: homePath(in, "z", "4", "s"),
			})
			m := synthAnswer(t, in, e.OnFrame(raw), 0x7113)
			if got := hex.EncodeToString(m.Payload); m.Code != "2.05" || got != tc.want {
				t.Errorf("answer=%s body=%s, want 2.05 %s", m.Code, got, tc.want)
			}
			// zone_state_timer_cb runs the same 6240 test on every zone.
			state := make(map[string]byte)
			for _, tlv := range parseTLVList(m.Payload) {
				if len(tlv.Val) == 1 {
					state[tlv.Tag] = tlv.Val[0]
				}
			}
			if enable, published := thermostatDHWEnable(state); !published || enable != 1 {
				t.Errorf("restarted thermostat after the read: enable=%d published=%v, want 1 published", enable, published)
			}
		})
	}
}

// TestZoneOpenWindowAnsweredWithContent: a write of h/<home>/z/N/ow gets a 2.05
// with no body, not the 2.04 every other write gets.
func TestZoneOpenWindowAnsweredWithContent(t *testing.T) {
	in := synthInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	ru, err := in.Resolve("RU0000000001")
	if err != nil {
		t.Fatal(err)
	}
	raw := synthDeviceUplink(t, in, ru.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7201, Token: []byte{0x11},
		Path: homePath(in, "z", "4", "ow"), Options: []CoAPOption{optOctets},
	})
	m := synthAnswer(t, in, e.OnFrame(raw), 0x7201)
	if m.Code != "2.05" || len(m.Payload) != 0 {
		t.Errorf("answer=%s payload=%x, want 2.05 with no body", m.Code, m.Payload)
	}

	// The neighbouring write path keeps its 2.04.
	raw = synthDeviceUplink(t, in, ru.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7202, Token: []byte{0x12},
		Path: homePath(in, "z", "4", "act"), Options: []CoAPOption{optOctets},
		Payload: encodeTLVs([]TLV{{Tag: "40a0", Val: []byte{0}}}),
	})
	if m := synthAnswer(t, in, e.OnFrame(raw), 0x7202); m.Code != "2.04" {
		t.Errorf("act answer=%s, want 2.04", m.Code)
	}
}

// TestZoneOpenWindowReportTaken: the bodiless z/ow report reaches the daemon
// once, with the zone of the path and the reporter; a GET of it does not.
func TestZoneOpenWindowReportTaken(t *testing.T) {
	in := synthInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	va, err := in.Resolve("VA0000000001")
	if err != nil {
		t.Fatal(err)
	}
	raw := synthDeviceUplink(t, in, va.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7301, Path: homePath(in, "z", "4", "ow"),
		Options: []CoAPOption{optOctets},
	})
	if m := synthAnswer(t, in, e.OnFrame(raw), 0x7301); m.Code != "2.05" {
		t.Fatalf("answer=%s, want 2.05", m.Code)
	}
	want := OpenWindowReport{Zone: 4, Device: hex.EncodeToString(va.WireEUI())}
	if got := e.TakeOpenWindowReports(); len(got) != 1 || got[0] != want {
		t.Fatalf("reports=%+v, want [%+v]", got, want)
	}
	if got := e.TakeOpenWindowReports(); len(got) != 0 {
		t.Errorf("a second take returned %+v, want none", got)
	}

	raw = synthDeviceUplink(t, in, va.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7302, Path: homePath(in, "z", "4", "ow"),
	})
	e.OnFrame(raw)
	if got := e.TakeOpenWindowReports(); len(got) != 0 {
		t.Errorf("a GET was taken as a report: %+v", got)
	}
}

// TestHeldOpenWindowStampsZoneWrites: while the daemon holds a zone open, a
// setpoint write or a zone rewrite to it carries 62e0 = 1.
func TestHeldOpenWindowStampsZoneWrites(t *testing.T) {
	in := synthInstall(t)
	va, err := in.Resolve("VA0000000001")
	if err != nil {
		t.Fatal(err)
	}
	target := hex.EncodeToString(va.WireEUI())
	for _, tc := range []struct {
		name    string
		enqueue func(*EmuState) error
	}{
		{"setpoint", func(e *EmuState) error {
			return e.EnqueueSetpoint(target, SetpointParams{ID: 4, Setpoint: 21, Overlay: 2})
		}},
		{"rewrite", func(e *EmuState) error { return e.EnqueueZoneRewrite(target, 4) }},
	} {
		for _, open := range []bool{false, true} {
			e := NewEmu(in.EmuConfig(), nil)
			e.SetOpenWindows(map[int]bool{4: open})
			if err := tc.enqueue(e); err != nil {
				t.Fatal(err)
			}
			actions, ok, err := e.NextQueuedDownlink(target)
			if err != nil || !ok {
				t.Fatalf("%s: no downlink (err=%v)", tc.name, err)
			}
			df, err := ParseData(in.NetworkKey(), actions[0].Bytes)
			if err != nil {
				t.Fatal(err)
			}
			want := []byte{0x00}
			if open {
				want = []byte{0x01}
			}
			var got []byte
			for _, tlv := range ParseApp(df.Plaintext).TLVs {
				if tlv.Tag == "62e0" {
					got = tlv.Val
				}
			}
			if hex.EncodeToString(got) != hex.EncodeToString(want) {
				t.Errorf("%s with the zone open=%t: 62e0=%x, want %x", tc.name, open, got, want)
			}
		}
	}
}

// TestZoneCPEReadFromCache: the heat percent the thermostat pushes as a NON
// aimed at the zone member is cached, and a GET z/cpe?lid=N reads it back.
func TestZoneCPEReadFromCache(t *testing.T) {
	in := synthInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	ru, err := in.Resolve("RU0000000001")
	if err != nil {
		t.Fatal(err)
	}
	va, err := in.Resolve("VA0000000001")
	if err != nil {
		t.Fatal(err)
	}

	// An unknown zone still gets the bare 2.05.
	cold := synthZoneUplink(t, in, va.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7301, Token: []byte{0x21},
		Path: []string{"z", "cpe"}, Query: []string{"lid=4"},
	})
	if m := synthAnswer(t, in, e.OnFrame(cold), 0x7301); m.Code != "2.05" || len(m.Payload) != 0 {
		t.Errorf("cold answer=%s payload=%x, want an empty 2.05", m.Code, m.Payload)
	}

	push := synthZonePush(t, in, ru.WireEUI(), va.WireEUI(), CoAPReq{
		Type: "NON", Code: "0.03", MID: 0x7302, Token: []byte{},
		Path: []string{"z", "cpe"}, Query: []string{"lid=4"},
		Options: []CoAPOption{optOctets},
		Payload: encodeTLVs([]TLV{{Tag: "4160", Val: []byte{100}}}),
	})
	e.OnFrame(push)

	get := synthZoneUplink(t, in, va.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7303, Token: []byte{0x22},
		Path: []string{"z", "cpe"}, Query: []string{"lid=4"},
	})
	m := synthAnswer(t, in, e.OnFrame(get), 0x7303)
	if got := hex.EncodeToString(m.Payload); m.Code != "2.05" || got != "41600164" {
		t.Errorf("answer=%s body=%s, want 2.05 41600164", m.Code, got)
	}
	if _, ok := m.Option(14); ok {
		t.Error("z/cpe answer carries a Max-Age option, the captured push carries none")
	}
}

// TestZoneMeasureReadFromCache: a GET z/p?lid=N reads back the measures the zone
// member pushed, in the pushed tag order, with the captured Max-Age of 1800 s.
func TestZoneMeasureReadFromCache(t *testing.T) {
	in := synthInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	ru, err := in.Resolve("RU0000000001")
	if err != nil {
		t.Fatal(err)
	}
	va, err := in.Resolve("VA0000000001")
	if err != nil {
		t.Fatal(err)
	}
	push := synthZoneUplink(t, in, va.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.03", MID: 0x7401, Token: []byte{},
		Path: []string{"z", "p"}, Query: []string{"lid=4"},
		Options: []CoAPOption{optOctets, optZoneMeasureAge},
		Payload: encodeTLVs([]TLV{
			{Tag: "4060", Val: []byte{0x0a, 0xd2}},
			{Tag: "40a0", Val: []byte{0x64}},
			{Tag: "0135", Val: []byte{0x18, 0x7e}},
		}),
	})
	e.OnFrame(push)

	get := synthZoneUplink(t, in, ru.WireEUI(), CoAPReq{
		Type: "CON", Code: "0.01", MID: 0x7402, Token: []byte{0x23},
		Path: []string{"z", "p"}, Query: []string{"lid=4"},
	})
	m := synthAnswer(t, in, e.OnFrame(get), 0x7402)
	want := "4060020ad2" + "40a00164" + "013502187e"
	if got := hex.EncodeToString(m.Payload); m.Code != "2.05" || got != want {
		t.Errorf("answer=%s body=%s, want 2.05 %s", m.Code, got, want)
	}
	age, ok := m.Option(14)
	if !ok || decodeCoAPUint(age.Val) != 1800 {
		t.Errorf("Max-Age=%x present=%v, want 1800", age.Val, ok)
	}
}
