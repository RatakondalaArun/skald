package service

import (
	"context"
	"database/sql"

	"skald/internal/domain"
	"skald/internal/store"
)

// ArtifactInput is one piece of verification evidence.
type ArtifactInput struct {
	Kind    domain.ArtifactKind
	Ref     string
	Summary string
}

// CompleteOptions tunes CompleteTask.
type CompleteOptions struct {
	ExpectedVersion int64
	Note            string
	Force           bool // human override of the evidence gate; ignored for agents
}

// TaskResult is the input of ReportTaskResult.
type TaskResult struct {
	Summary         string // recorded as a note when non-empty
	Artifacts       []ArtifactInput
	ExpectedVersion int64
	Force           bool
}

// RecordArtifact attaches verification evidence to a task. It is refused on
// cancelled tasks; evidence may still be added to done tasks.
func (s *Service) RecordArtifact(ctx context.Context, actor domain.Actor, taskID int64, in ArtifactInput) (domain.Artifact, error) {
	var out domain.Artifact
	err := s.db.WriteTx(ctx, func(tx *sql.Tx) (err error) {
		out, err = s.recordArtifactTx(ctx, tx, s.meta(actor), taskID, in)
		return err
	})
	if err != nil {
		return domain.Artifact{}, err
	}
	return out, nil
}

func (s *Service) recordArtifactTx(ctx context.Context, tx *sql.Tx, m store.Meta, taskID int64, in ArtifactInput) (domain.Artifact, error) {
	task, err := store.GetTask(ctx, tx, taskID)
	if err != nil {
		return domain.Artifact{}, err
	}
	if task.Status == domain.TaskCancelled {
		return domain.Artifact{}, &domain.ValidationError{
			Field:   "task",
			Problem: domain.FormatID(domain.EntityTask, task.ID) + " is cancelled and cannot take evidence",
			Hint:    "Reopen the task first if the work is still relevant.",
		}
	}
	return store.AddArtifact(ctx, tx, m, domain.Artifact{TaskID: taskID, Kind: in.Kind, Ref: in.Ref, Summary: in.Summary})
}

// CompleteTask marks an in-progress task done. When the project requires
// evidence the task needs at least one artifact. Force overrides that gate
// for human actors only and is recorded in the event.
func (s *Service) CompleteTask(ctx context.Context, actor domain.Actor, taskID int64, o CompleteOptions) (domain.Task, error) {
	return s.UpdateTaskStatus(ctx, actor, taskID, StatusChange{
		To: domain.TaskDone, Note: o.Note, ExpectedVersion: o.ExpectedVersion, Force: o.Force,
	})
}

// ReportTaskResult records a summary note, attaches the artifacts and
// completes the task in one transaction. If any step fails nothing is
// written.
func (s *Service) ReportTaskResult(ctx context.Context, actor domain.Actor, taskID int64, r TaskResult) (domain.Task, []domain.Artifact, error) {
	var (
		task      domain.Task
		artifacts []domain.Artifact
	)
	err := s.db.WriteTx(ctx, func(tx *sql.Tx) error {
		m := s.meta(actor)
		// Check the version before writing anything, so a stale caller gets a
		// conflict rather than half a report.
		cur, err := store.GetTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if r.ExpectedVersion > 0 && r.ExpectedVersion != cur.Version {
			return &domain.ConflictError{Entity: domain.EntityTask, ID: cur.ID, Expected: r.ExpectedVersion, Current: cur.Version, CurrentTask: &cur}
		}
		if r.Summary != "" {
			if _, err := store.AddTaskNote(ctx, tx, m, taskID, r.Summary); err != nil {
				return err
			}
		}
		for _, in := range r.Artifacts {
			a, err := s.recordArtifactTx(ctx, tx, m, taskID, in)
			if err != nil {
				return err
			}
			artifacts = append(artifacts, a)
		}
		task, err = s.transitionTx(ctx, tx, m, taskID, StatusChange{
			To: domain.TaskDone, ExpectedVersion: r.ExpectedVersion, Force: r.Force,
		})
		return err
	})
	if err != nil {
		return domain.Task{}, nil, err
	}
	return task, artifacts, nil
}
