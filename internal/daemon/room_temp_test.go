package daemon

// room_temp_test: the external room temperature of a zone the coordinator
// owns. Zone 3 has one valve and runs 19.5 C all day; the clock, the reports
// and the readings are synthetic.

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"freedo/tado"
)

type roomRig struct {
	t     *testing.T
	a     *installAPI
	d     *daemon
	url   string
	in    *tado.Install
	clock *testClock
}

// loadStored reads the installation back from the store of a, validated.
func loadStored(t *testing.T, a *installAPI) *tado.Install {
	t.Helper()
	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stored.ValidateCommissionable(true); err != nil {
		t.Fatal(err)
	}
	return stored
}

// newRoomRig starts a daemon on a store with the coordinator as owner of zone
// 3, edit applied to the installation, the program on and the valve block
// known.
func newRoomRig(t *testing.T, edit func(*tado.Install)) *roomRig {
	t.Helper()
	a := newInstallAPIOn(t, windowInstall(t, func(in *tado.Install) {
		in.Options.ZoneOwners = map[int]string{3: tado.ZoneOwnerCoordinator}
		if edit != nil {
			edit(in)
		}
	}))
	stored := loadStored(t, a)
	clock := newTestClock(windowStart)
	d, url := newWindowDaemon(t, stored, clock, a.store)
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/3/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 19.5}), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	seedBlock(t, d, stored, earlyValve, 3)
	return &roomRig{t: t, a: a, d: d, url: url, in: stored, clock: clock}
}

func (r *roomRig) push(c float64) (map[string]any, int) {
	r.t.Helper()
	var out map[string]any
	code := postJSON(r.t, http.MethodPost, r.url+"/api/zones/3/temperature", map[string]any{"tempC": c}, &out)
	return out, code
}

type roomZones struct {
	Declared []struct {
		Number       int            `json:"number"`
		ExternalTemp map[string]any `json:"externalTemp"`
	} `json:"declared"`
	Demand []struct {
		Number int
		TempC  float64
	} `json:"demand"`
}

// view returns externalTemp of zone 3 and the temperature its demand view
// reports.
func (r *roomRig) view() (map[string]any, float64) {
	r.t.Helper()
	var zones roomZones
	if code := getJSON(r.t, r.url+"/api/zones", &zones); code != http.StatusOK {
		r.t.Fatalf("zones: code=%d", code)
	}
	var ext map[string]any
	for _, z := range zones.Declared {
		if z.Number == 3 {
			ext = z.ExternalTemp
		}
	}
	for _, z := range zones.Demand {
		if z.Number == 3 {
			return ext, z.TempC
		}
	}
	return ext, math.NaN()
}

func wantView(t *testing.T, what string, view map[string]any, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if view[k] != v {
			t.Errorf("%s: externalTemp.%s = %v, want %v (view %v)", what, k, view[k], v, view)
		}
	}
}

// TestRoomTempClear: DELETE drops the reading, and the zone reports the valve
// temperature at once.
func TestRoomTempClear(t *testing.T) {
	r := newRoomRig(t, nil)
	zoneAct(t, r.d, r.in, 22, 50)
	if _, code := r.push(20); code != http.StatusOK {
		t.Fatal("push failed")
	}
	if view, temp := r.view(); view["inUse"] != true || temp != 20 {
		t.Fatalf("pushed: view %v temperature %v, want the external 20 in use", view, temp)
	}
	var view map[string]any
	if code := postJSON(t, http.MethodDelete, r.url+"/api/zones/3/temperature", map[string]any{}, &view); code != http.StatusOK {
		t.Fatalf("delete: code=%d %v", code, view)
	}
	wantView(t, "cleared", view, map[string]any{"tempC": nil, "inUse": false, "deviceC": 22.0})
	if view, temp := r.view(); view["tempC"] != nil || temp != 22 {
		t.Errorf("after the delete: view %v temperature %v, want no reading and the valve's 22", view, temp)
	}
}

// TestRoomTempSourceLabel: the push names its sensor, the view shows the name
// of the last reading, a push without one clears it, a bad label is refused
// with the reading untouched, and a DELETE drops the label with the reading.
func TestRoomTempSourceLabel(t *testing.T) {
	r := newRoomRig(t, nil)
	post := func(body map[string]any) (map[string]any, int) {
		t.Helper()
		var out map[string]any
		return out, postJSON(t, http.MethodPost, r.url+"/api/zones/3/temperature", body, &out)
	}
	answer, code := post(map[string]any{"tempC": 20.4, "source": "Soggiorno thermometer"})
	if code != http.StatusOK {
		t.Fatalf("push: code=%d %v", code, answer)
	}
	wantView(t, "the answer", answer, map[string]any{"tempC": 20.4, "source": "Soggiorno thermometer"})
	view, _ := r.view()
	wantView(t, "GET /api/zones", view, map[string]any{"tempC": 20.4, "source": "Soggiorno thermometer"})

	for _, tc := range []struct {
		name   string
		source any
	}{
		{"too long", strings.Repeat("a", roomTempSourceMaxLen+1)},
		{"control character", "a\tb"},
		{"not a string", 3},
	} {
		if answer, code := post(map[string]any{"tempC": 21.0, "source": tc.source}); code != http.StatusBadRequest {
			t.Errorf("%s: code=%d %v, want 400", tc.name, code, answer)
		}
	}
	view, _ = r.view()
	wantView(t, "after the refused pushes", view, map[string]any{"tempC": 20.4, "source": "Soggiorno thermometer"})

	answer, _ = post(map[string]any{"tempC": 20.6})
	wantView(t, "a push without a source", answer, map[string]any{"tempC": 20.6, "source": nil})

	post(map[string]any{"tempC": 20.8, "source": "Aqara"})
	if code := postJSON(t, http.MethodDelete, r.url+"/api/zones/3/temperature", map[string]any{}, &answer); code != http.StatusOK {
		t.Fatalf("delete: code=%d %v", code, answer)
	}
	wantView(t, "cleared", answer, map[string]any{"tempC": nil, "source": nil})
}

// TestRoomTempRefusals: hot water, an unknown zone, a bad body and a value out
// of range are refused, and a refused push stores nothing.
func TestRoomTempRefusals(t *testing.T) {
	r := newRoomRig(t, nil)
	post := func(zone int, body any) int {
		var out map[string]any
		return postJSON(t, http.MethodPost, r.url+"/api/zones/"+strconv.Itoa(zone)+"/temperature", body, &out)
	}
	for _, tc := range []struct {
		name string
		zone int
		body any
		code int
	}{
		{"hot water", 0, map[string]any{"tempC": 20}, http.StatusBadRequest},
		{"unknown zone", 99, map[string]any{"tempC": 20}, http.StatusNotFound},
		{"zone its device owns", 5, map[string]any{"tempC": 20}, http.StatusConflict},
		{"no tempC", 3, map[string]any{}, http.StatusBadRequest},
		{"null tempC", 3, map[string]any{"tempC": nil}, http.StatusBadRequest},
		{"above range", 3, map[string]any{"tempC": 80}, http.StatusBadRequest},
		{"below range", 3, map[string]any{"tempC": -30}, http.StatusBadRequest},
		{"unknown field", 3, map[string]any{"tempC": 20, "open": true}, http.StatusBadRequest},
	} {
		if code := post(tc.zone, tc.body); code != tc.code {
			t.Errorf("%s: code=%d, want %d", tc.name, code, tc.code)
		}
	}
	var out map[string]any
	if code := postJSON(t, http.MethodDelete, r.url+"/api/zones/0/temperature", map[string]any{}, &out); code != http.StatusBadRequest {
		t.Errorf("delete on hot water: code=%d, want 400", code)
	}
	if view, _ := r.view(); view["tempC"] != nil {
		t.Errorf("a refused push left a reading: %v", view)
	}
	var zones roomZones
	if code := getJSON(t, r.url+"/api/zones", &zones); code != http.StatusOK {
		t.Fatal("zones failed")
	}
	for _, z := range zones.Declared {
		if (z.Number == 0) != (z.ExternalTemp == nil) {
			t.Errorf("zone %d externalTemp = %v: only heating zones carry one", z.Number, z.ExternalTemp)
		}
	}
}

// TestRoomTempViews: the reading is rounded to 0.01 C, its age follows the
// clock, and it goes stale after room_temp_max_age_sec, which the installation
// sets. With no report from the valve the zone still reports the reading. The
// view carries no field of a setpoint shift.
func TestRoomTempViews(t *testing.T) {
	r := newRoomRig(t, func(in *tado.Install) {
		maxAge := 900
		in.Options.RoomTempMaxAgeSec = &maxAge
	})
	view, code := r.push(20.374)
	if code != http.StatusOK {
		t.Fatalf("push: code=%d", code)
	}
	wantView(t, "pushed", view, map[string]any{
		"tempC": 20.37, "ageSec": 0.0, "inUse": true, "stale": false, "deviceC": nil,
		"at": windowStart.Format(time.RFC3339),
	})
	for _, key := range []string{"shiftC", "wantedC", "offsetC", "learnInUse"} {
		if _, present := view[key]; present {
			t.Errorf("externalTemp carries %s: %v", key, view)
		}
	}
	r.clock.advance(10 * time.Minute)
	if view, temp := r.view(); view["ageSec"] != 600.0 || view["inUse"] != true || temp != 20.37 {
		t.Errorf("after 10 minutes: view %v temperature %v", view, temp)
	}
	r.clock.advance(6 * time.Minute)
	if view, _ := r.view(); view["stale"] != true || view["inUse"] != false {
		t.Errorf("after 16 minutes with a limit of 15: %v, want stale", view)
	}
}

// TestRoomTempFeedsTheLearners: the early start, the flow adaptation and the
// plant episodes read the zone temperature through zoneReport and plantSample,
// which give the external temperature while it is fresh and the valve's after.
func TestRoomTempFeedsTheLearners(t *testing.T) {
	r := newRoomRig(t, nil)
	zoneAct(t, r.d, r.in, 22, 50)
	if _, code := r.push(20.4); code != http.StatusOK {
		t.Fatal("push failed")
	}
	read := func() (float64, float64) {
		v, err := r.d.do(func(e *tado.EmuState) (any, error) {
			c, _, _, _ := r.d.zoneReport(e, 3)
			s, _, ok := r.d.plantSample(e, r.d.inst.Load(), 3, r.d.retryNow())
			if !ok {
				return nil, context.Canceled
			}
			return [2]float64{c, s.TempC}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got := v.([2]float64)
		return got[0], got[1]
	}
	if c, s := read(); c != 20.4 || s != 20.4 {
		t.Errorf("fresh: zoneReport %v, plantSample %v, want 20.4", c, s)
	}
	r.clock.advance(31 * time.Minute)
	if c, s := read(); c != 22 || s != 22 {
		t.Errorf("stale: zoneReport %v, plantSample %v, want the valve's 22", c, s)
	}
}

// TestRoomTempMaxAgeOption: room_temp_max_age_sec defaults to 1800, a PATCH
// stores it, and a value that is not positive is refused.
func TestRoomTempMaxAgeOption(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	if got := a.get("/api/install")["room_temp_max_age_sec"]; got != 1800.0 {
		t.Errorf("default room_temp_max_age_sec = %v, want 1800", got)
	}
	a.mutate(http.MethodPatch, "/api/install", map[string]any{"room_temp_max_age_sec": 900})
	if got := a.get("/api/install")["room_temp_max_age_sec"]; got != 900.0 {
		t.Errorf("room_temp_max_age_sec after the patch = %v, want 900", got)
	}
	stored := loadStored(t, a)
	if got := stored.Options.RoomTempMaxAge(); got != 15*time.Minute {
		t.Errorf("stored limit = %v, want 15m", got)
	}
	if code, res := a.call(http.MethodPatch, "/api/install", map[string]any{"room_temp_max_age_sec": 0}); code != http.StatusBadRequest {
		t.Errorf("zero limit: code=%d %v, want 400", code, res)
	}
}

// refusedOf returns externalTemp.refused of zone 3, nil when it is null.
func (r *roomRig) refusedOf() map[string]any {
	r.t.Helper()
	view, _ := r.view()
	refused, present := view["refused"]
	if !present {
		r.t.Fatalf("externalTemp has no refused key: %v", view)
	}
	if refused == nil {
		return nil
	}
	m, ok := refused.(map[string]any)
	if !ok {
		r.t.Fatalf("refused = %v, want an object or null", refused)
	}
	return m
}

func (r *roomRig) setOwner(owner string) {
	r.t.Helper()
	var res map[string]any
	if code := postJSON(r.t, http.MethodPatch, r.url+"/api/install/zones/3", map[string]any{"owner": owner}, &res); code != http.StatusOK {
		r.t.Fatalf("patch owner %s: code=%d %v", owner, code, res)
	}
}

func (r *roomRig) pushFrom(c float64, source string) int {
	r.t.Helper()
	var out map[string]any
	return postJSON(r.t, http.MethodPost, r.url+"/api/zones/3/temperature", map[string]any{"tempC": c, "source": source}, &out)
}

// TestRoomTempRefusedPush: a push on a zone its device owns gets 409 and shows
// as externalTemp.refused with the source and the age, even though the zone
// has no reading. The coordinator as owner, an accepted push, a DELETE and the
// age limit each clear it.
func TestRoomTempRefusedPush(t *testing.T) {
	t.Run("set by a refused push", func(t *testing.T) {
		r := newRoomRig(t, nil)
		if r.refusedOf() != nil {
			t.Fatal("refused is set before any push")
		}
		r.setOwner(tado.ZoneOwnerDevice)
		if code := r.pushFrom(20, "Soggiorno thermometer"); code != http.StatusConflict {
			t.Fatalf("push: code=%d, want 409", code)
		}
		refused := r.refusedOf()
		if refused["source"] != "Soggiorno thermometer" || refused["at"] != windowStart.Format(time.RFC3339) || refused["ageSec"] != 0.0 {
			t.Errorf("refused = %v, want the source, the push time and age 0", refused)
		}
		view, _ := r.view()
		wantView(t, "the zone has no reading", view, map[string]any{"tempC": nil, "inUse": false})
		r.clock.advance(4 * time.Minute)
		if refused := r.refusedOf(); refused["ageSec"] != 240.0 {
			t.Errorf("refused after 4 minutes = %v, want age 240", refused)
		}
	})
	t.Run("no source is null", func(t *testing.T) {
		r := newRoomRig(t, nil)
		r.setOwner(tado.ZoneOwnerDevice)
		if _, code := r.push(20); code != http.StatusConflict {
			t.Fatalf("push: code=%d, want 409", code)
		}
		if refused := r.refusedOf(); refused == nil || refused["source"] != nil {
			t.Errorf("refused = %v, want an object with a null source", refused)
		}
	})
	t.Run("a bad push is not remembered", func(t *testing.T) {
		r := newRoomRig(t, nil)
		r.setOwner(tado.ZoneOwnerDevice)
		if code := r.pushFrom(80, "Soggiorno thermometer"); code != http.StatusBadRequest {
			t.Fatalf("push: code=%d, want 400", code)
		}
		if refused := r.refusedOf(); refused != nil {
			t.Errorf("refused = %v after a 400, want null", refused)
		}
	})
	t.Run("cleared by an accepted push", func(t *testing.T) {
		r := newRoomRig(t, nil)
		r.setOwner(tado.ZoneOwnerDevice)
		if code := r.pushFrom(20, "Soggiorno thermometer"); code != http.StatusConflict {
			t.Fatalf("push: code=%d, want 409", code)
		}
		r.setOwner(tado.ZoneOwnerCoordinator)
		if refused := r.refusedOf(); refused != nil {
			t.Errorf("refused = %v after the coordinator took the zone, want null", refused)
		}
		if code := r.pushFrom(20, "Soggiorno thermometer"); code != http.StatusOK {
			t.Fatalf("push after the owner change: code=%d, want 200", code)
		}
		if refused := r.refusedOf(); refused != nil {
			t.Errorf("refused = %v after an accepted push, want null", refused)
		}
	})
	t.Run("cleared by delete", func(t *testing.T) {
		r := newRoomRig(t, nil)
		r.setOwner(tado.ZoneOwnerDevice)
		if code := r.pushFrom(20, "Soggiorno thermometer"); code != http.StatusConflict {
			t.Fatalf("push: code=%d, want 409", code)
		}
		var view map[string]any
		if code := postJSON(t, http.MethodDelete, r.url+"/api/zones/3/temperature", map[string]any{}, &view); code != http.StatusOK {
			t.Fatalf("delete: code=%d %v", code, view)
		}
		if view["refused"] != nil {
			t.Errorf("the delete answer holds refused = %v, want null", view["refused"])
		}
		if refused := r.refusedOf(); refused != nil {
			t.Errorf("refused = %v after a delete, want null", refused)
		}
	})
	t.Run("old refusal is not shown", func(t *testing.T) {
		r := newRoomRig(t, func(in *tado.Install) {
			maxAge := 900
			in.Options.RoomTempMaxAgeSec = &maxAge
		})
		r.setOwner(tado.ZoneOwnerDevice)
		if code := r.pushFrom(20, "Soggiorno thermometer"); code != http.StatusConflict {
			t.Fatalf("push: code=%d, want 409", code)
		}
		r.clock.advance(14 * time.Minute)
		if r.refusedOf() == nil {
			t.Error("refused is gone after 14 minutes with a limit of 15")
		}
		r.clock.advance(2 * time.Minute)
		if refused := r.refusedOf(); refused != nil {
			t.Errorf("refused = %v after 16 minutes with a limit of 15, want null", refused)
		}
	})
	t.Run("hot water has none", func(t *testing.T) {
		r := newRoomRig(t, nil)
		var out map[string]any
		postJSON(t, http.MethodPost, r.url+"/api/zones/0/temperature", map[string]any{"tempC": 20, "source": "x"}, &out)
		var zones roomZones
		getJSON(t, r.url+"/api/zones", &zones)
		for _, z := range zones.Declared {
			if z.Number == 0 && z.ExternalTemp != nil {
				t.Errorf("hot water carries externalTemp %v", z.ExternalTemp)
			}
		}
	})
}
