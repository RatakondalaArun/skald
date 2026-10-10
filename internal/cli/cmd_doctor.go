package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	_ "modernc.org/sqlite"

	"skald/internal/config"
	"skald/internal/store"
)

const (
	checkOK   = "OK"
	checkWarn = "WARN"
	checkFail = "FAIL"

	walWarnBytes = 64 << 20
	backupStale  = 7 * 24 * time.Hour
)

// silentError makes run exit 1 without printing anything; the command has
// already reported what went wrong on stdout.
type silentError struct{}

func (silentError) Error() string { return "" }

type doctorCheck struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Detail   string `json:"detail"`
	NextStep string `json:"next_step,omitempty"`
}

type doctorReport struct {
	OK     bool          `json:"ok"`
	Checks []doctorCheck `json:"checks"`
}

func (r *doctorReport) add(name, status, detail, next string) {
	r.Checks = append(r.Checks, doctorCheck{Name: name, Status: status, Detail: detail, NextStep: next})
	if status == checkFail {
		r.OK = false
	}
}

func newDoctorCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the skald data directory and database for problems",
		Long: `Checks the data directory and file permissions, the database's integrity and
foreign keys, the schema version against this binary, the write-ahead log size
and the backups. Prints OK, WARN or FAIL per check with the next step for each
problem, and exits 1 if anything FAILs. It never modifies the database.`,
		Args: noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			ctx := c.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			rep, err := runDoctor(ctx)
			if err != nil {
				return err
			}
			e := &env{json: a.json, out: c.OutOrStdout()}
			if err := e.emit(rep, func(w io.Writer) { printDoctor(w, rep) }); err != nil {
				return err
			}
			if !rep.OK {
				return silentError{}
			}
			return nil
		},
	}
}

func printDoctor(w io.Writer, r *doctorReport) {
	for _, ck := range r.Checks {
		fmt.Fprintf(w, "%-4s  %-16s  %s\n", ck.Status, ck.Name, ck.Detail)
		if ck.NextStep != "" {
			for _, l := range strings.Split(ck.NextStep, "\n") {
				fmt.Fprintf(w, "      -> %s\n", l)
			}
		}
	}
	if r.OK {
		fmt.Fprintln(w, "\nNo problems that need fixing.")
	} else {
		fmt.Fprintln(w, "\nProblems found. Fix the FAIL items above; skald changed nothing.")
	}
}

func runDoctor(ctx context.Context) (*doctorReport, error) {
	home, err := config.Home()
	if err != nil {
		return nil, err
	}
	dbPath, err := config.DBPath()
	if err != nil {
		return nil, err
	}
	r := &doctorReport{OK: true}

	homeOK := checkHome(r, home)
	if !homeOK {
		return r, nil
	}
	if !checkDBFile(r, dbPath) {
		checkBackups(r, dbPath)
		return r, nil
	}
	checkDBContents(ctx, r, dbPath)
	checkWAL(r, dbPath)
	checkBackups(r, dbPath)
	return r, nil
}

func checkHome(r *doctorReport, home string) bool {
	st, err := os.Stat(home)
	switch {
	case errors.Is(err, os.ErrNotExist):
		r.add("data directory", checkWarn, home+" does not exist yet", "Run `skald init` to create it.")
		return false
	case err != nil:
		r.add("data directory", checkFail, fmt.Sprintf("cannot read %s: %v", home, err), fmt.Sprintf("Check permissions with `ls -ld '%s'` or set SKALD_HOME to another directory.", home))
		return false
	case !st.IsDir():
		r.add("data directory", checkFail, home+" is not a directory", "Move that file away or set SKALD_HOME to another directory.")
		return false
	}
	f, err := os.CreateTemp(home, ".doctor-*")
	if err != nil {
		r.add("data directory", checkFail, fmt.Sprintf("%s is not writable (mode %04o)", home, st.Mode().Perm()), fmt.Sprintf("Run `chmod u+rwx '%s'` or set SKALD_HOME to another directory.", home))
		return false
	}
	f.Close()
	os.Remove(f.Name())
	if st.Mode().Perm()&0o077 != 0 {
		r.add("data directory", checkWarn, fmt.Sprintf("%s has mode %04o; other users can read it", home, st.Mode().Perm()), fmt.Sprintf("Run `chmod 700 '%s'`.", home))
		return true
	}
	r.add("data directory", checkOK, fmt.Sprintf("%s (mode %04o, writable)", home, st.Mode().Perm()), "")
	return true
}

func checkDBFile(r *doctorReport, dbPath string) bool {
	st, err := os.Stat(dbPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		r.add("database file", checkWarn, dbPath+" does not exist yet", "Run `skald init` to create it.")
		return false
	case err != nil:
		r.add("database file", checkFail, fmt.Sprintf("cannot read %s: %v", dbPath, err), fmt.Sprintf("Check permissions with `ls -l '%s'`.", dbPath))
		return false
	}
	if st.Mode().Perm()&0o077 != 0 {
		r.add("database file", checkWarn, fmt.Sprintf("%s has mode %04o; other users can read it", dbPath, st.Mode().Perm()), fmt.Sprintf("Run `chmod 600 '%s'`.", dbPath))
		return true
	}
	r.add("database file", checkOK, fmt.Sprintf("%s (%d bytes, mode %04o)", dbPath, st.Size(), st.Mode().Perm()), "")
	return true
}

func checkDBContents(ctx context.Context, r *doctorReport, dbPath string) {
	// A plain connection with no journal-mode pragma: it reads, never writes.
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(3000)")
	dsn := (&url.URL{Scheme: "file", Opaque: filepath.ToSlash(dbPath), RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		r.add("integrity", checkFail, err.Error(), "")
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var ver int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&ver); err != nil {
		failFromErr(r, "integrity", dbPath, err)
		return
	}

	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		failFromErr(r, "integrity", dbPath, err)
		return
	}
	var problems []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			failFromErr(r, "integrity", dbPath, err)
			return
		}
		if s != "ok" {
			problems = append(problems, s)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		failFromErr(r, "integrity", dbPath, err)
		return
	}
	if len(problems) > 0 {
		shown := problems
		if len(shown) > 3 {
			shown = shown[:3]
		}
		r.add("integrity", checkFail, fmt.Sprintf("integrity_check reported %d problem(s): %s", len(problems), strings.Join(shown, "; ")),
			(&store.CorruptError{Path: dbPath, Reason: "integrity check failed", Backups: store.ListBackups(dbPath)}).Message())
	} else {
		r.add("integrity", checkOK, "integrity_check passed", "")
	}

	n := 0
	fk, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		failFromErr(r, "foreign keys", dbPath, err)
	} else {
		for fk.Next() {
			n++
		}
		fk.Close()
		if n > 0 {
			r.add("foreign keys", checkFail, fmt.Sprintf("%d row(s) reference something that does not exist", n),
				"Restore from a backup (see `skald doctor` backups below), or inspect with `sqlite3 '"+dbPath+"' \"PRAGMA foreign_key_check\"`.")
		} else {
			r.add("foreign keys", checkOK, "no dangling references", "")
		}
	}

	latest := store.LatestSchemaVersion()
	switch {
	case ver > latest:
		r.add("schema version", checkFail, fmt.Sprintf("database is v%d, this skald supports up to v%d", ver, latest), "Upgrade skald, then restart any running `skald mcp` or `skald serve`.")
	case ver < latest:
		r.add("schema version", checkWarn, fmt.Sprintf("database is v%d, this skald is v%d", ver, latest), "It will be migrated the next time skald opens it. Run `skald backup` first if you want a safety copy.")
	default:
		r.add("schema version", checkOK, fmt.Sprintf("v%d matches this skald", ver), "")
	}
}

func failFromErr(r *doctorReport, name, dbPath string, err error) {
	cerr := store.Classify(dbPath, err)
	var ce *store.CorruptError
	var le *store.LockedError
	switch {
	case errors.As(cerr, &ce):
		r.add(name, checkFail, ce.Reason, ce.Message())
	case errors.As(cerr, &le):
		r.add(name, checkWarn, "the database is busy; could not check it", le.Message())
	default:
		r.add(name, checkFail, cerr.Error(), "")
	}
}

func checkWAL(r *doctorReport, dbPath string) {
	st, err := os.Stat(dbPath + "-wal")
	if err != nil {
		r.add("write-ahead log", checkOK, "no WAL file (nothing pending)", "")
		return
	}
	if st.Size() > walWarnBytes {
		r.add("write-ahead log", checkWarn, fmt.Sprintf("WAL is %.0f MB", float64(st.Size())/(1<<20)),
			"A long-running reader is probably holding it open. Stop idle `skald mcp` / `skald serve` processes; the log shrinks when they close.")
		return
	}
	r.add("write-ahead log", checkOK, fmt.Sprintf("WAL is %d bytes", st.Size()), "")
}

func checkBackups(r *doctorReport, dbPath string) {
	bks := store.ListBackups(dbPath)
	if len(bks) == 0 {
		r.add("backups", checkWarn, "no backups in "+store.BackupsDir(dbPath), "Run `skald backup`.")
		return
	}
	newest := bks[0]
	age := time.Since(newest.ModTime).Round(time.Minute)
	detail := fmt.Sprintf("%d backup(s); newest %s (%d bytes, %s ago)", len(bks), filepath.Base(newest.Path), newest.Size, age)
	if age > backupStale {
		r.add("backups", checkWarn, detail, "The newest backup is over 7 days old. Run `skald backup`.")
		return
	}
	r.add("backups", checkOK, detail, "")
}
