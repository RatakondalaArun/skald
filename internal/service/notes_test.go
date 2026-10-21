package service

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"skald/internal/domain"
	"skald/internal/store"
)

func TestNotesAddAndList(t *testing.T) {
	e := projANew(t)
	p, ref := e.project(t, "demo")
	f := e.feature(t, ref, "Core")
	other := e.feature(t, ref, "Extras")
	task := e.task(t, ref, f.ID, "Login")
	agent := domain.NewAgentActor("claude-code", "s1")

	plain := projAMust[domain.Note](t)(e.svc.AddNote(projACtx, agent, ref, AddNoteInput{Body: "  staging tokens expire after 15 minutes  "}))
	if plain.Body != "staging tokens expire after 15 minutes" || plain.Actor != agent || plain.TaskID != 0 || plain.FeatureID != 0 || plain.ProjectID != p.ID {
		t.Fatalf("plain note = %+v", plain)
	}
	if !plain.CreatedAt.Equal(projAT0) {
		t.Errorf("created_at = %v, want %v", plain.CreatedAt, projAT0)
	}
	e.now = e.now.Add(time.Minute)
	onTask := projAMust[domain.Note](t)(e.svc.AddNote(projACtx, agent, ref, AddNoteInput{Body: "bcrypt cost 14 is too slow on CI", TaskID: task.ID}))
	if onTask.TaskID != task.ID || onTask.FeatureID != f.ID {
		t.Fatalf("a task link should imply its feature: %+v", onTask)
	}
	onFeature := projAMust[domain.Note](t)(e.svc.AddNote(projACtx, domain.Human, ref, AddNoteInput{Body: "maybe drop this 100% feature", FeatureID: other.ID}))

	all := projAMust[NoteList](t)(e.svc.ListNotes(projACtx, ref, NoteFilter{}))
	if len(all.Notes) != 3 || all.Truncated || all.Notes[0].ID != onFeature.ID || all.Notes[2].ID != plain.ID {
		t.Fatalf("list should be newest first: %+v", all)
	}
	ids := func(l NoteList) []int64 {
		var out []int64
		for _, n := range l.Notes {
			out = append(out, n.ID)
		}
		return out
	}
	for name, c := range map[string]struct {
		f    NoteFilter
		want []int64
	}{
		"task":           {NoteFilter{TaskID: task.ID}, []int64{onTask.ID}},
		"feature":        {NoteFilter{FeatureID: f.ID}, []int64{onTask.ID}},
		"author":         {NoteFilter{Actor: domain.Human}, []int64{onFeature.ID}},
		"query":          {NoteFilter{Query: "BCRYPT"}, []int64{onTask.ID}},
		"query wildcard": {NoteFilter{Query: "100%"}, []int64{onFeature.ID}},
		"query no match": {NoteFilter{Query: "%_"}, nil},
	} {
		got := ids(projAMust[NoteList](t)(e.svc.ListNotes(projACtx, ref, c.f)))
		if len(got) != len(c.want) || (len(got) > 0 && got[0] != c.want[0]) {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}

	page := projAMust[NoteList](t)(e.svc.ListNotes(projACtx, ref, NoteFilter{Limit: 2}))
	if len(page.Notes) != 2 || !page.Truncated {
		t.Errorf("limit 2 = %d notes, truncated %v", len(page.Notes), page.Truncated)
	}
	page = projAMust[NoteList](t)(e.svc.ListNotes(projACtx, ref, NoteFilter{Limit: 2, Offset: 2}))
	if len(page.Notes) != 1 || page.Truncated || page.Notes[0].ID != plain.ID {
		t.Errorf("offset 2 = %+v", page)
	}

	got := projAMust[domain.Note](t)(e.svc.GetNote(projACtx, ref, onTask.ID))
	if got.Body != onTask.Body {
		t.Errorf("GetNote = %+v", got)
	}
	sum := projAMust[ProjectSummary](t)(e.svc.ProjectSummary(projACtx, ref))
	if sum.NoteCount != 3 {
		t.Errorf("summary note count = %d, want 3", sum.NoteCount)
	}
}

func TestNotesValidation(t *testing.T) {
	e := projANew(t)
	_, ref := e.project(t, "demo")
	_, otherRef := e.project(t, "other")
	f := e.feature(t, ref, "Core")
	f2 := e.feature(t, ref, "Extras")
	task := e.task(t, ref, f.ID, "Login")
	foreignFeature := e.feature(t, otherRef, "Theirs")
	foreignTask := e.task(t, otherRef, foreignFeature.ID, "Their task")

	for name, c := range map[string]struct {
		in   AddNoteInput
		kind domain.ErrorKind
	}{
		"empty":            {AddNoteInput{Body: " \n "}, domain.KindValidation},
		"too long":         {AddNoteInput{Body: strings.Repeat("é", store.MaxNoteBody+1)}, domain.KindValidation},
		"missing task":     {AddNoteInput{Body: "x", TaskID: 999}, domain.KindNotFound},
		"foreign task":     {AddNoteInput{Body: "x", TaskID: foreignTask.ID}, domain.KindNotFound},
		"foreign feature":  {AddNoteInput{Body: "x", FeatureID: foreignFeature.ID}, domain.KindNotFound},
		"feature mismatch": {AddNoteInput{Body: "x", TaskID: task.ID, FeatureID: f2.ID}, domain.KindValidation},
	} {
		_, err := e.svc.AddNote(projACtx, domain.Human, ref, c.in)
		if err == nil || domain.KindOf(err) != c.kind {
			t.Errorf("%s: err = %v, want kind %v", name, err, c.kind)
		}
	}
	if _, err := e.svc.AddNote(projACtx, domain.Human, ref, AddNoteInput{Body: strings.Repeat("é", store.MaxNoteBody)}); err != nil {
		t.Errorf("a note at the limit should be accepted: %v", err)
	}
	// The matching feature alongside its task is fine.
	if _, err := e.svc.AddNote(projACtx, domain.Human, ref, AddNoteInput{Body: "ok", TaskID: task.ID, FeatureID: f.ID}); err != nil {
		t.Errorf("task with its own feature: %v", err)
	}

	theirs := projAMust[domain.Note](t)(e.svc.AddNote(projACtx, domain.Human, otherRef, AddNoteInput{Body: "theirs"}))
	if _, err := e.svc.GetNote(projACtx, ref, theirs.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a note of another project should be NotFound, got %v", err)
	}
	if _, err := e.svc.ListNotes(projACtx, ref, NoteFilter{TaskID: foreignTask.ID}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("filtering by another project's task should be NotFound, got %v", err)
	}
	if _, err := e.svc.ListNotes(projACtx, ref, NoteFilter{Limit: -1}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("negative limit err = %v", err)
	}

	p := projAMust[domain.Project](t)(e.svc.ResolveProject(projACtx, otherRef))
	projAMust[domain.Project](t)(e.svc.ArchiveProject(projACtx, domain.Human, p.ID))
	if _, err := e.svc.AddNote(projACtx, domain.Human, ProjectRef{Slug: "other"}, AddNoteInput{Body: "late"}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("adding to an archived project err = %v", err)
	}
}

// Notes are a side channel: they never move progress, task versions or task
// timestamps, and each one writes exactly one note.added event.
func TestNotesDoNotTouchTasksOrProgress(t *testing.T) {
	e := projANew(t)
	p, ref := e.project(t, "demo")
	f := e.feature(t, ref, "Core")
	task := e.task(t, ref, f.ID, "Login")
	e.task(t, ref, f.ID, "Logout")
	e.setStatus(t, task.ID, domain.TaskInProgress)
	before := projAMust[TaskDetail](t)(e.svc.GetTask(projACtx, domain.Human, ref, task.ID))
	progBefore := projAMust[ProgressReport](t)(e.svc.ProjectProgress(projACtx, ref))
	evBefore := len(e.events(t, p.ID))

	e.now = e.now.Add(time.Hour)
	n := projAMust[domain.Note](t)(e.svc.AddNote(projACtx, domain.NewAgentActor("cc", "s"), ref, AddNoteInput{Body: strings.Repeat("long note ", 100), TaskID: task.ID}))

	after := projAMust[TaskDetail](t)(e.svc.GetTask(projACtx, domain.Human, ref, task.ID))
	if after.Task.Version != before.Task.Version || !after.Task.UpdatedAt.Equal(before.Task.UpdatedAt) {
		t.Errorf("a note changed the task: %+v -> %+v", before.Task, after.Task)
	}
	progAfter := projAMust[ProgressReport](t)(e.svc.ProjectProgress(projACtx, ref))
	if progAfter.Progress != progBefore.Progress || progAfter.Counts != progBefore.Counts {
		t.Errorf("progress moved: %+v -> %+v", progBefore.Progress, progAfter.Progress)
	}
	evs := e.events(t, p.ID)
	if len(evs) != evBefore+1 {
		t.Fatalf("events %d -> %d, want one more", evBefore, len(evs))
	}
	ev := evs[len(evs)-1]
	if ev.Type != domain.EventNoteAdded || ev.EntityType != domain.EntityNote || ev.EntityID != n.ID {
		t.Fatalf("event = %+v", ev)
	}
	var payload struct {
		Text      string `json:"text"`
		TaskID    int64  `json:"task_id"`
		FeatureID int64  `json:"feature_id"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.TaskID != task.ID || payload.FeatureID != f.ID || len([]rune(payload.Text)) > 280 || !strings.HasSuffix(payload.Text, "…") {
		t.Errorf("payload = %+v", payload)
	}
}

func TestNotesAreAppendOnly(t *testing.T) {
	e := projANew(t)
	_, ref := e.project(t, "demo")
	n := projAMust[domain.Note](t)(e.svc.AddNote(projACtx, domain.Human, ref, AddNoteInput{Body: "keep me"}))
	for _, q := range []string{`UPDATE notes SET body = 'changed' WHERE id = ?`, `DELETE FROM notes WHERE id = ?`} {
		err := e.db.WriteTx(projACtx, func(tx *sql.Tx) error { _, err := tx.ExecContext(projACtx, q, n.ID); return err })
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v, want append-only refusal", q, err)
		}
	}
}

func TestNotesExportRoundTrip(t *testing.T) {
	e := projANew(t)
	_, ref := e.project(t, "demo")
	f := e.feature(t, ref, "Core")
	task := e.task(t, ref, f.ID, "Login")
	projAMust[domain.Note](t)(e.svc.AddNote(projACtx, domain.Human, ref, AddNoteInput{Body: "loose"}))
	projAMust[domain.Note](t)(e.svc.AddNote(projACtx, domain.Human, ref, AddNoteInput{Body: "linked", TaskID: task.ID}))

	x := projAMust[*Export](t)(e.svc.Export(projACtx, ExportOptions{}))
	if len(x.Notes) != 2 || x.Notes[1].TaskID != task.ID || x.Notes[1].ShortID != "N-2" {
		t.Fatalf("exported notes = %+v", x.Notes)
	}
	var buf strings.Builder
	if err := x.Write(&buf); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseExport(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	fresh := projANew(t)
	if err := fresh.svc.ImportExport(projACtx, parsed); err != nil {
		t.Fatal(err)
	}
	list := projAMust[NoteList](t)(fresh.svc.ListNotes(projACtx, ref, NoteFilter{}))
	if len(list.Notes) != 2 || list.Notes[0].Body != "linked" || list.Notes[0].TaskID != task.ID || list.Notes[0].FeatureID != f.ID {
		t.Fatalf("imported notes = %+v", list.Notes)
	}
	// The imported database keeps numbering after the imported notes.
	n := projAMust[domain.Note](t)(fresh.svc.AddNote(projACtx, domain.Human, ref, AddNoteInput{Body: "after import"}))
	if n.ID != 3 {
		t.Errorf("next note id = %d, want 3", n.ID)
	}
}
