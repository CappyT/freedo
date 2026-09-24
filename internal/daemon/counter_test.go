package daemon

// counter_test: the downlink frame counter and the CoAP Message ID survive a
// restart, so no CCM* nonce and no Message ID repeats.

import (
	"context"
	"errors"
	"testing"
	"time"

	"freedo/internal/install"
	"freedo/tado"
)

// buildFrame queues a child lock write to target and builds its frame. It
// returns the frame counter the frame took.
func buildFrame(t *testing.T, d *daemon, target string) uint32 {
	t.Helper()
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		if err := e.EnqueueChildLock(target, 3, true); err != nil {
			return nil, err
		}
		if _, ok, err := e.NextQueuedDownlink(target); err != nil || !ok {
			return nil, errors.Join(err, errors.New("no frame built"))
		}
		e.DropQueue(target)
		return e.TxCounter(), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return v.(uint32)
}

func metaUint(t *testing.T, store *stateStore, key string) (uint64, bool) {
	t.Helper()
	v, found, err := install.ReadMetaUint(context.Background(), store.db, key)
	if err != nil {
		t.Fatal(err)
	}
	return v, found
}

// TestTxCounterResumesAboveUsed: a transmitting daemon stores a resume point
// above the counters it uses before its first frame, and a restart resumes
// from it, not from the configured seed. A higher seed stays a floor.
func TestTxCounterResumesAboveUsed(t *testing.T) {
	path := t.TempDir() + "/state.db"
	in := windowInstall(t, nil)
	valve := wireOf(t, in, "VA1000000002")
	clock := newTestClock(windowStart)
	d, _, stop := startDaemonAs(t, in, clock, newTestStore(t, path), false, "live", 0)
	var used uint32
	for range 3 {
		used = buildFrame(t, d, valve)
	}
	stop()

	d, _, stop = startDaemonAs(t, in, clock, newTestStore(t, path), false, "live", 0)
	if got := buildFrame(t, d, valve); got <= used {
		t.Fatalf("the first frame after the restart took counter %d, want above %d", got, used)
	}
	stop()

	store := newTestStore(t, path)
	d, _, _ = startDaemonAs(t, in, clock, store, false, "live", 1_000_000)
	if got := buildFrame(t, d, valve); got != 1_000_001 {
		t.Errorf("a configured seed above the resume point: the first frame took %d, want 1000001", got)
	}
	if v, _ := metaUint(t, store, metaTxResume); v != 1_000_000+counterLease {
		t.Errorf("stored resume point = %d, want %d", v, 1_000_000+counterLease)
	}
}

// TestTxCounterLeaseMovesAhead: the tick stores a new resume point once less
// than half a lease is left, and not before.
func TestTxCounterLeaseMovesAhead(t *testing.T) {
	path := t.TempDir() + "/state.db"
	in := windowInstall(t, nil)
	store := newTestStore(t, path)
	d, _, _ := startDaemonAs(t, in, newTestClock(windowStart), store, false, "live", 0)
	first, _ := metaUint(t, store, metaTxResume)
	for _, tc := range []struct {
		counter uint32
		want    uint64
	}{
		{uint32(first) - counterLease/2, first},
		{uint32(first) - counterLease/2 + 1, first + 1 + counterLease/2},
		{uint32(first) + 10_000, first + 10_000 + counterLease},
	} {
		if _, err := d.do(func(e *tado.EmuState) (any, error) {
			e.SetTxCounter(tc.counter)
			d.tickCounters(e)
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		if v, _ := metaUint(t, store, metaTxResume); v != tc.want {
			t.Errorf("counter %d: stored resume point = %d, want %d", tc.counter, v, tc.want)
		}
	}
}

// TestDryRunReadsCountersStoresNone: a dry-run resumes from the stored
// counters and the stored Message ID, and stores nothing: it puts no frame on the air.
func TestDryRunReadsCountersStoresNone(t *testing.T) {
	saved := tado.LastMID()
	defer tado.SeedMID(saved)
	path := t.TempDir() + "/state.db"
	store := newTestStore(t, path)
	resume := map[string]uint64{metaTxResume: 5000, metaBcastResume: 7000, metaMIDResume: 0x1234}
	if err := store.SetCounterResume(context.Background(), resume); err != nil {
		t.Fatal(err)
	}
	in := windowInstall(t, nil)
	valve := wireOf(t, in, "VA1000000002")
	d, _, _ := startDaemonAs(t, in, newTestClock(windowStart), store, true, "mock", 0)
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		if err := e.EnqueueChildLock(valve, 3, true); err != nil {
			return nil, err
		}
		return []uint64{uint64(e.TxCounter()), uint64(e.Queue(valve)[0].MID)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Broadcasts share the frame counter: a stored broadcast resume point is a
	// floor for it.
	if got := v.([]uint64); got[0] != 7000 || got[1] != 0x1235 {
		t.Errorf("tx=%d first MID=%#x, want 7000 and 0x1235", got[0], got[1])
	}
	for key, want := range resume {
		if got, _ := metaUint(t, store, key); got != want {
			t.Errorf("stored %s = %d, want it left at %d", key, got, want)
		}
	}
}

// TestMIDResumesPastStored: a transmitting daemon stores a Message ID midLease
// past the one in use, and a restart mints from there.
func TestMIDResumesPastStored(t *testing.T) {
	saved := tado.LastMID()
	defer tado.SeedMID(saved)
	path := t.TempDir() + "/state.db"
	store := newTestStore(t, path)
	in := windowInstall(t, nil)
	valve := wireOf(t, in, "VA1000000002")
	inUse := uint16(0xfff0)
	stored := inUse + midLease
	tado.SeedMID(inUse)
	_, _, stop := startDaemonAs(t, in, newTestClock(windowStart), store, false, "live", 0)
	stop()
	if got, _ := metaUint(t, store, metaMIDResume); got != uint64(stored) {
		t.Fatalf("stored MID = %#x, want %#x", got, stored)
	}
	tado.SeedMID(0)
	d, _, _ := startDaemonAs(t, in, newTestClock(windowStart.Add(time.Minute)), newTestStore(t, path), false, "live", 0)
	v, err := d.do(func(e *tado.EmuState) (any, error) {
		if err := e.EnqueueChildLock(valve, 3, true); err != nil {
			return nil, err
		}
		return e.Queue(valve)[0].MID, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := stored + 1; v.(uint16) != want {
		t.Errorf("first MID after the restart = %#x, want %#x", v, want)
	}
}
