package service

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"skald/internal/domain"
	"skald/internal/store"
)

var (
	nextCCtx = context.Background()
	nextCT0  = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
)

func nextCMeta(actor domain.Actor, d time.Duration) store.Meta {
	return store.Meta{Actor: actor, Now: nextCT0.Add(d)}
}

// nextCEnv is a service over a fresh database with one project, "demo".
type nextCEnv struct {
	t   *testing.T
	svc *Service
	db  *store.DB
	p   domain.Project
}

func nextCNew(t *testing.T) *nextCEnv {
	t.Helper()
	db, err := store.Open(nextCCtx, filepath.Join(t.TempDir(), "db", "skald.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc := New(db,
		WithClock(func() time.Time { return nextCT0 }),
		WithEnv(func(string) string { return "" }),
		WithWorkdir(func() (string, error) { return "/definitely/not/a/project", nil }),
	)
	e := &nextCEnv{t: t, svc: svc, db: db}
	e.write(func(tx *sql.Tx) (err error) {
		e.p, err = store.CreateProject(nextCCtx, tx, nextCMeta(domain.Human, 0), domain.Project{Slug: "demo", Name: "Demo"})
		return
	})
	return e
}

func (e *nextCEnv) write(fn func(tx *sql.Tx) error) {
	e.t.Helper()
	if err := e.db.WriteTx(nextCCtx, fn); err != nil {
		e.t.Fatal(err)
	}
}

func (e *nextCEnv) feature(title string, weight, position int) domain.Feature {
	e.t.Helper()
	var f domain.Feature
	e.write(func(tx *sql.Tx) (err error) {
		f, err = store.CreateFeature(nextCCtx, tx, nextCMeta(domain.Human, 0), domain.Feature{ProjectID: e.p.ID, Title: title, Weight: weight, Position: position})
		return
	})
	return f
}

func (e *nextCEnv) task(f domain.Feature, title string, weight int) domain.Task {
	e.t.Helper()
	return e.taskWith(store.TaskInput{FeatureID: f.ID, Title: title, Weight: weight})
}

func (e *nextCEnv) taskWith(in store.TaskInput) domain.Task {
	e.t.Helper()
	in.ProjectID = e.p.ID
	var task domain.Task
	e.write(func(tx *sql.Tx) (err error) {
		task, _, err = store.CreateTask(nextCCtx, tx, nextCMeta(domain.Human, 0), in)
		return
	})
	return task
}

func (e *nextCEnv) setStatus(task domain.Task, to domain.TaskStatus) {
	e.t.Helper()
	e.setStatusAt(task, to, 0, "")
}

func (e *nextCEnv) setStatusAt(task domain.Task, to domain.TaskStatus, at time.Duration, reason string) {
	e.t.Helper()
	e.write(func(tx *sql.Tx) error {
		_, err := store.SetTaskStatus(nextCCtx, tx, nextCMeta(domain.Human, at), store.TaskStatusChange{TaskID: task.ID, To: to, BlockedReason: reason})
		return err
	})
}

func (e *nextCEnv) dep(task, dependsOn domain.Task) {
	e.t.Helper()
	e.write(func(tx *sql.Tx) error {
		_, err := store.AddDependency(nextCCtx, tx, nextCMeta(domain.Human, 0), task.ID, dependsOn.ID)
		return err
	})
}

func nextCTitles(tasks []domain.Task) []string {
	out := make([]string, len(tasks))
	for i, t := range tasks {
		out[i] = t.Title
	}
	return out
}

func nextCEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func nextCNext(t *testing.T, e *nextCEnv, limit int) NextTasksResult {
	t.Helper()
	res, err := e.svc.NextTasks(nextCCtx, ProjectRef{Slug: "demo"}, limit)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestNextCOrdering(t *testing.T) {
	e := nextCNew(t)
	f1 := e.feature("F1", 1, 1)
	f2 := e.feature("F2", 1, 2)
	urgent, low := 0, 3

	e.taskWith(store.TaskInput{FeatureID: f1.ID, Title: "f1-normal-pos2", Position: 2})
	e.taskWith(store.TaskInput{FeatureID: f1.ID, Title: "f1-normal-pos1", Position: 1})
	e.taskWith(store.TaskInput{FeatureID: f2.ID, Title: "f2-normal-pos1", Position: 1})
	e.taskWith(store.TaskInput{FeatureID: f2.ID, Title: "f2-urgent", Priority: &urgent, Position: 9})
	e.taskWith(store.TaskInput{FeatureID: f1.ID, Title: "f1-low", Priority: &low, Position: 1})
	// Same priority, feature and explicit position: creation order (ID) decides.
	e.taskWith(store.TaskInput{FeatureID: f1.ID, Title: "tie-a", Position: 5})
	e.taskWith(store.TaskInput{FeatureID: f1.ID, Title: "tie-b", Position: 5})

	res := nextCNext(t, e, 50)
	nextCEqual(t, nextCTitles(res.Tasks), []string{
		"f2-urgent", "f1-normal-pos1", "f1-normal-pos2", "tie-a", "tie-b", "f2-normal-pos1", "f1-low",
	})
	if res.Total != 7 || res.Truncated {
		t.Fatalf("total=%d truncated=%v", res.Total, res.Truncated)
	}
	if res.Project.Slug != "demo" {
		t.Fatalf("project = %q", res.Project.Slug)
	}
}

func TestNextCFeaturePositionBeatsFeatureID(t *testing.T) {
	e := nextCNew(t)
	late := e.feature("created-first", 1, 2)
	early := e.feature("created-second", 1, 1)
	e.task(late, "late", 1)
	e.task(early, "early", 1)
	nextCEqual(t, nextCTitles(nextCNext(t, e, 10).Tasks), []string{"early", "late"})
}

func TestNextCTruncation(t *testing.T) {
	e := nextCNew(t)
	f := e.feature("F", 1, 1)
	for _, title := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		e.task(f, title, 1)
	}
	res := nextCNext(t, e, 3)
	nextCEqual(t, nextCTitles(res.Tasks), []string{"a", "b", "c"})
	if res.Total != 7 || !res.Truncated {
		t.Fatalf("total=%d truncated=%v", res.Total, res.Truncated)
	}
	if res := nextCNext(t, e, 7); res.Truncated || len(res.Tasks) != 7 {
		t.Fatalf("limit == total must not truncate: %d tasks, truncated=%v", len(res.Tasks), res.Truncated)
	}
	if res := nextCNext(t, e, 0); len(res.Tasks) != 7 {
		t.Fatalf("default limit should return all 7, got %d", len(res.Tasks))
	}
	if res := nextCNext(t, e, 10_000); len(res.Tasks) != 7 {
		t.Fatalf("oversized limit is capped, not an error: %d", len(res.Tasks))
	}
}

func TestNextCDiamondDependencies(t *testing.T) {
	e := nextCNew(t)
	f := e.feature("F", 1, 1)
	a, b, c, d := e.task(f, "A", 1), e.task(f, "B", 1), e.task(f, "C", 1), e.task(f, "D", 1)
	e.dep(b, a)
	e.dep(c, a)
	e.dep(d, b)
	e.dep(d, c)

	nextCEqual(t, nextCTitles(nextCNext(t, e, 10).Tasks), []string{"A"})

	e.setStatus(a, domain.TaskInProgress)
	if got := nextCNext(t, e, 10); got.Total != 0 {
		t.Fatalf("in-progress dependency keeps dependents waiting: %v", nextCTitles(got.Tasks))
	}

	e.setStatus(a, domain.TaskDone)
	nextCEqual(t, nextCTitles(nextCNext(t, e, 10).Tasks), []string{"B", "C"})

	e.setStatus(b, domain.TaskDone)
	nextCEqual(t, nextCTitles(nextCNext(t, e, 10).Tasks), []string{"C"})

	e.setStatus(c, domain.TaskDone)
	nextCEqual(t, nextCTitles(nextCNext(t, e, 10).Tasks), []string{"D"})
}

func TestNextCCancelledDependencyUnblocks(t *testing.T) {
	e := nextCNew(t)
	f := e.feature("F", 1, 1)
	a, b := e.task(f, "A", 1), e.task(f, "B", 1)
	e.dep(b, a)
	e.setStatus(a, domain.TaskCancelled)
	nextCEqual(t, nextCTitles(nextCNext(t, e, 10).Tasks), []string{"B"})
}

func TestNextCBlockedAndDoneAreNotReady(t *testing.T) {
	e := nextCNew(t)
	f := e.feature("F", 1, 1)
	todo, blocked, done, started := e.task(f, "todo", 1), e.task(f, "blocked", 1), e.task(f, "done", 1), e.task(f, "started", 1)
	_ = todo
	e.setStatusAt(blocked, domain.TaskBlocked, 0, "waiting on API key")
	e.setStatus(done, domain.TaskDone)
	e.setStatus(started, domain.TaskInProgress)
	nextCEqual(t, nextCTitles(nextCNext(t, e, 10).Tasks), []string{"todo"})
}

func TestNextCUnknownProject(t *testing.T) {
	e := nextCNew(t)
	_, err := e.svc.NextTasks(nextCCtx, ProjectRef{Slug: "nope"}, 5)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func nextCClose(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNextCProgressMatchesHandComputedNumbers(t *testing.T) {
	e := nextCNew(t)

	// A (w3): tasks done w1, todo w1, done w2 -> 3/4.
	a := e.feature("A", 3, 1)
	e.setStatus(e.task(a, "a1", 1), domain.TaskDone)
	e.task(a, "a2", 1)
	e.setStatus(e.task(a, "a3", 2), domain.TaskDone)
	// B (w1): one todo -> 0.
	b := e.feature("B", 1, 2)
	e.task(b, "b1", 1)
	// C (w2): no tasks -> counted at 0%.
	e.feature("C", 2, 3)
	// D (w5): every task cancelled -> excluded.
	d := e.feature("D", 5, 4)
	e.setStatus(e.task(d, "d1", 1), domain.TaskCancelled)
	// E (w4): cancelled feature -> excluded.
	cancelled := e.feature("E", 4, 5)
	e.task(cancelled, "e1", 1)
	e.write(func(tx *sql.Tx) error {
		_, err := store.CancelFeature(nextCCtx, tx, nextCMeta(domain.Human, 0), cancelled.ID)
		return err
	})
	// F (w2): one done task -> 100%, done.
	f := e.feature("F", 2, 6)
	e.setStatus(e.task(f, "f1", 1), domain.TaskDone)

	rep, err := e.svc.ProjectProgress(nextCCtx, ProjectRef{Slug: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	// Counted features: A(3, .75), B(1, 0), C(2, 0), F(2, 1) -> (2.25+0+0+2)/8.
	nextCClose(t, rep.Progress.Fraction, 4.25/8)
	if rep.Progress.Total != 8 || rep.Progress.Done != 2 {
		t.Fatalf("progress = %+v, want Total 8, Done 2", rep.Progress)
	}

	if len(rep.Features) != 6 {
		t.Fatalf("features = %d, want 6", len(rep.Features))
	}
	want := []struct {
		title    string
		status   domain.FeatureStatus
		fraction float64
		counted  bool
		counts   TaskCounts
	}{
		{"A", domain.FeatureActive, 0.75, true, TaskCounts{Todo: 1, Done: 2, Total: 3}},
		{"B", domain.FeaturePlanned, 0, true, TaskCounts{Todo: 1, Total: 1}},
		{"C", domain.FeaturePlanned, 0, true, TaskCounts{}},
		{"D", domain.FeaturePlanned, 0, false, TaskCounts{Cancelled: 1, Total: 1}},
		{"E", domain.FeatureCancelled, 0, false, TaskCounts{Todo: 1, Total: 1}},
		{"F", domain.FeatureDone, 1, true, TaskCounts{Done: 1, Total: 1}},
	}
	for i, w := range want {
		got := rep.Features[i]
		if got.Feature.Title != w.title || got.Status != w.status || got.Counted != w.counted || got.Counts != w.counts {
			t.Errorf("feature %s: got status=%s counted=%v counts=%+v", w.title, got.Status, got.Counted, got.Counts)
		}
		nextCClose(t, got.Progress.Fraction, w.fraction)
	}

	if rep.Counts != (TaskCounts{Todo: 3, Done: 3, Cancelled: 1, Total: 7}) {
		t.Fatalf("project counts = %+v", rep.Counts)
	}
	if rep.ReadyCount != 3 {
		t.Fatalf("ready = %d, want 3 (a2, b1, e1)", rep.ReadyCount)
	}
}

func TestNextCProgressEmptyProject(t *testing.T) {
	e := nextCNew(t)
	rep, err := e.svc.ProjectProgress(nextCCtx, ProjectRef{Slug: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Progress.Fraction != 0 || rep.Counts.Total != 0 || len(rep.Features) != 0 || rep.ReadyCount != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestNextCBlockersAndStale(t *testing.T) {
	e := nextCNew(t)
	f := e.feature("F", 1, 1)
	blocked := e.task(f, "blocked", 1)
	fresh := e.task(f, "fresh", 1)
	old := e.task(f, "old", 1)
	exact := e.task(f, "exact", 1)
	e.setStatusAt(blocked, domain.TaskBlocked, 0, "needs credentials")
	// The service clock is nextCT0. Touch times are relative to it.
	e.setStatusAt(fresh, domain.TaskInProgress, -1*time.Hour, "")
	e.setStatusAt(old, domain.TaskInProgress, -5*time.Hour, "")
	e.setStatusAt(exact, domain.TaskInProgress, -domain.StaleThreshold, "")

	rep, err := e.svc.ProjectProgress(nextCCtx, ProjectRef{Slug: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Blocked) != 1 || rep.Blocked[0].Task.Title != "blocked" || rep.Blocked[0].Reason != "needs credentials" {
		t.Fatalf("blocked = %+v", rep.Blocked)
	}
	got := nextCTitles(rep.Stale)
	if len(got) != 2 || !(got[0] == "old" && got[1] == "exact" || got[0] == "exact" && got[1] == "old") {
		t.Fatalf("stale = %v, want old and exact", got)
	}
}

func TestNextCSummaryIncludesDocuments(t *testing.T) {
	e := nextCNew(t)
	f := e.feature("F", 1, 1)
	e.task(f, "t", 1)
	if _, _, err := e.svc.PutDocument(nextCCtx, domain.Human, ProjectRef{Slug: "demo"}, "architecture", "Overview", "body"); err != nil {
		t.Fatal(err)
	}
	sum, err := e.svc.ProjectSummary(nextCCtx, ProjectRef{Slug: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Project.Slug != "demo" || sum.Counts.Total != 1 || sum.ReadyCount != 1 || len(sum.Features) != 1 {
		t.Fatalf("summary = %+v", sum.ProgressReport)
	}
	if len(sum.Documents) != 1 || sum.Documents[0].Title != "Overview" {
		t.Fatalf("documents = %+v", sum.Documents)
	}
}

// Every write below adds two tasks in one transaction, so a reader that sees
// one consistent snapshot always sees an even task count. A torn read would
// show an odd one.
func TestNextCSnapshotConsistency(t *testing.T) {
	e := nextCNew(t)
	f := e.feature("F", 1, 1)

	const rounds = 60
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			err := e.db.WriteTx(nextCCtx, func(tx *sql.Tx) error {
				for j := 0; j < 2; j++ {
					if _, _, err := store.CreateTask(nextCCtx, tx, nextCMeta(domain.Human, 0), store.TaskInput{ProjectID: e.p.ID, FeatureID: f.ID, Title: "t"}); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Errorf("write: %v", err)
				return
			}
		}
	}()

	for i := 0; i < 150; i++ {
		rep, err := e.svc.ProjectProgress(nextCCtx, ProjectRef{Slug: "demo"})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Counts.Total%2 != 0 {
			t.Fatalf("torn read: %d tasks", rep.Counts.Total)
		}
		if rep.Counts.Total != rep.Features[0].Counts.Total || rep.ReadyCount != rep.Counts.Todo {
			t.Fatalf("inconsistent report: %+v", rep.Counts)
		}
		next, err := e.svc.NextTasks(nextCCtx, ProjectRef{Slug: "demo"}, 50)
		if err != nil {
			t.Fatal(err)
		}
		if next.Total%2 != 0 {
			t.Fatalf("torn read in NextTasks: %d", next.Total)
		}
	}
	wg.Wait()
}
