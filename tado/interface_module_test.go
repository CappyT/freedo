package tado

import (
	"encoding/hex"
	"strings"
	"testing"
)

// servedModules returns every tag 015d value of the document held for dev.
func servedModules(e *EmuState, dev []byte) []string {
	var out []string
	for _, tlv := range parseTLVList(e.cfgDoc[hexEUI(dev)].body) {
		if tlv.Tag == interfaceModuleTag {
			out = append(out, hex.EncodeToString(tlv.Val))
		}
	}
	return out
}

func synthDevices(t *testing.T, in *Install) (ru, va []byte) {
	t.Helper()
	thermostat, err := in.Resolve("RU0000000001")
	if err != nil {
		t.Fatal(err)
	}
	valve, err := in.Resolve("VA0000000001")
	if err != nil {
		t.Fatal(err)
	}
	return thermostat.WireEUI(), valve.WireEUI()
}

// moduleReport is the thermostat's h/<home>/hvac/codes PUT carrying tag 015d.
func moduleReport(t *testing.T, in *Install, dev []byte, module uint16, mid uint16) []byte {
	t.Helper()
	return synthDeviceUplink(t, in, dev, CoAPReq{
		Type: "CON", Code: "0.03", MID: mid, Token: []byte{0x4c},
		Path:    homePath(in, "hvac", "codes"),
		Payload: TLV{Tag: interfaceModuleTag, Val: be16(module)}.Encode(),
	})
}

func configGET(t *testing.T, in *Install, e *EmuState, dev []byte, mid uint16) *CoAPMessage {
	t.Helper()
	raw := synthDeviceUplink(t, in, dev, CoAPReq{
		Type: "CON", Code: "0.01", MID: mid, Token: []byte{0x21},
		Path: []string{"d", "RU0000000001", "config"},
	})
	return synthAnswer(t, in, e.OnFrame(raw), mid)
}

// TestValveDocumentCarriesModule112: every valve document freedo serves or
// writes carries 015d=112, and no write can move it.
func TestValveDocumentCarriesModule112(t *testing.T) {
	in := synthInstall(t)
	_, va := synthDevices(t, in)
	e := NewEmu(in.EmuConfig(), nil)
	if got := servedModules(e, va); len(got) != 1 || got[0] != "0070" {
		t.Fatalf("valve 015d = %v, want [0070]", got)
	}
	offset := 50
	if err := e.EnqueueConfig(hexEUI(va), 4, ConfigWrite{CalibOffset: &offset}); err != nil {
		t.Fatal(err)
	}
	if got := servedModules(e, va); len(got) != 1 || got[0] != "0070" {
		t.Errorf("valve 015d after a write = %v, want [0070]", got)
	}
	for _, module := range []uint16{71, 0, interfaceModuleTeardown} {
		if err := e.EnqueueConfig(hexEUI(va), 4, ConfigWrite{InterfaceModuleID: &module}); err == nil {
			t.Errorf("a valve write of 015d=%d was queued", module)
		}
	}
	if got := servedModules(e, va); len(got) != 1 || got[0] != "0070" {
		t.Errorf("valve 015d after the refusals = %v, want [0070]", got)
	}
}

// servedConfigDocument reads the whole d/config document dev is served, block by
// block, with etag in option 4 when it is not nil. It returns the code of the
// first answer, the body and the ETag of block 0.
func servedConfigDocument(t *testing.T, in *Install, e *EmuState, dev []byte, mid uint16, etag []byte) (string, []byte, []byte) {
	t.Helper()
	var body, served []byte
	for block := 0; ; block++ {
		options := []CoAPOption{{Num: 23, Val: []byte{byte(block<<4 | 3)}}}
		if etag != nil {
			options = append(options, CoAPOption{Num: 4, Val: etag})
		}
		raw := synthDeviceUplink(t, in, dev, CoAPReq{
			Type: "CON", Code: "0.01", MID: mid + uint16(block), Token: []byte{0x21},
			Path: []string{"d", "RU0000000001", "config"}, Options: options,
		})
		m := synthAnswer(t, in, e.OnFrame(raw), mid+uint16(block))
		if m.Code != "2.05" {
			return m.Code, m.Payload, nil
		}
		if block == 0 {
			if o, ok := m.Option(4); ok {
				served = append([]byte(nil), o.Val...)
			}
		}
		body = append(body, m.Payload...)
		if o, ok := m.Option(23); !ok || decodeCoAPUint(o.Val)&0x08 == 0 {
			return "2.05", body, served
		}
	}
}

// TestThermostatWithoutModuleServedWithout015d: before the thermostat reports
// tag 015d, freedo holds no document for it, queues no write to it on a reload
// and flags it. Its d/config GET still gets a 2.05: every uplink after d/config
// waits for a 2.05 or 2.03 of it (thermostat 0x1e6dc, response handler
// 0x1e4b4), hvac/codes, which carries 015d, too. The document leaves 015d out,
// and the tag handler (thermostat 0x1e2e8) writes only the tags a body carries,
// so the device keeps its own id. Once the id is learned the ETag moves and the
// next GET gets the document with 015d.
func TestThermostatWithoutModuleServedWithout015d(t *testing.T) {
	in := synthInstall(t)
	ru, _ := synthDevices(t, in)
	e := NewEmu(in.EmuConfig(), nil)
	if _, ok := e.cfgDoc[hexEUI(ru)]; ok {
		t.Fatal("a document is held for a thermostat with no known module")
	}
	if got := e.UnknownInterfaceModules(); len(got) != 1 {
		t.Errorf("unknown modules = %v, want the thermostat", got)
	}
	code, body, etag := servedConfigDocument(t, in, e, ru, 0x7001, nil)
	if code != "2.05" || len(etag) == 0 {
		t.Fatalf("d/config GET = %s with ETag %x, want 2.05 with an ETag", code, etag)
	}
	tlvs := parseTLVList(body)
	if _, ok := tlvU16(tlvs, interfaceModuleTag); ok {
		t.Errorf("the document carries 015d: %x", body)
	}
	if len(tlvs) == 0 {
		t.Error("the document is empty")
	}
	if _, ok := e.cfgDoc[hexEUI(ru)]; ok {
		t.Error("the served document is held as the thermostat's")
	}
	if code, _, _ := servedConfigDocument(t, in, e, ru, 0x7011, etag); code != "2.03" {
		t.Errorf("GET with the served ETag = %s, want 2.03", code)
	}
	if _, _, err := e.SetInstall(synthInstall(t)); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.cfgDoc[hexEUI(ru)]; ok {
		t.Error("a reload built a document for the thermostat")
	}
	if n := e.Pending(hexEUI(ru)); n != 0 {
		t.Errorf("thermostat pending = %d, want 0", n)
	}
	if err := e.EnqueueConfig(hexEUI(ru), 0, ConfigWrite{Resend: true}); err == nil {
		t.Error("a d/config write to the thermostat was queued")
	}

	e.OnFrame(moduleReport(t, in, ru, 71, 0x7021))
	code, body, _ = servedConfigDocument(t, in, e, ru, 0x7031, etag)
	if v, ok := tlvU16(parseTLVList(body), interfaceModuleTag); code != "2.05" || !ok || v != 71 {
		t.Errorf("GET after the report = %s with 015d=%d (present=%t), want 2.05 with 71", code, v, ok)
	}
}

// TestThermostatLearnsModuleFromCodesReport: the hvac/codes report carries the
// thermostat's own 015d, and the document served after it carries that value.
func TestThermostatLearnsModuleFromCodesReport(t *testing.T) {
	in := synthInstall(t)
	ru, _ := synthDevices(t, in)
	e := NewEmu(in.EmuConfig(), nil)
	e.OnFrame(moduleReport(t, in, ru, 71, 0x7101))
	if got := servedModules(e, ru); len(got) != 1 || got[0] != "0047" {
		t.Fatalf("thermostat 015d = %v, want [0047]", got)
	}
	if got := e.UnknownInterfaceModules(); len(got) != 0 {
		t.Errorf("unknown modules = %v, want none", got)
	}
	if !e.TakeInterfaceModuleLearned() || e.TakeInterfaceModuleLearned() {
		t.Error("the learned id is not reported once to the owner")
	}
	if answer := configGET(t, in, e, ru, 0x7102); answer.Code != "2.05" {
		t.Errorf("d/config GET after the report = %s, want 2.05", answer.Code)
	}
	if n := e.Pending(hexEUI(ru)); n != 0 {
		t.Errorf("thermostat pending = %d, want 0: it is served, never written", n)
	}

	// The id survives an export and a restore on a new emulator.
	restarted := NewEmu(in.EmuConfig(), nil)
	if err := restarted.RestoreDesiredState(e.ExportDesiredState()); err != nil {
		t.Fatal(err)
	}
	if got := servedModules(restarted, ru); len(got) != 1 || got[0] != "0047" {
		t.Errorf("thermostat 015d after the restore = %v, want [0047]", got)
	}
}

// TestThermostatRefusesUnsafeModule: 0, 112, 0x7fff and an id outside the
// family table are never learned for the thermostat. A report is refused; the
// durable state restores without the id and flags the drop.
func TestThermostatRefusesUnsafeModule(t *testing.T) {
	in := synthInstall(t)
	ru, _ := synthDevices(t, in)
	for i, module := range []uint16{0, valveInterfaceModule, interfaceModuleTeardown, 13} {
		e := NewEmu(in.EmuConfig(), nil)
		e.OnFrame(moduleReport(t, in, ru, module, 0x7201+uint16(i)))
		if _, ok := e.cfgDoc[hexEUI(ru)]; ok {
			t.Errorf("015d=%d: a report built a thermostat document", module)
		}
		if e.TakeInterfaceModuleLearned() {
			t.Errorf("015d=%d: the report was learned", module)
		}
		state := e.ExportDesiredState()
		state.InterfaceModule[hexEUI(ru)] = module
		restored := NewEmu(in.EmuConfig(), nil)
		if err := restored.RestoreDesiredState(state); err != nil {
			t.Errorf("015d=%d: restore failed: %v", module, err)
		}
		if _, ok := restored.ExportDesiredState().InterfaceModule[hexEUI(ru)]; ok {
			t.Errorf("015d=%d: the stored id was restored", module)
		}
		if _, ok := restored.cfgDoc[hexEUI(ru)]; ok {
			t.Errorf("015d=%d: a restore built a thermostat document", module)
		}
		issues := restored.InterfaceModuleIssues()
		if len(issues) != 1 || !issues[0].Refused || issues[0].Module != module || !strings.HasPrefix(issues[0].Device, "RU0000000001") {
			t.Errorf("015d=%d: issues = %+v, want the drop of the thermostat", module, issues)
		}
	}
}

// TestThermostatLearnsModuleFromCachedDocument: a d/config document held for
// the thermostat gives its 015d when it is one a thermostat may carry. One
// that carries the valve's 112 is dropped and served to nobody.
func TestThermostatLearnsModuleFromCachedDocument(t *testing.T) {
	in := synthInstall(t)
	ru, _ := synthDevices(t, in)
	cached := func(module uint16) DesiredState {
		_, snap, err := mergeConfigDocument(cfgSnapshot{}, ConfigWrite{
			HomeID: &in.HomeID, InterfaceModuleID: &module, OfflineScheduleEnabled: cfgPtr(false),
		}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		state := NewEmu(in.EmuConfig(), nil).ExportDesiredState()
		state.Config[hexEUI(ru)] = DesiredConfigDocument{Body: snap.body, ETag: snap.etag}
		return state
	}

	e := NewEmu(in.EmuConfig(), nil)
	if err := e.RestoreDesiredState(cached(71)); err != nil {
		t.Fatal(err)
	}
	if got := e.ExportDesiredState().InterfaceModule[hexEUI(ru)]; got != 71 {
		t.Errorf("learned module = %d, want 71 from the cached document", got)
	}
	if got := servedModules(e, ru); len(got) != 1 || got[0] != "0047" {
		t.Errorf("thermostat 015d = %v, want [0047]", got)
	}

	stale := NewEmu(in.EmuConfig(), nil)
	if err := stale.RestoreDesiredState(cached(valveInterfaceModule)); err != nil {
		t.Fatal(err)
	}
	if _, ok := stale.cfgDoc[hexEUI(ru)]; ok {
		t.Error("a cached thermostat document carrying 112 is still held")
	}
	if got := stale.UnknownInterfaceModules(); len(got) != 1 {
		t.Errorf("unknown modules = %v, want the thermostat", got)
	}
}
