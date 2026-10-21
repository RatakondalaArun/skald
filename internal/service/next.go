package service

import (
	"context"
	"database/sql"
	"sort"

	"skald/internal/domain"
	"skald/internal/store"
)

// Limits for NextTasks. A limit of zero or less means DefaultNextLimit; larger
// values are capped at MaxNextLimit.
const (
	DefaultNextLimit = 10
	MaxNextLimit     = 50
)

// NextTasksResult is the answer to "what should I do now". Tasks holds at most
// the requested number of ready tasks; Total is how many are ready in all.
type NextTasksResult struct {
	Project   domain.Project
	Tasks     []domain.Task
	Total     int
	Truncated bool
}

// TaskCounts is the number of tasks in each status.
type TaskCounts struct {
	Todo       int
	InProgress int
	Blocked    int
	Done       int
	Cancelled  int
	Total      int
}

func (c *TaskCounts) add(s domain.TaskStatus) {
	c.Total++
	switch s {
	case domain.TaskTodo:
		c.Todo++
	case domain.TaskInProgress:
		c.InProgress++
	case domain.TaskBlocked:
		c.Blocked++
	case domain.TaskDone:
		c.Done++
	case domain.TaskCancelled:
		c.Cancelled++
	}
}

// FeatureReport is one feature with its derived status, progress and counts.
type FeatureReport struct {
	Feature  domain.Feature
	Status   domain.FeatureStatus // derived from its tasks
	Progress domain.Progress
	Counted  bool // whether it contributes to the project figure
	Counts   TaskCounts
}

// BlockedTask is a task that was explicitly blocked, with its reason.
type BlockedTask struct {
	Task   domain.Task
	Reason string
}

// ProgressReport is the derived state of a project. Progress is computed from
// task state and weights, never stored.
type ProgressReport struct {
	Project    domain.Project
	Progress   domain.Progress
	Features   []FeatureReport
	Counts     TaskCounts
	Blocked    []BlockedTask
	Stale      []domain.Task // in_progress and untouched for domain.StaleThreshold
	ReadyCount int
}

// ProjectSummary is what the MCP get_project tool returns: the progress report
// plus the project's documents (without bodies) and how many scratchpad notes
// it has.
type ProjectSummary struct {
	ProgressReport
	Documents []domain.Document
	NoteCount int
}

// NextTasks returns the ready tasks of a project (todo, every dependency done
// or cancelled), ordered by priority, then feature position, then task
// position, then ID. limit caps the result; Truncated says more are ready.
func (s *Service) NextTasks(ctx context.Context, ref ProjectRef, limit int) (NextTasksResult, error) {
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return NextTasksResult{}, err
	}
	snap, err := s.db.ReadSnapshot(ctx, p.ID)
	if err != nil {
		return NextTasksResult{}, err
	}
	if limit <= 0 {
		limit = DefaultNextLimit
	}
	if limit > MaxNextLimit {
		limit = MaxNextLimit
	}
	ready := orderedReady(snap)
	res := NextTasksResult{Project: snap.Project, Total: len(ready), Tasks: ready}
	if len(ready) > limit {
		res.Tasks, res.Truncated = ready[:limit], true
	}
	return res, nil
}

// ProjectProgress computes a project's progress report from one consistent
// snapshot.
func (s *Service) ProjectProgress(ctx context.Context, ref ProjectRef) (ProgressReport, error) {
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return ProgressReport{}, err
	}
	snap, err := s.db.ReadSnapshot(ctx, p.ID)
	if err != nil {
		return ProgressReport{}, err
	}
	return s.report(snap), nil
}

// ProjectSummary returns the progress report and document list from one
// consistent snapshot.
func (s *Service) ProjectSummary(ctx context.Context, ref ProjectRef) (ProjectSummary, error) {
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return ProjectSummary{}, err
	}
	var sum ProjectSummary
	err = s.db.ReadTx(ctx, func(tx *sql.Tx) error {
		snap, err := store.LoadSnapshot(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		sum.ProgressReport = s.report(snap)
		if sum.Documents, err = store.ListDocuments(ctx, tx, p.ID, ""); err != nil {
			return err
		}
		sum.NoteCount, err = store.CountNotes(ctx, tx, p.ID)
		return err
	})
	return sum, err
}

// orderedReady returns the snapshot's ready tasks in the documented order.
func orderedReady(snap store.Snapshot) []domain.Task {
	featurePos := make(map[int64]int, len(snap.Features))
	for _, f := range snap.Features {
		featurePos[f.ID] = f.Position
	}
	ready := snap.ReadyTasks()
	sort.SliceStable(ready, func(i, j int) bool {
		a, b := ready[i], ready[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if fa, fb := featurePos[a.FeatureID], featurePos[b.FeatureID]; fa != fb {
			return fa < fb
		}
		if a.FeatureID != b.FeatureID {
			return a.FeatureID < b.FeatureID
		}
		if a.Position != b.Position {
			return a.Position < b.Position
		}
		return a.ID < b.ID
	})
	return ready
}

func (s *Service) report(snap store.Snapshot) ProgressReport {
	now := s.now()
	pp := domain.ProjectProgress(snap.FeaturesWithTasks())
	r := ProgressReport{
		Project:    snap.Project,
		Progress:   pp.Progress,
		ReadyCount: len(snap.ReadyTasks()),
	}
	featureCounts := make(map[int64]*TaskCounts, len(snap.Features))
	for _, t := range snap.Tasks {
		r.Counts.add(t.Status)
		c := featureCounts[t.FeatureID]
		if c == nil {
			c = &TaskCounts{}
			featureCounts[t.FeatureID] = c
		}
		c.add(t.Status)
		switch {
		case t.Status == domain.TaskBlocked:
			r.Blocked = append(r.Blocked, BlockedTask{Task: t, Reason: t.BlockedReason})
		case domain.Stale(t, now, domain.StaleThreshold):
			r.Stale = append(r.Stale, t)
		}
	}
	for i, f := range snap.Features {
		fr := FeatureReport{
			Feature:  f,
			Status:   pp.Features[i].Status,
			Progress: pp.Features[i].Progress,
			Counted:  pp.Features[i].Counted,
		}
		if c := featureCounts[f.ID]; c != nil {
			fr.Counts = *c
		}
		r.Features = append(r.Features, fr)
	}
	return r
}
