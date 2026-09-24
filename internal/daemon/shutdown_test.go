package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"freedo/internal/radio"
	"freedo/tado"
)

// The budgets of the shutdown path add up under the backstop, and the backstop
// itself stays under the 10 s `docker stop` grants before SIGKILL.
func TestShutdownBudgetsFitTheContainerGrace(t *testing.T) {
	const dockerStopGrace = 10 * time.Second
	sum := httpDrainBudget + stateSaveBudget + radioCloseBudget + stateCloseBudget
	if sum >= shutdownBudget {
		t.Errorf("the shutdown steps add up to %s, want less than the %s backstop", sum, shutdownBudget)
	}
	if shutdownBudget >= dockerStopGrace {
		t.Errorf("the backstop is %s, want less than the %s `docker stop` grace", shutdownBudget, dockerStopGrace)
	}
}

// A close that never returns must not hold the shutdown: closeWithin gives up
// at its budget and says so.
func TestCloseWithinGivesUpAtItsBudget(t *testing.T) {
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })

	var warn bytes.Buffer
	start := time.Now()
	closeWithinTo(&warn, "the stuck thing", 50*time.Millisecond, func() error {
		<-stuck
		return nil
	})
	elapsed := time.Since(start)
	if elapsed < 50*time.Millisecond {
		t.Errorf("closeWithin returned after %s, before its budget", elapsed)
	}
	if elapsed > time.Second {
		t.Errorf("closeWithin waited %s on a close that never returns", elapsed)
	}
	if !strings.Contains(warn.String(), "the stuck thing") ||
		!strings.Contains(warn.String(), "did not finish") {
		t.Errorf("warning=%q", warn.String())
	}

	// A close that answers returns at once and warns only about its error.
	warn.Reset()
	start = time.Now()
	closeWithinTo(&warn, "the fast thing", time.Minute, func() error { return nil })
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a fast close took %s", elapsed)
	}
	if warn.Len() != 0 {
		t.Errorf("a clean close warned: %q", warn.String())
	}
	warn.Reset()
	closeWithinTo(&warn, "the failing thing", time.Minute, func() error { return errors.New("boom") })
	if !strings.Contains(warn.String(), "boom") {
		t.Errorf("warning=%q", warn.String())
	}
}

// A deadline already spent buys nothing: the save reports the busy engine and
// writes nothing at all.
func TestSaveBeforeExpiredDeadlineWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	d.state = newTestStore(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	t.Cleanup(cancel)

	if err := d.saveDesiredStateBefore(time.Now().Add(-time.Millisecond)); !errors.Is(err, errEngineBusy) {
		t.Fatalf("save=%v, want %v", err, errEngineBusy)
	}
	if d.saveSeq != 0 {
		t.Fatalf("saveSeq=%d, want the export skipped", d.saveSeq)
	}
	var seq int
	if err := d.state.db.QueryRow(
		`SELECT count(*) FROM meta WHERE key = 'save_seq'`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq != 0 {
		t.Fatalf("the expired save reached the database")
	}
}

// The engine hop and the write share one deadline. The hop here eats most of
// the budget and the write blocks on the single database connection, so the
// whole call still ends at the deadline instead of starting a second budget.
func TestSaveBeforeSharesOneDeadlineAcrossHopAndWrite(t *testing.T) {
	const budget = 400 * time.Millisecond
	const hop = 300 * time.Millisecond

	path := filepath.Join(t.TempDir(), "state.db")
	emu := tado.NewEmu(defaultInstall.EmuConfig(), tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")
	store := newTestStore(t, path)
	d.state = store
	ctx, cancel := context.WithCancel(context.Background())
	go d.run(ctx)
	t.Cleanup(cancel)

	// The store keeps one connection, so an open transaction holds every writer
	// out until its context expires.
	held, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback()

	busy := make(chan struct{})
	go func() {
		_, _ = d.do(func(*tado.EmuState) (any, error) {
			close(busy)
			time.Sleep(hop)
			return nil, nil
		})
	}()
	<-busy

	start := time.Now()
	err = d.saveDesiredStateBefore(start.Add(budget))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("the blocked write reported success")
	}
	if elapsed > budget+150*time.Millisecond {
		t.Fatalf("the save took %s, want at most the %s deadline: the write started a second budget", elapsed, budget)
	}
	if elapsed < hop {
		t.Fatalf("the save took %s, less than the %s engine hop", elapsed, hop)
	}
}
