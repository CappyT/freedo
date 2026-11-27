package daemon

import (
	"net/http"

	"freedo/tado"
)

// interfaceModuleDevice resolves the ref of an interface module route. Only a
// wall thermostat has an interface module id.
func (d *daemon) interfaceModuleDevice(w http.ResponseWriter, r *http.Request) (*tado.Device, bool) {
	dev, err := d.resolve(r.PathValue("ref"))
	if err != nil {
		writeResolveErr(w, err)
		return nil, false
	}
	if dev.Kind != tado.KindThermostat {
		writeErr(w, http.StatusBadRequest, "only a wall thermostat has an interface module id")
		return nil, false
	}
	return dev, true
}

// getInstallDeviceInterfaceModule answers the id held for a wall thermostat
// and the ids that the PUT of the same route accepts.
func (d *daemon) getInstallDeviceInterfaceModule(w http.ResponseWriter, r *http.Request) {
	dev, ok := d.interfaceModuleDevice(w, r)
	if !ok {
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) { return e.InterfaceModuleState(dev), nil })
	if writeHandlerErr(w, err) {
		return
	}
	state := v.(tado.InterfaceModuleState)
	writeJSON(w, http.StatusOK, map[string]any{
		"module": state.Module,
		"family": state.Family,
		"source": state.Source,
		"known":  tado.KnownInterfaceModules(),
	})
}

// putInstallDeviceInterfaceModule sets the id of a wall thermostat that lost
// it. A wrong id stops the boiler interface, so only an id of a known wall
// thermostat image passes.
func (d *daemon) putInstallDeviceInterfaceModule(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Module *int `json:"module"`
	}
	if !decodeBody(w, r, &p) {
		return
	}
	if p.Module == nil || *p.Module < 0 || *p.Module > 0xffff {
		writeErr(w, http.StatusBadRequest, "module is required: an interface module id from 0 to 65535")
		return
	}
	dev, ok := d.interfaceModuleDevice(w, r)
	if !ok {
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		if err := e.SetInterfaceModule(dev, uint16(*p.Module)); err != nil {
			return nil, err
		}
		return e.InterfaceModuleState(dev), nil
	})
	if writeHandlerErr(w, d.persist(err)) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}
