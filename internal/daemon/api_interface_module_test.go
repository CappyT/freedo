package daemon

import (
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"testing"

	"freedo/tado"
)

func moduleRoute(ref string) string {
	return "/api/install/devices/" + ref + "/interface_module"
}

// moduleIssues reads the health list of refused and suspicious ids on the
// engine goroutine.
func (a *installAPI) moduleIssues() []tado.InterfaceModuleIssue {
	a.t.Helper()
	v, err := a.d.do(func(e *tado.EmuState) (any, error) { return e.InterfaceModuleIssues(), nil })
	if err != nil {
		a.t.Fatal(err)
	}
	return v.([]tado.InterfaceModuleIssue)
}

func (a *installAPI) thermostatKey() string {
	a.t.Helper()
	dev, err := a.d.inst.Load().Resolve(apiThermostatEUI)
	if err != nil {
		a.t.Fatal(err)
	}
	return hex.EncodeToString(dev.WireEUI())
}

func TestInterfaceModuleRouteSetsAndPersistsTheID(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()

	code, res := a.call(http.MethodPut, moduleRoute(apiThermostatEUI), map[string]any{"module": 80})
	if code != http.StatusOK || res["module"] != float64(80) || res["family"] != "ebus_a" || res["source"] != "operator" {
		t.Fatalf("PUT = %d %v, want 200 with module 80, ebus_a, operator", code, res)
	}
	v, err := a.d.do(func(e *tado.EmuState) (any, error) { return e.UnknownInterfaceModules(), nil })
	if err != nil || len(v.([]string)) != 0 {
		t.Errorf("unknown modules after the PUT = %v (%v), want none", v, err)
	}

	stored, err := a.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.InterfaceModule[a.thermostatKey()]; got != 80 {
		t.Fatalf("stored interface module = %d, want 80 saved by the PUT", got)
	}
	restarted := bootEmuFromStore(t, a.store)
	if got := restarted.UnknownInterfaceModules(); len(got) != 0 {
		t.Errorf("unknown modules after the restart = %v, want none", got)
	}
	dev, err := a.d.inst.Load().Resolve(apiThermostatEUI)
	if err != nil {
		t.Fatal(err)
	}
	if st := restarted.InterfaceModuleState(dev); st.Module == nil || *st.Module != 80 {
		t.Errorf("state after the restart = %+v, want module 80", st)
	}
	var served []string
	for _, tlv := range tado.ParseTLVs(restarted.ExportDesiredState().Config[a.thermostatKey()].Body) {
		if tlv.Tag == "015d" {
			served = append(served, hex.EncodeToString(tlv.Val))
		}
	}
	if len(served) != 1 || served[0] != "0050" {
		t.Errorf("served 015d after the restart = %v, want 0050 alone", served)
	}
}

func TestInterfaceModuleRouteRefusals(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	for _, tc := range []struct {
		name, ref, body string
		want            int
		message         string
	}{
		{"a valve", apiValveEUI, `{"module": 80}`, http.StatusBadRequest, "wall thermostat"},
		{"an unknown ref", "ffffffffffffffff", `{"module": 80}`, http.StatusNotFound, ""},
		{"the firmware default", apiThermostatEUI, `{"module": 0}`, http.StatusBadRequest, "firmware default"},
		{"the valve id", apiThermostatEUI, `{"module": 112}`, http.StatusBadRequest, "valve"},
		{"the teardown value", apiThermostatEUI, `{"module": 32767}`, http.StatusBadRequest, "tears"},
		{"an id no image answers", apiThermostatEUI, `{"module": 999}`, http.StatusBadRequest, "no known wall thermostat image"},
		{"a value above 16 bits", apiThermostatEUI, `{"module": 70000}`, http.StatusBadRequest, "0 to 65535"},
		{"the stub image", apiThermostatEUI, `{"module": 202}`, http.StatusBadRequest, "stub image"},
		{"a current above 16 bits", apiThermostatEUI, `{"module": 80, "current": 70000}`, http.StatusBadRequest, "current"},
		{"a negative value", apiThermostatEUI, `{"module": -1}`, http.StatusBadRequest, "0 to 65535"},
		{"no module", apiThermostatEUI, `{}`, http.StatusBadRequest, "module is required"},
		{"an unknown field", apiThermostatEUI, `{"module": 80, "force": true}`, http.StatusBadRequest, "force"},
	} {
		code, res := a.raw(http.MethodPut, moduleRoute(tc.ref), tc.body)
		if code != tc.want || !strings.Contains(res["error"].(string), tc.message) {
			t.Errorf("%s: PUT = %d %v, want %d naming %q", tc.name, code, res, tc.want, tc.message)
		}
	}
	code, res := a.call(http.MethodGet, moduleRoute(apiThermostatEUI), nil)
	if code != http.StatusOK || res["module"] != nil || res["source"] != nil {
		t.Errorf("GET after the refusals = %d %v, want no id held", code, res)
	}
	if code, _ := a.call(http.MethodGet, moduleRoute(apiValveEUI), nil); code != http.StatusBadRequest {
		t.Errorf("GET on a valve = %d, want 400", code)
	}
}

func TestInterfaceModuleRouteGETListsTheAcceptedIDs(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	code, res := a.call(http.MethodGet, moduleRoute(apiThermostatEUI), nil)
	if code != http.StatusOK || res["module"] != nil || res["family"] != "unknown" || res["source"] != nil {
		t.Fatalf("GET before = %d %v, want no id and family unknown", code, res)
	}
	known, _ := res["known"].([]any)
	ids := make([]int, 0, len(known))
	var held any
	for _, item := range known {
		entry := item.(map[string]any)
		id := int(entry["module"].(float64))
		ids = append(ids, id)
		if entry["family"] != tado.InterfaceModuleFamily(uint16(id)) {
			t.Errorf("known %d has family %v", id, entry["family"])
		}
		// Every listed id must pass the PUT.
		if code, body := a.call(http.MethodPut, moduleRoute(apiThermostatEUI), map[string]any{"module": id, "current": held}); code != http.StatusOK {
			t.Errorf("PUT of the listed id %d = %d %v", id, code, body)
		}
		held = id
	}
	if !sort.IntsAreSorted(ids) || len(ids) < 10 {
		t.Errorf("known ids = %v, want a sorted list of the table", ids)
	}
	for _, id := range ids {
		if id == 202 || id == 0 {
			t.Errorf("known ids hold %d, which the route does not offer", id)
		}
	}

	if code, _ := a.call(http.MethodPut, moduleRoute(apiThermostatEUI), map[string]any{"module": 71, "current": held}); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	_, res = a.call(http.MethodGet, moduleRoute(apiThermostatEUI), nil)
	if res["module"] != float64(71) || res["family"] != "opentherm" || res["source"] != "operator" {
		t.Errorf("GET after = %v, want 71, opentherm, operator", res)
	}
}

func TestOperatorModuleLosesToAReportWithAWarning(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	if code, _ := a.call(http.MethodPut, moduleRoute(apiThermostatEUI), map[string]any{"module": 71}); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	if issues := a.moduleIssues(); len(issues) != 0 {
		t.Fatalf("issues after the PUT = %+v, want none", issues)
	}

	a.reportThermostatModule(80)
	_, res := a.call(http.MethodGet, moduleRoute(apiThermostatEUI), nil)
	if res["module"] != float64(80) || res["source"] != "hvac/codes" {
		t.Errorf("GET after the report = %v, want 80 from hvac/codes", res)
	}
	issues := a.moduleIssues()
	if len(issues) != 1 || issues[0].Module != 80 || issues[0].Refused || !strings.Contains(issues[0].Reason, "operator id 71") {
		t.Errorf("issues = %+v, want one warning that the report replaces the operator id 71", issues)
	}
}

func TestReportedModuleReplacedByAReportRaisesNoWarning(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	a.reportThermostatModule(71)
	a.reportThermostatModule(80)
	if issues := a.moduleIssues(); len(issues) != 0 {
		t.Errorf("issues = %+v, want none: only an operator id is guarded", issues)
	}
}

func TestOperatorModuleSameAsTheReportRaisesNoWarning(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	if code, _ := a.call(http.MethodPut, moduleRoute(apiThermostatEUI), map[string]any{"module": 71}); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	a.reportThermostatModule(71)
	if issues := a.moduleIssues(); len(issues) != 0 {
		t.Errorf("issues = %+v, want none for a report that agrees", issues)
	}
	_, res := a.call(http.MethodGet, moduleRoute(apiThermostatEUI), nil)
	if res["source"] != "hvac/codes" {
		t.Errorf("source = %v, want the report that confirmed the id", res["source"])
	}
}
