package daemon

import (
	"encoding/hex"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"freedo/tado"
)

// modulePUT sends a PUT with a body the caller spells, so current can be
// omitted, null or a number.
func (a *installAPI) modulePUT(body string) (int, map[string]any) {
	a.t.Helper()
	return a.raw(http.MethodPut, moduleRoute(apiThermostatEUI), body)
}

func (a *installAPI) heldModule() (module any, source any) {
	a.t.Helper()
	code, res := a.call(http.MethodGet, moduleRoute(apiThermostatEUI), nil)
	if code != http.StatusOK {
		a.t.Fatalf("GET = %d %v", code, res)
	}
	return res["module"], res["source"]
}

// A thermostat that reported its own id keeps it against a PUT that does not
// name it: the id is the better source.
func TestInterfaceModulePUTNeedsTheHeldIDToReplaceOne(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	a.reportThermostatModule(80)

	code, res := a.modulePUT(`{"module": 71}`)
	msg, _ := res["error"].(string)
	if code != http.StatusConflict || !strings.Contains(msg, "80") || !strings.Contains(msg, "hvac/codes") {
		t.Fatalf("PUT without current = %d %v, want 409 naming 80 and hvac/codes", code, res)
	}
	if module, source := a.heldModule(); module != float64(80) || source != "hvac/codes" {
		t.Errorf("held after the refusal = %v from %v, want 80 from hvac/codes", module, source)
	}
	if code, _ := a.modulePUT(`{"module": 71, "current": null}`); code != http.StatusConflict {
		t.Errorf("PUT with a null current over a held id = %d, want 409", code)
	}
	if code, res := a.modulePUT(`{"module": 71, "current": 5}`); code != http.StatusConflict {
		t.Errorf("PUT with a wrong current = %d %v, want 409", code, res)
	}
	if module, _ := a.heldModule(); module != float64(80) {
		t.Errorf("held after the wrong current = %v, want 80", module)
	}

	code, res = a.modulePUT(`{"module": 71, "current": 80}`)
	if code != http.StatusOK || res["module"] != float64(71) || res["source"] != "operator" {
		t.Errorf("PUT with the held id as current = %d %v, want 200 with 71 from operator", code, res)
	}
	// An operator id is guarded the same way.
	if code, _ := a.modulePUT(`{"module": 80}`); code != http.StatusConflict {
		t.Errorf("PUT over an operator id without current = %d, want 409", code)
	}
}

func TestInterfaceModulePUTWithNoHeldIDTakesNoCurrent(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	if code, res := a.modulePUT(`{"module": 71, "current": 80}`); code != http.StatusConflict ||
		!strings.Contains(res["error"].(string), "holds no interface module id") {
		t.Errorf("PUT with a current and no held id = %d %v, want 409", code, res)
	}
	if module, _ := a.heldModule(); module != nil {
		t.Fatalf("held after the refusal = %v, want none", module)
	}
	if code, res := a.modulePUT(`{"module": 71, "current": null}`); code != http.StatusOK {
		t.Errorf("PUT with a null current and no held id = %d %v, want 200", code, res)
	}
}

// The PUT rebuilds the document the thermostat gets now, not only at the next
// start, and queues the re-read that makes the thermostat fetch it.
func TestInterfaceModulePUTRefreshesTheLiveDocument(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	if code, res := a.modulePUT(`{"module": 80}`); code != http.StatusOK {
		t.Fatalf("PUT = %d %v", code, res)
	}
	key := a.thermostatKey()
	v, err := a.d.do(func(e *tado.EmuState) (any, error) {
		var served []string
		for _, tlv := range tado.ParseTLVs(e.ExportDesiredState().Config[key].Body) {
			if tlv.Tag == "015d" {
				served = append(served, hex.EncodeToString(tlv.Val))
			}
		}
		return struct {
			served []string
			reread bool
		}{served, e.ConfigRereadQueued(key)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := v.(struct {
		served []string
		reread bool
	})
	if len(got.served) != 1 || got.served[0] != "0050" || !got.reread {
		t.Errorf("served 015d = %v, re-read queued = %v, want 0050 alone and a re-read", got.served, got.reread)
	}
}

// The ids that start no boiler bus are refused by the PUT, which exists to
// restore one, and a report of one is learned with a warning.
func TestInterfaceModulePUTRefusesTheIDsWithNoBus(t *testing.T) {
	for _, id := range []int{202, 200} {
		a := newInstallAPI(t)
		a.declareDevices()
		if code, res := a.modulePUT(`{"module": ` + strconv.Itoa(id) + `}`); code != http.StatusBadRequest {
			t.Fatalf("PUT %d = %d %v, want 400", id, code, res)
		}
		if module, _ := a.heldModule(); module != nil {
			t.Errorf("held after the refusal of %d = %v, want none", id, module)
		}
		a.reportThermostatModule(uint16(id))
		if module, _ := a.heldModule(); module != float64(id) {
			t.Errorf("held after a report of %d = %v, want %d", id, module, id)
		}
		if issues := a.moduleIssues(); len(issues) != 1 || issues[0].Refused {
			t.Errorf("issues after a report of %d = %+v, want one warning", id, issues)
		}
	}
}

// waitBlockedIn polls the goroutine dump until a goroutine sits in a frame whose
// name has marker, so a test knows a handler passed its checks and waits for the
// engine.
func waitBlockedIn(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	buf := make([]byte, 1<<20)
	for time.Now().Before(deadline) {
		if strings.Contains(string(buf[:runtime.Stack(buf, true)]), marker) {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("no goroutine reached %s", marker)
}

// A device deleted while the PUT waits for the engine is unknown to the PUT: it
// leaves no id and no document behind.
func TestInterfaceModulePUTResolvesTheRefOnTheEngine(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	dev, err := a.d.inst.Load().Resolve(apiThermostatEUI)
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	blocked := make(chan error, 1)
	go func() {
		_, err := a.d.do(func(*tado.EmuState) (any, error) {
			close(started)
			<-release
			without := *a.d.inst.Load()
			without.Devices = nil
			for _, d := range a.d.inst.Load().Devices {
				if d.Kind != tado.KindThermostat {
					without.Devices = append(without.Devices, d)
				}
			}
			_, _, err := a.d.setInstall(&without)
			return nil, err
		})
		blocked <- err
	}()
	<-started

	type answer struct {
		code int
		res  map[string]any
	}
	done := make(chan answer, 1)
	go func() {
		code, res := a.modulePUT(`{"module": 80}`)
		done <- answer{code, res}
	}()
	waitBlockedIn(t, "putInstallDeviceInterfaceModule")
	close(release)
	if err := <-blocked; err != nil {
		t.Fatal(err)
	}
	if got := <-done; got.code != http.StatusNotFound {
		t.Errorf("PUT after the delete = %d %v, want 404", got.code, got.res)
	}
	v, err := a.d.do(func(e *tado.EmuState) (any, error) {
		_, held := e.ExportDesiredState().Config[hex.EncodeToString(dev.WireEUI())]
		return held || e.InterfaceModuleState(dev).Module != nil, nil
	})
	if err != nil || v.(bool) {
		t.Errorf("state kept for the deleted thermostat = %v (%v), want none", v, err)
	}
}

// A PUT of the held id takes the id over: the answer and the next GET name the
// operator as its source.
func TestInterfaceModulePUTOfTheHeldIDTakesItOver(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	a.reportThermostatModule(71)
	code, res := a.modulePUT(`{"module": 71, "current": 71}`)
	if code != http.StatusOK || res["source"] != "operator" {
		t.Fatalf("PUT of the held id = %d %v, want 200 from operator", code, res)
	}
	if _, source := a.heldModule(); source != "operator" {
		t.Errorf("source after the PUT = %v, want operator", source)
	}
}
