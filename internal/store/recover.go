package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"modernc.org/sqlite"

	"skald/internal/domain"
)

// SQLite primary result codes the recovery errors care about.
const (
	sqlitePerm     = 3
	sqliteBusy     = 5
	sqliteLocked   = 6
	sqliteReadonly = 8
	sqliteIOErr    = 10
	sqliteCorrupt  = 11
	sqliteCantOpen = 14
	sqliteNotADB   = 26
)

// BackupInfo describes one file in the backups directory.
type BackupInfo struct {
	Path    string    `json:"path"`
	Size    int64     `json:"bytes"`
	ModTime time.Time `json:"modified"`
}

// BackupsDir returns the directory that `skald backup` writes to for the
// database at dbPath.
func BackupsDir(dbPath string) string { return filepath.Join(filepath.Dir(dbPath), "backups") }

// ListBackups returns the .db files in the backups directory, newest first.
// A missing directory yields an empty list.
func ListBackups(dbPath string) []BackupInfo {
	entries, err := os.ReadDir(BackupsDir(dbPath))
	if err != nil {
		return nil
	}
	var out []BackupInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupInfo{Path: filepath.Join(BackupsDir(dbPath), e.Name()), Size: info.Size(), ModTime: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ModTime.Equal(out[j].ModTime) {
			return out[i].ModTime.After(out[j].ModTime)
		}
		return out[i].Path > out[j].Path
	})
	return out
}

// The recovery errors implement domain.Error (with KindUnknown) so entry
// points that pass domain error messages through, such as the MCP server,
// show their guidance instead of a generic failure.

// CorruptError means the database file is damaged or is not a SQLite file.
type CorruptError struct {
	Path    string
	Reason  string
	Backups []BackupInfo
	err     error
}

func (e *CorruptError) Error() string          { return e.Message() }
func (e *CorruptError) Unwrap() error          { return e.err }
func (e *CorruptError) Kind() domain.ErrorKind { return domain.KindUnknown }

func (e *CorruptError) Message() string {
	var b strings.Builder
	fmt.Fprintf(&b, "the skald database %s is damaged (%s). skald did not modify it.\n", e.Path, e.Reason)
	if len(e.Backups) > 0 {
		b.WriteString("Backups, newest first:\n")
		for _, bk := range e.Backups {
			fmt.Fprintf(&b, "  %s  %s  %s\n", bk.Path, humanSize(bk.Size), bk.ModTime.Format("2006-01-02 15:04 UTC"))
		}
		newest := e.Backups[0].Path
		b.WriteString("To restore the newest backup:\n")
		b.WriteString("  1. Stop every running `skald mcp` and `skald serve` (close Claude Code sessions that use skald).\n")
		fmt.Fprintf(&b, "  2. mv '%s' '%s.damaged'\n", e.Path, e.Path)
		fmt.Fprintf(&b, "  3. cp '%s' '%s'\n", newest, e.Path)
		fmt.Fprintf(&b, "  4. rm -f '%s-wal' '%s-shm'\n", e.Path, e.Path)
		b.WriteString("  5. skald doctor")
	} else {
		fmt.Fprintf(&b, "There are no backups in %s. Keep the file as it is and try `sqlite3 '%s' \".recover\"` to salvage data into a new database. Run `skald backup` regularly to avoid this.", BackupsDir(e.Path), e.Path)
	}
	return b.String()
}

// LockedError means another process held the write lock for longer than the
// busy timeout.
type LockedError struct {
	Path string
	err  error
}

func (e *LockedError) Error() string          { return e.Message() }
func (e *LockedError) Unwrap() error          { return e.err }
func (e *LockedError) Kind() domain.ErrorKind { return domain.KindUnknown }

func (e *LockedError) Message() string {
	return fmt.Sprintf("the skald database %s is busy: another skald process is writing to it. This is normally brief, so retry in a few seconds. To see which processes have it open, run: lsof '%s' (long-lived holders are usually `skald mcp` or `skald serve`). Nothing was changed by this call.", e.Path, e.Path)
}

// PermissionError means skald cannot read or write its data directory or file.
type PermissionError struct {
	Path string
	Op   string
	err  error
}

func (e *PermissionError) Error() string          { return e.Message() }
func (e *PermissionError) Unwrap() error          { return e.err }
func (e *PermissionError) Kind() domain.ErrorKind { return domain.KindUnknown }

func (e *PermissionError) Message() string {
	dir := filepath.Dir(e.Path)
	return fmt.Sprintf("skald cannot %s %s: permission denied or the location is read-only. skald needs read and write access to the directory %s (it also creates skald.db-wal and skald.db-shm there). Check it with `ls -ld '%s'`, fix it with `chmod u+rwx '%s'`, or point skald at another directory with SKALD_HOME.", e.Op, e.Path, dir, dir, dir)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// sqliteCode returns the primary SQLite result code in err's chain, or 0.
func sqliteCode(err error) int {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() & 0xff
	}
	return 0
}

func isBusy(err error) bool {
	var le *LockedError
	if errors.As(err, &le) {
		return true
	}
	if c := sqliteCode(err); c == sqliteBusy || c == sqliteLocked {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") || strings.Contains(s, "database is locked")
}

// Classify turns raw driver and filesystem failures for the database at path
// into the recovery errors above. Errors it does not recognise, and errors
// that are already typed, are returned unchanged.
func Classify(path string, err error) error {
	if err == nil {
		return nil
	}
	var ce *CorruptError
	var le *LockedError
	var pe *PermissionError
	var te *SchemaTooNewError
	if errors.As(err, &ce) || errors.As(err, &le) || errors.As(err, &pe) || errors.As(err, &te) || domain.KindOf(err) != domain.KindUnknown {
		return err
	}
	switch c := sqliteCode(err); {
	case c == sqliteCorrupt:
		return &CorruptError{Path: path, Reason: "database disk image is malformed", Backups: ListBackups(path), err: err}
	case c == sqliteNotADB:
		return &CorruptError{Path: path, Reason: "the file is not a SQLite database", Backups: ListBackups(path), err: err}
	case c == sqliteBusy || c == sqliteLocked:
		return &LockedError{Path: path, err: err}
	case c == sqlitePerm || c == sqliteReadonly || c == sqliteCantOpen:
		return &PermissionError{Path: path, Op: "open or write", err: err}
	}
	if errors.Is(err, fs.ErrPermission) {
		return &PermissionError{Path: path, Op: "access", err: err}
	}
	if isBusy(err) {
		return &LockedError{Path: path, err: err}
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "file is not a database"):
		return &CorruptError{Path: path, Reason: "the file is not a SQLite database", Backups: ListBackups(path), err: err}
	case strings.Contains(s, "malformed"):
		return &CorruptError{Path: path, Reason: "database disk image is malformed", Backups: ListBackups(path), err: err}
	case strings.Contains(s, "readonly database"), strings.Contains(s, "unable to open database file"):
		return &PermissionError{Path: path, Op: "open or write", err: err}
	}
	return err
}

// LatestSchemaVersion is the schema version this binary migrates to.
func LatestSchemaVersion() int {
	ms, err := loadMigrations()
	if err != nil {
		return 0
	}
	return len(ms)
}
