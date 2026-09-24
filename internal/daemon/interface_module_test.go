package daemon

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"freedo/internal/install"
	"freedo/internal/radio"
	"freedo/tado"
)

// setThermostatModule gives the fixture thermostat ref a learned interface
// module id through the durable state, the way a restart brings one back.
func setThermostatModule(t *testing.T, e *tado.EmuState, ref string, module uint16) {
	t.Helper()
	dev, err := defaultInstall.Resolve(ref)
	if err != nil {
		t.Fatal(err)
	}
	state := e.ExportDesiredState()
	state.InterfaceModule[hex.EncodeToString(dev.WireEUI())] = module
	if err := e.RestoreDesiredState(state); err != nil {
		t.Fatal(err)
	}
}

// thermostatModuleReport is a synthetic h/<home>/hvac/codes PUT from the
// device euiHex that carries tag 015d.
func thermostatModuleReport(t *testing.T, in *tado.Install, euiHex string, module uint16, mid uint16) []byte {
	t.Helper()
	wire, err := hex.DecodeString(euiHex)
	if err != nil {
		t.Fatal(err)
	}
	pkt, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: mid, Token: []byte{0x5d},
		Path:    []string{"h", strconv.FormatUint(uint64(in.HomeID), 10), "hvac", "codes"},
		Payload: tado.TLV{Tag: "015d", Val: []byte{byte(module >> 8), byte(module)}}.Encode(),
		SrcEUI:  wire, DstEUI: in.CoordinatorEUI(), SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.BuildData(in.NetworkKey(), byte(mid), in.CoordinatorEUI(), wire[:5],
		tado.WrapAppPayload(pkt, uint32(mid), nil))
	if err != nil {
		t.Fatal(err)
	}
	return frame.Bytes()
}

// reportThermostatModule feeds the declared thermostat's hvac/codes report
// through the daemon's frame funnel.
func (a *installAPI) reportThermostatModule(module uint16) {
	a.t.Helper()
	frame := thermostatModuleReport(a.t, a.d.inst.Load(), apiThermostatEUI, module, 0x5d01)
	if _, err := a.d.do(func(*tado.EmuState) (any, error) {
		a.d.handleFrame(frame, true)
		return nil, nil
	}); err != nil {
		a.t.Fatal(err)
	}
}

// TestConfigAPIRejectsInterfaceModule: tag 015d is not writable through the
// device config API, whatever value the body carries.
func TestConfigAPIRejectsInterfaceModule(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{TxCounter: 100000}, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	for _, value := range []any{112, 71, 0, nil} {
		var res struct {
			Queued bool   `json:"queued"`
			Error  string `json:"error"`
		}
		body := map[string]any{"reportIvlSec": value, "id": 10}
		code := postJSON(t, http.MethodPost, srv.URL+"/api/nodes/a602/config", body, &res)
		if code != http.StatusBadRequest || res.Queued || !strings.Contains(res.Error, "interface module") {
			t.Errorf("reportIvlSec=%v: got %d %+v, want 400 naming the interface module", value, code, res)
		}
	}
	if n, err := d.do(func(e *tado.EmuState) (any, error) { return e.Pending("a602"), nil }); err != nil || n.(int) != 0 {
		t.Errorf("pending after the refusals = %v (%v), want 0", n, err)
	}
}

// TestThermostatModuleLearnedAndPersisted: until the thermostat reports tag 015d
// the health flag names it and its d/config GET gets no document. The report
// is stored at once, and a restart serves the reported id.
func TestThermostatModuleLearnedAndPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := install.WriteInstall(path, defaultInstall); err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t, path)
	thermostat, err := defaultInstall.Resolve(dhwNode)
	if err != nil {
		t.Fatal(err)
	}
	wire := thermostat.WireEUI()
	key := hex.EncodeToString(wire)

	emu := bootEmuFromStore(t, store)
	if got := emu.UnknownInterfaceModules(); len(got) != 1 || !strings.Contains(got[0], dhwNode) {
		t.Fatalf("unknown modules = %v, want the thermostat", got)
	}
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	d.state = store
	health := d.healthEngine(emu, false)
	if got, _ := health["interfaceModuleUnknown"].([]string); len(got) != 1 || !strings.Contains(got[0], dhwNode) {
		t.Errorf("health interfaceModuleUnknown = %v, want the thermostat", health["interfaceModuleUnknown"])
	}
	if answer := deviceConfigGET(t, emu, wire, 0x9101, nil); answer.Code != "5.03" || len(answer.Payload) != 0 {
		t.Errorf("GET before the report = %s with %d bytes, want an empty 5.03", answer.Code, len(answer.Payload))
	}

	d.handleFrame(thermostatModuleReport(t, defaultInstall, key, 71, 0x9102), true)
	if _, ok := d.healthEngine(emu, false)["interfaceModuleUnknown"]; ok {
		t.Error("the health flag outlives the report")
	}
	stored, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.InterfaceModule[key]; got != 71 {
		t.Fatalf("stored interface module = %d, want 71 saved on the report", got)
	}

	restarted := bootEmuFromStore(t, store)
	if got := restarted.UnknownInterfaceModules(); len(got) != 0 {
		t.Errorf("unknown modules after the restart = %v, want none", got)
	}
	// The thermostat's document spans several fragments, so the served bytes
	// are read from the document the GET answers with.
	var module []string
	for _, tlv := range tado.ParseTLVs(restarted.ExportDesiredState().Config[key].Body) {
		if tlv.Tag == "015d" {
			module = append(module, hex.EncodeToString(tlv.Val))
		}
	}
	if len(module) != 1 || module[0] != "0047" {
		t.Errorf("served 015d = %v, want the reported 0047 alone", module)
	}
}

// TestStoredRefusedModuleIsDropped: a database holding the valve's 112 for the
// thermostat restores and boots, health lists the drop, and the thermostat is
// served no d/config until it reports again.
func TestStoredRefusedModuleIsDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := install.WriteInstall(path, defaultInstall); err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t, path)
	thermostat, err := defaultInstall.Resolve(dhwNode)
	if err != nil {
		t.Fatal(err)
	}
	wire := thermostat.WireEUI()
	key := hex.EncodeToString(wire)
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	state.InterfaceModule[key] = 112
	if err := store.Save(context.Background(), state, 1); err != nil {
		t.Fatal(err)
	}

	emu := bootEmuFromStore(t, store)
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	d.state = store
	health := d.healthEngine(emu, false)
	issues, _ := health["interfaceModuleIssues"].([]tado.InterfaceModuleIssue)
	if len(issues) != 1 || !issues[0].Refused || issues[0].Module != 112 || !strings.Contains(issues[0].Device, dhwNode) {
		t.Errorf("health interfaceModuleIssues = %+v, want the drop of 112", issues)
	}
	if got, _ := health["interfaceModuleUnknown"].([]string); len(got) != 1 || !strings.Contains(got[0], dhwNode) {
		t.Errorf("health interfaceModuleUnknown = %v, want the thermostat", health["interfaceModuleUnknown"])
	}
	if answer := deviceConfigGET(t, emu, wire, 0x9301, nil); answer.Code != "5.03" || len(answer.Payload) != 0 {
		t.Errorf("GET after the drop = %s with %d bytes, want an empty 5.03", answer.Code, len(answer.Payload))
	}
}

// TestThermostatModuleIssueInHealth: a stub id is learned and flagged in the
// health report, and the boiler view names its family.
func TestThermostatModuleIssueInHealth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := install.WriteInstall(path, defaultInstall); err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t, path)
	thermostat, err := defaultInstall.Resolve(dhwNode)
	if err != nil {
		t.Fatal(err)
	}
	emu := bootEmuFromStore(t, store)
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	d.state = store
	d.handleFrame(thermostatModuleReport(t, defaultInstall, hex.EncodeToString(thermostat.WireEUI()), 202, 0x9201), true)

	issues, _ := d.healthEngine(emu, false)["interfaceModuleIssues"].([]tado.InterfaceModuleIssue)
	if len(issues) != 1 || issues[0].Module != 202 || issues[0].Refused || issues[0].Family != "stub" {
		t.Errorf("health interfaceModuleIssues = %+v, want a stub warning", issues)
	}
	if b := emu.Snapshot().Boiler; b.BusType != "stub" || b.InterfaceModule == nil || *b.InterfaceModule != 202 {
		t.Errorf("boiler bus=%q module=%v, want stub and 202", b.BusType, b.InterfaceModule)
	}
}
