package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"skald/internal/domain"
)

const featureCols = `id, project_id, title, description, status, weight, position, created_at, updated_at`

func scanFeature(sc scanner) (domain.Feature, error) {
	var (
		f                domain.Feature
		desc             sql.NullString
		status           string
		created, updated string
	)
	if err := sc.Scan(&f.ID, &f.ProjectID, &f.Title, &desc, &status, &f.Weight, &f.Position, &created, &updated); err != nil {
		return domain.Feature{}, err
	}
	f.Description, f.Status = desc.String, domain.FeatureStatus(status)
	var err error
	if f.CreatedAt, err = parseTime(created); err != nil {
		return domain.Feature{}, err
	}
	if f.UpdatedAt, err = parseTime(updated); err != nil {
		return domain.Feature{}, err
	}
	return f, nil
}

// CreateFeature inserts a feature and appends feature.created. A zero Weight
// becomes 1 and a zero Position becomes the next free position in the
// project. Status is always stored as planned.
func CreateFeature(ctx context.Context, tx *sql.Tx, m Meta, f domain.Feature) (domain.Feature, error) {
	if err := m.check(); err != nil {
		return domain.Feature{}, err
	}
	if err := requireText("title", f.Title); err != nil {
		return domain.Feature{}, err
	}
	if f.Weight == 0 {
		f.Weight = 1
	}
	if err := validateRange("weight", f.Weight, 1, 10); err != nil {
		return domain.Feature{}, err
	}
	if _, err := GetProject(ctx, tx, f.ProjectID); err != nil {
		return domain.Feature{}, err
	}
	if f.Position == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), 0) + 1 FROM features WHERE project_id = ?`, f.ProjectID).Scan(&f.Position); err != nil {
			return domain.Feature{}, err
		}
	}
	now := formatTime(m.now())
	res, err := tx.ExecContext(ctx,
		`INSERT INTO features (project_id, title, description, status, weight, position, created_at, updated_at) VALUES (?, ?, ?, 'planned', ?, ?, ?, ?)`,
		f.ProjectID, f.Title, nullStr(f.Description), f.Weight, f.Position, now, now)
	if err != nil {
		return domain.Feature{}, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return domain.Feature{}, err
	}
	if _, err := appendEvent(ctx, tx, m, f.ProjectID, domain.EntityFeature, id, domain.EventFeatureCreated,
		map[string]any{"title": f.Title, "weight": f.Weight}); err != nil {
		return domain.Feature{}, err
	}
	return GetFeature(ctx, tx, id)
}

// GetFeature returns the feature with the given ID or a NotFoundError.
func GetFeature(ctx context.Context, q Querier, id int64) (domain.Feature, error) {
	f, err := scanFeature(q.QueryRowContext(ctx, `SELECT `+featureCols+` FROM features WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Feature{}, &domain.NotFoundError{Entity: domain.EntityFeature, ID: id}
	}
	return f, err
}

// ListFeatures returns a project's features ordered by position, then ID.
func ListFeatures(ctx context.Context, q Querier, projectID int64) ([]domain.Feature, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+featureCols+` FROM features WHERE project_id = ? ORDER BY position, id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Feature
	for rows.Next() {
		f, err := scanFeature(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FeaturePatch lists the fields to change; nil fields are left alone.
type FeaturePatch struct {
	Title       *string
	Description *string
	Weight      *int
	Position    *int
}

// UpdateFeature applies a patch and appends feature.updated with the names of
// the changed fields. An empty patch is a no-op without an event.
func UpdateFeature(ctx context.Context, tx *sql.Tx, m Meta, id int64, p FeaturePatch) (domain.Feature, error) {
	if err := m.check(); err != nil {
		return domain.Feature{}, err
	}
	cur, err := GetFeature(ctx, tx, id)
	if err != nil {
		return domain.Feature{}, err
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
			return domain.Feature{}, err
		}
		set("title", *p.Title)
	}
	if p.Description != nil {
		set("description", nullStr(*p.Description))
	}
	if p.Weight != nil {
		if err := validateRange("weight", *p.Weight, 1, 10); err != nil {
			return domain.Feature{}, err
		}
		set("weight", *p.Weight)
	}
	if p.Position != nil {
		set("position", *p.Position)
	}
	if len(sets) == 0 {
		return cur, nil
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, formatTime(m.now()), id)
	if _, err := tx.ExecContext(ctx, `UPDATE features SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return domain.Feature{}, mapErr(err)
	}
	if _, err := appendEvent(ctx, tx, m, cur.ProjectID, domain.EntityFeature, id, domain.EventFeatureUpdated, map[string]any{"fields": changed}); err != nil {
		return domain.Feature{}, err
	}
	return GetFeature(ctx, tx, id)
}

// CancelFeature stores the cancelled status and appends feature.cancelled. It
// does not touch the feature's tasks; the service decides what to do with
// them. Cancelling an already cancelled feature is a no-op without an event.
func CancelFeature(ctx context.Context, tx *sql.Tx, m Meta, id int64) (domain.Feature, error) {
	if err := m.check(); err != nil {
		return domain.Feature{}, err
	}
	cur, err := GetFeature(ctx, tx, id)
	if err != nil {
		return domain.Feature{}, err
	}
	if cur.Status == domain.FeatureCancelled {
		return cur, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE features SET status = 'cancelled', updated_at = ? WHERE id = ?`, formatTime(m.now()), id); err != nil {
		return domain.Feature{}, mapErr(err)
	}
	if _, err := appendEvent(ctx, tx, m, cur.ProjectID, domain.EntityFeature, id, domain.EventFeatureCancelled, nil); err != nil {
		return domain.Feature{}, err
	}
	return GetFeature(ctx, tx, id)
}
