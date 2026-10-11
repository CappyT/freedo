package tado

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// The tests in this file pin the options of the answers the commercial bridge
// gives to a device GET, as ota-watch-20260923-2008.log,
// ota-watch-20260924-1410.log, capture-app-20260907.log and
// coap_sniff_1786619225.log record them.

// shapeGET sends a device GET of path with the captured request options:
// Content-Format 42, the ETag when set, the Block2 when set.
func shapeGET(t *testing.T, in *Install, e *EmuState, dev []byte, path []string, etag, block2 []byte, mid uint16) *CoAPMessage {
	t.Helper()
	opts := []CoAPOption{optOctets}
	if etag != nil {
		opts = append(opts, CoAPOption{Num: 4, Val: etag})
	}
	if block2 != nil {
		opts = append(opts, CoAPOption{Num: 23, Val: block2})
	}
	raw := synthDeviceUplink(t, in, dev, CoAPReq{
		Type: "CON", Code: "0.01", MID: mid, Token: []byte{},
		Path: path, Options: opts,
	})
	return synthAnswer(t, in, e.OnFrame(raw), mid)
}

func optionNums(m *CoAPMessage) []int {
	var out []int
	for _, o := range m.Options {
		out = append(out, o.Num)
	}
	return out
}

// wantOptions fails unless m carries exactly the options nums, in this order.
func wantOptions(t *testing.T, what string, m *CoAPMessage, nums ...int) {
	t.Helper()
	got := optionNums(m)
	if len(got) != len(nums) {
		t.Errorf("%s: %s options %v, want %v", what, m.Code, got, nums)
		return
	}
	for i := range got {
		if got[i] != nums[i] {
			t.Errorf("%s: %s options %v, want %v", what, m.Code, got, nums)
			return
		}
	}
}

// TestGetAnswersMatchCapturedOptions: a 2.05 to a GET carries ETag and Block2
// and no Content-Format; a 2.03 echoes the ETag it validates and the request's
// Block2. Captured: d/config and z/N/config 2.05 and 2.03, c/N/config 2.05,
// d/lock 2.05 (etag + blk2=0/0/128 + child_lock=0).
func TestGetAnswersMatchCapturedOptions(t *testing.T) {
	in := synthInstall(t)
	limit := 48
	in.Options.FlowLimitC = &limit
	e := NewEmu(in.EmuConfig(), nil)
	ru, va := synthDevices(t, in)
	e.OnFrame(moduleReport(t, in, ru, 71, 0x7000))

	for i, tc := range []struct {
		name  string
		dev   []byte
		path  []string
		block []byte
	}{
		// 0/0/128 is the Block2 every device GET carries. The first 128-byte
		// block of a zone config document takes two 6LoWPAN fragments, so that
		// read asks for 64-byte blocks.
		{"valve d/config", va, []string{"d", "VA0000000001", "config"}, []byte{0x03}},
		{"thermostat d/config", ru, []string{"d", "RU0000000001", "config"}, []byte{0x03}},
		{"z/4/config", ru, homePath(in, "z", "4", "config"), []byte{0x02}},
		{"c/1/config", ru, homePath(in, "c", "1", "config"), []byte{0x03}},
		{"d/lock", va, []string{"d", "VA0000000001", "lock"}, []byte{0x03}},
	} {
		mid := uint16(0x7100 + 2*i)
		block := tc.block
		full := shapeGET(t, in, e, tc.dev, tc.path, nil, block, mid)
		if full.Code != "2.05" || len(full.Payload) == 0 {
			t.Errorf("%s: %s with %d body bytes, want 2.05 with a body", tc.name, full.Code, len(full.Payload))
			continue
		}
		wantOptions(t, tc.name, full, 4, 23)
		etag := optionValue(full, 4)
		if len(etag) != 8 {
			t.Errorf("%s: ETag %x, want 8 bytes", tc.name, etag)
		}
		valid := shapeGET(t, in, e, tc.dev, tc.path, etag, block, mid+1)
		if valid.Code != "2.03" || len(valid.Payload) != 0 {
			t.Errorf("%s: conditional GET got %s with %d body bytes, want an empty 2.03", tc.name, valid.Code, len(valid.Payload))
			continue
		}
		wantOptions(t, tc.name+" 2.03", valid, 4, 23)
		if got := optionValue(valid, 4); !bytes.Equal(got, etag) {
			t.Errorf("%s: 2.03 ETag %x, want the validated %x", tc.name, got, etag)
		}
		if got := optionValue(valid, 23); !bytes.Equal(got, block) {
			t.Errorf("%s: 2.03 Block2 %x, want the request's %x", tc.name, got, block)
		}
	}
}

// TestChildLockGetMatchesCapture: GET d/lock is answered as the captured one:
// 2.05, ETag, Block2 0/0/128, body 0290 = 00. The ETag follows the body, so
// two valves with the same lock state read the same ETag, as in the capture.
func TestChildLockGetMatchesCapture(t *testing.T) {
	in := synthInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	_, va := synthDevices(t, in)
	m := shapeGET(t, in, e, va, []string{"d", "VA0000000001", "lock"}, nil, []byte{0x03}, 0x7200)
	if m.Code != "2.05" {
		t.Fatalf("d/lock answer %s, want 2.05", m.Code)
	}
	if got := hex.EncodeToString(m.Payload); got != "02900100" {
		t.Errorf("body %s, want 02900100", got)
	}
	if got, want := optionValue(m, 4), configEtag(m.Payload); !bytes.Equal(got, want) {
		t.Errorf("ETag %x, want %x derived from the body", got, want)
	}
	if got := optionValue(m, 23); !bytes.Equal(got, []byte{0x03}) {
		t.Errorf("Block2 %x, want 03", got)
	}
}

// TestAuthTokenAnswerCarriesNoContentFormat: the captured auth/token 2.05
// carries the session TLVs and no option at all.
func TestAuthTokenAnswerCarriesNoContentFormat(t *testing.T) {
	dev := mustHex("02a6cefa07c51b00")
	e, _, _ := newTestEmu(t)
	m := servedAnswer(t, e.OnFrame(deviceUplink(t, dev, CoAPReq{
		Type: "CON", Code: "0.02", MID: 0x7300, Token: []byte{},
		Path: []string{"auth", "token"}, Options: []CoAPOption{optOctets},
		Payload: mustHex("0102"),
	})), 0x7300)
	if m.Code != "2.05" || len(m.Payload) == 0 {
		t.Fatalf("auth/token answer %s with %d body bytes, want 2.05 with the session", m.Code, len(m.Payload))
	}
	wantOptions(t, "auth/token", m)
}

// TestMissingZoneConfigGetIsNotFound: a GET of the config of a zone the
// installation does not hold gets 4.04 with the request's Block2 and nothing
// else (coap_sniff_1786619225.log, line 9504: "4.04 blk2=0/0/128").
func TestMissingZoneConfigGetIsNotFound(t *testing.T) {
	in := synthInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	ru, _ := synthDevices(t, in)
	for i, block := range [][]byte{{0x03}, nil} {
		mid := uint16(0x7400 + i)
		m := shapeGET(t, in, e, ru, homePath(in, "z", "17", "config"), nil, block, mid)
		if m.Code != "4.04" || len(m.Payload) != 0 {
			t.Errorf("block2=%x: answer %s with %d body bytes, want an empty 4.04", block, m.Code, len(m.Payload))
			continue
		}
		if block == nil {
			wantOptions(t, "4.04 without Block2", m)
			continue
		}
		wantOptions(t, "4.04", m, 23)
		if got := optionValue(m, 23); !bytes.Equal(got, block) {
			t.Errorf("4.04 Block2 %x, want the request's %x", got, block)
		}
	}
}

// TestThermostatConfigDocumentMatchesCapture: the wall thermostat's d/config
// document carries no 021a and no 0149, and 0158 = 0000 as its last record
// (capture-app-20260907.log line 70943, ota-watch-20260924-1410.log line 5199).
// The valve document keeps 021a, 0149 and 0158 = 0200, and its ETag.
func TestThermostatConfigDocumentMatchesCapture(t *testing.T) {
	in := synthInstall(t)
	e := NewEmu(in.EmuConfig(), nil)
	ru, va := synthDevices(t, in)
	e.OnFrame(moduleReport(t, in, ru, 71, 0x7500))

	ruDoc := parseTLVList(shapeGET(t, in, e, ru, []string{"d", "RU0000000001", "config"}, nil, []byte{0x03}, 0x7501).Payload)
	for _, tag := range []string{"021a", "0149"} {
		if v := tlvValue(ruDoc, tag); v != "" {
			t.Errorf("thermostat document carries %s=%s", tag, v)
		}
	}
	if len(ruDoc) == 0 || ruDoc[len(ruDoc)-1].Tag != "0158" || hex.EncodeToString(ruDoc[len(ruDoc)-1].Val) != "0000" {
		t.Errorf("thermostat document %v, want 0158=0000 last", ruDoc)
	}

	valve := shapeGET(t, in, e, va, []string{"d", "VA0000000001", "config"}, nil, []byte{0x03}, 0x7502)
	vaDoc := parseTLVList(valve.Payload)
	for tag, want := range map[string]string{"021a": "0000", "0149": "00", "0158": "0200"} {
		if got := tlvValue(vaDoc, tag); got != want {
			t.Errorf("valve document %s=%q, want %s", tag, got, want)
		}
	}
	// The ETag the code served before the thermostat document changed shape.
	if got := hex.EncodeToString(optionValue(valve, 4)); got != "22ddd1f6b3875d5c" {
		t.Errorf("valve ETag %s moved", got)
	}
}

// TestRestoredThermostatDocumentTakesCapturedShape: a stored thermostat
// document in the valve shape is served in the captured shape after a
// restore, under a new ETag: the thermostat re-reads it once.
func TestRestoredThermostatDocumentTakesCapturedShape(t *testing.T) {
	in := synthInstall(t)
	ru, _ := synthDevices(t, in)
	module := uint16(71)
	stored := encodeTLVs([]TLV{
		{Tag: "0143", Val: []byte{0x00}}, {Tag: "015d", Val: be16(module)},
		{Tag: "015c", Val: []byte{0x00, 0x74, 0xcb, 0xb1}}, {Tag: "02b3", Val: []byte{0x00}},
		{Tag: "021a", Val: []byte{0x00, 0x00}}, {Tag: "0149", Val: []byte{0x00}},
		{Tag: "0158", Val: []byte{0x02, 0x00}}, {Tag: "015a", Val: mustHex("1122334455667788")},
	})
	state := NewEmu(in.EmuConfig(), nil).ExportDesiredState()
	state.Config[hexEUI(ru)] = DesiredConfigDocument{Body: stored, ETag: mustHex("1122334455667788")}
	e := NewEmu(in.EmuConfig(), nil)
	if err := e.RestoreDesiredState(state); err != nil {
		t.Fatal(err)
	}
	snap, ok := e.cfgDoc[hexEUI(ru)]
	if !ok {
		t.Fatal("the restored thermostat document is gone")
	}
	doc := parseTLVList(snap.body)
	if tlvValue(doc, "021a") != "" || tlvValue(doc, "0149") != "" || tlvValue(doc, "0158") != "0000" {
		t.Errorf("restored document %v, want no 021a, no 0149, 0158=0000", doc)
	}
	if bytes.Equal(snap.etag, mustHex("1122334455667788")) {
		t.Error("the reshaped document kept the stored ETag")
	}
}

// TestFactoryResetResourceKnown: the firmware names d/reboot and factoryreset
// as device topics, so neither is logged as an unknown resource.
func TestFactoryResetResourceKnown(t *testing.T) {
	for _, p := range []string{"d/factoryreset", "d/VA1000000001/factoryreset"} {
		if !knownResource(p) {
			t.Errorf("knownResource(%q) = false, want true (normalized %q)", p, normalizeResource(p))
		}
	}
}
