package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"skald/internal/domain"
	"skald/internal/store"
)

// End-to-end tests for F2 (f2.e2e). They use the public service API only.

var e2eCtx = context.Background()

type e2eEnv struct {
	t    *testing.T
	svc  *Service
	now  time.Time
	repo string
}

func e2eNew(t *testing.T) *e2eEnv {
	t.Helper()
	e2eT = t
	e := &e2eEnv{t: t, now: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC), repo: t.TempDir()}
	db, err := store.Open(e2eCtx, filepath.Join(t.TempDir(), "db", "skald.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.svc = New(db,
		WithClock(func() time.Time { e.now = e.now.Add(time.Minute); return e.now }),
		WithEnv(func(string) string { return "" }),
		// The working directory is inside the linked repo, so resolution has to
		// walk up to find the project.
		WithWorkdir(func() (string, error) { return filepath.Join(e.repo, "sub"), nil }),
	)
	return e
}

// e2eT is the running test, set by e2eNew. These tests are not parallel, which
// is what lets e2eOK take a call's (value, error) pair directly.
var e2eT *testing.T

func e2eOK[T any](v T, err error) T {
	e2eT.Helper()
	if err != nil {
		e2eT.Fatal(err)
	}
	return v
}

func e2eNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *e2eEnv) project() domain.Project {
	e.t.Helper()
	return e2eOK(e.svc.CreateProject(e2eCtx, domain.Human, CreateProjectInput{
		Slug: "school", Name: "School Management", LinkCwd: true, Cwd: e.repo,
	}))
}

func e2eKey(ref string) string { return "key-" + ref }

// e2eTask builds a plan task whose idempotency key is derived from its ref.
func e2eTask(ref string, weight int, deps ...string) PlanTask {
	return PlanTask{Ref: ref, Title: "Task " + ref, Weight: weight, DependsOn: deps, IdempotencyKey: e2eKey(ref),
		AcceptanceCriteria: "works"}
}

// e2ePlan is 3 features and 10 tasks with a diamond (a -> b,c -> d) and
// cross-feature dependencies (f needs a, i needs d, g needs e and f).
func e2ePlan() PlanInput {
	return PlanInput{Features: []PlanFeature{
		{Title: "Foundation", Weight: 2, Tasks: []PlanTask{
			e2eTask("a", 1), e2eTask("b", 1, "a"), e2eTask("c", 1, "a"),
		}},
		{Title: "Core", Weight: 3, Tasks: []PlanTask{
			e2eTask("d", 2, "b", "c"), e2eTask("e", 1, "d"), e2eTask("f", 1, "a"),
		}},
		{Title: "Release", Weight: 1, Tasks: []PlanTask{
			e2eTask("g", 1, "e", "f"), e2eTask("h", 1, "g"), e2eTask("i", 1, "d"), e2eTask("j", 1),
		}},
	}}
}

const e2eEdges = 10 // number of dependencies in e2ePlan

func TestE2EFullWorkflow(t *testing.T) {
	e := e2eNew(t)
	agent := domain.NewAgentActor("claude-code", "s1")
	p := e.project()
	ref := ProjectRef{} // resolved from the working directory

	plan := e2eOK(e.svc.CreatePlan(e2eCtx, agent, ref, e2ePlan()))
	if plan.ProjectID != p.ID || plan.TasksCreated != 10 || plan.FeaturesCreated != 3 || plan.DependenciesAdded != e2eEdges {
		t.Fatalf("plan result = %+v", plan)
	}
	idOf := map[string]int64{}
	refOf := map[int64]string{}
	for r, tr := range plan.Tasks {
		idOf[r] = tr.TaskID
		refOf[tr.TaskID] = r
	}

	// Work until nothing is ready. Task number n decides who does it and how:
	// the agent uses the individual calls for even n and ReportTaskResult for
	// every third; the human takes the rest.
	done := 0
	order := []string{}
	for round := 0; round < 20; round++ {
		next := e2eOK(e.svc.NextTasks(e2eCtx, ref, 50))
		if len(next.Tasks) == 0 {
			break
		}
		// Everything NextTasks returns must be startable now.
		task := next.Tasks[0]
		who := agent
		if done%4 == 3 {
			who = domain.Human
		}
		started := e2eOK(e.svc.StartTask(e2eCtx, who, task.ID, task.Version))
		if started.Status != domain.TaskInProgress || started.Assignee != string(who) {
			t.Fatalf("start %s: %+v", refOf[task.ID], started)
		}
		if done%3 == 2 {
			_, arts, err := e.svc.ReportTaskResult(e2eCtx, who, task.ID, TaskResult{
				Summary:   "implemented " + refOf[task.ID],
				Artifacts: []ArtifactInput{{Kind: domain.ArtifactCommit, Ref: "sha" + refOf[task.ID]}, {Kind: domain.ArtifactTestReport, Ref: "ci-1"}},
			})
			e2eNoErr(t, err)
			if len(arts) != 2 {
				t.Fatalf("artifacts = %d", len(arts))
			}
		} else {
			e2eOK(e.svc.AddTaskNote(e2eCtx, who, task.ID, "working on "+refOf[task.ID]))
			e2eOK(e.svc.RecordArtifact(e2eCtx, who, task.ID, ArtifactInput{Kind: domain.ArtifactCommit, Ref: "sha" + refOf[task.ID]}))
			fin := e2eOK(e.svc.CompleteTask(e2eCtx, who, task.ID, CompleteOptions{}))
			if fin.Status != domain.TaskDone || fin.CompletedAt == nil {
				t.Fatalf("complete %s: %+v", refOf[task.ID], fin)
			}
		}
		order = append(order, refOf[task.ID])
		done++
	}
	if done != 10 {
		t.Fatalf("completed %d tasks (order %v), want 10", done, order)
	}

	// Progress is exactly 100% and every feature is derived done.
	rep := e2eOK(e.svc.ProjectProgress(e2eCtx, ref))
	if rep.Progress.Fraction != 1 {
		t.Fatalf("project fraction = %v, want exactly 1", rep.Progress.Fraction)
	}
	if rep.Counts.Done != 10 || rep.Counts.Total != 10 || rep.ReadyCount != 0 || len(rep.Blocked) != 0 || len(rep.Stale) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.Features) != 3 {
		t.Fatalf("features = %d", len(rep.Features))
	}
	for _, f := range rep.Features {
		if f.Status != domain.FeatureDone || f.Progress.Fraction != 1 || !f.Counted {
			t.Fatalf("feature %q: %+v", f.Feature.Title, f)
		}
	}

	// The event log replays the full history in strictly increasing order.
	page := e2eOK(e.svc.EventsSince(e2eCtx, p.ID, 0, 1000))
	if page.More {
		t.Fatal("expected the whole history in one page")
	}
	counts := map[domain.EventType]int{}
	actors := map[domain.Actor]int{}
	var last int64
	startedAt := map[int64]int64{} // task -> event id of its start
	doneAt := map[int64]int64{}    // task -> event id of its completion
	for i, ev := range page.Events {
		if ev.ID <= last {
			t.Fatalf("event %d id %d not after %d", i, ev.ID, last)
		}
		last = ev.ID
		counts[ev.Type]++
		actors[ev.Actor]++
		if ev.Type == domain.EventTaskStatus {
			var pl struct{ From, To domain.TaskStatus }
			e2eNoErr(t, json.Unmarshal(ev.Payload, &pl))
			switch pl.To {
			case domain.TaskInProgress:
				startedAt[ev.EntityID] = ev.ID
			case domain.TaskDone:
				doneAt[ev.EntityID] = ev.ID
			}
		}
	}
	if first := page.Events[0]; first.Type != domain.EventProjectCreated || first.Actor != domain.Human {
		t.Fatalf("first event = %+v", first)
	}
	want := map[domain.EventType]int{
		domain.EventProjectCreated: 1, domain.EventFeatureCreated: 3, domain.EventTaskCreated: 10,
		domain.EventTaskDepAdded: e2eEdges, domain.EventTaskStatus: 20,
	}
	for typ, n := range want {
		if counts[typ] != n {
			t.Errorf("%s events = %d, want %d (all: %v)", typ, counts[typ], n, counts)
		}
	}
	if counts[domain.EventTaskNote] == 0 || counts[domain.EventArtifactAdded] < 10 {
		t.Errorf("notes/artifacts missing: %v", counts)
	}
	if actors[agent] == 0 || actors[domain.Human] == 0 {
		t.Errorf("both actors should appear: %v", actors)
	}
	if page.Cursor != last {
		t.Errorf("cursor = %d, want %d", page.Cursor, last)
	}
	// Dependencies were respected: a task started only after each of its
	// prerequisites completed.
	edges := map[string][]string{"b": {"a"}, "c": {"a"}, "d": {"b", "c"}, "e": {"d"}, "f": {"a"}, "g": {"e", "f"}, "h": {"g"}, "i": {"d"}}
	for task, deps := range edges {
		for _, d := range deps {
			if doneAt[idOf[d]] == 0 || startedAt[idOf[task]] < doneAt[idOf[d]] {
				t.Errorf("task %s started (event %d) before dependency %s finished (event %d)", task, startedAt[idOf[task]], d, doneAt[idOf[d]])
			}
		}
	}
	// Reading from the middle returns exactly the rest.
	mid := page.Events[len(page.Events)/2].ID
	tail := e2eOK(e.svc.EventsSince(e2eCtx, p.ID, mid, 1000))
	if got := len(tail.Events); got != len(page.Events)-len(page.Events)/2-1 || tail.Events[0].ID <= mid {
		t.Errorf("tail from %d has %d events", mid, got)
	}

	// Re-running the plan creates nothing.
	again := e2eOK(e.svc.CreatePlan(e2eCtx, agent, ref, e2ePlan()))
	if again.TasksCreated != 0 || again.TasksReused != 10 || again.FeaturesCreated != 0 || again.FeaturesReused != 3 || again.DependenciesAdded != 0 {
		t.Fatalf("re-run created something: %+v", again)
	}
	if latest := e2eOK(e.svc.LatestEventID(e2eCtx, p.ID)); latest != last {
		t.Fatalf("re-run wrote events: latest %d, was %d", latest, last)
	}
}

func TestE2EEvidenceGate(t *testing.T) {
	e := e2eNew(t)
	agent := domain.NewAgentActor("claude-code", "s2")
	p := e.project()
	ref := ProjectRef{}
	plan := e2eOK(e.svc.CreatePlan(e2eCtx, agent, ref, PlanInput{Features: []PlanFeature{
		{Title: "F", Weight: 1, Tasks: []PlanTask{e2eTask("x", 1), e2eTask("y", 1)}},
	}}))
	x, y := plan.Tasks["x"].TaskID, plan.Tasks["y"].TaskID
	for _, id := range []int64{x, y} {
		e2eOK(e.svc.StartTask(e2eCtx, agent, id, 0))
	}

	// The agent cannot complete without evidence, not even by forcing.
	for _, force := range []bool{false, true} {
		_, err := e.svc.CompleteTask(e2eCtx, agent, x, CompleteOptions{Force: force})
		var ee *domain.EvidenceRequiredError
		if !errors.As(err, &ee) {
			t.Fatalf("agent complete (force=%v) err = %v, want EvidenceRequiredError", force, err)
		}
	}
	rep := e2eOK(e.svc.ProjectProgress(e2eCtx, ref))
	if rep.Counts.Done != 0 || rep.Progress.Fraction != 0 {
		t.Fatalf("nothing should be done: %+v", rep)
	}

	// A human can force it, and the forced completion is recorded.
	done := e2eOK(e.svc.CompleteTask(e2eCtx, domain.Human, x, CompleteOptions{Force: true}))
	if done.Status != domain.TaskDone {
		t.Fatalf("forced completion: %+v", done)
	}
	// With evidence nobody needs to force, and the event says so.
	e2eOK(e.svc.RecordArtifact(e2eCtx, agent, y, ArtifactInput{Kind: domain.ArtifactManual, Ref: "checked by hand"}))
	e2eOK(e.svc.CompleteTask(e2eCtx, agent, y, CompleteOptions{}))

	forced := map[int64]bool{}
	for _, ev := range e2eOK(e.svc.EventsSince(e2eCtx, p.ID, 0, 1000)).Events {
		if ev.Type != domain.EventTaskStatus {
			continue
		}
		var pl struct {
			To     domain.TaskStatus
			Forced bool
		}
		e2eNoErr(t, json.Unmarshal(ev.Payload, &pl))
		if pl.To == domain.TaskDone {
			forced[ev.EntityID] = pl.Forced
		}
	}
	if !forced[x] || forced[y] {
		t.Fatalf("forced flags = %v, want x forced and y not", forced)
	}
	if rep := e2eOK(e.svc.ProjectProgress(e2eCtx, ref)); rep.Progress.Fraction != 1 {
		t.Fatalf("both done, fraction = %v", rep.Progress.Fraction)
	}
}

// TestE2ECancelledAndBlocked checks progress against hand-computed numbers.
//
//	Feature A (weight 2): a1 w1 done, a2 w3 cancelled, a3 w1 blocked
//	  -> counted tasks a1+a3 = weight 2, done weight 1  => 1/2
//	Feature B (weight 1): b1 w2 done, b2 w1 todo
//	  -> done weight 2 of 3                              => 2/3
//	Project = (2*(1/2) + 1*(2/3)) / (2+1) = (5/3)/3 = 5/9
func TestE2ECancelledAndBlocked(t *testing.T) {
	e := e2eNew(t)
	agent := domain.NewAgentActor("claude-code", "s3")
	e.project()
	ref := ProjectRef{}
	plan := e2eOK(e.svc.CreatePlan(e2eCtx, agent, ref, PlanInput{Features: []PlanFeature{
		{Title: "A", Weight: 2, Tasks: []PlanTask{e2eTask("a1", 1), e2eTask("a2", 3), e2eTask("a3", 1)}},
		{Title: "B", Weight: 1, Tasks: []PlanTask{e2eTask("b1", 2), e2eTask("b2", 1)}},
	}}))
	id := func(r string) int64 { return plan.Tasks[r].TaskID }

	for _, r := range []string{"a1", "b1"} {
		e2eOK(e.svc.StartTask(e2eCtx, agent, id(r), 0))
		_, _, err := e.svc.ReportTaskResult(e2eCtx, agent, id(r), TaskResult{Summary: "done", Artifacts: []ArtifactInput{{Kind: domain.ArtifactCommit, Ref: "sha" + r}}})
		e2eNoErr(t, err)
	}
	e2eOK(e.svc.UpdateTaskStatus(e2eCtx, agent, id("a2"), StatusChange{To: domain.TaskCancelled}))
	e2eOK(e.svc.StartTask(e2eCtx, agent, id("a3"), 0))
	e2eOK(e.svc.UpdateTaskStatus(e2eCtx, agent, id("a3"), StatusChange{To: domain.TaskBlocked, BlockedReason: "waiting on API key"}))

	rep := e2eOK(e.svc.ProjectProgress(e2eCtx, ref))
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
	if !near(rep.Progress.Fraction, 5.0/9.0) {
		t.Fatalf("project fraction = %v, want %v", rep.Progress.Fraction, 5.0/9.0)
	}
	byTitle := map[string]FeatureReport{}
	for _, f := range rep.Features {
		byTitle[f.Feature.Title] = f
	}
	a, b := byTitle["A"], byTitle["B"]
	if a.Progress.Done != 1 || a.Progress.Total != 2 || !near(a.Progress.Fraction, 0.5) || a.Status != domain.FeatureActive {
		t.Errorf("feature A = %+v", a)
	}
	if b.Progress.Done != 2 || b.Progress.Total != 3 || !near(b.Progress.Fraction, 2.0/3.0) || b.Status != domain.FeatureActive {
		t.Errorf("feature B = %+v", b)
	}
	if rep.Counts.Done != 2 || rep.Counts.Cancelled != 1 || rep.Counts.Blocked != 1 || rep.Counts.Todo != 1 || rep.Counts.Total != 5 {
		t.Errorf("counts = %+v", rep.Counts)
	}
	if len(rep.Blocked) != 1 || rep.Blocked[0].Task.ID != id("a3") || rep.Blocked[0].Reason != "waiting on API key" {
		t.Errorf("blocked = %+v", rep.Blocked)
	}
	if rep.ReadyCount != 1 {
		t.Errorf("ready = %d, want only b2", rep.ReadyCount)
	}
	next := e2eOK(e.svc.NextTasks(e2eCtx, ref, 10))
	if len(next.Tasks) != 1 || next.Tasks[0].ID != id("b2") {
		t.Errorf("next = %+v", next.Tasks)
	}

	// Unblocking and finishing the rest brings the project to 100%; the
	// cancelled task never counts.
	e2eOK(e.svc.UpdateTaskStatus(e2eCtx, agent, id("a3"), StatusChange{To: domain.TaskInProgress}))
	e2eOK(e.svc.StartTask(e2eCtx, agent, id("b2"), 0))
	for _, r := range []string{"a3", "b2"} {
		_, _, err := e.svc.ReportTaskResult(e2eCtx, agent, id(r), TaskResult{Summary: "done", Artifacts: []ArtifactInput{{Kind: domain.ArtifactCommit, Ref: "sha" + r}}})
		e2eNoErr(t, err)
	}
	if rep := e2eOK(e.svc.ProjectProgress(e2eCtx, ref)); rep.Progress.Fraction != 1 {
		t.Fatalf("final fraction = %v, want 1", rep.Progress.Fraction)
	}
}
