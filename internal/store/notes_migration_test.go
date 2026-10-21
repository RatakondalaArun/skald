package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// Migration 2 rebuilds the events table to accept note events. The rebuilt
// table must keep its rows, its cursor, its index and its append-only
// triggers.
func TestMigration2RebuildsEvents(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "skald.db")
	copyFile(t, fixtureV1, path)
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertSeedIntact(t, db)

	err = db.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO notes (project_id, task_id, feature_id, body, actor, created_at)
			VALUES (1, 2, 1, 'upgraded', 'human', '2026-02-01T00:00:00Z')`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO events (project_id, entity_type, entity_id, type, actor, created_at)
			VALUES (1, 'note', 1, 'note.added', 'human', '2026-02-01T00:00:00Z')`)
		return err
	})
	if err != nil {
		t.Fatalf("note rows after upgrade: %v", err)
	}
	var maxID int64
	if err := db.Reader().QueryRowContext(ctx, `SELECT MAX(id) FROM events`).Scan(&maxID); err != nil || maxID != 4 {
		t.Errorf("event cursor after upgrade = %d, %v; want 4", maxID, err)
	}
	for _, q := range []string{`UPDATE events SET actor = 'x'`, `DELETE FROM events`} {
		err := db.WriteTx(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, q); return err })
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s after upgrade: %v, want append-only refusal", q, err)
		}
	}
	err = db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO events (project_id, entity_type, entity_id, type, actor, created_at)
			VALUES (1, 'bogus', 1, 'x', 'human', '2026-02-01T00:00:00Z')`)
		return err
	})
	if err == nil {
		t.Error("unknown entity_type accepted after upgrade")
	}
	var idx int
	if err := db.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_events_project_id'`).Scan(&idx); err != nil || idx != 1 {
		t.Errorf("events index after upgrade: %d, %v", idx, err)
	}
}
