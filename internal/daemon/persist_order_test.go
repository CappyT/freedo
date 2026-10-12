package daemon

// persist_order_test: a handler that changes engine state stores the value the
// engine took, in the order the engine took it, however the requests race.

import (
	"context"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"freedo/tado"
)

func openTestStore(t *testing.T) *stateStore {
	t.Helper()
	store, err := openStateStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestConcurrentHomePatchStoresEngineState: racing PATCH /api/home requests
// leave the stored away flag equal to the engine's.
func TestConcurrentHomePatchStoresEngineState(t *testing.T) {
	store := openTestStore(t)
	d, url := newWindowDaemon(t, defaultInstall, newTestClock(windowStart), store)
	for round := range 5 {
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Go(func() {
				var out map[string]any
				if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": (i+round)%2 == 0}, &out); code != http.StatusOK {
					t.Errorf("patch home: code=%d %v", code, out)
				}
			})
		}
		wg.Wait()
		v, err := d.do(func(*tado.EmuState) (any, error) { return d.away, nil })
		if err != nil {
			t.Fatal(err)
		}
		stored, err := store.Away(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if stored != v.(bool) {
			t.Fatalf("round %d: stored away=%t, engine away=%t", round, stored, v)
		}
	}
}

// TestDeleteZoneForgetsLearnedState: a deleted zone leaves no heat-up model,
// early start or plant episode behind, in the engine or in the database, so a
// zone later declared under the same number starts from nothing. The other
// zones keep theirs.
func TestDeleteZoneForgetsLearnedState(t *testing.T) {
	a := newInstallAPIOn(t, defaultInstall)
	ctx := context.Background()
	model := &thermalModel{Episodes: []thermalEpisode{{Rate: 1}}}
	for _, zone := range []int{3, 5} {
		if err := a.store.SetThermalModel(ctx, zone, model); err != nil {
			t.Fatal(err)
		}
		if err := a.store.AddPlantEpisode(ctx, zone, &plantEpisode{Kind: plantCool}); err != nil {
			t.Fatal(err)
		}
	}
	d := a.d
	if _, err := d.do(func(*tado.EmuState) (any, error) {
		d.thermal[3], d.thermal[5] = model, model
		d.early[3] = earlyStart{block: scheduledBlock{c: 21}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	a.mutate(http.MethodDelete, "/api/install/zones/3", nil)

	v, err := d.do(func(*tado.EmuState) (any, error) {
		_, thermal := d.thermal[3]
		_, early := d.early[3]
		_, kept := d.thermal[5]
		return [3]bool{thermal, early, kept}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := v.([3]bool); got != [3]bool{false, false, true} {
		t.Errorf("engine zone 3 model=%t early=%t, zone 5 model=%t; want false false true", got[0], got[1], got[2])
	}
	models, err := a.store.ThermalModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := models[3]; ok || models[5] == nil {
		t.Errorf("stored models %v, want zone 5 only", models)
	}
	for zone, want := range map[int]int{3: 0, 5: 1} {
		eps, err := a.store.PlantEpisodes(ctx, zone)
		if err != nil {
			t.Fatal(err)
		}
		if len(eps) != want {
			t.Errorf("zone %d plant episodes %d, want %d", zone, len(eps), want)
		}
	}
}

// TestConcurrentHotWaterPutStoresEngineProgram: racing hot-water schedule PUTs
// leave the stored program equal to the one the engine runs.
func TestConcurrentHotWaterPutStoresEngineProgram(t *testing.T) {
	store := openTestStore(t)
	d, url := newWindowDaemon(t, defaultInstall, newTestClock(windowStart), store)
	for round := range 5 {
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Go(func() {
				body := allDayProgram(
					map[string]any{"minute": 0, "setpoint_c": 45.0},
					map[string]any{"minute": 360 + i + round, "setpoint_c": 55.0},
				)
				var out map[string]any
				if code := postJSON(t, http.MethodPut, url+"/api/zones/0/schedule", body, &out); code != http.StatusOK {
					t.Errorf("hot-water schedule: code=%d %v", code, out)
				}
			})
		}
		wg.Wait()
		v, err := d.do(func(*tado.EmuState) (any, error) {
			home, _ := d.zonePrograms(0)
			return home, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.PresetState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if stored := st.programs[st.active][0].home; !reflect.DeepEqual(stored, v.([]tado.ScheduleProgram)) {
			t.Fatalf("round %d: stored program %+v, engine program %+v", round, stored, v)
		}
	}
}

// failWrites makes every insert into table fail, or only the insert of the
// meta row key when key is set.
func failWrites(t *testing.T, store *stateStore, table, key string) {
	t.Helper()
	when := ""
	if key != "" {
		when = "WHEN NEW.key = '" + key + "'"
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_` + table + key + ` BEFORE INSERT ON ` + table + ` ` + when +
		` BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`); err != nil {
		t.Fatal(err)
	}
}

// engineValue reads one value on the engine goroutine.
func engineValue[V any](t *testing.T, d *daemon, read func() V) V {
	t.Helper()
	v, err := d.do(func(*tado.EmuState) (any, error) { return read(), nil })
	if err != nil {
		t.Fatal(err)
	}
	return v.(V)
}

// TestFailedSaveLeavesEngineUnchanged: a handler whose save fails answers 500
// and leaves the engine where it was, so a restart does not revert a change
// the caller was told failed, and the running daemon does not keep one either.
func TestFailedSaveLeavesEngineUnchanged(t *testing.T) {
	t.Run("home", func(t *testing.T) {
		store := openTestStore(t)
		d, url := newWindowDaemon(t, windowInstall(t, nil), newTestClock(windowStart), store)
		failWrites(t, store, "meta", "away")
		var out map[string]any
		if code := postJSON(t, http.MethodPatch, url+"/api/home", map[string]any{"away": true}, &out); code != http.StatusInternalServerError {
			t.Errorf("PATCH /api/home: code=%d %v, want 500", code, out)
		}
		if engineValue(t, d, func() bool { return d.away }) {
			t.Error("the engine switched to away although the save failed")
		}
	})
	t.Run("hot water schedule", func(t *testing.T) {
		store := openTestStore(t)
		d, url := newWindowDaemon(t, windowInstall(t, nil), newTestClock(windowStart), store)
		failWrites(t, store, "zone_program", "")
		var out map[string]any
		if code := postJSON(t, http.MethodPut, url+"/api/zones/0/schedule", hotWaterWeek(), &out); code != http.StatusInternalServerError {
			t.Errorf("PUT /api/zones/0/schedule: code=%d %v, want 500", code, out)
		}
		if p := engineValue(t, d, func() []tado.ScheduleProgram { home, _ := d.zonePrograms(0); return home }); p != nil {
			t.Errorf("the engine runs %+v although the save failed", p)
		}
	})
	t.Run("zone window", func(t *testing.T) {
		store := openTestStore(t)
		d, url := newWindowDaemon(t, windowInstall(t, nil), newTestClock(windowStart), store)
		failWrites(t, store, "zone_window", "")
		var out map[string]any
		if code := postJSON(t, http.MethodPost, url+"/api/zones/3/window", map[string]any{"open": true}, &out); code != http.StatusInternalServerError {
			t.Errorf("POST /api/zones/3/window: code=%d %v, want 500", code, out)
		}
		if open := engineValue(t, d, func() bool { return d.windows[3].open() }); open {
			t.Error("the engine opened zone 3 although the save failed")
		}
		if owed := engineValue(t, d, func() int { return len(d.windowOwed) }); owed != 0 {
			t.Errorf("the engine owes %d window writes although the save failed", owed)
		}
	})
	t.Run("outdoor", func(t *testing.T) {
		store := openTestStore(t)
		in := windowInstall(t, func(in *tado.Install) {
			in.Options.FlowCurves = map[int]tado.FlowCurve{1: testCurve}
		})
		clock := newTestClock(windowStart)
		_, url := newWindowDaemon(t, in, clock, store)
		pushOutdoor(t, url, 0)
		clock.advance(30 * time.Minute)
		failWrites(t, store, "meta", "save_seq")
		var out map[string]any
		if code := postJSON(t, http.MethodPost, url+"/api/outdoor", map[string]any{"tempC": 0}, &out); code != http.StatusInternalServerError {
			t.Errorf("POST /api/outdoor: code=%d %v, want 500", code, out)
		}
	})
}
