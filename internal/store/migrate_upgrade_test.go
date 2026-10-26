package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureV1 is a schema-version-1 database built from 0001_init.sql with
// representative rows. Regenerate it with SKALD_UPDATE_FIXTURE=1 go test ./internal/store -run TestFixtureV1Generate.
const fixtureV1 = "testdata/v1.db"

// seedSQL uses only columns present in schema v1, with explicit column lists,
// so it stays valid after later migrations add columns with defaults.
var seedSQL = []string{
	`INSERT INTO projects (id, slug, name, description, status, repo_path, settings, created_at, updated_at)
	 VALUES (1, 'demo', 'Demo', 'seed project', 'active', '/tmp/demo', '{"require_evidence":true}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
	        (2, 'old', 'Old', NULL, 'archived', NULL, '{}', '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z')`,
	`INSERT INTO features (id, project_id, title, description, status, weight, position, created_at, updated_at)
	 VALUES (1, 1, 'Core', 'core feature', 'active', 3, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
	        (2, 1, 'Extras', NULL, 'planned', 1, 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	`INSERT INTO tasks (id, project_id, feature_id, title, description, acceptance_criteria, status, priority, weight, position,
	                    assignee, blocked_reason, idempotency_key, version, started_at, completed_at, created_at, updated_at)
	 VALUES (1, 1, 1, 'Design', 'd', 'ac1', 'done', 1, 2, 0, 'human', NULL, 'k-design', 4, '2026-01-01T01:00:00Z', '2026-01-01T02:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T02:00:00Z'),
	        (2, 1, 1, 'Build', NULL, NULL, 'in_progress', 2, 1, 1, 'claude:cli/abc', NULL, 'k-build', 2, '2026-01-01T03:00:00Z', NULL, '2026-01-01T00:00:00Z', '2026-01-01T03:00:00Z'),
	        (3, 1, 2, 'Polish', NULL, NULL, 'blocked', 3, 1, 0, NULL, 'waiting on review', NULL, 3, NULL, NULL, '2026-01-01T00:00:00Z', '2026-01-01T04:00:00Z')`,
	`INSERT INTO task_dependencies (task_id, depends_on_id) VALUES (2, 1), (3, 2)`,
	`INSERT INTO events (project_id, entity_type, entity_id, type, actor, payload, created_at)
	 VALUES (1, 'project', 1, 'project.created', 'human', '{}', '2026-01-01T00:00:00Z'),
	        (1, 'task', 1, 'task.status_changed', 'human', '{"to":"done"}', '2026-01-01T02:00:00Z'),
	        (1, 'task', 3, 'task.note', 'claude:cli/abc', '{"text":"blocked on review"}', '2026-01-01T04:00:00Z')`,
	`INSERT INTO artifacts (id, task_id, kind, ref, summary, actor, created_at)
	 VALUES (1, 1, 'commit', 'abc1234', 'initial design', 'human', '2026-01-01T02:00:00Z'),
	        (2, 1, 'link', 'https://example.com/spec', NULL, 'human', '2026-01-01T02:00:00Z')`,
	`INSERT INTO documents (id, project_id, kind, title, current_version) VALUES (1, 1, 'architecture', 'Overview', 2)`,
	`INSERT INTO document_versions (document_id, version, body, actor, created_at)
	 VALUES (1, 1, 'first draft', 'human', '2026-01-01T00:00:00Z'),
	        (1, 2, 'second draft', 'claude:cli/abc', '2026-01-01T05:00:00Z')`,
}

// seedCounts is what seedSQL inserts.
var seedCounts = map[string]int{
	"projects": 2, "features": 2, "tasks": 3, "task_dependencies": 2,
	"events": 3, "artifacts": 2, "documents": 1, "document_versions": 2,
}

// buildDBAt creates a database at path with migrations 1..n applied through
// raw SQL (not the runner), the way a binary that only knew n migrations would
// have left it. When seed is true and n >= 1, the seed rows are inserted.
func buildDBAt(t *testing.T, path string, n int, seed bool) {
	t.Helper()
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 || n > len(ms) {
		t.Fatalf("buildDBAt: n=%d out of range 1..%d", n, len(ms))
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := raw.ExecContext(ctx, ms[i].sql); err != nil {
			t.Fatalf("apply %s: %v", ms[i].name, err)
		}
		if i == 0 && seed {
			for _, q := range seedSQL {
				if _, err := raw.ExecContext(ctx, q); err != nil {
					t.Fatalf("seed: %v\n%s", err, q)
				}
			}
		}
	}
	if _, err := raw.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", n)); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, q Querier, table string) int {
	t.Helper()
	var n int
	if err := q.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func assertSeedIntact(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	r := db.Reader()
	for table, want := range seedCounts {
		if got := countRows(t, r, table); got != want {
			t.Errorf("%s: %d rows, want %d", table, got, want)
		}
	}
	var name, status, key, reason string
	var version int
	if err := r.QueryRowContext(ctx, `SELECT title, status, idempotency_key, version FROM tasks WHERE id = 1`).
		Scan(&name, &status, &key, &version); err != nil {
		t.Fatal(err)
	}
	if name != "Design" || status != "done" || key != "k-design" || version != 4 {
		t.Errorf("task 1 = %q %q %q %d", name, status, key, version)
	}
	if err := r.QueryRowContext(ctx, `SELECT blocked_reason FROM tasks WHERE id = 3`).Scan(&reason); err != nil || reason != "waiting on review" {
		t.Errorf("task 3 blocked_reason = %q, %v", reason, err)
	}
	var body string
	if err := r.QueryRowContext(ctx, `SELECT body FROM document_versions WHERE document_id = 1 AND version = 2`).Scan(&body); err != nil || body != "second draft" {
		t.Errorf("document body = %q, %v", body, err)
	}
	var ref string
	if err := r.QueryRowContext(ctx, `SELECT ref FROM artifacts WHERE id = 1`).Scan(&ref); err != nil || ref != "abc1234" {
		t.Errorf("artifact ref = %q, %v", ref, err)
	}
	// The event cursor must survive: new events continue after the old ones.
	var maxID int64
	if err := r.QueryRowContext(ctx, `SELECT MAX(id) FROM events`).Scan(&maxID); err != nil || maxID != 3 {
		t.Errorf("max event id = %d, %v", maxID, err)
	}
	// Domain-level read through the real query layer.
	ps, err := ListProjects(ctx, r, true)
	if err != nil || len(ps) != 2 {
		t.Errorf("ListProjects = %d projects, %v", len(ps), err)
	}
	// Tasks from before tags (migration 2) read back with no tags.
	if task, err := GetTask(ctx, r, 1); err != nil || task.Tags == nil || len(task.Tags) != 0 {
		t.Errorf("task 1 tags = %#v, %v; want empty", task.Tags, err)
	}
}

func latestVersion(t *testing.T) int {
	t.Helper()
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	return len(ms)
}

// TestFixtureV1Generate regenerates the checked-in fixture on request. It is a
// no-op unless SKALD_UPDATE_FIXTURE=1.
func TestFixtureV1Generate(t *testing.T) {
	if os.Getenv("SKALD_UPDATE_FIXTURE") != "1" {
		t.Skip("set SKALD_UPDATE_FIXTURE=1 to regenerate " + fixtureV1)
	}
	if err := os.MkdirAll(filepath.Dir(fixtureV1), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Remove(fixtureV1)
	buildDBAt(t, fixtureV1, 1, true)
}

// TestFixtureV1Upgrades opens the checked-in v1 database with the current code.
func TestFixtureV1Upgrades(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "skald.db")
	copyFile(t, fixtureV1, path)

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open(v1 fixture): %v", err)
	}
	defer db.Close()
	if v, err := db.SchemaVersion(ctx); err != nil || v != latestVersion(t) {
		t.Errorf("version = %d, %v; want %d", v, err, latestVersion(t))
	}
	assertSeedIntact(t, db)

	// The upgraded database is writable and keeps assigning event ids after the old ones.
	err = db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO events (project_id, entity_type, entity_id, type, actor, created_at)
			VALUES (1, 'task', 2, 'task.note', 'human', '2026-02-01T00:00:00Z')`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var maxID int64
	if err := db.Reader().QueryRowContext(ctx, `SELECT MAX(id) FROM events`).Scan(&maxID); err != nil || maxID != 4 {
		t.Errorf("max event id after insert = %d, %v; want 4", maxID, err)
	}
}

// TestFixtureV1MatchesMigration1 guards against the fixture drifting from
// 0001_init.sql: its schema must equal a freshly built v1 database.
func TestFixtureV1MatchesMigration1(t *testing.T) {
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	buildDBAt(t, fresh, 1, true)
	schema := func(path string) string {
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		rows, err := raw.Query(`SELECT type, name, sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var typ, name string
			var s sql.NullString
			if err := rows.Scan(&typ, &name, &s); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s %s: %s\n", typ, name, s.String)
		}
		var v int
		raw.QueryRow("PRAGMA user_version").Scan(&v)
		fmt.Fprintf(&b, "user_version=%d\n", v)
		return b.String()
	}
	if got, want := schema(fixtureV1), schema(fresh); got != want {
		t.Errorf("fixture schema differs from migration 1; regenerate with SKALD_UPDATE_FIXTURE=1\nfixture:\n%s\nfresh:\n%s", got, want)
	}
}

// TestUpgradeFromEveryVersion builds a database at every historical version N
// (migrations 1..N) and checks the current code brings it to the latest
// version with its data intact. A future migration is covered automatically.
func TestUpgradeFromEveryVersion(t *testing.T) {
	latest := latestVersion(t)
	for n := 1; n <= latest; n++ {
		t.Run(fmt.Sprintf("v%d", n), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "skald.db")
			buildDBAt(t, path, n, true)
			db, err := Open(ctx, path)
			if err != nil {
				t.Fatalf("Open at v%d: %v", n, err)
			}
			defer db.Close()
			if v, err := db.SchemaVersion(ctx); err != nil || v != latest {
				t.Errorf("version = %d, %v; want %d", v, err, latest)
			}
			assertSeedIntact(t, db)
			// Opening again is a no-op.
			db.Close()
			db2, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db2.Close()
			assertSeedIntact(t, db2)
		})
	}
}

// TestMigrationsAreContiguousAndNamed checks what the runner itself requires,
// so a bad new migration file fails here with a clear message.
func TestMigrationsAreContiguousAndNamed(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 {
		t.Fatal("no migrations embedded")
	}
	for _, m := range ms {
		if strings.Contains(strings.ToLower(m.sql), "pragma user_version") {
			t.Errorf("%s sets user_version; the runner owns it", m.name)
		}
	}
}

// TestFakeNextMigrationApplies runs a synthetic migration 2 through the
// runner's apply step on top of the seeded fixture: the runner takes its list
// as a parameter, so a future migration can be rehearsed without a new file.
func TestFakeNextMigrationApplies(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "skald.db")
	copyFile(t, fixtureV1, path)
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	next := len(ms) + 1
	fake := append(append([]migration{}, ms...), migration{
		version: next, name: "fake_add_column.sql",
		sql: `ALTER TABLE tasks ADD COLUMN estimate INTEGER NOT NULL DEFAULT 0;
		      UPDATE tasks SET estimate = weight * 2;`,
	})
	applied, err := db.applyNext(ctx, fake)
	if err != nil || !applied {
		t.Fatalf("applyNext = %v, %v", applied, err)
	}
	if v, _ := db.SchemaVersion(ctx); v != next {
		t.Errorf("version = %d, want %d", v, next)
	}
	var est int
	if err := db.Reader().QueryRowContext(ctx, `SELECT estimate FROM tasks WHERE id = 1`).Scan(&est); err != nil || est != 4 {
		t.Errorf("estimate = %d, %v; want 4", est, err)
	}
	assertSeedIntact(t, db)
	// Nothing further to apply.
	if applied, err := db.applyNext(ctx, fake); err != nil || applied {
		t.Errorf("second applyNext = %v, %v", applied, err)
	}
}

// TestFailedMigrationRollsBack: a migration that fails part-way (after some
// statements succeeded) must leave the previous version and data intact.
func TestFailedMigrationRollsBack(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "skald.db")
	copyFile(t, fixtureV1, path)
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	before, _ := db.SchemaVersion(ctx)

	ms, _ := loadMigrations()
	bad := append(append([]migration{}, ms...), migration{
		version: len(ms) + 1, name: "bad.sql",
		sql: `ALTER TABLE tasks ADD COLUMN half_done INTEGER;
		      DELETE FROM artifacts;
		      CREATE TABLE brand_new (id INTEGER PRIMARY KEY);
		      INSERT INTO table_that_does_not_exist VALUES (1);`,
	})
	applied, err := db.applyNext(ctx, bad)
	if err == nil || applied {
		t.Fatalf("applyNext = %v, %v; want failure", applied, err)
	}
	if !strings.Contains(err.Error(), "bad.sql") {
		t.Errorf("error should name the migration: %v", err)
	}
	if v, _ := db.SchemaVersion(ctx); v != before {
		t.Errorf("version = %d after failed migration, want %d", v, before)
	}
	assertSeedIntact(t, db)
	if _, err := db.Reader().ExecContext(ctx, `SELECT half_done FROM tasks`); err == nil {
		t.Error("column from failed migration survived")
	}
	if _, err := db.Reader().ExecContext(ctx, `SELECT * FROM brand_new`); err == nil {
		t.Error("table from failed migration survived")
	}
	// The writer is still usable afterwards.
	if err := db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE projects SET description = 'ok' WHERE id = 1`)
		return err
	}); err != nil {
		t.Errorf("writer unusable after failed migration: %v", err)
	}
	// And a reopen on disk still sees the same version.
	db.Close()
	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if v, _ := db2.SchemaVersion(ctx); v != before {
		t.Errorf("reopened version = %d, want %d", v, before)
	}
}

// TestNewerSchemaRefusedLeavesDataUntouched: Open refuses a too-new database
// with *SchemaTooNewError and changes neither the version nor any row.
func TestNewerSchemaRefusedLeavesDataUntouched(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "skald.db")
	copyFile(t, fixtureV1, path)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	_, err = Open(ctx, path)
	var tooNew *SchemaTooNewError
	if !errors.As(err, &tooNew) {
		t.Fatalf("err = %v, want *SchemaTooNewError", err)
	}
	if tooNew.DBVersion != 999 || tooNew.BinaryVersion != latestVersion(t) {
		t.Errorf("versions = %d/%d", tooNew.DBVersion, tooNew.BinaryVersion)
	}
	for _, want := range []string{"upgrade skald", "skald mcp", "skald serve"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}

	raw, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var v int
	raw.QueryRow("PRAGMA user_version").Scan(&v)
	if v != 999 {
		t.Errorf("user_version = %d, want 999", v)
	}
	for table, want := range seedCounts {
		if got := countRows(t, raw, table); got != want {
			t.Errorf("%s: %d rows, want %d", table, got, want)
		}
	}
}
