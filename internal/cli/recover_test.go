package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"skald/internal/store"
)

type doctorRun struct {
	stdout, stderr string
	code           int
}

// runDoctorCmd runs `skald doctor` in-process against home.
func runDoctorCmd(t *testing.T, home string, asJSON bool) doctorRun {
	t.Helper()
	t.Setenv("SKALD_HOME", home)
	cmd := newDoctorCmd(&app{json: asJSON})
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(nil)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true // as on the real root command
	code := run(cmd, &errb)
	return doctorRun{out.String(), errb.String(), code}
}

func healthyHome(t *testing.T) (home, dbPath string) {
	t.Helper()
	home = filepath.Join(t.TempDir(), "home")
	dbPath = filepath.Join(home, "skald.db")
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	return home, dbPath
}

func writeBackup(t *testing.T, dbPath, name string, age time.Duration) {
	t.Helper()
	dir := store.BackupsDir(dbPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
}

func lineFor(out, name string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, name) {
			return l
		}
	}
	return ""
}

func TestDoctorHealthy(t *testing.T) {
	home, dbPath := healthyHome(t)
	writeBackup(t, dbPath, "skald-recent.db", time.Hour)
	r := runDoctorCmd(t, home, false)
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for name, status := range map[string]string{
		"data directory": "OK", "database file": "OK", "integrity": "OK",
		"foreign keys": "OK", "schema version": "OK", "write-ahead log": "OK", "backups": "OK",
	} {
		if l := lineFor(r.stdout, name); !strings.HasPrefix(l, status) {
			t.Errorf("%s line = %q, want status %s\n%s", name, l, status, r.stdout)
		}
	}
	if !strings.Contains(r.stdout, "No problems") || r.stderr != "" {
		t.Errorf("stdout=%q stderr=%q", r.stdout, r.stderr)
	}
}

func TestDoctorJSON(t *testing.T) {
	home, _ := healthyHome(t)
	r := runDoctorCmd(t, home, true)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var rep struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name, Status, Detail string
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, r.stdout)
	}
	if !rep.OK || len(rep.Checks) < 6 {
		t.Errorf("report = %+v", rep)
	}
	// With no backups it is a WARN, which keeps ok=true.
	var backups string
	for _, c := range rep.Checks {
		if c.Name == "backups" {
			backups = c.Status
		}
	}
	if backups != "WARN" {
		t.Errorf("backups status = %q, want WARN", backups)
	}
}

func TestDoctorNothingInitialisedIsWarnNotFail(t *testing.T) {
	home := filepath.Join(t.TempDir(), "never-created")
	r := runDoctorCmd(t, home, false)
	if r.code != 0 {
		t.Fatalf("exit %d", r.code)
	}
	if l := lineFor(r.stdout, "data directory"); !strings.HasPrefix(l, "WARN") || !strings.Contains(r.stdout, "skald init") {
		t.Errorf("output:\n%s", r.stdout)
	}
	if _, err := os.Stat(home); err == nil {
		t.Error("doctor must not create the data directory")
	}
}

func TestDoctorCorruptDatabaseFails(t *testing.T) {
	home, dbPath := healthyHome(t)
	writeBackup(t, dbPath, "skald-good.db", time.Hour)
	junk := bytes.Repeat([]byte("garbage "), 1000)
	if err := os.WriteFile(dbPath, junk, 0o600); err != nil {
		t.Fatal(err)
	}
	r := runDoctorCmd(t, home, false)
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", r.code, r.stdout)
	}
	if r.stderr != "" {
		t.Errorf("stderr should be empty, got %q", r.stderr)
	}
	for _, want := range []string{"FAIL", "not a SQLite database", "skald-good.db", "cp '", "Problems found"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}
	after, _ := os.ReadFile(dbPath)
	if !bytes.Equal(after, junk) {
		t.Error("doctor modified the database file")
	}
}

func TestDoctorSchemaNewerThanBinaryFails(t *testing.T) {
	home, dbPath := healthyHome(t)
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	r := runDoctorCmd(t, home, false)
	if r.code != 1 {
		t.Fatalf("exit %d\n%s", r.code, r.stdout)
	}
	if l := lineFor(r.stdout, "schema version"); !strings.HasPrefix(l, "FAIL") || !strings.Contains(r.stdout, "Upgrade skald") {
		t.Errorf("output:\n%s", r.stdout)
	}
}

func TestDoctorOlderSchemaIsWarn(t *testing.T) {
	home, dbPath := healthyHome(t)
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 0"); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	r := runDoctorCmd(t, home, false)
	if r.code != 0 {
		t.Fatalf("exit %d\n%s", r.code, r.stdout)
	}
	if l := lineFor(r.stdout, "schema version"); !strings.HasPrefix(l, "WARN") {
		t.Errorf("output:\n%s", r.stdout)
	}
}

func TestDoctorForeignKeyViolationFails(t *testing.T) {
	home, dbPath := healthyHome(t)
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=foreign_keys(0)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO features(project_id,title,created_at,updated_at) VALUES(999,'orphan','t','t')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	r := runDoctorCmd(t, home, false)
	if r.code != 1 {
		t.Fatalf("exit %d\n%s", r.code, r.stdout)
	}
	if l := lineFor(r.stdout, "foreign keys"); !strings.HasPrefix(l, "FAIL") || !strings.Contains(l, "1 row") {
		t.Errorf("output:\n%s", r.stdout)
	}
}

func TestDoctorPermissionWarnings(t *testing.T) {
	home, dbPath := healthyHome(t)
	if err := os.Chmod(dbPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	r := runDoctorCmd(t, home, false)
	if r.code != 0 {
		t.Fatalf("exit %d", r.code)
	}
	if !strings.HasPrefix(lineFor(r.stdout, "data directory"), "WARN") || !strings.Contains(r.stdout, "chmod 700") {
		t.Errorf("directory warning missing:\n%s", r.stdout)
	}
	if !strings.HasPrefix(lineFor(r.stdout, "database file"), "WARN") || !strings.Contains(r.stdout, "chmod 600") {
		t.Errorf("file warning missing:\n%s", r.stdout)
	}
}

func TestDoctorUnwritableHomeFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	home, _ := healthyHome(t)
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(home, 0o700) })
	r := runDoctorCmd(t, home, false)
	if r.code != 1 {
		t.Fatalf("exit %d\n%s", r.code, r.stdout)
	}
	if l := lineFor(r.stdout, "data directory"); !strings.HasPrefix(l, "FAIL") || !strings.Contains(r.stdout, "not writable") {
		t.Errorf("output:\n%s", r.stdout)
	}
}

func TestDoctorBackupsAndWAL(t *testing.T) {
	home, dbPath := healthyHome(t)
	writeBackup(t, dbPath, "skald-old.db", 30*24*time.Hour)
	r := runDoctorCmd(t, home, false)
	if l := lineFor(r.stdout, "backups"); !strings.HasPrefix(l, "WARN") || !strings.Contains(r.stdout, "over 7 days") {
		t.Errorf("stale backup not flagged:\n%s", r.stdout)
	}
	writeBackup(t, dbPath, "skald-new.db", time.Minute)
	r = runDoctorCmd(t, home, false)
	if l := lineFor(r.stdout, "backups"); !strings.HasPrefix(l, "OK") || !strings.Contains(l, "2 backup(s)") || !strings.Contains(l, "skald-new.db") {
		t.Errorf("backups line = %q", l)
	}
	if l := lineFor(r.stdout, "write-ahead log"); !strings.HasPrefix(l, "OK") {
		t.Errorf("wal line = %q", l)
	}
}

// ---- error printing through the real command tree -----------------------

func runRoot(t *testing.T, home string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	t.Setenv("SKALD_HOME", home)
	t.Setenv("SKALD_PROJECT", "")
	root := NewRootCmd()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(args)
	code = run(root, &errb)
	return out.String(), errb.String(), code
}

func TestCorruptDatabaseMessageThroughCLI(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	dbPath := filepath.Join(home, "skald.db")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	writeBackup(t, dbPath, "skald-b1.db", time.Hour)
	if err := os.WriteFile(dbPath, bytes.Repeat([]byte("nope"), 3000), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errs, code := runRoot(t, home, "project", "list")
	if code != 1 || out != "" {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
	for _, want := range []string{"skald: ", dbPath, "did not modify it", "skald-b1.db", "Stop every running"} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr missing %q:\n%s", want, errs)
		}
	}
}

func TestLockedDatabaseMessageThroughCLI(t *testing.T) {
	home, dbPath := healthyHome(t)
	// Hold the write lock from another connection (as a busy `skald mcp` would).
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(100)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	conn, err := raw.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")

	// Reads keep working while another process writes.
	if _, errs, code := runRoot(t, home, "project", "list"); code != 0 {
		t.Fatalf("read while locked: code=%d stderr=%s", code, errs)
	}
	// A write waits out the busy timeout (5s), then reports the friendly message.
	out, errs, code := runRoot(t, home, "project", "create", "Locked", "--no-repo")
	if code != 1 || out != "" {
		t.Fatalf("code=%d stdout=%q stderr=%s", code, out, errs)
	}
	for _, want := range []string{"another skald process is writing", "lsof '" + dbPath + "'", "retry"} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr missing %q:\n%s", want, errs)
		}
	}
	for _, bad := range []string{"SQLITE_BUSY", "database is locked", "begin write tx"} {
		if strings.Contains(errs, bad) {
			t.Errorf("raw driver text %q leaked:\n%s", bad, errs)
		}
	}
}

func TestUnwritableHomeMessageThroughCLI(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o700) })
	home := filepath.Join(parent, "skald-home")
	_, errs, code := runRoot(t, home, "init")
	if code != 1 {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"permission denied", "SKALD_HOME", "chmod u+rwx"} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr missing %q:\n%s", want, errs)
		}
	}
}
