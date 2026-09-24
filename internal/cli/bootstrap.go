package cli

// bootstrap: the flat process configuration. It says where to listen, which
// radio to open and which database to read. The installation itself - the
// network key, the coordinator identity, the devices, the zones and the
// circuits - lives in that database and is managed through the HTTP API.

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// BootstrapVersion is the schema version of the bootstrap file. Version 1 was
// the full installation config, which now lives in the database.
const BootstrapVersion = 2

// Defaults for the keys a bootstrap file leaves out.
const (
	defaultDatabaseName = "freedo-state.db"
	defaultUnhandledLog = "freedo-unhandled.log"
)

// DefaultHTTPListen is the API address of a bootstrap that names none. It is
// loopback, so a daemon started with no API token is not reachable from other
// hosts.
const DefaultHTTPListen = "127.0.0.1:8080"

// DefaultBeaconSec is the longest Router Advertisement interval of the
// commercial coordinator: its intervals fall between 200 s and 600 s on air, the
// Contiki MaxRtrAdvInterval of 600 s with MinRtrAdvInterval at a third of it. A
// device that gets no advertisement stops reporting after about 20 minutes and
// solicits one in broadcast.
const DefaultBeaconSec = 600

// Bootstrap is the whole freedo.yaml: twelve flat keys and a version.
type Bootstrap struct {
	Version int `yaml:"version"`
	// SerialPort is the ESP32 bridge. Empty means no live radio.
	SerialPort string `yaml:"serial_port"`
	HTTPListen string `yaml:"http_listen"`
	// Database holds the installation and the desired state. Empty means
	// freedo-state.db beside this file.
	Database     string `yaml:"database"`
	UnhandledLog string `yaml:"unhandled_log"`
	// APITokenFile holds the bearer token of the HTTP API. Empty means no
	// token from this file.
	APITokenFile string `yaml:"api_token_file"`
	// DebugAPI serves the decrypt and live-tap endpoints.
	DebugAPI bool   `yaml:"debug_api"`
	CSLPhase uint16 `yaml:"csl_phase"`
	// TxCounter seeds the downlink frame counter. Unset starts at zero.
	TxCounter    *uint32 `yaml:"tx_counter"`
	ZoneGetProxy bool    `yaml:"zone_get_proxy"`
	// ZoneProxyTimeoutSec bounds the wait for a proxied zone GET, in seconds.
	// Unset takes the emulator default; zero or negative waits forever.
	ZoneProxyTimeoutSec *int `yaml:"zone_proxy_timeout_sec"`
	// RadioTune is accepted and ignored, so a file that still sets it loads:
	// FSCTRL0 holds the stored calibration for the whole session.
	RadioTune *bool `yaml:"radio_tune"`
	// TuneDebug logs the FREQEST reading of every received frame.
	TuneDebug bool `yaml:"tune_debug"`
	// BeaconSec is the longest interval of the Router Advertisement we
	// broadcast, in seconds: each interval is random between a third of it and
	// all of it. Unset takes DefaultBeaconSec; 0 sends none.
	BeaconSec *int `yaml:"beacon_sec"`

	dir string
}

// movedKeys are the installation keys of the version-1 config. Finding one is
// how a bootstrap loader recognises a file that predates the database.
var movedKeys = []string{
	"home_id", "network_key", "network_key_file", "coordinator",
	"devices", "zones", "circuits", "options",
}

// LoadBootstrap reads the bootstrap file at path. A file that is not there is
// the built-in defaults when the operator named no --config, and an error when
// they did: a named file that cannot be read is a typo, not a container start.
func LoadBootstrap(path string, explicit bool) (*Bootstrap, error) {
	b := &Bootstrap{
		Version:      BootstrapVersion,
		HTTPListen:   DefaultHTTPListen,
		UnhandledLog: defaultUnhandledLog,
		dir:          filepath.Dir(path),
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !explicit && errors.Is(err, fs.ErrNotExist) {
			b.UnhandledLog = b.resolve(b.UnhandledLog)
			return b, nil
		}
		return nil, fmt.Errorf("bootstrap config: %w", err)
	}
	if err := checkMovedKeys(path, b.DefaultDatabase(), data); err != nil {
		return nil, err
	}
	// The file has to declare its own version, so an unversioned document is
	// refused instead of read as the current schema.
	b.Version = 0
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(b); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("bootstrap config %s: empty document", path)
		}
		return nil, fmt.Errorf("bootstrap config %s: %w", path, err)
	}
	if b.Version != BootstrapVersion {
		return nil, fmt.Errorf("bootstrap config %s: version %d is unsupported, this build reads %d",
			path, b.Version, BootstrapVersion)
	}
	if b.HTTPListen == "" {
		b.HTTPListen = DefaultHTTPListen
	}
	if b.UnhandledLog == "" {
		b.UnhandledLog = defaultUnhandledLog
	}
	b.Database = b.resolve(b.Database)
	b.UnhandledLog = b.resolve(b.UnhandledLog)
	b.APITokenFile = b.resolve(b.APITokenFile)
	return b, nil
}

// checkMovedKeys refuses a version-1 installation config. Migrating it in
// silence would start the daemon on a generated key while the real one sits in
// the file, which on air looks like a total radio failure.
func checkMovedKeys(path, dbPath string, data []byte) error {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		// A document that is not a mapping cannot hold the moved keys; the strict
		// decode reports it with the better message.
		return nil
	}
	var found []string
	for _, key := range movedKeys {
		if _, ok := raw[key]; ok {
			found = append(found, key)
		}
	}
	if v, ok := raw["version"]; ok {
		if n, ok := v.(int); ok && n == 1 {
			found = append(found, "version: 1")
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf(`bootstrap config %s still holds the installation (%s)

the installation lives in the SQLite database at %s: the network key, the
coordinator identity, the devices, the zones and the circuits.
this file keeps the bootstrap keys alone and declares version %d.
the options block is flat now: serial_port, http_listen, database,
unhandled_log, debug_api, csl_phase, tx_counter, zone_get_proxy,
zone_proxy_timeout_sec, tune_debug and beacon_sec are top-level keys.
options.utc_offset_min moved into the installation and is managed through the API.
options.pan_id is gone: the PAN id is a protocol constant.
to move this installation into the database, run the one-off seed script
(issue #8), or start the daemon and write the installation through the
management API under /api/install.
nothing is migrated for you on purpose`,
		path, strings.Join(found, ", "), dbPath, BootstrapVersion)
}

// resolve reads a relative path against the directory of the bootstrap file,
// so a daemon started from another working directory finds the same files.
func (b *Bootstrap) resolve(p string) string {
	if p == "" || filepath.IsAbs(p) || b.dir == "" || b.dir == "." {
		return p
	}
	return filepath.Join(b.dir, p)
}

// DefaultDatabase is the database path of a bootstrap that names none.
func (b *Bootstrap) DefaultDatabase() string {
	if b.dir == "" || b.dir == "." {
		return defaultDatabaseName
	}
	return filepath.Join(b.dir, defaultDatabaseName)
}

// Warnings lists the keys the file sets and the daemon ignores.
func (b *Bootstrap) Warnings() []string {
	if b.RadioTune == nil {
		return nil
	}
	return []string{"radio_tune is ignored: FSCTRL0 holds the calibration stored by 'freedo-dev autotune' for the whole session"}
}

// TxCounterSeed returns the initial downlink frame counter.
func (b *Bootstrap) TxCounterSeed() uint32 {
	if b.TxCounter == nil {
		return 0
	}
	return *b.TxCounter
}

// ZoneProxyTimeout is the bound on a proxied zone GET. Unset returns 0, which
// the emulator reads as its own default. A configured 0 collapses to the same
// "wait forever" as a negative value: a zero-second bound would expire every
// proxied GET before the member could possibly answer.
func (b *Bootstrap) ZoneProxyTimeout() time.Duration {
	if b.ZoneProxyTimeoutSec == nil {
		return 0
	}
	if *b.ZoneProxyTimeoutSec <= 0 {
		return -1
	}
	return time.Duration(*b.ZoneProxyTimeoutSec) * time.Second
}

// BeaconInterval is the longest interval of our Router Advertisement. Unset
// takes the one of the commercial coordinator; 0 and below disable the beacon.
func (b *Bootstrap) BeaconInterval() time.Duration {
	if b.BeaconSec == nil {
		return DefaultBeaconSec * time.Second
	}
	if *b.BeaconSec <= 0 {
		return 0
	}
	return time.Duration(*b.BeaconSec) * time.Second
}

// MustBootstrap loads the file named by --config, or dies.
func MustBootstrap(fs *flag.FlagSet) *Bootstrap {
	f := fs.Lookup("config")
	if f == nil {
		Die("this command registers no --config flag")
	}
	b, err := LoadBootstrap(f.Value.String(), SetFlags(fs)["config"])
	if err != nil {
		Die("%v", err)
	}
	return b
}

// StateFlag registers the database override on a flag set.
func StateFlag(fs *flag.FlagSet) {
	fs.String("state", "",
		"SQLite database holding the installation and the desired state (default: the bootstrap `database`, else freedo-state.db beside --config)")
}

// ResolveDatabase picks the database: --state first, then the bootstrap key,
// then freedo-state.db beside the bootstrap file.
func ResolveDatabase(fs *flag.FlagSet, b *Bootstrap) string {
	if f := fs.Lookup("state"); f != nil && f.Value.String() != "" {
		return f.Value.String()
	}
	if b.Database != "" {
		return b.Database
	}
	return b.DefaultDatabase()
}
