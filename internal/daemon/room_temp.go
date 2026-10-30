package daemon

// room_temp: the room temperature an external sensor measures for a heating
// zone the coordinator owns, pushed over HTTP. While the reading is fresh the
// zone controller regulates on it (zone_owner.go), the zone reports it as its
// own temperature and the learners read it. A zone its device owns takes no
// reading: its valves regulate on their own sensor, with the manual correction
// in tag 0140. The last reading is stored, so a restart does not leave the zone
// without a measure until the sensor pushes again.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"freedo/tado"
)

const (
	// roomTempMinC and roomTempMaxC bound a pushed room temperature to what a
	// room sensor measures in a heated home.
	roomTempMinC = -10
	roomTempMaxC = 50
	// roomTempSourceMaxLen bounds the label of the sensor that sends a reading,
	// in bytes.
	roomTempSourceMaxLen = 255
)

var (
	errRoomTempZone = errors.New("only a heating zone takes an external room temperature")
	errRoomTempMode = errors.New("an external room temperature needs the coordinator as owner: " +
		"a zone its device owns regulates on the valve sensor, with the correction in tag 0140")
)

// roomTemp is the external room temperature of one zone. at is zero without a
// reading. source labels the sensor of the reading for display, empty when the
// push named none. It belongs to the engine goroutine.
type roomTemp struct {
	tempC  float64
	source string
	at     time.Time
}

// roomRefusal is the last push a zone refused because the coordinator does not
// own it. It stays in memory and shows while younger than
// options.room_temp_max_age_sec: the integration pushes every 15 minutes while
// the mapping lives.
type roomRefusal struct {
	at     time.Time
	source string
}

func (d *daemon) roomTempZone(number int) (*tado.Zone, error) {
	inst := d.inst.Load()
	if inst == nil {
		return nil, errNoInstall
	}
	z, err := inst.ZoneByNumber(number)
	if err != nil {
		return nil, err
	}
	if z.Kind != tado.ZoneHeating {
		return nil, fmt.Errorf("zone %d: %w", number, errRoomTempZone)
	}
	return z, nil
}

// roomTempFresh reports whether the reading of x still drives its zone.
func roomTempFresh(inst *tado.Install, x *roomTemp, now time.Time) bool {
	return !x.at.IsZero() && now.Sub(x.at) <= inst.Options.RoomTempMaxAge()
}

// freshRoomTemp returns the external room temperature of zone while it is fresh.
func (d *daemon) freshRoomTemp(inst *tado.Install, zone int) (float64, bool) {
	x := d.roomTemps[zone]
	if x == nil || !roomTempFresh(inst, x, d.retryNow()) {
		return 0, false
	}
	return x.tempC, true
}

// learnerReadsExternal reports whether the learners of zone read the external
// room temperature now: with the coordinator as owner, while the reading is
// fresh. The reading does not move when the correction is written, so a write
// does not disturb a running episode then.
func (d *daemon) learnerReadsExternal(inst *tado.Install, zone int) bool {
	if inst == nil {
		return false
	}
	if _, ok := d.freshRoomTemp(inst, zone); !ok {
		return false
	}
	return zoneTakesReading(inst, zone)
}

// roomTempC is the room temperature of zone: the external one while the
// learners read it, else the temperature c the zone reports.
func (d *daemon) roomTempC(inst *tado.Install, zone int, c float64) float64 {
	if d.learnerReadsExternal(inst, zone) {
		t, _ := d.freshRoomTemp(inst, zone)
		return t
	}
	return c
}

// zoneReport is tado.EmuState.ZoneReport with the room temperature of the zone.
func (d *daemon) zoneReport(e *tado.EmuState, zone int) (float64, int, time.Time, bool) {
	tempC, demand, updated, ok := e.ZoneReport(zone)
	return d.roomTempC(d.inst.Load(), zone, tempC), demand, updated, ok
}

// roomZoneViews returns the zone reports with the room temperature of each zone.
func (d *daemon) roomZoneViews(zones []tado.ZoneView) []tado.ZoneView {
	inst := d.inst.Load()
	out := make([]tado.ZoneView, len(zones))
	for i, z := range zones {
		out[i] = z
		out[i].TempC = d.roomTempC(inst, z.Number, z.TempC)
	}
	return out
}

func roundCenti(c float64) float64 { return math.Round(c*100) / 100 }

// postZoneTemperature stores the external room temperature of a zone,
// {"tempC": number, "source": string}. source is optional and only labels the
// sensor in the view.
func (d *daemon) postZoneTemperature(w http.ResponseWriter, r *http.Request) {
	zone, _, ok := d.pathZoneMembers(w, r)
	if !ok {
		return
	}
	var b struct {
		TempC  *float64 `json:"tempC"`
		Source string   `json:"source"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	if b.TempC == nil || !(*b.TempC >= roomTempMinC && *b.TempC <= roomTempMaxC) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("tempC is required, within %d..%d", roomTempMinC, roomTempMaxC))
		return
	}
	if msg := checkRoomTempSource(b.Source); msg != "" {
		writeErr(w, http.StatusBadRequest, "source: "+msg)
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		z, err := d.roomTempZone(zone)
		if err != nil {
			return nil, err
		}
		inst := d.inst.Load()
		now := d.retryNow()
		if !zoneTakesReading(inst, z.Number) {
			d.roomRefused[z.Number] = roomRefusal{at: now, source: b.Source}
			return nil, fmt.Errorf("zone %d: %w", z.Number, errRoomTempMode)
		}
		delete(d.roomRefused, z.Number)
		d.roomTemps[z.Number] = &roomTemp{tempC: roundCenti(*b.TempC), at: now, source: b.Source}
		d.saveRoomTemps()
		return d.roomTempView(e, z.Number), nil
	})
	if errors.Is(err, errRoomTempMode) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if writeHandlerErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// checkRoomTempSource returns a problem with the source label of a reading, or
// "" when it is valid. An empty label is valid: the push names no sensor.
func checkRoomTempSource(source string) string {
	switch {
	case len(source) > roomTempSourceMaxLen:
		return fmt.Sprintf("longer than %d bytes", roomTempSourceMaxLen)
	case !utf8.ValidString(source):
		return "not valid UTF-8"
	case strings.ContainsFunc(source, unicode.IsControl):
		return "holds a control character"
	}
	return ""
}

// deleteZoneTemperature drops the external room temperature of a zone. The
// controller falls back to the measuring device, as it does when the reading
// goes stale.
func (d *daemon) deleteZoneTemperature(w http.ResponseWriter, r *http.Request) {
	zone, _, ok := d.pathZoneMembers(w, r)
	if !ok {
		return
	}
	v, err := d.doReq(r, func(e *tado.EmuState) (any, error) {
		z, err := d.roomTempZone(zone)
		if err != nil {
			return nil, err
		}
		delete(d.roomRefused, z.Number)
		delete(d.roomTemps, z.Number)
		d.saveRoomTemps()
		return d.roomTempView(e, z.Number), nil
	})
	if writeHandlerErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// dropRoomTemps forgets the zones the installation no longer lets take an
// external room temperature: removed, no longer heating, or no longer owned by
// the coordinator.
func (d *daemon) dropRoomTemps(inst *tado.Install) {
	for zone := range d.roomTemps {
		z, err := inst.ZoneByNumber(zone)
		kept := err == nil && z.Kind == tado.ZoneHeating &&
			zoneTakesReading(inst, zone)
		if kept {
			continue
		}
		delete(d.roomTemps, zone)
	}
	for zone := range d.roomRefused {
		if z, err := inst.ZoneByNumber(zone); err != nil || z.Kind != tado.ZoneHeating ||
			zoneTakesReading(inst, zone) {
			delete(d.roomRefused, zone)
		}
	}
	d.saveRoomTemps()
}

// storedRoomTemp is one stored reading: the zone, the temperature, the time of
// the push in unix milliseconds and the label of the sensor.
type storedRoomTemp struct {
	Zone   int     `json:"zone"`
	TempC  float64 `json:"tempC"`
	AtMs   int64   `json:"at"`
	Source string  `json:"source"`
}

func (d *daemon) roomTempRows() []storedRoomTemp {
	rows := make([]storedRoomTemp, 0, len(d.roomTemps))
	for zone, x := range d.roomTemps {
		if !x.at.IsZero() {
			rows = append(rows, storedRoomTemp{Zone: zone, TempC: x.tempC, AtMs: x.at.UnixMilli(), Source: x.source})
		}
	}
	slices.SortFunc(rows, func(a, b storedRoomTemp) int { return a.Zone - b.Zone })
	return rows
}

// saveRoomTemps stores the readings once they changed. A failed save stays owed
// to the next one.
func (d *daemon) saveRoomTemps() {
	if d.state == nil {
		return
	}
	rows := d.roomTempRows()
	if d.roomTempsSaved != nil && slices.Equal(rows, d.roomTempsSaved) {
		return
	}
	if err := d.state.SetRoomTemps(context.Background(), rows); err != nil {
		log.Printf("persist the room temperatures: %v", err)
		return
	}
	d.roomTempsSaved = rows
}

// restoreRoomTemps loads the stored readings with their original time. A
// reading that is stale, or of a zone that cannot take one any more, is
// dropped.
func (d *daemon) restoreRoomTemps() error {
	inst := d.inst.Load()
	if d.state == nil || inst == nil {
		return nil
	}
	stored, err := d.state.RoomTemps(context.Background())
	if err != nil {
		return err
	}
	d.roomTempsSaved = stored
	now := d.retryNow()
	for _, s := range stored {
		z, err := inst.ZoneByNumber(s.Zone)
		if err != nil || z.Kind != tado.ZoneHeating || !zoneTakesReading(inst, s.Zone) {
			continue
		}
		x := &roomTemp{tempC: s.TempC, at: time.UnixMilli(s.AtMs), source: s.Source}
		if !roomTempFresh(inst, x, now) {
			continue
		}
		d.roomTemps[s.Zone] = x
	}
	d.saveRoomTemps()
	return nil
}

// roomTempView renders the external room temperature of one heating zone.
// tempC, at, ageSec and source describe the last reading and stale says it is
// too old to drive the zone; inUse says that it does: the controller, the early
// start, the plant episodes and the flow adaptation read tempC, and the zone
// reports it as its temperature. deviceC is the temperature the zone reports.
// refused is the last push the zone refused because the coordinator does not
// own it, {source, at, ageSec}, or nil once that push is older than the limit
// for a reading. A zone with no reading carries it too.
func (d *daemon) roomTempView(e *tado.EmuState, zone int) map[string]any {
	inst := d.inst.Load()
	view := map[string]any{
		"tempC": nil, "at": nil, "ageSec": nil, "source": nil, "stale": false, "inUse": false,
		"deviceC": nil, "refused": d.roomRefusedView(inst, zone),
	}
	if deviceC, _, _, ok := e.ZoneReport(zone); ok {
		view["deviceC"] = deviceC
	}
	x := d.roomTemps[zone]
	if x == nil {
		return view
	}
	if !x.at.IsZero() {
		age := d.retryNow().Sub(x.at)
		fresh := roomTempFresh(inst, x, d.retryNow())
		view["tempC"], view["at"] = x.tempC, x.at.UTC().Format(time.RFC3339)
		if x.source != "" {
			view["source"] = x.source
		}
		view["ageSec"], view["stale"], view["inUse"] = int(age/time.Second), !fresh, fresh
	}
	return view
}

// roomRefusedView renders the last refused push of zone, nil when there is none
// or it is older than the limit for a reading.
func (d *daemon) roomRefusedView(inst *tado.Install, zone int) any {
	r, ok := d.roomRefused[zone]
	if !ok {
		return nil
	}
	age := d.retryNow().Sub(r.at)
	if age > inst.Options.RoomTempMaxAge() {
		return nil
	}
	var source any
	if r.source != "" {
		source = r.source
	}
	return map[string]any{"source": source, "at": r.at.UTC().Format(time.RFC3339), "ageSec": int(age / time.Second)}
}

// zoneTakesReading reports whether zone takes an external room temperature:
// with the coordinator as owner.
func zoneTakesReading(inst *tado.Install, zone int) bool {
	return inst.Options.CoordinatorOwns(zone)
}
