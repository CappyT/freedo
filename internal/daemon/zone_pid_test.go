package daemon

// zone_pid_test: the controller gains of a zone on /api/install/zones/{n}.
// Frames and installation are synthetic.

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/http"
	"strconv"
	"testing"

	"freedo/tado"
)

// zoneConfigGET feeds the valve's GET of h/<home>/z/<zone>/config, with etag
// as option 4, and returns the answer. One 16-byte block keeps the answer
// inside a single frame; every block carries the ETag of the whole document.
func zoneConfigGET(t *testing.T, d *daemon, ref string, zone int, mid uint16, etag []byte) *tado.CoAPMessage {
	t.Helper()
	in := d.inst.Load()
	var options []tado.CoAPOption
	if etag != nil {
		options = append(options, tado.CoAPOption{Num: 4, Val: etag})
	}
	options = append(options, tado.CoAPOption{Num: 23, Val: []byte{0x00}})
	frame := uplinkFrame(t, in, ref, false, tado.CoAPReq{
		Type: "CON", Code: "0.01", MID: mid, Token: []byte{0x0e},
		Path:    []string{"h", "7654321", "z", strconv.Itoa(zone), "config"},
		Options: options,
	})
	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.OnFrame(frame), nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range v.([]tado.Action) {
		if a.Kind != tado.ActData {
			continue
		}
		answer, err := tado.ParseData(in.NetworkKey(), a.Bytes)
		if err != nil {
			continue
		}
		if app := tado.ParseApp(answer.Plaintext); app != nil && app.CoAP != nil && app.CoAP.MID == mid {
			return app.CoAP
		}
	}
	t.Fatalf("no answer with mid=%04x", mid)
	return nil
}

func etagOf(t *testing.T, m *tado.CoAPMessage) []byte {
	t.Helper()
	etag, ok := m.Option(4)
	if !ok || len(etag.Val) == 0 {
		t.Fatalf("%s answer carries no ETag", m.Code)
	}
	return etag.Val
}

func pidView(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	pid, ok := res["zone"].(map[string]any)["pid"].(map[string]any)
	if !ok {
		t.Fatalf("zone view = %v, want a pid object", res["zone"])
	}
	return pid
}

func servedGains(t *testing.T, d *daemon, zone int) string {
	t.Helper()
	doc, err := tado.BuildZoneConfigDocument(d.inst.Load(), zone)
	if err != nil {
		t.Fatal(err)
	}
	out := ""
	for _, tlv := range tado.ParseTLVs(doc.Body) {
		switch tlv.Tag {
		case "6080", "60a0", "60c0":
			out += tlv.Tag + "=" + hex.EncodeToString(tlv.Val) + " "
		}
	}
	return out
}

// TestZonePIDPatch: zone 3 serves the default gains. A PATCH of kp alone keeps
// ki and kd, serves the document under a new ETag that the valve's conditional
// GET picks up, and queues no write to the valve. A clear returns the document
// and its ETag to the default.
func TestZonePIDPatch(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	d := a.d
	in := d.inst.Load()
	valve := wireOf(t, in, "VA1000000002")
	dropQueue(t, d, valve)

	pid := pidView(t, a.get("/api/install/zones/3"))
	if pid["kp"] != 1000.0 || pid["ki"] != 500.0 || pid["kd"] != 1500.0 {
		t.Errorf("default pid = %v, want kp 1000, ki 500, kd 1500", pid)
	}
	if hot := a.get("/api/install/zones/0")["zone"].(map[string]any)["pid"]; hot != nil {
		t.Errorf("hot water zone pid = %v, want null", hot)
	}
	first := zoneConfigGET(t, d, "VA1000000002", 3, 0xe101, nil)
	if first.Code != "2.05" {
		t.Fatalf("first GET = %s, want 2.05", first.Code)
	}
	held := append([]byte(nil), etagOf(t, first)...)

	pid = pidView(t, a.mutate(http.MethodPatch, "/api/install/zones/3", map[string]any{"pid": map[string]any{"kp": 2000}}))
	if pid["kp"] != 2000.0 || pid["ki"] != 500.0 || pid["kd"] != 1500.0 {
		t.Errorf("pid after the PATCH = %v, want kp 2000, ki 500, kd 1500", pid)
	}
	if got := servedGains(t, d, 3); got != "6080=07d0 60a0=01f4 60c0=05dc " {
		t.Errorf("served gains = %q, want 6080=07d0 60a0=01f4 60c0=05dc", got)
	}
	if got := servedGains(t, d, 10); got != "6080=03e8 60a0=01f4 60c0=05dc " {
		t.Errorf("zone 10 gains = %q, want the default", got)
	}
	changed := zoneConfigGET(t, d, "VA1000000002", 3, 0xe102, held)
	if changed.Code != "2.05" || bytes.Equal(etagOf(t, changed), held) {
		t.Errorf("GET with the held ETag = %s %x, want 2.05 with a new ETag", changed.Code, etagOf(t, changed))
	}
	if n := pending(t, d, valve); n != 0 {
		t.Errorf("%d queued writes to the valve, want none", n)
	}

	pid = pidView(t, a.mutate(http.MethodPatch, "/api/install/zones/3", map[string]any{"pid": nil}))
	if pid["kp"] != 1000.0 || pid["ki"] != 500.0 || pid["kd"] != 1500.0 {
		t.Errorf("pid after the clear = %v, want the default", pid)
	}
	if again := zoneConfigGET(t, d, "VA1000000002", 3, 0xe103, held); again.Code != "2.03" {
		t.Errorf("GET with the default ETag after the clear = %s, want 2.03", again.Code)
	}
	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Options.ZonePIDs) != 0 {
		t.Errorf("stored gains = %v, want none after the clear", stored.Options.ZonePIDs)
	}
}

// TestZonePIDPersists: the gains live in the installation rows, so a daemon
// started on them serves the same document.
func TestZonePIDPersists(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	a.mutate(http.MethodPatch, "/api/install/zones/3", map[string]any{"pid": map[string]any{"kp": 800, "ki": 0, "kd": 3000}})
	stored, _, err := a.store.LoadInstall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stored.ValidateCommissionable(true); err != nil {
		t.Fatal(err)
	}
	if got := stored.Options.ZonePID(3); got != (tado.ZonePID{Kp: 800, Ki: 0, Kd: 3000}) {
		t.Errorf("stored zone 3 gains = %+v, want 800/0/3000", got)
	}

	d, url := newWindowDaemon(t, stored, newTestClock(windowStart), a.store)
	var zone map[string]any
	if code := getJSON(t, url+"/api/install/zones/3", &zone); code != http.StatusOK {
		t.Fatalf("zone after restart: code=%d", code)
	}
	if pid := pidView(t, zone); pid["kp"] != 800.0 || pid["ki"] != 0.0 || pid["kd"] != 3000.0 {
		t.Errorf("pid after restart = %v, want 800/0/3000", pid)
	}
	if got := servedGains(t, d, 3); got != "6080=0320 60a0=0000 60c0=0bb8 " {
		t.Errorf("served gains after restart = %q, want 6080=0320 60a0=0000 60c0=0bb8", got)
	}
}

func TestZonePIDRejects(t *testing.T) {
	a := newInstallAPIOn(t, windowInstall(t, nil))
	for _, tc := range []struct {
		name string
		zone string
		pid  any
	}{
		{"kp and ki 0", "3", map[string]any{"kp": 0, "ki": 0}},
		{"gain over uint16", "3", map[string]any{"kd": 65536}},
		{"negative gain", "3", map[string]any{"ki": -1}},
		{"hot water zone", "0", map[string]any{"kp": 1200}},
		{"not an object", "3", 1000},
		{"unknown field", "3", map[string]any{"kp": 1200, "gain": 7}},
		{"case-mismatched key", "3", map[string]any{"kP": 1200}},
		{"upper-case key", "3", map[string]any{"KD": 1200}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, res := a.call(http.MethodPatch, "/api/install/zones/"+tc.zone, map[string]any{"pid": tc.pid})
			if code != http.StatusBadRequest {
				t.Errorf("code=%d %v, want 400", code, res)
			}
		})
	}
	if got := servedGains(t, a.d, 3); got != "6080=03e8 60a0=01f4 60c0=05dc " {
		t.Errorf("served gains = %q, want the default after the refused writes", got)
	}
}
