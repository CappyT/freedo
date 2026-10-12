package daemon

import (
	"io"
	"testing"

	"freedo/internal/radio"
	"freedo/tado"
)

// TestFwReadAfterStartQueuesOneRead: with an armed radio the daemon queues a GET
// d/fw/state for a declared device that holds no report yet.
func TestFwReadAfterStartQueuesOneRead(t *testing.T) {
	cfg := defaultInstall.EmuConfig()
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, newImmediateTestRadio(true, nil), false, "live")

	d.tickFwRead(emu)
	if len(d.dispatchQ) != 1 {
		t.Fatalf("dispatchQ = %v, want one read", d.dispatchQ)
	}
	queued := emu.Queue(d.dispatchQ[0])
	if len(queued) != 1 || queued[0].Desc != "get d/fw/state" {
		t.Fatalf("queued %+v, want one get d/fw/state", queued)
	}
	// One read per device per run: the next call queues nothing until the spacing
	// elapses.
	d.dispatchQ = nil
	d.tickFwRead(emu)
	if len(d.dispatchQ) != 0 {
		t.Fatalf("second tick queued %v, want none", d.dispatchQ)
	}
}

// TestFwReadNotQueuedInDryRun: a dry-run daemon queues no fw/state read.
func TestFwReadNotQueuedInDryRun(t *testing.T) {
	cfg := defaultInstall.EmuConfig()
	emu := tado.NewEmu(cfg, tado.NewUnhandledLogger(io.Discard))
	d := newDaemon(defaultInstall, emu, radio.NewMock(nil), true, "mock")

	d.tickFwRead(emu)
	if len(d.dispatchQ) != 0 {
		t.Fatalf("dry-run queued %v, want none", d.dispatchQ)
	}
	for _, dev := range defaultInstall.Devices {
		if q := emu.Queue(dev.Short()); len(q) != 0 {
			t.Fatalf("dry-run queued %+v for %s", q, dev.Serial)
		}
	}
}
