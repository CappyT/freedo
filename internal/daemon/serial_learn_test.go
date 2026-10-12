package daemon

import (
	"net/http"
	"testing"
)

// deviceSerial reads the stored serial of one device through the API.
func (a *installAPI) deviceSerial(ref string) any {
	a.t.Helper()
	code, res := a.call(http.MethodGet, "/api/install/devices/"+ref, nil)
	if code != http.StatusOK {
		a.t.Fatalf("GET device %s: got %d %v", ref, code, res)
	}
	dev, ok := res["device"].(map[string]any)
	if !ok {
		a.t.Fatalf("GET device %s: no device in %v", ref, res)
	}
	return dev["serial"]
}

// TestAdoptSerialsFillsAnEmptySerial: a serial learned on air goes into a device
// row that has none, and the running installation takes it. A row that has a
// serial keeps it, and a serial another device holds is refused.
func TestAdoptSerialsFillsAnEmptySerial(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	a.mutate(http.MethodPost, "/api/install/devices",
		map[string]any{"kind": "valve", "eui": apiValveLeftEUI, "label": "kitchen"})
	a.mutate(http.MethodPost, "/api/install/devices",
		map[string]any{"kind": "valve", "eui": apiValveRightEUI, "label": "pantry"})

	a.d.adoptSerials(map[string]string{
		apiValveLeftEUI:  "VA9000000003",
		apiValveEUI:      "VA9000000077",
		apiValveRightEUI: "VA9000000002",
	})

	if got := a.deviceSerial(apiValveLeftEUI); got != "VA9000000003" {
		t.Errorf("learned serial = %v, want VA9000000003", got)
	}
	if dev, err := a.d.inst.Load().Resolve("VA9000000003"); err != nil || dev.Label != "kitchen" {
		t.Errorf("the running installation does not resolve the learned serial: %v %v", dev, err)
	}
	if got := a.deviceSerial(apiValveEUI); got != "VA9000000001" {
		t.Errorf("declared serial = %v, want VA9000000001 kept", got)
	}
	if got := a.deviceSerial(apiValveRightEUI); got == "VA9000000002" {
		t.Error("a serial another device holds was stored twice")
	}
}
