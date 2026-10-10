package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"skald/internal/domain"
)

var (
	ctx   = context.Background()
	t0    = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	human = Meta{Actor: domain.Human, Now: t0}
)

func at(d time.Duration) Meta { return Meta{Actor: domain.Human, Now: t0.Add(d)} }

// write runs fn in a write transaction and fails the test on error.
func write(t *testing.T, db *DB, fn func(tx *sql.Tx) error) {
	t.Helper()
	if err := db.WriteTx(ctx, fn); err != nil {
		t.Fatal(err)
	}
}

func mustProject(t *testing.T, db *DB, slug string) domain.Project {
	t.Helper()
	var p domain.Project
	write(t, db, func(tx *sql.Tx) (err error) {
		p, err = CreateProject(ctx, tx, human, domain.Project{Slug: slug, Name: slug + " project"})
		return
	})
	return p
}

func mustFeature(t *testing.T, db *DB, projectID int64, title string) domain.Feature {
	t.Helper()
	var f domain.Feature
	write(t, db, func(tx *sql.Tx) (err error) {
		f, err = CreateFeature(ctx, tx, human, domain.Feature{ProjectID: projectID, Title: title})
		return
	})
	return f
}

func mustTask(t *testing.T, db *DB, projectID, featureID int64, title string) domain.Task {
	t.Helper()
	var task domain.Task
	write(t, db, func(tx *sql.Tx) (err error) {
		task, _, err = CreateTask(ctx, tx, human, TaskInput{ProjectID: projectID, FeatureID: featureID, Title: title})
		return
	})
	return task
}

func mustDep(t *testing.T, db *DB, task, dependsOn int64) {
	t.Helper()
	write(t, db, func(tx *sql.Tx) error {
		_, err := AddDependency(ctx, tx, human, task, dependsOn)
		return err
	})
}

func isKind(err error, target error) bool { return errors.Is(err, target) }

func TestProjects(t *testing.T) {
	db := openTemp(t)
	var p domain.Project
	write(t, db, func(tx *sql.Tx) (err error) {
		p, err = CreateProject(ctx, tx, human, domain.Project{Slug: "school", Name: "School", Description: "d", RepoPath: "/repo/school", Settings: domain.ProjectSettings{RequireEvidence: false}})
		return
	})
	if p.ID == 0 || p.Status != domain.ProjectActive || p.Description != "d" || p.RepoPath != "/repo/school" {
		t.Fatalf("unexpected project %+v", p)
	}
	if !p.Settings.RequireEvidence {
		t.Error("new projects must start with default settings (evidence required)")
	}
	if !p.CreatedAt.Equal(t0) {
		t.Errorf("created_at = %v, want %v", p.CreatedAt, t0)
	}
	for name, get := range map[string]func() (domain.Project, error){
		"id":   func() (domain.Project, error) { return GetProject(ctx, db.Reader(), p.ID) },
		"slug": func() (domain.Project, error) { return GetProjectBySlug(ctx, db.Reader(), "school") },
		"repo": func() (domain.Project, error) { return GetProjectByRepoPath(ctx, db.Reader(), "/repo/school") },
	} {
		got, err := get()
		if err != nil || got.ID != p.ID {
			t.Errorf("get by %s: %v %+v", name, err, got)
		}
	}
	if _, err := GetProjectBySlug(ctx, db.Reader(), "nope"); !isKind(err, domain.ErrNotFound) {
		t.Errorf("missing slug: %v", err)
	}
	if _, err := GetProject(ctx, db.Reader(), 999); !isKind(err, domain.ErrNotFound) {
		t.Errorf("missing id: %v", err)
	}

	// Validation and duplicates map to ValidationError, never driver errors.
	write(t, db, func(tx *sql.Tx) error { // use a nested check without failing the tx
		for _, bad := range []domain.Project{{Slug: "Bad Slug", Name: "x"}, {Slug: "ok", Name: " "}, {Slug: "", Name: "x"}} {
			if _, err := CreateProject(ctx, tx, human, bad); !isKind(err, domain.ErrValidation) {
				t.Errorf("CreateProject(%+v) = %v, want validation error", bad, err)
			}
		}
		return nil
	})
	err := db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := CreateProject(ctx, tx, human, domain.Project{Slug: "school", Name: "again"})
		return err
	})
	var ve *domain.ValidationError
	if !errors.As(err, &ve) || ve.Field != "slug" {
		t.Errorf("duplicate slug = %v, want ValidationError on slug", err)
	}

	mustProject(t, db, "other")
	list, err := ListProjects(ctx, db.Reader(), false)
	if err != nil || len(list) != 2 || list[0].Slug != "other" {
		t.Fatalf("list = %v %v", list, err)
	}

	write(t, db, func(tx *sql.Tx) error {
		got, err := UpdateProjectSettings(ctx, tx, human, p.ID, domain.ProjectSettings{RequireEvidence: false})
		if err != nil || got.Settings.RequireEvidence {
			t.Errorf("settings update: %v %+v", err, got)
		}
		return err
	})
	write(t, db, func(tx *sql.Tx) error {
		got, err := ArchiveProject(ctx, tx, human, p.ID)
		if err != nil || got.Status != domain.ProjectArchived {
			t.Errorf("archive: %v %+v", err, got)
		}
		// Archiving twice is a no-op.
		_, err = ArchiveProject(ctx, tx, human, p.ID)
		return err
	})
	if list, _ = ListProjects(ctx, db.Reader(), false); len(list) != 1 {
		t.Errorf("archived project still listed: %v", list)
	}
	if list, _ = ListProjects(ctx, db.Reader(), true); len(list) != 2 {
		t.Errorf("includeArchived list = %v", list)
	}
	evs, _ := EventsSince(ctx, db.Reader(), p.ID, 0, 0)
	var types []domain.EventType
	for _, e := range evs {
		types = append(types, e.Type)
	}
	want := []domain.EventType{domain.EventProjectCreated, domain.EventProjectUpdated, domain.EventProjectArchived, domain.EventProjectUpdated}
	if len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events = %v, want %v", types, want)
		}
	}
}

func TestFeatures(t *testing.T) {
	db := openTemp(t)
	p := mustProject(t, db, "p")
	f1 := mustFeature(t, db, p.ID, "one")
	f2 := mustFeature(t, db, p.ID, "two")
	if f1.Position != 1 || f2.Position != 2 || f1.Weight != 1 || f1.Status != domain.FeaturePlanned {
		t.Fatalf("defaults: %+v %+v", f1, f2)
	}
	list, err := ListFeatures(ctx, db.Reader(), p.ID)
	if err != nil || len(list) != 2 || list[0].ID != f1.ID {
		t.Fatalf("list: %v %v", list, err)
	}

	err = db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := CreateFeature(ctx, tx, human, domain.Feature{ProjectID: p.ID, Title: "heavy", Weight: 11})
		return err
	})
	if !isKind(err, domain.ErrValidation) {
		t.Errorf("weight 11: %v", err)
	}
	err = db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := CreateFeature(ctx, tx, human, domain.Feature{ProjectID: 999, Title: "orphan"})
		return err
	})
	if !isKind(err, domain.ErrNotFound) {
		t.Errorf("missing project: %v", err)
	}

	title, weight := "renamed", 5
	write(t, db, func(tx *sql.Tx) error {
		got, err := UpdateFeature(ctx, tx, at(time.Minute), f1.ID, FeaturePatch{Title: &title, Weight: &weight})
		if err != nil || got.Title != "renamed" || got.Weight != 5 || !got.UpdatedAt.Equal(t0.Add(time.Minute)) {
			t.Errorf("update: %v %+v", err, got)
		}
		// Empty patch: no event.
		_, err = UpdateFeature(ctx, tx, human, f1.ID, FeaturePatch{})
		return err
	})
	write(t, db, func(tx *sql.Tx) error {
		got, err := CancelFeature(ctx, tx, human, f2.ID)
		if err != nil || got.Status != domain.FeatureCancelled {
			t.Errorf("cancel: %v %+v", err, got)
		}
		_, err = CancelFeature(ctx, tx, human, f2.ID) // no-op
		return err
	})
	evs, _ := EventsSince(ctx, db.Reader(), p.ID, 0, 0)
	counts := map[domain.EventType]int{}
	for _, e := range evs {
		counts[e.Type]++
	}
	if counts[domain.EventFeatureCreated] != 2 || counts[domain.EventFeatureUpdated] != 1 || counts[domain.EventFeatureCancelled] != 1 {
		t.Errorf("event counts = %v", counts)
	}
}

func TestTaskCreateDefaultsAndIdempotency(t *testing.T) {
	db := openTemp(t)
	p := mustProject(t, db, "p")
	f := mustFeature(t, db, p.ID, "f")
	other := mustProject(t, db, "q")
	otherF := mustFeature(t, db, other.ID, "g")

	var first, again domain.Task
	var created1, created2 bool
	write(t, db, func(tx *sql.Tx) (err error) {
		first, created1, err = CreateTask(ctx, tx, human, TaskInput{ProjectID: p.ID, FeatureID: f.ID, Title: "t", IdempotencyKey: "k1"})
		return
	})
	if !created1 || first.Status != domain.TaskTodo || first.Priority != 2 || first.Weight != 1 || first.Version != 1 || first.Position != 1 || first.IdempotencyKey != "k1" {
		t.Fatalf("defaults: %+v", first)
	}
	before, _ := MaxEventID(ctx, db.Reader(), 0)
	write(t, db, func(tx *sql.Tx) (err error) {
		again, created2, err = CreateTask(ctx, tx, human, TaskInput{ProjectID: p.ID, FeatureID: f.ID, Title: "different title", IdempotencyKey: "k1"})
		return
	})
	after, _ := MaxEventID(ctx, db.Reader(), 0)
	if created2 || again.ID != first.ID || again.Title != "t" || after != before {
		t.Errorf("idempotent create: created=%v again=%+v events %d->%d", created2, again, before, after)
	}
	// The same key in another project is a different task.
	var third domain.Task
	write(t, db, func(tx *sql.Tx) (err error) {
		third, _, err = CreateTask(ctx, tx, human, TaskInput{ProjectID: other.ID, FeatureID: otherF.ID, Title: "t", IdempotencyKey: "k1"})
		return
	})
	if third.ID == first.ID {
		t.Error("idempotency key must be scoped per project")
	}

	urgent := 0
	var u domain.Task
	write(t, db, func(tx *sql.Tx) (err error) {
		u, _, err = CreateTask(ctx, tx, human, TaskInput{ProjectID: p.ID, FeatureID: f.ID, Title: "urgent", Priority: &urgent})
		return
	})
	if u.Priority != 0 || u.Position != 2 {
		t.Errorf("explicit urgent priority lost: %+v", u)
	}

	for name, in := range map[string]TaskInput{
		"feature of other project": {ProjectID: p.ID, FeatureID: otherF.ID, Title: "x"},
		"empty title":              {ProjectID: p.ID, FeatureID: f.ID, Title: ""},
		"weight 11":                {ProjectID: p.ID, FeatureID: f.ID, Title: "x", Weight: 11},
		"priority 4":               {ProjectID: p.ID, FeatureID: f.ID, Title: "x", Priority: ptr(4)},
	} {
		err := db.WriteTx(ctx, func(tx *sql.Tx) error {
			_, _, err := CreateTask(ctx, tx, human, in)
			return err
		})
		if !isKind(err, domain.ErrValidation) {
			t.Errorf("%s: %v, want validation error", name, err)
		}
	}
	err := db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, _, err := CreateTask(ctx, tx, human, TaskInput{ProjectID: p.ID, FeatureID: 999, Title: "x"})
		return err
	})
	if !isKind(err, domain.ErrNotFound) {
		t.Errorf("missing feature: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestListTasksFilters(t *testing.T) {
	db := openTemp(t)
	p := mustProject(t, db, "p")
	f1 := mustFeature(t, db, p.ID, "f1")
	f2 := mustFeature(t, db, p.ID, "f2")
	a := mustTask(t, db, p.ID, f1.ID, "a")
	b := mustTask(t, db, p.ID, f1.ID, "b")
	c := mustTask(t, db, p.ID, f2.ID, "c")
	var urgent domain.Task
	write(t, db, func(tx *sql.Tx) (err error) {
		urgent, _, err = CreateTask(ctx, tx, human, TaskInput{ProjectID: p.ID, FeatureID: f2.ID, Title: "urgent", Priority: ptr(0)})
		return
	})
	mustDep(t, db, b.ID, a.ID) // b waits for a

	ids := func(f TaskFilter) []int64 {
		ts, err := ListTasks(ctx, db.Reader(), f)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, x := range ts {
			out = append(out, x.ID)
		}
		return out
	}
	eq := func(name string, got, want []int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s = %v, want %v", name, got, want)
			return
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s = %v, want %v", name, got, want)
				return
			}
		}
	}
	eq("all (priority then position)", ids(TaskFilter{ProjectID: p.ID}), []int64{urgent.ID, a.ID, b.ID, c.ID})
	eq("feature", ids(TaskFilter{ProjectID: p.ID, FeatureID: f1.ID}), []int64{a.ID, b.ID})
	eq("ready", ids(TaskFilter{ProjectID: p.ID, ReadyOnly: true}), []int64{urgent.ID, a.ID, c.ID})
	eq("limit/offset", ids(TaskFilter{ProjectID: p.ID, Limit: 2, Offset: 1}), []int64{a.ID, b.ID})

	write(t, db, func(tx *sql.Tx) error {
		if _, err := SetTaskStatus(ctx, tx, human, TaskStatusChange{TaskID: a.ID, To: domain.TaskInProgress}); err != nil {
			return err
		}
		_, err := SetTaskStatus(ctx, tx, human, TaskStatusChange{TaskID: a.ID, To: domain.TaskDone})
		return err
	})
	eq("ready after dependency done", ids(TaskFilter{ProjectID: p.ID, ReadyOnly: true}), []int64{urgent.ID, b.ID, c.ID})
	eq("status filter", ids(TaskFilter{ProjectID: p.ID, Statuses: []domain.TaskStatus{domain.TaskDone}}), []int64{a.ID})
	if _, err := ListTasks(ctx, db.Reader(), TaskFilter{ProjectID: p.ID, Statuses: []domain.TaskStatus{"bogus"}}); !isKind(err, domain.ErrValidation) {
		t.Errorf("bogus status: %v", err)
	}
	// Tasks of another project never show up.
	q := mustProject(t, db, "q")
	if got := ids(TaskFilter{ProjectID: q.ID}); len(got) != 0 {
		t.Errorf("other project tasks = %v", got)
	}
}

func TestSetTaskStatusSideEffectsAndVersion(t *testing.T) {
	db := openTemp(t)
	p := mustProject(t, db, "p")
	f := mustFeature(t, db, p.ID, "f")
	task := mustTask(t, db, p.ID, f.ID, "t")
	agent := domain.NewAgentActor("claude-code", "s1")

	var got domain.Task
	set := func(m Meta, c TaskStatusChange) (domain.Task, error) {
		var out domain.Task
		err := db.WriteTx(ctx, func(tx *sql.Tx) (err error) {
			out, err = SetTaskStatus(ctx, tx, m, c)
			return
		})
		return out, err
	}

	got, err := set(Meta{Actor: agent, Now: t0.Add(time.Hour)}, TaskStatusChange{TaskID: task.ID, To: domain.TaskInProgress, ExpectedVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskInProgress || got.Version != 2 || got.Assignee != string(agent) || got.StartedAt == nil || !got.StartedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("start: %+v", got)
	}

	// Stale expected version -> ConflictError with the current version.
	_, err = set(human, TaskStatusChange{TaskID: task.ID, To: domain.TaskDone, ExpectedVersion: 1})
	var ce *domain.ConflictError
	if !errors.As(err, &ce) || ce.Current != 2 || ce.Expected != 1 || ce.CurrentTask == nil || ce.CurrentTask.Status != domain.TaskInProgress {
		t.Fatalf("conflict = %v", err)
	}

	got, err = set(at(2*time.Hour), TaskStatusChange{TaskID: task.ID, To: domain.TaskBlocked, BlockedReason: "waiting on API key", Note: "n"})
	if err != nil || got.BlockedReason != "waiting on API key" || got.Version != 3 || got.Assignee != string(agent) {
		t.Fatalf("block: %v %+v", err, got)
	}
	got, err = set(at(3*time.Hour), TaskStatusChange{TaskID: task.ID, To: domain.TaskInProgress})
	if err != nil || got.BlockedReason != "" || got.Assignee != string(agent) || !got.StartedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("unblock must clear reason and keep assignee/started_at: %v %+v", err, got)
	}
	got, err = set(at(4*time.Hour), TaskStatusChange{TaskID: task.ID, To: domain.TaskDone, Forced: true})
	if err != nil || got.CompletedAt == nil || !got.CompletedAt.Equal(t0.Add(4*time.Hour)) {
		t.Fatalf("done: %v %+v", err, got)
	}
	got, err = set(at(5*time.Hour), TaskStatusChange{TaskID: task.ID, To: domain.TaskTodo})
	if err != nil || got.CompletedAt != nil || got.StartedAt != nil || got.Assignee != "" {
		t.Fatalf("reopen must clear completed_at, started_at and assignee: %v %+v", err, got)
	}
	if _, err = set(human, TaskStatusChange{TaskID: task.ID, To: "weird"}); !isKind(err, domain.ErrValidation) {
		t.Errorf("unknown status: %v", err)
	}
	if _, err = set(human, TaskStatusChange{TaskID: 999, To: domain.TaskDone}); !isKind(err, domain.ErrNotFound) {
		t.Errorf("missing task: %v", err)
	}

	evs, _ := TaskEvents(ctx, db.Reader(), task.ID, 0)
	if len(evs) != 6 { // created + 5 status changes
		t.Fatalf("task events = %d, want 6", len(evs))
	}
	if evs[0].Type != domain.EventTaskStatus || evs[len(evs)-1].Type != domain.EventTaskCreated {
		t.Errorf("TaskEvents must be newest first: %v ... %v", evs[0].Type, evs[len(evs)-1].Type)
	}
	if string(evs[1].Payload) == "" || evs[3].Actor != domain.Human {
		t.Errorf("payload/actor: %s %v", evs[1].Payload, evs[3].Actor)
	}
}

func TestUpdateTaskFieldsAndNotes(t *testing.T) {
	db := openTemp(t)
	p := mustProject(t, db, "p")
	f1 := mustFeature(t, db, p.ID, "f1")
	f2 := mustFeature(t, db, p.ID, "f2")
	task := mustTask(t, db, p.ID, f1.ID, "t")
	otherF := mustFeature(t, db, mustProject(t, db, "q").ID, "g")

	title, prio, w := "new title", 0, 7
	write(t, db, func(tx *sql.Tx) error {
		got, err := UpdateTaskFields(ctx, tx, at(time.Minute), task.ID, 1, TaskPatch{Title: &title, Priority: &prio, Weight: &w, FeatureID: &f2.ID})
		if err != nil || got.Title != title || got.Priority != 0 || got.Weight != 7 || got.FeatureID != f2.ID || got.Version != 2 {
			t.Errorf("update: %v %+v", err, got)
		}
		return err
	})
	err := db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := UpdateTaskFields(ctx, tx, human, task.ID, 1, TaskPatch{Title: &title})
		return err
	})
	if !isKind(err, domain.ErrConflict) {
		t.Errorf("stale version: %v", err)
	}
	err = db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := UpdateTaskFields(ctx, tx, human, task.ID, 0, TaskPatch{FeatureID: &otherF.ID})
		return err
	})
	if !isKind(err, domain.ErrValidation) {
		t.Errorf("feature of another project: %v", err)
	}
	bad := 11
	err = db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := UpdateTaskFields(ctx, tx, human, task.ID, 0, TaskPatch{Weight: &bad})
		return err
	})
	if !isKind(err, domain.ErrValidation) {
		t.Errorf("weight 11: %v", err)
	}

	// Notes: event + touched updated_at, version unchanged.
	var id int64
	write(t, db, func(tx *sql.Tx) (err error) {
		id, err = AddTaskNote(ctx, tx, at(2*time.Hour), task.ID, "halfway there")
		return
	})
	got, _ := GetTask(ctx, db.Reader(), task.ID)
	if got.Version != 2 || !got.UpdatedAt.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("note must touch updated_at without bumping version: %+v", got)
	}
	evs, _ := EventsSince(ctx, db.Reader(), p.ID, id-1, 0)
	if len(evs) != 1 || evs[0].Type != domain.EventTaskNote || evs[0].EntityID != task.ID {
		t.Errorf("note event: %+v", evs)
	}
	err = db.WriteTx(ctx, func(tx *sql.Tx) error { _, err := AddTaskNote(ctx, tx, human, task.ID, "  "); return err })
	if !isKind(err, domain.ErrValidation) {
		t.Errorf("empty note: %v", err)
	}
	if _, err := GetTaskInProject(ctx, db.Reader(), p.ID+100, task.ID); !isKind(err, domain.ErrNotFound) {
		t.Errorf("GetTaskInProject wrong project: %v", err)
	}
}

func TestDependencies(t *testing.T) {
	db := openTemp(t)
	p := mustProject(t, db, "p")
	f := mustFeature(t, db, p.ID, "f")
	a, b, c, d := mustTask(t, db, p.ID, f.ID, "a"), mustTask(t, db, p.ID, f.ID, "b"), mustTask(t, db, p.ID, f.ID, "c"), mustTask(t, db, p.ID, f.ID, "d")

	// Diamond: d -> b -> a and d -> c -> a. Not a cycle.
	mustDep(t, db, b.ID, a.ID)
	mustDep(t, db, c.ID, a.ID)
	mustDep(t, db, d.ID, b.ID)
	mustDep(t, db, d.ID, c.ID)

	ids, _ := ListDependencyIDs(ctx, db.Reader(), d.ID)
	if len(ids) != 2 || ids[0] != b.ID || ids[1] != c.ID {
		t.Errorf("deps of d = %v", ids)
	}
	dts, _ := ListDependencyTasks(ctx, db.Reader(), d.ID)
	if len(dts) != 2 || dts[0].Title != "b" {
		t.Errorf("dep tasks = %v", dts)
	}
	dependents, _ := ListDependents(ctx, db.Reader(), a.ID)
	if len(dependents) != 2 {
		t.Errorf("dependents of a = %v", dependents)
	}
	edges, _ := ListProjectDependencies(ctx, db.Reader(), p.ID)
	if len(edges) != 4 {
		t.Errorf("edges = %v", edges)
	}

	addErr := func(task, dep int64) error {
		return db.WriteTx(ctx, func(tx *sql.Tx) error { _, err := AddDependency(ctx, tx, human, task, dep); return err })
	}

	// Direct cycle: a -> b while b -> a.
	var ce *domain.DependencyCycleError
	err := addErr(a.ID, b.ID)
	if !errors.As(err, &ce) || len(ce.Path) != 3 || ce.Path[0] != a.ID || ce.Path[1] != b.ID || ce.Path[2] != a.ID {
		t.Fatalf("direct cycle = %v (%+v)", err, ce)
	}
	// Transitive cycle: a -> d closes a -> d -> b -> a (shortest path, via b or c).
	err = addErr(a.ID, d.ID)
	if !errors.As(err, &ce) || len(ce.Path) != 4 || ce.Path[0] != a.ID || ce.Path[1] != d.ID || ce.Path[3] != a.ID {
		t.Fatalf("transitive cycle = %v (%+v)", err, ce)
	}
	// Self dependency.
	if err := addErr(a.ID, a.ID); !isKind(err, domain.ErrValidation) {
		t.Errorf("self dependency: %v", err)
	}
	// Missing task.
	if err := addErr(a.ID, 999); !isKind(err, domain.ErrNotFound) {
		t.Errorf("missing dependency target: %v", err)
	}
	// Cross-project.
	q := mustProject(t, db, "q")
	qf := mustFeature(t, db, q.ID, "g")
	qt := mustTask(t, db, q.ID, qf.ID, "qt")
	if err := addErr(a.ID, qt.ID); !isKind(err, domain.ErrValidation) {
		t.Errorf("cross-project dependency: %v", err)
	}
	// None of the rejected additions left edges or events behind.
	edges, _ = ListProjectDependencies(ctx, db.Reader(), p.ID)
	if len(edges) != 4 {
		t.Errorf("rejected dependencies left edges: %v", edges)
	}

	// Duplicate add: no-op, no event.
	before, _ := MaxEventID(ctx, db.Reader(), p.ID)
	var added bool
	write(t, db, func(tx *sql.Tx) (err error) { added, err = AddDependency(ctx, tx, human, d.ID, b.ID); return })
	if added {
		t.Error("duplicate add reported added")
	}
	if after, _ := MaxEventID(ctx, db.Reader(), p.ID); after != before {
		t.Error("duplicate add appended an event")
	}

	// Remove, then the previously cyclic edge is fine.
	var removed bool
	write(t, db, func(tx *sql.Tx) (err error) { removed, err = RemoveDependency(ctx, tx, human, b.ID, a.ID); return })
	if !removed {
		t.Error("remove reported nothing removed")
	}
	write(t, db, func(tx *sql.Tx) (err error) { removed, err = RemoveDependency(ctx, tx, human, b.ID, a.ID); return })
	if removed {
		t.Error("second remove should be a no-op")
	}
	mustDep(t, db, a.ID, b.ID)

	path, _ := FindDependencyPath(ctx, db.Reader(), d.ID, a.ID)
	if len(path) != 3 || path[0] != d.ID || path[2] != a.ID { // d -> c -> a (b->a was removed)
		t.Errorf("FindDependencyPath = %v", path)
	}
	if path, _ := FindDependencyPath(ctx, db.Reader(), c.ID, d.ID); path != nil {
		t.Errorf("unreachable path = %v", path)
	}
}

func TestArtifacts(t *testing.T) {
	db := openTemp(t)
	p := mustProject(t, db, "p")
	f := mustFeature(t, db, p.ID, "f")
	task := mustTask(t, db, p.ID, f.ID, "t")
	agent := domain.NewAgentActor("claude-code", "s1")

	if n, _ := CountArtifacts(ctx, db.Reader(), task.ID); n != 0 {
		t.Fatalf("count = %d", n)
	}
	var a domain.Artifact
	write(t, db, func(tx *sql.Tx) (err error) {
		a, err = AddArtifact(ctx, tx, Meta{Actor: agent, Now: t0.Add(time.Hour)}, domain.Artifact{TaskID: task.ID, Kind: domain.ArtifactCommit, Ref: "abc123", Summary: "impl"})
		return
	})
	if a.ID == 0 || a.Actor != agent || a.Summary != "impl" {
		t.Fatalf("artifact: %+v", a)
	}
	got, _ := GetTask(ctx, db.Reader(), task.ID)
	if got.Version != 1 || !got.UpdatedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("artifact must touch updated_at only: %+v", got)
	}
	list, _ := ListArtifacts(ctx, db.Reader(), task.ID)
	if len(list) != 1 || list[0].Ref != "abc123" {
		t.Errorf("list = %+v", list)
	}
	if n, _ := CountArtifacts(ctx, db.Reader(), task.ID); n != 1 {
		t.Errorf("count = %d", n)
	}
	evs, _ := TaskEvents(ctx, db.Reader(), task.ID, 0)
	if len(evs) != 2 || evs[0].Type != domain.EventArtifactAdded || evs[0].EntityType != domain.EntityArtifact {
		t.Errorf("TaskEvents should include the artifact event: %+v", evs)
	}
	for name, in := range map[string]domain.Artifact{
		"bad kind":  {TaskID: task.ID, Kind: "nope", Ref: "x"},
		"empty ref": {TaskID: task.ID, Kind: domain.ArtifactLink, Ref: ""},
	} {
		err := db.WriteTx(ctx, func(tx *sql.Tx) error { _, err := AddArtifact(ctx, tx, human, in); return err })
		if !isKind(err, domain.ErrValidation) {
			t.Errorf("%s: %v", name, err)
		}
	}
	err := db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := AddArtifact(ctx, tx, human, domain.Artifact{TaskID: 999, Kind: domain.ArtifactLink, Ref: "x"})
		return err
	})
	if !isKind(err, domain.ErrNotFound) {
		t.Errorf("missing task: %v", err)
	}
}

func TestDocuments(t *testing.T) {
	db := openTemp(t)
	p := mustProject(t, db, "p")
	var d1, d2 domain.Document
	var v1, v2 domain.DocumentVersion
	write(t, db, func(tx *sql.Tx) (err error) {
		d1, v1, err = PutDocument(ctx, tx, human, p.ID, "architecture", "Overview", "first draft")
		return
	})
	write(t, db, func(tx *sql.Tx) (err error) {
		d2, v2, err = PutDocument(ctx, tx, at(time.Hour), p.ID, "architecture", "Overview", "second draft")
		return
	})
	if d1.ID != d2.ID || v1.Version != 1 || v2.Version != 2 || d2.CurrentVersion != 2 {
		t.Fatalf("versions: %+v %+v %+v %+v", d1, d2, v1, v2)
	}
	doc, cur, err := GetDocument(ctx, db.Reader(), p.ID, "architecture", "Overview")
	if err != nil || doc.CurrentVersion != 2 || cur.Body != "second draft" {
		t.Fatalf("get: %v %+v %+v", err, doc, cur)
	}
	old, err := GetDocumentVersion(ctx, db.Reader(), doc.ID, 1)
	if err != nil || old.Body != "first draft" {
		t.Errorf("old version unreadable: %v %+v", err, old)
	}
	if _, err := GetDocumentVersion(ctx, db.Reader(), doc.ID, 9); !isKind(err, domain.ErrNotFound) {
		t.Errorf("missing version: %v", err)
	}
	if _, _, err := GetDocument(ctx, db.Reader(), p.ID, "architecture", "Nope"); !isKind(err, domain.ErrNotFound) {
		t.Errorf("missing doc: %v", err)
	}
	if _, byID, err := GetDocumentByID(ctx, db.Reader(), doc.ID); err != nil || byID.Version != 2 {
		t.Errorf("by id: %v %+v", err, byID)
	}
	write(t, db, func(tx *sql.Tx) error {
		_, _, err := PutDocument(ctx, tx, human, p.ID, "conventions", "Style", "use gofmt")
		return err
	})
	all, _ := ListDocuments(ctx, db.Reader(), p.ID, "")
	byKind, _ := ListDocuments(ctx, db.Reader(), p.ID, "conventions")
	if len(all) != 2 || all[0].Kind != "architecture" || len(byKind) != 1 {
		t.Errorf("list: %+v %+v", all, byKind)
	}
	err = db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, _, err := PutDocument(ctx, tx, human, p.ID, "x", "", "body")
		return err
	})
	if !isKind(err, domain.ErrValidation) {
		t.Errorf("empty title: %v", err)
	}
}

func TestEventsAtomicWithMutation(t *testing.T) {
	db := openTemp(t)
	p := mustProject(t, db, "p")
	f := mustFeature(t, db, p.ID, "f")
	before, _ := MaxEventID(ctx, db.Reader(), 0)

	boom := errors.New("boom")
	err := db.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, _, err := CreateTask(ctx, tx, human, TaskInput{ProjectID: p.ID, FeatureID: f.ID, Title: "rolled back"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	after, _ := MaxEventID(ctx, db.Reader(), 0)
	if after != before {
		t.Errorf("rolled-back mutation left events: %d -> %d", before, after)
	}
	if ts, _ := ListTasks(ctx, db.Reader(), TaskFilter{ProjectID: p.ID}); len(ts) != 0 {
		t.Errorf("rolled-back mutation left tasks: %v", ts)
	}

	// Every successful mutation appends exactly one event.
	n0, _ := MaxEventID(ctx, db.Reader(), 0)
	mustTask(t, db, p.ID, f.ID, "kept")
	n1, _ := MaxEventID(ctx, db.Reader(), 0)
	if n1 != n0+1 {
		t.Errorf("one mutation, %d events", n1-n0)
	}
}

func TestEventsSince(t *testing.T) {
	db := openTemp(t)
	p := mustProject(t, db, "p")
	q := mustProject(t, db, "q")
	f := mustFeature(t, db, p.ID, "f")
	for i := 0; i < 5; i++ {
		mustTask(t, db, p.ID, f.ID, "t")
	}
	all, err := EventsSince(ctx, db.Reader(), 0, 0, 0)
	if err != nil || len(all) != 8 { // 2 projects + 1 feature + 5 tasks
		t.Fatalf("all = %d, %v", len(all), err)
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatal("events must be ordered by ID")
		}
	}
	page, _ := EventsSince(ctx, db.Reader(), p.ID, 0, 3)
	if len(page) != 3 || page[0].ProjectID != p.ID {
		t.Fatalf("page = %+v", page)
	}
	next, _ := EventsSince(ctx, db.Reader(), p.ID, page[2].ID, 100)
	if len(next) != 4 || next[0].ID <= page[2].ID { // 1 feature... project(1)+feature(1)+tasks(5)=7; 7-3=4
		t.Fatalf("next = %d", len(next))
	}
	none, _ := EventsSince(ctx, db.Reader(), p.ID, next[len(next)-1].ID, 10)
	if len(none) != 0 {
		t.Errorf("past the end = %v", none)
	}
	if got, _ := EventsSince(ctx, db.Reader(), q.ID, 0, 0); len(got) != 1 || got[0].Type != domain.EventProjectCreated {
		t.Errorf("project filter = %+v", got)
	}
	max, _ := MaxEventID(ctx, db.Reader(), p.ID)
	if max != next[len(next)-1].ID {
		t.Errorf("MaxEventID = %d", max)
	}
	if empty, _ := MaxEventID(ctx, db.Reader(), 9999); empty != 0 {
		t.Errorf("MaxEventID of unknown project = %d", empty)
	}
}

func TestSnapshot(t *testing.T) {
	db := openTemp(t)
	p := mustProject(t, db, "p")
	f1 := mustFeature(t, db, p.ID, "f1")
	f2 := mustFeature(t, db, p.ID, "f2")
	a := mustTask(t, db, p.ID, f1.ID, "a")
	b := mustTask(t, db, p.ID, f1.ID, "b")
	c := mustTask(t, db, p.ID, f2.ID, "c")
	mustDep(t, db, b.ID, a.ID)
	write(t, db, func(tx *sql.Tx) error {
		if _, err := SetTaskStatus(ctx, tx, human, TaskStatusChange{TaskID: c.ID, To: domain.TaskInProgress}); err != nil {
			return err
		}
		_, err := SetTaskStatus(ctx, tx, human, TaskStatusChange{TaskID: c.ID, To: domain.TaskDone})
		return err
	})

	s, err := db.ReadSnapshot(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s.Project.ID != p.ID || len(s.Features) != 2 || len(s.Tasks) != 3 || len(s.Deps) != 1 {
		t.Fatalf("snapshot = %+v", s)
	}
	fw := s.FeaturesWithTasks()
	if len(fw) != 2 || len(fw[0].Tasks) != 2 || len(fw[1].Tasks) != 1 {
		t.Errorf("grouping = %+v", fw)
	}
	res := domain.ProjectProgress(fw)
	if res.Progress.Fraction != 0.5 { // f1 0%, f2 100%, equal weights
		t.Errorf("progress = %v", res.Progress.Fraction)
	}
	ready := s.ReadyTasks()
	if len(ready) != 1 || ready[0].ID != a.ID { // b waits for a; c is done
		t.Errorf("ready = %+v", ready)
	}
	if got := s.DepsByTask()[b.ID]; len(got) != 1 || got[0] != a.ID {
		t.Errorf("DepsByTask = %v", got)
	}
	if _, err := db.ReadSnapshot(ctx, 999); !isKind(err, domain.ErrNotFound) {
		t.Errorf("missing project: %v", err)
	}
}

func TestInvalidActorRejected(t *testing.T) {
	db := openTemp(t)
	err := db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := CreateProject(ctx, tx, Meta{Actor: "has space"}, domain.Project{Slug: "p", Name: "P"})
		return err
	})
	if !isKind(err, domain.ErrValidation) {
		t.Errorf("invalid actor: %v", err)
	}
	if n, _ := MaxEventID(ctx, db.Reader(), 0); n != 0 {
		t.Error("events written for invalid actor")
	}
}

func TestMapErrConstraints(t *testing.T) {
	db := openTemp(t)
	p := mustProject(t, db, "p")
	cases := map[string]string{
		"foreign key": `INSERT INTO features (project_id, title, created_at, updated_at) VALUES (999, 't', 'x', 'x')`,
		"check":       `INSERT INTO features (project_id, title, weight, created_at, updated_at) VALUES (` + "1" + `, 't', 99, 'x', 'x')`,
		"unique":      `INSERT INTO projects (slug, name, created_at, updated_at) VALUES ('p', 'n', 'x', 'x')`,
		"not null":    `INSERT INTO projects (slug, created_at, updated_at) VALUES ('z', 'x', 'x')`,
	}
	_ = p
	for name, stmt := range cases {
		_, err := db.Writer().ExecContext(ctx, stmt)
		if err == nil {
			t.Errorf("%s: statement unexpectedly succeeded", name)
			continue
		}
		mapped := mapErr(err)
		if !isKind(mapped, domain.ErrValidation) {
			t.Errorf("%s: mapErr(%v) = %v, want ValidationError", name, err, mapped)
		}
	}
}
