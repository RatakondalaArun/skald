package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"skald/internal/domain"
)

const projectCols = `id, slug, name, description, status, repo_path, settings, created_at, updated_at`

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func scanProject(sc scanner) (domain.Project, error) {
	var (
		p                domain.Project
		desc, repo       sql.NullString
		status, settings string
		created, updated string
	)
	if err := sc.Scan(&p.ID, &p.Slug, &p.Name, &desc, &status, &repo, &settings, &created, &updated); err != nil {
		return domain.Project{}, err
	}
	p.Description, p.RepoPath, p.Status = desc.String, repo.String, domain.ProjectStatus(status)
	var err error
	if p.Settings, err = domain.ParseSettings(settings); err != nil {
		return domain.Project{}, err
	}
	if p.CreatedAt, err = parseTime(created); err != nil {
		return domain.Project{}, err
	}
	if p.UpdatedAt, err = parseTime(updated); err != nil {
		return domain.Project{}, err
	}
	return p, nil
}

// CreateProject inserts a project and appends project.created. Slug must be
// lowercase letters, digits and dashes. p.Settings is ignored: a new project
// always starts with domain.DefaultSettings, and callers change them with
// UpdateProjectSettings (the zero value would otherwise silently turn the
// evidence gate off).
func CreateProject(ctx context.Context, tx *sql.Tx, m Meta, p domain.Project) (domain.Project, error) {
	if err := m.check(); err != nil {
		return domain.Project{}, err
	}
	p.Slug = strings.TrimSpace(p.Slug)
	if !slugRE.MatchString(p.Slug) {
		return domain.Project{}, &domain.ValidationError{Field: "slug", Problem: "must be lowercase letters, digits and dashes, starting with a letter or digit", Hint: "For example \"school-management\"."}
	}
	if err := requireText("name", p.Name); err != nil {
		return domain.Project{}, err
	}
	settings, err := json.Marshal(domain.DefaultSettings())
	if err != nil {
		return domain.Project{}, err
	}
	now := formatTime(m.now())
	res, err := tx.ExecContext(ctx,
		`INSERT INTO projects (slug, name, description, status, repo_path, settings, created_at, updated_at) VALUES (?, ?, ?, 'active', ?, ?, ?, ?)`,
		p.Slug, p.Name, nullStr(p.Description), nullStr(p.RepoPath), string(settings), now, now)
	if err != nil {
		return domain.Project{}, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return domain.Project{}, err
	}
	if _, err := appendEvent(ctx, tx, m, id, domain.EntityProject, id, domain.EventProjectCreated,
		map[string]any{"slug": p.Slug, "name": p.Name, "repo_path": p.RepoPath}); err != nil {
		return domain.Project{}, err
	}
	return GetProject(ctx, tx, id)
}

// GetProject returns the project with the given ID or a NotFoundError.
func GetProject(ctx context.Context, q Querier, id int64) (domain.Project, error) {
	p, err := scanProject(q.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Project{}, &domain.NotFoundError{Entity: domain.EntityProject, ID: id}
	}
	return p, err
}

// GetProjectBySlug returns the project with the given slug or a NotFoundError.
func GetProjectBySlug(ctx context.Context, q Querier, slug string) (domain.Project, error) {
	p, err := scanProject(q.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects WHERE slug = ?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Project{}, &domain.NotFoundError{Entity: domain.EntityProject, Ref: slug}
	}
	return p, err
}

// GetProjectByRepoPath returns the project linked to an exact repo path.
func GetProjectByRepoPath(ctx context.Context, q Querier, repoPath string) (domain.Project, error) {
	p, err := scanProject(q.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects WHERE repo_path = ?`, repoPath))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Project{}, &domain.NotFoundError{Entity: domain.EntityProject, Ref: repoPath}
	}
	return p, err
}

// ListProjects returns projects ordered by slug. Archived ones are included
// only when includeArchived is true.
func ListProjects(ctx context.Context, q Querier, includeArchived bool) ([]domain.Project, error) {
	query := `SELECT ` + projectCols + ` FROM projects`
	if !includeArchived {
		query += ` WHERE status = 'active'`
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ArchiveProject marks a project archived and appends project.archived.
// It also frees the project's repo_path (set to NULL) so the repository can be
// linked to a new project; the old path is kept in a project.updated event
// payload as {"released_repo_path": "..."}. Archiving an already archived
// project is a no-op without an event.
func ArchiveProject(ctx context.Context, tx *sql.Tx, m Meta, id int64) (domain.Project, error) {
	if err := m.check(); err != nil {
		return domain.Project{}, err
	}
	p, err := GetProject(ctx, tx, id)
	if err != nil {
		return domain.Project{}, err
	}
	if p.Status == domain.ProjectArchived {
		return p, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET status = 'archived', repo_path = NULL, updated_at = ? WHERE id = ?`, formatTime(m.now()), id); err != nil {
		return domain.Project{}, mapErr(err)
	}
	if _, err := appendEvent(ctx, tx, m, id, domain.EntityProject, id, domain.EventProjectArchived, nil); err != nil {
		return domain.Project{}, err
	}
	if p.RepoPath != "" {
		if _, err := appendEvent(ctx, tx, m, id, domain.EntityProject, id, domain.EventProjectUpdated, map[string]any{"released_repo_path": p.RepoPath}); err != nil {
			return domain.Project{}, err
		}
	}
	return GetProject(ctx, tx, id)
}

// UpdateProjectSettings replaces a project's settings and appends
// project.updated.
func UpdateProjectSettings(ctx context.Context, tx *sql.Tx, m Meta, id int64, s domain.ProjectSettings) (domain.Project, error) {
	if err := m.check(); err != nil {
		return domain.Project{}, err
	}
	if _, err := GetProject(ctx, tx, id); err != nil {
		return domain.Project{}, err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return domain.Project{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET settings = ?, updated_at = ? WHERE id = ?`, string(b), formatTime(m.now()), id); err != nil {
		return domain.Project{}, mapErr(err)
	}
	if _, err := appendEvent(ctx, tx, m, id, domain.EntityProject, id, domain.EventProjectUpdated, map[string]any{"settings": s}); err != nil {
		return domain.Project{}, err
	}
	return GetProject(ctx, tx, id)
}
