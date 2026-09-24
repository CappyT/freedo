package main

import (
	"bytes"
	"flag"
	"path/filepath"
	"reflect"
	"testing"

	"freedo/internal/install"
)

// The tools take the installation from the database the bootstrap names. The
// YAML they used to read holds the bootstrap keys alone.
func TestMustInstallReadsTheDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := install.WriteInstall(path, fixtureInstall); err != nil {
		t.Fatalf("WriteInstall: %v", err)
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	configFlag(fs)
	stateFlag(fs)
	if err := fs.Parse([]string{"--state", path}); err != nil {
		t.Fatal(err)
	}

	in := mustInstall(fs)
	if len(in.Devices) != len(fixtureInstall.Devices) || len(in.Zones) != len(fixtureInstall.Zones) {
		t.Errorf("devices = %d, zones = %d, want %d and %d",
			len(in.Devices), len(in.Zones), len(fixtureInstall.Devices), len(fixtureInstall.Zones))
	}
	if !bytes.Equal(in.NetworkKey(), fixtureInstall.NetworkKey()) {
		t.Error("the network key did not survive the round trip")
	}
	if !bytes.Equal(in.CoordinatorEUI(), fixtureInstall.CoordinatorEUI()) || in.HomeID != fixtureInstall.HomeID {
		t.Errorf("identity = %x/%d, want %x/%d",
			in.CoordinatorEUI(), in.HomeID, fixtureInstall.CoordinatorEUI(), fixtureInstall.HomeID)
	}
	// The fixture carries the room labels of the capture, duplicates included,
	// so the round trip has to report those and invent nothing else.
	if got, want := in.Warnings(), fixtureInstall.Warnings(); !reflect.DeepEqual(got, want) {
		t.Errorf("warnings = %v, want %v", got, want)
	}
}
