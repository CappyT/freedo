package daemon

// api_presets: the program presets, the routes over the registry of
// presets.go. A create names a new preset and copies the active one (or the
// one copy_from names). A rename keeps the name unique without case. A delete
// keeps the active preset and the last one alive. The activation of a preset
// is one action: the database takes the active id, the home programs of the
// preset are served to the zone members, and each zone whose program in force
// moved gets one write, a program block write that carries no 6440.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"freedo/tado"
)

// apiError is a handler rejection that carries its own status: the schedule
// and preset routes answer 404 for an unknown preset or a missing program and
// 409 for a name or state conflict, where the shared mapping of
// writeHandlerErr would say 400.
type apiError struct {
	code int
	msg  string
}

func (e *apiError) Error() string { return e.msg }

// apiErrf is an apiError with a formatted message.
func apiErrf(code int, format string, a ...any) error {
	return &apiError{code: code, msg: fmt.Sprintf(format, a...)}
}

// writeAPIErr is writeHandlerErr plus the status an apiError carries.
func writeAPIErr(w http.ResponseWriter, err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		writeErr(w, ae.code, ae.msg)
		return true
	}
	return writeHandlerErr(w, err)
}

// pathPresetID is the {id} path value of a preset route.
func pathPresetID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeErr(w, http.StatusBadRequest, "a preset id is a number")
		return 0, false
	}
	return id, true
}

// presetView is one preset of the API.
type presetView struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

// presetViews lists the presets in creation order.
func (d *daemon) presetViews() []presetView {
	out := make([]presetView, 0, len(d.presetOrder))
	for _, id := range d.presetOrder {
		p := d.presets[id]
		out = append(out, presetView{ID: p.id, Name: p.name, Active: p.id == d.activePreset})
	}
	return out
}

// getPresets answers the presets and the active one.
func (d *daemon) getPresets(w http.ResponseWriter, r *http.Request) {
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		d.ensurePresets()
		return map[string]any{"presets": d.presetViews()}, nil
	})
	write(w, v, err)
}

// postPresetBody is the body of POST /api/presets. copy_from names the preset
// to copy; absent it is the active one.
type postPresetBody struct {
	Name     *string `json:"name"`
	CopyFrom *int64  `json:"copy_from"`
}

// postPreset creates a preset with a copy of every program of the source
// preset, the active one when copy_from passes no id.
func (d *daemon) postPreset(w http.ResponseWriter, r *http.Request) {
	var b postPresetBody
	if !decodeBody(w, r, &b) {
		return
	}
	if b.Name == nil || strings.TrimSpace(*b.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	name := strings.TrimSpace(*b.Name)
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		d.ensurePresets()
		source := d.activePreset
		if b.CopyFrom != nil {
			if d.presets[*b.CopyFrom] == nil {
				return nil, apiErrf(http.StatusNotFound, "preset %d is not declared", *b.CopyFrom)
			}
			source = *b.CopyFrom
		}
		if d.presetNamed(name) {
			return nil, apiErrf(http.StatusConflict, "a preset named %q already exists", name)
		}
		var id int64
		if d.state != nil {
			var err error
			switch id, err = d.state.CreatePreset(context.Background(), name, source); {
			case errors.Is(err, errPresetNameTaken):
				return nil, apiErrf(http.StatusConflict, "a preset named %q already exists", name)
			case err != nil:
				return nil, &stateSaveError{err: err}
			}
		} else {
			id, d.nextPresetID = d.nextPresetID, d.nextPresetID+1
		}
		rec := &presetRecord{id: id, name: name, programs: map[int]storedZonePrograms{}}
		if src := d.presets[source]; src != nil {
			// Programs are only ever replaced as a whole, never edited in
			// place, so a shallow copy of the maps is enough.
			for zone, zp := range src.programs {
				rec.programs[zone] = zp
			}
		}
		d.presets[id] = rec
		d.presetOrder = append(d.presetOrder, id)
		return presetView{ID: id, Name: name, Active: false}, nil
	})
	if writeAPIErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// patchPreset renames a preset. The name stays unique without case.
func (d *daemon) patchPreset(w http.ResponseWriter, r *http.Request) {
	id, ok := pathPresetID(w, r)
	if !ok {
		return
	}
	var b postPresetBody
	if !decodeBody(w, r, &b) {
		return
	}
	if b.Name == nil || strings.TrimSpace(*b.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	name := strings.TrimSpace(*b.Name)
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		d.ensurePresets()
		p := d.presets[id]
		if p == nil {
			return nil, apiErrf(http.StatusNotFound, "preset %d is not declared", id)
		}
		for _, other := range d.presets {
			if other.id != id && strings.EqualFold(other.name, name) {
				return nil, apiErrf(http.StatusConflict, "a preset named %q already exists", name)
			}
		}
		if d.state != nil {
			err := d.state.RenamePreset(context.Background(), id, name)
			switch {
			case errors.Is(err, errPresetNameTaken):
				return nil, apiErrf(http.StatusConflict, "a preset named %q already exists", name)
			case errors.Is(err, sql.ErrNoRows):
				return nil, apiErrf(http.StatusNotFound, "preset %d is not declared", id)
			case err != nil:
				return nil, &stateSaveError{err: err}
			}
		}
		p.name = name
		return presetView{ID: id, Name: name, Active: p.id == d.activePreset}, nil
	})
	if writeAPIErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// deletePreset drops a preset. The active preset and the last one cannot be
// deleted.
func (d *daemon) deletePreset(w http.ResponseWriter, r *http.Request) {
	id, ok := pathPresetID(w, r)
	if !ok {
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		d.ensurePresets()
		if d.presets[id] == nil {
			return nil, apiErrf(http.StatusNotFound, "preset %d is not declared", id)
		}
		if id == d.activePreset {
			return nil, apiErrf(http.StatusConflict, "the active preset cannot be deleted")
		}
		if len(d.presets) == 1 {
			return nil, apiErrf(http.StatusConflict, "the last preset cannot be deleted")
		}
		if d.state != nil {
			switch err := d.state.DeletePreset(context.Background(), id); {
			case errors.Is(err, sql.ErrNoRows):
				return nil, apiErrf(http.StatusNotFound, "preset %d is not declared", id)
			case err != nil:
				return nil, &stateSaveError{err: err}
			}
		}
		delete(d.presets, id)
		d.presetOrder = slices.DeleteFunc(d.presetOrder, func(other int64) bool { return other == id })
		return map[string]any{"dropped": id}, nil
	})
	if writeAPIErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// activatePreset switches the active preset. The database takes the active id
// first, so a failed save leaves the old preset serving. Then the preset
// serves its home programs to the zone members and each zone whose program in
// force moved to another block gets one write. The write is a program block
// write: it carries no 6440, whether the block moved at home or in away.
func (d *daemon) activatePreset(w http.ResponseWriter, r *http.Request) {
	id, ok := pathPresetID(w, r)
	if !ok {
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		d.ensurePresets()
		p := d.presets[id]
		if p == nil {
			return nil, apiErrf(http.StatusNotFound, "preset %d is not declared", id)
		}
		if id == d.activePreset {
			return map[string]any{"id": p.id, "name": p.name, "active": true, "queued": 0}, nil
		}
		// Stored first: a failed save leaves the old preset active.
		if d.state != nil {
			if err := d.state.SetActivePreset(context.Background(), id); err != nil {
				return nil, &stateSaveError{err: err}
			}
		}
		d.activePreset = id
		changed := d.applyHomeState(e)
		if err := d.serveHomePrograms(e); err != nil {
			return nil, err
		}
		if d.away {
			changed = slices.DeleteFunc(changed, func(zone int) bool {
				_, has := d.awayProgramOf(zone)
				return d.isHotWaterZone(zone) && !has
			})
		}
		queued := d.rewriteZones(e, changed, func(target string) { d.dispatchQueued(e, target) }, false)
		// The owed writes reach the database with the pushed blocks: a restart
		// re-evaluates from the stored preset and stored blocks, not from the
		// old preset.
		d.saveProgramBlocks()
		return map[string]any{"id": p.id, "name": p.name, "active": true, "queued": queued}, nil
	})
	if writeAPIErr(w, d.persist(err)) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}
