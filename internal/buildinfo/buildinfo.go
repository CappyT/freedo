// Package buildinfo carries the version stamp the --version flag prints.
package buildinfo

import (
	"fmt"
	"runtime/debug"
)

// A release build sets these through
// -ldflags "-X freedo/internal/buildinfo.Version=...".
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// unset marks a value that no linker flag replaced.
const unset = "unknown"

// Stamp is the one-line build stamp of the named binary.
func Stamp(prog string) string {
	var settings []debug.BuildSetting
	if bi, ok := debug.ReadBuildInfo(); ok {
		settings = bi.Settings
	}
	return stamp(prog, Version, Commit, Date, settings)
}

// stamp formats the build stamp. A build without the linker flags falls back to
// the vcs stamp that the Go toolchain embeds when it builds from a git tree.
func stamp(prog, version, commit, date string, settings []debug.BuildSetting) string {
	if commit == unset {
		if rev := setting(settings, "vcs.revision"); rev != "" {
			commit = rev
			if setting(settings, "vcs.modified") == "true" {
				commit += "-dirty"
			}
		}
		if t := setting(settings, "vcs.time"); t != "" && date == unset {
			date = t
		}
	}
	return fmt.Sprintf("%s %s (%s %s)", prog, version, commit, date)
}

// setting reads one build setting; an absent key returns the empty string.
func setting(settings []debug.BuildSetting, key string) string {
	for _, s := range settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}
