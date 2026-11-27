package daemon

import (
	"errors"
	"net/http"

	"freedo/tado"
)

var errNotThermostat = errors.New("only a wall thermostat has an interface module id")

// refError marks a failed lookup of the ref, which answers 404 or 400 as every
// other route does.
type refError struct{ error }

// moduleDevice resolves the ref of an interface module route against the
// installation the engine holds now, so a device deleted in between is
// unknown. It runs on the engine goroutine. Only a wall thermostat has an
// interface module id.
func (d *daemon) moduleDevice(ref string) (*tado.Device, error) {
	dev, err := d.resolve(ref)
	if err != nil {
		return nil, refError{err}
	}
	if dev.Kind != tado.KindThermostat {
		return nil, errNotThermostat
	}
	return dev, nil
}

// writeModuleErr maps the errors of an interface module route to a status.
func writeModuleErr(w http.ResponseWriter, err error) bool {
	var ref refError
	var held *tado.InterfaceModuleHeldError
	switch {
	case errors.As(err, &ref):
		writeResolveErr(w, ref.error)
	case errors.As(err, &held):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		return writeHandlerErr(w, err)
	}
	return true
}

// getInstallDeviceInterfaceModule answers the id held for a wall thermostat
// and the ids that the PUT of the same route accepts.
func (d *daemon) getInstallDeviceInterfaceModule(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		dev, err := d.moduleDevice(ref)
		if err != nil {
			return nil, err
		}
		return e.InterfaceModuleState(dev), nil
	})
	if writeModuleErr(w, err) {
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
// thermostat image passes, and an id the thermostat holds is replaced only when
// the body names it in current.
func (d *daemon) putInstallDeviceInterfaceModule(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Module  *int `json:"module"`
		Current *int `json:"current"`
	}
	if !decodeBody(w, r, &p) {
		return
	}
	if p.Module == nil || *p.Module < 0 || *p.Module > 0xffff {
		writeErr(w, http.StatusBadRequest, "module is required: an interface module id from 0 to 65535")
		return
	}
	var current *uint16
	if p.Current != nil {
		if *p.Current < 0 || *p.Current > 0xffff {
			writeErr(w, http.StatusBadRequest, "current is the held interface module id, from 0 to 65535, or null")
			return
		}
		c := uint16(*p.Current)
		current = &c
	}
	ref := r.PathValue("ref")
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		dev, err := d.moduleDevice(ref)
		if err != nil {
			return nil, err
		}
		if err := e.SetInterfaceModule(dev, uint16(*p.Module), current); err != nil {
			return nil, err
		}
		return e.InterfaceModuleState(dev), nil
	})
	if writeModuleErr(w, d.persist(err)) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}
