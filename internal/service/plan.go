package service

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"skald/internal/domain"
	"skald/internal/store"
)

// Plan size limits. A bigger plan is almost certainly a mistake and would make
// one very long write transaction.
const (
	MaxPlanTasks    = 200
	MaxPlanFeatures = 50
	maxKeyLen       = 200
)

// PlanInput is a whole project plan: features, each with tasks. Feature and
// task order in the input becomes their position.
type PlanInput struct {
	Features []PlanFeature
}

// PlanFeature is one feature in a plan. Weight 0 means 1.
type PlanFeature struct {
	Title       string
	Description string
	Weight      int
	Tasks       []PlanTask
}

// PlanTask is one task in a plan.
type PlanTask struct {
	// Ref names the task inside this plan so other tasks can depend on it.
	// Required and unique within the plan. Refs are not stored.
	Ref                string
	Title              string
	Description        string
	AcceptanceCriteria string
	Priority           *int // nil means 2 (normal); 0 is urgent
	Weight             int  // 0 means 1
	// DependsOn lists refs of other tasks in this plan, in any feature.
	DependsOn []string
	// DependsOnExisting lists IDs of tasks already in the project.
	DependsOnExisting []int64
	// IdempotencyKey makes the task safe to re-submit; see CreatePlan.
	IdempotencyKey string
}

// PlanResult says what CreatePlan created and what it reused.
type PlanResult struct {
	ProjectID   int64
	ProjectSlug string
	Features    []PlanFeatureResult       // in input order
	Tasks       map[string]PlanTaskResult // by task ref

	FeaturesCreated   int
	FeaturesReused    int
	TasksCreated      int
	TasksReused       int
	DependenciesAdded int // edges that did not exist before
}

// PlanFeatureResult is the outcome for one feature.
type PlanFeatureResult struct {
	Title     string
	FeatureID int64
	Created   bool
}

// PlanTaskResult is the outcome for one task.
type PlanTaskResult struct {
	Ref       string
	TaskID    int64
	FeatureID int64 // the feature the task actually lives in (a reused task keeps its own)
	Created   bool
}

// PlanCycleError is returned when the plan's own depends_on entries form a
// cycle. It carries refs because the tasks have no IDs yet. It matches
// errors.Is(err, domain.ErrDependencyCycle) and reports KindDependencyCycle.
type PlanCycleError struct {
	// Refs lists the cycle in order; the last ref repeats the first.
	Refs []string
}

func (e *PlanCycleError) Kind() domain.ErrorKind { return domain.KindDependencyCycle }
func (e *PlanCycleError) Error() string          { return e.Message() }
func (e *PlanCycleError) Is(t error) bool {
	_, ok := t.(*domain.DependencyCycleError)
	return ok
}
func (e *PlanCycleError) Message() string {
	return fmt.Sprintf("The plan has a circular dependency: %s (each task depends on the next). Remove one depends_on entry in the cycle and resubmit.", strings.Join(e.Refs, " -> "))
}

// CreatePlan creates features, tasks and dependencies in ONE transaction.
// Input is validated before anything is written, and any failure (bad ref,
// cycle, unknown existing task) rolls the whole plan back, leaving no rows and
// no events.
//
// Dependencies: DependsOn refers to tasks of this plan; DependsOnExisting to
// tasks already in the project (other projects are rejected). Cycles inside
// the plan are reported as a *PlanCycleError before any write. Cycles that go
// through tasks already in the database are caught when the edge is added and
// reported as a *domain.DependencyCycleError with task IDs.
//
// Idempotency rule (re-running the same plan creates nothing new):
//   - A task with an IdempotencyKey that already exists in the project is
//     reused, not modified, and keeps whatever feature it already has. A task
//     without a key is created every time.
//   - A plan feature is matched to an existing non-cancelled feature of the
//     same title in the project, and reused, only when EVERY task in it
//     carries an IdempotencyKey (a feature with no tasks matches by title).
//     Otherwise the feature is created. If several existing features share the
//     title, the one with the lowest ID is used.
//   - New tasks of a reused feature are appended to it.
//   - Dependencies are reconciled: every listed edge is added if missing.
//     Edges already in the database that the plan does not list are kept.
//
// Within a plan, feature titles and idempotency keys must be unique.
func (s *Service) CreatePlan(ctx context.Context, actor domain.Actor, ref ProjectRef, in PlanInput) (PlanResult, error) {
	in, err := normalizePlan(in)
	if err != nil {
		return PlanResult{}, err
	}
	project, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return PlanResult{}, err
	}
	if project.Status != domain.ProjectActive {
		return PlanResult{}, &domain.ValidationError{Field: "project", Problem: fmt.Sprintf("project %q is archived", project.Slug), Hint: "Plan into an active project."}
	}

	var res PlanResult
	err = s.db.WriteTx(ctx, func(tx *sql.Tx) error {
		res = PlanResult{ProjectID: project.ID, ProjectSlug: project.Slug, Tasks: map[string]PlanTaskResult{}}
		return writePlan(ctx, tx, s.meta(actor), project, in, &res)
	})
	if err != nil {
		return PlanResult{}, err
	}
	return res, nil
}

func writePlan(ctx context.Context, tx *sql.Tx, m store.Meta, project domain.Project, in PlanInput, res *PlanResult) error {
	// Existing prerequisites must be in this project.
	for fi, f := range in.Features {
		for ti, t := range f.Tasks {
			for _, id := range t.DependsOnExisting {
				if _, err := store.GetTaskInProject(ctx, tx, project.ID, id); err != nil {
					if domain.KindOf(err) == domain.KindNotFound {
						return &domain.ValidationError{
							Field:   fmt.Sprintf("features[%d].tasks[%d].deps_existing", fi, ti),
							Problem: fmt.Sprintf("%s is not a task in project %q", domain.FormatID(domain.EntityTask, id), project.Slug),
							Hint:    "Use IDs of tasks in this project, or list the prerequisite in this plan and use depends_on.",
						}
					}
					return err
				}
			}
		}
	}

	existing, err := store.ListFeatures(ctx, tx, project.ID)
	if err != nil {
		return err
	}
	byTitle := map[string]domain.Feature{}
	for _, f := range existing { // ListFeatures is ordered; keep the lowest ID per title
		if f.Status == domain.FeatureCancelled {
			continue
		}
		if cur, ok := byTitle[f.Title]; !ok || f.ID < cur.ID {
			byTitle[f.Title] = f
		}
	}

	taskIDs := map[string]int64{} // ref -> task id
	for _, pf := range in.Features {
		var feat domain.Feature
		reused := false
		if allKeyed(pf) {
			feat, reused = byTitle[pf.Title]
		}
		if !reused {
			feat, err = store.CreateFeature(ctx, tx, m, domain.Feature{
				ProjectID: project.ID, Title: pf.Title, Description: pf.Description, Weight: pf.Weight,
			})
			if err != nil {
				return err
			}
			res.FeaturesCreated++
		} else {
			res.FeaturesReused++
		}
		res.Features = append(res.Features, PlanFeatureResult{Title: pf.Title, FeatureID: feat.ID, Created: !reused})

		for _, pt := range pf.Tasks {
			task, created, err := store.CreateTask(ctx, tx, m, store.TaskInput{
				ProjectID: project.ID, FeatureID: feat.ID,
				Title: pt.Title, Description: pt.Description, AcceptanceCriteria: pt.AcceptanceCriteria,
				Priority: pt.Priority, Weight: pt.Weight, IdempotencyKey: pt.IdempotencyKey,
			})
			if err != nil {
				return err
			}
			if created {
				res.TasksCreated++
			} else {
				res.TasksReused++
			}
			taskIDs[pt.Ref] = task.ID
			res.Tasks[pt.Ref] = PlanTaskResult{Ref: pt.Ref, TaskID: task.ID, FeatureID: task.FeatureID, Created: created}
		}
	}

	// All tasks exist now; add edges. AddDependency checks cycles against the
	// live graph, which covers cycles through reused and existing tasks.
	for _, pf := range in.Features {
		for _, pt := range pf.Tasks {
			id := taskIDs[pt.Ref]
			for _, depRef := range pt.DependsOn {
				added, err := store.AddDependency(ctx, tx, m, id, taskIDs[depRef])
				if err != nil {
					return err
				}
				if added {
					res.DependenciesAdded++
				}
			}
			for _, depID := range pt.DependsOnExisting {
				added, err := store.AddDependency(ctx, tx, m, id, depID)
				if err != nil {
					return err
				}
				if added {
					res.DependenciesAdded++
				}
			}
		}
	}
	return nil
}

// allKeyed reports whether every task of the feature carries an idempotency
// key (true for a feature with no tasks).
func allKeyed(f PlanFeature) bool {
	for _, t := range f.Tasks {
		if t.IdempotencyKey == "" {
			return false
		}
	}
	return true
}

// normalizePlan trims text, validates the whole plan without touching the
// database and returns the normalized copy.
func normalizePlan(in PlanInput) (PlanInput, error) {
	bad := func(field, problem, hint string) error {
		return &domain.ValidationError{Field: field, Problem: problem, Hint: hint}
	}
	if len(in.Features) == 0 {
		return in, bad("features", "the plan has no features", "Add at least one feature with tasks.")
	}
	if len(in.Features) > MaxPlanFeatures {
		return in, bad("features", fmt.Sprintf("the plan has %d features, the limit is %d", len(in.Features), MaxPlanFeatures), "Split the plan into several create_plan calls.")
	}

	out := PlanInput{Features: make([]PlanFeature, len(in.Features))}
	total := 0
	for _, f := range in.Features {
		total += len(f.Tasks)
	}
	if total > MaxPlanTasks {
		return in, bad("features", fmt.Sprintf("the plan has %d tasks, the limit is %d", total, MaxPlanTasks), "Split the plan into several create_plan calls, one or a few features each.")
	}

	refs := map[string]bool{}
	keys := map[string]bool{}
	titles := map[string]bool{}
	for fi, f := range in.Features {
		fp := fmt.Sprintf("features[%d]", fi)
		f.Title = strings.TrimSpace(f.Title)
		if f.Title == "" {
			return in, bad(fp+".title", "feature title is empty", "Give every feature a title.")
		}
		if titles[f.Title] {
			return in, bad(fp+".title", fmt.Sprintf("feature title %q appears twice in the plan", f.Title), "Merge the features or give them different titles.")
		}
		titles[f.Title] = true
		if f.Weight < 0 || f.Weight > 10 {
			return in, bad(fp+".weight", fmt.Sprintf("weight %d is out of range", f.Weight), "Use a weight from 1 to 10 (0 means 1).")
		}
		tasks := make([]PlanTask, len(f.Tasks))
		for ti, t := range f.Tasks {
			tp := fmt.Sprintf("%s.tasks[%d]", fp, ti)
			t.Ref = strings.TrimSpace(t.Ref)
			t.Title = strings.TrimSpace(t.Title)
			t.IdempotencyKey = strings.TrimSpace(t.IdempotencyKey)
			if t.Ref == "" {
				return in, bad(tp+".ref", "task ref is empty", "Give every task a short unique ref so other tasks can depend on it.")
			}
			if refs[t.Ref] {
				return in, bad(tp+".ref", fmt.Sprintf("ref %q is used by more than one task", t.Ref), "Refs must be unique within the plan.")
			}
			refs[t.Ref] = true
			if t.Title == "" {
				return in, bad(tp+".title", fmt.Sprintf("task %q has an empty title", t.Ref), "Give every task a title.")
			}
			if t.Weight < 0 || t.Weight > 10 {
				return in, bad(tp+".weight", fmt.Sprintf("task %q: weight %d is out of range", t.Ref, t.Weight), "Use a weight from 1 to 10 (0 means 1).")
			}
			if t.Priority != nil && (*t.Priority < 0 || *t.Priority > 3) {
				return in, bad(tp+".priority", fmt.Sprintf("task %q: priority %d is out of range", t.Ref, *t.Priority), "Use 0 (urgent) to 3 (low).")
			}
			if len(t.IdempotencyKey) > maxKeyLen {
				return in, bad(tp+".idempotency_key", fmt.Sprintf("task %q: idempotency key is longer than %d characters", t.Ref, maxKeyLen), "Use a short stable key.")
			}
			if t.IdempotencyKey != "" {
				if keys[t.IdempotencyKey] {
					return in, bad(tp+".idempotency_key", fmt.Sprintf("idempotency key %q is used by more than one task", t.IdempotencyKey), "Keys must be unique within the plan, otherwise two tasks would collapse into one.")
				}
				keys[t.IdempotencyKey] = true
			}
			t.DependsOn = append([]string(nil), t.DependsOn...)
			for i := range t.DependsOn {
				t.DependsOn[i] = strings.TrimSpace(t.DependsOn[i])
			}
			t.DependsOnExisting = append([]int64(nil), t.DependsOnExisting...)
			tasks[ti] = t
		}
		f.Tasks = tasks
		out.Features[fi] = f
	}

	// Refs are all known now, so check depends_on targets and cycles.
	graph := map[string][]string{}
	for fi, f := range out.Features {
		for ti, t := range f.Tasks {
			tp := fmt.Sprintf("features[%d].tasks[%d]", fi, ti)
			for _, d := range t.DependsOn {
				switch {
				case d == t.Ref:
					return in, bad(tp+".depends_on", fmt.Sprintf("task %q depends on itself", t.Ref), "Remove the self-dependency.")
				case !refs[d]:
					return in, bad(tp+".depends_on", fmt.Sprintf("task %q depends on unknown ref %q", t.Ref, d), "Use the ref of another task in this plan, or deps_existing for tasks already in the project.")
				}
			}
			for _, id := range t.DependsOnExisting {
				if id <= 0 {
					return in, bad(tp+".deps_existing", fmt.Sprintf("task %q: %d is not a valid task ID", t.Ref, id), "Use positive task IDs.")
				}
			}
			graph[t.Ref] = t.DependsOn
		}
	}
	if cycle := findRefCycle(out, graph); cycle != nil {
		return in, &PlanCycleError{Refs: cycle}
	}
	return out, nil
}

// findRefCycle returns one dependency cycle among task refs as
// [a, b, ..., a], or nil. Tasks are visited in plan order so the result is
// deterministic.
func findRefCycle(in PlanInput, graph map[string][]string) []string {
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var stack []string
	var visit func(ref string) []string
	visit = func(ref string) []string {
		color[ref] = grey
		stack = append(stack, ref)
		for _, d := range graph[ref] {
			switch color[d] {
			case grey:
				i := len(stack) - 1
				for stack[i] != d {
					i--
				}
				return append(append([]string(nil), stack[i:]...), d)
			case white:
				if c := visit(d); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[ref] = black
		return nil
	}
	for _, f := range in.Features {
		for _, t := range f.Tasks {
			if color[t.Ref] == white {
				if c := visit(t.Ref); c != nil {
					return c
				}
			}
		}
	}
	return nil
}
