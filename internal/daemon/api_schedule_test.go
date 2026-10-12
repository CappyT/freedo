package daemon

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"freedo/internal/radio"
	"freedo/tado"
)

// scheduleDaemon starts a daemon on the fixture installation and returns it
// with its API base URL. The engine reference is what feeds a device uplink in.
func scheduleDaemon(t *testing.T) (*daemon, string) {
	t.Helper()
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	return d, srv.URL
}

// weekPlan is a working week and a weekend program, the day selection given
// once as names and once as the mask itself.
func weekPlan() map[string]any {
	return map[string]any{"programs": []map[string]any{
		{"days": []string{"mon", "tue", "wed", "thu", "fri"}, "entries": []map[string]any{
			{"minute": 0, "setpoint_c": 16.0},
			{"minute": 390, "setpoint_c": 21.0},
			{"minute": 1380, "setpoint_c": 16.5, "enabled": false},
		}},
		{"days": 0x60, "entries": []map[string]any{
			{"minute": 0, "setpoint_c": 18.0},
			{"minute": 1439, "setpoint_c": 5.0},
		}},
	}}
}

// TestAPIZoneScheduleRoundTrip: the PUT stores the two day programs and the GET
// reads them back. A zone with no document answers 404.
func TestAPIZoneScheduleRoundTrip(t *testing.T) {
	_, url := scheduleDaemon(t)
	var stored scheduleView
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule", weekPlan(), &stored); code != http.StatusOK {
		t.Fatalf("put schedule: got %d, want 200 (%+v)", code, stored)
	}
	if stored.Zone != 10 || len(stored.Members) != 1 || len(stored.Programs) != 2 {
		t.Fatalf("stored = %+v, want zone 10, one member and two programs", stored)
	}
	week := stored.Programs[0]
	if week.Mask != 0x1f || !reflect.DeepEqual(week.Days, []string{"mon", "tue", "wed", "thu", "fri"}) {
		t.Errorf("working week = %+v, want mask 1f named mon..fri", week)
	}
	if len(week.Entries) != 3 || week.Entries[2].SetpointC != 16.5 || week.Entries[2].Enabled {
		t.Errorf("working week entries = %+v", week.Entries)
	}
	weekend := stored.Programs[1]
	if weekend.Mask != 0x60 || !reflect.DeepEqual(weekend.Days, []string{"sat", "sun"}) || weekend.Entries[1].Minute != 1439 {
		t.Errorf("weekend = %+v, want mask 60 named sat,sun", weekend)
	}

	var read scheduleView
	if code := getJSON(t, url+"/api/zones/10/schedule", &read); code != http.StatusOK {
		t.Fatalf("get schedule: got %d", code)
	}
	if !reflect.DeepEqual(read, stored) {
		t.Errorf("read = %+v, want %+v", read, stored)
	}

	var missing struct {
		Error string `json:"error"`
	}
	if code := getJSON(t, url+"/api/zones/3/schedule", &missing); code != http.StatusNotFound {
		t.Errorf("zone with no schedule: got %d, want 404", code)
	}
	if code := getJSON(t, url+"/api/zones/99/schedule", &missing); code != http.StatusNotFound {
		t.Errorf("unknown zone: got %d, want 404", code)
	}
}

// TestAPIZoneScheduleRejectsBrokenWeek: every rejected plan answers 400 and
// names the rule it broke.
func TestAPIZoneScheduleRejectsBrokenWeek(t *testing.T) {
	_, url := scheduleDaemon(t)
	day := []map[string]any{{"minute": 0, "setpoint_c": 18.0}}
	for _, tc := range []struct {
		name string
		plan map[string]any
		want string
	}{
		{"overlapping masks", map[string]any{"programs": []map[string]any{
			{"days": 0x3f, "entries": day}, {"days": 0x60, "entries": day},
		}}, "must not share a day"},
		{"missing day", map[string]any{"programs": []map[string]any{
			{"days": []string{"mon", "tue", "wed", "thu", "fri"}, "entries": day},
			{"days": []string{"sat"}, "entries": day},
		}}, "all seven day bits"},
		{"first entry after 0", map[string]any{"programs": []map[string]any{
			{"days": 0x7f, "entries": []map[string]any{{"minute": 60, "setpoint_c": 18.0}}},
		}}, "must start at minute 0"},
		{"decreasing minutes", map[string]any{"programs": []map[string]any{
			{"days": 0x7f, "entries": []map[string]any{
				{"minute": 0, "setpoint_c": 18.0},
				{"minute": 360, "setpoint_c": 21.0},
				{"minute": 120, "setpoint_c": 16.0},
			}},
		}}, "strictly increasing"},
		{"unknown day name", map[string]any{"programs": []map[string]any{
			{"days": []string{"lunedi"}, "entries": day},
		}}, "not a day name"},
		{"no program", map[string]any{"programs": []map[string]any{}}, "programs is required"},
	} {
		var body struct {
			Error string `json:"error"`
		}
		code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule", tc.plan, &body)
		if code != http.StatusBadRequest || !strings.Contains(body.Error, tc.want) {
			t.Errorf("%s: got %d %q, want 400 naming %q", tc.name, code, body.Error, tc.want)
		}
	}
	var missing struct {
		Error string `json:"error"`
	}
	if code := getJSON(t, url+"/api/zones/10/schedule", &missing); code != http.StatusNotFound {
		t.Errorf("a rejected plan stored a document: got %d, want 404", code)
	}
}

// TestAPIZoneScheduleServedWithFreshETag: after a PUT the zone member reads the
// document with a 2.05, the same ETag answers 2.03, and a second PUT changes the
// ETag so the device reads the new document.
func TestAPIZoneScheduleServedWithFreshETag(t *testing.T) {
	d, url := scheduleDaemon(t)
	bedroom, err := defaultInstall.Resolve("VA1000000005")
	if err != nil {
		t.Fatal(err)
	}
	var stored scheduleView
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule", weekPlan(), &stored); code != http.StatusOK {
		t.Fatalf("put schedule: got %d", code)
	}

	first := deviceScheduleGET(t, d, bedroom, 0xc100, nil)
	if first.Code != "2.05" || len(first.Payload) == 0 {
		t.Fatalf("first read: code=%s payload=%x, want a 2.05 document", first.Code, first.Payload)
	}
	etag, ok := first.Option(4)
	if !ok || len(etag.Val) != 8 {
		t.Fatalf("first read ETag=%x, want 8 bytes", etag.Val)
	}
	stamp := append([]byte(nil), etag.Val...)

	valid := deviceScheduleGET(t, d, bedroom, 0xc101, stamp)
	if valid.Code != "2.03" || len(valid.Payload) != 0 {
		t.Errorf("conditional read: code=%s payload=%x, want an empty 2.03", valid.Code, valid.Payload)
	}

	changed := weekPlan()
	changed["programs"].([]map[string]any)[1]["entries"] = []map[string]any{{"minute": 0, "setpoint_c": 19.0}}
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule", changed, &stored); code != http.StatusOK {
		t.Fatalf("second put: got %d", code)
	}
	again := deviceScheduleGET(t, d, bedroom, 0xc102, stamp)
	if again.Code != "2.05" {
		t.Fatalf("read after the change: code=%s, want 2.05", again.Code)
	}
	fresh, ok := again.Option(4)
	if !ok || reflect.DeepEqual(fresh.Val, stamp) {
		t.Errorf("ETag after the change = %x, want a different stamp", fresh.Val)
	}
}

// deviceScheduleGET feeds one conditional GET of the zone schedule resource into
// the engine and returns the answer it built.
func deviceScheduleGET(t *testing.T, d *daemon, dev *tado.Device, mid uint16, etag []byte) *tado.CoAPMessage {
	t.Helper()
	var options []tado.CoAPOption
	if etag != nil {
		options = append(options, tado.CoAPOption{Num: 4, Val: etag})
	}
	pkt, err := tado.BuildCoAP(tado.CoAPReq{
		Type: "CON", Code: "0.01", MID: mid, Token: []byte{0x0c},
		Path:    []string{"h", "7654321", "z", "10", "sched"},
		Options: options,
		SrcEUI:  dev.WireEUI(), DstEUI: defaultInstall.CoordinatorEUI(),
		SrcPort: 5683, DstPort: 4005,
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.BuildData(defaultInstall.NetworkKey(), 0x22, defaultInstall.CoordinatorEUI(),
		dev.WireEUI()[:5], tado.WrapAppPayload(pkt, 700, nil))
	if err != nil {
		t.Fatal(err)
	}
	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.OnFrame(frame.Bytes()), nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range v.([]tado.Action) {
		if a.Kind != tado.ActData {
			continue
		}
		answer, err := tado.ParseData(defaultInstall.NetworkKey(), a.Bytes)
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

// takeQueue returns how many downlinks wait for ref and the CoAP request of
// the head one, and empties the queue.
func takeQueue(t *testing.T, d *daemon, ref string) (int, *tado.CoAPMessage) {
	t.Helper()
	target := hex.EncodeToString(mustResolve(t, ref).WireEUI())
	var pending int
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		pending = e.Pending(target)
		actions, ok, err := e.NextQueuedDownlink(target)
		e.DropQueue(target)
		if err != nil || !ok || len(actions) == 0 {
			return nil, err
		}
		return actions[0].Bytes, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if v == nil {
		return pending, nil
	}
	frame, err := tado.ParseData(defaultInstall.NetworkKey(), v.([]byte))
	if err != nil {
		t.Fatal(err)
	}
	app := tado.ParseApp(frame.Plaintext)
	if app == nil || app.CoAP == nil {
		t.Fatalf("queued downlink for %s does not parse as CoAP", ref)
	}
	return pending, app.CoAP
}

// TestAPIScheduleEditQueuesFetchTrigger: a zone schedule PUT queues one empty
// POST d/sched to every member, the wall thermostat of a heating zone
// included, and so does the node schedule route. The hot-water program stays
// with the daemon and queues nothing.
func TestAPIScheduleEditQueuesFetchTrigger(t *testing.T) {
	d, url := scheduleDaemon(t)
	oneTrigger := func(ref, step string) {
		t.Helper()
		n, head := takeQueue(t, d, ref)
		if n != 1 || head == nil || head.Code != "0.02" || head.URI() != "d/sched" || len(head.Payload) != 0 {
			t.Errorf("%s after the %s: pending=%d head=%v, want one empty POST d/sched", ref, step, n, head)
		}
	}
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/1/schedule", weekPlan(), &view); code != http.StatusOK {
		t.Fatalf("put zone 1 schedule: code=%d %v", code, view)
	}
	for _, ref := range []string{"RU1000000001", "VA1000000003"} {
		oneTrigger(ref, "zone PUT")
	}

	if code := postJSON(t, http.MethodPut, url+"/api/zones/0/schedule", hotWaterWeek(), &view); code != http.StatusOK {
		t.Fatalf("put hot-water schedule: code=%d %v", code, view)
	}
	if n, head := takeQueue(t, d, "RU1000000001"); n != 0 {
		t.Errorf("thermostat after the hot-water PUT: pending=%d head=%v, want nothing", n, head)
	}

	body := map[string]any{"dayMask": 127, "entries": []map[string]any{{"startMin": 0, "tempC": 19.0}}}
	if code := postJSON(t, http.MethodPut, url+"/api/nodes/VA1000000007/schedule", body, &view); code != http.StatusOK {
		t.Fatalf("put node schedule: code=%d %v", code, view)
	}
	oneTrigger("VA1000000007", "node PUT")
}

// TestNodeScheduleWritesTheZoneProgram: the node schedule route on a member of
// a heating zone writes the home program of that zone in the active preset:
// every member is served it, the engine follows it and the zone GET reads it.
func TestNodeScheduleWritesTheZoneProgram(t *testing.T) {
	d, url := scheduleDaemon(t)
	body := map[string]any{"dayMask": 127, "entries": []map[string]any{{"startMin": 0, "tempC": 19.5}}}
	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/nodes/VA1000000001/schedule", body, &view); code != http.StatusOK {
		t.Fatalf("put node schedule: code=%d %v", code, view)
	}
	want := []tado.ScheduleProgram{{DayMask: 0x7f, Entries: []tado.ScheduleEntry{{Minute: 0, SetpointCenti: 1950, Enabled: true}}}}
	for _, ref := range []string{"VA1000000001", "VA1000000006"} {
		target := hex.EncodeToString(mustResolve(t, ref).WireEUI())
		got := engineValue(t, d, func() []tado.ScheduleProgram {
			p, _, _ := d.emu.Schedule(target)
			return p
		})
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s is served %v, want %v", ref, got, want)
		}
	}
	if got := engineValue(t, d, func() []tado.ScheduleProgram { p, _ := d.zonePrograms(5); return p }); !reflect.DeepEqual(got, want) {
		t.Errorf("the engine follows %v on zone 5, want %v", got, want)
	}
	var read scheduleView
	if code := getJSON(t, url+"/api/zones/5/schedule", &read); code != http.StatusOK || read.Programs[0].Entries[0].SetpointC != 19.5 {
		t.Errorf("GET zone 5: code=%d %+v, want the 19.5 C program", code, read.Programs)
	}
}
