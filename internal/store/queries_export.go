package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"skald/internal/domain"
)

// ExportRows is a verbatim copy of the rows that belong to one project, or to
// every project. It is what skald export writes and skald import reads.
type ExportRows struct {
	Projects     []domain.Project
	Features     []domain.Feature
	Tasks        []domain.Task
	Dependencies []domain.Dependency
	Artifacts    []domain.Artifact
	Documents    []domain.Document
	DocVersions  []domain.DocumentVersion
	Events       []domain.Event
}

func collect[T any](ctx context.Context, q Querier, scan func(scanner) (T, error), query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// LoadExportRows reads every row for projectID (0 = all projects) in id order.
// Run it inside ReadTx so all tables come from one snapshot.
func LoadExportRows(ctx context.Context, q Querier, projectID int64) (ExportRows, error) {
	var (
		r   ExportRows
		err error
		// scope appends "WHERE <col> = ?" only for a single project.
		scope = func(col string) (string, []any) {
			if projectID == 0 {
				return "", nil
			}
			return " WHERE " + col + " = ?", []any{projectID}
		}
	)
	w, a := scope("id")
	if r.Projects, err = collect(ctx, q, scanProject, `SELECT `+projectCols+` FROM projects`+w+` ORDER BY id`, a...); err != nil {
		return r, err
	}
	w, a = scope("project_id")
	if r.Features, err = collect(ctx, q, scanFeature, `SELECT `+featureCols+` FROM features`+w+` ORDER BY id`, a...); err != nil {
		return r, err
	}
	if r.Tasks, err = collect(ctx, q, scanTask, `SELECT `+taskCols+` FROM tasks`+w+` ORDER BY id`, a...); err != nil {
		return r, err
	}
	if r.Documents, err = collect(ctx, q, scanDocument, `SELECT `+documentCols+` FROM documents`+w+` ORDER BY id`, a...); err != nil {
		return r, err
	}
	if r.Events, err = collect(ctx, q, scanEvent, `SELECT `+eventCols+` FROM events`+w+` ORDER BY id`, a...); err != nil {
		return r, err
	}
	// Dependencies, artifacts and document versions hang off tasks/documents,
	// so scope them through their parent.
	depQ := `SELECT task_id, depends_on_id FROM task_dependencies d`
	artQ := `SELECT ` + artifactCols + ` FROM artifacts`
	verQ := `SELECT document_id, version, body, actor, created_at FROM document_versions`
	if projectID != 0 {
		depQ += ` WHERE d.task_id IN (SELECT id FROM tasks WHERE project_id = ?)`
		artQ += ` WHERE task_id IN (SELECT id FROM tasks WHERE project_id = ?)`
		verQ += ` WHERE document_id IN (SELECT id FROM documents WHERE project_id = ?)`
	}
	if r.Dependencies, err = collect(ctx, q, func(sc scanner) (domain.Dependency, error) {
		var d domain.Dependency
		return d, sc.Scan(&d.TaskID, &d.DependsOnID)
	}, depQ+` ORDER BY task_id, depends_on_id`, a...); err != nil {
		return r, err
	}
	if r.Artifacts, err = collect(ctx, q, scanArtifact, artQ+` ORDER BY id`, a...); err != nil {
		return r, err
	}
	r.DocVersions, err = collect(ctx, q, scanDocumentVersion, verQ+` ORDER BY document_id, version`, a...)
	return r, err
}

// DatabaseIsEmpty reports whether no project, task or event exists yet.
func DatabaseIsEmpty(ctx context.Context, q Querier) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM projects) + (SELECT COUNT(*) FROM events) + (SELECT COUNT(*) FROM tasks)`).Scan(&n)
	return n == 0, err
}

// ImportExportRows inserts rows with their original ids, events included,
// into an empty database. It does not append events of its own: the exported
// event log is the history. Foreign keys are checked by SQLite, so a row set
// that refers to missing parents is rejected and the caller's transaction
// rolls back.
func ImportExportRows(ctx context.Context, tx *sql.Tx, r ExportRows) error {
	empty, err := DatabaseIsEmpty(ctx, tx)
	if err != nil {
		return err
	}
	if !empty {
		return &domain.ValidationError{Field: "database", Problem: "is not empty", Hint: "Import into a fresh database: set SKALD_HOME to a new directory and run `skald import` there."}
	}
	exec := func(what string, query string, args ...any) error {
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("import %s: %w", what, mapErr(err))
		}
		return nil
	}
	for _, p := range r.Projects {
		settings, err := json.Marshal(p.Settings)
		if err != nil {
			return err
		}
		if err := exec(fmt.Sprintf("project %d", p.ID), `INSERT INTO projects (`+projectCols+`) VALUES (?,?,?,?,?,?,?,?,?)`,
			p.ID, p.Slug, p.Name, nullStr(p.Description), string(p.Status), nullStr(p.RepoPath), string(settings), formatTime(p.CreatedAt), formatTime(p.UpdatedAt)); err != nil {
			return err
		}
	}
	for _, f := range r.Features {
		if err := exec(fmt.Sprintf("feature %d", f.ID), `INSERT INTO features (`+featureCols+`) VALUES (?,?,?,?,?,?,?,?,?)`,
			f.ID, f.ProjectID, f.Title, nullStr(f.Description), string(f.Status), f.Weight, f.Position, formatTime(f.CreatedAt), formatTime(f.UpdatedAt)); err != nil {
			return err
		}
	}
	for _, t := range r.Tasks {
		if err := exec(fmt.Sprintf("task %d", t.ID), `INSERT INTO tasks (id, project_id, feature_id, title, description, acceptance_criteria, status, priority, weight, position,
			assignee, blocked_reason, idempotency_key, version, started_at, completed_at, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			t.ID, t.ProjectID, t.FeatureID, t.Title, nullStr(t.Description), nullStr(t.AcceptanceCriteria), string(t.Status), t.Priority, t.Weight, t.Position,
			nullStr(t.Assignee), nullStr(t.BlockedReason), nullStr(t.IdempotencyKey), t.Version, nullTime(t.StartedAt), nullTime(t.CompletedAt),
			formatTime(t.CreatedAt), formatTime(t.UpdatedAt)); err != nil {
			return err
		}
	}
	for _, d := range r.Dependencies {
		if err := exec("dependency", `INSERT INTO task_dependencies (task_id, depends_on_id) VALUES (?,?)`, d.TaskID, d.DependsOnID); err != nil {
			return err
		}
	}
	for _, a := range r.Artifacts {
		if err := exec(fmt.Sprintf("artifact %d", a.ID), `INSERT INTO artifacts (`+artifactCols+`) VALUES (?,?,?,?,?,?,?)`,
			a.ID, a.TaskID, string(a.Kind), a.Ref, nullStr(a.Summary), string(a.Actor), formatTime(a.CreatedAt)); err != nil {
			return err
		}
	}
	for _, d := range r.Documents {
		if err := exec(fmt.Sprintf("document %d", d.ID), `INSERT INTO documents (`+documentCols+`) VALUES (?,?,?,?,?)`,
			d.ID, d.ProjectID, d.Kind, d.Title, d.CurrentVersion); err != nil {
			return err
		}
	}
	for _, v := range r.DocVersions {
		if err := exec("document version", `INSERT INTO document_versions (document_id, version, body, actor, created_at) VALUES (?,?,?,?,?)`,
			v.DocumentID, v.Version, v.Body, string(v.Actor), formatTime(v.CreatedAt)); err != nil {
			return err
		}
	}
	for _, e := range r.Events {
		payload := string(e.Payload)
		if payload == "" {
			payload = "{}"
		}
		if err := exec(fmt.Sprintf("event %d", e.ID), `INSERT INTO events (`+eventCols+`) VALUES (?,?,?,?,?,?,?,?)`,
			e.ID, e.ProjectID, string(e.EntityType), e.EntityID, string(e.Type), string(e.Actor), payload, formatTime(e.CreatedAt)); err != nil {
			return err
		}
	}
	return nil
}
