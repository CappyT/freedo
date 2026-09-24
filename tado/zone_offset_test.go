package tado

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// servedConfigAnswer GETs d/config as the device does, with etag as option 4 when
// set, and returns the answer.
func servedConfigAnswer(t *testing.T, e *EmuState, euiHex string, etag []byte, mid uint16) *CoAPMessage {
	t.Helper()
	opts := []CoAPOption{{Num: 23, Val: []byte{0x03}}}
	if etag != nil {
		opts = append([]CoAPOption{{Num: 4, Val: etag}}, opts...)
	}
	raw := deviceUplink(t, mustHex(euiHex), CoAPReq{
		Type: "CON", Code: "0.01", MID: mid, Token: []byte{0x0d},
		Path: []string{"d", "config"}, Options: opts,
	})
	return servedAnswer(t, e.OnFrame(raw), mid)
}

func tlvValue(list []TLV, tag string) string {
	for _, tlv := range list {
		if tlv.Tag == tag {
			return hex.EncodeToString(tlv.Val)
		}
	}
	return ""
}

// TestCalibOffsetDbg2KeepsTheConfigETag: the dbg2 write is one PUT
// d/dbg2/tlvs carrying the single record 0140 and no d/config write. The
// served document takes the new 0140 under the ETag the valve holds: its
// conditional GET earns 2.03, a full read gets the new value with the same
// ETag, and neither a reload nor a restart mints a new ETag or queues a write.
func TestCalibOffsetDbg2KeepsTheConfigETag(t *testing.T) {
	e, in := fixtureEmu(t)
	before, ok := e.cfgDoc[cameraEUIHex]
	if !ok {
		t.Fatal("the camera valve holds no d/config document")
	}
	etag := append([]byte(nil), before.etag...)

	if err := e.EnqueueCalibOffsetDbg2(cameraEUIHex, 10, -70); err != nil {
		t.Fatal(err)
	}
	if n := queuedDownlinks(e); n != 1 {
		t.Fatalf("queued downlinks = %d, want the dbg2 write alone", n)
	}
	actions, ok, err := e.NextQueuedDownlink(cameraEUIHex)
	if err != nil || !ok || len(actions) == 0 {
		t.Fatalf("no queued downlink: ok=%v err=%v", ok, err)
	}
	df, err := ParseData(NetworkKey, actions[0].Bytes)
	if err != nil {
		t.Fatal(err)
	}
	app := ParseApp(df.Plaintext)
	if app == nil || app.CoAP == nil || app.CoAP.Code != "0.03" || app.CoAP.URI() != "d/dbg2/tlvs" {
		t.Fatalf("dbg2 write = %+v, want PUT d/dbg2/tlvs", app)
	}
	if got := hex.EncodeToString(app.CoAP.Payload); got != "014002ffba" {
		t.Errorf("dbg2 payload = %s, want the single record 014002ffba", got)
	}

	after := e.cfgDoc[cameraEUIHex]
	body := parseTLVList(after.body)
	if !bytes.Equal(after.etag, etag) || tlvValue(body, configEtagTag) != hex.EncodeToString(etag) {
		t.Errorf("etag = %x, 015a = %s, want both %x", after.etag, tlvValue(body, configEtagTag), etag)
	}
	if got := tlvValue(body, "0140"); got != "ffba" {
		t.Errorf("served 0140 = %q, want ffba", got)
	}

	if m := servedConfigAnswer(t, e, cameraEUIHex, etag, 0xd401); m.Code != "2.03" {
		t.Errorf("conditional GET with the held ETag = %s, want 2.03", m.Code)
	}
	full := servedConfigAnswer(t, e, cameraEUIHex, nil, 0xd402)
	if full.Code != "2.05" || tlvValue(parseTLVList(full.Payload), "0140") != "ffba" {
		t.Errorf("full GET = %s %x, want 2.05 with 0140=ffba", full.Code, full.Payload)
	}
	if et, ok := full.Option(4); !ok || !bytes.Equal(et.Val, etag) {
		t.Errorf("full GET ETag = %x, want %x", et.Val, etag)
	}

	e.DropQueue(cameraEUIHex)
	if _, _, err := e.SetInstall(in); err != nil {
		t.Fatal(err)
	}
	if n := queuedDownlinks(e); n != 0 || !bytes.Equal(e.cfgDoc[cameraEUIHex].etag, etag) {
		t.Errorf("reload: %d queued, etag %x, want none and %x", n, e.cfgDoc[cameraEUIHex].etag, etag)
	}

	restarted, _ := fixtureEmu(t)
	if err := restarted.RestoreDesiredState(e.ExportDesiredState()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := restarted.SetInstall(in); err != nil {
		t.Fatal(err)
	}
	doc := restarted.cfgDoc[cameraEUIHex]
	if n := queuedDownlinks(restarted); n != 0 || !bytes.Equal(doc.etag, etag) || tlvValue(parseTLVList(doc.body), "0140") != "ffba" {
		t.Errorf("restart: %d queued, etag %x, 0140 %s, want none, %x, ffba",
			n, doc.etag, tlvValue(parseTLVList(doc.body), "0140"), etag)
	}

	// A real change still mints a new ETag.
	calib := 20
	if err := restarted.EnqueueConfig(cameraEUIHex, 10, ConfigWrite{CalibOffset: &calib}); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(restarted.cfgDoc[cameraEUIHex].etag, etag) {
		t.Error("a d/config change kept the old ETag")
	}
}

// TestCalibOffsetDbg2Refuses: the wall thermostat serves no
// d/dbg2/tlvs, and a value out of range is refused before anything queues.
func TestCalibOffsetDbg2Refuses(t *testing.T) {
	e, in := fixtureEmu(t)
	var thermostat string
	for i := range in.Devices {
		if in.Devices[i].Kind == KindThermostat {
			thermostat = hexEUI(in.Devices[i].WireEUI())
		}
	}
	if err := e.EnqueueCalibOffsetDbg2(thermostat, 1, -70); err == nil {
		t.Error("the thermostat took a dbg2 write")
	}
	if err := e.EnqueueCalibOffsetDbg2(cameraEUIHex, 10, 1001); err == nil {
		t.Error("an offset of 10.01 C was queued")
	}
	if n := queuedDownlinks(e); n != 0 {
		t.Errorf("queued downlinks = %d, want none", n)
	}
}

func TestZoneOffsetCheck(t *testing.T) {
	for _, tc := range []struct {
		off  ZoneOffset
		good bool
	}{
		{ZoneOffset{Mode: OffsetModeConfig}, true},
		{ZoneOffset{Mode: OffsetModeDbg2}, true},
		{ZoneOffset{Mode: OffsetModeSetpoint, SetpointCenti: -1000}, true},
		{ZoneOffset{Mode: OffsetModeSetpoint, SetpointCenti: 1001}, false},
		{ZoneOffset{Mode: OffsetModeDbg2, SetpointCenti: 50}, false},
		{ZoneOffset{Mode: "cloud"}, false},
	} {
		if got := tc.off.Check() == ""; got != tc.good {
			t.Errorf("%+v: Check() = %q, want good=%v", tc.off, tc.off.Check(), tc.good)
		}
	}
	var o Options
	if o.ZoneOffset(3).Mode != OffsetModeConfig || o.SetpointShiftC(3) != 0 {
		t.Errorf("default = %+v shift %v, want config and 0", o.ZoneOffset(3), o.SetpointShiftC(3))
	}
	o.ZoneOffsets = map[int]ZoneOffset{3: {Mode: OffsetModeSetpoint, SetpointCenti: -70}}
	if got := o.SetpointShiftC(3); got != -0.7 {
		t.Errorf("setpoint shift = %v, want -0.7", got)
	}
}
