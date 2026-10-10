package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// SchemaTooNewError is returned when the database schema is newer than this binary.
type SchemaTooNewError struct {
	DBVersion, BinaryVersion int
}

func (e *SchemaTooNewError) Error() string {
	return fmt.Sprintf("database schema version %d is newer than this skald binary supports (%d): upgrade skald (and restart any running `skald mcp` or `skald serve` processes)",
		e.DBVersion, e.BinaryVersion)
}

func loadMigrations() ([]migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range entries {
		name := e.Name()
		prefix, _, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %q: name must start with a positive number and underscore", name)
		}
		b, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: v, name: name, sql: string(b)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration versions must be contiguous from 1: got %d at position %d", m.version, i+1)
		}
	}
	return ms, nil
}

// migrate applies pending migrations. The version check happens inside the
// write transaction, so concurrent openers cannot apply a migration twice.
func (d *DB) migrate(ctx context.Context) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	// An up-to-date database needs no write lock to open, so another process
	// holding a long write does not block read-only commands.
	if v, err := d.SchemaVersion(ctx); err == nil {
		if v > len(ms) {
			return &SchemaTooNewError{DBVersion: v, BinaryVersion: len(ms)}
		}
		if v == len(ms) {
			return nil
		}
	} else if c := sqliteCode(err); c == sqliteCorrupt || c == sqliteNotADB {
		return err
	}
	for {
		applied, err := d.applyNextRetry(ctx, ms)
		if err != nil {
			return err
		}
		if !applied {
			return nil
		}
	}
}

// migrateRetryWindow bounds how long a first open retries a busy database.
var migrateRetryWindow = 10 * time.Second

// applyNextRetry retries on SQLITE_BUSY. Concurrent first opens race on the
// one-time switch to WAL mode, where the busy timeout is not always honoured.
func (d *DB) applyNextRetry(ctx context.Context, ms []migration) (bool, error) {
	deadline := time.Now().Add(migrateRetryWindow)
	for {
		applied, err := d.applyNext(ctx, ms)
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return applied, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (d *DB) applyNext(ctx context.Context, ms []migration) (bool, error) {
	applied := false
	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		var v int
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
			return fmt.Errorf("read user_version: %w", err)
		}
		if v > len(ms) {
			return &SchemaTooNewError{DBVersion: v, BinaryVersion: len(ms)}
		}
		if v == len(ms) {
			return nil
		}
		m := ms[v]
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			return fmt.Errorf("set user_version: %w", err)
		}
		applied = true
		return nil
	})
	return applied, err
}

// SchemaVersion returns the current PRAGMA user_version.
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := d.reader.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}
