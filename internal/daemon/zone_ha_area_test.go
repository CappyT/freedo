package daemon

// zone_ha_area_test: the Home Assistant area of a zone on /api/install/zones,
// its storage and the zone views. The installation and the areas are synthetic.

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"freedo/tado"
)

func haAreaOf(t *testing.T, res map[string]any) any {
	t.Helper()
	zone, ok := res["zone"].(map[string]any)
	if !ok {
		t.Fatalf("answer %v carries no zone", res)
	}
	value, present := zone["ha_area"]
	if !present {
		t.Fatalf("zone view %v carries no ha_area", zone)
	}
	return value
}

// declaredHAArea reads haArea of zone in GET /api/zones declared[].
func declaredHAArea(t *testing.T, a *installAPI, zone float64) any {
	t.Helper()
	code, res := a.call(http.MethodGet, "/api/zones", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/zones: code=%d %v", code, res)
	}
	for _, z := range res["declared"].([]any) {
		view := z.(map[string]any)
		if view["number"] == zone {
			value, present := view["haArea"]
			if !present {
				t.Fatalf("declared zone %v carries no haArea", view)
			}
			return value
		}
	}
	t.Fatalf("GET /api/zones declares no zone %v", zone)
	return nil
}

// TestHAAreaPatch: the area reads back as written in both zone views, null
// clears it, the hot water zone takes one too, a bad area is refused with the
// rows untouched, and the store keeps it.
func TestHAAreaPatch(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	const zone = "/api/install/zones/3"
	living := map[string]any{"id": "living_room", "name": "Soggiorno"}

	if v := haAreaOf(t, a.get(zone)); v != nil {
		t.Errorf("default ha_area = %v, want null", v)
	}
	if v := declaredHAArea(t, a, 3); v != nil {
		t.Errorf("default declared haArea = %v, want null", v)
	}
	if v := haAreaOf(t, a.mutate(http.MethodPatch, zone, map[string]any{"ha_area": living})); !reflect.DeepEqual(v, living) {
		t.Errorf("PATCH answers ha_area %v, want %v", v, living)
	}
	if v := haAreaOf(t, a.get(zone)); !reflect.DeepEqual(v, living) {
		t.Errorf("GET reads ha_area %v, want %v", v, living)
	}
	if v := declaredHAArea(t, a, 3); !reflect.DeepEqual(v, living) {
		t.Errorf("declared haArea = %v, want %v", v, living)
	}
	boiler := map[string]any{"id": "utility", "name": "Utility room"}
	a.mutate(http.MethodPatch, "/api/install/zones/0", map[string]any{"ha_area": boiler})
	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]tado.HAArea{3: {ID: "living_room", Name: "Soggiorno"}, 0: {ID: "utility", Name: "Utility room"}}
	if !reflect.DeepEqual(stored.Options.HAAreas, want) {
		t.Errorf("stored areas = %v, want %v", stored.Options.HAAreas, want)
	}

	for _, tc := range []struct {
		name string
		path string
		body any
		code int
	}{
		{"empty id", zone, map[string]any{"ha_area": map[string]any{"id": "", "name": "x"}}, http.StatusBadRequest},
		{"no name", zone, map[string]any{"ha_area": map[string]any{"id": "x"}}, http.StatusBadRequest},
		{"unknown field", zone, map[string]any{"ha_area": map[string]any{"id": "x", "name": "x", "floor": "1"}}, http.StatusBadRequest},
		{"wrong case", zone, map[string]any{"ha_area": map[string]any{"ID": "x", "name": "x"}}, http.StatusBadRequest},
		{"too long", zone, map[string]any{"ha_area": map[string]any{"id": "x", "name": strings.Repeat("a", tado.HAAreaMaxLen+1)}}, http.StatusBadRequest},
		{"control character", zone, map[string]any{"ha_area": map[string]any{"id": "x", "name": "a\nb"}}, http.StatusBadRequest},
		{"a string", zone, map[string]any{"ha_area": "living_room"}, http.StatusBadRequest},
		{"unknown zone", "/api/install/zones/99", map[string]any{"ha_area": living}, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, res := a.call(http.MethodPatch, tc.path, tc.body); code != tc.code {
				t.Errorf("code=%d %v, want %d", code, res, tc.code)
			}
		})
	}
	if v := haAreaOf(t, a.get(zone)); !reflect.DeepEqual(v, living) {
		t.Errorf("ha_area = %v after the refused writes, want %v", v, living)
	}

	if v := haAreaOf(t, a.mutate(http.MethodPatch, zone, map[string]any{"ha_area": nil})); v != nil {
		t.Errorf("PATCH null answers %v, want null", v)
	}
	stored, _, err = a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stored.Options.HAAreas[3]; ok {
		t.Errorf("stored areas = %v after the clear, want none for zone 3", stored.Options.HAAreas)
	}
}

// TestHAAreaGoesWithTheZone: a zone declared again under the number of a
// deleted one starts with no area.
func TestHAAreaGoesWithTheZone(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	create := map[string]any{"number": 7, "name": "Office", "kind": "heating"}
	a.mutate(http.MethodPost, "/api/install/zones", create)
	a.mutate(http.MethodPatch, "/api/install/zones/7", map[string]any{"ha_area": map[string]any{"id": "office", "name": "Office"}})
	a.mutate(http.MethodDelete, "/api/install/zones/7", nil)
	a.mutate(http.MethodPost, "/api/install/zones", create)
	if v := haAreaOf(t, a.get("/api/install/zones/7")); v != nil {
		t.Errorf("ha_area of the new zone 7 = %v, want null", v)
	}
}
