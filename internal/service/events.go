package service

import (
	"context"

	"skald/internal/domain"
	"skald/internal/store"
)

// Limits for EventsSince. A limit of zero or less means DefaultEventLimit;
// larger values are capped at MaxEventLimit.
const (
	DefaultEventLimit = 100
	MaxEventLimit     = 1000
)

// EventPage is one page of the change feed. Cursor is the value to pass to the
// next EventsSince call: the ID of the last event returned, or the cursor that
// was passed in when there were none. More says further events already exist.
type EventPage struct {
	Events []domain.Event
	Cursor int64
	More   bool
}

// EventsSince returns events with an ID greater than cursor, oldest first. A
// projectID of 0 means every project. Start with cursor 0, or with
// LatestEventID to see only new events.
func (s *Service) EventsSince(ctx context.Context, projectID, cursor int64, limit int) (EventPage, error) {
	if limit <= 0 {
		limit = DefaultEventLimit
	}
	if limit > MaxEventLimit {
		limit = MaxEventLimit
	}
	if cursor < 0 {
		return EventPage{}, &domain.ValidationError{Field: "cursor", Problem: "must not be negative", Hint: "Start from 0, or from LatestEventID."}
	}
	// Ask for one extra event to learn whether another page exists.
	events, err := store.EventsSince(ctx, s.db.Reader(), projectID, cursor, limit+1)
	if err != nil {
		return EventPage{}, store.Classify(s.db.Path(), err)
	}
	page := EventPage{Cursor: cursor}
	if len(events) > limit {
		events, page.More = events[:limit], true
	}
	page.Events = events
	if n := len(events); n > 0 {
		page.Cursor = events[n-1].ID
	}
	return page, nil
}

// LatestEventID returns the newest event ID, or 0 when there are none. A
// projectID of 0 means every project.
func (s *Service) LatestEventID(ctx context.Context, projectID int64) (int64, error) {
	id, err := store.MaxEventID(ctx, s.db.Reader(), projectID)
	return id, store.Classify(s.db.Path(), err)
}
