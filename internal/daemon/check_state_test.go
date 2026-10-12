package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"freedo/internal/install"
)

const daemonCheckStateHelper = "FREEDO_TEST_DAEMON_CHECK_STATE_HELPER"

// --check-state maps the schema of the file to an exit status for the add-on
// start script: 0 for a missing or readable database, 3 for a newer schema.
func TestDaemonCheckStateCLI(t *testing.T) {
	if os.Getenv(daemonCheckStateHelper) == "1" {
		Main(daemonHelperArgs())
		return
	}
	dir := t.TempDir()

	missing := filepath.Join(dir, "absent.db")

	current := filepath.Join(dir, "current.db")
	store, err := install.Open(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	older := filepath.Join(dir, "older.db")
	if err := legacyDatabase(t, older, "1").Close(); err != nil {
		t.Fatal(err)
	}

	newer := filepath.Join(dir, "newer.db")
	if err := legacyDatabase(t, newer, "8").Close(); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name string
		path string
		want int
	}{
		{"missing", missing, 0},
		{"current", current, 0},
		{"older", older, 0},
		{"newer", newer, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonCheckStateCLI$")
			cmd.Dir = dir
			cmd.Env = helperEnv(daemonCheckStateHelper+"=1", "FREEDO_TEST_DAEMON_ARGS=--check-state\n"+tt.path)
			out, err := cmd.CombinedOutput()
			got := 0
			if ee, ok := err.(*exec.ExitError); ok {
				got = ee.ExitCode()
			} else if err != nil {
				t.Fatalf("run --check-state: %v\n%s", err, out)
			}
			if got != tt.want {
				t.Errorf("--check-state exit = %d, want %d\n%s", got, tt.want, out)
			}
			if tt.want == 3 && !strings.Contains(string(out), "newer") {
				t.Errorf("the refusal does not say the schema is newer:\n%s", out)
			}
		})
	}
}
