package tado

import (
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// TestEmuResolvesThroughInstall: with an installation configured, a command
// target is resolved against it in every spelling, and a room label shared by
// two devices is refused by name instead of hitting the first match.
func TestEmuResolvesThroughInstall(t *testing.T) {
	in := fixtureInstall(t)
	e := NewEmu(in.EmuConfig(), NewUnhandledLogger(io.Discard))

	for _, ref := range []string{"VA1000000005", "camera", "a602", "02a6cefa07c51b00"} {
		wire, label, err := e.resolve(ref)
		if err != nil {
			t.Errorf("resolve(%q): %v", ref, err)
			continue
		}
		if got := hex.EncodeToString(wire); got != "02a6cefa07c51b00" {
			t.Errorf("resolve(%q) = %s", ref, got)
		}
		if label != "camera" {
			t.Errorf("resolve(%q) label = %q, want camera", ref, label)
		}
	}

	_, _, err := e.resolve("cucina")
	var amb *AmbiguousRefError
	if !errors.As(err, &amb) {
		t.Fatalf("resolve of a shared label = %v, want an ambiguity error", err)
	}
	if len(amb.Candidates) != 2 {
		t.Errorf("candidates = %v, want the two cucina valves", amb.Candidates)
	}
	if err := e.EnqueueSetpoint("cucina", SetpointParams{ID: 5, Setpoint: 20, Sched: 20}); err == nil {
		t.Error("EnqueueSetpoint on a shared label was accepted")
	}
}

// TestEmuWithoutInstallStillResolvesRawEUI: no installation means no names, but
// an explicit wire address still addresses a device.
func TestEmuWithoutInstallStillResolvesRawEUI(t *testing.T) {
	e := NewEmu(EmuConfig{Key: NetworkKey, SelfEUI: BridgeEUI}, NewUnhandledLogger(io.Discard))
	wire, _, err := e.resolve("02a6cefa07c51b00")
	if err != nil || hex.EncodeToString(wire) != "02a6cefa07c51b00" {
		t.Fatalf("resolve of a raw EUI = %x, %v", wire, err)
	}
	if _, _, err := e.resolve("nosuchnode"); err == nil {
		t.Error("resolve of an unknown target = nil error")
	}
}

// utcUplink is a telemetry publish from a valve, carrying the "id=10"
// the emulator needs to address that device's own d/config.
func utcUplink(t *testing.T, dev []byte, mid uint16) []byte {
	t.Helper()
	return deviceUplink(t, dev, CoAPReq{
		Type: "NON", Code: "0.03", MID: mid, Token: []byte{0x51},
		Path: []string{"h", "7654321", "z", "10", "s"}, Query: []string{"id=10"},
		Options: []CoAPOption{optOctets},
		Payload: TLV{Tag: "6280", Val: tempC(21.0)}.Encode(),
	})
}

// utcWrite returns the config write the emulator decided, if any, plus the
// offset it carries.
func utcWrite(t *testing.T, actions []Action) (*Frame, string, bool) {
	t.Helper()
	a, ok := findAction(actions, ActData, "downlink config write")
	if !ok {
		return nil, "", false
	}
	f, err := ParseData(NetworkKey, a.Bytes)
	if err != nil {
		t.Fatalf("the config write does not decrypt: %v", err)
	}
	app := ParseApp(f.Plaintext)
	if app == nil || app.CoAP == nil {
		t.Fatal("the config write does not parse")
	}
	if got := app.CoAP.URI(); got != "d/config" {
		t.Errorf("uri = %q, want d/config: the write carries no query", got)
	}
	return f, hex.EncodeToString(app.CoAP.Payload), true
}

func TestUTCOffsetPushesOnlyChangedDesiredValue(t *testing.T) {
	in := fixtureInstall(t)
	cfg := in.EmuConfig()
	if cfg.UTCOffsetMin == nil || *cfg.UTCOffsetMin != 60 {
		t.Fatalf("EmuConfig.UTCOffsetMin = %v, want the fixture's 60", cfg.UTCOffsetMin)
	}
	e := NewEmu(cfg, NewUnhandledLogger(io.Discard))
	dev := mustHex("02a6cefa07c51b00") // camera, VA1000000005

	if _, _, ok := utcWrite(t, e.OnFrame(utcUplink(t, dev, 0x6101))); ok {
		t.Fatal("initial desired config caused a redundant UTC-offset write")
	}

	v := 120
	e.cfg.UTCOffsetMin = &v
	now := time.Now()
	if !e.SyncUTCOffset(now) {
		t.Fatal("SyncUTCOffset missed the changed offset")
	}
	if got := e.PushUTCOffset(now); got != hexEUI(dev) {
		t.Fatalf("PushUTCOffset = %q, want the camera valve first", got)
	}
	f, body, ok := utcWrite(t, e.OnFrame(utcUplink(t, dev, 0x6102)))
	if !ok {
		t.Fatal("a changed offset was never pushed")
	}
	want := hex.EncodeToString(e.cfgDoc[hexEUI(dev)].body)
	if body != want || !strings.Contains(body, "02b2020078") {
		t.Errorf("body = %s, want the whole document %s with 02b2 = +120 min", body, want)
	}
	e.OnFrame(BuildEnhAck(f.Seq, BridgeEUI, dev, 0x0101))
	request := ParseApp(f.Plaintext).CoAP
	e.confirmByMID(dev, &CoAPMessage{Type: "ACK", Code: "2.04", MID: request.MID, Token: request.Token})
	if got := e.PushUTCOffset(now.Add(time.Hour)); got == hexEUI(dev) {
		t.Error("pushed the changed offset twice")
	}
	if _, _, ok := utcWrite(t, e.OnFrame(utcUplink(t, dev, 0x6103))); ok {
		t.Error("pushed the changed offset twice")
	}
}

func TestNoAutoQueueSuppressesUTCOffset(t *testing.T) {
	in := fixtureInstall(t)
	cfg := in.EmuConfig()
	cfg.NoAutoQueue = true
	e := NewEmu(cfg, NewUnhandledLogger(io.Discard))
	dev := mustHex("02a6cefa07c51b00")

	v := 120
	e.cfg.UTCOffsetMin = &v
	e.SyncUTCOffset(time.Now())
	if got := e.PushUTCOffset(time.Now()); got != "" {
		t.Fatalf("no-auto-queue pushed the offset to %s", got)
	}
	actions := e.OnFrame(utcUplink(t, dev, 0x6110))
	if _, _, ok := utcWrite(t, actions); ok {
		t.Fatal("no-auto-queue emitted an automatic UTC-offset write")
	}
	if n := e.Pending("camera"); n != 0 {
		t.Fatalf("pending=%d, want 0", n)
	}
	if _, ok := findAction(actions, ActEnhAck, ""); !ok {
		t.Fatal("no-auto-queue suppressed the MAC Enhanced ACK")
	}
}

// TestUTCOffsetOperatorWriteWins: an explicit config write carrying 02b2 records
// what the device was told, so the automatic push does not follow it up.
func TestUTCOffsetOperatorWriteWins(t *testing.T) {
	in := fixtureInstall(t)
	e := NewEmu(in.EmuConfig(), NewUnhandledLogger(io.Discard))
	dev := mustHex("02a6cefa07c51b00")

	off := 90
	if err := e.EnqueueConfig("camera", 10, ConfigWrite{UTCOffsetMin: &off}); err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueConfig("camera", 10, ConfigWrite{UTCOffsetMin: &off}); err != nil {
		t.Fatal(err)
	}
	if n := e.Pending("camera"); n != 1 {
		t.Fatalf("identical config produced %d queued writes, want 1", n)
	}
	_, body, ok := utcWrite(t, e.OnFrame(utcUplink(t, dev, 0x6201)))
	if !ok {
		t.Fatal("the operator's config write never went out")
	}
	want := hex.EncodeToString(e.cfgDoc[hexEUI(dev)].body)
	if body != want || !strings.Contains(body, "02b202005a") {
		t.Errorf("body = %s, want the whole document %s with the operator's +90 min", body, want)
	}
	if n := e.Pending("camera"); n != 1 {
		t.Errorf("pending=%d, want only the operator's write", n)
	}
}

// TestConfigWriteRefusedByWallThermostat: the wall thermostat drops the
// document a d/config write carries, and its served document takes no
// operator's 02b2, so neither an operator nor the automatic UTC-offset push
// puts a write on the air. A valve in the same zone still takes its write.
func TestConfigWriteRefusedByWallThermostat(t *testing.T) {
	in := fixtureInstall(t)
	e := NewEmu(in.EmuConfig(), NewUnhandledLogger(io.Discard))
	off := 90

	err := e.EnqueueConfig("RU1000000001", 1, ConfigWrite{UTCOffsetMin: &off})
	if err == nil || !strings.Contains(err.Error(), "d/config") {
		t.Fatalf("EnqueueConfig to the wall thermostat = %v, want a refusal naming d/config", err)
	}
	if n := e.Pending("RU1000000001"); n != 0 {
		t.Errorf("pending=%d, want no queued write", n)
	}

	thermostat := mustHex("05a2cafa07c51b00")
	uplink := deviceUplink(t, thermostat, CoAPReq{
		Type: "NON", Code: "0.03", MID: 0x6301, Token: []byte{0x52},
		Path: []string{"z", "p"}, Query: []string{"lid=1"},
		Options: []CoAPOption{optOctets},
		Payload: TLV{Tag: "6280", Val: tempC(21.0)}.Encode(),
	})
	if _, ok := findAction(e.OnFrame(uplink), ActData, "downlink config write"); ok {
		t.Error("the automatic UTC-offset push wrote to the wall thermostat")
	}

	if err := e.EnqueueConfig("VA1000000003", 1, ConfigWrite{UTCOffsetMin: &off}); err != nil {
		t.Fatalf("the valve of the same zone refused the write: %v", err)
	}
}

// TestInstallDissectNamesNodes: the installation supplies both the key and the
// node names the CLI prints.
func TestInstallDissectNamesNodes(t *testing.T) {
	in := fixtureInstall(t)
	frames := loadFrames(t, "testdata/frames_69ec.txt")
	named := 0
	for _, fr := range frames {
		s := in.Dissect(fr)
		if strings.Contains(s, "MIC fail") {
			t.Fatalf("install key does not decrypt the corpus: %s", s)
		}
		if strings.Contains(s, "camera") || strings.Contains(s, "cucina") || strings.Contains(s, "corridoio") {
			named++
		}
	}
	if named == 0 {
		t.Error("no frame was rendered with an installation label")
	}
}

// TestAnalyzerForInstall: the install-scoped analyzer decrypts with the config
// key and tells a downlink apart by the configured coordinator address.
func TestAnalyzerForInstall(t *testing.T) {
	in := fixtureInstall(t)
	a := NewAnalyzerFor(in, NewUnhandledLogger(io.Discard))
	for _, fr := range loadFrames(t, "testdata/frames_69ec.txt") {
		a.Add(fr)
	}
	st := a.Stats()
	if st.MICFails != 0 {
		t.Errorf("MIC failures = %d, want 0", st.MICFails)
	}
	if st.Downlinks == 0 || st.Uplinks == 0 {
		t.Errorf("downlinks=%d uplinks=%d, want both directions", st.Downlinks, st.Uplinks)
	}
}
