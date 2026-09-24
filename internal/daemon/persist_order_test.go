package daemon

// persist_order_test: a handler that changes engine state stores the value the
// engine took, in the order the engine took it, however the requests race.

import (
	"context"
	"net/http"
	"reflect"
	"sync"
	"testing"

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
		v, err := d.do(func(*tado.EmuState) (any, error) { return d.dhwProgram, nil })
		if err != nil {
			t.Fatal(err)
		}
		stored, err := store.HotWaterProgram(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(stored, v.([]tado.ScheduleProgram)) {
			t.Fatalf("round %d: stored program %+v, engine program %+v", round, stored, v)
		}
	}
}
