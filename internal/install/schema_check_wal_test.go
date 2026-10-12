package install

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// InspectSchema reads a database that a stopped daemon left in the write-ahead
// log state, with its -wal and -shm files, the way a crash leaves it. The add-on
// start script runs the check on exactly that file before the daemon starts.
func TestInspectSchemaReadsAnUncleanlyClosedDatabase(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.db")
	store, err := Open(live)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(),
		`INSERT INTO plant_episode (zone, kind, body) VALUES (1, 'x', 'y')`); err != nil {
		t.Fatal(err)
	}
	// Copy the three files while the store holds the log open, so the copy keeps
	// its -wal and -shm.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		copyFile(t, live+suffix, filepath.Join(dir, "copy.db"+suffix))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "copy.db-wal")); err != nil {
		t.Fatalf("the copy has no write-ahead log: %v", err)
	}
	status, version, err := InspectSchema(filepath.Join(dir, "copy.db"))
	if err != nil {
		t.Fatalf("InspectSchema on a database with a write-ahead log: %v", err)
	}
	if status != SchemaReadable || version != SchemaVersion {
		t.Errorf("InspectSchema = %v/%q, want %v/%q", status, version, SchemaReadable, SchemaVersion)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
}
