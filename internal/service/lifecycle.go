package service

import (
	"context"
	"database/sql"
	"slices"
	"strings"

	"skald/internal/domain"
	"skald/internal/store"
)

// StatusChange describes a requested task status change.
type StatusChange struct {
	To              domain.TaskStatus
	BlockedReason   string // required when To is blocked
	Note            string // recorded in the status event
	ExpectedVersion int64  // 0 skips the optimistic-concurrency check
	Force           bool   // human override of the evidence gate; ignored for agents
}

// StartTask moves a todo task to in_progress, assigning it to actor. It fails
// with an InvalidTransitionError listing unmet dependencies when the task is
// not ready.
func (s *Service) StartTask(ctx context.Context, actor domain.Actor, taskID, expectedVersion int64) (domain.Task, error) {
	return s.UpdateTaskStatus(ctx, actor, taskID, StatusChange{To: domain.TaskInProgress, ExpectedVersion: expectedVersion})
}

// UpdateTaskStatus applies any legal status change: block, unblock, cancel,
// reopen, start or complete. The checks and the write share one write
// transaction, so they cannot race with another process.
func (s *Service) UpdateTaskStatus(ctx context.Context, actor domain.Actor, taskID int64, c StatusChange) (domain.Task, error) {
	var out domain.Task
	err := s.db.WriteTx(ctx, func(tx *sql.Tx) (err error) {
		out, err = s.transitionTx(ctx, tx, s.meta(actor), taskID, c)
		return err
	})
	if err != nil {
		return domain.Task{}, err
	}
	return out, nil
}

// transitionTx loads the task, checks the transition and applies it, all
// inside the caller's write transaction.
func (s *Service) transitionTx(ctx context.Context, tx *sql.Tx, m store.Meta, taskID int64, c StatusChange) (domain.Task, error) {
	task, err := store.GetTask(ctx, tx, taskID)
	if err != nil {
		return domain.Task{}, err
	}
	if c.ExpectedVersion > 0 && c.ExpectedVersion != task.Version {
		return domain.Task{}, &domain.ConflictError{Entity: domain.EntityTask, ID: task.ID, Expected: c.ExpectedVersion, Current: task.Version, CurrentTask: &task}
	}
	tc := domain.TransitionContext{TaskID: task.ID, BlockedReason: c.BlockedReason}
	if c.To == domain.TaskInProgress && task.Status == domain.TaskTodo {
		deps, err := store.ListDependencyTasks(ctx, tx, task.ID)
		if err != nil {
			return domain.Task{}, err
		}
		tc.UnmetDeps = domain.UnmetDependencies(deps)
	}
	forced := false
	if c.To == domain.TaskDone {
		project, err := store.GetProject(ctx, tx, task.ProjectID)
		if err != nil {
			return domain.Task{}, err
		}
		n, err := store.CountArtifacts(ctx, tx, task.ID)
		if err != nil {
			return domain.Task{}, err
		}
		tc.RequireEvidence = project.Settings.RequireEvidence
		tc.HasEvidence = n > 0
		// Only humans may override the gate; an agent that asks to force gets
		// the normal EvidenceRequiredError.
		tc.Force = c.Force && m.Actor.IsHuman()
		forced = tc.Force && tc.RequireEvidence && !tc.HasEvidence
	}
	if err := domain.CheckTransition(task.Status, c.To, tc); err != nil {
		return domain.Task{}, err
	}
	return store.SetTaskStatus(ctx, tx, m, store.TaskStatusChange{
		TaskID:          task.ID,
		To:              c.To,
		ExpectedVersion: c.ExpectedVersion,
		BlockedReason:   c.BlockedReason,
		Note:            c.Note,
		Forced:          forced,
	})
}

// AddTaskNote records a free-text progress note and returns its event ID.
func (s *Service) AddTaskNote(ctx context.Context, actor domain.Actor, taskID int64, text string) (int64, error) {
	var id int64
	err := s.db.WriteTx(ctx, func(tx *sql.Tx) (err error) {
		id, err = store.AddTaskNote(ctx, tx, s.meta(actor), taskID, text)
		return err
	})
	return id, err
}

// AddDependency makes taskID depend on dependsOnID. Both must be in one
// project, and a cycle is rejected with the cycle path. It reports false when
// the dependency already existed.
func (s *Service) AddDependency(ctx context.Context, actor domain.Actor, taskID, dependsOnID int64) (bool, error) {
	var added bool
	err := s.db.WriteTx(ctx, func(tx *sql.Tx) (err error) {
		added, err = store.AddDependency(ctx, tx, s.meta(actor), taskID, dependsOnID)
		return err
	})
	return added, err
}

// RemoveDependency deletes a dependency. It reports false when there was none.
func (s *Service) RemoveDependency(ctx context.Context, actor domain.Actor, taskID, dependsOnID int64) (bool, error) {
	var removed bool
	err := s.db.WriteTx(ctx, func(tx *sql.Tx) (err error) {
		removed, err = store.RemoveDependency(ctx, tx, s.meta(actor), taskID, dependsOnID)
		return err
	})
	return removed, err
}

// TaskEdit lists the fields of a task to change. Nil fields are left alone; an
// empty description or criteria string clears the field.
type TaskEdit struct {
	Title              *string
	Description        *string
	AcceptanceCriteria *string
	Priority           *int
	Weight             *int
	// Tags replaces the task's tags when non-nil (an empty slice clears
	// them). AddTags and RemoveTags are applied after it, so callers can
	// change a few tags without reading the current set first.
	Tags            *[]string
	AddTags         []string
	RemoveTags      []string
	ExpectedVersion int64 // 0 skips the optimistic-concurrency check
}

// UpdateTask edits a task's title, description, acceptance criteria, priority,
// weight or tags. It works in any status and records one task.updated event naming
// the fields that changed. An edit that changes nothing writes nothing.
func (s *Service) UpdateTask(ctx context.Context, actor domain.Actor, taskID int64, e TaskEdit) (domain.Task, error) {
	return s.updateTask(ctx, actor, 0, taskID, e)
}

// TagTask adds and removes tags on a task of the resolved project. A task
// from another project is NotFound. Removing a tag the task does not carry is
// not an error, and a change that leaves the tags as they were writes nothing.
func (s *Service) TagTask(ctx context.Context, actor domain.Actor, ref ProjectRef, taskID int64, add, remove []string) (domain.Task, error) {
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return domain.Task{}, err
	}
	return s.updateTask(ctx, actor, p.ID, taskID, TaskEdit{AddTags: add, RemoveTags: remove})
}

// updateTask is UpdateTask limited to one project; projectID 0 allows any.
func (s *Service) updateTask(ctx context.Context, actor domain.Actor, projectID, taskID int64, e TaskEdit) (domain.Task, error) {
	var out domain.Task
	err := s.db.WriteTx(ctx, func(tx *sql.Tx) error {
		cur, err := store.GetTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if projectID != 0 && cur.ProjectID != projectID {
			return &domain.NotFoundError{Entity: domain.EntityTask, ID: taskID}
		}
		p := store.TaskPatch{}
		differs := func(old string, v *string) *string {
			if v == nil || *v == old {
				return nil
			}
			return v
		}
		p.Title = differs(cur.Title, e.Title)
		p.Description = differs(cur.Description, e.Description)
		p.AcceptanceCriteria = differs(cur.AcceptanceCriteria, e.AcceptanceCriteria)
		if e.Priority != nil && *e.Priority != cur.Priority {
			p.Priority = e.Priority
		}
		if e.Weight != nil && *e.Weight != cur.Weight {
			p.Weight = e.Weight
		}
		if e.Tags != nil || len(e.AddTags) > 0 || len(e.RemoveTags) > 0 {
			tags, err := editTags(cur.Tags, e)
			if err != nil {
				return err
			}
			if !slices.Equal(tags, cur.Tags) {
				p.Tags = &tags
			}
		}
		out, err = store.UpdateTaskFields(ctx, tx, s.meta(actor), taskID, e.ExpectedVersion, p)
		return err
	})
	if err != nil {
		return domain.Task{}, err
	}
	return out, nil
}

// editTags applies a TaskEdit's tag changes to the current tags.
func editTags(cur []string, e TaskEdit) ([]string, error) {
	base := cur
	if e.Tags != nil {
		base = *e.Tags
	}
	remove, err := domain.NormalizeTags(e.RemoveTags)
	if err != nil {
		return nil, err
	}
	var next []string
	for _, t := range append(slices.Clone(base), e.AddTags...) {
		n, err := domain.NormalizeTag(t)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(remove, n) {
			next = append(next, n)
		}
	}
	return domain.NormalizeTags(next)
}

// TagCount is a tag and the number of tasks that carry it.
type TagCount struct {
	Tag   string
	Tasks int
}

// ListTags returns the tags used in the resolved project, most used first,
// then by name.
func (s *Service) ListTags(ctx context.Context, actor domain.Actor, ref ProjectRef) ([]TagCount, error) {
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	var counts map[string]int
	err = s.db.ReadTx(ctx, func(tx *sql.Tx) (err error) {
		counts, err = store.ProjectTagCounts(ctx, tx, p.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]TagCount, 0, len(counts))
	for t, n := range counts {
		out = append(out, TagCount{Tag: t, Tasks: n})
	}
	slices.SortFunc(out, func(a, b TagCount) int {
		if a.Tasks != b.Tasks {
			return b.Tasks - a.Tasks
		}
		return strings.Compare(a.Tag, b.Tag)
	})
	return out, nil
}
