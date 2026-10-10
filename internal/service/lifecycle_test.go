package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"skald/internal/domain"
	"skald/internal/store"
)

var (
	lifeBctx   = context.Background()
	lifeBt0    = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	lifeBAgent = domain.NewAgentActor("claude-code", "s1")
)

type lifeBEnv struct {
	t       *testing.T
	svc     *Service
	db      *store.DB
	project domain.Project
	feature domain.Feature
}

func lifeBNew(t *testing.T) *lifeBEnv {
	t.Helper()
	db, err := store.Open(lifeBctx, filepath.Join(t.TempDir(), "db", "skald.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &lifeBEnv{t: t, db: db, svc: New(db, WithClock(func() time.Time { return lifeBt0 }))}
	m := store.Meta{Actor: domain.Human, Now: lifeBt0}
	e.write(func(tx *sql.Tx) (err error) {
		if e.project, err = store.CreateProject(lifeBctx, tx, m, domain.Project{Slug: "p", Name: "P"}); err != nil {
			return err
		}
		e.feature, err = store.CreateFeature(lifeBctx, tx, m, domain.Feature{ProjectID: e.project.ID, Title: "F"})
		return err
	})
	return e
}

func (e *lifeBEnv) write(fn func(*sql.Tx) error) {
	e.t.Helper()
	if err := e.db.WriteTx(lifeBctx, fn); err != nil {
		e.t.Fatal(err)
	}
}

func (e *lifeBEnv) task(title string, deps ...int64) domain.Task {
	e.t.Helper()
	var task domain.Task
	m := store.Meta{Actor: domain.Human, Now: lifeBt0}
	e.write(func(tx *sql.Tx) (err error) {
		task, _, err = store.CreateTask(lifeBctx, tx, m, store.TaskInput{ProjectID: e.project.ID, FeatureID: e.feature.ID, Title: title})
		if err != nil {
			return err
		}
		for _, d := range deps {
			if _, err = store.AddDependency(lifeBctx, tx, m, task.ID, d); err != nil {
				return err
			}
		}
		return nil
	})
	return task
}

func (e *lifeBEnv) get(id int64) domain.Task {
	e.t.Helper()
	var task domain.Task
	if err := e.db.ReadTx(lifeBctx, func(tx *sql.Tx) (err error) {
		task, err = store.GetTask(lifeBctx, tx, id)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return task
}

func (e *lifeBEnv) maxEvent() int64 {
	e.t.Helper()
	var id int64
	if err := e.db.ReadTx(lifeBctx, func(tx *sql.Tx) (err error) {
		id, err = store.MaxEventID(lifeBctx, tx, 0)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *lifeBEnv) events(since int64) []domain.Event {
	e.t.Helper()
	var evs []domain.Event
	if err := e.db.ReadTx(lifeBctx, func(tx *sql.Tx) (err error) {
		evs, err = store.EventsSince(lifeBctx, tx, 0, since, 0)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return evs
}

func (e *lifeBEnv) artifacts(id int64) []domain.Artifact {
	e.t.Helper()
	var as []domain.Artifact
	if err := e.db.ReadTx(lifeBctx, func(tx *sql.Tx) (err error) {
		as, err = store.ListArtifacts(lifeBctx, tx, id)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return as
}

func (e *lifeBEnv) setEvidence(on bool) {
	e.t.Helper()
	e.write(func(tx *sql.Tx) error {
		s := domain.DefaultSettings()
		s.RequireEvidence = on
		_, err := store.UpdateProjectSettings(lifeBctx, tx, store.Meta{Actor: domain.Human, Now: lifeBt0}, e.project.ID, s)
		return err
	})
}

func lifeBEvidence() ArtifactInput {
	return ArtifactInput{Kind: domain.ArtifactCommit, Ref: "abc123", Summary: "does the thing"}
}

func lifeBKind(t *testing.T, err error, want domain.ErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %v error, got nil", want)
	}
	if got := domain.KindOf(err); got != want {
		t.Fatalf("kind = %v, want %v (err: %v)", got, want, err)
	}
}

func TestLifeBStartAssignsActor(t *testing.T) {
	e := lifeBNew(t)
	task := e.task("a")
	got, err := e.svc.StartTask(lifeBctx, lifeBAgent, task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskInProgress || got.Assignee != string(lifeBAgent) || got.StartedAt == nil || got.Version != 2 {
		t.Fatalf("unexpected task %+v", got)
	}
}

func TestLifeBStartGatedByDependencies(t *testing.T) {
	e := lifeBNew(t)
	a := e.task("a")
	b := e.task("b", a.ID)

	_, err := e.svc.StartTask(lifeBctx, domain.Human, b.ID, 0)
	lifeBKind(t, err, domain.KindInvalidTransition)
	var ite *domain.InvalidTransitionError
	if !errors.As(err, &ite) || len(ite.UnmetDeps) != 1 || ite.UnmetDeps[0] != a.ID {
		t.Fatalf("expected unmet deps [%d], got %+v", a.ID, ite)
	}
	if got := e.get(b.ID); got.Status != domain.TaskTodo || got.Version != 1 {
		t.Fatalf("failed start must not change the task: %+v", got)
	}

	e.svc.StartTask(lifeBctx, domain.Human, a.ID, 0)
	if _, err := e.svc.CompleteTask(lifeBctx, domain.Human, a.ID, CompleteOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.StartTask(lifeBctx, domain.Human, b.ID, 0); err != nil {
		t.Fatalf("start after dependency done: %v", err)
	}
}

func TestLifeBStartDiamond(t *testing.T) {
	e := lifeBNew(t)
	top := e.task("top")
	left := e.task("left", top.ID)
	right := e.task("right", top.ID)
	bottom := e.task("bottom", left.ID, right.ID)

	finish := func(id int64) {
		t.Helper()
		if _, err := e.svc.StartTask(lifeBctx, domain.Human, id, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.CompleteTask(lifeBctx, domain.Human, id, CompleteOptions{Force: true}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int64{left.ID, right.ID, bottom.ID} {
		_, err := e.svc.StartTask(lifeBctx, domain.Human, id, 0)
		lifeBKind(t, err, domain.KindInvalidTransition)
	}
	finish(top.ID)
	finish(left.ID)
	// bottom still waits for right.
	_, err := e.svc.StartTask(lifeBctx, domain.Human, bottom.ID, 0)
	var ite *domain.InvalidTransitionError
	if !errors.As(err, &ite) || len(ite.UnmetDeps) != 1 || ite.UnmetDeps[0] != right.ID {
		t.Fatalf("expected only right unmet, got %v", err)
	}
	finish(right.ID)
	finish(bottom.ID)
}

func TestLifeBCancelledDependencyIsSatisfied(t *testing.T) {
	e := lifeBNew(t)
	a := e.task("a")
	b := e.task("b", a.ID)
	if _, err := e.svc.UpdateTaskStatus(lifeBctx, domain.Human, a.ID, StatusChange{To: domain.TaskCancelled}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.StartTask(lifeBctx, domain.Human, b.ID, 0); err != nil {
		t.Fatalf("cancelled dependency should not block: %v", err)
	}
}

func TestLifeBBlockUnblockReopenCancel(t *testing.T) {
	e := lifeBNew(t)
	task := e.task("a")

	_, err := e.svc.UpdateTaskStatus(lifeBctx, domain.Human, task.ID, StatusChange{To: domain.TaskBlocked})
	lifeBKind(t, err, domain.KindValidation)

	got, err := e.svc.UpdateTaskStatus(lifeBctx, lifeBAgent, task.ID, StatusChange{To: domain.TaskBlocked, BlockedReason: "waiting on API key"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskBlocked || got.BlockedReason != "waiting on API key" {
		t.Fatalf("unexpected %+v", got)
	}

	got, err = e.svc.UpdateTaskStatus(lifeBctx, lifeBAgent, task.ID, StatusChange{To: domain.TaskInProgress})
	if err != nil {
		t.Fatal(err)
	}
	if got.BlockedReason != "" || got.Assignee != string(lifeBAgent) {
		t.Fatalf("unblock should clear reason: %+v", got)
	}

	if _, err := e.svc.CompleteTask(lifeBctx, domain.Human, task.ID, CompleteOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	got, err = e.svc.UpdateTaskStatus(lifeBctx, domain.Human, task.ID, StatusChange{To: domain.TaskTodo})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskTodo || got.CompletedAt != nil || got.Assignee != "" {
		t.Fatalf("reopen should reset: %+v", got)
	}

	got, err = e.svc.UpdateTaskStatus(lifeBctx, domain.Human, task.ID, StatusChange{To: domain.TaskCancelled})
	if err != nil || got.Status != domain.TaskCancelled {
		t.Fatalf("cancel: %v %+v", err, got)
	}
	// Cancelled can only be reopened.
	_, err = e.svc.UpdateTaskStatus(lifeBctx, domain.Human, task.ID, StatusChange{To: domain.TaskDone})
	lifeBKind(t, err, domain.KindInvalidTransition)
	_, err = e.svc.UpdateTaskStatus(lifeBctx, domain.Human, task.ID, StatusChange{To: domain.TaskTodo})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLifeBInvalidTransitionAndNotFound(t *testing.T) {
	e := lifeBNew(t)
	task := e.task("a")
	_, err := e.svc.UpdateTaskStatus(lifeBctx, domain.Human, task.ID, StatusChange{To: domain.TaskDone})
	lifeBKind(t, err, domain.KindInvalidTransition)
	_, err = e.svc.UpdateTaskStatus(lifeBctx, domain.Human, task.ID, StatusChange{To: "weird"})
	lifeBKind(t, err, domain.KindValidation)
	_, err = e.svc.StartTask(lifeBctx, domain.Human, 9999, 0)
	lifeBKind(t, err, domain.KindNotFound)
	_, err = e.svc.AddTaskNote(lifeBctx, domain.Human, 9999, "x")
	lifeBKind(t, err, domain.KindNotFound)
	_, err = e.svc.RecordArtifact(lifeBctx, domain.Human, 9999, lifeBEvidence())
	lifeBKind(t, err, domain.KindNotFound)
}

func TestLifeBVersionConflict(t *testing.T) {
	e := lifeBNew(t)
	task := e.task("a")
	if _, err := e.svc.StartTask(lifeBctx, domain.Human, task.ID, task.Version); err != nil {
		t.Fatal(err)
	}
	// A second caller still holding version 1 loses.
	_, err := e.svc.UpdateTaskStatus(lifeBctx, domain.Human, task.ID, StatusChange{To: domain.TaskBlocked, BlockedReason: "x", ExpectedVersion: task.Version})
	lifeBKind(t, err, domain.KindConflict)
	var ce *domain.ConflictError
	if !errors.As(err, &ce) || ce.Expected != 1 || ce.Current != 2 || ce.CurrentTask == nil || ce.CurrentTask.Status != domain.TaskInProgress {
		t.Fatalf("conflict should carry versions: %+v", ce)
	}
	if got := e.get(task.ID); got.Status != domain.TaskInProgress {
		t.Fatalf("conflicting change must not apply: %+v", got)
	}
	// With the current version it works.
	if _, err := e.svc.UpdateTaskStatus(lifeBctx, domain.Human, task.ID, StatusChange{To: domain.TaskBlocked, BlockedReason: "x", ExpectedVersion: 2}); err != nil {
		t.Fatal(err)
	}
}

func TestLifeBNotes(t *testing.T) {
	e := lifeBNew(t)
	task := e.task("a")
	before := e.maxEvent()
	id, err := e.svc.AddTaskNote(lifeBctx, lifeBAgent, task.ID, "halfway there")
	if err != nil || id == 0 {
		t.Fatalf("note: %v %d", err, id)
	}
	evs := e.events(before)
	if len(evs) != 1 || evs[0].Type != domain.EventTaskNote || evs[0].Actor != lifeBAgent {
		t.Fatalf("unexpected events %+v", evs)
	}
	_, err = e.svc.AddTaskNote(lifeBctx, lifeBAgent, task.ID, "  ")
	lifeBKind(t, err, domain.KindValidation)
}

func TestLifeBDependencies(t *testing.T) {
	e := lifeBNew(t)
	a := e.task("a")
	b := e.task("b")
	c := e.task("c")

	added, err := e.svc.AddDependency(lifeBctx, domain.Human, b.ID, a.ID)
	if err != nil || !added {
		t.Fatalf("add: %v %v", err, added)
	}
	if added, err = e.svc.AddDependency(lifeBctx, domain.Human, b.ID, a.ID); err != nil || added {
		t.Fatalf("duplicate add should be a no-op: %v %v", err, added)
	}
	if _, err = e.svc.AddDependency(lifeBctx, domain.Human, c.ID, b.ID); err != nil {
		t.Fatal(err)
	}

	// a -> c would close a -> c -> b -> a.
	_, err = e.svc.AddDependency(lifeBctx, domain.Human, a.ID, c.ID)
	lifeBKind(t, err, domain.KindDependencyCycle)
	var cyc *domain.DependencyCycleError
	if !errors.As(err, &cyc) || len(cyc.Path) < 3 || cyc.Path[0] != a.ID || cyc.Path[len(cyc.Path)-1] != a.ID {
		t.Fatalf("cycle path should start and end at the task: %+v", cyc)
	}

	_, err = e.svc.AddDependency(lifeBctx, domain.Human, a.ID, a.ID)
	lifeBKind(t, err, domain.KindValidation)

	// Different project is rejected.
	var other domain.Task
	e.write(func(tx *sql.Tx) error {
		m := store.Meta{Actor: domain.Human, Now: lifeBt0}
		p2, err := store.CreateProject(lifeBctx, tx, m, domain.Project{Slug: "q", Name: "Q"})
		if err != nil {
			return err
		}
		f2, err := store.CreateFeature(lifeBctx, tx, m, domain.Feature{ProjectID: p2.ID, Title: "F2"})
		if err != nil {
			return err
		}
		other, _, err = store.CreateTask(lifeBctx, tx, m, store.TaskInput{ProjectID: p2.ID, FeatureID: f2.ID, Title: "x"})
		return err
	})
	_, err = e.svc.AddDependency(lifeBctx, domain.Human, a.ID, other.ID)
	lifeBKind(t, err, domain.KindValidation)

	removed, err := e.svc.RemoveDependency(lifeBctx, domain.Human, b.ID, a.ID)
	if err != nil || !removed {
		t.Fatalf("remove: %v %v", err, removed)
	}
	if removed, err = e.svc.RemoveDependency(lifeBctx, domain.Human, b.ID, a.ID); err != nil || removed {
		t.Fatalf("second remove should be a no-op: %v %v", err, removed)
	}
	// With the edge gone, the former cycle is fine.
	if _, err := e.svc.AddDependency(lifeBctx, domain.Human, a.ID, c.ID); err != nil {
		t.Fatalf("a depends on c after b->a removed: %v", err)
	}
}

func TestLifeBRecordArtifact(t *testing.T) {
	e := lifeBNew(t)
	task := e.task("a")

	a, err := e.svc.RecordArtifact(lifeBctx, lifeBAgent, task.ID, lifeBEvidence())
	if err != nil {
		t.Fatal(err)
	}
	if a.TaskID != task.ID || a.Kind != domain.ArtifactCommit || a.Actor != lifeBAgent {
		t.Fatalf("unexpected %+v", a)
	}
	_, err = e.svc.RecordArtifact(lifeBctx, lifeBAgent, task.ID, ArtifactInput{Kind: "bogus", Ref: "x"})
	lifeBKind(t, err, domain.KindValidation)
	_, err = e.svc.RecordArtifact(lifeBctx, lifeBAgent, task.ID, ArtifactInput{Kind: domain.ArtifactLink, Ref: ""})
	lifeBKind(t, err, domain.KindValidation)

	e.svc.UpdateTaskStatus(lifeBctx, domain.Human, task.ID, StatusChange{To: domain.TaskCancelled})
	_, err = e.svc.RecordArtifact(lifeBctx, lifeBAgent, task.ID, lifeBEvidence())
	lifeBKind(t, err, domain.KindValidation)
	if n := len(e.artifacts(task.ID)); n != 1 {
		t.Fatalf("cancelled task should keep 1 artifact, has %d", n)
	}
}

func TestLifeBEvidenceGate(t *testing.T) {
	e := lifeBNew(t)
	task := e.task("a")
	e.svc.StartTask(lifeBctx, lifeBAgent, task.ID, 0)

	_, err := e.svc.CompleteTask(lifeBctx, lifeBAgent, task.ID, CompleteOptions{})
	lifeBKind(t, err, domain.KindEvidenceRequired)
	if got := e.get(task.ID); got.Status != domain.TaskInProgress {
		t.Fatalf("gate failure must not change status: %+v", got)
	}

	if _, err := e.svc.RecordArtifact(lifeBctx, lifeBAgent, task.ID, lifeBEvidence()); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.CompleteTask(lifeBctx, lifeBAgent, task.ID, CompleteOptions{})
	if err != nil || got.Status != domain.TaskDone || got.CompletedAt == nil {
		t.Fatalf("complete with evidence: %v %+v", err, got)
	}
}

func TestLifeBAgentCannotForce(t *testing.T) {
	e := lifeBNew(t)
	task := e.task("a")
	e.svc.StartTask(lifeBctx, lifeBAgent, task.ID, 0)
	before := e.maxEvent()

	_, err := e.svc.CompleteTask(lifeBctx, lifeBAgent, task.ID, CompleteOptions{Force: true})
	lifeBKind(t, err, domain.KindEvidenceRequired)
	_, err = e.svc.UpdateTaskStatus(lifeBctx, lifeBAgent, task.ID, StatusChange{To: domain.TaskDone, Force: true})
	lifeBKind(t, err, domain.KindEvidenceRequired)
	_, _, err = e.svc.ReportTaskResult(lifeBctx, lifeBAgent, task.ID, TaskResult{Summary: "done!", Force: true})
	lifeBKind(t, err, domain.KindEvidenceRequired)

	if got := e.get(task.ID); got.Status != domain.TaskInProgress {
		t.Fatalf("agent force must not complete: %+v", got)
	}
	if evs := e.events(before); len(evs) != 0 {
		t.Fatalf("rejected attempts must write nothing, got %d events", len(evs))
	}
}

func TestLifeBForcedCompletionRecordedInEvent(t *testing.T) {
	e := lifeBNew(t)
	task := e.task("a")
	e.svc.StartTask(lifeBctx, domain.Human, task.ID, 0)
	before := e.maxEvent()

	got, err := e.svc.CompleteTask(lifeBctx, domain.Human, task.ID, CompleteOptions{Force: true, Note: "verified by hand"})
	if err != nil || got.Status != domain.TaskDone {
		t.Fatalf("forced complete: %v %+v", err, got)
	}
	evs := e.events(before)
	if len(evs) != 1 || evs[0].Type != domain.EventTaskStatus {
		t.Fatalf("expected one status event, got %+v", evs)
	}
	var payload map[string]any
	if err := json.Unmarshal(evs[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["forced"] != true || payload["note"] != "verified by hand" || payload["to"] != "done" {
		t.Fatalf("payload should record the override: %v", payload)
	}
}

func TestLifeBForceNotRecordedWhenNotNeeded(t *testing.T) {
	e := lifeBNew(t)
	task := e.task("a")
	e.svc.StartTask(lifeBctx, domain.Human, task.ID, 0)
	e.svc.RecordArtifact(lifeBctx, domain.Human, task.ID, lifeBEvidence())
	before := e.maxEvent()
	if _, err := e.svc.CompleteTask(lifeBctx, domain.Human, task.ID, CompleteOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	json.Unmarshal(e.events(before)[0].Payload, &payload)
	if _, ok := payload["forced"]; ok {
		t.Fatalf("force was not used, event must not claim it: %v", payload)
	}
}

func TestLifeBEvidenceSettingOff(t *testing.T) {
	e := lifeBNew(t)
	e.setEvidence(false)
	task := e.task("a")
	e.svc.StartTask(lifeBctx, lifeBAgent, task.ID, 0)
	if _, err := e.svc.CompleteTask(lifeBctx, lifeBAgent, task.ID, CompleteOptions{}); err != nil {
		t.Fatalf("no evidence needed when setting is off: %v", err)
	}
}

func TestLifeBCompleteRequiresInProgress(t *testing.T) {
	e := lifeBNew(t)
	task := e.task("a")
	e.svc.RecordArtifact(lifeBctx, domain.Human, task.ID, lifeBEvidence())
	_, err := e.svc.CompleteTask(lifeBctx, domain.Human, task.ID, CompleteOptions{})
	lifeBKind(t, err, domain.KindInvalidTransition)
}

func TestLifeBReportTaskResult(t *testing.T) {
	e := lifeBNew(t)
	task := e.task("a")
	e.svc.StartTask(lifeBctx, lifeBAgent, task.ID, 0)
	before := e.maxEvent()

	got, arts, err := e.svc.ReportTaskResult(lifeBctx, lifeBAgent, task.ID, TaskResult{
		Summary:   "implemented and tested",
		Artifacts: []ArtifactInput{lifeBEvidence(), {Kind: domain.ArtifactTestReport, Ref: "go test ./... ok"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskDone || len(arts) != 2 {
		t.Fatalf("unexpected result %+v %+v", got, arts)
	}
	var types []domain.EventType
	for _, ev := range e.events(before) {
		types = append(types, ev.Type)
	}
	want := []domain.EventType{domain.EventTaskNote, domain.EventArtifactAdded, domain.EventArtifactAdded, domain.EventTaskStatus}
	if len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events = %v, want %v", types, want)
		}
	}
}

func TestLifeBReportTaskResultIsAtomic(t *testing.T) {
	e := lifeBNew(t)

	check := func(name string, id int64, r TaskResult, kind domain.ErrorKind, wantStatus domain.TaskStatus) {
		t.Helper()
		before := e.maxEvent()
		verBefore := e.get(id).Version
		_, _, err := e.svc.ReportTaskResult(lifeBctx, lifeBAgent, id, r)
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
		lifeBKind(t, err, kind)
		if evs := e.events(before); len(evs) != 0 {
			t.Fatalf("%s: rollback expected, found %d events", name, len(evs))
		}
		if n := len(e.artifacts(id)); n != 0 {
			t.Fatalf("%s: rollback expected, found %d artifacts", name, n)
		}
		got := e.get(id)
		if got.Status != wantStatus || got.Version != verBefore || !got.UpdatedAt.Equal(lifeBt0) {
			t.Fatalf("%s: task changed after failed report: %+v", name, got)
		}
	}

	// Not in progress: note and artifacts succeed first, completion fails.
	todo := e.task("todo")
	check("invalid transition", todo.ID, TaskResult{Summary: "s", Artifacts: []ArtifactInput{lifeBEvidence()}}, domain.KindInvalidTransition, domain.TaskTodo)

	// Bad second artifact after a good note and artifact.
	run := e.task("run")
	e.svc.StartTask(lifeBctx, lifeBAgent, run.ID, 0)
	check("bad artifact", run.ID, TaskResult{Summary: "s", Artifacts: []ArtifactInput{lifeBEvidence(), {Kind: "bogus", Ref: "x"}}}, domain.KindValidation, domain.TaskInProgress)

	// No evidence at all: note is written, then the gate fails.
	check("no evidence", run.ID, TaskResult{Summary: "claims it works"}, domain.KindEvidenceRequired, domain.TaskInProgress)

	// Stale version.
	check("conflict", run.ID, TaskResult{Summary: "s", Artifacts: []ArtifactInput{lifeBEvidence()}, ExpectedVersion: 1}, domain.KindConflict, domain.TaskInProgress)

	// Cancelled task refuses evidence.
	gone := e.task("gone")
	e.svc.UpdateTaskStatus(lifeBctx, domain.Human, gone.ID, StatusChange{To: domain.TaskCancelled})
	check("cancelled", gone.ID, TaskResult{Artifacts: []ArtifactInput{lifeBEvidence()}}, domain.KindValidation, domain.TaskCancelled)
}

func TestLifeBReportTaskResultNotFound(t *testing.T) {
	e := lifeBNew(t)
	_, _, err := e.svc.ReportTaskResult(lifeBctx, lifeBAgent, 4242, TaskResult{Summary: "x"})
	lifeBKind(t, err, domain.KindNotFound)
}

func TestLifeBEveryMutationWritesEvent(t *testing.T) {
	e := lifeBNew(t)
	a := e.task("a")
	b := e.task("b")
	before := e.maxEvent()
	e.svc.AddDependency(lifeBctx, domain.Human, b.ID, a.ID)
	e.svc.StartTask(lifeBctx, domain.Human, a.ID, 0)
	e.svc.AddTaskNote(lifeBctx, domain.Human, a.ID, "n")
	e.svc.RecordArtifact(lifeBctx, domain.Human, a.ID, lifeBEvidence())
	e.svc.CompleteTask(lifeBctx, domain.Human, a.ID, CompleteOptions{})
	e.svc.RemoveDependency(lifeBctx, domain.Human, b.ID, a.ID)
	if n := len(e.events(before)); n != 6 {
		t.Fatalf("expected 6 events, got %d", n)
	}
}
