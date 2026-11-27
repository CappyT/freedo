package daemon

import (
	"slices"
	"testing"
	"time"

	"freedo/tado"
)

const moduleThermostat = "RU1000000001"

func raised(h alertHealth, name, alert string) bool {
	return slices.Contains(h.alerts()[name], alert)
}

// reportBoiler feeds an hvac/mon report from the thermostat.
func reportBoiler(t *testing.T, d *daemon, in *tado.Install, mid uint16) {
	t.Helper()
	feed(t, d, in, moduleThermostat, false, tado.CoAPReq{
		Type: "CON", Code: "0.03", MID: mid, Path: []string{"h", "7654321", "hvac", "mon"},
		Payload: tado.TLV{Tag: "0452", Val: []byte{0, 50}}.Encode(),
	})
}

// heardFrom feeds a frame of the thermostat that carries no boiler report and
// no module id.
func heardFrom(t *testing.T, d *daemon, in *tado.Install, mid uint16) {
	t.Helper()
	deviceReport(t, d, in, moduleThermostat, "sen", mid, battery(3300))
}

func TestHealthAlertsAThermostatWithNoModuleAfterTenMinutes(t *testing.T) {
	clock := newTestClock(windowStart)
	in := windowInstall(t, nil)
	d, url := newAlertDaemon(t, in, clock)
	heardFrom(t, d, in, 0x7101)

	if h := alertHealthOf(t, url); raised(h, moduleThermostat, alertSetInterfaceModule) {
		t.Fatalf("alerts at the start = %v, want none yet", h.alerts())
	}
	clock.advance(interfaceModuleAlertAfter - time.Second)
	if h := alertHealthOf(t, url); raised(h, moduleThermostat, alertSetInterfaceModule) {
		t.Errorf("alerts after 9 minutes 59 seconds = %v, want none", h.alerts())
	}
	clock.advance(time.Second)
	h := alertHealthOf(t, url)
	if !raised(h, moduleThermostat, alertSetInterfaceModule) {
		t.Fatalf("alerts after 10 minutes = %v, want %s on the thermostat", h.alerts(), alertSetInterfaceModule)
	}
	for name, alerts := range h.alerts() {
		if name != moduleThermostat && slices.Contains(alerts, alertSetInterfaceModule) {
			t.Errorf("%s raised %s, only a wall thermostat may", name, alertSetInterfaceModule)
		}
	}

	dev, err := in.Resolve(moduleThermostat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.do(func(e *tado.EmuState) (any, error) { return nil, e.SetInterfaceModule(dev, 71, nil) }); err != nil {
		t.Fatal(err)
	}
	if h := alertHealthOf(t, url); raised(h, moduleThermostat, alertSetInterfaceModule) {
		t.Errorf("alerts after the id is set = %v, want the alert gone", h.alerts())
	}
}

func TestHealthRaisesNoModuleAlertAfterABoilerReport(t *testing.T) {
	clock := newTestClock(windowStart)
	in := windowInstall(t, nil)
	d, url := newAlertDaemon(t, in, clock)
	heardFrom(t, d, in, 0x7102)

	alertHealthOf(t, url)
	clock.advance(5 * time.Minute)
	reportBoiler(t, d, in, 0x7001)
	clock.advance(interfaceModuleAlertAfter)
	if h := alertHealthOf(t, url); raised(h, moduleThermostat, alertSetInterfaceModule) {
		t.Errorf("alerts = %v, want none for a thermostat that sent hvac/mon", h.alerts())
	}
}

func TestHealthRaisesNoModuleAlertWhenTheIDIsKnown(t *testing.T) {
	clock := newTestClock(windowStart)
	in := windowInstall(t, nil)
	d, url := newAlertDaemon(t, in, clock)
	dev, err := in.Resolve(moduleThermostat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.do(func(e *tado.EmuState) (any, error) { return nil, e.SetInterfaceModule(dev, 71, nil) }); err != nil {
		t.Fatal(err)
	}
	alertHealthOf(t, url)
	clock.advance(time.Hour)
	if h := alertHealthOf(t, url); raised(h, moduleThermostat, alertSetInterfaceModule) {
		t.Errorf("alerts = %v, want none for a thermostat with an id", h.alerts())
	}
}

// A thermostat that sent no frame, or none within the offline limit, needs a
// battery or a radio link: the alert would point at the wrong cure.
func TestHealthRaisesNoModuleAlertForASilentThermostat(t *testing.T) {
	clock := newTestClock(windowStart)
	in := windowInstall(t, nil)
	d, url := newAlertDaemon(t, in, clock)

	alertHealthOf(t, url)
	clock.advance(interfaceModuleAlertAfter)
	if h := alertHealthOf(t, url); raised(h, moduleThermostat, alertSetInterfaceModule) {
		t.Errorf("alerts for a thermostat never heard = %v, want none", h.alerts())
	}

	heardFrom(t, d, in, 0x7103)
	alertHealthOf(t, url)
	clock.advance(interfaceModuleAlertAfter)
	if h := alertHealthOf(t, url); !raised(h, moduleThermostat, alertSetInterfaceModule) {
		t.Fatalf("alerts for a thermostat heard 10 minutes ago = %v, want the alert", h.alerts())
	}

	clock.advance(2 * time.Hour)
	h := alertHealthOf(t, url)
	if raised(h, moduleThermostat, alertSetInterfaceModule) || !raised(h, moduleThermostat, "offline") {
		t.Errorf("alerts for an offline thermostat = %v, want offline alone", h.alerts())
	}
}
