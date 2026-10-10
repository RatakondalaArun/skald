package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "sub", "skald.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestFileModes(t *testing.T) {
	db := openTemp(t)
	fi, err := os.Stat(db.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 600", got)
	}
	di, err := os.Stat(filepath.Dir(db.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("dir mode = %o, want 700", got)
	}
}

func checkPragmas(t *testing.T, conn *sql.Conn, label string) {
	t.Helper()
	ctx := context.Background()
	want := map[string]string{
		"journal_mode": "wal",
		"busy_timeout": "5000",
		"foreign_keys": "1",
		"synchronous":  "1", // NORMAL
	}
	for p, w := range want {
		var got string
		if err := conn.QueryRowContext(ctx, "PRAGMA "+p).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != w {
			t.Errorf("%s: PRAGMA %s = %q, want %q", label, p, got, w)
		}
	}
}

func TestPragmasOnEveryConnection(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	wc, err := db.Writer().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkPragmas(t, wc, "writer")
	wc.Close()

	// Hold several reader connections open at once so the pool must create distinct ones.
	var conns []*sql.Conn
	for i := 0; i < 4; i++ {
		c, err := db.Reader().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		checkPragmas(t, c, fmt.Sprintf("reader %d", i))
	}
	for _, c := range conns {
		c.Close()
	}
}

func TestConcurrentWritersNoBusy(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	if err := db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO projects (slug, name, created_at, updated_at) VALUES ('p','p','t','t')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	const workers, per = 8, 50
	var wg sync.WaitGroup
	errs := make(chan error, workers*per)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				errs <- db.WriteTx(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `INSERT INTO events (project_id, entity_type, entity_id, type, actor, created_at) VALUES (1,'project',1,'x','human','t')`)
					return err
				})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("write failed: %v", err)
		}
	}
	var n int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != workers*per {
		t.Errorf("events = %d, want %d", n, workers*per)
	}
}

func TestWriteTxRollsBackOnErrorAndPanic(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	ins := func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO projects (slug, name, created_at, updated_at) VALUES ('r','r','t','t')`)
		return err
	}
	boom := errors.New("boom")
	if err := db.WriteTx(ctx, func(tx *sql.Tx) error {
		if err := ins(tx); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	func() {
		defer func() { _ = recover() }()
		_ = db.WriteTx(ctx, func(tx *sql.Tx) error {
			_ = ins(tx)
			panic("p")
		})
	}()
	var n int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM projects`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("projects = %d, want 0 after rollbacks", n)
	}
	// Writer must still be usable after a panic.
	if err := db.WriteTx(ctx, ins); err != nil {
		t.Fatal(err)
	}
}

func TestReopenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "skald.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	v1, _ := db.SchemaVersion(ctx)
	if err := db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO projects (slug, name, created_at, updated_at) VALUES ('keep','k','t','t')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	v2, _ := db.SchemaVersion(ctx)
	ms, _ := loadMigrations()
	if v1 != len(ms) || v2 != len(ms) {
		t.Errorf("versions = %d, %d; want %d", v1, v2, len(ms))
	}
	var n int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM projects`).Scan(&n); err != nil || n != 1 {
		t.Errorf("projects = %d, err %v; want data preserved", n, err)
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "skald.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer().ExecContext(ctx, "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = Open(ctx, path)
	var tooNew *SchemaTooNewError
	if !errors.As(err, &tooNew) {
		t.Fatalf("err = %v, want SchemaTooNewError", err)
	}
	if tooNew.DBVersion != 999 || !strings.Contains(err.Error(), "upgrade skald") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestConcurrentOpenMigratesOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "skald.db")
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := Open(ctx, path)
			if err == nil {
				db.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// --- schema constraint tests ---

func exec(t *testing.T, db *DB, q string, args ...any) error {
	t.Helper()
	return db.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(q, args...)
		return err
	})
}

func mustExec(t *testing.T, db *DB, q string, args ...any) {
	t.Helper()
	if err := exec(t, db, q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func seed(t *testing.T) *DB {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO projects (id, slug, name, created_at, updated_at) VALUES (1,'p','p','t','t')`)
	mustExec(t, db, `INSERT INTO features (id, project_id, title, created_at, updated_at) VALUES (1,1,'f','t','t')`)
	mustExec(t, db, `INSERT INTO tasks (id, project_id, feature_id, title, created_at, updated_at) VALUES (1,1,1,'a','t','t')`)
	mustExec(t, db, `INSERT INTO tasks (id, project_id, feature_id, title, created_at, updated_at) VALUES (2,1,1,'b','t','t')`)
	return db
}

func TestSchemaConstraints(t *testing.T) {
	db := seed(t)
	const task = `INSERT INTO tasks (project_id, feature_id, title, %s, created_at, updated_at) VALUES (1,1,'x',%s,'t','t')`
	rejects := map[string]string{
		"task weight 0":        fmt.Sprintf(task, "weight", "0"),
		"task weight 11":       fmt.Sprintf(task, "weight", "11"),
		"task priority -1":     fmt.Sprintf(task, "priority", "-1"),
		"task priority 4":      fmt.Sprintf(task, "priority", "4"),
		"task bad status":      fmt.Sprintf(task, "status", "'nope'"),
		"task missing feature": `INSERT INTO tasks (project_id, feature_id, title, created_at, updated_at) VALUES (1,99,'x','t','t')`,
		"task missing project": `INSERT INTO tasks (project_id, feature_id, title, created_at, updated_at) VALUES (99,1,'x','t','t')`,
		"feature weight 11":    `INSERT INTO features (project_id, title, weight, created_at, updated_at) VALUES (1,'f',11,'t','t')`,
		"feature bad status":   `INSERT INTO features (project_id, title, status, created_at, updated_at) VALUES (1,'f','nope','t','t')`,
		"project bad status":   `INSERT INTO projects (slug, name, status, created_at, updated_at) VALUES ('q','q','nope','t','t')`,
		"duplicate slug":       `INSERT INTO projects (slug, name, created_at, updated_at) VALUES ('p','p','t','t')`,
		"self dependency":      `INSERT INTO task_dependencies (task_id, depends_on_id) VALUES (1,1)`,
		"dependency fk":        `INSERT INTO task_dependencies (task_id, depends_on_id) VALUES (1,99)`,
		"artifact bad kind":    `INSERT INTO artifacts (task_id, kind, ref, actor, created_at) VALUES (1,'nope','r','a','t')`,
		"event bad entity":     `INSERT INTO events (project_id, entity_type, entity_id, type, actor, created_at) VALUES (1,'nope',1,'x','a','t')`,
	}
	for name, q := range rejects {
		if err := exec(t, db, q); err == nil {
			t.Errorf("%s: expected constraint failure", name)
		}
	}

	mustExec(t, db, `INSERT INTO task_dependencies (task_id, depends_on_id) VALUES (1,2)`)
	if err := exec(t, db, `INSERT INTO task_dependencies (task_id, depends_on_id) VALUES (1,2)`); err == nil {
		t.Error("duplicate dependency should fail")
	}
	mustExec(t, db, `INSERT INTO artifacts (task_id, kind, ref, actor, created_at) VALUES (1,'commit','abc','a','t')`)
	// Deleting a task cascades to its dependencies and artifacts.
	mustExec(t, db, `DELETE FROM tasks WHERE id = 1`)
	var n int
	db.Reader().QueryRow(`SELECT (SELECT count(*) FROM task_dependencies) + (SELECT count(*) FROM artifacts)`).Scan(&n)
	if n != 0 {
		t.Errorf("cascade left %d rows", n)
	}
}

func TestEventsAppendOnly(t *testing.T) {
	db := seed(t)
	mustExec(t, db, `INSERT INTO events (project_id, entity_type, entity_id, type, actor, created_at) VALUES (1,'task',1,'x','a','t')`)
	if err := exec(t, db, `UPDATE events SET type = 'y'`); err == nil {
		t.Error("UPDATE on events should fail")
	}
	if err := exec(t, db, `DELETE FROM events`); err == nil {
		t.Error("DELETE on events should fail")
	}
	var typ string
	if err := db.Reader().QueryRow(`SELECT type FROM events`).Scan(&typ); err != nil || typ != "x" {
		t.Errorf("event = %q, err %v; want untouched", typ, err)
	}
}

func TestIdempotencyKey(t *testing.T) {
	db := seed(t)
	ins := `INSERT INTO tasks (project_id, feature_id, title, idempotency_key, created_at, updated_at) VALUES (1,1,'x',?,'t','t')`
	mustExec(t, db, ins, nil)
	mustExec(t, db, ins, nil) // two NULL keys in one project are allowed
	mustExec(t, db, ins, "k1")
	if err := exec(t, db, ins, "k1"); err == nil {
		t.Error("duplicate non-null idempotency key should be rejected")
	}
	// Same key in a different project is fine.
	mustExec(t, db, `INSERT INTO projects (id, slug, name, created_at, updated_at) VALUES (2,'p2','p2','t','t')`)
	mustExec(t, db, `INSERT INTO features (id, project_id, title, created_at, updated_at) VALUES (2,2,'f','t','t')`)
	mustExec(t, db, `INSERT INTO tasks (project_id, feature_id, title, idempotency_key, created_at, updated_at) VALUES (2,2,'x','k1','t','t')`)
}
