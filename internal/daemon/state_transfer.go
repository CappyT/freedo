package daemon

// state_transfer: the export and the import of the whole state database. The
// export is one plain SQLite file, a snapshot of the live database. The import
// checks an uploaded file, stages it next to the database and restarts the
// process: the start applies a staged file before it opens the database, so no
// code replaces a database that is open.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"freedo/internal/buildinfo"
	"freedo/internal/install"
)

const (
	metaExportedBy = "exported_by" // the daemon version that wrote an export
	metaExportedAt = "exported_at" // RFC 3339, UTC
	metaImportedAt = "imported_at" // unix seconds of the start that applied an import

	stagedImportSuffix = ".import"
	importBackupPrefix = "before-import-"
	importBackupsKept  = 3
	backupTimeLayout   = "20060102T150405Z"

	// midWait is how long a transmitting start waits after an import. A device
	// keeps the CoAP Message IDs it saw for EXCHANGE_LIFETIME (247 s), and the
	// imported database resumes its own Message ID: it could repeat one that
	// this daemon sent from the replaced database inside that time.
	midWait = 250 * time.Second

	stateExportBudget = 5 * time.Second
)

// stateImportMaxBody bounds an uploaded database. A test lowers it.
var stateImportMaxBody int64 = 64 << 20

var sqliteMagic = []byte("SQLite format 3\x00")

// errImportNewer marks a file whose schema this binary does not read.
var errImportNewer = errors.New("newer schema")

// Snapshot writes a consistent copy of the database to dest, and stamps the
// copy with the daemon version and the time. VACUUM INTO reads one snapshot
// of the database, so a write that runs at the same time is whole or absent.
func (s *stateStore) Snapshot(ctx context.Context, dest, version string, at time.Time) error {
	// The steps need one connection: the attachment belongs to it.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS snapshot`, dest); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT OR REPLACE INTO snapshot.meta (key, value) VALUES (?, ?), (?, ?)`,
		metaExportedBy, version, metaExportedAt, at.UTC().Format(time.RFC3339))
	if _, detachErr := conn.ExecContext(context.Background(), `DETACH DATABASE snapshot`); err == nil {
		err = detachErr
	}
	return err
}

// exportFileName is the download name of an export.
func exportFileName(version string, at time.Time) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			return r
		}
		return '_'
	}, version)
	return "freedo-state-" + clean + "-" + at.UTC().Format(backupTimeLayout) + ".db"
}

func (d *daemon) getStateExport(w http.ResponseWriter, r *http.Request) {
	if d.state == nil || d.state.path == "" {
		writeErr(w, http.StatusConflict, "this daemon has no state database")
		return
	}
	// The database holds the desired state of the last save: save now, so the
	// file holds what the daemon knows.
	deadline := time.Now().Add(stateExportBudget)
	if err := d.saveDesiredStateBefore(deadline); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "save the state before the export: "+err.Error())
		return
	}
	if err := d.saveEnergyBefore(deadline); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "save the boiler energy before the export: "+err.Error())
		return
	}
	// The copy holds the network key: it stays in the folder of the database.
	dir, err := os.MkdirTemp(filepath.Dir(d.state.path), ".export-")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.RemoveAll(dir)
	now := time.Now()
	file := filepath.Join(dir, "state.db")
	if err := d.state.Snapshot(r.Context(), file, buildinfo.Version, now); err != nil {
		writeErr(w, http.StatusInternalServerError, "export: "+err.Error())
		return
	}
	log.Printf("state: export served to %s", stateCaller(r))
	f, err := os.Open(file)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/vnd.sqlite3")
	h.Set("Content-Disposition", `attachment; filename="`+exportFileName(buildinfo.Version, now)+`"`)
	h.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	h.Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, f)
}

// importInfo is what checkImport reads from an accepted file.
type importInfo struct {
	Schema     string `json:"schema"`
	ExportedBy string `json:"exportedBy,omitempty"`
	ExportedAt string `json:"exportedAt,omitempty"`
}

// checkImport accepts the file at path as the next state database, the way the
// start would read it: a SQLite file, a freedo schema that is not newer than
// this binary, no damage, and an installation that loads. It migrates an older
// schema in the file and leaves one file with no -wal file.
func checkImport(path string) (importInfo, error) {
	var info importInfo
	head := make([]byte, len(sqliteMagic))
	f, err := os.Open(path)
	if err != nil {
		return info, err
	}
	_, err = io.ReadFull(f, head)
	f.Close()
	if err != nil || !bytes.Equal(head, sqliteMagic) {
		return info, errors.New("the file is not a SQLite database")
	}
	status, version, err := install.InspectSchema(path)
	if err != nil {
		return info, err
	}
	if status == install.SchemaNewer {
		return info, fmt.Errorf("%w: the file holds schema version %s and this daemon reads %s: update the daemon first",
			errImportNewer, version, install.SchemaVersion)
	}
	info.Schema = version
	store, err := openStateStore(path)
	if err != nil {
		return info, err
	}
	err = func() error {
		ctx := context.Background()
		var result string
		if err := store.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
			return err
		}
		if result != "ok" {
			return fmt.Errorf("the database is damaged: %s", result)
		}
		in, _, err := store.LoadInstall(ctx)
		if err != nil {
			return err
		}
		if err := in.ValidateCommissionable(true); err != nil {
			return err
		}
		for key, dst := range map[string]*string{metaExportedBy: &info.ExportedBy, metaExportedAt: &info.ExportedAt} {
			err := store.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(dst)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		// One file is what the start moves into place.
		var mode string
		if err := store.db.QueryRowContext(ctx, `PRAGMA journal_mode = DELETE`).Scan(&mode); err != nil {
			return err
		}
		if mode != "delete" {
			return fmt.Errorf("the journal mode stays %s", mode)
		}
		return nil
	}()
	if closeErr := store.Close(); err == nil {
		err = closeErr
	}
	return info, err
}

func (d *daemon) postStateImport(w http.ResponseWriter, r *http.Request) {
	if d.state == nil || d.state.path == "" {
		writeErr(w, http.StatusConflict, "this daemon has no state database")
		return
	}
	if r.URL.Query().Get("confirm") != "true" {
		writeErr(w, http.StatusBadRequest,
			"the import replaces the installation, the network key, the counters and the programs: add ?confirm=true")
		return
	}
	dir, err := os.MkdirTemp(filepath.Dir(d.state.path), ".import-")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "state.db")
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, err = io.Copy(f, http.MaxBytesReader(w, r.Body, stateImportMaxBody))
	if syncErr := f.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge,
			"body is longer than "+strconv.FormatInt(tooLarge.Limit, 10)+" bytes")
		return
	case err != nil:
		writeErr(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	info, err := checkImport(file)
	switch {
	case errors.Is(err, errImportNewer):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusBadRequest, "not a usable freedo state database: "+err.Error())
		return
	}
	if err := os.Rename(file, d.state.path+stagedImportSuffix); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("state: import accepted from %s: exported by freedo %s at %s", stateCaller(r), info.ExportedBy, info.ExportedAt)
	waitSec := 0
	if d.leasesCounters() {
		waitSec = int(midWait / time.Second)
	}
	writeJSON(w, http.StatusAccepted, struct {
		importInfo
		Restart bool `json:"restart"`
		WaitSec int  `json:"waitSec"`
	}{info, true, waitSec})
	select {
	case d.restart <- struct{}{}:
	default:
	}
}

// stateCaller names the peer of an export or an import for the log. The user id is
// the one the request carries: only the ingress gateway sets it from a session.
func stateCaller(r *http.Request) string {
	if id := r.Header.Get(remoteUserHeader); id != "" {
		return fmt.Sprintf("%s, Home Assistant user %q", r.RemoteAddr, id)
	}
	return r.RemoteAddr + ", with the API token"
}

// applyStagedImport moves a staged import into place before the start opens the
// database. The replaced database goes to backup/before-import-<time>/ next to
// it, and the three newest of these folders stay. A stop between the two moves
// leaves the staged file, and the next start finishes the work.
func applyStagedImport(path string, now time.Time) (bool, error) {
	staged := path + stagedImportSuffix
	if _, err := os.Stat(staged); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	var current []string
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(name); err == nil {
			current = append(current, name)
		}
	}
	if len(current) > 0 {
		backups := filepath.Join(filepath.Dir(path), "backup")
		if err := os.MkdirAll(backups, 0o700); err != nil {
			return false, err
		}
		keep := ""
		// Two imports in one second must not share a folder.
		for at := now; ; at = at.Add(time.Second) {
			keep = filepath.Join(backups, importBackupPrefix+at.UTC().Format(backupTimeLayout))
			err := os.Mkdir(keep, 0o700)
			if err == nil {
				break
			}
			if !errors.Is(err, os.ErrExist) {
				return false, err
			}
		}
		for _, name := range current {
			if err := os.Rename(name, filepath.Join(keep, filepath.Base(name))); err != nil {
				return false, err
			}
		}
		fmt.Printf("import: the state before this import is in %s\n", keep)
		if err := pruneImportBackups(backups); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: remove old import backups: %v\n", err)
		}
	}
	if err := os.Rename(staged, path); err != nil {
		return false, err
	}
	return true, nil
}

// pruneImportBackups keeps the newest importBackupsKept folders. The names sort
// by time. Each removal names one folder.
func pruneImportBackups(backups string) error {
	entries, err := os.ReadDir(backups)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), importBackupPrefix) {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for i, name := range names {
		if i < importBackupsKept {
			continue
		}
		if err := os.RemoveAll(filepath.Join(backups, name)); err != nil {
			return err
		}
	}
	return nil
}

// importWaitLeft is how long a transmitting start still waits after the import
// at importedAt (unix seconds). A time in the future waits the full time.
func importWaitLeft(importedAt uint64, now time.Time) time.Duration {
	left := time.Unix(int64(importedAt), 0).Add(midWait).Sub(now)
	return max(0, min(left, midWait))
}

// importWaitHandler is the API of a daemon that waits after an import. It holds
// no engine, no database and no radio, so no request changes the state or
// queues a frame. GET /api/health says how long the wait still lasts, and every
// other route answers 503.
func importWaitHandler(until time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		left := max(1, int(math.Ceil(time.Until(until).Seconds())))
		if r.URL.Path == "/api/health" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "mode": "live", "tx": true, "txArmed": false, "transmitAllowed": false,
				"importWait": map[string]int{"leftSec": left},
			})
			return
		}
		msg := fmt.Sprintf("freedo starts in %d s: it waits after the import of a state, so that no device still holds a Message ID of the replaced state", left)
		if strings.HasPrefix(r.URL.Path, "/api/state/") {
			msg += ". The export and the import are refused until the start: the daemon does not serve its database during the wait"
		}
		w.Header().Set("Retry-After", strconv.Itoa(left))
		writeErr(w, http.StatusServiceUnavailable, msg)
	})
}

// serveImportWait serves h on addr until the time until, then closes the
// listener and returns, so the start can listen on addr again.
func serveImportWait(addr string, h http.Handler, until time.Time) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := newAPIServer(addr, h)
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()
	select {
	case err := <-failed:
		return err
	case <-time.After(time.Until(until)):
	}
	ctx, cancel := context.WithTimeout(context.Background(), httpDrainBudget)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		return srv.Close()
	}
	return nil
}

// reexec replaces the process with a new start of the same command line. The
// process id stays, so a start script, a container or a service manager sees
// one process that keeps running.
func reexec() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
