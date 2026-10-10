package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"skald/internal/domain"
	"skald/internal/store"
)

var (
	projACtx = context.Background()
	projAT0  = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
)

type projAEnv struct {
	svc *Service
	db  *store.DB
	now time.Time // the service clock; tests may advance it
	env map[string]string
}

func projANew(t *testing.T) *projAEnv {
	t.Helper()
	e := &projAEnv{now: projAT0, env: map[string]string{}}
	db, err := store.Open(projACtx, filepath.Join(t.TempDir(), "db", "skald.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.db = db
	e.svc = New(db,
		WithClock(func() time.Time { return e.now }),
		WithEnv(func(k string) string { return e.env[k] }),
		WithWorkdir(func() (string, error) { return "/definitely/not/a/project", nil }),
	)
	return e
}

func projADir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func projAMust[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// project creates a project without a repo and returns a ref to it.
func (e *projAEnv) project(t *testing.T, slug string) (domain.Project, ProjectRef) {
	t.Helper()
	p := projAMust[domain.Project](t)(e.svc.CreateProject(projACtx, domain.Human, CreateProjectInput{Slug: slug, Name: strings.ToUpper(slug)}))
	return p, ProjectRef{Slug: slug}
}

func (e *projAEnv) feature(t *testing.T, ref ProjectRef, title string) domain.Feature {
	t.Helper()
	return projAMust[domain.Feature](t)(e.svc.CreateFeature(projACtx, domain.Human, ref, CreateFeatureInput{Title: title}))
}

func (e *projAEnv) task(t *testing.T, ref ProjectRef, featureID int64, title string) domain.Task {
	t.Helper()
	task, _, err := e.svc.CreateTask(projACtx, domain.Human, ref, CreateTaskInput{FeatureID: featureID, Title: title})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// setStatus writes a status straight through the store, bypassing lifecycle
// rules; it keeps these tests independent of the lifecycle use cases.
func (e *projAEnv) setStatus(t *testing.T, taskID int64, to domain.TaskStatus) {
	t.Helper()
	err := e.db.WriteTx(projACtx, func(tx *sql.Tx) error {
		_, err := store.SetTaskStatus(projACtx, tx, store.Meta{Actor: domain.Human, Now: e.now}, store.TaskStatusChange{TaskID: taskID, To: to, BlockedReason: "why"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e *projAEnv) addDep(t *testing.T, task, dependsOn int64) {
	t.Helper()
	err := e.db.WriteTx(projACtx, func(tx *sql.Tx) error {
		_, err := store.AddDependency(projACtx, tx, store.Meta{Actor: domain.Human, Now: e.now}, task, dependsOn)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e *projAEnv) events(t *testing.T, projectID int64) []domain.Event {
	t.Helper()
	return projAMust[[]domain.Event](t)(store.EventsSince(projACtx, e.db.Reader(), projectID, 0, 0))
}

func projAKind(t *testing.T, err error, want domain.ErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %v error, got nil", want)
	}
	if got := domain.KindOf(err); got != want {
		t.Fatalf("error kind = %v, want %v (%v)", got, want, err)
	}
}

// --- projects ---

func TestProjA_CreateProjectDefaults(t *testing.T) {
	e := projANew(t)
	p, err := e.svc.CreateProject(projACtx, domain.Human, CreateProjectInput{Slug: "school", Name: "School", Description: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == 0 || p.Slug != "school" || p.Status != domain.ProjectActive || p.RepoPath != "" {
		t.Fatalf("unexpected project %+v", p)
	}
	if !p.Settings.RequireEvidence {
		t.Fatal("new project must require evidence by default")
	}
	if !p.CreatedAt.Equal(projAT0) {
		t.Fatalf("created_at = %v, want clock time", p.CreatedAt)
	}
	evs := e.events(t, p.ID)
	if len(evs) != 1 || evs[0].Type != domain.EventProjectCreated || evs[0].Actor != domain.Human {
		t.Fatalf("events = %+v", evs)
	}
}

func TestProjA_CreateProjectRepoPathNormalised(t *testing.T) {
	e := projANew(t)
	real := projADir(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.CreateProject(projACtx, domain.Human, CreateProjectInput{Slug: "a", Name: "A", RepoPath: link})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := NormalizeRepoPath(real)
	if p.RepoPath != want {
		t.Fatalf("repo_path = %q, want %q", p.RepoPath, want)
	}
	// And resolution from inside the repo finds it.
	sub := filepath.Join(real, "pkg", "x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.ResolveProject(projACtx, ProjectRef{Cwd: sub})
	if err != nil || got.ID != p.ID {
		t.Fatalf("resolve = %+v, %v", got, err)
	}
}

func TestProjA_CreateProjectLinkCwd(t *testing.T) {
	e := projANew(t)
	dir := projADir(t)
	p, err := e.svc.CreateProject(projACtx, domain.Human, CreateProjectInput{Slug: "a", Name: "A", LinkCwd: true, Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := NormalizeRepoPath(dir)
	if p.RepoPath != want {
		t.Fatalf("repo_path = %q, want %q", p.RepoPath, want)
	}

	// Without Cwd it falls back to the working directory function.
	other := projADir(t)
	e.svc.getwd = func() (string, error) { return other, nil }
	p2, err := e.svc.CreateProject(projACtx, domain.Human, CreateProjectInput{Slug: "b", Name: "B", LinkCwd: true})
	if err != nil {
		t.Fatal(err)
	}
	want2, _ := NormalizeRepoPath(other)
	if p2.RepoPath != want2 {
		t.Fatalf("repo_path = %q, want %q", p2.RepoPath, want2)
	}

	// LinkCwd is ignored when RepoPath is given.
	explicit := projADir(t)
	p3, err := e.svc.CreateProject(projACtx, domain.Human, CreateProjectInput{Slug: "c", Name: "C", RepoPath: explicit, LinkCwd: true})
	if err != nil {
		t.Fatal(err)
	}
	want3, _ := NormalizeRepoPath(explicit)
	if p3.RepoPath != want3 {
		t.Fatalf("repo_path = %q, want %q", p3.RepoPath, want3)
	}
}

func TestProjA_CreateProjectValidation(t *testing.T) {
	e := projANew(t)
	dir := projADir(t)
	e.project(t, "taken")
	if _, err := e.svc.CreateProject(projACtx, domain.Human, CreateProjectInput{Slug: "first", Name: "First", RepoPath: dir}); err != nil {
		t.Fatal(err)
	}

	cases := map[string]CreateProjectInput{
		"bad slug":      {Slug: "Not Valid", Name: "x"},
		"empty slug":    {Slug: "", Name: "x"},
		"empty name":    {Slug: "ok", Name: "  "},
		"dup slug":      {Slug: "taken", Name: "x"},
		"missing dir":   {Slug: "m", Name: "x", RepoPath: filepath.Join(dir, "nope")},
		"dup repo path": {Slug: "second", Name: "x", RepoPath: dir},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := e.svc.CreateProject(projACtx, domain.Human, in)
			projAKind(t, err, domain.KindValidation)
		})
	}

	_, err := e.svc.CreateProject(projACtx, domain.Human, CreateProjectInput{Slug: "second", Name: "x", RepoPath: dir})
	if !strings.Contains(err.Error(), "first") {
		t.Fatalf("duplicate repo error should name the owning project, got: %v", err)
	}
	// A failed create must not leave an event or a row behind.
	if _, err := store.GetProjectBySlug(projACtx, e.db.Reader(), "second"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("project second should not exist, err = %v", err)
	}
	if _, err := e.svc.CreateProject(projACtx, "bad actor", CreateProjectInput{Slug: "z", Name: "z"}); err == nil {
		t.Fatal("invalid actor accepted")
	}
}

func TestProjA_ArchiveFreesRepoPath(t *testing.T) {
	e := projANew(t)
	dir := projADir(t)
	norm, _ := NormalizeRepoPath(dir)
	p := projAMust[domain.Project](t)(e.svc.CreateProject(projACtx, domain.Human, CreateProjectInput{Slug: "old", Name: "Old", RepoPath: dir}))

	e.now = e.now.Add(time.Hour)
	archived, err := e.svc.ArchiveProject(projACtx, domain.Human, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if archived.Status != domain.ProjectArchived || archived.RepoPath != "" {
		t.Fatalf("archived project = %+v", archived)
	}

	// The old path is recorded in the event log.
	var found bool
	for _, ev := range e.events(t, p.ID) {
		if ev.Type == domain.EventProjectUpdated {
			var payload map[string]string
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["released_repo_path"] == norm {
				found = true
				if !ev.CreatedAt.Equal(e.now) {
					t.Fatalf("event time = %v, want %v", ev.CreatedAt, e.now)
				}
			}
		}
	}
	if !found {
		t.Fatalf("no event records released path %q: %+v", norm, e.events(t, p.ID))
	}

	// The repository can be linked to a new project, and resolves to it.
	fresh, err := e.svc.CreateProject(projACtx, domain.Human, CreateProjectInput{Slug: "new", Name: "New", RepoPath: dir})
	if err != nil {
		t.Fatalf("reusing repo path: %v", err)
	}
	got, err := e.svc.ResolveProject(projACtx, ProjectRef{Cwd: dir})
	if err != nil || got.ID != fresh.ID {
		t.Fatalf("resolve = %+v, %v", got, err)
	}

	// Archiving again is a no-op: no extra events.
	before := len(e.events(t, p.ID))
	again, err := e.svc.ArchiveProject(projACtx, domain.Human, p.ID)
	if err != nil || again.Status != domain.ProjectArchived {
		t.Fatalf("second archive = %+v, %v", again, err)
	}
	if after := len(e.events(t, p.ID)); after != before {
		t.Fatalf("events grew from %d to %d on repeat archive", before, after)
	}

	_, err = e.svc.ArchiveProject(projACtx, domain.Human, 9999)
	projAKind(t, err, domain.KindNotFound)
}

func TestProjA_ArchiveWithoutRepoPath(t *testing.T) {
	e := projANew(t)
	p, _ := e.project(t, "norepo")
	got, err := e.svc.ArchiveProject(projACtx, domain.Human, p.ID)
	if err != nil || got.Status != domain.ProjectArchived {
		t.Fatalf("archive = %+v, %v", got, err)
	}
	for _, ev := range e.events(t, p.ID) {
		if ev.Type == domain.EventProjectUpdated {
			t.Fatalf("no repo path was released, but got event %+v", ev)
		}
	}
}

func TestProjA_ListProjects(t *testing.T) {
	e := projANew(t)
	_, refB := e.project(t, "bravo")
	e.project(t, "alpha")
	gone, _ := e.project(t, "gone")
	f := e.feature(t, refB, "F")
	task := e.task(t, refB, f.ID, "T")
	e.setStatus(t, task.ID, domain.TaskInProgress)
	e.setStatus(t, task.ID, domain.TaskDone)
	if _, err := e.svc.ArchiveProject(projACtx, domain.Human, gone.ID); err != nil {
		t.Fatal(err)
	}

	list, err := e.svc.ListProjects(projACtx, domain.Human, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Project.Slug != "alpha" || list[1].Project.Slug != "bravo" {
		t.Fatalf("list = %+v", list)
	}
	if got := list[1].Progress.Progress.Fraction; got != 1 {
		t.Fatalf("bravo progress = %v, want 1", got)
	}
	if list[0].Features != 0 || list[0].Progress.Progress.Fraction != 0 {
		t.Fatalf("alpha summary = %+v", list[0])
	}

	all, err := e.svc.ListProjects(projACtx, domain.Human, true)
	if err != nil || len(all) != 3 {
		t.Fatalf("all = %d, %v", len(all), err)
	}
}

func TestProjA_GetProjectSummary(t *testing.T) {
	e := projANew(t)
	p, ref := e.project(t, "proj")
	f1 := e.feature(t, ref, "F1")
	t1 := e.task(t, ref, f1.ID, "one")
	t2 := e.task(t, ref, f1.ID, "two")
	t3 := e.task(t, ref, f1.ID, "three")
	e.addDep(t, t3.ID, t1.ID)
	e.setStatus(t, t1.ID, domain.TaskInProgress)

	sum, err := e.svc.GetProject(projACtx, domain.Human, ref)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Project.ID != p.ID || sum.Features != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.Counts[domain.TaskInProgress] != 1 || sum.Counts[domain.TaskTodo] != 2 || sum.Counts.Total() != 3 {
		t.Fatalf("counts = %v", sum.Counts)
	}
	// t2 is ready; t3 waits on t1; t1 is in progress.
	if sum.Ready != 1 {
		t.Fatalf("ready = %d, want 1", sum.Ready)
	}
	e.setStatus(t, t1.ID, domain.TaskDone)
	_ = t2
	sum, _ = e.svc.GetProject(projACtx, domain.Human, ref)
	if sum.Ready != 2 {
		t.Fatalf("ready after t1 done = %d, want 2", sum.Ready)
	}
	// 1 of 3 equal-weight tasks is done.
	if got := sum.Progress.Progress.Fraction; got < 0.333 || got > 0.334 {
		t.Fatalf("progress = %v", got)
	}

	// Resolution failures pass through.
	_, err = e.svc.GetProject(projACtx, domain.Human, ProjectRef{Slug: "missing"})
	projAKind(t, err, domain.KindNotFound)
}

func TestProjA_GetProjectResolvesViaEnv(t *testing.T) {
	e := projANew(t)
	p, _ := e.project(t, "one")
	e.project(t, "two")
	e.env[EnvProject] = "one"
	sum, err := e.svc.GetProject(projACtx, domain.Human, ProjectRef{})
	if err != nil || sum.Project.ID != p.ID {
		t.Fatalf("summary = %+v, %v", sum, err)
	}
}

// --- features ---

func TestProjA_CreateAndListFeatures(t *testing.T) {
	e := projANew(t)
	_, ref := e.project(t, "proj")
	f1 := projAMust[domain.Feature](t)(e.svc.CreateFeature(projACtx, domain.Human, ref, CreateFeatureInput{Title: "Core", Description: "d", Weight: 3}))
	f2 := e.feature(t, ref, "Extras")
	if f1.Weight != 3 || f2.Weight != 1 || f1.Position != 1 || f2.Position != 2 || f1.Status != domain.FeaturePlanned {
		t.Fatalf("features = %+v %+v", f1, f2)
	}

	ta := e.task(t, ref, f1.ID, "a")
	tb := e.task(t, ref, f1.ID, "b")
	e.setStatus(t, ta.ID, domain.TaskInProgress)

	list, err := e.svc.ListFeatures(projACtx, domain.Human, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Feature.ID != f1.ID || list[1].Feature.ID != f2.ID {
		t.Fatalf("order = %+v", list)
	}
	if list[0].Feature.Status != domain.FeatureActive {
		t.Fatalf("derived status = %v, want active", list[0].Feature.Status)
	}
	if list[1].Feature.Status != domain.FeaturePlanned || list[1].Progress.Fraction != 0 {
		t.Fatalf("empty feature = %+v", list[1])
	}

	e.setStatus(t, ta.ID, domain.TaskDone)
	e.setStatus(t, tb.ID, domain.TaskCancelled)
	list, _ = e.svc.ListFeatures(projACtx, domain.Human, ref)
	if list[0].Feature.Status != domain.FeatureDone || list[0].Progress.Fraction != 1 {
		t.Fatalf("done feature = %+v", list[0])
	}
	if list[0].Counts[domain.TaskDone] != 1 || list[0].Counts[domain.TaskCancelled] != 1 {
		t.Fatalf("counts = %v", list[0].Counts)
	}
}

func TestProjA_CreateFeatureValidation(t *testing.T) {
	e := projANew(t)
	p, ref := e.project(t, "proj")
	for name, in := range map[string]CreateFeatureInput{
		"empty title":    {Title: " "},
		"weight too big": {Title: "x", Weight: 11},
		"negative":       {Title: "x", Weight: -1},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.svc.CreateFeature(projACtx, domain.Human, ref, in)
			projAKind(t, err, domain.KindValidation)
		})
	}
	if _, err := e.svc.CreateFeature(projACtx, domain.Human, ProjectRef{Slug: "nope"}, CreateFeatureInput{Title: "x"}); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("unknown project: %v", err)
	}

	if _, err := e.svc.ArchiveProject(projACtx, domain.Human, p.ID); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.CreateFeature(projACtx, domain.Human, ref, CreateFeatureInput{Title: "late"})
	projAKind(t, err, domain.KindValidation)
	if !strings.Contains(err.Error(), "archived") {
		t.Fatalf("error should say archived: %v", err)
	}
}

// --- tasks ---

func TestProjA_CreateTaskDefaultsAndIdempotency(t *testing.T) {
	e := projANew(t)
	p, ref := e.project(t, "proj")
	f := e.feature(t, ref, "F")

	task, created, err := e.svc.CreateTask(projACtx, domain.Human, ref, CreateTaskInput{
		FeatureID: f.ID, Title: "do it", AcceptanceCriteria: "works", IdempotencyKey: "k1",
	})
	if err != nil || !created {
		t.Fatalf("create = %+v, %v, %v", task, created, err)
	}
	if task.Status != domain.TaskTodo || task.Priority != 2 || task.Weight != 1 || task.Version != 1 ||
		task.ProjectID != p.ID || task.AcceptanceCriteria != "works" || task.Position != 1 {
		t.Fatalf("task = %+v", task)
	}

	again, created, err := e.svc.CreateTask(projACtx, domain.Human, ref, CreateTaskInput{FeatureID: f.ID, Title: "renamed", IdempotencyKey: "k1"})
	if err != nil || created || again.ID != task.ID || again.Title != "do it" {
		t.Fatalf("repeat = %+v, %v, %v", again, created, err)
	}
	var n int
	for _, ev := range e.events(t, p.ID) {
		if ev.Type == domain.EventTaskCreated {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("task.created events = %d, want 1", n)
	}

	// Priority 0 (urgent) is a real value, not "unset".
	zero := 0
	urgent, _, err := e.svc.CreateTask(projACtx, domain.Human, ref, CreateTaskInput{FeatureID: f.ID, Title: "fire", Priority: &zero, Weight: 5})
	if err != nil || urgent.Priority != 0 || urgent.Weight != 5 || urgent.Position != 2 {
		t.Fatalf("urgent = %+v, %v", urgent, err)
	}
}

func TestProjA_CreateTaskRepeatedKeySurvivesCancelledFeature(t *testing.T) {
	e := projANew(t)
	_, ref := e.project(t, "proj")
	f := e.feature(t, ref, "F")
	first, _, err := e.svc.CreateTask(projACtx, domain.Human, ref, CreateTaskInput{FeatureID: f.ID, Title: "t", IdempotencyKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.WriteTx(projACtx, func(tx *sql.Tx) error {
		_, err := store.CancelFeature(projACtx, tx, store.Meta{Actor: domain.Human, Now: e.now}, f.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	again, created, err := e.svc.CreateTask(projACtx, domain.Human, ref, CreateTaskInput{FeatureID: f.ID, Title: "t", IdempotencyKey: "k"})
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("retry after cancel = %+v, %v, %v", again, created, err)
	}
	// A new task in the cancelled feature is refused.
	_, _, err = e.svc.CreateTask(projACtx, domain.Human, ref, CreateTaskInput{FeatureID: f.ID, Title: "other"})
	projAKind(t, err, domain.KindValidation)
	if !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("error should say cancelled: %v", err)
	}
}

func TestProjA_CreateTaskValidation(t *testing.T) {
	e := projANew(t)
	p, ref := e.project(t, "proj")
	_, otherRef := e.project(t, "other")
	f := e.feature(t, ref, "F")
	otherFeature := e.feature(t, otherRef, "OF")
	high, neg := 4, -1

	cases := []struct {
		name string
		in   CreateTaskInput
		kind domain.ErrorKind
	}{
		{"empty title", CreateTaskInput{FeatureID: f.ID, Title: ""}, domain.KindValidation},
		{"priority high", CreateTaskInput{FeatureID: f.ID, Title: "x", Priority: &high}, domain.KindValidation},
		{"priority negative", CreateTaskInput{FeatureID: f.ID, Title: "x", Priority: &neg}, domain.KindValidation},
		{"weight", CreateTaskInput{FeatureID: f.ID, Title: "x", Weight: 11}, domain.KindValidation},
		{"feature of other project", CreateTaskInput{FeatureID: otherFeature.ID, Title: "x"}, domain.KindValidation},
		{"unknown feature", CreateTaskInput{FeatureID: 9999, Title: "x"}, domain.KindNotFound},
		{"no feature", CreateTaskInput{Title: "x"}, domain.KindNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, created, err := e.svc.CreateTask(projACtx, domain.Human, ref, c.in)
			projAKind(t, err, c.kind)
			if created {
				t.Fatal("created must be false on error")
			}
		})
	}

	// Nothing was written by the failed attempts.
	list, _ := e.svc.ListTasks(projACtx, domain.Human, ref, ListTasksFilter{})
	if len(list.Tasks) != 0 {
		t.Fatalf("tasks = %+v", list.Tasks)
	}

	if _, err := e.svc.ArchiveProject(projACtx, domain.Human, p.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err := e.svc.CreateTask(projACtx, domain.Human, ref, CreateTaskInput{FeatureID: f.ID, Title: "late"})
	projAKind(t, err, domain.KindValidation)
}

func TestProjA_GetTaskDetail(t *testing.T) {
	e := projANew(t)
	_, ref := e.project(t, "proj")
	f := e.feature(t, ref, "Feature")
	base := e.task(t, ref, f.ID, "base")
	mid := e.task(t, ref, f.ID, "mid")
	top := e.task(t, ref, f.ID, "top")
	e.addDep(t, mid.ID, base.ID)
	e.addDep(t, top.ID, mid.ID)
	err := e.db.WriteTx(projACtx, func(tx *sql.Tx) error {
		if _, err := store.AddTaskNote(projACtx, tx, store.Meta{Actor: domain.Human, Now: e.now}, mid.ID, "thinking"); err != nil {
			return err
		}
		_, err := store.AddArtifact(projACtx, tx, store.Meta{Actor: domain.Human, Now: e.now}, domain.Artifact{TaskID: mid.ID, Kind: domain.ArtifactLink, Ref: "http://x"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	d, err := e.svc.GetTask(projACtx, domain.Human, ref, mid.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Task.ID != mid.ID || d.Feature.ID != f.ID || d.Feature.Title != "Feature" {
		t.Fatalf("detail = %+v", d)
	}
	if len(d.Dependencies) != 1 || d.Dependencies[0].ID != base.ID {
		t.Fatalf("dependencies = %+v", d.Dependencies)
	}
	if len(d.Dependents) != 1 || d.Dependents[0].ID != top.ID {
		t.Fatalf("dependents = %+v", d.Dependents)
	}
	if len(d.Artifacts) != 1 || d.Artifacts[0].Ref != "http://x" {
		t.Fatalf("artifacts = %+v", d.Artifacts)
	}
	if d.Ready || len(d.UnmetDependencies) != 1 || d.UnmetDependencies[0] != base.ID {
		t.Fatalf("ready=%v unmet=%v", d.Ready, d.UnmetDependencies)
	}
	// Newest first: artifact, note, dependency added, created.
	if len(d.RecentEvents) < 3 || d.RecentEvents[0].ID < d.RecentEvents[1].ID {
		t.Fatalf("events not newest first: %+v", d.RecentEvents)
	}
	var sawNote, sawArtifact bool
	for _, ev := range d.RecentEvents {
		sawNote = sawNote || ev.Type == domain.EventTaskNote
		sawArtifact = sawArtifact || ev.Type == domain.EventArtifactAdded
	}
	if !sawNote || !sawArtifact {
		t.Fatalf("note=%v artifact=%v in %+v", sawNote, sawArtifact, d.RecentEvents)
	}

	// Once the dependency is done, mid becomes ready.
	e.setStatus(t, base.ID, domain.TaskInProgress)
	e.setStatus(t, base.ID, domain.TaskDone)
	d, _ = e.svc.GetTask(projACtx, domain.Human, ref, mid.ID)
	if !d.Ready || len(d.UnmetDependencies) != 0 {
		t.Fatalf("after dep done: ready=%v unmet=%v", d.Ready, d.UnmetDependencies)
	}
}

func TestProjA_GetTaskStale(t *testing.T) {
	e := projANew(t)
	_, ref := e.project(t, "proj")
	f := e.feature(t, ref, "F")
	task := e.task(t, ref, f.ID, "t")
	e.setStatus(t, task.ID, domain.TaskInProgress)

	e.now = e.now.Add(domain.StaleThreshold - time.Minute)
	if d, _ := e.svc.GetTask(projACtx, domain.Human, ref, task.ID); d.Stale {
		t.Fatal("task should not be stale yet")
	}
	e.now = e.now.Add(2 * time.Minute)
	if d, _ := e.svc.GetTask(projACtx, domain.Human, ref, task.ID); !d.Stale {
		t.Fatal("task should be stale after the threshold")
	}
}

func TestProjA_GetTaskScopedToProject(t *testing.T) {
	e := projANew(t)
	_, ref := e.project(t, "proj")
	_, otherRef := e.project(t, "other")
	f := e.feature(t, ref, "F")
	task := e.task(t, ref, f.ID, "t")

	_, err := e.svc.GetTask(projACtx, domain.Human, otherRef, task.ID)
	projAKind(t, err, domain.KindNotFound)
	_, err = e.svc.GetTask(projACtx, domain.Human, ref, 9999)
	projAKind(t, err, domain.KindNotFound)
}

func TestProjA_ListTasksFilters(t *testing.T) {
	e := projANew(t)
	_, ref := e.project(t, "proj")
	_, otherRef := e.project(t, "other")
	f1 := e.feature(t, ref, "F1")
	f2 := e.feature(t, ref, "F2")
	foreign := e.feature(t, otherRef, "foreign")

	a := e.task(t, ref, f1.ID, "a")
	b := e.task(t, ref, f1.ID, "b")
	c := e.task(t, ref, f2.ID, "c")
	d := e.task(t, ref, f2.ID, "d")
	e.task(t, otherRef, foreign.ID, "not mine")
	e.addDep(t, b.ID, a.ID)
	e.setStatus(t, c.ID, domain.TaskInProgress)
	e.setStatus(t, d.ID, domain.TaskBlocked)

	ids := func(l TaskList) []int64 {
		var out []int64
		for _, t := range l.Tasks {
			out = append(out, t.ID)
		}
		return out
	}
	same := func(got []int64, want ...int64) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	all := projAMust[TaskList](t)(e.svc.ListTasks(projACtx, domain.Human, ref, ListTasksFilter{}))
	if !same(ids(all), a.ID, b.ID, c.ID, d.ID) || all.Truncated {
		t.Fatalf("all = %v truncated=%v", ids(all), all.Truncated)
	}
	byFeature := projAMust[TaskList](t)(e.svc.ListTasks(projACtx, domain.Human, ref, ListTasksFilter{FeatureID: f2.ID}))
	if !same(ids(byFeature), c.ID, d.ID) {
		t.Fatalf("by feature = %v", ids(byFeature))
	}
	byStatus := projAMust[TaskList](t)(e.svc.ListTasks(projACtx, domain.Human, ref, ListTasksFilter{Statuses: []domain.TaskStatus{domain.TaskBlocked, domain.TaskInProgress}}))
	if !same(ids(byStatus), c.ID, d.ID) {
		t.Fatalf("by status = %v", ids(byStatus))
	}
	ready := projAMust[TaskList](t)(e.svc.ListTasks(projACtx, domain.Human, ref, ListTasksFilter{ReadyOnly: true}))
	if !same(ids(ready), a.ID) { // b waits on a; c and d are not todo
		t.Fatalf("ready = %v", ids(ready))
	}
	e.setStatus(t, a.ID, domain.TaskInProgress)
	e.setStatus(t, a.ID, domain.TaskDone)
	ready = projAMust[TaskList](t)(e.svc.ListTasks(projACtx, domain.Human, ref, ListTasksFilter{ReadyOnly: true}))
	if !same(ids(ready), b.ID) {
		t.Fatalf("ready after a done = %v", ids(ready))
	}

	_, err := e.svc.ListTasks(projACtx, domain.Human, ref, ListTasksFilter{Statuses: []domain.TaskStatus{"bogus"}})
	projAKind(t, err, domain.KindValidation)
	_, err = e.svc.ListTasks(projACtx, domain.Human, ref, ListTasksFilter{FeatureID: foreign.ID})
	projAKind(t, err, domain.KindNotFound)
	_, err = e.svc.ListTasks(projACtx, domain.Human, ref, ListTasksFilter{FeatureID: 9999})
	projAKind(t, err, domain.KindNotFound)
	_, err = e.svc.ListTasks(projACtx, domain.Human, ref, ListTasksFilter{Limit: -1})
	projAKind(t, err, domain.KindValidation)
	_, err = e.svc.ListTasks(projACtx, domain.Human, ref, ListTasksFilter{Offset: -1})
	projAKind(t, err, domain.KindValidation)
}

func TestProjA_ListTasksOrderAndTruncation(t *testing.T) {
	e := projANew(t)
	_, ref := e.project(t, "proj")
	f := e.feature(t, ref, "F")
	low, urgent := 3, 0
	lowTask, _, _ := e.svc.CreateTask(projACtx, domain.Human, ref, CreateTaskInput{FeatureID: f.ID, Title: "low", Priority: &low})
	normal := e.task(t, ref, f.ID, "normal")
	urgentTask, _, _ := e.svc.CreateTask(projACtx, domain.Human, ref, CreateTaskInput{FeatureID: f.ID, Title: "urgent", Priority: &urgent})

	list, err := e.svc.ListTasks(projACtx, domain.Human, ref, ListTasksFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tasks) != 3 || list.Tasks[0].ID != urgentTask.ID || list.Tasks[1].ID != normal.ID || list.Tasks[2].ID != lowTask.ID {
		t.Fatalf("priority order wrong: %+v", list.Tasks)
	}

	page1 := projAMust[TaskList](t)(e.svc.ListTasks(projACtx, domain.Human, ref, ListTasksFilter{Limit: 2}))
	if len(page1.Tasks) != 2 || !page1.Truncated {
		t.Fatalf("page1 = %d truncated=%v", len(page1.Tasks), page1.Truncated)
	}
	page2 := projAMust[TaskList](t)(e.svc.ListTasks(projACtx, domain.Human, ref, ListTasksFilter{Limit: 2, Offset: 2}))
	if len(page2.Tasks) != 1 || page2.Truncated || page2.Tasks[0].ID != lowTask.ID {
		t.Fatalf("page2 = %+v truncated=%v", page2.Tasks, page2.Truncated)
	}
	exact := projAMust[TaskList](t)(e.svc.ListTasks(projACtx, domain.Human, ref, ListTasksFilter{Limit: 3}))
	if len(exact.Tasks) != 3 || exact.Truncated {
		t.Fatalf("exact page = %d truncated=%v", len(exact.Tasks), exact.Truncated)
	}
}

func TestProjA_ListTasksCapsLimit(t *testing.T) {
	e := projANew(t)
	_, ref := e.project(t, "proj")
	f := e.feature(t, ref, "F")
	err := e.db.WriteTx(projACtx, func(tx *sql.Tx) error {
		for i := 0; i < MaxListLimit+5; i++ {
			if _, _, err := store.CreateTask(projACtx, tx, store.Meta{Actor: domain.Human, Now: e.now}, store.TaskInput{
				ProjectID: f.ProjectID, FeatureID: f.ID, Title: "t",
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	def := projAMust[TaskList](t)(e.svc.ListTasks(projACtx, domain.Human, ref, ListTasksFilter{}))
	if len(def.Tasks) != DefaultListLimit || !def.Truncated {
		t.Fatalf("default page = %d truncated=%v", len(def.Tasks), def.Truncated)
	}
	huge := projAMust[TaskList](t)(e.svc.ListTasks(projACtx, domain.Human, ref, ListTasksFilter{Limit: 10 * MaxListLimit}))
	if len(huge.Tasks) != MaxListLimit || !huge.Truncated {
		t.Fatalf("capped page = %d truncated=%v", len(huge.Tasks), huge.Truncated)
	}
}
