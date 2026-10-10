package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"skald/internal/domain"
)

// populate writes enough rows to span several database pages.
func populate(t *testing.T, db *DB) {
	t.Helper()
	err := db.WriteTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO projects(slug,name,created_at,updated_at) VALUES('p','P','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
			return err
		}
		for i := 0; i < 600; i++ {
			if _, err := tx.Exec(`INSERT INTO events(project_id,entity_type,entity_id,type,actor,payload,created_at) VALUES(1,'task',?,?,'human',?, '2026-01-01T00:00:00Z')`,
				i, "task.note", fmt.Sprintf(`{"text":"%s"}`, strings.Repeat("x", 60))); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// makeBackup writes a file into the backups directory with the given age.
func makeBackup(t *testing.T, dbPath, name string, size int, age time.Duration) string {
	t.Helper()
	dir := BackupsDir(dbPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, bytes.Repeat([]byte("b"), size), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
	return p
}

func shortTimeouts(t *testing.T) {
	t.Helper()
	oldB, oldM := busyTimeoutMS, migrateRetryWindow
	busyTimeoutMS, migrateRetryWindow = 100, 200*time.Millisecond
	t.Cleanup(func() { busyTimeoutMS, migrateRetryWindow = oldB, oldM })
}

func mustCorrupt(t *testing.T, err error) *CorruptError {
	t.Helper()
	var ce *CorruptError
	if !errors.As(err, &ce) {
		t.Fatalf("error = %T %v, want *CorruptError", err, err)
	}
	return ce
}

func TestListBackupsNewestFirst(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "skald.db")
	if got := ListBackups(dbPath); len(got) != 0 {
		t.Fatalf("missing dir should give no backups, got %v", got)
	}
	old := makeBackup(t, dbPath, "skald-old.db", 10, 48*time.Hour)
	mid := makeBackup(t, dbPath, "skald-mid.db", 2048, 24*time.Hour)
	fresh := makeBackup(t, dbPath, "skald-new.db", 3*1024*1024, time.Minute)
	if err := os.WriteFile(filepath.Join(BackupsDir(dbPath), "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := ListBackups(dbPath)
	if len(got) != 3 || got[0].Path != fresh || got[1].Path != mid || got[2].Path != old {
		t.Fatalf("order = %+v", got)
	}
	if got[0].Size != 3*1024*1024 {
		t.Errorf("size = %d", got[0].Size)
	}
}

func TestOpenNotADatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "skald.db")
	junk := bytes.Repeat([]byte("this is not sqlite. "), 500)
	if err := os.WriteFile(path, junk, 0o600); err != nil {
		t.Fatal(err)
	}
	newest := makeBackup(t, path, "skald-20261010T000000Z.db", 5000, time.Hour)
	makeBackup(t, path, "skald-20261001T000000Z.db", 100, 240*time.Hour)

	db, err := Open(context.Background(), path)
	if err == nil {
		db.Close()
		t.Fatal("Open succeeded on a non-database file")
	}
	ce := mustCorrupt(t, err)
	msg := err.Error()
	for _, want := range []string{
		path, "did not modify it", "not a SQLite database",
		newest, "4.9 KB", "Stop every running `skald mcp` and `skald serve`",
		"mv '" + path + "' '" + path + ".damaged'", "cp '" + newest + "' '" + path + "'", "rm -f '" + path + "-wal' '" + path + "-shm'",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	if strings.Index(msg, "20261010") > strings.Index(msg, "20261001") {
		t.Errorf("backups not listed newest first:\n%s", msg)
	}
	if len(ce.Backups) != 2 {
		t.Errorf("backups = %d", len(ce.Backups))
	}
	// The file is untouched and no WAL files appeared.
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, junk) {
		t.Error("database file was modified")
	}
	for _, sfx := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + sfx); err == nil {
			t.Errorf("%s was created", sfx)
		}
	}
	var de domain.Error
	if !errors.As(err, &de) || de.Message() != msg {
		t.Error("CorruptError should satisfy domain.Error so MCP passes it through")
	}
}

func TestOpenNotADatabaseWithoutBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skald.db")
	if err := os.WriteFile(path, bytes.Repeat([]byte("zz"), 4000), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), path)
	ce := mustCorrupt(t, err)
	if !strings.Contains(ce.Message(), "no backups") || !strings.Contains(ce.Message(), ".recover") {
		t.Errorf("message = %s", ce.Message())
	}
}

func TestOpenTruncatedHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skald.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	populate(t, db)
	db.Close()
	if err := os.Truncate(path, 100); err != nil {
		t.Fatal(err)
	}
	os.Remove(path + "-wal")
	os.Remove(path + "-shm")
	_, err = Open(context.Background(), path)
	mustCorrupt(t, err)
}

func TestTruncatedMidFileSurfacesAsCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skald.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	populate(t, db)
	db.Close()
	os.Remove(path + "-wal")
	os.Remove(path + "-shm")
	st, _ := os.Stat(path)
	if err := os.Truncate(path, st.Size()/2); err != nil {
		t.Fatal(err)
	}
	// Opening may or may not notice; reading the lost pages must produce a
	// recovery error, never raw driver text.
	db, err = Open(context.Background(), path)
	if err != nil {
		mustCorrupt(t, err)
		return
	}
	defer db.Close()
	err = db.ReadTx(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT payload FROM events`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
		}
		return rows.Err()
	})
	ce := mustCorrupt(t, err)
	if strings.Contains(ce.Error(), "SQLITE") {
		t.Errorf("raw driver text leaked: %s", ce.Error())
	}
}

func TestLockedDatabaseGivesFriendlyError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skald.db")
	shortTimeouts(t) // before Open: the timeout is part of each pool's DSN
	first, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	// Another connection takes the write lock and holds it.
	other, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(100)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	conn, err := other.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")

	check := func(label string, err error) {
		t.Helper()
		var le *LockedError
		if !errors.As(err, &le) {
			t.Fatalf("%s: error = %T %v, want *LockedError", label, err, err)
		}
		msg := err.Error()
		for _, want := range []string{path, "another skald process is writing", "retry", "lsof '" + path + "'"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: message missing %q: %s", label, want, msg)
			}
		}
		for _, bad := range []string{"SQLITE_BUSY", "database is locked", "(5)"} {
			if strings.Contains(msg, bad) {
				t.Errorf("%s: raw driver text %q leaked: %s", label, bad, msg)
			}
		}
	}

	start := time.Now()
	err = first.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO projects(slug,name,created_at,updated_at) VALUES('x','X','t','t')`)
		return err
	})
	check("WriteTx", err)
	if time.Since(start) > 3*time.Second {
		t.Errorf("lock wait took %v with a 100ms timeout", time.Since(start))
	}

	// An up-to-date database opens without the write lock, so Open does not
	// hang or fail while another process is writing.
	second, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open while another process writes: %v", err)
	}
	second.Close()

	// Reads are not blocked by the writer in WAL mode.
	if err := first.ReadTx(context.Background(), func(tx *sql.Tx) error {
		var n int
		return tx.QueryRow("SELECT count(*) FROM projects").Scan(&n)
	}); err != nil {
		t.Errorf("read while locked: %v", err)
	}
}

func TestOpenInUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o700) })

	path := filepath.Join(parent, "home", "skald.db")
	_, err := Open(context.Background(), path)
	var pe *PermissionError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %T %v, want *PermissionError", err, err)
	}
	for _, want := range []string{path, "permission denied", "chmod u+rwx", "SKALD_HOME"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message missing %q: %s", want, err)
		}
	}
}

func TestOpenExistingDatabaseInReadOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "skald.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	os.Remove(path + "-wal")
	os.Remove(path + "-shm")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	db, err = Open(context.Background(), path)
	if err == nil {
		db.Close()
		t.Skip("this SQLite build opens the database without a WAL; nothing to report")
	}
	var pe *PermissionError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %T %v, want *PermissionError", err, err)
	}
	if strings.Contains(err.Error(), "unable to open database file") {
		t.Errorf("raw driver text leaked: %s", err)
	}
}

func TestClassifyLeavesOtherErrorsAlone(t *testing.T) {
	plain := errors.New("something else")
	if Classify("/x/skald.db", plain) != plain {
		t.Error("unknown errors must pass through unchanged")
	}
	if Classify("/x/skald.db", nil) != nil {
		t.Error("nil must stay nil")
	}
	tooNew := &SchemaTooNewError{DBVersion: 9, BinaryVersion: 1}
	if Classify("/x/skald.db", fmt.Errorf("open: %w", tooNew)) == nil || !errors.As(Classify("/x/skald.db", tooNew), new(*SchemaTooNewError)) {
		t.Error("SchemaTooNewError must pass through")
	}
	v := &domain.ValidationError{Field: "f", Problem: "p", Hint: "h"}
	if Classify("/x/skald.db", v) != error(v) {
		t.Error("domain errors must pass through")
	}
}

func TestSchemaTooNewStillReportedByOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skald.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer().Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = Open(context.Background(), path)
	var te *SchemaTooNewError
	if !errors.As(err, &te) {
		t.Fatalf("error = %T %v, want *SchemaTooNewError", err, err)
	}
}
