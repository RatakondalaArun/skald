package service

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"skald/internal/domain"
	"skald/internal/store"
)

func expFresh(t *testing.T) *Service {
	t.Helper()
	db, err := store.Open(e2eCtx, filepath.Join(t.TempDir(), "db", "skald.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var mu sync.Mutex // the clock is called from concurrent goroutines in some tests
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	return New(db, WithClock(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(time.Second)
		return now
	}))
}

// expPopulate builds a database that touches every table: two projects, a
// plan with dependencies, done/blocked/cancelled/in-progress tasks, evidence,
// notes and a two-version document. It returns the first project.
func expPopulate(t *testing.T, e *e2eEnv) domain.Project {
	t.Helper()
	agent := domain.NewAgentActor("claude-code", "s1")
	p := e.project()
	ref := ProjectRef{Slug: p.Slug}
	res := e2eOK(e.svc.CreatePlan(e2eCtx, agent, ref, e2ePlan()))
	id := func(r string) int64 { return res.Tasks[r].TaskID }

	e2eOK(e.svc.StartTask(e2eCtx, agent, id("a"), 0))
	e2eOK(e.svc.AddTaskNote(e2eCtx, agent, id("a"), "starting \"quoted\" work\nwith a newline"))
	e2eOK(e.svc.RecordArtifact(e2eCtx, agent, id("a"), ArtifactInput{Kind: domain.ArtifactCommit, Ref: "abc123", Summary: "first"}))
	e2eOK(e.svc.CompleteTask(e2eCtx, agent, id("a"), CompleteOptions{}))
	e2eOK(e.svc.StartTask(e2eCtx, agent, id("b"), 0))
	e2eOK(e.svc.UpdateTaskStatus(e2eCtx, agent, id("b"), StatusChange{To: domain.TaskBlocked, BlockedReason: "waiting on API key"}))
	e2eOK(e.svc.StartTask(e2eCtx, domain.Human, id("c"), 0))
	e2eOK(e.svc.TagTask(e2eCtx, agent, ref, id("c"), []string{"backend", "UI"}, nil))
	e2eOK(e.svc.UpdateTaskStatus(e2eCtx, domain.Human, id("j"), StatusChange{To: domain.TaskCancelled}))
	for _, d := range []struct {
		who  domain.Actor
		body string
	}{{domain.Human, "v1 body"}, {agent, "v2 body ✓"}} {
		if _, _, err := e.svc.PutDocument(e2eCtx, d.who, ref, "architecture", "Overview", d.body); err != nil {
			t.Fatal(err)
		}
	}

	// A second project with no repo link, to prove --project scoping.
	other := e2eOK(e.svc.CreateProject(e2eCtx, domain.Human, CreateProjectInput{Slug: "other", Name: "Other"}))
	f := e2eOK(e.svc.CreateFeature(e2eCtx, domain.Human, ProjectRef{Slug: "other"}, CreateFeatureInput{Title: "Solo"}))
	if _, _, err := e.svc.CreateTask(e2eCtx, domain.Human, ProjectRef{Slug: "other"}, CreateTaskInput{FeatureID: f.ID, Title: "Lonely"}); err != nil {
		t.Fatal(err)
	}
	_ = other
	return p
}

// expJSON returns the export as JSON with the one field that legitimately
// differs between two exports blanked out.
func expJSON(t *testing.T, x *Export) string {
	t.Helper()
	c := *x
	c.ExportedAt = ""
	var b bytes.Buffer
	e2eNoErr(t, c.Write(&b))
	return b.String()
}

func TestExpAllContent(t *testing.T) {
	e := e2eNew(t)
	expPopulate(t, e)
	x := e2eOK(e.svc.Export(e2eCtx, ExportOptions{SkaldVersion: "9.9"}))

	if x.Format != ExportFormat || x.FormatVersion != ExportFormatVersion || x.SkaldVersion != "9.9" || x.Scope != "all" {
		t.Fatalf("header = %+v", x)
	}
	if x.SchemaVersion < 1 {
		t.Fatalf("schema_version = %d", x.SchemaVersion)
	}
	if _, err := time.Parse(time.RFC3339, x.ExportedAt); err != nil {
		t.Fatalf("exported_at %q: %v", x.ExportedAt, err)
	}
	if len(x.Projects) != 2 || len(x.Features) != 4 || len(x.Tasks) != 11 || len(x.Dependencies) != e2eEdges || len(x.Artifacts) != 1 || len(x.Documents) != 1 {
		t.Fatalf("counts p=%d f=%d t=%d d=%d a=%d doc=%d", len(x.Projects), len(x.Features), len(x.Tasks), len(x.Dependencies), len(x.Artifacts), len(x.Documents))
	}
	if got := len(x.Documents[0].Versions); got != 2 || x.Documents[0].CurrentVersion != 2 || x.Documents[0].Versions[1].Body != "v2 body ✓" {
		t.Fatalf("document versions = %+v", x.Documents[0])
	}
	latest := e2eOK(e.svc.LatestEventID(e2eCtx, 0))
	if int64(len(x.Events)) != latest || x.Events[0].ID != 1 || x.Events[len(x.Events)-1].ID != latest {
		t.Fatalf("events = %d, latest id %d", len(x.Events), latest)
	}
	for i := 1; i < len(x.Events); i++ {
		if x.Events[i].ID <= x.Events[i-1].ID {
			t.Fatalf("events not in increasing order at %d", i)
		}
	}
	// Short ids sit next to raw ids; nothing is null that a consumer would range over.
	for _, tk := range x.Tasks {
		if tk.ShortID != domain.FormatID(domain.EntityTask, tk.ID) {
			t.Fatalf("task %d short id %q", tk.ID, tk.ShortID)
		}
	}
	// Spot-check fields that carry state: version, timestamps, blocked reason, actor.
	var blocked, done *ExportTask
	for i := range x.Tasks {
		switch x.Tasks[i].Status {
		case domain.TaskBlocked:
			blocked = &x.Tasks[i]
		case domain.TaskDone:
			done = &x.Tasks[i]
		}
	}
	if blocked == nil || blocked.BlockedReason != "waiting on API key" || blocked.StartedAt == nil || blocked.Version < 3 {
		t.Fatalf("blocked task = %+v", blocked)
	}
	if done == nil || done.CompletedAt == nil || done.IdempotencyKey == "" || done.Assignee == "" {
		t.Fatalf("done task = %+v", done)
	}
	var agentEvents int
	for _, ev := range x.Events {
		if ev.Actor.IsAgent() {
			agentEvents++
		}
		if ev.Entity != domain.FormatID(ev.EntityType, ev.EntityID) {
			t.Fatalf("event %d entity %q", ev.ID, ev.Entity)
		}
	}
	if agentEvents == 0 {
		t.Fatal("no agent actor in exported events")
	}
}

func TestExpSingleProjectScope(t *testing.T) {
	e := e2eNew(t)
	p := expPopulate(t, e)
	x := e2eOK(e.svc.Export(e2eCtx, ExportOptions{Project: p.ID}))
	if x.Scope != "project:school" || len(x.Projects) != 1 || x.Projects[0].ID != p.ID {
		t.Fatalf("scope %q projects %+v", x.Scope, x.Projects)
	}
	if len(x.Features) != 3 || len(x.Tasks) != 10 || len(x.Dependencies) != e2eEdges || len(x.Artifacts) != 1 || len(x.Documents) != 1 {
		t.Fatalf("counts f=%d t=%d d=%d a=%d doc=%d", len(x.Features), len(x.Tasks), len(x.Dependencies), len(x.Artifacts), len(x.Documents))
	}
	for _, ev := range x.Events {
		if ev.ProjectID != p.ID {
			t.Fatalf("event %d belongs to project %d", ev.ID, ev.ProjectID)
		}
	}
	if len(x.Events) == 0 {
		t.Fatal("no events")
	}
	_, err := e.svc.Export(e2eCtx, ExportOptions{Project: 999})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
}

func TestExpRoundTripPreservesEverything(t *testing.T) {
	for _, scope := range []string{"all", "project"} {
		t.Run(scope, func(t *testing.T) {
			e := e2eNew(t)
			p := expPopulate(t, e)
			opts := ExportOptions{SkaldVersion: "9.9"}
			if scope == "project" {
				opts.Project = p.ID
			}
			first := e2eOK(e.svc.Export(e2eCtx, opts))

			// Through the wire format, as the CLI does it.
			var wire bytes.Buffer
			e2eNoErr(t, first.Write(&wire))
			parsed, err := ParseExport(&wire)
			e2eNoErr(t, err)

			fresh := expFresh(t)
			e2eNoErr(t, fresh.ImportExport(e2eCtx, parsed))
			second := e2eOK(fresh.Export(e2eCtx, opts))

			if a, b := expJSON(t, first), expJSON(t, second); a != b {
				t.Fatalf("second export differs from the first\n--- first\n%s\n--- second\n%s", a, b)
			}

			// The imported database is live: derived state matches and the event
			// cursor continues after the last imported id instead of reusing ids.
			want := e2eOK(e.svc.ProjectProgress(e2eCtx, ProjectRef{Slug: "school"}))
			got := e2eOK(fresh.ProjectProgress(e2eCtx, ProjectRef{Slug: "school"}))
			if want.Progress != got.Progress || want.Counts != got.Counts {
				t.Fatalf("progress %+v/%+v, want %+v/%+v", got.Progress, got.Counts, want.Progress, want.Counts)
			}
			last := first.Events[len(first.Events)-1].ID
			id := e2eOK(fresh.AddTaskNote(e2eCtx, domain.Human, first.Tasks[0].ID, "after import"))
			if id != last+1 {
				t.Fatalf("next event id = %d, want %d", id, last+1)
			}
			if _, err := fresh.StartTask(e2eCtx, domain.Human, first.Tasks[len(first.Tasks)-2].ID, 0); err != nil && !errors.Is(err, domain.ErrInvalidTransition) {
				t.Fatalf("start after import: %v", err)
			}
		})
	}
}

func TestExpImportRefusesNonEmptyDatabase(t *testing.T) {
	e := e2eNew(t)
	expPopulate(t, e)
	x := e2eOK(e.svc.Export(e2eCtx, ExportOptions{}))

	fresh := expFresh(t)
	e2eOK(fresh.CreateProject(e2eCtx, domain.Human, CreateProjectInput{Slug: "existing", Name: "Existing"}))
	before := expJSON(t, e2eOK(fresh.Export(e2eCtx, ExportOptions{})))

	err := fresh.ImportExport(e2eCtx, x)
	var ve *domain.ValidationError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "fresh") {
		t.Fatalf("err = %v, want a ValidationError that says to use a fresh database", err)
	}
	if after := expJSON(t, e2eOK(fresh.Export(e2eCtx, ExportOptions{}))); after != before {
		t.Fatal("refused import changed the database")
	}
	// Importing twice into the same database is refused for the same reason.
	again := expFresh(t)
	e2eNoErr(t, again.ImportExport(e2eCtx, x))
	if err := again.ImportExport(e2eCtx, x); !errors.As(err, &ve) {
		t.Fatalf("second import: %v", err)
	}
}

func TestExpImportRollsBackOnBadData(t *testing.T) {
	e := e2eNew(t)
	expPopulate(t, e)
	x := e2eOK(e.svc.Export(e2eCtx, ExportOptions{}))
	x.Tasks[len(x.Tasks)-1].FeatureID = 9999 // dangling reference

	fresh := expFresh(t)
	if err := fresh.ImportExport(e2eCtx, x); err == nil {
		t.Fatal("import of a dangling feature reference succeeded")
	}
	empty := e2eOK(fresh.Export(e2eCtx, ExportOptions{}))
	if len(empty.Projects)+len(empty.Tasks)+len(empty.Events) != 0 {
		t.Fatalf("partial import left rows behind: %+v", empty)
	}
}

func TestExpParseExportErrors(t *testing.T) {
	for name, in := range map[string]string{
		"not json":       `nope`,
		"wrong format":   `{"format":"other","format_version":1}`,
		"future version": `{"format":"skald-export","format_version":99}`,
		"zero version":   `{"format":"skald-export","format_version":0}`,
	} {
		_, err := ParseExport(strings.NewReader(in))
		var ve *domain.ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: err = %v, want ValidationError", name, err)
		}
	}
	// Fields added by a newer skald are ignored.
	if _, err := ParseExport(strings.NewReader(`{"format":"skald-export","format_version":1,"new_field":true}`)); err != nil {
		t.Fatalf("unknown field rejected: %v", err)
	}
}

// The export reads every table in one transaction, so an event never refers
// to a task the snapshot does not contain, even while writers are active.
func TestExpSnapshotIsConsistentUnderWrites(t *testing.T) {
	e2eT = t
	svc := expFresh(t)
	p := e2eOK(svc.CreateProject(e2eCtx, domain.Human, CreateProjectInput{Slug: "busy", Name: "Busy"}))
	ref := ProjectRef{Slug: p.Slug}
	f := e2eOK(svc.CreateFeature(e2eCtx, domain.Human, ref, CreateFeatureInput{Title: "Busy"}))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, _, err := svc.CreateTask(e2eCtx, domain.Human, ref, CreateTaskInput{FeatureID: f.ID, Title: "t"}); err != nil {
				t.Errorf("writer: %v", err)
				return
			}
		}
	}()
	for i := 0; i < 25; i++ {
		x, err := svc.Export(e2eCtx, ExportOptions{})
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatal(err)
		}
		tasks := map[int64]bool{}
		for _, tk := range x.Tasks {
			tasks[tk.ID] = true
		}
		var created int
		for _, ev := range x.Events {
			if ev.Type == domain.EventTaskCreated {
				created++
				if !tasks[ev.EntityID] {
					close(stop)
					wg.Wait()
					t.Fatalf("export %d: event %d creates task %d, which is not in the snapshot", i, ev.ID, ev.EntityID)
				}
			}
		}
		if created != len(x.Tasks) {
			close(stop)
			wg.Wait()
			t.Fatalf("export %d: %d task.created events but %d tasks", i, created, len(x.Tasks))
		}
	}
	close(stop)
	wg.Wait()
}
