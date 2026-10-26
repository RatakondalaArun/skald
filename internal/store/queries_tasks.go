package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"skald/internal/domain"
)

const taskCols = `id, project_id, feature_id, title, description, acceptance_criteria, status, priority, weight, position,
  assignee, blocked_reason, idempotency_key, version, started_at, completed_at, created_at, updated_at, tags`

func scanTask(sc scanner) (domain.Task, error) {
	var (
		t                                     domain.Task
		desc, accept, assignee, blocked, idem sql.NullString
		status                                string
		started, completed                    sql.NullString
		created, updated, tags                string
	)
	if err := sc.Scan(&t.ID, &t.ProjectID, &t.FeatureID, &t.Title, &desc, &accept, &status, &t.Priority, &t.Weight, &t.Position,
		&assignee, &blocked, &idem, &t.Version, &started, &completed, &created, &updated, &tags); err != nil {
		return domain.Task{}, err
	}
	t.Description, t.AcceptanceCriteria, t.Status = desc.String, accept.String, domain.TaskStatus(status)
	t.Assignee, t.BlockedReason, t.IdempotencyKey = assignee.String, blocked.String, idem.String
	if err := json.Unmarshal([]byte(tags), &t.Tags); err != nil {
		return domain.Task{}, fmt.Errorf("parse stored tags %q: %w", tags, err)
	}
	if t.Tags == nil {
		t.Tags = []string{}
	}
	var err error
	if t.StartedAt, err = parseNullTime(started); err != nil {
		return domain.Task{}, err
	}
	if t.CompletedAt, err = parseNullTime(completed); err != nil {
		return domain.Task{}, err
	}
	if t.CreatedAt, err = parseTime(created); err != nil {
		return domain.Task{}, err
	}
	if t.UpdatedAt, err = parseTime(updated); err != nil {
		return domain.Task{}, err
	}
	return t, nil
}

// TaskInput is the data for a new task. Zero values mean defaults: Priority
// nil is 2 (normal), Weight 0 is 1, Position 0 is the next free position in
// the feature. (Priority is a pointer because 0 is a real priority, urgent.)
type TaskInput struct {
	ProjectID          int64
	FeatureID          int64
	Title              string
	Description        string
	AcceptanceCriteria string
	Priority           *int
	Weight             int
	Position           int
	IdempotencyKey     string   // optional; repeating a key in a project returns the existing task
	Tags               []string // normalized by domain.NormalizeTags
}

// CreateTask inserts a task in status todo and appends task.created. When
// IdempotencyKey is set and a task with that key already exists in the
// project, that task is returned with created == false and nothing is written.
func CreateTask(ctx context.Context, tx *sql.Tx, m Meta, in TaskInput) (task domain.Task, created bool, err error) {
	if err := m.check(); err != nil {
		return domain.Task{}, false, err
	}
	if in.IdempotencyKey != "" {
		existing, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE project_id = ? AND idempotency_key = ?`, in.ProjectID, in.IdempotencyKey))
		if err == nil {
			return existing, false, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return domain.Task{}, false, err
		}
	}
	if err := requireText("title", in.Title); err != nil {
		return domain.Task{}, false, err
	}
	priority := 2
	if in.Priority != nil {
		priority = *in.Priority
	}
	if err := validateRange("priority", priority, 0, 3); err != nil {
		return domain.Task{}, false, err
	}
	if in.Weight == 0 {
		in.Weight = 1
	}
	if err := validateRange("weight", in.Weight, 1, 10); err != nil {
		return domain.Task{}, false, err
	}
	tags, err := encodeTags(in.Tags)
	if err != nil {
		return domain.Task{}, false, err
	}
	if _, err := GetProject(ctx, tx, in.ProjectID); err != nil {
		return domain.Task{}, false, err
	}
	feat, err := GetFeature(ctx, tx, in.FeatureID)
	if err != nil {
		return domain.Task{}, false, err
	}
	if feat.ProjectID != in.ProjectID {
		return domain.Task{}, false, &domain.ValidationError{Field: "feature", Problem: domain.FormatID(domain.EntityFeature, feat.ID) + " belongs to a different project", Hint: "Use a feature from the same project."}
	}
	if in.Position == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), 0) + 1 FROM tasks WHERE feature_id = ?`, in.FeatureID).Scan(&in.Position); err != nil {
			return domain.Task{}, false, err
		}
	}
	now := formatTime(m.now())
	res, err := tx.ExecContext(ctx,
		`INSERT INTO tasks (project_id, feature_id, title, description, acceptance_criteria, status, priority, weight, position, idempotency_key, tags, version, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 'todo', ?, ?, ?, ?, ?, 1, ?, ?)`,
		in.ProjectID, in.FeatureID, in.Title, nullStr(in.Description), nullStr(in.AcceptanceCriteria),
		priority, in.Weight, in.Position, nullStr(in.IdempotencyKey), tags, now, now)
	if err != nil {
		return domain.Task{}, false, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return domain.Task{}, false, err
	}
	payload := map[string]any{"title": in.Title, "feature_id": in.FeatureID}
	if len(in.Tags) > 0 {
		payload["tags"] = in.Tags
	}
	if _, err := appendEvent(ctx, tx, m, in.ProjectID, domain.EntityTask, id, domain.EventTaskCreated, payload); err != nil {
		return domain.Task{}, false, err
	}
	t, err := GetTask(ctx, tx, id)
	return t, true, err
}

// GetTask returns the task with the given ID or a NotFoundError.
func GetTask(ctx context.Context, q Querier, id int64) (domain.Task, error) {
	t, err := scanTask(q.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Task{}, &domain.NotFoundError{Entity: domain.EntityTask, ID: id}
	}
	return t, err
}

// GetTaskInProject is GetTask but reports NotFound when the task belongs to a
// different project, so one project's session cannot touch another's tasks.
func GetTaskInProject(ctx context.Context, q Querier, projectID, id int64) (domain.Task, error) {
	t, err := GetTask(ctx, q, id)
	if err != nil {
		return domain.Task{}, err
	}
	if t.ProjectID != projectID {
		return domain.Task{}, &domain.NotFoundError{Entity: domain.EntityTask, ID: id}
	}
	return t, nil
}

// TaskFilter selects tasks for ListTasks. ProjectID is required. A zero
// Limit means no limit (the service layer applies caps).
type TaskFilter struct {
	ProjectID int64
	FeatureID int64 // 0 = any
	Statuses  []domain.TaskStatus
	ReadyOnly bool     // todo tasks whose dependencies are all done or cancelled
	Tags      []string // tasks carrying every one of these normalized tags
	Limit     int
	Offset    int
}

// ListTasks returns tasks ordered by priority (urgent first), then feature
// position, then task position, then ID.
func ListTasks(ctx context.Context, q Querier, f TaskFilter) ([]domain.Task, error) {
	var where []string
	var args []any
	where = append(where, "t.project_id = ?")
	args = append(args, f.ProjectID)
	if f.FeatureID != 0 {
		where = append(where, "t.feature_id = ?")
		args = append(args, f.FeatureID)
	}
	if len(f.Statuses) > 0 {
		marks := make([]string, len(f.Statuses))
		for i, s := range f.Statuses {
			if !s.Valid() {
				return nil, &domain.ValidationError{Field: "status", Problem: "unknown status " + string(s), Hint: "Use one of: todo, in_progress, blocked, done, cancelled."}
			}
			marks[i] = "?"
			args = append(args, string(s))
		}
		where = append(where, "t.status IN ("+strings.Join(marks, ",")+")")
	}
	if f.ReadyOnly {
		where = append(where, `t.status = 'todo' AND NOT EXISTS (
			SELECT 1 FROM task_dependencies d JOIN tasks p ON p.id = d.depends_on_id
			WHERE d.task_id = t.id AND p.status NOT IN ('done', 'cancelled'))`)
	}
	for _, tag := range f.Tags {
		where = append(where, "EXISTS (SELECT 1 FROM json_each(t.tags) WHERE json_each.value = ?)")
		args = append(args, tag)
	}
	query := `SELECT ` + prefixCols("t.", taskCols) + ` FROM tasks t JOIN features f ON f.id = t.feature_id WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY t.priority, f.position, t.position, t.id`
	if f.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, f.Limit, f.Offset)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func prefixCols(prefix, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = prefix + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// TaskStatusChange describes a status change. It does not validate the
// transition: the service runs domain.CheckTransition first, in the same write
// transaction, and this function applies the result and its side effects.
type TaskStatusChange struct {
	TaskID          int64
	To              domain.TaskStatus
	ExpectedVersion int64  // 0 = no optimistic check
	BlockedReason   string // stored when To is blocked, cleared otherwise
	Assignee        string // optional override; starting a task defaults to the actor
	Note            string // recorded in the event only
	Forced          bool   // recorded in the event when a human overrode the evidence gate
}

// SetTaskStatus changes a task's status, bumps its version and appends
// task.status_changed. Side effects: blocked_reason is kept only while
// blocked; completed_at is set on done and cleared otherwise; started_at is
// set on the first move to in_progress and cleared when the task goes back to
// todo (which also clears the assignee). A version mismatch returns a
// ConflictError carrying the current version.
func SetTaskStatus(ctx context.Context, tx *sql.Tx, m Meta, c TaskStatusChange) (domain.Task, error) {
	if err := m.check(); err != nil {
		return domain.Task{}, err
	}
	if !c.To.Valid() {
		return domain.Task{}, &domain.ValidationError{Field: "status", Problem: "unknown status " + string(c.To), Hint: "Use one of: todo, in_progress, blocked, done, cancelled."}
	}
	cur, err := GetTask(ctx, tx, c.TaskID)
	if err != nil {
		return domain.Task{}, err
	}
	if c.ExpectedVersion > 0 && c.ExpectedVersion != cur.Version {
		return domain.Task{}, &domain.ConflictError{Entity: domain.EntityTask, ID: cur.ID, Expected: c.ExpectedVersion, Current: cur.Version, CurrentTask: &cur}
	}
	now := m.now()
	started, assignee := cur.StartedAt, cur.Assignee
	var completed *time.Time
	switch c.To {
	case domain.TaskInProgress:
		if started == nil {
			started = &now
		}
		if c.Assignee != "" {
			assignee = c.Assignee
		} else if assignee == "" {
			assignee = string(m.Actor)
		}
	case domain.TaskTodo:
		started, assignee = nil, ""
	case domain.TaskDone:
		completed = &now
		if c.Assignee != "" {
			assignee = c.Assignee
		}
	}
	reason := ""
	if c.To == domain.TaskBlocked {
		reason = c.BlockedReason
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = ?, blocked_reason = ?, assignee = ?, started_at = ?, completed_at = ?, version = version + 1, updated_at = ?
		 WHERE id = ? AND version = ?`,
		string(c.To), nullStr(reason), nullStr(assignee), nullTime(started), nullTime(completed), formatTime(now), cur.ID, cur.Version)
	if err != nil {
		return domain.Task{}, mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.Task{}, &domain.ConflictError{Entity: domain.EntityTask, ID: cur.ID, Expected: cur.Version, Current: cur.Version + 1}
	}
	payload := map[string]any{"from": cur.Status, "to": c.To, "version": cur.Version + 1}
	if reason != "" {
		payload["reason"] = reason
	}
	if c.Note != "" {
		payload["note"] = c.Note
	}
	if c.Forced {
		payload["forced"] = true
	}
	if _, err := appendEvent(ctx, tx, m, cur.ProjectID, domain.EntityTask, cur.ID, domain.EventTaskStatus, payload); err != nil {
		return domain.Task{}, err
	}
	return GetTask(ctx, tx, cur.ID)
}

// TaskPatch lists the fields to change; nil fields are left alone.
type TaskPatch struct {
	Title              *string
	Description        *string
	AcceptanceCriteria *string
	Priority           *int
	Weight             *int
	Position           *int
	FeatureID          *int64    // must be a feature of the same project
	Tags               *[]string // replaces the set; normalized by domain.NormalizeTags
}

// UpdateTaskFields applies a patch, bumps the version and appends
// task.updated with the changed field names. expectedVersion 0 skips the
// optimistic check. An empty patch is a no-op without an event.
func UpdateTaskFields(ctx context.Context, tx *sql.Tx, m Meta, id, expectedVersion int64, p TaskPatch) (domain.Task, error) {
	if err := m.check(); err != nil {
		return domain.Task{}, err
	}
	cur, err := GetTask(ctx, tx, id)
	if err != nil {
		return domain.Task{}, err
	}
	if expectedVersion > 0 && expectedVersion != cur.Version {
		return domain.Task{}, &domain.ConflictError{Entity: domain.EntityTask, ID: id, Expected: expectedVersion, Current: cur.Version, CurrentTask: &cur}
	}
	var sets []string
	var args []any
	var changed []string
	set := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
		changed = append(changed, col)
	}
	if p.Title != nil {
		if err := requireText("title", *p.Title); err != nil {
			return domain.Task{}, err
		}
		set("title", *p.Title)
	}
	if p.Description != nil {
		set("description", nullStr(*p.Description))
	}
	if p.AcceptanceCriteria != nil {
		set("acceptance_criteria", nullStr(*p.AcceptanceCriteria))
	}
	if p.Priority != nil {
		if err := validateRange("priority", *p.Priority, 0, 3); err != nil {
			return domain.Task{}, err
		}
		set("priority", *p.Priority)
	}
	if p.Weight != nil {
		if err := validateRange("weight", *p.Weight, 1, 10); err != nil {
			return domain.Task{}, err
		}
		set("weight", *p.Weight)
	}
	if p.Position != nil {
		set("position", *p.Position)
	}
	if p.FeatureID != nil {
		feat, err := GetFeature(ctx, tx, *p.FeatureID)
		if err != nil {
			return domain.Task{}, err
		}
		if feat.ProjectID != cur.ProjectID {
			return domain.Task{}, &domain.ValidationError{Field: "feature", Problem: domain.FormatID(domain.EntityFeature, feat.ID) + " belongs to a different project", Hint: "Use a feature from the same project."}
		}
		set("feature_id", feat.ID)
	}
	var newTags json.RawMessage
	if p.Tags != nil {
		tags, err := encodeTags(*p.Tags)
		if err != nil {
			return domain.Task{}, err
		}
		set("tags", tags)
		newTags = json.RawMessage(tags)
	}
	if len(sets) == 0 {
		return cur, nil
	}
	sets = append(sets, "version = version + 1", "updated_at = ?")
	args = append(args, formatTime(m.now()), id)
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return domain.Task{}, mapErr(err)
	}
	payload := map[string]any{"fields": changed, "version": cur.Version + 1}
	if newTags != nil {
		payload["tags"] = newTags
	}
	if _, err := appendEvent(ctx, tx, m, cur.ProjectID, domain.EntityTask, id, domain.EventTaskUpdated, payload); err != nil {
		return domain.Task{}, err
	}
	return GetTask(ctx, tx, id)
}

// AddTaskNote records a progress note as a task.note event and touches the
// task's updated_at so stale detection restarts. It does not bump the version,
// so notes never cause optimistic-concurrency conflicts.
func AddTaskNote(ctx context.Context, tx *sql.Tx, m Meta, taskID int64, text string) (int64, error) {
	if err := m.check(); err != nil {
		return 0, err
	}
	if err := requireText("note", text); err != nil {
		return 0, err
	}
	t, err := GetTask(ctx, tx, taskID)
	if err != nil {
		return 0, err
	}
	if err := touchTask(ctx, tx, m, t.ID); err != nil {
		return 0, err
	}
	return appendEvent(ctx, tx, m, t.ProjectID, domain.EntityTask, t.ID, domain.EventTaskNote, map[string]any{"text": text})
}

// touchTask sets updated_at without changing the version.
func touchTask(ctx context.Context, tx *sql.Tx, m Meta, taskID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE id = ?`, formatTime(m.now()), taskID)
	return mapErr(err)
}

// encodeTags normalizes tags and returns the JSON stored in tasks.tags.
func encodeTags(tags []string) (string, error) {
	norm, err := domain.NormalizeTags(tags)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(norm)
	return string(b), err
}

// ProjectTagCounts returns how many tasks of the project carry each tag.
func ProjectTagCounts(ctx context.Context, q Querier, projectID int64) (map[string]int, error) {
	rows, err := q.QueryContext(ctx, `SELECT j.value, COUNT(*) FROM tasks t, json_each(t.tags) j
		WHERE t.project_id = ? GROUP BY j.value`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var tag string
		var n int
		if err := rows.Scan(&tag, &n); err != nil {
			return nil, err
		}
		out[tag] = n
	}
	return out, rows.Err()
}
