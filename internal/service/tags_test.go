package service

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"skald/internal/domain"
)

func tagsEnv(t *testing.T) (*e2eEnv, ProjectRef, domain.Feature) {
	t.Helper()
	e := e2eNew(t)
	p := e.project()
	ref := ProjectRef{Slug: p.Slug}
	f := e2eOK(e.svc.CreateFeature(e2eCtx, domain.Human, ref, CreateFeatureInput{Title: "F"}))
	return e, ref, f
}

func (e *e2eEnv) taggedTask(ref ProjectRef, f domain.Feature, title string, tags ...string) domain.Task {
	e.t.Helper()
	task, _ := e2eOK2(e.svc.CreateTask(e2eCtx, domain.Human, ref, CreateTaskInput{FeatureID: f.ID, Title: title, Tags: tags}))
	return task
}

func e2eOK2[A, B any](a A, b B, err error) (A, B) {
	e2eT.Helper()
	if err != nil {
		e2eT.Fatal(err)
	}
	return a, b
}

func TestCreateTaskNormalizesTags(t *testing.T) {
	e, ref, f := tagsEnv(t)
	task := e.taggedTask(ref, f, "A", "UI", "backend", "ui")
	if !slices.Equal(task.Tags, []string{"backend", "ui"}) {
		t.Fatalf("tags = %v", task.Tags)
	}
	plain := e.taggedTask(ref, f, "B")
	if plain.Tags == nil || len(plain.Tags) != 0 {
		t.Fatalf("untagged task tags = %#v, want empty", plain.Tags)
	}
	if _, _, err := e.svc.CreateTask(e2eCtx, domain.Human, ref, CreateTaskInput{FeatureID: f.ID, Title: "C", Tags: []string{"bad tag"}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad tag: err = %v", err)
	}
}

func TestUpdateTaskTags(t *testing.T) {
	e, ref, f := tagsEnv(t)
	task := e.taggedTask(ref, f, "A", "ui", "bug")

	got := e2eOK(e.svc.UpdateTask(e2eCtx, domain.Human, task.ID, TaskEdit{AddTags: []string{"Backend"}, RemoveTags: []string{"bug", "never-there"}}))
	if !slices.Equal(got.Tags, []string{"backend", "ui"}) || got.Version != task.Version+1 {
		t.Fatalf("after add/remove: %v v%d", got.Tags, got.Version)
	}
	// Replacing, then adding on top of the replacement.
	repl := []string{"docs"}
	got = e2eOK(e.svc.UpdateTask(e2eCtx, domain.Human, task.ID, TaskEdit{Tags: &repl, AddTags: []string{"api"}}))
	if !slices.Equal(got.Tags, []string{"api", "docs"}) {
		t.Fatalf("after replace: %v", got.Tags)
	}
	// The event records the new set.
	d := e2eOK(e.svc.GetTask(e2eCtx, domain.Human, ref, task.ID))
	var payload struct {
		Fields []string `json:"fields"`
		Tags   []string `json:"tags"`
	}
	if err := json.Unmarshal(d.RecentEvents[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if d.RecentEvents[0].Type != domain.EventTaskUpdated || !slices.Equal(payload.Fields, []string{"tags"}) || !slices.Equal(payload.Tags, []string{"api", "docs"}) {
		t.Fatalf("event = %s %s", d.RecentEvents[0].Type, d.RecentEvents[0].Payload)
	}
	// Same set in another order changes nothing.
	same := []string{"DOCS", "api"}
	again := e2eOK(e.svc.UpdateTask(e2eCtx, domain.Human, task.ID, TaskEdit{Tags: &same}))
	if again.Version != got.Version {
		t.Fatalf("no-op tag edit bumped the version to %d", again.Version)
	}
	// Clearing.
	none := []string{}
	if got = e2eOK(e.svc.UpdateTask(e2eCtx, domain.Human, task.ID, TaskEdit{Tags: &none})); len(got.Tags) != 0 {
		t.Fatalf("after clear: %v", got.Tags)
	}
	if _, err := e.svc.UpdateTask(e2eCtx, domain.Human, task.ID, TaskEdit{AddTags: []string{"-x"}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad tag: err = %v", err)
	}
}

func TestTagTaskIsScopedToProject(t *testing.T) {
	e, ref, f := tagsEnv(t)
	task := e.taggedTask(ref, f, "A")
	e2eOK(e.svc.CreateProject(e2eCtx, domain.Human, CreateProjectInput{Slug: "other", Name: "Other"}))
	if _, err := e.svc.TagTask(e2eCtx, domain.Human, ProjectRef{Slug: "other"}, task.ID, []string{"x"}, nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-project tag: err = %v", err)
	}
	got := e2eOK(e.svc.TagTask(e2eCtx, domain.Human, ref, task.ID, []string{"x"}, nil))
	if !slices.Equal(got.Tags, []string{"x"}) {
		t.Fatalf("tags = %v", got.Tags)
	}
}

func TestListTasksAndTagsFilter(t *testing.T) {
	e, ref, f := tagsEnv(t)
	a := e.taggedTask(ref, f, "A", "ui", "bug")
	b := e.taggedTask(ref, f, "B", "ui")
	e.taggedTask(ref, f, "C", "backend")
	e.taggedTask(ref, f, "D")

	ids := func(tags ...string) []int64 {
		l := e2eOK(e.svc.ListTasks(e2eCtx, domain.Human, ref, ListTasksFilter{Tags: tags}))
		var out []int64
		for _, task := range l.Tasks {
			out = append(out, task.ID)
		}
		return out
	}
	if got := ids("UI"); !slices.Equal(got, []int64{a.ID, b.ID}) {
		t.Fatalf("ui: %v", got)
	}
	if got := ids("ui", "bug"); !slices.Equal(got, []int64{a.ID}) {
		t.Fatalf("ui+bug: %v", got)
	}
	if got := ids("nope"); len(got) != 0 {
		t.Fatalf("nope: %v", got)
	}
	if _, err := e.svc.ListTasks(e2eCtx, domain.Human, ref, ListTasksFilter{Tags: []string{"no spaces"}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad filter tag: err = %v", err)
	}

	tags := e2eOK(e.svc.ListTags(e2eCtx, domain.Human, ref))
	want := []TagCount{{"ui", 2}, {"backend", 1}, {"bug", 1}}
	if !slices.Equal(tags, want) {
		t.Fatalf("tags = %v, want %v", tags, want)
	}
}

func TestPlanTaskTags(t *testing.T) {
	e := e2eNew(t)
	p := e.project()
	ref := ProjectRef{Slug: p.Slug}
	plan := PlanInput{Features: []PlanFeature{{Title: "F", Tasks: []PlanTask{{Ref: "a", Title: "A", Tags: []string{"Infra", "infra"}}}}}}
	res := e2eOK(e.svc.CreatePlan(e2eCtx, domain.Human, ref, plan))
	d := e2eOK(e.svc.GetTask(e2eCtx, domain.Human, ref, res.Tasks["a"].TaskID))
	if !slices.Equal(d.Task.Tags, []string{"infra"}) {
		t.Fatalf("tags = %v", d.Task.Tags)
	}
	plan.Features[0].Tasks[0].Tags = []string{"bad tag"}
	plan.Features[0].Title = "G"
	plan.Features[0].Tasks[0].Ref = "b"
	_, err := e.svc.CreatePlan(e2eCtx, domain.Human, ref, plan)
	var ve *domain.ValidationError
	if !errors.As(err, &ve) || ve.Field != "features[0].tasks[0].tags" {
		t.Fatalf("err = %v", err)
	}
}
