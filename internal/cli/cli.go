// Package cli holds the flag and config plumbing shared by the freedo binaries.
package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"freedo/tado"
)

// defaultConfigPath is the bootstrap config the binaries read when --config is
// absent.
const defaultConfigPath = "freedo.yaml"

// ConfigFlag registers the bootstrap config on a flag set. The installation
// itself comes from the database the bootstrap names.
func ConfigFlag(fs *flag.FlagSet) {
	fs.String("config", defaultConfigPath,
		"bootstrap config: listen address, serial port, database (see freedo.example.yaml)")
}

// PrintWarnings prints every installation warning on stderr.
func PrintWarnings(in *tado.Install) {
	for _, w := range in.Warnings() {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
}

// SetFlags returns the flags the operator passed explicitly, so a config value
// only fills in what the command line left alone.
func SetFlags(fs *flag.FlagSet) map[string]bool {
	out := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { out[f.Name] = true })
	return out
}

// ParseFlags parses flags that may appear before or after positional args (the
// stdlib flag package stops at the first positional). Returns the positionals.
func ParseFlags(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			os.Exit(2)
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	return pos
}

// Check exits with the error when err is not nil.
func Check(err error) {
	if err != nil {
		Die("%v", err)
	}
}

// Die prints the message on stderr and exits with status 1.
func Die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	os.Exit(1)
}

func lastToken(line string) string {
	line = strings.TrimSpace(line)
	if i := strings.LastIndexAny(line, ": "); i >= 0 {
		return strings.TrimSpace(line[i+1:])
	}
	return line
}
