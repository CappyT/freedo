package main

import (
	"io"
	"testing"
	"time"

	"freedo/tado"
)

func TestEmulateDryRunReleasesRouterAdvert(t *testing.T) {
	emu := tado.NewEmu(tado.EmuConfig{RouterAdvertInterval: time.Minute}, tado.NewUnhandledLogger(io.Discard))
	actions, due, err := emu.MaybeBeacon()
	if err != nil || !due || len(actions) != tado.RouterAdvertWakeUpCount+1 {
		t.Fatalf("first dry-run beacon: actions=%d due=%v err=%v", len(actions), due, err)
	}
	completeDryRunBeacon(emu, false)
	actions, due, err = emu.MaybeBeacon()
	if err != nil || !due || len(actions) != tado.RouterAdvertWakeUpCount+1 {
		t.Fatalf("dry-run left beacon pending: actions=%d due=%v err=%v", len(actions), due, err)
	}
	emu.CompleteBeacon(false)
}
