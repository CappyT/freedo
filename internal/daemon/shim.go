package daemon

// shim: the shared cli helpers under the names this package already uses.
// Plain functions, so `go vet` keeps checking the printf arguments of die.

import (
	"flag"
	"time"

	"freedo/internal/cli"
)

// defaultBeaconInterval keeps the flag default and the bootstrap default on
// the same value.
const defaultBeaconInterval = cli.DefaultBeaconSec * time.Second

// defaultHTTPListen does the same for the API address.
const defaultHTTPListen = cli.DefaultHTTPListen

func configFlag(fs *flag.FlagSet)                   { cli.ConfigFlag(fs) }
func stateFlag(fs *flag.FlagSet)                    { cli.StateFlag(fs) }
func mustBootstrap(fs *flag.FlagSet) *cli.Bootstrap { return cli.MustBootstrap(fs) }
func setFlags(fs *flag.FlagSet) map[string]bool     { return cli.SetFlags(fs) }

func resolveDatabase(fs *flag.FlagSet, b *cli.Bootstrap) string { return cli.ResolveDatabase(fs, b) }

func parseFlags(fs *flag.FlagSet, args []string) []string { return cli.ParseFlags(fs, args) }

func check(err error)                      { cli.Check(err) }
func die(format string, a ...any)          { cli.Die(format, a...) }
func replayHex(line string) (string, bool) { return cli.ReplayHex(line) }
