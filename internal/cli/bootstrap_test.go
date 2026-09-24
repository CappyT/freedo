package cli

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeBootstrap(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// flagsFor returns a parsed flag set carrying --config and --state.
func flagsFor(t *testing.T, args ...string) *flag.FlagSet {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	ConfigFlag(fs)
	StateFlag(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return fs
}

const fullBootstrap = `
version: 2
serial_port: /dev/ttyACM0
http_listen: "127.0.0.1:9090"
database: /var/lib/freedo/state.db
unhandled_log: /var/log/freedo.log
api_token_file: /run/secrets/freedo_api_token
debug_api: true
csl_phase: 7
tx_counter: 4242
zone_get_proxy: true
zone_proxy_timeout_sec: 25
radio_tune: true
beacon_sec: 60
`

func TestLoadBootstrap(t *testing.T) {
	path := writeBootstrap(t, t.TempDir(), "freedo.yaml", fullBootstrap)
	b, err := LoadBootstrap(path, true)
	if err != nil {
		t.Fatalf("LoadBootstrap: %v", err)
	}
	if b.Version != BootstrapVersion {
		t.Errorf("version = %d, want %d", b.Version, BootstrapVersion)
	}
	if b.SerialPort != "/dev/ttyACM0" || b.HTTPListen != "127.0.0.1:9090" {
		t.Errorf("radio and listen = %q %q", b.SerialPort, b.HTTPListen)
	}
	if b.Database != "/var/lib/freedo/state.db" || b.UnhandledLog != "/var/log/freedo.log" {
		t.Errorf("paths = %q %q", b.Database, b.UnhandledLog)
	}
	if b.APITokenFile != "/run/secrets/freedo_api_token" {
		t.Errorf("api_token_file = %q", b.APITokenFile)
	}
	if !b.DebugAPI || !b.ZoneGetProxy {
		t.Errorf("debug_api = %v, zone_get_proxy = %v, want both true", b.DebugAPI, b.ZoneGetProxy)
	}
	if w := b.Warnings(); len(w) != 1 || !strings.Contains(w[0], "radio_tune is ignored") {
		t.Errorf("warnings = %q, want one that names the ignored radio_tune", w)
	}
	if b.CSLPhase != 7 {
		t.Errorf("csl_phase = %d, want 7", b.CSLPhase)
	}
	if b.TxCounterSeed() != 4242 {
		t.Errorf("tx_counter = %d, want 4242", b.TxCounterSeed())
	}
	if got := b.ZoneProxyTimeout(); got != 25*time.Second {
		t.Errorf("zone_proxy_timeout = %v, want 25s", got)
	}
	if got := b.BeaconInterval(); got != time.Minute {
		t.Errorf("beacon_sec = %v, want 1m", got)
	}
}

// The beacon holds the devices in the network, so an unset beacon_sec takes the
// period of the commercial coordinator. Only an explicit 0 sends no beacon.
func TestBootstrapBeaconInterval(t *testing.T) {
	dir := t.TempDir()
	path := writeBootstrap(t, dir, "freedo.yaml", "version: 2\n")
	b, err := LoadBootstrap(path, true)
	if err != nil {
		t.Fatalf("LoadBootstrap: %v", err)
	}
	if b.BeaconSec != nil || b.BeaconInterval() != DefaultBeaconSec*time.Second {
		t.Errorf("unset beacon_sec = %v, want %ds", b.BeaconInterval(), DefaultBeaconSec)
	}
	path = writeBootstrap(t, dir, "off.yaml", "version: 2\nbeacon_sec: 0\n")
	b, err = LoadBootstrap(path, true)
	if err != nil {
		t.Fatalf("LoadBootstrap: %v", err)
	}
	if b.BeaconInterval() != 0 {
		t.Errorf("beacon_sec: 0 = %v, want no beacon", b.BeaconInterval())
	}
}

// A tx_counter and a zone_proxy_timeout_sec nobody wrote are not zero values:
// the counter starts at 0 and the proxy takes the emulator's own bound.
func TestLoadBootstrapUnsetPointers(t *testing.T) {
	dir := t.TempDir()
	path := writeBootstrap(t, dir, "freedo.yaml", "version: 2\n")
	b, err := LoadBootstrap(path, true)
	if err != nil {
		t.Fatalf("LoadBootstrap: %v", err)
	}
	if b.TxCounter != nil || b.TxCounterSeed() != 0 {
		t.Errorf("tx_counter = %v, want unset", b.TxCounter)
	}
	if b.ZoneProxyTimeoutSec != nil || b.ZoneProxyTimeout() != 0 {
		t.Errorf("zone_proxy_timeout_sec = %v, want unset", b.ZoneProxyTimeoutSec)
	}
	if b.HTTPListen != DefaultHTTPListen || b.UnhandledLog != filepath.Join(dir, defaultUnhandledLog) {
		t.Errorf("defaults = %q %q", b.HTTPListen, b.UnhandledLog)
	}
	if b.Database != "" {
		t.Errorf("database = %q, want empty for the caller to resolve", b.Database)
	}
	if b.APITokenFile != "" {
		t.Errorf("api_token_file = %q, want unset", b.APITokenFile)
	}
	if w := b.Warnings(); len(w) != 0 {
		t.Errorf("warnings = %q, want none", w)
	}
}

// With no API token the daemon accepts a loopback address alone, so the
// default has to be one.
func TestDefaultHTTPListenIsLoopback(t *testing.T) {
	b, err := LoadBootstrap(filepath.Join(t.TempDir(), "freedo.yaml"), false)
	if err != nil {
		t.Fatal(err)
	}
	if b.HTTPListen != "127.0.0.1:8080" {
		t.Errorf("default http_listen = %q, want 127.0.0.1:8080", b.HTTPListen)
	}
}

func TestLoadBootstrapRejectsUnknownKey(t *testing.T) {
	path := writeBootstrap(t, t.TempDir(), "freedo.yaml", "version: 2\nhttp_listem: \":80\"\n")
	_, err := LoadBootstrap(path, true)
	if err == nil || !strings.Contains(err.Error(), "http_listem") {
		t.Fatalf("error = %v, want it to name the unknown key", err)
	}
}

func TestLoadBootstrapRejectsAnotherVersion(t *testing.T) {
	for _, body := range []string{"serial_port: /dev/ttyUSB0\n", "version: 3\n"} {
		path := writeBootstrap(t, t.TempDir(), "freedo.yaml", body)
		if _, err := LoadBootstrap(path, true); err == nil {
			t.Errorf("%q loaded, want a version error", body)
		}
	}
}

// A missing file is the container start on flags alone, unless the operator
// named it: then it is a typo.
func TestLoadBootstrapMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "freedo.yaml")
	b, err := LoadBootstrap(path, false)
	if err != nil {
		t.Fatalf("LoadBootstrap on a missing file: %v", err)
	}
	if b.HTTPListen != DefaultHTTPListen || b.UnhandledLog != filepath.Join(dir, defaultUnhandledLog) {
		t.Errorf("defaults = %q %q", b.HTTPListen, b.UnhandledLog)
	}
	if got := ResolveDatabase(flagsFor(t), b); got != filepath.Join(dir, defaultDatabaseName) {
		t.Errorf("database = %q, want it beside the missing config", got)
	}
	if _, err := LoadBootstrap(path, true); err == nil {
		t.Error("an explicit --config on a missing file loaded, want an error")
	}
}

func TestLoadBootstrapRefusesTheInstallationYAML(t *testing.T) {
	dir := t.TempDir()
	path := writeBootstrap(t, dir, "freedo.yaml", `
version: 1
home_id: 4711
network_key: "000102030405060708090a0b0c0d0e0f"
network_key_file: ""
coordinator: {eui: "aa016a3107c51b00"}
devices: []
zones: []
circuits: []
options: {utc_offset_min: 60}
`)
	_, err := LoadBootstrap(path, true)
	if err == nil {
		t.Fatal("a version-1 installation config loaded as a bootstrap")
	}
	msg := err.Error()
	wants := append(append([]string{}, movedKeys...),
		"version: 1", filepath.Join(dir, defaultDatabaseName),
		"utc_offset_min", "pan_id", "/api/install", "seed script")
	for _, want := range wants {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, msg)
		}
	}
}

// A path in the file is read against the directory of the file, so the daemon
// finds the same database whatever its working directory is.
func TestLoadBootstrapResolvesRelativePaths(t *testing.T) {
	dir := t.TempDir()
	path := writeBootstrap(t, dir, "freedo.yaml",
		"version: 2\ndatabase: state.db\nunhandled_log: logs/unhandled.log\napi_token_file: secrets/token\n")
	b, err := LoadBootstrap(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "state.db"); b.Database != want {
		t.Errorf("database = %q, want %q", b.Database, want)
	}
	if want := filepath.Join(dir, "logs/unhandled.log"); b.UnhandledLog != want {
		t.Errorf("unhandled_log = %q, want %q", b.UnhandledLog, want)
	}
	if want := filepath.Join(dir, "secrets/token"); b.APITokenFile != want {
		t.Errorf("api_token_file = %q, want %q", b.APITokenFile, want)
	}

	abs := writeBootstrap(t, dir, "abs.yaml", "version: 2\ndatabase: /srv/freedo.db\n")
	b, err = LoadBootstrap(abs, true)
	if err != nil {
		t.Fatal(err)
	}
	if b.Database != "/srv/freedo.db" {
		t.Errorf("absolute database = %q, want it untouched", b.Database)
	}
}

func TestResolveDatabasePrecedence(t *testing.T) {
	dir := t.TempDir()
	named := writeBootstrap(t, dir, "named.yaml", "version: 2\ndatabase: named.db\n")
	bare := writeBootstrap(t, dir, "bare.yaml", "version: 2\n")

	load := func(path string) *Bootstrap {
		t.Helper()
		b, err := LoadBootstrap(path, true)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, tc := range []struct {
		name string
		args []string
		b    *Bootstrap
		want string
	}{
		{"--state wins", []string{"--state", "/tmp/flag.db"}, load(named), "/tmp/flag.db"},
		{"then the file", nil, load(named), filepath.Join(dir, "named.db")},
		{"then beside the config", nil, load(bare), filepath.Join(dir, defaultDatabaseName)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveDatabase(flagsFor(t, tc.args...), tc.b); got != tc.want {
				t.Errorf("ResolveDatabase = %q, want %q", got, tc.want)
			}
		})
	}
	// With no --config either, the database sits in the working directory.
	b, err := LoadBootstrap(defaultConfigPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := ResolveDatabase(flagsFor(t), b); got != defaultDatabaseName {
		t.Errorf("ResolveDatabase with no config = %q, want %q", got, defaultDatabaseName)
	}
}

// The shipped example is a bootstrap now, not an installation.
func TestExampleConfigIsABootstrap(t *testing.T) {
	const path = "../../freedo.example.yaml"
	b, err := LoadBootstrap(path, true)
	if err != nil {
		t.Fatalf("LoadBootstrap(%s): %v", path, err)
	}
	if b.Version != BootstrapVersion {
		t.Errorf("version = %d, want %d", b.Version, BootstrapVersion)
	}
	if b.HTTPListen != DefaultHTTPListen || b.SerialPort != "/dev/ttyUSB0" {
		t.Errorf("listen = %q, serial = %q", b.HTTPListen, b.SerialPort)
	}
	if want := filepath.Join("../..", defaultDatabaseName); b.Database != want {
		t.Errorf("database = %q, want %q", b.Database, want)
	}
	if b.DebugAPI {
		t.Error("the example enables the debug API")
	}
	if b.APITokenFile != "" {
		t.Errorf("api_token_file = %q, want the example to leave it commented out", b.APITokenFile)
	}
	if got := b.BeaconInterval(); got != DefaultBeaconSec*time.Second {
		t.Errorf("beacon = %v, want %ds", got, DefaultBeaconSec)
	}
}
