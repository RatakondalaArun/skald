package store

import (
	"context"
	"database/sql"
	"errors"

	"skald/internal/domain"
)

const documentCols = `id, project_id, kind, title, current_version`

func scanDocument(sc scanner) (domain.Document, error) {
	var d domain.Document
	err := sc.Scan(&d.ID, &d.ProjectID, &d.Kind, &d.Title, &d.CurrentVersion)
	return d, err
}

func scanDocumentVersion(sc scanner) (domain.DocumentVersion, error) {
	var (
		v              domain.DocumentVersion
		actor, created string
	)
	if err := sc.Scan(&v.DocumentID, &v.Version, &v.Body, &actor, &created); err != nil {
		return domain.DocumentVersion{}, err
	}
	v.Actor = domain.Actor(actor)
	var err error
	v.CreatedAt, err = parseTime(created)
	return v, err
}

// PutDocument creates the document (kind + title are unique per project) or
// adds a new version to it, and appends document.version_added. Versions start
// at 1 and old versions stay readable.
func PutDocument(ctx context.Context, tx *sql.Tx, m Meta, projectID int64, kind, title, body string) (domain.Document, domain.DocumentVersion, error) {
	if err := m.check(); err != nil {
		return domain.Document{}, domain.DocumentVersion{}, err
	}
	for field, v := range map[string]string{"kind": kind, "title": title, "body": body} {
		if err := requireText(field, v); err != nil {
			return domain.Document{}, domain.DocumentVersion{}, err
		}
	}
	if _, err := GetProject(ctx, tx, projectID); err != nil {
		return domain.Document{}, domain.DocumentVersion{}, err
	}
	doc, err := scanDocument(tx.QueryRowContext(ctx, `SELECT `+documentCols+` FROM documents WHERE project_id = ? AND kind = ? AND title = ?`, projectID, kind, title))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.ExecContext(ctx, `INSERT INTO documents (project_id, kind, title, current_version) VALUES (?, ?, ?, 1)`, projectID, kind, title)
		if err != nil {
			return domain.Document{}, domain.DocumentVersion{}, mapErr(err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return domain.Document{}, domain.DocumentVersion{}, err
		}
		doc = domain.Document{ID: id, ProjectID: projectID, Kind: kind, Title: title, CurrentVersion: 1}
	case err != nil:
		return domain.Document{}, domain.DocumentVersion{}, err
	default:
		doc.CurrentVersion++
		if _, err := tx.ExecContext(ctx, `UPDATE documents SET current_version = ? WHERE id = ?`, doc.CurrentVersion, doc.ID); err != nil {
			return domain.Document{}, domain.DocumentVersion{}, mapErr(err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO document_versions (document_id, version, body, actor, created_at) VALUES (?, ?, ?, ?, ?)`,
		doc.ID, doc.CurrentVersion, body, string(m.Actor), formatTime(m.now())); err != nil {
		return domain.Document{}, domain.DocumentVersion{}, mapErr(err)
	}
	if _, err := appendEvent(ctx, tx, m, projectID, domain.EntityDocument, doc.ID, domain.EventDocumentVersion,
		map[string]any{"kind": kind, "title": title, "version": doc.CurrentVersion}); err != nil {
		return domain.Document{}, domain.DocumentVersion{}, err
	}
	v, err := GetDocumentVersion(ctx, tx, doc.ID, doc.CurrentVersion)
	return doc, v, err
}

// GetDocument returns a document and its current version.
func GetDocument(ctx context.Context, q Querier, projectID int64, kind, title string) (domain.Document, domain.DocumentVersion, error) {
	doc, err := scanDocument(q.QueryRowContext(ctx, `SELECT `+documentCols+` FROM documents WHERE project_id = ? AND kind = ? AND title = ?`, projectID, kind, title))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Document{}, domain.DocumentVersion{}, &domain.NotFoundError{Entity: domain.EntityDocument, Ref: kind + "/" + title}
	}
	if err != nil {
		return domain.Document{}, domain.DocumentVersion{}, err
	}
	v, err := GetDocumentVersion(ctx, q, doc.ID, doc.CurrentVersion)
	return doc, v, err
}

// GetDocumentByID returns a document and its current version.
func GetDocumentByID(ctx context.Context, q Querier, id int64) (domain.Document, domain.DocumentVersion, error) {
	doc, err := scanDocument(q.QueryRowContext(ctx, `SELECT `+documentCols+` FROM documents WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Document{}, domain.DocumentVersion{}, &domain.NotFoundError{Entity: domain.EntityDocument, ID: id}
	}
	if err != nil {
		return domain.Document{}, domain.DocumentVersion{}, err
	}
	v, err := GetDocumentVersion(ctx, q, doc.ID, doc.CurrentVersion)
	return doc, v, err
}

// GetDocumentVersion returns one version of a document.
func GetDocumentVersion(ctx context.Context, q Querier, documentID int64, version int) (domain.DocumentVersion, error) {
	v, err := scanDocumentVersion(q.QueryRowContext(ctx,
		`SELECT document_id, version, body, actor, created_at FROM document_versions WHERE document_id = ? AND version = ?`, documentID, version))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DocumentVersion{}, &domain.NotFoundError{Entity: domain.EntityDocument, ID: documentID}
	}
	return v, err
}

// ListDocuments returns a project's documents ordered by kind then title.
// An empty kind lists every kind. Bodies are not loaded.
func ListDocuments(ctx context.Context, q Querier, projectID int64, kind string) ([]domain.Document, error) {
	query := `SELECT ` + documentCols + ` FROM documents WHERE project_id = ?`
	args := []any{projectID}
	if kind != "" {
		query += ` AND kind = ?`
		args = append(args, kind)
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY kind, title`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Document
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
