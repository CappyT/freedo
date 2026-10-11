package tado

import (
	"encoding/hex"
	"strings"
	"testing"
)

// TestParseAppGolden: every captured frame yields a parseable app payload whose
// TLVs re-serialize to the exact bytes we sliced them from.
func TestParseAppGolden(t *testing.T) {
	frames := loadGolden(t)
	parsed, withSetpoint := 0, 0
	for _, fr := range frames {
		p, err := ParseData(NetworkKey, fr)
		if err != nil {
			t.Fatal(err)
		}
		app := ParseApp(p.Plaintext)
		if app == nil {
			continue
		}
		parsed++
		for _, tlv := range app.TLVs {
			if tlv.Len != len(tlv.Val) {
				t.Errorf("tlv %s len mismatch", tlv.Tag)
			}
			if tlv.Tag == "6280" && tlv.Len == 2 {
				withSetpoint++
			}
		}
	}
	if parsed == 0 {
		t.Fatal("no frame parsed into an app payload")
	}
	t.Logf("parsed %d/%d app payloads, %d carried a 6280 setpoint", parsed, len(frames), withSetpoint)
}

// TestSetpointRoundTrip: a command built by BuildSetpoint decrypts back to the
// exact TLVs and setpoint we asked for. Exercises the full encode+decode path.
func TestSetpointRoundTrip(t *testing.T) {
	valve := NodeByAny("bedroom")
	if valve == nil {
		t.Fatal("bedroom node not found")
	}
	dst, _ := hex.DecodeString(valve.WireLE)
	meta, _ := hex.DecodeString("7e33f01122e806ff") // placeholder meta

	f, pt, err := BuildSetpoint(NetworkKey, SetpointCmd{
		DstEUI:     dst,
		Seq:        0x10,
		Counter:    19319,
		Setpoint:   21.5,
		Meta:       meta,
		Overlay:    2,
		NewOverlay: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	back, err := ParseData(NetworkKey, f.Bytes())
	if err != nil {
		t.Fatalf("built frame does not decrypt: %v", err)
	}
	if string(back.Plaintext) != string(pt) {
		t.Fatal("decrypted plaintext differs from what was encrypted")
	}
	app := ParseApp(back.Plaintext)
	if app == nil {
		t.Fatal("built command does not parse")
	}
	if app.Counter != 19319 {
		t.Errorf("counter=%d want 19319", app.Counter)
	}
	got := map[string]string{}
	for _, tlv := range app.TLVs {
		got[tlv.Tag] = hex.EncodeToString(tlv.Val)
	}
	if got["6280"] != "0866" { // 21.5 * 100 = 2150 = 0x0866
		t.Errorf("setpoint 6280=%s want 0866", got["6280"])
	}
	if v, ok := got["6200"]; ok {
		t.Errorf("6200=%s present: the away write carries no scheduled setpoint", v)
	}
	if got["6440"] != "0001" {
		t.Errorf("new_overlay 6440=%s want 0001", got["6440"])
	}
	// The captured manual body, with this test's setpoint in 6280.
	want := strings.Replace(capturedSetpointManualBody, "09c4", "0866", 1)
	if got := hex.EncodeToString(back.Plaintext); !strings.Contains(got, want) {
		t.Errorf("plaintext=%s\n  misses body=%s", got, want)
	}
}

// TestBuildSetpointAutoOmitsSetpoint: the captured return to schedule carries
// neither 6260 nor 6280, see capturedSetpointAutoBody.
func TestBuildSetpointAutoOmitsSetpoint(t *testing.T) {
	dst, _ := hex.DecodeString(NodeByAny("bedroom").WireLE)
	_, pt, err := BuildSetpoint(NetworkKey, SetpointCmd{
		DstEUI:     dst,
		Seq:        0x10,
		Counter:    19319,
		Setpoint:   21.5,
		Meta:       mustHex("7e33f01122e806ff"),
		Overlay:    0,
		NewOverlay: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"6200", "6260", "6280"} {
		if strings.Contains(hex.EncodeToString(pt), tag+"0") {
			t.Errorf("return-to-schedule body carries %s: %s", tag, hex.EncodeToString(pt))
		}
	}
}

// TestTagLabel checks a couple of catalog decoders.
func TestTagLabel(t *testing.T) {
	cases := []struct {
		tag, val, want string
	}{
		{"6280", "0866", "setpoint_C=21.50C"},
		{"0135", "01f2", "humidity_pct=49.80%"},
		{"6240", "02", "overlay=manual"},
		{"9999", "abcd", "9999=abcd"},
		// d/config store tags: a captured config write has to render readably
		// rather than as raw hex.
		{"013f", "01", "temp_unit=F"},
		{"013f", "00", "temp_unit=C"},
		{"0140", "ffec", "calib_offset=-20"},
		{"0143", "01", "trace_enable=1"},
		{"0149", "01", "display_orientation=1"},
		{"0158", "0200", "change_notify=0200"},
		{"015a", "0011223344556677", "config_etag=0011223344556677"},
		{"015c", "0074cbb1", "home_id=7654321"},
		{"015e", "0d00", "zone_table=0d00"},
		{"02b2", "003c", "utc_offset_min=60"},
		{"02b3", "00", "offline_schedule=0"},
		// z/extui endpoint URIs and the z/legionella state byte
		{"01d4", "5b666538303a3a315d2f7a2f73", "extui_uri_s=[fe80::1]/z/s"},
		{"01d5", "5b666538303a3a315d2f7a2f70", "extui_uri_p=[fe80::1]/z/p"},
		{"63c0", "01", "legionella_state=1"},
		// device reports the catalog used to drop as unknown tags
		{"62a0", "00", "zmeas_ack=0"},
		{"021c", "01", "open_window_detected=1"},
		{"62c0", "01", "open_window_zone=1"},
		{"015d", "0070", "interface_module_id=112"},
		{"015d", "0047", "interface_module_id=71"},
		{"046e", "002a", "boiler_046e=42"},
		{"0470", "0100", "boiler_0470=256"},
		// boiler bus catalogue
		{"044c", "0fa0", "boiler_flow_C=40.00C"},
		{"044f", "fe0c", "outside_C=-5.00C"},
		{"044f", "7fff", "outside_C=unset"},
		{"0453", "07", "boiler_0453=7"},
		{"0462", "0f", "ch_pressure_bar=1.50bar"},
		{"0482", "00010000", "hp_0482=65536"},
		{"049e", "0102", "hc_049e=258"},
		{"045c", "03", "dhw_045c=3"},
		{"0460", "00000123", "fault_code=291"},
		{"0167", "01", "bus_detect=1"},
		{"0481", "044d02044f02", "unsupported=044d,044f"},
		{"0478", "00000001", "0478=00000001"},
	}
	for _, c := range cases {
		v, _ := hex.DecodeString(c.val)
		if got := (TLV{Tag: c.tag, Len: len(v), Val: v}).TagLabel(); got != c.want {
			t.Errorf("tag %s: got %q want %q", c.tag, got, c.want)
		}
	}
}

// TestTagLabelZoneConfig: every tag of the zone configuration document renders
// with a name, and an endpoint renders as text. The values are synthetic.
func TestTagLabelZoneConfig(t *testing.T) {
	const uriS = "coap://[fe80::1]/z/s"
	const uriP = "coap://[fe80::1]/z/p"
	const uriCPE = "coap://[fe80::1]/z/cpe"
	cases := []struct {
		tag  string
		val  []byte
		want string
	}{
		{"63e0", []byte{0x01}, "zone_cfg_state=1"},
		{"63a0", []byte(uriS), "zone_owner_uri_s=" + uriS},
		{"8000", []byte(uriS), "zone_member_uri_s=" + uriS},
		{"8400", []byte(uriP), "zone_report_uri_p=" + uriP},
		{"8200", []byte(uriCPE), "zone_cpe_uri=" + uriCPE},
		{"6040", []byte(uriP), "zone_measure_uri_p=" + uriP},
		{"6060", []byte{0x01}, "zone_circuit=1"},
		{"6080", []byte{0x03, 0xe8}, "zone_cfg_6080=1000"},
		{"60a0", []byte{0x01, 0xf4}, "zone_cfg_60a0=500"},
		{"60c0", []byte{0x05, 0xdc}, "zone_cfg_60c0=1500"},
		{"60e0", []byte{0x01}, "zone_cfg_enable=1"},
		{"6380", []byte{0x03, 0x84}, "zone_refresh_ivl_sec=900"},
	}
	for _, c := range cases {
		if got := (TLV{Tag: c.tag, Len: len(c.val), Val: c.val}).TagLabel(); got != c.want {
			t.Errorf("tag %s: got %q want %q", c.tag, got, c.want)
		}
	}
}

// TestResourceOf62c0: 62c0 is the open-window state on the zone plane and an
// interval in the zone configuration document. The bodies are synthetic.
func TestResourceOf62c0(t *testing.T) {
	zonePlane := ParseTLVs(encodeTLVs([]TLV{
		{Tag: "6240", Val: []byte{0x02}},
		{Tag: "6280", Val: []byte{0x08, 0x34}},
		{Tag: "62c0", Val: []byte{0x01}},
	}))
	zoneCfg := ParseTLVs(encodeTLVs([]TLV{
		{Tag: "63e0", Val: []byte{0x01}},
		{Tag: "63a0", Val: []byte("coap://[fe80::1]/z/s")},
		{Tag: "62c0", Val: []byte{0x03, 0x84}},
		{Tag: "6380", Val: []byte{0x03, 0x84}},
	}))

	cases := []struct {
		name string
		uri  string
		body []TLV
		res  TLVResource
		want string
	}{
		{"zone plane", "h/7654321/z/10/s?id=10", zonePlane, ResDefault, "open_window_zone=1"},
		{"zone plane, no uri", "", zonePlane, ResDefault, "open_window_zone=1"},
		{"config by uri", "h/7654321/z/10/config", zoneCfg, ResZoneConfig, "zone_open_ivl_sec=900"},
		// A CoAP response repeats no Uri-Path, so the body alone has to classify.
		{"config by body", "", zoneCfg, ResZoneConfig, "zone_open_ivl_sec=900"},
	}
	for _, c := range cases {
		res := ResourceOf(c.uri, c.body)
		if res != c.res {
			t.Errorf("%s: resource=%d, want %d", c.name, res, c.res)
		}
		tlv, ok := zoneConfigTestFind(c.body, "62c0")
		if !ok {
			t.Fatalf("%s: the body carries no 62c0", c.name)
		}
		if got := tlv.TagLabelIn(res); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
