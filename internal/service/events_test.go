package service

import (
	"database/sql"
	"errors"
	"testing"

	"skald/internal/domain"
	"skald/internal/store"
)

func nextCNote(e *nextCEnv, task domain.Task, text string) {
	e.t.Helper()
	e.write(func(tx *sql.Tx) error {
		_, err := store.AddTaskNote(nextCCtx, tx, nextCMeta(domain.Human, 0), task.ID, text)
		return err
	})
}

func TestNextCEventCursorPagesInOrderWithoutGaps(t *testing.T) {
	e := nextCNew(t)
	f := e.feature("F", 1, 1)
	task := e.task(f, "t", 1)
	for i := 0; i < 22; i++ {
		nextCNote(e, task, "note")
	}
	latest, err := e.svc.LatestEventID(nextCCtx, 0)
	if err != nil {
		t.Fatal(err)
	}

	var ids []int64
	cursor, pages := int64(0), 0
	for {
		page, err := e.svc.EventsSince(nextCCtx, 0, cursor, 7)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, ev := range page.Events {
			ids = append(ids, ev.ID)
		}
		cursor = page.Cursor
		if !page.More {
			break
		}
		if len(page.Events) != 7 {
			t.Fatalf("a page with More set must be full, got %d", len(page.Events))
		}
	}
	if int64(len(ids)) != latest {
		t.Fatalf("saw %d events, latest id is %d", len(ids), latest)
	}
	for i, id := range ids {
		if id != int64(i)+1 {
			t.Fatalf("ids = %v: gap or reorder at %d", ids, i)
		}
	}
	if cursor != latest || pages < 4 {
		t.Fatalf("cursor = %d (latest %d), pages = %d", cursor, latest, pages)
	}

	// Caught up: an empty page keeps the cursor and reports nothing more.
	page, err := e.svc.EventsSince(nextCCtx, 0, cursor, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 0 || page.More || page.Cursor != cursor {
		t.Fatalf("empty page = %+v", page)
	}

	// New events show up after the cursor, and only those.
	nextCNote(e, task, "later")
	page, err = e.svc.EventsSince(nextCCtx, 0, cursor, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].ID != latest+1 || page.Cursor != latest+1 {
		t.Fatalf("page = %+v", page)
	}
}

func TestNextCEventsExactPageBoundaryHasNoMore(t *testing.T) {
	e := nextCNew(t)
	latest, _ := e.svc.LatestEventID(nextCCtx, 0) // project creation already wrote one event
	page, err := e.svc.EventsSince(nextCCtx, 0, 0, int(latest))
	if err != nil {
		t.Fatal(err)
	}
	if page.More || int64(len(page.Events)) != latest {
		t.Fatalf("page = %+v, latest = %d", page, latest)
	}
}

func TestNextCEventsProjectScope(t *testing.T) {
	e := nextCNew(t)
	var other domain.Project
	e.write(func(tx *sql.Tx) (err error) {
		other, err = store.CreateProject(nextCCtx, tx, nextCMeta(domain.Human, 0), domain.Project{Slug: "other", Name: "Other"})
		return
	})
	f := e.feature("F", 1, 1)
	e.task(f, "t", 1)

	mine, err := e.svc.EventsSince(nextCCtx, e.p.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range mine.Events {
		if ev.ProjectID != e.p.ID {
			t.Fatalf("event %d belongs to project %d", ev.ID, ev.ProjectID)
		}
	}
	theirs, err := e.svc.EventsSince(nextCCtx, other.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	all, err := e.svc.EventsSince(nextCCtx, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine.Events)+len(theirs.Events) != len(all.Events) || len(theirs.Events) != 1 {
		t.Fatalf("mine=%d theirs=%d all=%d", len(mine.Events), len(theirs.Events), len(all.Events))
	}

	if id, err := e.svc.LatestEventID(nextCCtx, other.ID); err != nil || id != theirs.Events[0].ID {
		t.Fatalf("latest(other) = %d, %v", id, err)
	}
	if id, err := e.svc.LatestEventID(nextCCtx, 0); err != nil || id != all.Events[len(all.Events)-1].ID {
		t.Fatalf("latest(all) = %d, %v", id, err)
	}
}

func TestNextCEventsRecordActor(t *testing.T) {
	e := nextCNew(t)
	f := e.feature("F", 1, 1)
	task := e.task(f, "t", 1)
	agent := domain.NewAgentActor("claude-code", "s1")
	e.write(func(tx *sql.Tx) error {
		_, err := store.AddTaskNote(nextCCtx, tx, nextCMeta(agent, 0), task.ID, "from the agent")
		return err
	})
	page, err := e.svc.EventsSince(nextCCtx, e.p.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	last := page.Events[len(page.Events)-1]
	if last.Type != domain.EventTaskNote || last.Actor != agent {
		t.Fatalf("last event = %+v", last)
	}
}

func TestNextCEventsValidation(t *testing.T) {
	e := nextCNew(t)
	_, err := e.svc.EventsSince(nextCCtx, 0, -1, 10)
	var v *domain.ValidationError
	if !errors.As(err, &v) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
	// Non-positive limits fall back to the default, they are not errors.
	if _, err := e.svc.EventsSince(nextCCtx, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
}
