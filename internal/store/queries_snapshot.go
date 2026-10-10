package store

import (
	"context"
	"database/sql"

	"skald/internal/domain"
)

// Snapshot is everything needed to compute a project's progress and readiness.
type Snapshot struct {
	Project  domain.Project
	Features []domain.Feature
	Tasks    []domain.Task
	Deps     []domain.Dependency
}

// LoadSnapshot loads a project with its features, tasks and dependencies. For
// a consistent view, call it with a *sql.Tx (see DB.ReadSnapshot).
func LoadSnapshot(ctx context.Context, q Querier, projectID int64) (Snapshot, error) {
	var s Snapshot
	var err error
	if s.Project, err = GetProject(ctx, q, projectID); err != nil {
		return Snapshot{}, err
	}
	if s.Features, err = ListFeatures(ctx, q, projectID); err != nil {
		return Snapshot{}, err
	}
	if s.Tasks, err = ListTasks(ctx, q, TaskFilter{ProjectID: projectID}); err != nil {
		return Snapshot{}, err
	}
	if s.Deps, err = ListProjectDependencies(ctx, q, projectID); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

// ReadSnapshot runs LoadSnapshot in a read transaction so the four queries see
// one consistent state even while other processes write.
func (d *DB) ReadSnapshot(ctx context.Context, projectID int64) (s Snapshot, err error) {
	err = d.ReadTx(ctx, func(tx *sql.Tx) error {
		s, err = LoadSnapshot(ctx, tx, projectID)
		return err
	})
	return s, err
}

// FeaturesWithTasks groups the tasks under their features, in feature order,
// ready for domain.ProjectProgress.
func (s Snapshot) FeaturesWithTasks() []domain.FeatureWithTasks {
	byFeature := make(map[int64][]domain.Task, len(s.Features))
	for _, t := range s.Tasks {
		byFeature[t.FeatureID] = append(byFeature[t.FeatureID], t)
	}
	out := make([]domain.FeatureWithTasks, len(s.Features))
	for i, f := range s.Features {
		out[i] = domain.FeatureWithTasks{Feature: f, Tasks: byFeature[f.ID]}
	}
	return out
}

// DepsByTask maps each task to the IDs it directly depends on.
func (s Snapshot) DepsByTask() map[int64][]int64 {
	m := make(map[int64][]int64)
	for _, d := range s.Deps {
		m[d.TaskID] = append(m[d.TaskID], d.DependsOnID)
	}
	return m
}

// ReadyTasks returns the todo tasks whose dependencies are all done or
// cancelled, in snapshot order.
func (s Snapshot) ReadyTasks() []domain.Task {
	byID := make(map[int64]domain.Task, len(s.Tasks))
	for _, t := range s.Tasks {
		byID[t.ID] = t
	}
	deps := s.DepsByTask()
	var out []domain.Task
	for _, t := range s.Tasks {
		var dts []domain.Task
		for _, id := range deps[t.ID] {
			dts = append(dts, byID[id])
		}
		if domain.Ready(t, dts) {
			out = append(out, t)
		}
	}
	return out
}
