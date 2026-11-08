package daemon

// zone_shared_space_test: the shared space of a heating zone on
// /api/install/zones and its storage. The installation is synthetic.

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func sharedSpaceView(t *testing.T, res map[string]any) any {
	t.Helper()
	zone, ok := res["zone"].(map[string]any)
	if !ok {
		t.Fatalf("answer %v carries no zone", res)
	}
	value, present := zone["shared_space"]
	if !present {
		t.Fatalf("zone view %v carries no shared_space", zone)
	}
	return value
}

// TestSharedSpacePatch: the name reads back trimmed, null clears it, and a bad
// name, a non-string or the hot water zone is refused with the rows untouched.
func TestSharedSpacePatch(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	const zone = "/api/install/zones/3"

	if v := sharedSpaceView(t, a.get(zone)); v != nil {
		t.Errorf("default shared_space = %v, want null", v)
	}
	if v := sharedSpaceView(t, a.get("/api/install/zones/0")); v != nil {
		t.Errorf("hot water shared_space = %v, want null", v)
	}
	res := a.mutate(http.MethodPatch, zone, map[string]any{"shared_space": "  Open-space_1 "})
	if got := sharedSpaceView(t, res); got != "Open-space_1" {
		t.Errorf("PATCH answers %v", got)
	}
	if got := sharedSpaceView(t, a.get(zone)); got != "Open-space_1" {
		t.Errorf("GET after PATCH reads %v", got)
	}
	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Options.SharedSpaces; len(got) != 1 || got[3] != "Open-space_1" {
		t.Errorf("stored shared spaces = %v, want zone 3 at Open-space_1", got)
	}

	if v := sharedSpaceView(t, a.mutate(http.MethodPatch, zone, map[string]any{"shared_space": nil})); v != nil {
		t.Errorf("PATCH null answers %v, want null", v)
	}
	stored, _, err = a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Options.SharedSpaces) != 0 {
		t.Errorf("stored shared spaces = %v after the clear, want none", stored.Options.SharedSpaces)
	}

	for _, tc := range []struct {
		name string
		path string
		body any
		code int
	}{
		{"empty", zone, map[string]any{"shared_space": ""}, http.StatusBadRequest},
		{"blank", zone, map[string]any{"shared_space": "   "}, http.StatusBadRequest},
		{"too long", zone, map[string]any{"shared_space": strings.Repeat("a", 33)}, http.StatusBadRequest},
		{"bad character", zone, map[string]any{"shared_space": "a/b"}, http.StatusBadRequest},
		{"a number", zone, map[string]any{"shared_space": 5}, http.StatusBadRequest},
		{"hot water zone", "/api/install/zones/0", map[string]any{"shared_space": "Hall"}, http.StatusBadRequest},
		{"unknown zone", "/api/install/zones/99", map[string]any{"shared_space": "Hall"}, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, res := a.call(http.MethodPatch, tc.path, tc.body); code != tc.code {
				t.Errorf("code=%d %v, want %d", code, res, tc.code)
			}
		})
	}
	if v := sharedSpaceView(t, a.get(zone)); v != nil {
		t.Errorf("shared_space = %v after the refused writes, want null", v)
	}
}
