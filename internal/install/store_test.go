package install

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// A --state typo that lands on somebody else's database must stop startup, and
// must leave that database alone.
func TestOpenStateStoreRefusesAForeignDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "other.db")
	foreign, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Exec(`CREATE TABLE states (entity_id TEXT, state TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err == nil {
		store.Close()
		t.Fatal("a foreign database should be refused")
	}

	reopened, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "states" {
		t.Errorf("the foreign database holds %v, want only states", names)
	}
}

// The tune value survives a restart, so the radio starts near the module
// crystal offset instead of the register default.
func TestStateStoreFSCTRL0RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()
	store := newTestStore(t, path)
	if _, ok, err := store.FSCTRL0(ctx); err != nil || ok {
		t.Fatalf("a fresh database holds a tune: ok=%t err=%v", ok, err)
	}
	if err := store.SetFSCTRL0(ctx, 0x3b); err != nil {
		t.Fatal(err)
	}
	if err := store.SetFSCTRL0(ctx, 0x41); err != nil {
		t.Fatal(err)
	}
	got, ok, err := newTestStore(t, path).FSCTRL0(ctx)
	if err != nil || !ok || got != 0x41 {
		t.Fatalf("FSCTRL0=%#02x,%t,%v want 0x41,true,nil", got, ok, err)
	}
}
