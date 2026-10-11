package daemon

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"freedo/tado"
)

// removalFixture declares an installation where each device carries other side
// effects when it leaves:
//
//	RU..1  thermostat: the hot-water zone 2 alone, and the heat source of circuit 1
//	VA..1  valve: reporter and measuring device of zone 1
//	VA..2  valve: plain member of zone 1
//	VA..3  valve: alone in zone 3, so also its measuring device
//	VA..4  valve: in no zone
func removalFixture(t *testing.T) *installAPI {
	t.Helper()
	a := newInstallAPI(t)
	a.declareDevices()
	a.mutate(http.MethodPost, "/api/install/devices",
		map[string]any{"kind": "valve", "eui": apiValveLeftEUI, "serial": "VA9000000003", "label": "bagno"})
	a.mutate(http.MethodPost, "/api/install/devices",
		map[string]any{"kind": "valve", "eui": apiValveRightEUI, "serial": "VA9000000004", "label": "cantina"})
	a.mutate(http.MethodPost, "/api/install/zones", map[string]any{"number": 1, "kind": "heating"})
	a.mutate(http.MethodPut, "/api/install/zones/1/members",
		map[string]any{"members": []string{"VA9000000001", "VA9000000002"}})
	a.mutate(http.MethodPut, "/api/install/zones/1/reporter", map[string]any{"ref": "VA9000000001"})
	a.mutate(http.MethodPut, "/api/install/zones/1/measuring", map[string]any{"ref": "VA9000000001"})
	a.mutate(http.MethodPost, "/api/install/zones", map[string]any{"number": 2, "kind": "hotwater"})
	a.mutate(http.MethodPut, "/api/install/zones/2/members", map[string]any{"members": []string{"RU9000000001"}})
	a.mutate(http.MethodPost, "/api/install/zones", map[string]any{"number": 3, "kind": "heating"})
	a.mutate(http.MethodPut, "/api/install/zones/3/members", map[string]any{"members": []string{"VA9000000003"}})
	a.mutate(http.MethodPost, "/api/install/circuits",
		map[string]any{"id": 1, "name": "casa", "heat_source": "RU9000000001"})
	return a
}

// effectsOf renders the effects of an answer as "effect zone N" or "effect
// circuit N", in the order the answer lists them.
func effectsOf(t *testing.T, res map[string]any) []string {
	t.Helper()
	list, ok := res["effects"].([]any)
	if !ok {
		t.Fatalf("effects = %#v, want an array", res["effects"])
	}
	out := []string{}
	for _, item := range list {
		e := item.(map[string]any)
		switch {
		case e["zone"] != nil:
			out = append(out, fmt.Sprintf("%s zone %v", e["effect"], e["zone"]))
		case e["circuit"] != nil:
			out = append(out, fmt.Sprintf("%s circuit %v", e["effect"], e["circuit"]))
		default:
			t.Fatalf("effect %v names neither a zone nor a circuit", e)
		}
	}
	return out
}

func TestInstallAPIReportsTheSideEffectsOfARemoval(t *testing.T) {
	a := removalFixture(t)
	for _, tc := range []struct {
		name, ref string
		want      []string
		forceable bool
	}{
		{"reporter and measuring device", "VA9000000001", []string{"reporter zone 1", "measuring_device zone 1"}, true},
		{"plain member", "VA9000000002", []string{}, true},
		{"hot water, last member and heat source", "RU9000000001",
			[]string{"hot_water zone 2", "last_member zone 2", "heat_source circuit 1"}, false},
		{"last member is the measuring device", "VA9000000003", []string{"last_member zone 3", "measuring_device zone 3"}, true},
		{"in no zone", "VA9000000004", []string{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := a.get("/api/install/devices/" + tc.ref + "/removal")
			if got := effectsOf(t, res); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("effects = %v, want %v", got, tc.want)
			}
			if res["forceable"] != tc.forceable {
				t.Errorf("forceable = %v, want %v", res["forceable"], tc.forceable)
			}
			if dev := res["device"].(map[string]any); dev["serial"] != tc.ref {
				t.Errorf("device = %v, want %s", dev, tc.ref)
			}
		})
	}
	if code, res := a.call(http.MethodGet, "/api/install/devices/VA9000009999/removal", nil); code != http.StatusNotFound {
		t.Errorf("unknown device: got %d %v, want 404", code, res)
	}
}

func TestInstallAPIRefusesARemovalWithoutForce(t *testing.T) {
	a := removalFixture(t)
	for _, tc := range []struct {
		ref       string
		effects   []string
		referrers []string
	}{
		{"VA9000000001", []string{"reporter zone 1", "measuring_device zone 1"}, []string{"zone 1 members"}},
		{"VA9000000002", []string{}, []string{"zone 1 members"}},
		{"RU9000000001", []string{"hot_water zone 2", "last_member zone 2", "heat_source circuit 1"},
			[]string{"zone 2 members", "circuit 1 heat_source"}},
	} {
		for _, query := range []string{"", "?force=false", "?force="} {
			code, res := a.call(http.MethodDelete, "/api/install/devices/"+tc.ref+query, nil)
			if code != http.StatusConflict {
				t.Fatalf("%s%s: got %d %v, want 409", tc.ref, query, code, res)
			}
			if got := effectsOf(t, res); !reflect.DeepEqual(got, tc.effects) {
				t.Errorf("%s%s: effects = %v, want %v", tc.ref, query, got, tc.effects)
			}
			if got := stringsOf(t, res["referrers"]); !reflect.DeepEqual(got, tc.referrers) {
				t.Errorf("%s%s: referrers = %v, want %v", tc.ref, query, got, tc.referrers)
			}
			if got := revOf(t, res); got != a.lastRev {
				t.Errorf("%s%s: rev = %d in the conflict body, want %d", tc.ref, query, got, a.lastRev)
			}
		}
	}
	if code, res := a.call(http.MethodDelete, "/api/install/devices/VA9000000001?force=maybe", nil); code != http.StatusBadRequest {
		t.Errorf("a force value that is no boolean: got %d %v, want 400", code, res)
	}
	if got := len(stringsOfKey(t, a.get("/api/install/devices"), "devices", "eui")); got != 5 {
		t.Errorf("%d devices after the refusals, want all 5", got)
	}
}

func TestInstallAPIRemovesADeviceWithNoSideEffectWithoutForce(t *testing.T) {
	a := removalFixture(t)
	res := a.mutate(http.MethodDelete, "/api/install/devices/VA9000000004", nil)
	if gone := res["device"].(map[string]any); gone["serial"] != "VA9000000004" {
		t.Fatalf("device = %v, want the removed valve", gone)
	}
}

func TestInstallAPIForcedRemovalReleasesTheZoneMemberships(t *testing.T) {
	a := removalFixture(t)

	a.mutate(http.MethodDelete, "/api/install/devices/VA9000000001?force=true", nil)
	zone := a.get("/api/install/zones/1")["zone"].(map[string]any)
	if got := stringsOf(t, zone["members"]); !reflect.DeepEqual(got, []string{apiValveTwoEUI}) {
		t.Errorf("zone 1 members = %v, want the plain member alone", got)
	}
	if zone["reporter"] != nil || zone["measuring_device"] != nil {
		t.Errorf("zone 1 = %v, want both elections gone with the device", zone)
	}
	var ord int
	if err := a.store.db.QueryRowContext(context.Background(),
		`SELECT ord FROM zone_member WHERE zone_number = 1 AND device_eui = ?`, apiValveTwoEUI).Scan(&ord); err != nil || ord != 0 {
		t.Errorf("remaining member at ord %d (%v), want the gap closed", ord, err)
	}

	res := a.mutate(http.MethodDelete, "/api/install/devices/VA9000000003?force=1", nil)
	if warns := warningsOf(t, res); !containsSubstring(warns, "zone 3 has no member") {
		t.Errorf("warnings = %v, want the emptied zone reported", warns)
	}
	if got := stringsOf(t, a.get("/api/install/zones/3")["zone"].(map[string]any)["members"]); len(got) != 0 {
		t.Errorf("zone 3 members = %v, want none", got)
	}

	// A plain member needs force too, and leaves its zone untouched.
	a.mutate(http.MethodDelete, "/api/install/devices/VA9000000002?force=true", nil)
	if got := len(stringsOfKey(t, a.get("/api/install/devices"), "devices", "eui")); got != 2 {
		t.Errorf("%d devices, want the thermostat and the unplaced valve", got)
	}
}

func TestInstallAPIForceDoesNotReleaseAHeatSource(t *testing.T) {
	a := removalFixture(t)
	code, res := a.call(http.MethodDelete, "/api/install/devices/RU9000000001?force=true", nil)
	if code != http.StatusConflict {
		t.Fatalf("got %d %v, want 409", code, res)
	}
	if got := effectsOf(t, res); !containsSubstring(got, "heat_source circuit 1") {
		t.Errorf("effects = %v, want the heat source named", got)
	}
	if got := revOf(t, res); got != a.lastRev {
		t.Errorf("rev = %d in the conflict body, want %d", got, a.lastRev)
	}

	// Once the circuit is gone the thermostat leaves its hot-water zone empty.
	a.mutate(http.MethodDelete, "/api/install/circuits/1", nil)
	res = a.get("/api/install/devices/RU9000000001/removal")
	if got := effectsOf(t, res); !reflect.DeepEqual(got, []string{"hot_water zone 2", "last_member zone 2"}) || res["forceable"] != true {
		t.Fatalf("effects = %v forceable = %v, want the hot-water zone left empty and the removal forceable", got, res["forceable"])
	}
	res = a.mutate(http.MethodDelete, "/api/install/devices/RU9000000001?force=true", nil)
	if warns := warningsOf(t, res); !containsSubstring(warns, "zone 2 has no member") {
		t.Errorf("warnings = %v, want the emptied hot-water zone reported", warns)
	}
}

// TestInstallAPIRemovalSendsNoFrame: DELETE edits the installation only. A
// factory reset the device has not taken yet goes with its queue, and the
// daemon no longer resolves the device to send another.
func TestInstallAPIRemovalSendsNoFrame(t *testing.T) {
	a := removalFixture(t)
	if code, res := a.call(http.MethodPost, "/api/nodes/VA9000000004/factoryreset", map[string]any{"confirm": true}); code != http.StatusOK && code != http.StatusAccepted {
		t.Fatalf("factory reset: got %d %v", code, res)
	}
	if got := a.pending()[apiValveRightEUI]; got == 0 {
		t.Fatal("no downlink waits for the device before the removal, want the netcmd")
	}
	a.mutate(http.MethodDelete, "/api/install/devices/VA9000000004", nil)
	left, err := a.d.do(func(e *tado.EmuState) (any, error) { return e.Pending(apiValveRightEUI), nil })
	if err != nil || left != 0 {
		t.Errorf("%v downlinks still wait for the removed device (%v), want none", left, err)
	}
	if code, res := a.call(http.MethodPost, "/api/nodes/VA9000000004/factoryreset", map[string]any{"confirm": true}); code != http.StatusNotFound {
		t.Errorf("factory reset after the removal: got %d %v, want 404", code, res)
	}
}
