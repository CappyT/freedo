package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"freedo/internal/install"
)

// transferMeta holds the rows that an export or an import adds: they are not
// part of the state.
var transferMeta = map[string]bool{metaExportedBy: true, metaExportedAt: true, metaImportedAt: true}

// dumpState reads every row of every table, table by table, without the rows of
// the transfer itself.
func dumpState(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	names, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for names.Next() {
		var name string
		if err := names.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	names.Close()
	out := make(map[string][]string, len(tables))
	for _, table := range tables {
		rows, err := db.Query(`SELECT * FROM "` + table + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		out[table] = []string{}
		for rows.Next() {
			values := make([]any, len(cols))
			refs := make([]any, len(cols))
			for i := range values {
				refs[i] = &values[i]
			}
			if err := rows.Scan(refs...); err != nil {
				t.Fatal(err)
			}
			if key, ok := values[0].(string); table == "meta" && ok && transferMeta[key] {
				continue
			}
			out[table] = append(out[table], fmt.Sprint(values...))
		}
		rows.Close()
		sort.Strings(out[table])
	}
	return out
}

func dumpFile(t *testing.T, path string) map[string][]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	return dumpState(t, db)
}

// exportTo downloads the export of the daemon at base into dir.
func exportTo(t *testing.T, base, dir string) (string, http.Header) {
	t.Helper()
	resp, err := http.Get(base + "/api/state/export")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export: %d %s", resp.StatusCode, body)
	}
	sub, err := os.MkdirTemp(dir, "export")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "export.db")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, resp.Header
}

func importFrom(t *testing.T, url string, body []byte) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestStateExportEqualsTheLiveState(t *testing.T) {
	a := newInstallAPIOn(t, defaultInstall)
	var stored map[string]any
	if code := postJSON(t, http.MethodPut, a.base+"/api/zones/10/schedule", weekPlan(), &stored); code != http.StatusOK {
		t.Fatalf("schedule: %d %v", code, stored)
	}
	path, header := exportTo(t, a.base, t.TempDir())

	if got := header.Get("Content-Disposition"); !strings.HasPrefix(got, `attachment; filename="freedo-state-`) || !strings.HasSuffix(got, `Z.db"`) {
		t.Errorf("Content-Disposition = %q", got)
	}
	if got := header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got, want := dumpFile(t, path), dumpState(t, a.store.db); !reflect.DeepEqual(got, want) {
		t.Errorf("the export differs from the live state\n got: %v\nwant: %v", got, want)
	}
	info, err := checkImport(path)
	if err != nil {
		t.Fatalf("the export is not a valid state: %v", err)
	}
	if info.Schema != install.SchemaVersion || info.ExportedBy == "" {
		t.Errorf("info = %+v", info)
	}
	if _, err := time.Parse(time.RFC3339, info.ExportedAt); err != nil {
		t.Errorf("exportedAt %q: %v", info.ExportedAt, err)
	}
	// The stamp goes into the copy, never into the live database.
	if _, found := metaUint(t, a.store, metaExportedAt); found {
		t.Error("the export wrote its stamp into the live database")
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(a.path), ".export-*"))
	if len(left) != 0 {
		t.Errorf("the export left %v", left)
	}
}

// An export taken while the installation changes is a whole database.
func TestStateExportDuringWrites(t *testing.T) {
	a := newInstallAPIOn(t, defaultInstall)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		zones := []string{"Europe/Rome", "Europe/Berlin"}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			raw, _ := json.Marshal(map[string]string{"timezone": zones[i%2]})
			req, _ := http.NewRequest(http.MethodPut, a.base+"/api/install/timezone", bytes.NewReader(raw))
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}()
	dir := t.TempDir()
	for i := 0; i < 8; i++ {
		path, _ := exportTo(t, a.base, dir)
		if _, err := checkImport(path); err != nil {
			t.Fatalf("export %d is not a valid state: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestStateImportRefusals(t *testing.T) {
	a := newInstallAPIOn(t, defaultInstall)
	dir := t.TempDir()
	good, _ := exportTo(t, a.base, dir)
	export, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}

	other := filepath.Join(dir, "other.db")
	db, err := sql.Open("sqlite", "file:"+other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE notes (body TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	otherBytes, _ := os.ReadFile(other)

	newer := filepath.Join(dir, "newer.db")
	cur, _ := strconv.Atoi(install.SchemaVersion)
	if err := legacyDatabase(t, newer, strconv.Itoa(cur+1)).Close(); err != nil {
		t.Fatal(err)
	}
	newerBytes, _ := os.ReadFile(newer)

	empty := filepath.Join(dir, "empty.db")
	store, err := install.Open(empty)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	emptyBytes, _ := os.ReadFile(empty)

	damaged := bytes.Clone(export)
	for i := len(damaged) / 2; i < len(damaged); i++ {
		damaged[i] ^= 0xFF
	}

	before := dumpState(t, a.store.db)
	url := a.base + "/api/state/import"
	for _, tc := range []struct {
		name string
		url  string
		body []byte
		want int
	}{
		{"no confirm", url, export, http.StatusBadRequest},
		{"confirm is not true", url + "?confirm=yes", export, http.StatusBadRequest},
		{"not a database", url + "?confirm=true", []byte("this is a text file, not a database"), http.StatusBadRequest},
		{"an empty body", url + "?confirm=true", nil, http.StatusBadRequest},
		{"another SQLite database", url + "?confirm=true", otherBytes, http.StatusBadRequest},
		{"a damaged database", url + "?confirm=true", damaged, http.StatusBadRequest},
		{"a database with no installation", url + "?confirm=true", emptyBytes, http.StatusBadRequest},
		{"a newer schema", url + "?confirm=true", newerBytes, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := importFrom(t, tc.url, tc.body)
			if code != tc.want || body["error"] == nil {
				t.Errorf("status = %d %v, want %d", code, body, tc.want)
			}
		})
	}
	t.Run("an oversized file", func(t *testing.T) {
		saved := stateImportMaxBody
		stateImportMaxBody = int64(len(export) - 1)
		defer func() { stateImportMaxBody = saved }()
		if code, body := importFrom(t, url+"?confirm=true", export); code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d %v, want 413", code, body)
		}
	})

	if _, err := os.Stat(a.path + stagedImportSuffix); !os.IsNotExist(err) {
		t.Errorf("a refused import left a staged file: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(a.path), ".import-*")); len(left) != 0 {
		t.Errorf("a refused import left %v", left)
	}
	select {
	case <-a.d.restart:
		t.Error("a refused import asked for a restart")
	default:
	}
	if got := dumpState(t, a.store.db); !reflect.DeepEqual(got, before) {
		t.Error("a refused import changed the live state")
	}
}

// An accepted import stages one file, asks for the restart and leaves the live
// database alone: the next start replaces it.
func TestStateImportStages(t *testing.T) {
	a := newInstallAPIOn(t, defaultInstall)
	path, _ := exportTo(t, a.base, t.TempDir())
	export, _ := os.ReadFile(path)
	before := dumpState(t, a.store.db)

	code, body := importFrom(t, a.base+"/api/state/import?confirm=true", export)
	if code != http.StatusAccepted || body["restart"] != true || body["schema"] != install.SchemaVersion || body["waitSec"] != float64(0) {
		t.Fatalf("import: %d %v", code, body)
	}
	select {
	case <-a.d.restart:
	default:
		t.Error("the import did not ask for a restart")
	}
	staged := a.path + stagedImportSuffix
	if got := dumpFile(t, staged); !reflect.DeepEqual(got, before) {
		t.Error("the staged file differs from the export")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(staged + suffix); !os.IsNotExist(err) {
			t.Errorf("the staged file has a %s file", suffix)
		}
	}
	if got := dumpState(t, a.store.db); !reflect.DeepEqual(got, before) {
		t.Error("the import changed the open database")
	}
}

// A file of an older schema is accepted and migrated before it is staged.
func TestStateImportMigratesAnOlderSchema(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "older.db")
	if err := install.WriteInstall(older, defaultInstall); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+older)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP TABLE zone_program`, `DROP TABLE preset`,
		`UPDATE meta SET value = '7' WHERE key = 'schema_version'`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	info, err := checkImport(older)
	if err != nil {
		t.Fatal(err)
	}
	if info.Schema != "7" {
		t.Errorf("schema = %q, want the stored 7", info.Schema)
	}
	if status, version, err := install.InspectSchema(older); err != nil || status != install.SchemaReadable || version != install.SchemaVersion {
		t.Errorf("after the check: %v %q %v", status, version, err)
	}
}

func TestApplyStagedImportKeepsThreeBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

	if applied, err := applyStagedImport(path, now); applied || err != nil {
		t.Fatalf("nothing staged: %v %v", applied, err)
	}
	// With no database, the staged file takes the place and nothing is kept.
	os.WriteFile(path+stagedImportSuffix, []byte("state 0"), 0o600)
	if applied, err := applyStagedImport(path, now); !applied || err != nil {
		t.Fatalf("first import: %v %v", applied, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "backup")); !os.IsNotExist(err) {
		t.Error("an import with no state to replace made a backup")
	}

	for i := 1; i <= 5; i++ {
		os.WriteFile(path+"-wal", []byte("wal"), 0o600)
		os.WriteFile(path+stagedImportSuffix, []byte("state "+strconv.Itoa(i)), 0o600)
		// Two imports share each instant: each one still gets its own folder.
		if applied, err := applyStagedImport(path, now.Add(time.Duration(i/2)*time.Minute)); !applied || err != nil {
			t.Fatalf("import %d: %v %v", i, applied, err)
		}
	}
	if got, _ := os.ReadFile(path); string(got) != "state 5" {
		t.Errorf("the state is %q", got)
	}
	for _, name := range []string{path + "-wal", path + stagedImportSuffix} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Errorf("%s is still there", name)
		}
	}
	kept, _ := filepath.Glob(filepath.Join(dir, "backup", importBackupPrefix+"*"))
	sort.Strings(kept)
	if len(kept) != importBackupsKept {
		t.Fatalf("kept %v, want %d folders", kept, importBackupsKept)
	}
	for i, folder := range kept {
		got, _ := os.ReadFile(filepath.Join(folder, "state.db"))
		if want := "state " + strconv.Itoa(i+2); string(got) != want {
			t.Errorf("%s holds %q, want %q", folder, got, want)
		}
		if _, err := os.Stat(filepath.Join(folder, "state.db-wal")); err != nil {
			t.Errorf("%s has no -wal file: %v", folder, err)
		}
	}
}

func TestImportWaitLeft(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, tc := range []struct {
		importedAt uint64
		want       time.Duration
	}{
		{0, 0},
		{uint64(now.Unix()) - 300, 0},
		{uint64(now.Unix()) - 250, 0},
		{uint64(now.Unix()) - 100, 150 * time.Second},
		{uint64(now.Unix()), midWait},
		{uint64(now.Unix()) + 3600, midWait},
	} {
		if got := importWaitLeft(tc.importedAt, now); got != tc.want {
			t.Errorf("importWaitLeft(%d) = %s, want %s", tc.importedAt, got, tc.want)
		}
	}
}

// storedKey reads the network key of a database.
func storedKey(t *testing.T, store *stateStore) []byte {
	t.Helper()
	var key []byte
	if err := store.db.QueryRowContext(context.Background(), `SELECT key FROM net_key WHERE id = 1`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	return key
}

const daemonImportHelper = "FREEDO_TEST_DAEMON_IMPORT_HELPER"

// The round trip: the export of daemon A goes into daemon B, a process on its
// own fresh installation. B keeps its old database as a backup, restarts by
// itself with the same process id and serves the installation, the schedule
// and the network key of A.
func TestStateRoundTripAcrossARestart(t *testing.T) {
	if os.Getenv(daemonImportHelper) == "1" {
		Main(daemonHelperArgs())
		return
	}
	a := newInstallAPIOn(t, defaultInstall)
	var stored map[string]any
	if code := postJSON(t, http.MethodPut, a.base+"/api/zones/10/schedule", weekPlan(), &stored); code != http.StatusOK {
		t.Fatalf("schedule: %d %v", code, stored)
	}
	exported, _ := exportTo(t, a.base, t.TempDir())
	export, _ := os.ReadFile(exported)
	wantState := dumpState(t, a.store.db)
	wantKey := storedKey(t, a.store)

	dir := t.TempDir()
	state := filepath.Join(dir, "freedo-state.db")
	addr := freeLoopbackAddr(t)
	base := "http://" + addr
	cmd := exec.Command(os.Args[0], "-test.run=^TestStateRoundTripAcrossARestart$")
	cmd.Dir = dir
	cmd.Env = helperEnv(daemonImportHelper+"=1", "FREEDO_TEST_DAEMON_ARGS="+strings.Join([]string{"--addr", addr}, "\n"))
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	defer func() {
		if !stopped {
			cmd.Process.Kill()
			<-done
		}
	}()

	client := &http.Client{Timeout: 500 * time.Millisecond}
	// homeOf waits until the daemon serves the installation and returns its home id.
	homeOf := func(what string) float64 {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if resp, err := client.Get(base + "/api/install"); err == nil {
				var in map[string]any
				err := json.NewDecoder(resp.Body).Decode(&in)
				resp.Body.Close()
				if home, ok := in["home_id"].(float64); err == nil && ok {
					return home
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s: the daemon does not serve the installation\n%s", what, output.String())
		return 0
	}
	if own := homeOf("first start"); own == float64(defaultInstall.HomeID) {
		t.Fatal("the fresh daemon already has the home id of the fixture")
	}

	code, body := importFrom(t, base+"/api/state/import?confirm=true", export)
	if code != http.StatusAccepted || body["restart"] != true {
		t.Fatalf("import: %d %v\n%s", code, body, output.String())
	}
	deadline := time.Now().Add(20 * time.Second)
	for homeOf("after the import") != float64(defaultInstall.HomeID) {
		if time.Now().After(deadline) {
			t.Fatalf("the daemon did not come up on the imported state\n%s", output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	var wantSchedule, gotSchedule, wantInstall, gotInstall map[string]any
	getJSON(t, a.base+"/api/zones/10/schedule", &wantSchedule)
	if code := getJSON(t, base+"/api/zones/10/schedule", &gotSchedule); code != http.StatusOK || !reflect.DeepEqual(gotSchedule, wantSchedule) {
		t.Errorf("schedule after the import: %d\n got: %v\nwant: %v", code, gotSchedule, wantSchedule)
	}
	getJSON(t, a.base+"/api/install", &wantInstall)
	getJSON(t, base+"/api/install", &gotInstall)
	if !reflect.DeepEqual(gotInstall, wantInstall) {
		t.Errorf("installation after the import:\n got: %v\nwant: %v", gotInstall, wantInstall)
	}

	// The signal handler is armed just after the banner.
	time.Sleep(250 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatalf("the daemon exited with %v\n%s", err, output.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the daemon did not stop on SIGTERM\n%s", output.String())
	}
	for _, want := range []string{"import: a state is staged, restarting", "import: the state before this import is in", "import: the daemon starts on the imported state"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("the log does not carry %q:\n%s", want, output.String())
		}
	}

	store := newTestStore(t, state)
	if len(wantKey) == 0 || !bytes.Equal(storedKey(t, store), wantKey) {
		t.Error("the network key after the import is not the key of the export")
	}
	if at, found := metaUint(t, store, metaImportedAt); !found || time.Since(time.Unix(int64(at), 0)) > time.Minute {
		t.Errorf("imported_at = %d (found %v)", at, found)
	}
	// The desired state moves on with the new process: compare the installation tables.
	got := dumpState(t, store.db)
	for _, table := range []string{"install", "net_key", "device", "zone", "zone_member", "circuit", "preset", "zone_program", "sched_doc"} {
		if !reflect.DeepEqual(got[table], wantState[table]) {
			t.Errorf("table %s differs after the import\n got: %v\nwant: %v", table, got[table], wantState[table])
		}
	}
	kept, _ := filepath.Glob(filepath.Join(dir, "backup", importBackupPrefix+"*", "freedo-state.db"))
	if len(kept) != 1 {
		t.Fatalf("backups: %v, want one", kept)
	}
	old := newTestStore(t, kept[0])
	if key := storedKey(t, old); len(key) == 0 || bytes.Equal(key, wantKey) {
		t.Error("the backup does not hold the replaced installation")
	}
}
