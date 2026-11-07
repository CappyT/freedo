package daemon

// zone_radiator_test: the radiator power of a heating zone on /api/install/zones
// and its storage. The installation is synthetic.

import (
	"context"
	"net/http"
	"testing"
)

func radiatorView(t *testing.T, res map[string]any) any {
	t.Helper()
	zone, ok := res["zone"].(map[string]any)
	if !ok {
		t.Fatalf("answer %v carries no zone", res)
	}
	value, present := zone["radiator_w"]
	if !present {
		t.Fatalf("zone view %v carries no radiator_w", zone)
	}
	return value
}

// TestRadiatorWPatch: the field reads back as written, null clears it, and a
// value outside 100..20000 W, a non-number or the hot water zone is refused with
// the rows untouched.
func TestRadiatorWPatch(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	const zone = "/api/install/zones/3"

	if v := radiatorView(t, a.get(zone)); v != nil {
		t.Errorf("default radiator_w = %v, want null", v)
	}
	if v := radiatorView(t, a.get("/api/install/zones/0")); v != nil {
		t.Errorf("hot water radiator_w = %v, want null", v)
	}
	for _, v := range []float64{1250.5, 100, 20000} {
		res := a.mutate(http.MethodPatch, zone, map[string]any{"radiator_w": v})
		if got := radiatorView(t, res); got != v {
			t.Errorf("PATCH %g answers %v", v, got)
		}
		if got := radiatorView(t, a.get(zone)); got != v {
			t.Errorf("GET after PATCH %g reads %v", v, got)
		}
	}
	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Options.RadiatorsW; len(got) != 1 || got[3] != 20000 {
		t.Errorf("stored radiator power = %v, want zone 3 at 20000", got)
	}

	if v := radiatorView(t, a.mutate(http.MethodPatch, zone, map[string]any{"radiator_w": nil})); v != nil {
		t.Errorf("PATCH null answers %v, want null", v)
	}
	stored, _, err = a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Options.RadiatorsW) != 0 {
		t.Errorf("stored radiator power = %v after the clear, want none", stored.Options.RadiatorsW)
	}

	for _, tc := range []struct {
		name string
		path string
		body any
		code int
	}{
		{"below the range", zone, map[string]any{"radiator_w": 99.9}, http.StatusBadRequest},
		{"above the range", zone, map[string]any{"radiator_w": 20000.5}, http.StatusBadRequest},
		{"zero", zone, map[string]any{"radiator_w": 0}, http.StatusBadRequest},
		{"negative", zone, map[string]any{"radiator_w": -500}, http.StatusBadRequest},
		{"a string", zone, map[string]any{"radiator_w": "big"}, http.StatusBadRequest},
		{"hot water zone", "/api/install/zones/0", map[string]any{"radiator_w": 1500}, http.StatusBadRequest},
		{"unknown zone", "/api/install/zones/99", map[string]any{"radiator_w": 1500}, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, res := a.call(http.MethodPatch, tc.path, tc.body); code != tc.code {
				t.Errorf("code=%d %v, want %d", code, res, tc.code)
			}
		})
	}
	if v := radiatorView(t, a.get(zone)); v != nil {
		t.Errorf("radiator_w = %v after the refused writes, want null", v)
	}
}
