// Package store is the SQLite persistence layer; see docs/ARCHITECTURE.md section 5.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	_ "modernc.org/sqlite"
)

// busyTimeoutMS is how long a connection waits for a lock. Tests shorten it.
var busyTimeoutMS = 5000

// DB wraps a single-connection writer pool and a multi-connection reader pool
// over one SQLite file.
type DB struct {
	path   string
	writer *sql.DB
	reader *sql.DB
}

// Open creates the database directory (0700) and file (0600) if needed, opens
// the writer and reader pools, and applies pending migrations.
func Open(ctx context.Context, path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, Classify(path, fmt.Errorf("create database dir: %w", err))
	}
	// Create the file with 0600 before SQLite does it with the umask default.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, Classify(path, fmt.Errorf("create database file: %w", err))
	}
	f.Close()

	w, err := openPool(path, true)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	r, err := openPool(path, false)
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(max(4, runtime.NumCPU()))

	db := &DB{path: path, writer: w, reader: r}
	if err := db.migrate(ctx); err != nil {
		db.Close()
		return nil, Classify(path, err)
	}
	return db, nil
}

func openPool(path string, writer bool) (*sql.DB, error) {
	q := url.Values{}
	for _, p := range []string{
		fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS), // first, so later pragmas (journal_mode) wait on locks
		"journal_mode(WAL)",
		"foreign_keys(1)",
		"synchronous(NORMAL)",
	} {
		q.Add("_pragma", p)
	}
	if writer {
		q.Set("_txlock", "immediate")
	}
	dsn := (&url.URL{Scheme: "file", Opaque: filepath.ToSlash(path), RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	return db, nil
}

// Reader returns the pool for read-only queries.
func (d *DB) Reader() *sql.DB { return d.reader }

// Writer returns the single-connection pool. Prefer WriteTx for mutations.
func (d *DB) Writer() *sql.DB { return d.writer }

// Path returns the database file path.
func (d *DB) Path() string { return d.path }

// Close closes both pools.
func (d *DB) Close() error {
	werr := d.writer.Close()
	rerr := d.reader.Close()
	if werr != nil {
		return werr
	}
	return rerr
}

// WriteTx runs fn inside a BEGIN IMMEDIATE transaction on the writer pool. It
// commits if fn returns nil and rolls back on error or panic.
func (d *DB) WriteTx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		return Classify(d.path, fmt.Errorf("begin write tx: %w", err))
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return Classify(d.path, err)
	}
	if err = tx.Commit(); err != nil {
		return Classify(d.path, fmt.Errorf("commit write tx: %w", err))
	}
	return nil
}
