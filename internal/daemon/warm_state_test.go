package daemon

// warm_state_test: the controller of an owned zone and the boiler monitor tags
// survive a short restart on the same database.

import (
	"testing"
	"time"

	"freedo/tado"
)

func ownedInstall(t *testing.T, owned bool) *tado.Install {
	return windowInstall(t, func(in *tado.Install) {
		if owned {
			in.Options.ZoneOwners = map[int]string{3: tado.ZoneOwnerCoordinator}
		}
	})
}

// warmSeed starts a daemon on a fresh database with an owned zone 3 holding a
// controller state and a boiler that reported a flow of 40 C, and saves it.
func warmSeed(t *testing.T) (*stateStore, *testClock) {
	t.Helper()
	a := newInstallAPIOn(t, ownedInstall(t, true))
	clock := newTestClock(windowStart)
	d := newEnergyDaemon(t, ownedInstall(t, true), clock, a.store)
	boilerReport(t, d, 1, tado.TLV{Tag: "044c", Val: []byte{0x0f, 0xa0}})
	d.zoneOwners[3] = &zoneOwner{
		integ: 750, tempC: 18.5, source: "external", pct: 42, haveOut: true,
		hist:     []ownerSample{{at: clock.now().Add(-time.Minute), c: 18.4}, {at: clock.now(), c: 18.5}},
		evalAt:   clock.now(),
		pushedAt: clock.now(),
	}
	if err := d.saveWarmState(d.emu); err != nil {
		t.Fatal(err)
	}
	return a.store, clock
}

func flowOf(d *daemon) *float64 { return d.emu.Snapshot().Boiler.FlowC }

// TestWarmStateRestore: a start within warmStateMaxAge brings back the
// controller and the unchanged boiler flow, and leaves the first tick to step
// and push.
func TestWarmStateRestore(t *testing.T) {
	store, clock := warmSeed(t)
	clock.advance(warmStateMaxAge)
	d := newEnergyDaemon(t, ownedInstall(t, true), clock, store)
	if flowOf(d) != nil {
		t.Fatal("a fresh daemon already knows the flow")
	}
	if err := d.restoreWarmState(d.emu); err != nil {
		t.Fatal(err)
	}
	x := d.zoneOwners[3]
	if x == nil || x.integ != 750 || x.tempC != 18.5 || x.source != "external" || x.pct != 42 || !x.haveOut || len(x.hist) != 2 {
		t.Fatalf("restored controller = %+v, want integ 750, 18.5 C external, 42 %%, 2 samples", x)
	}
	if !x.evalAt.IsZero() || !x.pushedAt.IsZero() {
		t.Errorf("evalAt %v pushedAt %v, want zero so the first tick steps and pushes", x.evalAt, x.pushedAt)
	}
	if f := flowOf(d); f == nil || *f != 40 {
		t.Errorf("restored flow = %v, want 40", f)
	}
}

// TestWarmStateTooOld: a save older than warmStateMaxAge restores nothing.
func TestWarmStateTooOld(t *testing.T) {
	store, clock := warmSeed(t)
	clock.advance(warmStateMaxAge + time.Second)
	d := newEnergyDaemon(t, ownedInstall(t, true), clock, store)
	if err := d.restoreWarmState(d.emu); err != nil {
		t.Fatal(err)
	}
	if len(d.zoneOwners) != 0 || flowOf(d) != nil {
		t.Errorf("owners %v flow %v, want nothing restored", d.zoneOwners, flowOf(d))
	}
}

// TestWarmStateDropsUnownedZone: a zone the coordinator no longer owns loses
// its controller state, and the boiler tags still come back.
func TestWarmStateDropsUnownedZone(t *testing.T) {
	store, clock := warmSeed(t)
	clock.advance(time.Minute)
	d := newEnergyDaemon(t, ownedInstall(t, false), clock, store)
	if err := d.restoreWarmState(d.emu); err != nil {
		t.Fatal(err)
	}
	if len(d.zoneOwners) != 0 {
		t.Errorf("owners = %v, want none for a zone the device owns", d.zoneOwners)
	}
	if f := flowOf(d); f == nil || *f != 40 {
		t.Errorf("flow = %v, want 40", f)
	}
}

// TestWarmStateKeepsFreshTag: a report that arrived before the restore wins,
// and the tags it did not carry come back.
func TestWarmStateKeepsFreshTag(t *testing.T) {
	store, clock := warmSeed(t)
	clock.advance(time.Minute)
	d := newEnergyDaemon(t, ownedInstall(t, true), clock, store)
	boilerReport(t, d, 2, tado.TLV{Tag: "044c", Val: []byte{0x0b, 0xb8}})
	if err := d.restoreWarmState(d.emu); err != nil {
		t.Fatal(err)
	}
	if f := flowOf(d); f == nil || *f != 30 {
		t.Errorf("flow = %v, want the fresh 30 and not the stored 40", f)
	}
}

// TestWarmStateDowntimeAddsNoEnergy: a burner restored on at 50 % of 24 kW
// counts from the start only, not across the two minutes of the stop.
func TestWarmStateDowntimeAddsNoEnergy(t *testing.T) {
	in := func() *tado.Install {
		return windowInstall(t, func(in *tado.Install) { in.Options.BoilerMaxPowerKW = new(24.0) })
	}
	a := newInstallAPIOn(t, in())
	clock := newTestClock(windowStart)
	before := newEnergyDaemon(t, in(), clock, a.store)
	boilerReport(t, before, 1, modFlag(50, 1)...)
	if err := before.saveWarmState(before.emu); err != nil {
		t.Fatal(err)
	}

	clock.advance(2 * time.Minute)
	d := newEnergyDaemon(t, in(), clock, a.store)
	if err := d.restoreWarmState(d.emu); err != nil {
		t.Fatal(err)
	}
	d.noteBoiler(d.emu)
	clock.advance(30 * time.Second)
	boilerReport(t, d, 2, modFlag(50, 1)...)
	near(t, "energy of 30 s at 12 kW after the start", d.energy.totalKWh, 12*30.0/3600)
}

// TestWarmStateUnreadable: a corrupt row or a save in the future restores
// nothing and does not stop the start.
func TestWarmStateUnreadable(t *testing.T) {
	store, clock := warmSeed(t)
	clock.advance(-time.Minute)
	d := newEnergyDaemon(t, ownedInstall(t, true), clock, store)
	if err := d.restoreWarmState(d.emu); err != nil {
		t.Fatal(err)
	}
	if len(d.zoneOwners) != 0 || flowOf(d) != nil {
		t.Errorf("save in the future: owners %v flow %v, want nothing restored", d.zoneOwners, flowOf(d))
	}

	if _, err := store.db.ExecContext(t.Context(), `UPDATE meta SET value = '{"savedMs":' WHERE key = 'warm_state'`); err != nil {
		t.Fatal(err)
	}
	clock.advance(2 * time.Minute)
	d = newEnergyDaemon(t, ownedInstall(t, true), clock, store)
	if err := d.restoreWarmState(d.emu); err != nil {
		t.Errorf("corrupt row: %v, want the start to go on", err)
	}
	if len(d.zoneOwners) != 0 {
		t.Errorf("corrupt row: owners %v, want none", d.zoneOwners)
	}
}

// TestWarmStateTickEvery: the tick saves at most once per warmStateEvery.
func TestWarmStateTickEvery(t *testing.T) {
	a := newInstallAPIOn(t, ownedInstall(t, true))
	clock := newTestClock(windowStart)
	d := newEnergyDaemon(t, ownedInstall(t, true), clock, a.store)
	saved := func() int64 {
		s, err := a.store.WarmState(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return s.SavedMs
	}
	d.tickWarmState(d.emu)
	first := saved()
	if first != windowStart.UnixMilli() {
		t.Fatalf("first save at %d, want %d", first, windowStart.UnixMilli())
	}
	clock.advance(warmStateEvery - time.Second)
	d.tickWarmState(d.emu)
	if saved() != first {
		t.Error("the tick saved again within warmStateEvery")
	}
	clock.advance(time.Second)
	d.tickWarmState(d.emu)
	if saved() != clock.now().UnixMilli() {
		t.Error("the tick did not save after warmStateEvery")
	}
}
