package service

import (
	"context"
	"database/sql"

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
	ExpectedVersion    int64 // 0 skips the optimistic-concurrency check
}

// UpdateTask edits a task's title, description, acceptance criteria, priority
// or weight. It works in any status and records one task.updated event naming
// the fields that changed. An edit that changes nothing writes nothing.
func (s *Service) UpdateTask(ctx context.Context, actor domain.Actor, taskID int64, e TaskEdit) (domain.Task, error) {
	var out domain.Task
	err := s.db.WriteTx(ctx, func(tx *sql.Tx) error {
		cur, err := store.GetTask(ctx, tx, taskID)
		if err != nil {
			return err
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
		out, err = store.UpdateTaskFields(ctx, tx, s.meta(actor), taskID, e.ExpectedVersion, p)
		return err
	})
	if err != nil {
		return domain.Task{}, err
	}
	return out, nil
}
