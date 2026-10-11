package tado

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// servedConfig GETs d/config as the device does and returns the served document.
func servedConfig(t *testing.T, e *EmuState, euiHex string, mid uint16) []TLV {
	t.Helper()
	doc, _ := servedConfigETag(t, e, euiHex, mid)
	return doc
}

// servedConfigETag returns the served d/config document and the ETag of its
// option 4.
func servedConfigETag(t *testing.T, e *EmuState, euiHex string, mid uint16) ([]TLV, []byte) {
	t.Helper()
	raw := deviceUplink(t, mustHex(euiHex), CoAPReq{
		Type: "CON", Code: "0.01", MID: mid, Token: []byte{0x0d},
		Path: []string{"d", "config"}, Options: []CoAPOption{{Num: 23, Val: []byte{0x03}}},
	})
	m := servedAnswer(t, e.OnFrame(raw), mid)
	if m.Code != "2.05" {
		t.Fatalf("%s d/config answer = %s, want 2.05", euiHex, m.Code)
	}
	etag, ok := m.Option(4)
	if !ok {
		t.Fatalf("%s d/config 2.05 carries no ETag option", euiHex)
	}
	return parseTLVList(m.Payload), etag.Val
}

func servedConfigBody(snap cfgSnapshot) []byte {
	return encodeTLVs(withoutConfigETag(parseTLVList(snap.body)))
}

func tlvVal(t *testing.T, doc []TLV, tag string) []byte {
	t.Helper()
	for _, tlv := range doc {
		if tlv.Tag == tag {
			return tlv.Val
		}
	}
	t.Fatalf("document carries no %s", tag)
	return nil
}

func displayEmu(t *testing.T) *EmuState {
	t.Helper()
	e, _ := fixtureEmu(t)
	e.node(mustHex(bedroomEUIHex)).LastID = 10 // takes the EnqueueConfig path
	return e
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// TestDisplayDefaultsServeCapturedValues: an install that never wrote a display
// setting serves 0149 = 00 and 0158 = 0200, as before the settings existed.
func TestDisplayDefaultsServeCapturedValues(t *testing.T) {
	e := displayEmu(t)
	got, err := e.DisplaySettings(bedroomEUIHex)
	if err != nil {
		t.Fatal(err)
	}
	if want := (DisplaySettings{Orientation: OrientationHorizontal, Notification: true}); got != want {
		t.Fatalf("defaults = %+v, want %+v", got, want)
	}
	doc := servedConfig(t, e, bedroomEUIHex, 0xd400)
	if v := tlvVal(t, doc, "0149"); !bytes.Equal(v, []byte{0x00}) {
		t.Errorf("0149 = %x, want 00", v)
	}
	if v := tlvVal(t, doc, "0158"); !bytes.Equal(v, []byte{0x02, 0x00}) {
		t.Errorf("0158 = %x, want 0200", v)
	}
}

// TestDisplayWriteServesAndPushesDocument: each change queues one whole d/config
// write, which replaces the one still queued, the served document carries the
// new values under a new ETag, and a write that changes nothing queues nothing.
func TestDisplayWriteServesAndPushesDocument(t *testing.T) {
	e := displayEmu(t)
	steps := []struct {
		name   string
		w      DisplaySettingsWrite
		d0149  []byte
		d0158  []byte
		queued bool
	}{
		{"vertical", DisplaySettingsWrite{Orientation: strPtr(OrientationVertical)}, []byte{0x01}, []byte{0x02, 0x00}, true},
		{"vertical again", DisplaySettingsWrite{Orientation: strPtr(OrientationVertical)}, []byte{0x01}, []byte{0x02, 0x00}, false},
		{"notification off", DisplaySettingsWrite{Notification: boolPtr(false)}, []byte{0x01}, []byte{0x00, 0x00}, true},
		{"notification on", DisplaySettingsWrite{Notification: boolPtr(true)}, []byte{0x01}, []byte{0x02, 0x00}, true},
		{"horizontal, off", DisplaySettingsWrite{Orientation: strPtr(OrientationHorizontal), Notification: boolPtr(false)},
			[]byte{0x00}, []byte{0x00, 0x00}, true},
	}
	_, etag := servedConfigETag(t, e, bedroomEUIHex, 0xd500)
	for i, s := range steps {
		last := func() *queuedDownlink {
			if q := e.queue[bedroomEUIHex]; len(q) > 0 {
				return q[len(q)-1]
			}
			return nil
		}
		before := last()
		if err := e.EnqueueDisplaySettings(bedroomEUIHex, 10, s.w); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if queued := last() != before; queued != s.queued {
			t.Fatalf("%s: queued = %v, want %v", s.name, queued, s.queued)
		}
		waiting := 0
		for _, d := range e.queue[bedroomEUIHex] {
			if !d.inFlight {
				waiting++
			}
		}
		if waiting > 1 {
			t.Fatalf("%s: %d writes wait, want the newest document alone", s.name, waiting)
		}
		doc, next := servedConfigETag(t, e, bedroomEUIHex, uint16(0xd510+i))
		if v := tlvVal(t, doc, "0149"); !bytes.Equal(v, s.d0149) {
			t.Errorf("%s: 0149 = %x, want %x", s.name, v, s.d0149)
		}
		if v := tlvVal(t, doc, "0158"); !bytes.Equal(v, s.d0158) {
			t.Errorf("%s: 0158 = %x, want %x", s.name, v, s.d0158)
		}
		if moved := !bytes.Equal(next, etag); moved != s.queued {
			t.Errorf("%s: ETag moved = %v, want %v", s.name, moved, s.queued)
		}
		etag = next
		if s.queued {
			if q := e.queue[bedroomEUIHex]; !bytes.Equal(configWriteDocument(t, q[len(q)-1]), e.cfgDoc[bedroomEUIHex].body) {
				t.Errorf("%s: the queued write is not the served document", s.name)
			}
		}
	}
}

// configWriteDocument decodes the d/config body a queued write carries.
func configWriteDocument(t *testing.T, q *queuedDownlink) []byte {
	t.Helper()
	m, err := parseCoAPCore(q.coap)
	if err != nil {
		t.Fatal(err)
	}
	return m.Payload
}

// TestDisplayNotificationKeepsOtherBits: the switch touches bits 9 and 10 of
// 0158 only; a bit freedo does not know keeps its value, and a set bit 10 counts
// as on and is not joined by bit 9.
func TestDisplayNotificationKeepsOtherBits(t *testing.T) {
	for _, c := range []struct {
		name      string
		start     uint16
		on        bool
		want      uint16
		wantState bool
	}{
		{"off keeps unknown bits", 0x8a05, false, 0x8805, true},
		{"on keeps unknown bits", 0x8805, true, 0x8a05, false},
		{"off clears bit 10", 0x0404, false, 0x0004, true},
		{"on keeps bit 10 alone", 0x0404, true, 0x0404, true},
		{"0x7fff reads as off", 0x7fff, true, 0x0200, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := displayEmu(t)
			start := c.start
			if err := e.EnqueueConfig(bedroomEUIHex, 10, ConfigWrite{ChangeNotify: &start}); err != nil {
				t.Fatal(err)
			}
			if got, _ := e.DisplaySettings(bedroomEUIHex); got.Notification != c.wantState {
				t.Fatalf("notification before = %v, want %v", got.Notification, c.wantState)
			}
			if err := e.EnqueueDisplaySettings(bedroomEUIHex, 10, DisplaySettingsWrite{Notification: &c.on}); err != nil {
				t.Fatal(err)
			}
			if v := tlvVal(t, servedConfig(t, e, bedroomEUIHex, 0xd600), "0158"); !bytes.Equal(v, be16(c.want)) {
				t.Errorf("0158 = %x, want %04x", v, c.want)
			}
		})
	}
}

// TestDisplaySettingsSurviveRestart: the settings live in the persisted d/config
// document and outlast an installation reload.
func TestDisplaySettingsSurviveRestart(t *testing.T) {
	e := displayEmu(t)
	w := DisplaySettingsWrite{Orientation: strPtr(OrientationVertical), Notification: boolPtr(false)}
	if err := e.EnqueueDisplaySettings(bedroomEUIHex, 10, w); err != nil {
		t.Fatal(err)
	}
	restarted, in := fixtureEmu(t)
	if err := restarted.RestoreDesiredState(e.ExportDesiredState()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := restarted.SetInstall(in); err != nil {
		t.Fatal(err)
	}
	got, err := restarted.DisplaySettings(bedroomEUIHex)
	if err != nil {
		t.Fatal(err)
	}
	if want := (DisplaySettings{Orientation: OrientationVertical}); got != want {
		t.Fatalf("after restart = %+v, want %+v", got, want)
	}
}

// TestDisplayDbg2KeepsTheConfigETag: each display write via dbg2 queues one PUT
// d/dbg2/tlvs with the changed 0149 and 0158 records only and never a d/config
// write. Once the valve answers 2.04, the served document carries the new
// values under the ETag the valve holds, so its conditional GET earns 2.03.
func TestDisplayDbg2KeepsTheConfigETag(t *testing.T) {
	e := displayEmu(t)
	etag := append([]byte(nil), e.cfgDoc[bedroomEUIHex].etag...)
	steps := []struct {
		name    string
		w       DisplaySettingsWrite
		payload string // the PUT d/dbg2/tlvs body, "" when nothing queues
		d0149   string
		d0158   string
	}{
		{"vertical, notification kept on", DisplaySettingsWrite{Orientation: strPtr(OrientationVertical), Notification: boolPtr(true)},
			"01490101", "01", "0200"},
		{"vertical again", DisplaySettingsWrite{Orientation: strPtr(OrientationVertical)}, "", "01", "0200"},
		{"notification off", DisplaySettingsWrite{Notification: boolPtr(false)}, "0158020000", "01", "0000"},
		{"horizontal, on", DisplaySettingsWrite{Orientation: strPtr(OrientationHorizontal), Notification: boolPtr(true)},
			"014901000158020200", "00", "0200"},
	}
	for i, s := range steps {
		s.w.Via = OffsetModeDbg2
		e.DropQueue(bedroomEUIHex)
		if err := e.EnqueueDisplaySettings(bedroomEUIHex, 10, s.w); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		var payloads []string
		for eui, q := range e.queue {
			for _, d := range q {
				m, err := parseCoAPCore(d.coap)
				if err != nil {
					t.Fatal(err)
				}
				if eui != bedroomEUIHex || m.Code != "0.03" || m.URI() != "d/dbg2/tlvs" {
					t.Fatalf("%s: queued %s %s to %s, want PUT d/dbg2/tlvs to the valve only", s.name, m.Code, m.URI(), eui)
				}
				payloads = append(payloads, hex.EncodeToString(m.Payload))
			}
		}
		switch {
		case s.payload == "" && len(payloads) != 0:
			t.Errorf("%s: queued %v, want nothing", s.name, payloads)
		case s.payload != "" && (len(payloads) != 1 || payloads[0] != s.payload):
			t.Errorf("%s: queued %v, want the one write %s", s.name, payloads, s.payload)
		}
		if len(payloads) > 0 {
			answerQueued(t, e, mustHex(bedroomEUIHex), "2.04")
		}

		after := e.cfgDoc[bedroomEUIHex]
		body := parseTLVList(after.body)
		if !bytes.Equal(after.etag, etag) || tlvValue(body, configEtagTag) != hex.EncodeToString(etag) {
			t.Errorf("%s: etag = %x, 015a = %s, want both %x", s.name, after.etag, tlvValue(body, configEtagTag), etag)
		}
		if m := servedConfigAnswer(t, e, bedroomEUIHex, etag, uint16(0xd700+2*i)); m.Code != "2.03" {
			t.Errorf("%s: conditional GET with the held ETag = %s, want 2.03", s.name, m.Code)
		}
		full := servedConfigAnswer(t, e, bedroomEUIHex, nil, uint16(0xd701+2*i))
		doc := parseTLVList(full.Payload)
		if full.Code != "2.05" || tlvValue(doc, "0149") != s.d0149 || tlvValue(doc, "0158") != s.d0158 {
			t.Errorf("%s: full GET = %s 0149=%s 0158=%s, want 2.05 %s %s",
				s.name, full.Code, tlvValue(doc, "0149"), tlvValue(doc, "0158"), s.d0149, s.d0158)
		}
	}

	// The default write of the same values finds the document unchanged.
	e.DropQueue(bedroomEUIHex)
	if err := e.EnqueueDisplaySettings(bedroomEUIHex, 10, DisplaySettingsWrite{Orientation: strPtr(OrientationHorizontal)}); err != nil {
		t.Fatal(err)
	}
	if n := queuedDownlinks(e); n != 0 || !bytes.Equal(e.cfgDoc[bedroomEUIHex].etag, etag) {
		t.Errorf("config write of the served values: %d queued, etag %x, want none and %x", n, e.cfgDoc[bedroomEUIHex].etag, etag)
	}
}

func TestDisplayWriteRefusals(t *testing.T) {
	e := displayEmu(t)
	for _, c := range []struct {
		name   string
		target string
		w      DisplaySettingsWrite
	}{
		{"no field", bedroomEUIHex, DisplaySettingsWrite{}},
		{"bad orientation", bedroomEUIHex, DisplaySettingsWrite{Orientation: strPtr("diagonal")}},
		{"wall thermostat", thermostatEUIHex, DisplaySettingsWrite{Notification: boolPtr(false)}},
		{"wall thermostat via dbg2", thermostatEUIHex, DisplaySettingsWrite{Notification: boolPtr(false), Via: OffsetModeDbg2}},
		{"bad via", bedroomEUIHex, DisplaySettingsWrite{Orientation: strPtr(OrientationVertical), Via: "cloud"}},
	} {
		if err := e.EnqueueDisplaySettings(c.target, 10, c.w); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	if n := len(e.queue[bedroomEUIHex]); n != 0 {
		t.Errorf("a refused write queued %d downlinks", n)
	}
	if _, err := e.DisplaySettings(thermostatEUIHex); err == nil {
		t.Error("the wall thermostat reports display settings")
	}
}
