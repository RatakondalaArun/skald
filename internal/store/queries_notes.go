package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"skald/internal/domain"
)

// MaxNoteBody caps a note's length in characters. The scratchpad is for
// notes, not files; longer text belongs in a document.
const MaxNoteBody = 20000

// noteEventText caps the copy of the body kept in the note.added event, so
// the change feed stays small. The full body is in the notes table.
const noteEventText = 280

const noteCols = `id, project_id, task_id, feature_id, body, actor, created_at`

func scanNote(sc scanner) (domain.Note, error) {
	var (
		n              domain.Note
		task, feature  sql.NullInt64
		actor, created string
	)
	if err := sc.Scan(&n.ID, &n.ProjectID, &task, &feature, &n.Body, &actor, &created); err != nil {
		return domain.Note{}, err
	}
	n.TaskID, n.FeatureID, n.Actor = task.Int64, feature.Int64, domain.Actor(actor)
	var err error
	n.CreatedAt, err = parseTime(created)
	return n, err
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// AddNote appends a scratchpad note to a project and appends note.added. A
// task link must be a task of the same project and implies its feature; a
// feature given alongside a task must be that task's feature. The note's
// actor is m.Actor. Nothing else is touched: notes do not bump a task's
// version or updated_at, and never affect progress.
func AddNote(ctx context.Context, tx *sql.Tx, m Meta, n domain.Note) (domain.Note, error) {
	if err := m.check(); err != nil {
		return domain.Note{}, err
	}
	body := strings.TrimSpace(n.Body)
	if err := requireText("body", body); err != nil {
		return domain.Note{}, err
	}
	if c := utf8.RuneCountInString(body); c > MaxNoteBody {
		return domain.Note{}, &domain.ValidationError{Field: "body", Problem: fmt.Sprintf("is %d characters, the limit is %d", c, MaxNoteBody), Hint: "Split it into several notes, or keep long text in a document (put_document)."}
	}
	if _, err := GetProject(ctx, tx, n.ProjectID); err != nil {
		return domain.Note{}, err
	}
	if n.TaskID != 0 {
		t, err := GetTaskInProject(ctx, tx, n.ProjectID, n.TaskID)
		if err != nil {
			return domain.Note{}, err
		}
		if n.FeatureID != 0 && n.FeatureID != t.FeatureID {
			return domain.Note{}, &domain.ValidationError{Field: "feature",
				Problem: fmt.Sprintf("%s belongs to %s, not %s", domain.FormatID(domain.EntityTask, t.ID), domain.FormatID(domain.EntityFeature, t.FeatureID), domain.FormatID(domain.EntityFeature, n.FeatureID)),
				Hint:    "Pass only the task; its feature is linked automatically."}
		}
		n.FeatureID = t.FeatureID
	} else if n.FeatureID != 0 {
		f, err := GetFeature(ctx, tx, n.FeatureID)
		if err != nil {
			return domain.Note{}, err
		}
		if f.ProjectID != n.ProjectID {
			return domain.Note{}, &domain.NotFoundError{Entity: domain.EntityFeature, ID: n.FeatureID}
		}
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO notes (project_id, task_id, feature_id, body, actor, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		n.ProjectID, nullID(n.TaskID), nullID(n.FeatureID), body, string(m.Actor), formatTime(m.now()))
	if err != nil {
		return domain.Note{}, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return domain.Note{}, err
	}
	payload := map[string]any{"text": clipRunes(body, noteEventText)}
	if n.TaskID != 0 {
		payload["task_id"] = n.TaskID
	}
	if n.FeatureID != 0 {
		payload["feature_id"] = n.FeatureID
	}
	if _, err := appendEvent(ctx, tx, m, n.ProjectID, domain.EntityNote, id, domain.EventNoteAdded, payload); err != nil {
		return domain.Note{}, err
	}
	return GetNote(ctx, tx, id)
}

// GetNote returns one note.
func GetNote(ctx context.Context, q Querier, id int64) (domain.Note, error) {
	n, err := scanNote(q.QueryRowContext(ctx, `SELECT `+noteCols+` FROM notes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Note{}, &domain.NotFoundError{Entity: domain.EntityNote, ID: id}
	}
	return n, err
}

// NoteFilter selects notes for ListNotes. Zero values mean no filter.
type NoteFilter struct {
	ProjectID int64 // required
	TaskID    int64
	FeatureID int64
	Actor     domain.Actor
	// Query keeps notes whose body contains it (case-insensitive for ASCII).
	Query  string
	Limit  int // 0 means no limit
	Offset int
}

// ListNotes returns a project's notes, newest first.
func ListNotes(ctx context.Context, q Querier, f NoteFilter) ([]domain.Note, error) {
	where := []string{"project_id = ?"}
	args := []any{f.ProjectID}
	if f.TaskID != 0 {
		where, args = append(where, "task_id = ?"), append(args, f.TaskID)
	}
	if f.FeatureID != 0 {
		where, args = append(where, "feature_id = ?"), append(args, f.FeatureID)
	}
	if f.Actor != "" {
		where, args = append(where, "actor = ?"), append(args, string(f.Actor))
	}
	if s := strings.TrimSpace(f.Query); s != "" {
		where, args = append(where, `body LIKE ? ESCAPE '\'`), append(args, "%"+likeEscaper.Replace(s)+"%")
	}
	query := `SELECT ` + noteCols + ` FROM notes WHERE ` + strings.Join(where, " AND ") + ` ORDER BY id DESC`
	if f.Limit > 0 {
		query += fmt.Sprintf(` LIMIT %d OFFSET %d`, f.Limit, f.Offset)
	}
	return collect(ctx, q, scanNote, query, args...)
}

// CountNotes returns how many notes a project has.
func CountNotes(ctx context.Context, q Querier, projectID int64) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM notes WHERE project_id = ?`, projectID).Scan(&n)
	return n, err
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// clipRunes shortens s to at most n runes, adding an ellipsis.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
