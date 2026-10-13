package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"freedo/internal/install"
	"freedo/internal/radio"
	"freedo/tado"
)

// The synthetic devices the management tests declare. The EUIs end in the tado
// suffix, so the gate raises no coordinator warning about them.
const (
	apiThermostatEUI = "03046a3107c51b00"
	apiValveEUI      = "01026a3107c51b00"
	apiValveTwoEUI   = "11226a3107c51b00"
	apiValveLeftEUI  = "21326a3107c51b00"
	apiValveRightEUI = "31426a3107c51b00"
)

// installAPI drives the real mux over httptest and keeps every response body,
// so one test can assert on what the whole suite ever answered.
type installAPI struct {
	t       *testing.T
	d       *daemon
	store   *stateStore
	base    string
	path    string
	lastRev uint64
	mu      sync.Mutex
	bodies  []string
}

// newInstallAPI starts a daemon on a fresh database that holds nothing but the
// generated identity: the state a first boot leaves behind.
func newInstallAPI(t *testing.T) *installAPI {
	t.Helper()
	return newInstallAPIOn(t, nil)
}

// newInstallAPIOn starts the same daemon over a database that already holds
// seed, the way a seeded or migrated one does. ReplaceInstall writes the rows
// as they are, so a seed can carry what the API itself refuses.
func newInstallAPIOn(t *testing.T, seed *tado.Install) *installAPI {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	store := newTestStore(t, path)
	ctx := context.Background()
	if _, err := store.EnsureIdentity(ctx); err != nil {
		t.Fatalf("EnsureIdentity: %v", err)
	}
	if seed != nil {
		if _, err := store.ReplaceInstall(ctx, seed); err != nil {
			t.Fatalf("ReplaceInstall: %v", err)
		}
	}
	in, rev, err := store.LoadInstall(ctx)
	if err != nil {
		t.Fatalf("LoadInstall: %v", err)
	}
	if err := in.ValidateCommissionable(true); err != nil {
		t.Fatalf("the generated identity does not validate: %v", err)
	}
	emu := tado.NewEmu(in.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(in, emu, radio.NewMock(nil), true, "mock")
	d.state = store
	if err := d.restorePresets(ctx, emu); err != nil {
		t.Fatalf("restorePresets: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	go d.run(runCtx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	return &installAPI{t: t, d: d, store: store, base: srv.URL, path: path, lastRev: rev}
}

// call runs one request and decodes the answer. body nil sends none.
func (a *installAPI) call(method, path string, body any) (int, map[string]any) {
	a.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	return a.send(method, path, reader)
}

// raw sends a body the caller spelled itself, for the malformed cases.
func (a *installAPI) raw(method, path, body string) (int, map[string]any) {
	a.t.Helper()
	return a.send(method, path, strings.NewReader(body))
}

func (a *installAPI) send(method, path string, body io.Reader) (int, map[string]any) {
	a.t.Helper()
	req, err := http.NewRequest(method, a.base+path, body)
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	a.mu.Lock()
	a.bodies = append(a.bodies, string(raw))
	a.mu.Unlock()
	out := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		var body any
		if err := json.Unmarshal(raw, &body); err != nil {
			a.t.Fatalf("%s %s: body %q: %v", method, path, raw, err)
		}
		// The runtime routes answer with arrays; only the management ones are
		// objects, and only those are read field by field here.
		if fields, ok := body.(map[string]any); ok {
			out = fields
		}
	}
	return resp.StatusCode, out
}

// mutate runs a mutation that must succeed and must move the revision by one.
func (a *installAPI) mutate(method, path string, body any) map[string]any {
	a.t.Helper()
	code, res := a.call(method, path, body)
	if code != http.StatusOK {
		a.t.Fatalf("%s %s: got %d %v, want 200", method, path, code, res)
	}
	rev := revOf(a.t, res)
	if rev != a.lastRev+1 {
		a.t.Fatalf("%s %s: rev = %d, want %d", method, path, rev, a.lastRev+1)
	}
	if _, ok := res["warnings"].([]any); !ok {
		a.t.Fatalf("%s %s: warnings = %#v, want an array", method, path, res["warnings"])
	}
	a.lastRev = rev
	return res
}

// get runs a read that must succeed and must report the revision unchanged.
func (a *installAPI) get(path string) map[string]any {
	a.t.Helper()
	code, res := a.call(http.MethodGet, path, nil)
	if code != http.StatusOK {
		a.t.Fatalf("GET %s: got %d %v, want 200", path, code, res)
	}
	if rev := revOf(a.t, res); rev != a.lastRev {
		a.t.Fatalf("GET %s: rev = %d, want %d", path, rev, a.lastRev)
	}
	return res
}

func revOf(t *testing.T, res map[string]any) uint64 {
	t.Helper()
	v, ok := res["rev"].(float64)
	if !ok {
		t.Fatalf("no rev in %v", res)
	}
	return uint64(v)
}

func warningsOf(t *testing.T, res map[string]any) []string {
	t.Helper()
	list, _ := res["warnings"].([]any)
	out := make([]string, 0, len(list))
	for _, w := range list {
		out = append(out, fmt.Sprint(w))
	}
	return out
}

func stringsOf(t *testing.T, v any) []string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("%#v is not an array", v)
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

// declareDevices creates the three devices every management test starts from.
func (a *installAPI) declareDevices() {
	a.t.Helper()
	for _, dev := range []map[string]any{
		{"kind": "thermostat", "eui": apiThermostatEUI, "serial": "RU9000000001", "label": "entry"},
		{"kind": "valve", "eui": apiValveEUI, "serial": "VA9000000001", "label": "lounge"},
		{"kind": "valve", "eui": apiValveTwoEUI, "serial": "VA9000000002", "label": "study"},
	} {
		a.mutate(http.MethodPost, "/api/install/devices", dev)
	}
}

// seedLabel writes a label straight into the rows, which is the only way to
// reach the shape the API refuses. It bumps no revision.
func (a *installAPI) seedLabel(eui, label string) {
	a.t.Helper()
	if _, err := a.store.db.ExecContext(context.Background(),
		`UPDATE device SET label = ? WHERE eui = ?`, label, eui); err != nil {
		a.t.Fatalf("seed label %q on %s: %v", label, eui, err)
	}
}

// TestInstallAPIBuildsInstallationFromEmpty walks every route once, from the
// generated identity to a zone with members, both elections and a circuit.
func TestInstallAPIBuildsInstallationFromEmpty(t *testing.T) {
	a := newInstallAPI(t)

	first := a.get("/api/install")
	if counts := first["counts"].(map[string]any); counts["devices"].(float64) != 0 ||
		counts["zones"].(float64) != 0 || counts["circuits"].(float64) != 0 {
		t.Fatalf("a fresh database reports counts %v, want zeroes", counts)
	}
	if key := first["network_key"].(map[string]any); key["present"] != true {
		t.Fatalf("network_key = %v, want the generated key reported as present", key)
	}
	if first["home_id"].(float64) == 0 {
		t.Fatal("home_id = 0, which no resource path accepts")
	}

	a.declareDevices()
	if devices := stringsOfKey(t, a.get("/api/install/devices"), "devices", "eui"); len(devices) != 3 {
		t.Fatalf("devices = %v, want three", devices)
	}
	dev := a.get("/api/install/devices/VA9000000001")["device"].(map[string]any)
	if dev["eui"] != apiValveEUI || dev["kind"] != "valve" || dev["label"] != "lounge" {
		t.Fatalf("device = %v, want the valve declared above", dev)
	}
	patched := a.mutate(http.MethodPatch, "/api/install/devices/VA9000000002", map[string]any{
		"label": "attic", "notes": "under the roof", "kind": "valve", "serial": "VA9000009002",
	})["device"].(map[string]any)
	if patched["label"] != "attic" || patched["notes"] != "under the roof" ||
		patched["serial"] != "VA9000009002" {
		t.Fatalf("patched device = %v", patched)
	}
	// The renamed device answers to its new serial, and to its EUI throughout.
	a.mutate(http.MethodPatch, "/api/install/devices/VA9000009002", map[string]any{"serial": "VA9000000002"})

	// A zone with no member yet is a warning, not an error: the commissioning
	// flow declares the zone before it fills it.
	zone := a.mutate(http.MethodPost, "/api/install/zones",
		map[string]any{"number": 1, "name": "lounge", "kind": "heating"})
	if warns := warningsOf(t, zone); !containsSubstring(warns, "zone 1 has no member") {
		t.Fatalf("warnings = %v, want the empty zone reported", warns)
	}

	members := a.mutate(http.MethodPut, "/api/install/zones/1/members",
		map[string]any{"members": []string{"RU9000000001", "VA9000000001"}})
	if got := stringsOf(t, members["members"]); !reflect.DeepEqual(got, []string{apiThermostatEUI, apiValveEUI}) {
		t.Fatalf("members = %v, want the thermostat first", got)
	}
	appended := a.mutate(http.MethodPost, "/api/install/zones/1/members", map[string]any{"ref": "VA9000000002"})
	if got := stringsOf(t, appended["members"]); !reflect.DeepEqual(got,
		[]string{apiThermostatEUI, apiValveEUI, apiValveTwoEUI}) {
		t.Fatalf("members = %v, want the appended valve last", got)
	}

	reporter := a.mutate(http.MethodPut, "/api/install/zones/1/reporter", map[string]any{"ref": "VA9000000001"})
	if reporter["reporter"] != apiValveEUI {
		t.Fatalf("reporter = %v, want %s", reporter["reporter"], apiValveEUI)
	}
	measuring := a.mutate(http.MethodPut, "/api/install/zones/1/measuring", map[string]any{"ref": "VA9000000001"})
	if measuring["measuring_device"] != apiValveEUI {
		t.Fatalf("measuring_device = %v, want %s", measuring["measuring_device"], apiValveEUI)
	}

	circuit := a.mutate(http.MethodPost, "/api/install/circuits",
		map[string]any{"id": 1, "name": "house", "heat_source": "RU9000000001"})["circuit"].(map[string]any)
	if circuit["heat_source"] != apiThermostatEUI {
		t.Fatalf("circuit = %v, want the thermostat as heat source", circuit)
	}
	a.mutate(http.MethodPatch, "/api/install/zones/1", map[string]any{"circuit": 1})
	a.mutate(http.MethodPatch, "/api/install/circuits/1",
		map[string]any{"name": "ground floor", "heat_source": apiThermostatEUI})
	if got := a.get("/api/install/circuits/1")["circuit"].(map[string]any); got["name"] != "ground floor" ||
		got["heat_source"] != apiThermostatEUI {
		t.Fatalf("circuit = %v, want the patched name and the same heat source", got)
	}

	a.mutate(http.MethodPatch, "/api/install", map[string]any{
		"utc_offset_min": 60,
		"coordinator":    map[string]any{"serial": "IB9000000001", "notes": "freedo"},
	})
	full := a.get("/api/install")
	if full["utc_offset_min"].(float64) != 60 {
		t.Fatalf("utc_offset_min = %v, want 60", full["utc_offset_min"])
	}
	if coord := full["coordinator"].(map[string]any); coord["serial"] != "IB9000000001" || coord["notes"] != "freedo" {
		t.Fatalf("coordinator = %v", coord)
	}
	a.mutate(http.MethodPatch, "/api/install", map[string]any{"utc_offset_min": nil})
	if got := a.get("/api/install")["utc_offset_min"]; got != nil {
		t.Fatalf("utc_offset_min = %v after the clear, want null", got)
	}
	a.mutate(http.MethodPatch, "/api/install", map[string]any{"timezone": "Europe/Rome", "utc_push_spacing_sec": 300})
	if got := a.get("/api/install"); got["timezone"] != "Europe/Rome" || got["utc_push_spacing_sec"].(float64) != 300 {
		t.Fatalf("timezone = %v, utc_push_spacing_sec = %v, want Europe/Rome and 300", got["timezone"], got["utc_push_spacing_sec"])
	}
	if code, body := a.call(http.MethodPatch, "/api/install", map[string]any{"timezone": "Europe/Nowhere"}); code != http.StatusBadRequest {
		t.Fatalf("unknown timezone: got %d %v, want 400", code, body)
	}
	a.mutate(http.MethodPatch, "/api/install", map[string]any{"timezone": nil})
	if got := a.get("/api/install")["timezone"]; got != "" {
		t.Fatalf("timezone = %v after the clear, want the host's zone", got)
	}

	// The zone the API built has to pass the strict gate the daemon boots on.
	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatalf("LoadInstall: %v", err)
	}
	if err := stored.Validate(); err != nil {
		t.Fatalf("the installation the API built does not validate: %v", err)
	}

	// Take it apart again through the same routes.
	a.mutate(http.MethodDelete, "/api/install/zones/1/members/VA9000000002", nil)
	a.mutate(http.MethodDelete, "/api/install/zones/1/measuring", nil)
	a.mutate(http.MethodDelete, "/api/install/zones/1/reporter", nil)
	if got := a.get("/api/install/zones/1")["zone"].(map[string]any); got["reporter"] != nil ||
		got["measuring_device"] != nil {
		t.Fatalf("zone = %v, want both elections cleared", got)
	}
	a.mutate(http.MethodPatch, "/api/install/zones/1", map[string]any{"circuit": nil})
	a.mutate(http.MethodDelete, "/api/install/circuits/1", nil)
	a.mutate(http.MethodDelete, "/api/install/zones/1", nil)
	a.mutate(http.MethodDelete, "/api/install/devices/VA9000000002", nil)
	final := a.get("/api/install")
	counts := final["counts"].(map[string]any)
	if counts["devices"].(float64) != 2 || counts["zones"].(float64) != 0 || counts["circuits"].(float64) != 0 {
		t.Fatalf("counts = %v, want two devices and nothing else", counts)
	}
}

// stringsOfKey pulls one field out of every object of a list response.
func stringsOfKey(t *testing.T, res map[string]any, list, field string) []string {
	t.Helper()
	items, ok := res[list].([]any)
	if !ok {
		t.Fatalf("%s is %#v, want an array", list, res[list])
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, fmt.Sprint(item.(map[string]any)[field]))
	}
	return out
}

func containsSubstring(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// TestInstallAPIRejectsBadInput: every way a request can be wrong on its face
// answers 400, and the ambiguous room label names its candidates.
func TestInstallAPIRejectsBadInput(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	a.mutate(http.MethodPost, "/api/install/zones", map[string]any{"number": 1, "kind": "heating"})
	a.mutate(http.MethodPost, "/api/install/circuits",
		map[string]any{"id": 1, "heat_source": "RU9000000001"})

	for _, tc := range []struct {
		name, method, path string
		body               any
		want               string
	}{
		{"bad device kind", http.MethodPost, "/api/install/devices",
			map[string]any{"kind": "boiler", "eui": apiValveLeftEUI}, "unknown device kind"},
		{"malformed eui", http.MethodPost, "/api/install/devices",
			map[string]any{"kind": "valve", "eui": "not-an-eui"}, "eui"},
		{"short eui", http.MethodPost, "/api/install/devices",
			map[string]any{"kind": "valve", "eui": "0102"}, "8 bytes"},
		{"bad zone kind", http.MethodPost, "/api/install/zones",
			map[string]any{"number": 2, "kind": "cooling"}, "unknown zone kind"},
		{"zone without a number", http.MethodPost, "/api/install/zones",
			map[string]any{"kind": "heating"}, "number is required"},
		{"unknown member", http.MethodPut, "/api/install/zones/1/members",
			map[string]any{"members": []string{"VA0000000000"}}, "unknown device"},
		{"circuit without a heat source", http.MethodPost, "/api/install/circuits",
			map[string]any{"id": 2}, "heat_source is required"},
		{"unknown heat source", http.MethodPost, "/api/install/circuits",
			map[string]any{"id": 2, "heat_source": "RU0000000000"}, "unknown device"},
		{"a valve as heat source", http.MethodPatch, "/api/install/circuits/1",
			map[string]any{"heat_source": "VA9000000001"}, "only a wall thermostat"},
		{"bad rev spelling", http.MethodDelete, "/api/install/devices/VA9000000002?rev=soon", nil, "bad rev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, res := a.call(tc.method, tc.path, tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("got %d %v, want 400", code, res)
			}
			if msg := fmt.Sprint(res["error"]); !strings.Contains(msg, tc.want) {
				t.Fatalf("error = %q, want it to mention %q", msg, tc.want)
			}
		})
	}

	t.Run("malformed body", func(t *testing.T) {
		code, res := a.raw(http.MethodPost, "/api/install/devices", `{"kind": "valve",`)
		if code != http.StatusBadRequest {
			t.Fatalf("got %d %v, want 400", code, res)
		}
		if msg := fmt.Sprint(res["error"]); !strings.Contains(msg, "bad body") {
			t.Fatalf("error = %q", msg)
		}
	})

	t.Run("ambiguous ref", func(t *testing.T) {
		// Two valves in one room: the label resolves to neither on its own. The
		// API refuses to write the second one, so the duplicate goes into the
		// rows directly, which is what an older installation carries.
		a.mutate(http.MethodPost, "/api/install/devices",
			map[string]any{"kind": "valve", "eui": apiValveLeftEUI, "serial": "VA9000000003", "label": "kitchen"})
		a.mutate(http.MethodPost, "/api/install/devices",
			map[string]any{"kind": "valve", "eui": apiValveRightEUI, "serial": "VA9000000004", "label": "pantry"})
		a.seedLabel(apiValveRightEUI, "kitchen")
		a.mutate(http.MethodPost, "/api/install/zones", map[string]any{"number": 3, "kind": "heating"})
		code, res := a.call(http.MethodPost, "/api/install/zones/3/members", map[string]any{"ref": "kitchen"})
		if code != http.StatusBadRequest {
			t.Fatalf("got %d %v, want 400", code, res)
		}
		if got := stringsOf(t, res["candidates"]); len(got) != 2 {
			t.Fatalf("candidates = %v, want both kitchen valves", got)
		}
	})

	// A refused request writes nothing, so the revision has not moved.
	if got := revOf(t, a.get("/api/install")); got != a.lastRev {
		t.Fatalf("rev = %d, want %d: a refused request moved it", got, a.lastRev)
	}
}

// TestInstallAPIConflicts: a stale revision and a reference another row still
// holds both answer 409, and the referrers are named.
func TestInstallAPIConflicts(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	a.mutate(http.MethodPost, "/api/install/zones", map[string]any{"number": 1, "kind": "heating"})
	a.mutate(http.MethodPut, "/api/install/zones/1/members",
		map[string]any{"members": []string{"RU9000000001", "VA9000000001"}})
	a.mutate(http.MethodPut, "/api/install/zones/1/reporter", map[string]any{"ref": "VA9000000001"})
	a.mutate(http.MethodPost, "/api/install/circuits",
		map[string]any{"id": 1, "name": "house", "heat_source": "RU9000000001"})
	a.mutate(http.MethodPatch, "/api/install/zones/1", map[string]any{"circuit": 1})

	t.Run("stale rev", func(t *testing.T) {
		code, res := a.call(http.MethodPatch, "/api/install",
			map[string]any{"rev": 1, "coordinator": map[string]any{"notes": "late"}})
		if code != http.StatusConflict {
			t.Fatalf("got %d %v, want 409", code, res)
		}
		if got := revOf(t, res); got != a.lastRev {
			t.Fatalf("rev = %d in the conflict body, want the current %d", got, a.lastRev)
		}
	})

	t.Run("fresh rev passes", func(t *testing.T) {
		a.mutate(http.MethodPatch, "/api/install",
			map[string]any{"rev": a.lastRev, "coordinator": map[string]any{"notes": "on time"}})
	})

	for _, tc := range []struct {
		name, path string
		want       []string
	}{
		{"member device", "/api/install/devices/VA9000000001", []string{"zone 1 members"}},
		{"heat source device", "/api/install/devices/RU9000000001",
			[]string{"zone 1 members", "circuit 1 heat_source"}},
		{"bound circuit", "/api/install/circuits/1", []string{"zone 1 circuit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, res := a.call(http.MethodDelete, tc.path, nil)
			if code != http.StatusConflict {
				t.Fatalf("got %d %v, want 409", code, res)
			}
			if got := stringsOf(t, res["referrers"]); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("referrers = %v, want %v", got, tc.want)
			}
			if got := revOf(t, res); got != a.lastRev {
				t.Fatalf("rev = %d in the conflict body, want the current %d", got, a.lastRev)
			}
		})
	}

	t.Run("zone claimed by a device", func(t *testing.T) {
		a.mutate(http.MethodPatch, "/api/install/devices/VA9000000001", map[string]any{"zone": 1})
		code, res := a.call(http.MethodDelete, "/api/install/zones/1", nil)
		if code != http.StatusConflict {
			t.Fatalf("got %d %v, want 409", code, res)
		}
		if got := stringsOf(t, res["referrers"]); !containsSubstring(got, apiValveEUI) {
			t.Fatalf("referrers = %v, want the claiming device", got)
		}
		a.mutate(http.MethodPatch, "/api/install/devices/VA9000000001", map[string]any{"zone": nil})
	})

	t.Run("identity is frozen once a device exists", func(t *testing.T) {
		for _, body := range []map[string]any{
			{"home_id": 4242},
			{"coordinator": map[string]any{"eui": apiValveLeftEUI}},
		} {
			code, res := a.call(http.MethodPatch, "/api/install", body)
			if code != http.StatusConflict {
				t.Fatalf("%v: got %d %v, want 409", body, code, res)
			}
		}
	})

	t.Run("duplicate keys", func(t *testing.T) {
		for _, tc := range []struct {
			name, path string
			body       any
		}{
			{"device", "/api/install/devices",
				map[string]any{"kind": "valve", "eui": apiValveEUI, "serial": "VA9000009999"}},
			{"serial", "/api/install/devices",
				map[string]any{"kind": "valve", "eui": apiValveLeftEUI, "serial": "VA9000000001"}},
			{"zone", "/api/install/zones", map[string]any{"number": 1, "kind": "heating"}},
			{"circuit", "/api/install/circuits", map[string]any{"id": 1, "heat_source": "RU9000000001"}},
			{"member", "/api/install/zones/1/members", map[string]any{"ref": "VA9000000001"}},
		} {
			code, res := a.call(http.MethodPost, tc.path, tc.body)
			if code != http.StatusConflict {
				t.Fatalf("%s: got %d %v, want 409", tc.name, code, res)
			}
		}
	})

	// Nothing above committed, so the daemon still serves the same revision.
	if got := revOf(t, a.get("/api/install")); got != a.lastRev {
		t.Fatalf("rev = %d, want %d", got, a.lastRev)
	}
}

// TestInstallAPIRefusesADuplicateLabel: the label is the name the dissector and
// every log line print, so one installation holds it once. Both the create and
// the rename answer 409 and name the device that already carries it.
func TestInstallAPIRefusesADuplicateLabel(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()

	code, res := a.call(http.MethodPost, "/api/install/devices",
		map[string]any{"kind": "valve", "eui": apiValveLeftEUI, "serial": "VA9000000003", "label": "lounge"})
	if code != http.StatusConflict {
		t.Fatalf("create with a duplicate label: got %d %v, want 409", code, res)
	}
	if msg := fmt.Sprint(res["error"]); !strings.Contains(msg, "VA9000000001") {
		t.Fatalf("error = %q, want it to name the device that holds the label", msg)
	}

	// One name in two spellings is still one name: the comparison ignores case.
	code, res = a.call(http.MethodPatch, "/api/install/devices/VA9000000002",
		map[string]any{"label": "Lounge"})
	if code != http.StatusConflict {
		t.Fatalf("rename onto a duplicate: got %d %v, want 409", code, res)
	}
	if msg := fmt.Sprint(res["error"]); !strings.Contains(msg, "VA9000000001") {
		t.Fatalf("error = %q, want it to name the device that holds the label", msg)
	}

	// A device keeps its own label: a rename onto it is not a duplicate.
	a.mutate(http.MethodPatch, "/api/install/devices/VA9000000001", map[string]any{"label": "lounge"})
	// The rule is about names, so two devices without one collide with nothing.
	a.mutate(http.MethodPost, "/api/install/devices",
		map[string]any{"kind": "valve", "eui": apiValveLeftEUI, "serial": "VA9000000003"})
	a.mutate(http.MethodPost, "/api/install/devices",
		map[string]any{"kind": "valve", "eui": apiValveRightEUI, "serial": "VA9000000004"})

	if got := stringsOfKey(t, a.get("/api/install/devices"), "devices", "label"); !reflect.DeepEqual(got,
		[]string{"entry", "lounge", "study", "", ""}) {
		t.Fatalf("labels = %v, want the declared ones: the refusals wrote nothing", got)
	}
}

// TestInstallAPIRenamesADeviceThroughTheLabelRoute: the ingress route a Home
// Assistant user reaches carries the label alone. The route changes the label
// under the rule of the general patch, and refuses every other field.
func TestInstallAPIRenamesADeviceThroughTheLabelRoute(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()

	dev := a.mutate(http.MethodPut, "/api/install/devices/VA9000000002/label",
		map[string]any{"label": "attic"})["device"].(map[string]any)
	if dev["label"] != "attic" || dev["kind"] != "valve" || dev["serial"] != "VA9000000002" {
		t.Fatalf("device = %v, want only the label changed", dev)
	}

	// A body that carries another field is refused, so the route cannot reach one.
	if code, res := a.call(http.MethodPut, "/api/install/devices/VA9000000001/label",
		map[string]any{"label": "hall", "kind": "thermostat"}); code != http.StatusBadRequest {
		t.Fatalf("a label route with kind: got %d %v, want 400", code, res)
	}
	// The duplicate rule is the same as the general patch.
	code, res := a.call(http.MethodPut, "/api/install/devices/VA9000000001/label",
		map[string]any{"label": "attic"})
	if code != http.StatusConflict {
		t.Fatalf("rename onto a duplicate label: got %d %v, want 409", code, res)
	}
	if msg := fmt.Sprint(res["error"]); !strings.Contains(msg, "VA9000000002") {
		t.Fatalf("error = %q, want it to name the device that holds the label", msg)
	}
}

// TestInstallAPIWritesTheZoneClaimThroughTheZoneRoute: the ingress route carries
// the zone claim alone, under the rule of the general patch.
func TestInstallAPIWritesTheZoneClaimThroughTheZoneRoute(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	a.mutate(http.MethodPost, "/api/install/zones", map[string]any{"number": 1, "kind": "heating"})
	a.mutate(http.MethodPost, "/api/install/zones/1/members", map[string]any{"ref": "VA9000000001"})

	dev := a.mutate(http.MethodPut, "/api/install/devices/VA9000000001/zone",
		map[string]any{"zone": 1})["device"].(map[string]any)
	if dev["zone"] != float64(1) || dev["label"] != "lounge" {
		t.Fatalf("device = %v, want only the zone claim changed", dev)
	}
	dev = a.mutate(http.MethodPut, "/api/install/devices/VA9000000001/zone",
		map[string]any{"zone": nil})["device"].(map[string]any)
	if dev["zone"] != nil {
		t.Fatalf("device = %v, want the claim removed", dev)
	}
	if code, res := a.call(http.MethodPut, "/api/install/devices/VA9000000001/zone",
		map[string]any{"zone": 1, "kind": "thermostat"}); code != http.StatusBadRequest {
		t.Fatalf("a zone route with kind: got %d %v, want 400", code, res)
	}
	if code, res := a.call(http.MethodPut, "/api/install/devices/VA9000000001/zone",
		map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("a zone route with no zone: got %d %v, want 400", code, res)
	}
	// The claim of a zone the device is not a member of is refused, as in the patch.
	if code, res := a.call(http.MethodPut, "/api/install/devices/VA9000000002/zone",
		map[string]any{"zone": 1}); code == http.StatusOK {
		t.Fatalf("a claim on a zone the device is not in: got %d %v, want a refusal", code, res)
	}
}

// TestInstallAPIWritesTheTimezoneThroughItsRoute: the ingress route carries the
// time zone alone. PATCH /api/install also writes the identity of the network,
// and this route must not reach it.
func TestInstallAPIWritesTheTimezoneThroughItsRoute(t *testing.T) {
	a := newInstallAPI(t)
	before := a.get("/api/install")

	view := a.mutate(http.MethodPut, "/api/install/timezone",
		map[string]any{"timezone": "Europe/Rome"})["install"].(map[string]any)
	if view["timezone"] != "Europe/Rome" || view["home_id"] != before["home_id"] {
		t.Fatalf("install = %v, want only the time zone changed", view)
	}
	for name, body := range map[string]map[string]any{
		"home_id":     {"timezone": "Europe/Rome", "home_id": 7},
		"coordinator": {"timezone": "Europe/Rome", "coordinator": map[string]any{"eui": "0102030405060708"}},
		"an option":   {"timezone": "Europe/Rome", "flow_limit_c": 60},
		"no timezone": {},
	} {
		if code, res := a.call(http.MethodPut, "/api/install/timezone", body); code != http.StatusBadRequest {
			t.Errorf("a timezone route with %s: got %d %v, want 400", name, code, res)
		}
	}
	if got := a.get("/api/install"); got["home_id"] != before["home_id"] ||
		!reflect.DeepEqual(got["coordinator"], before["coordinator"]) {
		t.Errorf("the identity moved: %v and %v, want %v and %v",
			got["home_id"], got["coordinator"], before["home_id"], before["coordinator"])
	}
	view = a.mutate(http.MethodPut, "/api/install/timezone",
		map[string]any{"timezone": nil})["install"].(map[string]any)
	if view["timezone"] != "" {
		t.Errorf("timezone = %v after null, want the host zone", view["timezone"])
	}
}

// TestInstallAPIServesADuplicateLabelFromTheRows: an installation seeded before
// the label rule keeps its duplicates. The daemon serves it and reports the
// finding, because refusing to start would leave nothing to rename it with.
func TestInstallAPIServesADuplicateLabelFromTheRows(t *testing.T) {
	seed, err := tado.Parse([]byte(duplicateLabelYAML))
	if err != nil {
		t.Fatalf("parse the seed: %v", err)
	}
	a := newInstallAPIOn(t, seed)

	warns := warningsOf(t, a.get("/api/install"))
	if !containsSubstring(warns, `"lounge" duplicates devices[0]`) {
		t.Fatalf("warnings = %v, want the duplicate label reported", warns)
	}
	if got := stringsOfKey(t, a.get("/api/install/devices"), "devices", "label"); !reflect.DeepEqual(got,
		[]string{"lounge", "lounge"}) {
		t.Fatalf("labels = %v, want both devices served", got)
	}

	// The way out is a rename onto a free label.
	a.mutate(http.MethodPatch, "/api/install/devices/VA9000000001", map[string]any{"label": "study"})
	if warns := warningsOf(t, a.get("/api/install")); containsSubstring(warns, "duplicates devices[") {
		t.Fatalf("warnings = %v, want the duplicate gone after the rename", warns)
	}
}

// duplicateLabelYAML is the shape the management API refuses and an older
// database still holds: one room label on two devices.
const duplicateLabelYAML = `
version: 1
home_id: 7801
network_key: "0102030405060708090a0b0c0d0e0f10"
coordinator: {eui: "aa016a3107c51b00", serial: IB9000000001}
devices:
  - {kind: thermostat, eui: "03046a3107c51b00", serial: RU9000000001, label: lounge}
  - {kind: valve, eui: "01026a3107c51b00", serial: VA9000000001, label: lounge}
zones:
  - {number: 1, name: lounge, kind: heating, circuit: 1, members: [RU9000000001, VA9000000001], reporter: VA9000000001}
circuits:
  - {id: 1, name: house, heat_source: RU9000000001}
`

// TestInstallAPIMissingObjects: a path that names nothing answers 404.
func TestInstallAPIMissingObjects(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	for _, tc := range [][2]string{
		{http.MethodGet, "/api/install/zones/7"},
		{http.MethodPatch, "/api/install/zones/7"},
		{http.MethodDelete, "/api/install/zones/7"},
		{http.MethodGet, "/api/install/circuits/7"},
		{http.MethodPatch, "/api/install/circuits/7"},
		{http.MethodDelete, "/api/install/circuits/7"},
		{http.MethodGet, "/api/install/devices/VA0000000000"},
		{http.MethodPut, "/api/install/zones/7/members"},
	} {
		code, res := a.call(tc[0], tc[1], map[string]any{})
		if code != http.StatusNotFound {
			t.Errorf("%s %s: got %d %v, want 404", tc[0], tc[1], code, res)
		}
	}
}

// TestInstallAPIReloadMovesTheElectionRecord: changing the reporter of a zone
// reaches the engine, and the served d/config document of both members carries
// the new 015e role byte. Replaying the same member list changes no bytes and
// queues no new work.
func TestInstallAPIReloadMovesTheElectionRecord(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	a.reportThermostatModule(71)
	a.mutate(http.MethodPost, "/api/install/zones", map[string]any{"number": 1, "kind": "heating"})
	a.mutate(http.MethodPut, "/api/install/zones/1/members",
		map[string]any{"members": []string{"RU9000000001", "VA9000000001"}})
	// The declared zone gives every member a logical id, so the reload takes
	// the config-write path instead of only refreshing the served document.
	for _, ref := range []string{"RU9000000001", "VA9000000001"} {
		a.mutate(http.MethodPatch, "/api/install/devices/"+ref, map[string]any{"zone": 1})
	}
	a.mutate(http.MethodPut, "/api/install/zones/1/reporter", map[string]any{"ref": "RU9000000001"})

	if role, ok := a.zoneRole(apiThermostatEUI, 1); !ok || role != 0x0d {
		t.Fatalf("thermostat 015e role = %#02x (present %v), want the elected 0x0d", role, ok)
	}
	if role, ok := a.zoneRole(apiValveEUI, 1); !ok || role != 0x05 {
		t.Fatalf("valve 015e role = %#02x (present %v), want the plain 0x05", role, ok)
	}

	a.mutate(http.MethodPut, "/api/install/zones/1/reporter", map[string]any{"ref": "VA9000000001"})
	if role, ok := a.zoneRole(apiValveEUI, 1); !ok || role != 0x0d {
		t.Fatalf("valve 015e role = %#02x (present %v), want the elected 0x0d after the re-election", role, ok)
	}
	if role, ok := a.zoneRole(apiThermostatEUI, 1); !ok || role != 0x05 {
		t.Fatalf("thermostat 015e role = %#02x (present %v), want the plain 0x05", role, ok)
	}

	before := a.pending()
	if before[apiValveEUI] == 0 {
		t.Fatalf("the re-election queued no config write: %v", before)
	}
	// The wall thermostat refuses a d/config write, so its role moves in the
	// served document, and it gets only the re-read trigger.
	if waiting, other := a.rereads(apiThermostatEUI); waiting != 1 || other != 0 {
		t.Fatalf("the wall thermostat queue = %d waiting re-read triggers and %d other downlinks, want the one trigger",
			waiting, other)
	}
	a.mutate(http.MethodPut, "/api/install/zones/1/members",
		map[string]any{"members": []string{"RU9000000001", "VA9000000001"}})
	if after := a.pending(); !reflect.DeepEqual(after, before) {
		t.Fatalf("an unchanged member list queued work: %v, was %v", after, before)
	}
}

// rereads counts the re-read triggers queued to one device and not on the air
// yet, and the downlinks queued to it that are no re-read trigger.
func (a *installAPI) rereads(euiHex string) (waiting, other int) {
	a.t.Helper()
	v, err := a.d.do(func(e *tado.EmuState) (any, error) { return e.Queue(euiHex), nil })
	if err != nil {
		a.t.Fatalf("Queue(%s): %v", euiHex, err)
	}
	for _, q := range v.([]tado.QueueEntry) {
		switch {
		case q.Kind != "reread":
			other++
		case !q.InFlight:
			waiting++
		}
	}
	return waiting, other
}

// zoneRole reads the 015e role byte the engine serves one device for one zone.
func (a *installAPI) zoneRole(euiHex string, zone int) (byte, bool) {
	a.t.Helper()
	v, err := a.d.do(func(e *tado.EmuState) (any, error) { return e.NodeRoles(euiHex) })
	if err != nil {
		a.t.Fatalf("NodeRoles(%s): %v", euiHex, err)
	}
	for _, rec := range v.([]tado.NodeTableRec) {
		if int(rec.Node) == zone {
			return rec.Role, true
		}
	}
	return 0, false
}

// pending counts the downlinks waiting for every declared device.
func (a *installAPI) pending() map[string]int {
	a.t.Helper()
	v, err := a.d.do(func(e *tado.EmuState) (any, error) {
		out := map[string]int{}
		in := a.d.inst.Load()
		for i := range in.Devices {
			key := hex.EncodeToString(in.Devices[i].WireEUI())
			out[key] = e.Pending(key)
		}
		return out, nil
	})
	if err != nil {
		a.t.Fatalf("pending: %v", err)
	}
	return v.(map[string]int)
}

// TestInstallAPINeverRevealsTheNetworkKey drives the whole surface and greps
// every answer for the key bytes.
func TestInstallAPINeverRevealsTheNetworkKey(t *testing.T) {
	a := newInstallAPI(t)
	var key []byte
	err := a.store.db.QueryRow(`SELECT key FROM net_key WHERE id = 1`).Scan(&key)
	if err != nil || len(key) != install.NetworkKeyLen {
		t.Fatalf("network key: %v (%d bytes)", err, len(key))
	}
	a.declareDevices()
	a.mutate(http.MethodPost, "/api/install/zones", map[string]any{"number": 1, "kind": "heating"})
	a.mutate(http.MethodPut, "/api/install/zones/1/members",
		map[string]any{"members": []string{"RU9000000001", "VA9000000001"}})
	a.mutate(http.MethodPost, "/api/install/zones/1/members", map[string]any{"ref": "VA9000000002"})
	a.mutate(http.MethodPut, "/api/install/zones/1/reporter", map[string]any{"ref": "VA9000000001"})
	a.mutate(http.MethodPut, "/api/install/zones/1/measuring", map[string]any{"ref": "VA9000000001"})
	a.mutate(http.MethodPost, "/api/install/circuits",
		map[string]any{"id": 1, "heat_source": "RU9000000001"})
	a.mutate(http.MethodPatch, "/api/install/zones/1", map[string]any{"circuit": 1})
	a.mutate(http.MethodPatch, "/api/install", map[string]any{"utc_offset_min": 60})
	a.mutate(http.MethodPatch, "/api/install/devices/VA9000000002", map[string]any{"label": "attic"})
	a.mutate(http.MethodPatch, "/api/install/circuits/1", map[string]any{"name": "house"})
	a.mutate(http.MethodDelete, "/api/install/zones/1/members/VA9000000002", nil)
	a.mutate(http.MethodDelete, "/api/install/zones/1/measuring", nil)
	a.mutate(http.MethodDelete, "/api/install/zones/1/reporter", nil)
	a.mutate(http.MethodDelete, "/api/install/devices/VA9000000002", nil)
	for _, path := range []string{
		"/api/install", "/api/install/devices", "/api/install/devices/VA9000000001",
		"/api/install/zones", "/api/install/zones/1", "/api/install/circuits", "/api/install/circuits/1",
		"/api/nodes", "/api/zones",
	} {
		if code, res := a.call(http.MethodGet, path, nil); code != http.StatusOK {
			t.Fatalf("GET %s: got %d %v", path, code, res)
		}
	}

	needles := []string{
		hex.EncodeToString(key),
		strings.ToUpper(hex.EncodeToString(key)),
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.bodies) < 20 {
		t.Fatalf("the suite recorded only %d answers", len(a.bodies))
	}
	for i, body := range a.bodies {
		for _, needle := range needles {
			if strings.Contains(body, needle) {
				t.Fatalf("answer %d leaks the network key: %s", i, body)
			}
		}
		if strings.Contains(body, "network_key\":\"") {
			t.Fatalf("answer %d carries a network_key string: %s", i, body)
		}
	}
}

// TestInstallAPISurvivesRestart: what a fresh load reads out of the database is
// what the API describes.
func TestInstallAPISurvivesRestart(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	a.mutate(http.MethodPost, "/api/install/zones", map[string]any{"number": 1, "name": "lounge", "kind": "heating"})
	a.mutate(http.MethodPut, "/api/install/zones/1/members",
		map[string]any{"members": []string{"RU9000000001", "VA9000000001", "VA9000000002"}})
	a.mutate(http.MethodPut, "/api/install/zones/1/reporter", map[string]any{"ref": "VA9000000001"})
	a.mutate(http.MethodPut, "/api/install/zones/1/measuring", map[string]any{"ref": "VA9000000002"})
	a.mutate(http.MethodPost, "/api/install/circuits",
		map[string]any{"id": 1, "name": "house", "heat_source": "RU9000000001"})
	a.mutate(http.MethodPatch, "/api/install/zones/1", map[string]any{"circuit": 1})
	a.mutate(http.MethodPatch, "/api/install", map[string]any{"utc_offset_min": -60})

	stored, rev, err := newTestStore(t, a.path).LoadInstall(context.Background())
	if err != nil {
		t.Fatalf("LoadInstall: %v", err)
	}
	if rev != a.lastRev {
		t.Fatalf("stored rev = %d, want %d", rev, a.lastRev)
	}
	if err := stored.Validate(); err != nil {
		t.Fatalf("the stored installation does not validate: %v", err)
	}

	view := a.get("/api/install")
	if uint32(view["home_id"].(float64)) != stored.HomeID {
		t.Errorf("home_id = %v, want %d", view["home_id"], stored.HomeID)
	}
	if view["utc_offset_min"].(float64) != float64(*stored.Options.UTCOffsetMin) {
		t.Errorf("utc_offset_min = %v, want %d", view["utc_offset_min"], *stored.Options.UTCOffsetMin)
	}
	if got := stringsOfKey(t, a.get("/api/install/devices"), "devices", "eui"); !reflect.DeepEqual(got,
		storedDeviceEUIs(stored)) {
		t.Errorf("devices = %v, want %v", got, storedDeviceEUIs(stored))
	}
	zone := a.get("/api/install/zones/1")["zone"].(map[string]any)
	storedZone, err := stored.ZoneByNumber(1)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stringsOf(t, zone["members"]), storedRefs(t, stored, storedZone.Members); !reflect.DeepEqual(got, want) {
		t.Errorf("members = %v, want %v", got, want)
	}
	reporter, err := stored.DemandReporter(1)
	if err != nil {
		t.Fatal(err)
	}
	if zone["reporter"] != hex.EncodeToString(reporter.WireEUI()) {
		t.Errorf("reporter = %v, want %x", zone["reporter"], reporter.WireEUI())
	}
	measuring, err := stored.MeasuringDevice(1)
	if err != nil {
		t.Fatal(err)
	}
	if zone["measuring_device"] != hex.EncodeToString(measuring.WireEUI()) {
		t.Errorf("measuring_device = %v, want %x", zone["measuring_device"], measuring.WireEUI())
	}
	source, err := stored.HeatSource(1)
	if err != nil {
		t.Fatal(err)
	}
	circuit := a.get("/api/install/circuits/1")["circuit"].(map[string]any)
	if circuit["heat_source"] != hex.EncodeToString(source.WireEUI()) {
		t.Errorf("heat_source = %v, want %x", circuit["heat_source"], source.WireEUI())
	}
}

func storedDeviceEUIs(in *tado.Install) []string {
	out := make([]string, 0, len(in.Devices))
	for i := range in.Devices {
		out = append(out, hex.EncodeToString(in.Devices[i].WireEUI()))
	}
	return out
}

func storedRefs(t *testing.T, in *tado.Install, refs []string) []string {
	t.Helper()
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		dev, err := in.Resolve(ref)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, hex.EncodeToString(dev.WireEUI()))
	}
	return out
}

// feedRadio keeps its frame channel open, so the engine goes on handling
// traffic while the management routes reload the installation under it.
type feedRadio struct {
	frames chan []byte
	mu     sync.Mutex
	sent   [][]byte
}

func (r *feedRadio) Frames() <-chan []byte { return r.frames }

func (r *feedRadio) Send(frame []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, append([]byte(nil), frame...))
	return nil
}

func (r *feedRadio) Close() error { return nil }

// TestInstallAPIMutatesWhileFramesArrive: the engine hop of a mutation lands
// between two frames, and nothing races.
func TestInstallAPIMutatesWhileFramesArrive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := newTestStore(t, path)
	ctx := context.Background()
	if _, err := store.EnsureIdentity(ctx); err != nil {
		t.Fatal(err)
	}
	in, rev, err := store.LoadInstall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.ValidateCommissionable(true); err != nil {
		t.Fatal(err)
	}
	radio := &feedRadio{frames: make(chan []byte, 16)}
	emu := tado.NewEmu(in.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(in, emu, radio, true, "mock")
	d.state = store
	runCtx, cancel := context.WithCancel(context.Background())
	go d.run(runCtx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	a := &installAPI{t: t, d: d, store: store, base: srv.URL, path: path, lastRev: rev}

	// The frames carry the fixture's key, not this database's, so they end up
	// counted as unhandled. The point is the engine goroutine being busy with
	// them while the reload arrives.
	stop := make(chan struct{})
	var feeder sync.WaitGroup
	feeder.Add(1)
	go func() {
		defer feeder.Done()
		frames := loadTestFrames(t)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case radio.frames <- frames[i%len(frames)]:
			}
		}
	}()

	a.declareDevices()
	a.mutate(http.MethodPost, "/api/install/zones", map[string]any{"number": 1, "kind": "heating"})
	a.mutate(http.MethodPut, "/api/install/zones/1/members",
		map[string]any{"members": []string{"RU9000000001", "VA9000000001"}})
	a.mutate(http.MethodPut, "/api/install/zones/1/reporter", map[string]any{"ref": "VA9000000001"})
	for range 4 {
		a.get("/api/install/zones")
		if code, res := a.call(http.MethodGet, "/api/nodes", nil); code != http.StatusOK {
			t.Fatalf("GET /api/nodes: got %d %v", code, res)
		}
	}
	close(stop)
	feeder.Wait()

	stored, got, err := newTestStore(t, path).LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != a.lastRev {
		t.Fatalf("stored rev = %d, want %d", got, a.lastRev)
	}
	if len(stored.Zones) != 1 || len(stored.Zones[0].Members) != 2 {
		t.Fatalf("stored zones = %+v, want one zone of two members", stored.Zones)
	}
}

// loadTestFrames returns a handful of frames to keep the engine busy. Their
// content is irrelevant: they only have to reach OnFrame.
func loadTestFrames(t *testing.T) [][]byte {
	t.Helper()
	var out [][]byte
	for i := range 8 {
		frame := make([]byte, 32)
		for j := range frame {
			frame[j] = byte(i*7 + j)
		}
		out = append(out, frame)
	}
	return out
}

// TestInstallAPIRefusesWithoutADatabase: the management routes are the only way
// in, so a daemon without a store says so instead of half-applying a change.
func TestInstallAPIRefusesWithoutADatabase(t *testing.T) {
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	var body map[string]any
	if code := postJSON(t, http.MethodPost, srv.URL+"/api/install/devices",
		map[string]any{"kind": "valve", "eui": apiValveLeftEUI}, &body); code != http.StatusServiceUnavailable {
		t.Fatalf("got %d %v, want 503", code, body)
	}
	// The read side still answers off the installation the daemon was given.
	if code := getJSON(t, srv.URL+"/api/install", &body); code != http.StatusOK {
		t.Fatalf("GET /api/install: got %d %v", code, body)
	}
	if body["rev"].(float64) != 0 {
		t.Fatalf("rev = %v, want 0 without a database", body["rev"])
	}
}

// The synthetic PAN keys the network-key tests write. Neither is the key
// EnsureIdentity generated, so a leak of either shows up as these bytes.
const (
	apiNetworkKeyHex    = "000102030405060708090a0b0c0d0e0f"
	apiNetworkKeyTwoHex = "101112131415161718191a1b1c1d1e1f"
)

// networkKeyPath is the one route that writes the key.
const networkKeyPath = "/api/install/network_key"

// storedKey reads the PAN key straight off the rows.
func (a *installAPI) storedKey() string {
	a.t.Helper()
	var key []byte
	err := a.store.db.QueryRow(`SELECT key FROM net_key WHERE id = 1`).Scan(&key)
	if err != nil || len(key) != install.NetworkKeyLen {
		a.t.Fatalf("network key: %v (%d bytes)", err, len(key))
	}
	return hex.EncodeToString(key)
}

// beacon asks the engine for a router advert. The engine builds it with the key
// it coordinates under, so the frame reports which key the reload left behind.
func (a *installAPI) beacon() []byte {
	a.t.Helper()
	v, err := a.d.do(func(e *tado.EmuState) (any, error) { return e.Beacon() })
	if err != nil {
		a.t.Fatalf("Beacon: %v", err)
	}
	return v.(tado.Action).Bytes
}

// TestInstallAPIWritesTheNetworkKeyWhileEmpty: the key of an installation with
// no device is writable, reaches the engine, and freezes with the first device.
func TestInstallAPIWritesTheNetworkKeyWhileEmpty(t *testing.T) {
	a := newInstallAPI(t)
	generated := a.storedKey()

	res := a.mutate(http.MethodPut, networkKeyPath, map[string]any{"key": apiNetworkKeyHex})
	if a.storedKey() != apiNetworkKeyHex {
		t.Fatal("the rows hold another key than the one written")
	}
	if hex.EncodeToString(a.d.inst.Load().NetworkKey()) != apiNetworkKeyHex {
		t.Fatal("the daemon serves an installation that carries another key")
	}
	view := res["install"].(map[string]any)
	if key := view["network_key"].(map[string]any); key["present"] != true {
		t.Fatalf("network_key = %v, want the written key reported as present", key)
	}

	// The engine protects what it sends with the new key, and no longer with
	// the generated one.
	frame := a.beacon()
	want, err := hex.DecodeString(apiNetworkKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tado.ParseBroadcast(want, frame); err != nil {
		t.Fatalf("the beacon does not decode under the written key: %v", err)
	}
	gone, err := hex.DecodeString(generated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tado.ParseBroadcast(gone, frame); err == nil {
		t.Fatal("the beacon still decodes under the generated key: the engine kept it")
	}

	a.declareDevices()
	code, refused := a.call(http.MethodPut, networkKeyPath, map[string]any{"key": apiNetworkKeyTwoHex})
	if code != http.StatusConflict {
		t.Fatalf("got %d %v, want 409", code, refused)
	}
	if msg := fmt.Sprint(refused["error"]); !strings.Contains(msg, "no device is declared") {
		t.Fatalf("error = %q, want it to name the rule", msg)
	}
	if a.storedKey() != apiNetworkKeyHex {
		t.Fatal("the refused write moved the key")
	}
	if got := revOf(t, a.get("/api/install")); got != a.lastRev {
		t.Fatalf("rev = %d, want %d: the refused write moved it", got, a.lastRev)
	}

	// No answer of this test carries any of the three keys.
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, body := range a.bodies {
		for _, needle := range []string{generated, apiNetworkKeyHex, apiNetworkKeyTwoHex} {
			if strings.Contains(strings.ToLower(body), needle) {
				t.Fatalf("answer %d leaks a network key: %s", i, body)
			}
		}
	}
}

// TestInstallAPIRefusesABadNetworkKey: a value that is not 16 bytes of hex
// answers 400, and a stale rev answers 409 like every other write.
func TestInstallAPIRefusesABadNetworkKey(t *testing.T) {
	a := newInstallAPI(t)
	// One accepted write, so the revision the stale case carries is behind.
	a.mutate(http.MethodPut, networkKeyPath, map[string]any{"key": apiNetworkKeyHex})

	for _, tc := range []struct{ name, key string }{
		{"empty", ""},
		{"too short", "0102030405060708"},
		{"too long", apiNetworkKeyHex + "20"},
		{"odd length", apiNetworkKeyHex[:31]},
		{"not hex", "zz0102030405060708090a0b0c0d0e0f"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, res := a.call(http.MethodPut, networkKeyPath, map[string]any{"key": tc.key})
			if code != http.StatusBadRequest {
				t.Fatalf("got %d %v, want 400", code, res)
			}
			if msg := fmt.Sprint(res["error"]); !strings.Contains(msg, "hex characters") {
				t.Fatalf("error = %q, want it to state the spelling", msg)
			}
		})
	}

	t.Run("stale rev", func(t *testing.T) {
		code, res := a.call(http.MethodPut, networkKeyPath,
			map[string]any{"key": apiNetworkKeyTwoHex, "rev": a.lastRev - 1})
		if code != http.StatusConflict {
			t.Fatalf("got %d %v, want 409", code, res)
		}
		if got := revOf(t, res); got != a.lastRev {
			t.Fatalf("rev = %d in the conflict body, want the current %d", got, a.lastRev)
		}
	})

	if a.storedKey() != apiNetworkKeyHex {
		t.Fatal("a refused write moved the key")
	}
	if got := revOf(t, a.get("/api/install")); got != a.lastRev {
		t.Fatalf("rev = %d, want %d: a refused write moved it", got, a.lastRev)
	}
}

// TestInstallAPIPatchesZoneOpenWindow: the two open window fields of a zone
// travel through the same PATCH the rest of the zone takes, and the rows keep
// them. A zone the API never touched answers with the captured defaults.
func TestInstallAPIPatchesZoneOpenWindow(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	zone := a.mutate(http.MethodPost, "/api/install/zones",
		map[string]any{"number": 1, "name": "lounge", "kind": "heating"})["zone"].(map[string]any)
	if zone["open_window_detection"] != true || zone["open_window_duration_sec"].(float64) != 900 {
		t.Fatalf("new zone = %v, want the detection on and 900 seconds", zone)
	}

	patched := a.mutate(http.MethodPatch, "/api/install/zones/1", map[string]any{
		"open_window_detection": false, "open_window_duration_sec": 1200,
	})["zone"].(map[string]any)
	if patched["open_window_detection"] != false || patched["open_window_duration_sec"].(float64) != 1200 {
		t.Fatalf("patched zone = %v, want the detection off and 1200 seconds", patched)
	}
	if got := a.get("/api/install/zones/1")["zone"].(map[string]any); got["open_window_detection"] != false ||
		got["open_window_duration_sec"].(float64) != 1200 {
		t.Fatalf("read back = %v, want what the patch wrote", got)
	}

	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatalf("LoadInstall: %v", err)
	}
	z, err := stored.ZoneByNumber(1)
	if err != nil {
		t.Fatal(err)
	}
	if z.OpenWindowEnabled() || z.OpenWindowSeconds() != 1200 {
		t.Errorf("stored zone = %v/%d, want off and 1200", z.OpenWindowEnabled(), z.OpenWindowSeconds())
	}

	// Tag 62c0 is a uint16: a duration the wire cannot carry is a bad request.
	if code, res := a.call(http.MethodPatch, "/api/install/zones/1",
		map[string]any{"open_window_duration_sec": 70000}); code != http.StatusBadRequest {
		t.Errorf("out of range duration = %d %v, want 400", code, res)
	}
	// One field at a time leaves the other one alone.
	only := a.mutate(http.MethodPatch, "/api/install/zones/1",
		map[string]any{"open_window_detection": true})["zone"].(map[string]any)
	if only["open_window_detection"] != true || only["open_window_duration_sec"].(float64) != 1200 {
		t.Errorf("zone = %v, want the duration kept at 1200", only)
	}
}

// served02b3 reads tag 02b3 of the d/config document the engine serves one
// device.
func (a *installAPI) served02b3(euiHex string) byte {
	a.t.Helper()
	v, err := a.d.do(func(e *tado.EmuState) (any, error) {
		for _, tlv := range tado.ParseTLVs(e.ExportDesiredState().Config[euiHex].Body) {
			if tlv.Tag == "02b3" && len(tlv.Val) == 1 {
				return tlv.Val[0], nil
			}
		}
		return nil, fmt.Errorf("the %s document carries no 02b3", euiHex)
	})
	if err != nil {
		a.t.Fatal(err)
	}
	return v.(byte)
}

// TestInstallAPIPatchesOfflineSchedule: options.offline_schedule defaults to on,
// the install patch turns it off and on for every device, and a per-device
// write through the node config route keeps its own value.
func TestInstallAPIPatchesOfflineSchedule(t *testing.T) {
	a := newInstallAPI(t)
	a.declareDevices()
	a.reportThermostatModule(71)
	devices := []string{apiThermostatEUI, apiValveEUI, apiValveTwoEUI}
	expect := func(step string, view map[string]any, want bool, served map[string]byte) {
		t.Helper()
		if view["offline_schedule"] != want {
			t.Errorf("%s: offline_schedule = %v, want %t", step, view["offline_schedule"], want)
		}
		for _, eui := range devices {
			if got := a.served02b3(eui); got != served[eui] {
				t.Errorf("%s: %s 02b3 = %d, want %d", step, eui, got, served[eui])
			}
		}
	}
	all := func(v byte) map[string]byte {
		return map[string]byte{apiThermostatEUI: v, apiValveEUI: v, apiValveTwoEUI: v}
	}
	expect("default", a.get("/api/install"), true, all(1))

	a.mutate(http.MethodPatch, "/api/install", map[string]any{"offline_schedule": false})
	expect("off", a.get("/api/install"), false, all(0))
	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatalf("LoadInstall: %v", err)
	}
	if stored.Options.OfflineScheduleEnabled() {
		t.Error("the stored installation still has the offline schedule on")
	}

	wire, err := tado.ParseEUI(apiValveEUI)
	if err != nil {
		t.Fatal(err)
	}
	var set struct {
		Queued bool   `json:"queued"`
		Error  string `json:"error"`
	}
	if code := postJSON(t, http.MethodPost, a.base+"/api/nodes/"+tado.ShortFromWire(wire)+"/config",
		map[string]any{"offlineScheduleEnabled": false, "id": 1}, &set); code != http.StatusOK {
		t.Fatalf("per-device write: got %d %+v", code, set)
	}
	on := all(1)
	on[apiValveEUI] = 0
	a.mutate(http.MethodPatch, "/api/install", map[string]any{"offline_schedule": true})
	expect("on with a per-device value", a.get("/api/install"), true, on)
}

// TestInstallAPIPatchesExternalWindowMax: options.external_window_max_sec
// reads two hours by default, the install patch stores a new cap, and a cap
// that is not positive is refused.
func TestInstallAPIPatchesExternalWindowMax(t *testing.T) {
	a := newInstallAPI(t)
	if got := a.get("/api/install")["external_window_max_sec"]; got != float64(7200) {
		t.Errorf("default external_window_max_sec = %v, want 7200", got)
	}
	a.mutate(http.MethodPatch, "/api/install", map[string]any{"external_window_max_sec": 600})
	if got := a.get("/api/install")["external_window_max_sec"]; got != float64(600) {
		t.Errorf("patched external_window_max_sec = %v, want 600", got)
	}
	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatalf("LoadInstall: %v", err)
	}
	if got := stored.Options.ExternalWindowMax(); got != 10*time.Minute {
		t.Errorf("stored cap = %s, want 10m", got)
	}
	if code, body := a.call(http.MethodPatch, "/api/install", map[string]any{"external_window_max_sec": 0}); code != http.StatusBadRequest {
		t.Errorf("a zero cap: code=%d %v, want 400", code, body)
	}
}

// TestInstallAPIPatchesRouterIPv4: the router advertisement addresses read the
// unspecified address by default, the install patch stores new ones, null or ""
// returns them to the default, and a value that is not IPv4 is refused.
func TestInstallAPIPatchesRouterIPv4(t *testing.T) {
	a := newInstallAPI(t)
	view := a.get("/api/install")
	if view["router_gateway"] != "0.0.0.0" || view["router_dns"] != "0.0.0.0" {
		t.Errorf("defaults = %v, %v, want 0.0.0.0 and 0.0.0.0", view["router_gateway"], view["router_dns"])
	}
	a.mutate(http.MethodPatch, "/api/install", map[string]any{"router_gateway": "10.1.2.1", "router_dns": "10.1.2.2"})
	view = a.get("/api/install")
	if view["router_gateway"] != "10.1.2.1" || view["router_dns"] != "10.1.2.2" {
		t.Errorf("patched = %v, %v, want 10.1.2.1 and 10.1.2.2", view["router_gateway"], view["router_dns"])
	}
	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatalf("LoadInstall: %v", err)
	}
	if gateway, dns := stored.Options.RouterIPv4(); gateway.String() != "10.1.2.1" || dns.String() != "10.1.2.2" {
		t.Errorf("stored = %v, %v, want 10.1.2.1 and 10.1.2.2", gateway, dns)
	}
	for name, body := range map[string]map[string]any{
		"gateway not an address": {"router_gateway": "gateway"},
		"dns out of range":       {"router_dns": "10.1.2.256"},
		"ipv6":                   {"router_gateway": "fe80::1"},
		"a number":               {"router_dns": 8},
	} {
		if code, resp := a.call(http.MethodPatch, "/api/install", body); code != http.StatusBadRequest {
			t.Errorf("%s: code=%d %v, want 400", name, code, resp)
		}
	}
	a.mutate(http.MethodPatch, "/api/install", map[string]any{"router_gateway": nil, "router_dns": ""})
	view = a.get("/api/install")
	if view["router_gateway"] != "0.0.0.0" || view["router_dns"] != "0.0.0.0" {
		t.Errorf("after the clear = %v, %v, want 0.0.0.0 and 0.0.0.0", view["router_gateway"], view["router_dns"])
	}
}

// TestInstallAPIPatchesAlertThresholds: the device alert thresholds read their
// defaults, the install patch stores new ones, and a value that is not positive
// is refused.
func TestInstallAPIPatchesAlertThresholds(t *testing.T) {
	a := newInstallAPI(t)
	view := a.get("/api/install")
	if view["low_battery_cell_mv"] != float64(1100) || view["offline_after_sec"] != float64(3600) {
		t.Errorf("defaults = %v mV, %v s, want 1100 mV and 3600 s", view["low_battery_cell_mv"], view["offline_after_sec"])
	}
	a.mutate(http.MethodPatch, "/api/install", map[string]any{"low_battery_cell_mv": 1200, "offline_after_sec": 600})
	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatalf("LoadInstall: %v", err)
	}
	if got := stored.Options.LowBatteryMV(tado.KindThermostat); got != 3600 {
		t.Errorf("stored thermostat limit = %d mV, want 3600", got)
	}
	if got := stored.Options.OfflineAfter(); got != 10*time.Minute {
		t.Errorf("stored offline limit = %s, want 10m", got)
	}
	for _, key := range []string{"low_battery_cell_mv", "offline_after_sec"} {
		if code, body := a.call(http.MethodPatch, "/api/install", map[string]any{key: 0}); code != http.StatusBadRequest {
			t.Errorf("a zero %s: code=%d %v, want 400", key, code, body)
		}
	}
}
