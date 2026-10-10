package cli_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"skald/internal/store"
)

// newerSchemaSandbox returns a sandbox whose database has one project and a
// schema version far newer than this binary knows, as if a newer skald had
// migrated it.
func newerSchemaSandbox(t *testing.T) *sandbox {
	t.Helper()
	s := newSandbox(t)
	s.ok("init")
	s.ok("project", "create", "Stale", "--no-repo")
	db, err := store.Open(context.Background(), filepath.Join(s.home, "skald.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer().ExecContext(context.Background(), "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	return s
}

// dbState is a dump of what must not change when skald refuses a database.
func dbState(t *testing.T, s *sandbox) string {
	t.Helper()
	raw, err := sql.Open("sqlite", filepath.Join(s.home, "skald.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var v, projects, events int
	raw.QueryRow("PRAGMA user_version").Scan(&v)
	raw.QueryRow("SELECT COUNT(*) FROM projects").Scan(&projects)
	raw.QueryRow("SELECT COUNT(*) FROM events").Scan(&events)
	var updated string
	raw.QueryRow("SELECT MAX(updated_at) FROM projects").Scan(&updated)
	return sprintf("version=%d projects=%d events=%d updated=%s", v, projects, events, updated)
}

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }

func TestStaleBinaryRefusesNewerSchema(t *testing.T) {
	s := newerSchemaSandbox(t)
	before := dbState(t, s)

	// Commands that open the database fail with exit 1, a helpful stderr and an empty stdout.
	for _, args := range [][]string{
		{"project", "list"},
		{"status", "--project", "stale"},
		{"next", "--project", "stale"},
		{"init"},
		{"backup"},
	} {
		r := s.fail(1, args...)
		for _, want := range []string{"upgrade skald", "skald mcp", "skald serve", "999"} {
			contains(t, r.stderr, want)
		}
	}
	if after := dbState(t, s); after != before {
		t.Errorf("database changed after refused commands:\nbefore %s\nafter  %s", before, after)
	}
}

func TestStaleBinaryMCPFailsOnlyOnStderr(t *testing.T) {
	s := newerSchemaSandbox(t)
	before := dbState(t, s)

	// stdin is empty, so a working server would exit 0 on EOF; this one must refuse up front.
	r := s.fail(1, "mcp")
	contains(t, r.stderr, "upgrade skald")
	contains(t, r.stderr, "restart")
	if r.stdout != "" {
		t.Errorf("mcp wrote to stdout: %q", r.stdout)
	}
	if after := dbState(t, s); after != before {
		t.Errorf("database changed:\nbefore %s\nafter  %s", before, after)
	}
	// No backup directory or stray files were created either.
	entries, err := os.ReadDir(s.home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch e.Name() {
		case "skald.db", "skald.db-wal", "skald.db-shm":
		default:
			t.Errorf("unexpected file in SKALD_HOME: %s", e.Name())
		}
	}
}

// A binary that knows the schema keeps working on a database at the same version
// (control for the tests above).
func TestCurrentSchemaIsAccepted(t *testing.T) {
	s := newSandbox(t)
	s.ok("init")
	s.ok("project", "create", "Fine", "--no-repo")
	s.ok("project", "list")
}
