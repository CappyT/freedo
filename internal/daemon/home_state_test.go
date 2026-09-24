package daemon

// home_state_test: the state of the whole home, the weekly program the daemon
// evaluates on its own clock, and the overlay timers it keeps.

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"freedo/internal/radio"
	"freedo/tado"
)

// testClock is a clock the test goroutine moves while the engine reads it.
type testClock struct{ nanos atomic.Int64 }

func newTestClock(at time.Time) *testClock {
	c := &testClock{}
	c.nanos.Store(at.UnixNano())
	return c
}

func (c *testClock) now() time.Time { return time.Unix(0, c.nanos.Load()).UTC() }

func (c *testClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

// newHomeDaemon starts a dry-run daemon on defaultInstall with an injected
// clock, and returns it with its API base URL.
func newHomeDaemon(t *testing.T, clock *testClock) (*daemon, string) {
	t.Helper()
	cfg := defaultInstall.EmuConfig()
	cfg.Now = clock.now
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	d.retryNow = clock.now
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	srv := httptest.NewServer(d.mux())
	t.Cleanup(func() {
		srv.Close()
		cancel()
	})
	return d, srv.URL
}

// allDayProgram is one weekly program covering the seven days.
func allDayProgram(entries ...map[string]any) map[string]any {
	return map[string]any{"programs": []map[string]any{{"days": 127, "entries": entries}}}
}

// nextZoneWrite pulls the head of the queue of target and returns its TLVs,
// keyed by tag.
func nextZoneWrite(t *testing.T, d *daemon, target string) map[string]string {
	t.Helper()
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		actions, ok, err := e.NextQueuedDownlink(target)
		if err != nil {
			return nil, err
		}
		if !ok || len(actions) == 0 {
			return nil, errors.New("no queued downlink for " + target)
		}
		return actions[0].Bytes, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := tado.ParseData(defaultInstall.NetworkKey(), v.([]byte))
	if err != nil {
		t.Fatal(err)
	}
	app := tado.ParseApp(frame.Plaintext)
	if app == nil || app.CoAP == nil {
		t.Fatalf("queued downlink for %s does not parse as CoAP", target)
	}
	tags := make(map[string]string, len(app.TLVs))
	for _, tlv := range app.TLVs {
		tags[tlv.Tag] = hex.EncodeToString(tlv.Val)
	}
	return tags
}

func dropQueue(t *testing.T, d *daemon, target string) {
	t.Helper()
	if _, err := d.do(func(e *tado.EmuState) (any, error) { return e.DropQueue(target), nil }); err != nil {
		t.Fatal(err)
	}
}

// wantTags checks the tags a write must carry, and absent the ones it must not.
func wantTags(t *testing.T, what string, tags map[string]string, want map[string]string, absent ...string) {
	t.Helper()
	for tag, value := range want {
		if tags[tag] != value {
			t.Errorf("%s: %s=%q, want %q", what, tag, tags[tag], value)
		}
	}
	for _, tag := range absent {
		if value, ok := tags[tag]; ok {
			t.Errorf("%s: %s=%q is present", what, tag, value)
		}
	}
}

// TestSetpointDurationQueuesReturnToSchedule: a setpoint write with a duration
// arms the coordinator-side timer, and the timer queues the return to schedule
// on the injected clock. No duration goes on the air.
func TestSetpointDurationQueuesReturnToSchedule(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url := newHomeDaemon(t, clock)
	target := hex.EncodeToString(mustResolve(t, "VA1000000005").WireEUI())

	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule",
		allDayProgram(map[string]any{"minute": 0, "setpoint_c": 20.0}), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	if code := postJSON(t, http.MethodPost, url+"/api/nodes/VA1000000005/setpoint",
		map[string]any{"tempC": 21.0, "mode": "manual", "durationSec": 1800, "id": 10}, &view); code != http.StatusOK {
		t.Fatalf("setpoint: code=%d %v", code, view)
	}
	wantTags(t, "manual set with a duration", nextZoneWrite(t, d, target), map[string]string{
		"6160": "01", "61e0": "01", "6200": "07d0", "6240": "02", "6280": "0834", "6440": "0001",
	})
	dropQueue(t, d, target)

	clock.advance(30 * time.Minute)
	v, err := d.do(func(e *tado.EmuState) (any, error) { return e.ExpireBoosts(d.retryNow()) })
	if err != nil {
		t.Fatal(err)
	}
	if due := v.([]string); len(due) != 1 || due[0] != target {
		t.Fatalf("expired timers=%v, want the one on %s", due, target)
	}
	wantTags(t, "the timer restores the program", nextZoneWrite(t, d, target), map[string]string{
		"6160": "01", "6180": "00", "6020": "01", "61e0": "01", "6200": "07d0", "6240": "00", "62e0": "00",
	}, "6260", "6280", "6440")
}

// TestAwayToggleRewritesEveryZone: the switch to away queues one write per zone,
// to its writer alone, keeping the overlay the zone already runs, so none
// carries 6440. The other members take the block from the writer's echo.
func TestAwayToggleRewritesEveryZone(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url := newHomeDaemon(t, clock)

	var res struct {
		Away   bool `json:"away"`
		Queued int  `json:"queued"`
	}
	if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &res); code != http.StatusOK {
		t.Fatalf("away: code=%d %+v", code, res)
	}
	// A writer of two zones takes one write per zone.
	writes := map[string]int{}
	for i := range defaultInstall.Zones {
		list, err := defaultInstall.ZoneMembers(defaultInstall.Zones[i].Number)
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range list {
			writes[hex.EncodeToString(member.WireEUI())] += 0
		}
		writer, err := zoneWriter(defaultInstall, &defaultInstall.Zones[i])
		if err != nil {
			t.Fatal(err)
		}
		writes[hex.EncodeToString(writer.WireEUI())]++
	}
	if !res.Away || res.Queued != len(defaultInstall.Zones) {
		t.Fatalf("away response=%+v, want one write per zone (%d)", res, len(defaultInstall.Zones))
	}
	for target, want := range writes {
		v, err := d.do(func(e *tado.EmuState) (any, error) { return e.Pending(target), nil })
		if err != nil {
			t.Fatal(err)
		}
		if got := v.(int); got != want {
			t.Errorf("%s holds %d queued writes, want %d", target, got, want)
		}
		if want == 0 {
			continue
		}
		wantTags(t, "the away burst on "+target, nextZoneWrite(t, d, target), map[string]string{
			"6160": "02", "61e0": "00",
		}, "6200", "6440")
	}

	var state struct {
		Away bool `json:"away"`
	}
	if code := getJSON(t, url+"/api/home", &state); code != http.StatusOK || !state.Away {
		t.Fatalf("home state=%+v code=%d, want away", state, code)
	}
}

// TestScheduleBlockChangePushesSetpoint: when the weekly program moves to its
// next entry the daemon pushes the new 6200 to the zone members, with no 6440.
func TestScheduleBlockChangePushesSetpoint(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 5, 5, 0, 0, 0, time.UTC))
	d, url := newHomeDaemon(t, clock)
	target := hex.EncodeToString(mustResolve(t, "VA1000000005").WireEUI())

	var view map[string]any
	if code := postJSON(t, http.MethodPut, url+"/api/zones/10/schedule", allDayProgram(
		map[string]any{"minute": 0, "setpoint_c": 20.0},
		map[string]any{"minute": 360, "setpoint_c": 18.0},
	), &view); code != http.StatusOK {
		t.Fatalf("schedule: code=%d %v", code, view)
	}
	if _, err := d.do(func(e *tado.EmuState) (any, error) { d.tickHomeState(e); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	dropQueue(t, d, target)

	clock.advance(2 * time.Hour) // past minute 360
	if _, err := d.do(func(e *tado.EmuState) (any, error) { d.tickHomeState(e); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	wantTags(t, "the schedule block change", nextZoneWrite(t, d, target), map[string]string{
		"6160": "01", "61e0": "01", "6200": "0708",
	}, "6440")
}

// TestHomeStateSurvivesStoreReload: the away flag is durable, so a restart
// keeps writing the state the operator left the home in.
func TestHomeStateSurvivesStoreReload(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	away, err := store.Away(context.Background())
	if err != nil || away {
		t.Fatalf("a fresh database reads away=%t err=%v, want home", away, err)
	}
	if err := store.SetAway(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if away, err := reopened.Away(context.Background()); err != nil || !away {
		t.Fatalf("reloaded away=%t err=%v, want away", away, err)
	}
}

func mustResolve(t *testing.T, ref string) *tado.Device {
	t.Helper()
	dev, err := defaultInstall.Resolve(ref)
	if err != nil {
		t.Fatal(err)
	}
	return dev
}
