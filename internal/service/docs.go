package service

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	"skald/internal/domain"
	"skald/internal/store"
)

// PutDocument stores body as the next version of the document identified by
// kind and title (for example "architecture" / "Overview"), creating the
// document on first use. Older versions stay readable.
func (s *Service) PutDocument(ctx context.Context, actor domain.Actor, ref ProjectRef, kind, title, body string) (domain.Document, domain.DocumentVersion, error) {
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return domain.Document{}, domain.DocumentVersion{}, err
	}
	kind, title = strings.TrimSpace(kind), strings.TrimSpace(title)
	var (
		doc domain.Document
		ver domain.DocumentVersion
	)
	err = s.db.WriteTx(ctx, func(tx *sql.Tx) (err error) {
		doc, ver, err = store.PutDocument(ctx, tx, s.meta(actor), p.ID, kind, title, body)
		return
	})
	return doc, ver, err
}

// GetDocument returns a document and one of its versions. version 0 means the
// current version; a version that does not exist is a NotFoundError.
func (s *Service) GetDocument(ctx context.Context, ref ProjectRef, kind, title string, version int) (domain.Document, domain.DocumentVersion, error) {
	if version < 0 {
		return domain.Document{}, domain.DocumentVersion{}, &domain.ValidationError{Field: "version", Problem: "must not be negative", Hint: "Use 0 for the current version."}
	}
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return domain.Document{}, domain.DocumentVersion{}, err
	}
	var (
		doc domain.Document
		ver domain.DocumentVersion
	)
	err = s.db.ReadTx(ctx, func(tx *sql.Tx) (err error) {
		doc, ver, err = store.GetDocument(ctx, tx, p.ID, strings.TrimSpace(kind), strings.TrimSpace(title))
		if err != nil || version == 0 || version == doc.CurrentVersion {
			return err
		}
		if version > doc.CurrentVersion {
			return &domain.NotFoundError{Entity: domain.EntityDocument, Ref: doc.Kind + "/" + doc.Title + " version " + strconv.Itoa(version)}
		}
		ver, err = store.GetDocumentVersion(ctx, tx, doc.ID, version)
		return err
	})
	return doc, ver, err
}

// ListDocuments lists a project's documents, ordered by kind then title,
// without bodies. An empty kind lists every kind.
func (s *Service) ListDocuments(ctx context.Context, ref ProjectRef, kind string) ([]domain.Document, error) {
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	return store.ListDocuments(ctx, s.db.Reader(), p.ID, strings.TrimSpace(kind))
}
