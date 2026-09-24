package main

// shim: the shared cli helpers under the names this package already uses, and
// the join point between them and the database in internal/install.
// Plain functions, so `go vet` keeps checking the printf arguments of die.

import (
	"errors"
	"flag"

	"freedo/internal/cli"
	"freedo/internal/install"
	"freedo/tado"
)

func configFlag(fs *flag.FlagSet)                   { cli.ConfigFlag(fs) }
func stateFlag(fs *flag.FlagSet)                    { cli.StateFlag(fs) }
func mustBootstrap(fs *flag.FlagSet) *cli.Bootstrap { return cli.MustBootstrap(fs) }
func setFlags(fs *flag.FlagSet) map[string]bool     { return cli.SetFlags(fs) }

// mustInstall reads the installation from the database the bootstrap names.
// The tools never commission one: only the daemon generates an identity.
func mustInstall(fs *flag.FlagSet) *tado.Install {
	b := cli.MustBootstrap(fs)
	path := cli.ResolveDatabase(fs, b)
	in, err := install.LoadInstall(path)
	switch {
	case errors.Is(err, install.ErrNoInstall):
		cli.Die("the database %s holds no installation: start the daemon once or seed it", path)
	case err != nil:
		cli.Die("%v", err)
	}
	cli.PrintWarnings(in)
	return in
}

// mustDatabase resolves the database the tools read, without loading the
// installation: a calibration writes one meta key and names no device.
func mustDatabase(fs *flag.FlagSet) string {
	return cli.ResolveDatabase(fs, cli.MustBootstrap(fs))
}

// storeCalibration writes the passive receiver calibration into that database.
func storeCalibration(path string, value byte) {
	cli.Check(install.StoreFSCTRL0(path, value))
}

func parseFlags(fs *flag.FlagSet, args []string) []string { return cli.ParseFlags(fs, args) }

func check(err error)                      { cli.Check(err) }
func die(format string, a ...any)          { cli.Die(format, a...) }
func replayHex(line string) (string, bool) { return cli.ReplayHex(line) }
