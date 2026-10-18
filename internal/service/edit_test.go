package service

import (
	"errors"
	"testing"

	"skald/internal/domain"
)

func TestUpdateTaskEditsFieldsAndRecordsOneEvent(t *testing.T) {
	e := lifeBNew(t)
	task := e.task("Old title")
	since := e.maxEvent()

	title, desc, prio, weight := "New title", "Why it matters", 0, 5
	got, err := e.svc.UpdateTask(lifeBctx, domain.Human, task.ID, TaskEdit{Title: &title, Description: &desc, Priority: &prio, Weight: &weight})
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != title || got.Description != desc || got.Priority != 0 || got.Weight != 5 || got.Version != task.Version+1 {
		t.Fatalf("task = %+v", got)
	}
	evs := e.events(since)
	if len(evs) != 1 || evs[0].Type != domain.EventTaskUpdated {
		t.Fatalf("events = %+v", evs)
	}
}

func TestUpdateTaskNoChangeWritesNothing(t *testing.T) {
	e := lifeBNew(t)
	task := e.task("Same")
	since := e.maxEvent()
	same := "Same"
	got, err := e.svc.UpdateTask(lifeBctx, domain.Human, task.ID, TaskEdit{Title: &same})
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != task.Version || len(e.events(since)) != 0 {
		t.Fatalf("a no-op edit changed state: version %d, events %d", got.Version, len(e.events(since)))
	}
}

func TestUpdateTaskValidationAndConflict(t *testing.T) {
	e := lifeBNew(t)
	task := e.task("T")
	empty, bad := "  ", 9
	if _, err := e.svc.UpdateTask(lifeBctx, domain.Human, task.ID, TaskEdit{Title: &empty}); !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("empty title: %v", err)
	}
	if _, err := e.svc.UpdateTask(lifeBctx, domain.Human, task.ID, TaskEdit{Priority: &bad}); !errors.As(err, new(*domain.ValidationError)) {
		t.Fatalf("priority 9: %v", err)
	}
	title := "x"
	if _, err := e.svc.UpdateTask(lifeBctx, domain.Human, task.ID, TaskEdit{Title: &title, ExpectedVersion: task.Version + 7}); !errors.As(err, new(*domain.ConflictError)) {
		t.Fatalf("stale version: %v", err)
	}
	// Clearing the description is allowed.
	d := "keep"
	if _, err := e.svc.UpdateTask(lifeBctx, domain.Human, task.ID, TaskEdit{Description: &d}); err != nil {
		t.Fatal(err)
	}
	none := ""
	got, err := e.svc.UpdateTask(lifeBctx, domain.Human, task.ID, TaskEdit{Description: &none})
	if err != nil || got.Description != "" {
		t.Fatalf("clear description: %+v %v", got, err)
	}
}
