package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"skald/internal/domain"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Querier is satisfied by *sql.DB, *sql.Conn and *sql.Tx, so read functions
// work against a pool or inside a transaction.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Meta says who is making a change and when. Every mutating query takes one
// and records it in the event it appends.
type Meta struct {
	Actor domain.Actor
	Now   time.Time // zero means time.Now()
}

func (m Meta) now() time.Time {
	if m.Now.IsZero() {
		return time.Now().UTC()
	}
	return m.Now.UTC()
}

func (m Meta) check() error {
	if !m.Actor.Valid() {
		return &domain.ValidationError{Field: "actor", Problem: fmt.Sprintf("%q is not a valid actor", string(m.Actor)), Hint: "Actors are non-empty and contain no spaces, such as \"human\"."}
	}
	return nil
}

// Design note: every mutating function in this package appends its own event
// in the transaction it is given. Callers cannot forget the event, and a
// rolled-back transaction never leaves an event behind. There is deliberately
// no exported AppendEvent.

const timeLayout = "2006-01-02T15:04:05Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	t, err := time.ParseInLocation(timeLayout, s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored time %q: %w", s, err)
	}
	return t, nil
}

func parseNullTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

type scanner interface{ Scan(dest ...any) error }

// appendEvent writes one row to the append-only event log and returns its ID.
func appendEvent(ctx context.Context, tx *sql.Tx, m Meta, projectID int64, et domain.EntityType, entityID int64, typ domain.EventType, payload any) (int64, error) {
	if err := m.check(); err != nil {
		return 0, err
	}
	if !typ.Valid() {
		return 0, fmt.Errorf("unknown event type %q", typ)
	}
	b := []byte("{}")
	if payload != nil {
		var err error
		if b, err = json.Marshal(payload); err != nil {
			return 0, fmt.Errorf("encode event payload: %w", err)
		}
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO events (project_id, entity_type, entity_id, type, actor, payload, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		projectID, string(et), entityID, string(typ), string(m.Actor), string(b), formatTime(m.now()))
	if err != nil {
		return 0, mapErr(err)
	}
	return res.LastInsertId()
}

var trailingCode = regexp.MustCompile(`\s*\(\d+\)$`)

// mapErr turns SQLite constraint failures into domain.ValidationError so
// driver errors never leak to callers. Other errors pass through.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return err
	}
	// The driver message looks like "constraint failed: UNIQUE constraint
	// failed: projects.slug (2067)"; keep only the part after the last colon.
	detail := strings.TrimSpace(trailingCode.ReplaceAllString(se.Error(), ""))
	if i := strings.LastIndex(detail, ": "); i >= 0 {
		detail = detail[i+2:]
	}
	switch se.Code() {
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
		field := detail
		if i := strings.LastIndex(field, "."); i >= 0 {
			field = field[i+1:]
		}
		return &domain.ValidationError{Field: field, Problem: "already exists", Hint: "Choose a different value, or update the existing item."}
	case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
		return &domain.ValidationError{Problem: "it refers to an item that does not exist", Hint: "Check the IDs you passed; list the items to see what exists."}
	case sqlite3.SQLITE_CONSTRAINT_CHECK:
		return &domain.ValidationError{Problem: fmt.Sprintf("a value is out of range or not allowed (%s)", detail), Hint: "Check the field limits: weight 1-10, priority 0-3, and valid status names."}
	case sqlite3.SQLITE_CONSTRAINT_NOTNULL:
		return &domain.ValidationError{Problem: fmt.Sprintf("a required value is missing (%s)", detail), Hint: "Provide all required fields."}
	}
	return err
}

// ReadTx runs fn in a read transaction on the reader pool. All reads inside
// see one consistent snapshot of the database.
func (d *DB) ReadTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.reader.BeginTx(ctx, nil)
	if err != nil {
		return Classify(d.path, fmt.Errorf("begin read tx: %w", err))
	}
	defer tx.Rollback()
	return Classify(d.path, fn(tx))
}

func validateRange(field string, v, lo, hi int) error {
	if v < lo || v > hi {
		return &domain.ValidationError{Field: field, Problem: fmt.Sprintf("%d is out of range", v), Hint: fmt.Sprintf("Use a value from %d to %d.", lo, hi)}
	}
	return nil
}

func requireText(field, v string) error {
	if strings.TrimSpace(v) == "" {
		return &domain.ValidationError{Field: field, Problem: "must not be empty", Hint: "Provide a non-empty " + field + "."}
	}
	return nil
}
