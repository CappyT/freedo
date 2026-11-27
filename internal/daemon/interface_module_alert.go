package daemon

import (
	"time"

	"freedo/tado"
)

const (
	// interfaceModuleAlertAfter is how long a wall thermostat may stay without an
	// interface module id and without a boiler report before health raises the
	// alert. A thermostat with a known id reports within a few minutes.
	interfaceModuleAlertAfter = 10 * time.Minute
	// alertSetInterfaceModule names the route that clears the alert:
	// PUT /api/install/devices/{ref}/interface_module.
	alertSetInterfaceModule = "setInterfaceModule"
)

// noteUnknownModules tracks the wall thermostats that have no interface module
// id and no boiler report, and sent a frame within the offline limit. A
// thermostat that is silent needs a battery or a radio link, not an id. It
// returns, by ref, those that have been in that state for
// interfaceModuleAlertAfter since the daemon first saw them. It runs on the
// engine goroutine.
func (d *daemon) noteUnknownModules(e *tado.EmuState) map[string]bool {
	inst := d.inst.Load()
	if inst == nil {
		return nil
	}
	now := d.retryNow()
	stalled := map[string]bool{}
	waiting := map[string]bool{}
	for i := range inst.Devices {
		dev := &inst.Devices[i]
		if dev.Kind != tado.KindThermostat || e.InterfaceModuleState(dev).Module != nil || e.BoilerReported(dev) {
			continue
		}
		if seen := e.LastSeen(dev); seen.IsZero() || now.Sub(seen) > inst.Options.OfflineAfter() {
			continue
		}
		ref := dev.Ref()
		waiting[ref] = true
		since, ok := d.moduleUnknownSince[ref]
		if !ok {
			since = now
			d.moduleUnknownSince[ref] = since
		}
		if now.Sub(since) >= interfaceModuleAlertAfter {
			stalled[ref] = true
		}
	}
	for ref := range d.moduleUnknownSince {
		if !waiting[ref] {
			delete(d.moduleUnknownSince, ref)
		}
	}
	return stalled
}
