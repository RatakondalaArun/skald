package store

import (
	"context"
	"database/sql"

	"skald/internal/domain"
)

const artifactCols = `id, task_id, kind, ref, summary, actor, created_at`

func scanArtifact(sc scanner) (domain.Artifact, error) {
	var (
		a                    domain.Artifact
		kind, actor, created string
		summary              sql.NullString
	)
	if err := sc.Scan(&a.ID, &a.TaskID, &kind, &a.Ref, &summary, &actor, &created); err != nil {
		return domain.Artifact{}, err
	}
	a.Kind, a.Summary, a.Actor = domain.ArtifactKind(kind), summary.String, domain.Actor(actor)
	var err error
	a.CreatedAt, err = parseTime(created)
	return a, err
}

// AddArtifact attaches verification evidence to a task and appends
// artifact.added (entity artifact, with the task ID in the payload). The
// artifact's actor is m.Actor. The task's updated_at is touched, but its
// version is not bumped.
func AddArtifact(ctx context.Context, tx *sql.Tx, m Meta, a domain.Artifact) (domain.Artifact, error) {
	if err := m.check(); err != nil {
		return domain.Artifact{}, err
	}
	if !a.Kind.Valid() {
		return domain.Artifact{}, &domain.ValidationError{Field: "kind", Problem: "unknown artifact kind " + string(a.Kind), Hint: "Use one of: commit, pr, test_report, link, manual."}
	}
	if err := requireText("ref", a.Ref); err != nil {
		return domain.Artifact{}, err
	}
	t, err := GetTask(ctx, tx, a.TaskID)
	if err != nil {
		return domain.Artifact{}, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO artifacts (task_id, kind, ref, summary, actor, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		a.TaskID, string(a.Kind), a.Ref, nullStr(a.Summary), string(m.Actor), formatTime(m.now()))
	if err != nil {
		return domain.Artifact{}, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return domain.Artifact{}, err
	}
	if err := touchTask(ctx, tx, m, t.ID); err != nil {
		return domain.Artifact{}, err
	}
	if _, err := appendEvent(ctx, tx, m, t.ProjectID, domain.EntityArtifact, id, domain.EventArtifactAdded,
		map[string]any{"task_id": t.ID, "kind": a.Kind, "ref": a.Ref}); err != nil {
		return domain.Artifact{}, err
	}
	return scanArtifact(tx.QueryRowContext(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE id = ?`, id))
}

// ListArtifacts returns a task's artifacts, oldest first.
func ListArtifacts(ctx context.Context, q Querier, taskID int64) ([]domain.Artifact, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountArtifacts returns how many artifacts a task has; the evidence gate uses
// it.
func CountArtifacts(ctx context.Context, q Querier, taskID int64) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE task_id = ?`, taskID).Scan(&n)
	return n, err
}
