package daemon

import (
	"net/http"
	"strconv"
)

type roomOrderBody struct {
	Rooms *[]int `json:"rooms"`
}

func (d *daemon) getRoomOrder(w http.ResponseWriter, r *http.Request) {
	if d.state == nil {
		writeJSON(w, http.StatusOK, map[string][]int{"rooms": {}})
		return
	}
	rooms, err := d.state.RoomOrder(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read room order: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string][]int{"rooms": rooms})
}

func (d *daemon) putRoomOrder(w http.ResponseWriter, r *http.Request) {
	var body roomOrderBody
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Rooms == nil {
		writeErr(w, http.StatusBadRequest, "rooms is required and must be an array of room numbers")
		return
	}
	seen := make(map[int]struct{}, len(*body.Rooms))
	for i, room := range *body.Rooms {
		if room <= 0 {
			writeErr(w, http.StatusBadRequest, "room number at index "+strconv.Itoa(i)+" must be positive")
			return
		}
		if _, ok := seen[room]; ok {
			writeErr(w, http.StatusBadRequest, "room numbers must not be duplicated")
			return
		}
		seen[room] = struct{}{}
	}
	if d.state == nil {
		writeErr(w, http.StatusServiceUnavailable, "state database is unavailable")
		return
	}
	if err := d.state.SaveRoomOrder(r.Context(), *body.Rooms); err != nil {
		writeErr(w, http.StatusInternalServerError, "save room order: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string][]int{"rooms": *body.Rooms})
}
