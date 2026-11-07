package tado

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// Boiler bus catalogue tests. Every body is synthetic.

const hvacNode = "05a2cafa07c51b00"

func hvacGroup(b BoilerView, res string) map[string]*float64 {
	switch res {
	case resHVACMon:
		return b.Mon
	case resHVACDHW:
		return b.DHW
	case resHVACHP:
		return b.HeatPump
	case resHVACHC:
		return b.HeatingCircuit
	case resHVACMaint:
		return b.Maintenance
	case resHVACConfig:
		return b.Config
	}
	return nil
}

func sized(v uint64, size int) []byte {
	out := make([]byte, size)
	for i := size - 1; i >= 0; i-- {
		out[i] = byte(v)
		v >>= 8
	}
	return out
}

func hvacReport(e *EmuState, res string, tlvs ...TLV) {
	e.cacheTLVs(e.node(mustB(hvacNode)), tlvs, "h/1/"+res, ClassData, "thermostat", "bridge", "")
}

// TestHVACCatalogue: each resource reports every attribute of its subset at
// its catalogue width, and the view scales each one into its group.
func TestHVACCatalogue(t *testing.T) {
	e, log, _ := newTestEmu(t)
	byRes := map[string][]TLV{}
	for i, h := range hvacTags {
		byRes[h.res] = append(byRes[h.res], TLV{Tag: h.tag, Val: sized(uint64(i+1), h.size)})
	}
	if len(byRes) != 6 {
		t.Fatalf("catalogue covers %d resources, want 6", len(byRes))
	}
	for res, tlvs := range byRes {
		hvacReport(e, res, tlvs...)
	}
	if n := countPrefix(log.Counts(), rsnUnknownTag); n != 0 {
		t.Errorf("unknown tags = %d, want 0: %v", n, log.Counts())
	}
	b := e.Snapshot().Boiler
	for i, h := range hvacTags {
		want := float64(i+1) / h.scale
		if got := hvacGroup(b, h.res)[h.name]; !isPtr(got, want) {
			t.Errorf("%s %s (%s) = %v, want %v", h.res, h.tag, h.name, got, want)
		}
		if strings.Contains(strings.ToLower(h.name), "opentherm") || strings.HasPrefix(h.name, "ot") {
			t.Errorf("%s name %q names OpenTherm", h.tag, h.name)
		}
	}
	for _, sentinel := range [][]byte{{0x7f}, {0x7f, 0xff}, {0x7f, 0xff, 0xff, 0xff}} {
		if _, ok := (hvacTag{scale: 1}).value(sentinel); ok {
			t.Errorf("sentinel %x decoded as a value", sentinel)
		}
	}
}

// TestHVACUnsupported: a tag 0481 entry {tag, 02} turns the attribute to null in
// the view and in the JSON, per resource, even after an earlier value.
func TestHVACUnsupported(t *testing.T) {
	e, _, _ := newTestEmu(t)
	hvacReport(e, resHVACMon,
		TLV{Tag: "044c", Val: []byte{0x0f, 0xa0}},
		TLV{Tag: "044d", Val: []byte{0x0b, 0xb8}},
	)
	hvacReport(e, resHVACMon,
		TLV{Tag: "0481", Val: mustB("044d02044f02")},
		TLV{Tag: "0452", Val: []byte{0x00, 0x20}},
	)
	hvacReport(e, resHVACMaint, TLV{Tag: "0481", Val: mustB("046202046302")})

	b := e.Snapshot().Boiler
	if b.ReturnC != nil || b.OutsideC != nil || b.ChPressureBar != nil || b.BurnerStarts != nil {
		t.Errorf("return=%v outside=%v pressure=%v starts=%v, want null", b.ReturnC, b.OutsideC, b.ChPressureBar, b.BurnerStarts)
	}
	if !isPtr(b.FlowC, 40) || !isPtr(b.ModulationPct, 32) {
		t.Errorf("flow=%v mod=%v, want 40 and 32", b.FlowC, b.ModulationPct)
	}
	want := []string{"burner_starts", "ch_pressure_bar", "outside_C", "return_water_C"}
	if !slices.Equal(b.Unsupported, want) {
		t.Errorf("unsupported = %v, want %v", b.Unsupported, want)
	}
	if v, ok := b.Mon["return_water_C"]; !ok || v != nil {
		t.Errorf("Mon return_water_C = %v present=%v, want a null entry", v, ok)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"ReturnC":null`, `"OutsideC":null`, `"FlowC":40`, `"ModulationPct":32`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("JSON lacks %s: %s", field, raw)
		}
	}

	// A new hvac/mon list replaces the old one and leaves hvac/maint alone.
	hvacReport(e, resHVACMon, TLV{Tag: "0481", Val: mustB("044f02")})
	b = e.Snapshot().Boiler
	if !isPtr(b.ReturnC, 30) || b.OutsideC != nil {
		t.Errorf("return=%v outside=%v, want 30 and null", b.ReturnC, b.OutsideC)
	}
	if want := []string{"burner_starts", "ch_pressure_bar", "outside_C"}; !slices.Equal(b.Unsupported, want) {
		t.Errorf("unsupported = %v, want %v", b.Unsupported, want)
	}

	// A body with no URI takes the resource of the attributes its list names.
	e.cacheTLVs(e.node(mustB(hvacNode)), []TLV{{Tag: "0481", Val: mustB("045a02")}}, "", ClassData, "thermostat", "bridge", "")
	if b = e.Snapshot().Boiler; b.DHWC != nil || !slices.Contains(b.Unsupported, "dhw_temp_C") {
		t.Errorf("dhw=%v unsupported=%v, want null and dhw_temp_C listed", b.DHWC, b.Unsupported)
	}
}

// TestHVACFaultCodes: hvac/codes carries its codes as repeated 0460 u32, and
// each report replaces the state. A high half of 0xffff is a diagnostic code, 0xff00
// a fault without an OEM code, any other value the OEM fault code.
func TestHVACFaultCodes(t *testing.T) {
	code := func(v uint32) TLV {
		return TLV{Tag: "0460", Val: []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}}
	}
	for _, tc := range []struct {
		name  string
		codes []TLV
		fault bool
		known []uint32
		diag  *uint16
	}{
		{"fault 42", []TLV{code(0x002affff)}, true, []uint32{42}, nil},
		{"fault without a code", []TLV{code(0xff00ffff)}, true, []uint32{}, nil},
		{"diagnostic 0 is no fault", []TLV{code(0xffff0000)}, false, []uint32{}, new(uint16(0))},
		{"diagnostic 291", []TLV{code(0xffff0123)}, false, []uint32{}, new(uint16(291))},
		{"skipped", []TLV{code(0xffffffff), code(0x7fffffff)}, false, []uint32{}, nil},
		{"two faults and a diagnostic", []TLV{code(0x002affff), code(0xff00ffff), code(0x0085ffff), code(0xffff0123)}, true, []uint32{42, 133}, new(uint16(291))},
		{"three bytes drop the zero byte", []TLV{{Tag: "0460", Val: []byte{0x2a, 0xff, 0xff}}}, true, []uint32{42}, nil},
		{"two bytes are fault code 0", []TLV{{Tag: "0460", Val: []byte{0xff, 0xff}}}, true, []uint32{0}, nil},
		{"five bytes are an unknown fault", []TLV{{Tag: "0460", Val: []byte{0, 0, 0x2a, 0xff, 0xff}}}, true, []uint32{}, nil},
		{"empty value is an unknown fault", []TLV{{Tag: "0460", Val: nil}}, true, []uint32{}, nil},
		{"duplicate codes", []TLV{code(0x002affff), code(0x002affff)}, true, []uint32{42}, nil},
		{"no entry", nil, false, []uint32{}, nil},
	} {
		e, _, _ := newTestEmu(t)
		if b := e.Snapshot().Boiler; b.Fault != nil || b.FaultCodes != nil || b.DiagnosticCode != nil {
			t.Fatalf("%s: before a report fault=%v codes=%v diag=%v, want all null", tc.name, b.Fault, b.FaultCodes, b.DiagnosticCode)
		}
		hvacReport(e, resHVACCodes, append([]TLV{{Tag: "015d", Val: be16(71)}}, tc.codes...)...)
		b := e.Snapshot().Boiler
		if !b.Present || b.Fault == nil || *b.Fault != tc.fault || !slices.Equal(b.FaultCodes, tc.known) || b.FaultCodes == nil {
			t.Errorf("%s: present=%v fault=%v codes=%v, want fault=%v codes=%v",
				tc.name, b.Present, b.Fault, b.FaultCodes, tc.fault, tc.known)
		}
		if (b.DiagnosticCode == nil) != (tc.diag == nil) || (tc.diag != nil && *b.DiagnosticCode != *tc.diag) {
			t.Errorf("%s: diagnostic %v, want %v", tc.name, b.DiagnosticCode, tc.diag)
		}
	}

	e, _, _ := newTestEmu(t)
	hvacReport(e, resHVACCodes, TLV{Tag: "015d", Val: be16(71)}, code(0x002affff), code(0xffff0123))
	hvacReport(e, resHVACCodes, TLV{Tag: "015d", Val: be16(71)})
	if b := e.Snapshot().Boiler; b.Fault == nil || *b.Fault || len(b.FaultCodes) != 0 || b.DiagnosticCode != nil {
		t.Errorf("after a clean report: fault=%v codes=%v diag=%v, want false, [] and null", b.Fault, b.FaultCodes, b.DiagnosticCode)
	}
}

// TestHVACBusDetectIsNotBusType: 0167 is a detect flag, the family comes from
// the learned 015d.
func TestHVACBusDetectIsNotBusType(t *testing.T) {
	in := synthInstall(t)
	ru, _ := synthDevices(t, in)
	e := NewEmu(in.EmuConfig(), nil)
	e.cacheTLVs(e.node(ru), []TLV{{Tag: "0167", Val: []byte{2}}}, "h/1/hvac/det/bus", ClassData, "thermostat", "bridge", "")
	if b := e.Snapshot().Boiler; b.BusType != "" || !isPtr(b.BusDetect, 2) {
		t.Errorf("bus=%q detect=%v, want no family and 2", b.BusType, b.BusDetect)
	}
	e.OnFrame(moduleReport(t, in, ru, 121, 0x7401))
	b := e.Snapshot().Boiler
	if b.BusType != "ebus_b" || !isPtr(b.InterfaceModule, 121) {
		t.Errorf("bus=%q module=%v, want ebus_b and 121", b.BusType, b.InterfaceModule)
	}
}

func TestInterfaceModuleFamily(t *testing.T) {
	families := map[string][]uint16{
		"opentherm": {71},
		"ebus_a":    {0, 1, 5, 6, 7, 9, 80, 83, 200},
		"ebus_b":    {121},
		"ebus_c":    {4, 122},
		"km":        {20, 22, 23, 24, 50},
		"ht_bus":    {41, 43, 44},
		"ems":       {120},
		"bsb":       {100, 110, 111, 123},
		"scom":      {31, 32},
		"analog":    {3, 10, 11, 12, 30, 40, 42, 60, 61},
		"stub":      {202},
		"unknown":   {2, 8, 13, 70, 112, 124, 201, 0x7fff},
	}
	n := 0
	for family, ids := range families {
		for _, id := range ids {
			if got := InterfaceModuleFamily(id); got != family {
				t.Errorf("family(%d) = %q, want %q", id, got, family)
			}
		}
		if family != "unknown" {
			n += len(ids)
		}
	}
	if n != len(moduleFamily) {
		t.Errorf("table holds %d ids, the test lists %d", len(moduleFamily), n)
	}
}

// TestThermostatModuleAllowlist: an id outside the table is refused and
// flagged, the stub id is learned with a warning, and a relay id after another
// family is learned with a warning that a later report of it keeps.
func TestThermostatModuleAllowlist(t *testing.T) {
	in := synthInstall(t)
	ru, _ := synthDevices(t, in)
	e := NewEmu(in.EmuConfig(), nil)
	issue := func() (InterfaceModuleIssue, bool) {
		issues := e.InterfaceModuleIssues()
		if len(issues) == 0 {
			return InterfaceModuleIssue{}, false
		}
		return issues[0], true
	}
	learned := func() uint16 { return e.ExportDesiredState().InterfaceModule[hexEUI(ru)] }

	e.OnFrame(moduleReport(t, in, ru, 13, 0x7501))
	if got, ok := issue(); !ok || !got.Refused || got.Module != 13 || got.Family != "unknown" {
		t.Errorf("issue after 13 = %+v, want refused unknown 13", got)
	}
	if e.TakeInterfaceModuleLearned() || len(e.UnknownInterfaceModules()) != 1 {
		t.Error("the unknown id 13 was learned")
	}
	e.OnFrame(moduleReport(t, in, ru, 71, 0x7502))
	if _, ok := issue(); ok || learned() != 71 {
		t.Errorf("issues=%v learned=%d after 71, want none and 71", e.InterfaceModuleIssues(), learned())
	}

	e.OnFrame(moduleReport(t, in, ru, 5, 0x7503))
	if got, ok := issue(); !ok || got.Refused || got.Module != 5 || !strings.Contains(got.Reason, "opentherm") {
		t.Errorf("issue after 5 = %+v, want a relay warning naming opentherm", got)
	}
	if learned() != 5 {
		t.Errorf("learned = %d, want 5", learned())
	}
	e.OnFrame(moduleReport(t, in, ru, 5, 0x7504))
	if _, ok := issue(); !ok {
		t.Error("a repeated relay report cleared the warning")
	}

	e.OnFrame(moduleReport(t, in, ru, stubModule, 0x7505))
	if got, ok := issue(); !ok || got.Refused || got.Family != "stub" || learned() != stubModule {
		t.Errorf("issue after 202 = %+v learned=%d, want a stub warning and 202", got, learned())
	}
	e.OnFrame(moduleReport(t, in, ru, 71, 0x7506))
	if _, ok := issue(); ok {
		t.Errorf("issues after 71 = %v, want none", e.InterfaceModuleIssues())
	}

	first := NewEmu(in.EmuConfig(), nil)
	first.OnFrame(moduleReport(t, in, ru, 5, 0x7507))
	if issues := first.InterfaceModuleIssues(); len(issues) != 0 {
		t.Errorf("issues for a first relay report = %v, want none", issues)
	}
}
