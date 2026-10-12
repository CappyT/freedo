package install

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// newerSchemaVersion is one version past SchemaVersion, so the test holds
// across schema bumps.
func newerSchemaVersion(t *testing.T) string {
	t.Helper()
	n, err := strconv.Atoi(SchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	return strconv.Itoa(n + 1)
}

// makeDatabase writes a current-schema database at path and closes it.
func makeDatabase(t *testing.T, path string) {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

// setSchemaVersion rewrites the schema_version meta row of an existing database.
func setSchemaVersion(t *testing.T, path, version string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`, version); err != nil {
		t.Fatal(err)
	}
}

// InspectSchema classifies the four states the add-on start script checks, and
// never creates the file.
func TestInspectSchemaClassifiesTheVersion(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.db")

	current := filepath.Join(dir, "current.db")
	makeDatabase(t, current)

	older := filepath.Join(dir, "older.db")
	makeDatabase(t, older)
	setSchemaVersion(t, older, "1")

	newer := filepath.Join(dir, "newer.db")
	makeDatabase(t, newer)
	next := newerSchemaVersion(t)
	setSchemaVersion(t, newer, next)

	for _, tt := range []struct {
		name    string
		path    string
		want    SchemaStatus
		wantVer string
	}{
		{"missing", missing, SchemaMissing, ""},
		{"current", current, SchemaReadable, SchemaVersion},
		{"older", older, SchemaReadable, "1"},
		{"newer", newer, SchemaNewer, next},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, version, err := InspectSchema(tt.path)
			if err != nil {
				t.Fatalf("InspectSchema: %v", err)
			}
			if got != tt.want || version != tt.wantVer {
				t.Errorf("InspectSchema = %v/%q, want %v/%q", got, version, tt.want, tt.wantVer)
			}
		})
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("InspectSchema created the missing database")
	}
}

// A database that is not a freedo store is an error, not a classification.
func TestInspectSchemaRefusesAForeignDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "other.db")
	foreign, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Exec(`CREATE TABLE states (entity_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InspectSchema(path); err == nil {
		t.Fatal("a foreign database should be an error")
	}
}
