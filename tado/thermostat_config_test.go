package tado

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// thermostatConfigGET is the wall thermostat's conditional d/config GET with etag.
func thermostatConfigGET(t *testing.T, in *Install, e *EmuState, dev []byte, mid uint16, etag []byte) *CoAPMessage {
	t.Helper()
	raw := synthDeviceUplink(t, in, dev, CoAPReq{
		Type: "CON", Code: "0.01", MID: mid, Token: []byte{0x24},
		Path:    []string{"d", "RU0000000001", "config"},
		Options: []CoAPOption{{Num: 4, Val: etag}},
	})
	return synthAnswer(t, in, e.OnFrame(raw), mid)
}

// TestThermostatCalibOffsetServed: the wall thermostat drops the document a
// d/config write carries, so a calibOffset write goes into the document it is
// served under a new ETag. The queue gets one empty PUT d/config, the trigger
// that makes the device GET at once, and no second one while it waits. The
// GET with the old ETag gets 2.05 carrying 0140, and the document survives a
// restart. A write that moves any other tag, and the d/dbg2 path, stay refused.
func TestThermostatCalibOffsetServed(t *testing.T) {
	in := synthInstall(t)
	ru, _ := synthDevices(t, in)
	key := hexEUI(ru)
	e := NewEmu(in.EmuConfig(), nil)
	e.OnFrame(moduleReport(t, in, ru, 71, 0x7601))
	drainQueue(t, e, ru)
	old := append([]byte(nil), e.cfgDoc[key].etag...)
	if len(old) == 0 {
		t.Fatal("no thermostat document after the module report")
	}
	if !e.ServesConfigOnRead(key) {
		t.Fatal("the wall thermostat does not read as served on its own read")
	}

	centi := -150
	if err := e.EnqueueConfig(key, 1, ConfigWrite{CalibOffset: &centi}); err != nil {
		t.Fatalf("calibOffset write to the wall thermostat: %v", err)
	}
	served := e.cfgDoc[key]
	if bytes.Equal(served.etag, old) {
		t.Fatal("the calibOffset write left the ETag")
	}
	if n := e.Pending(key); n != 1 || !e.ConfigRereadQueued(key) {
		t.Fatalf("pending=%d reread=%v, want the one re-read trigger", n, e.ConfigRereadQueued(key))
	}
	trigger := headRequest(t, e, ru)
	if trigger.Code != "0.03" || trigger.URI() != "d/config" || len(trigger.Payload) != 0 {
		t.Errorf("trigger = %s %s %x, want an empty PUT d/config", trigger.Code, trigger.URI(), trigger.Payload)
	}
	if _, ok := trigger.Option(27); ok {
		t.Error("the trigger carries Block1: its last block would get 5.01")
	}
	second := -120
	if err := e.EnqueueConfig(key, 1, ConfigWrite{CalibOffset: &second}); err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueConfig(key, 1, ConfigWrite{CalibOffset: &centi}); err != nil {
		t.Fatal(err)
	}
	if n := e.Pending(key); n != 1 {
		t.Errorf("pending=%d after two more writes, want the one waiting trigger", n)
	}
	if got := e.cfgDoc[key]; !bytes.Equal(got.body, served.body) {
		t.Error("the document differs after the offset came back to -150")
	}
	served = e.cfgDoc[key]
	answerQueued(t, e, ru, "2.04")
	if n := e.Pending(key); n != 0 {
		t.Errorf("pending=%d after the 2.04, want none", n)
	}

	m := thermostatConfigGET(t, in, e, ru, 0x7602, old)
	if m.Code != "2.05" {
		t.Fatalf("conditional GET with the old ETag = %s, want 2.05", m.Code)
	}
	if et, ok := m.Option(4); !ok || !bytes.Equal(et.Val, served.etag) {
		t.Errorf("2.05 ETag option = %x, want %x", et.Val, served.etag)
	}
	var calib string
	for _, tlv := range parseTLVList(m.Payload) {
		switch tlv.Tag {
		case "0140":
			calib = hex.EncodeToString(tlv.Val)
		case configEtagTag:
			t.Error("the 2.05 body carries 015a")
		}
	}
	if calib != "ff6a" {
		t.Errorf("2.05 body 0140 = %q, want ff6a (-150)", calib)
	}
	if m := thermostatConfigGET(t, in, e, ru, 0x7603, served.etag); m.Code != "2.03" {
		t.Errorf("conditional GET with the new ETag = %s, want 2.03", m.Code)
	}

	// The same offset again moves nothing.
	if err := e.EnqueueConfig(key, 1, ConfigWrite{CalibOffset: &centi}); err != nil {
		t.Fatal(err)
	}
	if got := e.cfgDoc[key]; !bytes.Equal(got.body, served.body) || !bytes.Equal(got.etag, served.etag) {
		t.Error("an unchanged calibOffset write moved the document")
	}

	// A write that moves another tag is refused whole and leaves the document.
	off, other := 90, 30
	for name, w := range map[string]ConfigWrite{
		"utc offset":          {UTCOffsetMin: &off},
		"offset with another": {CalibOffset: &other, UTCOffsetMin: &off},
		"resend":              {Resend: true},
		"etag":                {Etag: []byte{1, 2, 3, 4, 5, 6, 7, 8}},
	} {
		if err := e.EnqueueConfig(key, 1, w); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if got := e.cfgDoc[key]; !bytes.Equal(got.body, served.body) || !bytes.Equal(got.etag, served.etag) {
			t.Errorf("%s: the refused write moved the document", name)
		}
	}
	if err := e.EnqueueCalibOffsetDbg2(key, 1, centi); err == nil {
		t.Error("the d/dbg2 offset write to the wall thermostat was accepted")
	}
	if n := e.Pending(key); n != 0 {
		t.Errorf("pending=%d after the refused writes, want none", n)
	}

	restarted := NewEmu(in.EmuConfig(), nil)
	if err := restarted.RestoreDesiredState(e.ExportDesiredState()); err != nil {
		t.Fatal(err)
	}
	if got := restarted.cfgDoc[key]; !bytes.Equal(got.body, served.body) || !bytes.Equal(got.etag, served.etag) {
		t.Error("the served document differs after a restart")
	}
	if m := thermostatConfigGET(t, in, restarted, ru, 0x7604, old); m.Code != "2.05" || !bytes.Contains(m.Payload, mustHex("014002ff6a")) {
		t.Errorf("after a restart, conditional GET with the old ETag = %s %x, want 2.05 with 0140=ff6a", m.Code, m.Payload)
	}
}

// TestThermostatConfigWriteNeedsDocument: a wall thermostat whose interface
// module id is not known is served no document, so a calibOffset write has
// nothing to go into and is refused.
func TestThermostatConfigWriteNeedsDocument(t *testing.T) {
	in := synthInstall(t)
	ru, _ := synthDevices(t, in)
	e := NewEmu(in.EmuConfig(), nil)
	centi := -150
	if err := e.EnqueueConfig(hexEUI(ru), 1, ConfigWrite{CalibOffset: &centi}); err == nil {
		t.Error("a write to a thermostat with no served document was accepted")
	}
	if _, ok := e.cfgDoc[hexEUI(ru)]; ok {
		t.Error("the refused write created a document")
	}
}
