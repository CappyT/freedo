package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestStampFormat(t *testing.T) {
	vcs := []debug.BuildSetting{
		{Key: "vcs", Value: "git"},
		{Key: "vcs.revision", Value: "abc1234"},
		{Key: "vcs.time", Value: "2026-01-02T03:04:05Z"},
		{Key: "vcs.modified", Value: "false"},
	}
	dirty := []debug.BuildSetting{
		{Key: "vcs.revision", Value: "abc1234"},
		{Key: "vcs.time", Value: "2026-01-02T03:04:05Z"},
		{Key: "vcs.modified", Value: "true"},
	}
	cases := []struct {
		name                  string
		prog                  string
		version, commit, date string
		settings              []debug.BuildSetting
		want                  string
	}{
		{
			name: "linker flags win over the vcs stamp",
			prog: "freedo", version: "1.2.3", commit: "deadbee", date: "2026-02-03",
			settings: vcs,
			want:     "freedo 1.2.3 (deadbee 2026-02-03)",
		},
		{
			name: "vcs stamp fills in the defaults",
			prog: "freedo", version: "dev", commit: unset, date: unset,
			settings: vcs,
			want:     "freedo dev (abc1234 2026-01-02T03:04:05Z)",
		},
		{
			name: "a modified tree is dirty",
			prog: "freedo-dev", version: "dev", commit: unset, date: unset,
			settings: dirty,
			want:     "freedo-dev dev (abc1234-dirty 2026-01-02T03:04:05Z)",
		},
		{
			name: "no stamp at all keeps the defaults",
			prog: "freedo", version: "dev", commit: unset, date: unset,
			settings: nil,
			want:     "freedo dev (unknown unknown)",
		},
		{
			name: "a linker date survives the vcs fallback",
			prog: "freedo", version: "dev", commit: unset, date: "2026-02-03",
			settings: vcs,
			want:     "freedo dev (abc1234 2026-02-03)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stamp(tc.prog, tc.version, tc.commit, tc.date, tc.settings)
			if got != tc.want {
				t.Errorf("stamp = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStampUsesThePackageVariables(t *testing.T) {
	got := Stamp("freedo")
	if got == "" {
		t.Fatal("Stamp returned an empty string")
	}
	if want := "freedo " + Version + " ("; len(got) < len(want) || got[:len(want)] != want {
		t.Errorf("Stamp = %q, want the prefix %q", got, want)
	}
}
