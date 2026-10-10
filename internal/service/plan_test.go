package service

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"skald/internal/domain"
	"skald/internal/store"
)

var (
	planDCtx = context.Background()
	planDT0  = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
)

func planDSetup(t *testing.T) (*Service, *store.DB, domain.Project) {
	t.Helper()
	db, err := store.Open(planDCtx, filepath.Join(t.TempDir(), "db", "skald.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var p domain.Project
	err = db.WriteTx(planDCtx, func(tx *sql.Tx) (err error) {
		p, err = store.CreateProject(planDCtx, tx, store.Meta{Actor: domain.Human, Now: planDT0},
			domain.Project{Slug: "demo", Name: "Demo"})
		return
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := New(db,
		WithClock(func() time.Time { return planDT0 }),
		WithEnv(func(string) string { return "" }),
		WithWorkdir(func() (string, error) { return "/definitely/not/a/project", nil }),
	)
	return svc, db, p
}

func planDCount(t *testing.T, db *store.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRowContext(planDCtx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// planDRows returns the row counts that a rolled-back plan must leave at zero.
func planDRows(t *testing.T, db *store.DB) (features, tasks, deps, events int) {
	t.Helper()
	return planDCount(t, db, `SELECT COUNT(*) FROM features`),
		planDCount(t, db, `SELECT COUNT(*) FROM tasks`),
		planDCount(t, db, `SELECT COUNT(*) FROM task_dependencies`),
		planDCount(t, db, `SELECT COUNT(*) FROM events WHERE entity_type IN ('feature','task')`)
}

func planDPrio(n int) *int { return &n }

func planDBasic() PlanInput {
	return PlanInput{Features: []PlanFeature{
		{Title: "Store", Weight: 3, Tasks: []PlanTask{
			{Ref: "schema", Title: "Write schema", AcceptanceCriteria: "tables exist", Priority: planDPrio(0)},
			{Ref: "queries", Title: "Write queries", DependsOn: []string{"schema"}, Weight: 5},
		}},
		{Title: "CLI", Tasks: []PlanTask{
			{Ref: "cmds", Title: "Write commands", DependsOn: []string{"queries"}},
			{Ref: "tests", Title: "CLI tests", DependsOn: []string{"cmds", "schema"}},
		}},
	}}
}

func TestCreatePlanHappyPath(t *testing.T) {
	svc, db, p := planDSetup(t)
	res, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, planDBasic())
	if err != nil {
		t.Fatal(err)
	}
	if res.ProjectID != p.ID || res.ProjectSlug != "demo" {
		t.Fatalf("project = %d %q", res.ProjectID, res.ProjectSlug)
	}
	if res.FeaturesCreated != 2 || res.FeaturesReused != 0 || res.TasksCreated != 4 || res.TasksReused != 0 {
		t.Fatalf("counts = %+v", res)
	}
	// schema->queries, queries->cmds, cmds->tests, schema->tests
	if res.DependenciesAdded != 4 {
		t.Fatalf("DependenciesAdded = %d, want 4", res.DependenciesAdded)
	}
	if len(res.Features) != 2 || res.Features[0].Title != "Store" || res.Features[1].Title != "CLI" || !res.Features[0].Created {
		t.Fatalf("features = %+v", res.Features)
	}
	if len(res.Tasks) != 4 {
		t.Fatalf("tasks = %+v", res.Tasks)
	}

	schema, queries, cmds, tests := res.Tasks["schema"], res.Tasks["queries"], res.Tasks["cmds"], res.Tasks["tests"]
	if schema.FeatureID != res.Features[0].FeatureID || cmds.FeatureID != res.Features[1].FeatureID {
		t.Fatalf("feature ids wrong: %+v %+v", schema, cmds)
	}
	// A cross-feature dependency was stored.
	deps, err := store.ListDependencyIDs(planDCtx, db.Reader(), tests.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 2 {
		t.Fatalf("tests deps = %v", deps)
	}
	got, err := store.ListDependencyIDs(planDCtx, db.Reader(), cmds.TaskID)
	if err != nil || len(got) != 1 || got[0] != queries.TaskID {
		t.Fatalf("cmds deps = %v, %v", got, err)
	}

	// Field values and order are stored.
	task, err := store.GetTask(planDCtx, db.Reader(), schema.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Priority != 0 || task.AcceptanceCriteria != "tables exist" || task.Status != domain.TaskTodo {
		t.Fatalf("schema task = %+v", task)
	}
	feats, err := store.ListFeatures(planDCtx, db.Reader(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(feats) != 2 || feats[0].Title != "Store" || feats[0].Weight != 3 || feats[0].Position >= feats[1].Position {
		t.Fatalf("features = %+v", feats)
	}
	// queries (not ready) vs schema (ready).
	snap, err := db.ReadSnapshot(planDCtx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	ready := snap.ReadyTasks()
	if len(ready) != 1 || ready[0].ID != schema.TaskID {
		t.Fatalf("ready = %+v", ready)
	}
}

func TestCreatePlanEmitsEventPerCreatedEntity(t *testing.T) {
	svc, db, _ := planDSetup(t)
	actor := domain.NewAgentActor("claude-code", "s1")
	if _, err := svc.CreatePlan(planDCtx, actor, ProjectRef{Slug: "demo"}, planDBasic()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		typ  domain.EventType
		want int
	}{
		{domain.EventFeatureCreated, 2},
		{domain.EventTaskCreated, 4},
		{domain.EventTaskDepAdded, 4},
	} {
		if n := planDCount(t, db, `SELECT COUNT(*) FROM events WHERE type = ? AND actor = ?`, string(c.typ), string(actor)); n != c.want {
			t.Errorf("%s events = %d, want %d", c.typ, n, c.want)
		}
	}
}

func TestCreatePlanRollsBackOnUnknownRef(t *testing.T) {
	svc, db, _ := planDSetup(t)
	in := planDBasic()
	in.Features[1].Tasks[1].DependsOn = []string{"cmds", "nope"}
	_, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, in)
	if domain.KindOf(err) != domain.KindValidation || !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("err = %v", err)
	}
	if f, tk, d, e := planDRows(t, db); f+tk+d+e != 0 {
		t.Fatalf("rows left behind: features=%d tasks=%d deps=%d events=%d", f, tk, d, e)
	}
}

func TestCreatePlanCycleInPlanReportsRefs(t *testing.T) {
	svc, db, _ := planDSetup(t)
	in := PlanInput{Features: []PlanFeature{{Title: "F", Tasks: []PlanTask{
		{Ref: "a", Title: "A", DependsOn: []string{"c"}},
		{Ref: "b", Title: "B", DependsOn: []string{"a"}},
		{Ref: "c", Title: "C", DependsOn: []string{"b"}},
		{Ref: "d", Title: "D"},
	}}}}
	_, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, in)
	if !errors.Is(err, domain.ErrDependencyCycle) || domain.KindOf(err) != domain.KindDependencyCycle {
		t.Fatalf("err = %v", err)
	}
	var pc *PlanCycleError
	if !errors.As(err, &pc) {
		t.Fatalf("not a PlanCycleError: %T", err)
	}
	if got := strings.Join(pc.Refs, ","); got != "a,c,b,a" {
		t.Fatalf("cycle = %s", got)
	}
	if !strings.Contains(err.Error(), "a -> c -> b -> a") {
		t.Fatalf("message = %q", err.Error())
	}
	if f, tk, d, e := planDRows(t, db); f+tk+d+e != 0 {
		t.Fatalf("rows left behind: %d %d %d %d", f, tk, d, e)
	}
}

func TestCreatePlanValidation(t *testing.T) {
	svc, db, _ := planDSetup(t)
	task := func(mod func(*PlanTask)) PlanInput {
		pt := PlanTask{Ref: "a", Title: "A"}
		mod(&pt)
		return PlanInput{Features: []PlanFeature{{Title: "F", Tasks: []PlanTask{pt, {Ref: "b", Title: "B", IdempotencyKey: "kb"}}}}}
	}
	cases := map[string]PlanInput{
		"no features":       {},
		"empty feature":     {Features: []PlanFeature{{Title: "  "}}},
		"feature weight":    {Features: []PlanFeature{{Title: "F", Weight: 11}}},
		"duplicate feature": {Features: []PlanFeature{{Title: "F"}, {Title: "F"}}},
		"empty ref":         task(func(p *PlanTask) { p.Ref = " " }),
		"duplicate ref":     task(func(p *PlanTask) { p.Ref = "b" }),
		"empty title":       task(func(p *PlanTask) { p.Title = "" }),
		"task weight":       task(func(p *PlanTask) { p.Weight = -1 }),
		"task priority":     task(func(p *PlanTask) { p.Priority = planDPrio(4) }),
		"self dependency":   task(func(p *PlanTask) { p.DependsOn = []string{"a"} }),
		"duplicate key":     task(func(p *PlanTask) { p.IdempotencyKey = "kb" }),
		"bad existing id":   task(func(p *PlanTask) { p.DependsOnExisting = []int64{0} }),
		"missing existing":  task(func(p *PlanTask) { p.DependsOnExisting = []int64{999} }),
		"key too long":      task(func(p *PlanTask) { p.IdempotencyKey = strings.Repeat("k", maxKeyLen+1) }),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, in)
			if domain.KindOf(err) != domain.KindValidation {
				t.Fatalf("err = %v (kind %v), want validation", err, domain.KindOf(err))
			}
			var ve *domain.ValidationError
			if !errors.As(err, &ve) || ve.Field == "" || ve.Hint == "" {
				t.Fatalf("validation error lacks field or hint: %#v", err)
			}
		})
	}
	if f, tk, d, e := planDRows(t, db); f+tk+d+e != 0 {
		t.Fatalf("rows left behind: %d %d %d %d", f, tk, d, e)
	}
}

func TestCreatePlanOversize(t *testing.T) {
	svc, db, _ := planDSetup(t)
	tasks := make([]PlanTask, MaxPlanTasks+1)
	for i := range tasks {
		tasks[i] = PlanTask{Ref: "t" + string(rune('a'+i%26)) + strings.Repeat("x", i/26), Title: "T"}
	}
	_, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, PlanInput{Features: []PlanFeature{{Title: "Big", Tasks: tasks}}})
	if domain.KindOf(err) != domain.KindValidation || !strings.Contains(err.Error(), "limit is 200") {
		t.Fatalf("err = %v", err)
	}
	if f, tk, _, _ := planDRows(t, db); f+tk != 0 {
		t.Fatal("rows left behind")
	}

	// Exactly at the limit is fine.
	ok, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, PlanInput{Features: []PlanFeature{{Title: "Big", Tasks: tasks[:MaxPlanTasks]}}})
	if err != nil || ok.TasksCreated != MaxPlanTasks {
		t.Fatalf("at limit: %v %+v", err, ok.TasksCreated)
	}

	feats := make([]PlanFeature, MaxPlanFeatures+1)
	for i := range feats {
		feats[i] = PlanFeature{Title: "f" + strings.Repeat("x", i)}
	}
	if _, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, PlanInput{Features: feats}); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("too many features: %v", err)
	}
}

func planDKeyed() PlanInput {
	return PlanInput{Features: []PlanFeature{
		{Title: "Store", Tasks: []PlanTask{
			{Ref: "schema", Title: "Write schema", IdempotencyKey: "k-schema"},
			{Ref: "queries", Title: "Write queries", IdempotencyKey: "k-queries", DependsOn: []string{"schema"}},
		}},
		{Title: "CLI", Tasks: []PlanTask{
			{Ref: "cmds", Title: "Write commands", IdempotencyKey: "k-cmds", DependsOn: []string{"queries"}},
		}},
	}}
}

func TestCreatePlanIdempotentRerun(t *testing.T) {
	svc, db, _ := planDSetup(t)
	first, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, planDKeyed())
	if err != nil {
		t.Fatal(err)
	}
	f0, t0n, d0, e0 := planDRows(t, db)

	second, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, planDKeyed())
	if err != nil {
		t.Fatal(err)
	}
	if second.FeaturesCreated != 0 || second.FeaturesReused != 2 || second.TasksCreated != 0 || second.TasksReused != 3 || second.DependenciesAdded != 0 {
		t.Fatalf("second = %+v", second)
	}
	for ref, r := range first.Tasks {
		if got := second.Tasks[ref]; got.TaskID != r.TaskID || got.Created || got.FeatureID != r.FeatureID {
			t.Errorf("task %s: first %+v second %+v", ref, r, got)
		}
	}
	for i, f := range first.Features {
		if second.Features[i].FeatureID != f.FeatureID || second.Features[i].Created {
			t.Errorf("feature %d: first %+v second %+v", i, f, second.Features[i])
		}
	}
	f1, t1n, d1, e1 := planDRows(t, db)
	if f0 != f1 || t0n != t1n || d0 != d1 || e0 != e1 {
		t.Fatalf("rows changed on rerun: (%d %d %d %d) -> (%d %d %d %d)", f0, t0n, d0, e0, f1, t1n, d1, e1)
	}
}

func TestCreatePlanRerunReconcilesMissingDependency(t *testing.T) {
	svc, db, _ := planDSetup(t)
	flat := planDKeyed()
	for fi := range flat.Features {
		for ti := range flat.Features[fi].Tasks {
			flat.Features[fi].Tasks[ti].DependsOn = nil
		}
	}
	if _, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, flat); err != nil {
		t.Fatal(err)
	}
	if _, _, d, _ := planDRows(t, db); d != 0 {
		t.Fatalf("deps = %d", d)
	}
	res, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, planDKeyed())
	if err != nil {
		t.Fatal(err)
	}
	if res.TasksCreated != 0 || res.DependenciesAdded != 2 {
		t.Fatalf("res = %+v", res)
	}
	if _, _, d, _ := planDRows(t, db); d != 2 {
		t.Fatalf("deps = %d, want 2", d)
	}
}

func TestCreatePlanPartialOverlapRerun(t *testing.T) {
	svc, db, _ := planDSetup(t)
	first, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, planDKeyed())
	if err != nil {
		t.Fatal(err)
	}
	in := planDKeyed()
	in.Features[0].Tasks = append(in.Features[0].Tasks, PlanTask{Ref: "extra", Title: "Extra store task", IdempotencyKey: "k-extra", DependsOn: []string{"queries"}})
	in.Features = append(in.Features, PlanFeature{Title: "Web", Tasks: []PlanTask{
		{Ref: "ui", Title: "UI", IdempotencyKey: "k-ui", DependsOn: []string{"cmds"}},
	}})
	res, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.FeaturesCreated != 1 || res.FeaturesReused != 2 || res.TasksCreated != 2 || res.TasksReused != 3 || res.DependenciesAdded != 2 {
		t.Fatalf("res = %+v", res)
	}
	// The new task joined the reused feature.
	if got := res.Tasks["extra"]; !got.Created || got.FeatureID != first.Features[0].FeatureID {
		t.Fatalf("extra = %+v, store feature = %d", got, first.Features[0].FeatureID)
	}
	if f, tk, _, _ := planDRows(t, db); f != 3 || tk != 5 {
		t.Fatalf("features=%d tasks=%d", f, tk)
	}
}

func TestCreatePlanUnkeyedFeatureIsNotReused(t *testing.T) {
	svc, db, _ := planDSetup(t)
	in := PlanInput{Features: []PlanFeature{{Title: "F", Tasks: []PlanTask{{Ref: "a", Title: "A"}}}}}
	for i := 0; i < 2; i++ {
		if _, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, in); err != nil {
			t.Fatal(err)
		}
	}
	if f, tk, _, _ := planDRows(t, db); f != 2 || tk != 2 {
		t.Fatalf("features=%d tasks=%d, want 2 and 2 (no keys, no idempotency)", f, tk)
	}
}

func TestCreatePlanFeatureWithoutTasksIsReused(t *testing.T) {
	svc, db, _ := planDSetup(t)
	in := PlanInput{Features: []PlanFeature{{Title: "Empty"}}}
	for i := 0; i < 2; i++ {
		if _, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, in); err != nil {
			t.Fatal(err)
		}
	}
	if f, _, _, _ := planDRows(t, db); f != 1 {
		t.Fatalf("features = %d, want 1", f)
	}
}

func TestCreatePlanCancelledFeatureIsNotReused(t *testing.T) {
	svc, db, p := planDSetup(t)
	first, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, planDKeyed())
	if err != nil {
		t.Fatal(err)
	}
	err = db.WriteTx(planDCtx, func(tx *sql.Tx) error {
		_, err := store.CancelFeature(planDCtx, tx, store.Meta{Actor: domain.Human, Now: planDT0}, first.Features[1].FeatureID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, planDKeyed())
	if err != nil {
		t.Fatal(err)
	}
	// "CLI" was cancelled, so a new feature is created; its keyed task already
	// exists and is reused in its old feature.
	if res.FeaturesCreated != 1 || res.Features[1].Created != true || res.Features[1].FeatureID == first.Features[1].FeatureID {
		t.Fatalf("features = %+v", res.Features)
	}
	if got := res.Tasks["cmds"]; got.Created || got.TaskID != first.Tasks["cmds"].TaskID || got.FeatureID != first.Features[1].FeatureID {
		t.Fatalf("cmds = %+v", got)
	}
	_ = p
}

func TestCreatePlanDependsOnExistingTasks(t *testing.T) {
	svc, db, p := planDSetup(t)
	base, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, PlanInput{Features: []PlanFeature{{Title: "Base", Tasks: []PlanTask{
		{Ref: "root", Title: "Root", IdempotencyKey: "root"},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	rootID := base.Tasks["root"].TaskID

	res, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, PlanInput{Features: []PlanFeature{{Title: "Next", Tasks: []PlanTask{
		{Ref: "n1", Title: "N1", DependsOnExisting: []int64{rootID}},
		{Ref: "n2", Title: "N2", DependsOn: []string{"n1"}, DependsOnExisting: []int64{rootID}},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.DependenciesAdded != 3 {
		t.Fatalf("DependenciesAdded = %d", res.DependenciesAdded)
	}
	deps, err := store.ListDependencyIDs(planDCtx, db.Reader(), res.Tasks["n2"].TaskID)
	if err != nil || len(deps) != 2 {
		t.Fatalf("n2 deps = %v, %v", deps, err)
	}

	// An existing task from another project is rejected with a clear error.
	var other domain.Project
	var otherTask domain.Task
	err = db.WriteTx(planDCtx, func(tx *sql.Tx) error {
		m := store.Meta{Actor: domain.Human, Now: planDT0}
		var err error
		if other, err = store.CreateProject(planDCtx, tx, m, domain.Project{Slug: "other", Name: "Other"}); err != nil {
			return err
		}
		f, err := store.CreateFeature(planDCtx, tx, m, domain.Feature{ProjectID: other.ID, Title: "OF"})
		if err != nil {
			return err
		}
		otherTask, _, err = store.CreateTask(planDCtx, tx, m, store.TaskInput{ProjectID: other.ID, FeatureID: f.ID, Title: "OT"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeF, beforeT, beforeD, beforeE := planDRows(t, db)
	_, err = svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, PlanInput{Features: []PlanFeature{{Title: "Bad", Tasks: []PlanTask{
		{Ref: "x", Title: "X", DependsOnExisting: []int64{otherTask.ID}},
	}}}})
	if domain.KindOf(err) != domain.KindValidation || !strings.Contains(err.Error(), "not a task in project") {
		t.Fatalf("err = %v", err)
	}
	f, tk, d, e := planDRows(t, db)
	if f != beforeF || tk != beforeT || d != beforeD || e != beforeE {
		t.Fatal("rows changed after rejected plan")
	}
	_ = p
}

func TestCreatePlanCycleThroughExistingTasksRollsBack(t *testing.T) {
	svc, db, _ := planDSetup(t)
	base, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, PlanInput{Features: []PlanFeature{{Title: "Base", Tasks: []PlanTask{
		{Ref: "a", Title: "A", IdempotencyKey: "ka"},
		{Ref: "b", Title: "B", IdempotencyKey: "kb", DependsOn: []string{"a"}},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	a, b := base.Tasks["a"].TaskID, base.Tasks["b"].TaskID
	beforeF, beforeT, beforeD, beforeE := planDRows(t, db)

	// Re-submit "a" (reused) and make it depend on "b", which already depends on "a".
	// The new task "c" must roll back with the failed edge.
	_, err = svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, PlanInput{Features: []PlanFeature{{Title: "Base", Tasks: []PlanTask{
		{Ref: "a", Title: "A", IdempotencyKey: "ka", DependsOnExisting: []int64{b}},
		{Ref: "c", Title: "C", IdempotencyKey: "kc"},
	}}}})
	var ce *domain.DependencyCycleError
	if !errors.As(err, &ce) || domain.KindOf(err) != domain.KindDependencyCycle {
		t.Fatalf("err = %v", err)
	}
	if len(ce.Path) == 0 || ce.Path[0] != a || ce.Path[len(ce.Path)-1] != a {
		t.Fatalf("path = %v", ce.Path)
	}
	f, tk, d, e := planDRows(t, db)
	if f != beforeF || tk != beforeT || d != beforeD || e != beforeE {
		t.Fatalf("rows changed: (%d %d %d %d) -> (%d %d %d %d)", beforeF, beforeT, beforeD, beforeE, f, tk, d, e)
	}
}

func TestCreatePlanRejectsArchivedProject(t *testing.T) {
	svc, db, p := planDSetup(t)
	err := db.WriteTx(planDCtx, func(tx *sql.Tx) error {
		_, err := store.ArchiveProject(planDCtx, tx, store.Meta{Actor: domain.Human, Now: planDT0}, p.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "demo"}, planDBasic())
	if domain.KindOf(err) != domain.KindValidation || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("err = %v", err)
	}
}

func TestCreatePlanUnknownProject(t *testing.T) {
	svc, _, _ := planDSetup(t)
	_, err := svc.CreatePlan(planDCtx, domain.Human, ProjectRef{Slug: "ghost"}, planDBasic())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}
