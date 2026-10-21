package service

import (
	"context"
	"database/sql"

	"skald/internal/domain"
	"skald/internal/store"
)

// Note list sizes. Lists are capped so an agent's context stays lean.
const (
	DefaultNoteLimit = 20
	MaxNoteLimit     = 100
)

// AddNoteInput is the data for AddNote. TaskID and FeatureID are optional
// links; a task implies its feature.
type AddNoteInput struct {
	Body      string
	TaskID    int64
	FeatureID int64
}

// AddNote appends a note to the resolved project's scratchpad. Notes are for
// what an agent wants to remember while it works (findings, open questions,
// where it left off). They are append-only, are recorded with their actor,
// and never change task state or progress.
func (s *Service) AddNote(ctx context.Context, actor domain.Actor, ref ProjectRef, in AddNoteInput) (domain.Note, error) {
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return domain.Note{}, err
	}
	if err := ensureProjectActive(p); err != nil {
		return domain.Note{}, err
	}
	var out domain.Note
	err = s.db.WriteTx(ctx, func(tx *sql.Tx) (err error) {
		out, err = store.AddNote(ctx, tx, s.meta(actor), domain.Note{
			ProjectID: p.ID, TaskID: in.TaskID, FeatureID: in.FeatureID, Body: in.Body,
		})
		return err
	})
	return out, err
}

// GetNote returns one note of the resolved project. A note of another project
// is NotFound.
func (s *Service) GetNote(ctx context.Context, ref ProjectRef, noteID int64) (domain.Note, error) {
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return domain.Note{}, err
	}
	n, err := store.GetNote(ctx, s.db.Reader(), noteID)
	if err == nil && n.ProjectID != p.ID {
		err = &domain.NotFoundError{Entity: domain.EntityNote, ID: noteID}
	}
	return n, store.Classify(s.db.Path(), err)
}

// NoteFilter selects notes for ListNotes. Zero values mean no filter.
type NoteFilter struct {
	TaskID    int64
	FeatureID int64
	Actor     domain.Actor
	Query     string // substring of the body, case-insensitive for ASCII
	Limit     int    // 0 means DefaultNoteLimit; capped at MaxNoteLimit
	Offset    int
}

// NoteList is a page of notes, newest first.
type NoteList struct {
	Project domain.Project
	Notes   []domain.Note
	// Truncated is true when more notes match than were returned.
	Truncated bool
}

// ListNotes lists the resolved project's notes, newest first. A task or
// feature filter must name one of the project's own.
func (s *Service) ListNotes(ctx context.Context, ref ProjectRef, f NoteFilter) (NoteList, error) {
	if f.Limit < 0 || f.Offset < 0 {
		return NoteList{}, &domain.ValidationError{Field: "limit", Problem: "limit and offset must not be negative", Hint: "Use 0 for the default page size."}
	}
	limit := f.Limit
	if limit == 0 {
		limit = DefaultNoteLimit
	}
	if limit > MaxNoteLimit {
		limit = MaxNoteLimit
	}
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return NoteList{}, err
	}
	var notes []domain.Note
	err = s.db.ReadTx(ctx, func(tx *sql.Tx) error {
		if f.TaskID != 0 {
			if _, err := store.GetTaskInProject(ctx, tx, p.ID, f.TaskID); err != nil {
				return err
			}
		}
		if f.FeatureID != 0 {
			feat, err := store.GetFeature(ctx, tx, f.FeatureID)
			if err != nil {
				return err
			}
			if feat.ProjectID != p.ID {
				return &domain.NotFoundError{Entity: domain.EntityFeature, ID: f.FeatureID}
			}
		}
		// One extra row says whether the page was cut short.
		notes, err = store.ListNotes(ctx, tx, store.NoteFilter{
			ProjectID: p.ID, TaskID: f.TaskID, FeatureID: f.FeatureID, Actor: f.Actor,
			Query: f.Query, Limit: limit + 1, Offset: f.Offset,
		})
		return err
	})
	if err != nil {
		return NoteList{}, err
	}
	list := NoteList{Project: p, Notes: notes}
	if len(notes) > limit {
		list.Notes, list.Truncated = notes[:limit], true
	}
	return list, nil
}
