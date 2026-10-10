package mcpserver

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"skald/internal/domain"
	"skald/internal/service"
)

type startArgs struct {
	projectArg
	Task            string `json:"task" jsonschema:"task ID such as T-42"`
	ExpectedVersion int64  `json:"expected_version,omitempty" jsonschema:"fail if the task changed since you read this version"`
}

type statusArgs struct {
	projectArg
	Task            string `json:"task" jsonschema:"task ID such as T-42"`
	Status          string `json:"status" jsonschema:"todo (reopen), in_progress (unblock), blocked, done or cancelled"`
	BlockedReason   string `json:"blocked_reason,omitempty" jsonschema:"required when status is blocked: what is in the way"`
	Note            string `json:"note,omitempty"`
	ExpectedVersion int64  `json:"expected_version,omitempty" jsonschema:"fail if the task changed since you read this version"`
}

type noteArgs struct {
	projectArg
	Task string `json:"task" jsonschema:"task ID such as T-42"`
	Text string `json:"text" jsonschema:"progress, finding or decision"`
}

type evidenceArgs struct {
	projectArg
	Task    string `json:"task" jsonschema:"task ID such as T-42"`
	Kind    string `json:"kind" jsonschema:"commit, pr, test_report, link or manual"`
	Ref     string `json:"ref" jsonschema:"commit SHA, PR URL, report path or link"`
	Summary string `json:"summary,omitempty" jsonschema:"what this proves"`
}

type resultArgs struct {
	projectArg
	Task            string         `json:"task" jsonschema:"task ID such as T-42"`
	Summary         string         `json:"summary,omitempty" jsonschema:"what was done"`
	Evidence        []evidenceItem `json:"evidence,omitempty" jsonschema:"verification evidence; required unless already recorded"`
	ExpectedVersion int64          `json:"expected_version,omitempty" jsonschema:"fail if the task changed since you read this version"`
}

type evidenceItem struct {
	Kind    string `json:"kind" jsonschema:"commit, pr, test_report, link or manual"`
	Ref     string `json:"ref"`
	Summary string `json:"summary,omitempty"`
}

func (s *server) registerWork(srv *mcp.Server) {
	addTool(srv, s, "start_task",
		"Claim a ready task (todo to in_progress). Fails with the unfinished prerequisites if not ready. Next: add_task_note while working, then report_task_result.",
		false, func(ctx context.Context, actor domain.Actor, in startArgs) (any, error) {
			id, err := taskID(in.Task)
			if err != nil {
				return nil, err
			}
			t, err := s.svc.StartTask(ctx, actor, id, in.ExpectedVersion)
			if err != nil {
				return nil, err
			}
			return s.withProject(ctx, in.Project, obj{"task": line(t)}), nil
		})

	addTool(srv, s, "update_task_status",
		"Other status changes: blocked (needs blocked_reason), in_progress (unblock), todo (reopen), cancelled, done. To finish a task prefer report_task_result; done needs evidence. Next: get_next_tasks.",
		false, func(ctx context.Context, actor domain.Actor, in statusArgs) (any, error) {
			id, err := taskID(in.Task)
			if err != nil {
				return nil, err
			}
			to := domain.TaskStatus(strings.ToLower(strings.TrimSpace(in.Status)))
			if !to.Valid() {
				return nil, &domain.ValidationError{Field: "status", Problem: "unknown status " + in.Status, Hint: "Use todo, in_progress, blocked, done or cancelled."}
			}
			t, err := s.svc.UpdateTaskStatus(ctx, actor, id, service.StatusChange{
				To: to, BlockedReason: in.BlockedReason, Note: in.Note, ExpectedVersion: in.ExpectedVersion,
			})
			if err != nil {
				return nil, err
			}
			return s.withProject(ctx, in.Project, obj{"task": line(t)}), nil
		})

	addTool(srv, s, "add_task_note",
		"Record progress, a finding or a decision on a task. Next: record_verification when you have proof.",
		false, func(ctx context.Context, actor domain.Actor, in noteArgs) (any, error) {
			id, err := taskID(in.Task)
			if err != nil {
				return nil, err
			}
			if _, err := s.svc.AddTaskNote(ctx, actor, id, in.Text); err != nil {
				return nil, err
			}
			return s.withProject(ctx, in.Project, obj{"task": tid(id), "noted": true}), nil
		})

	addTool(srv, s, "record_verification",
		"Attach evidence (commit, pr, test_report, link, manual) to a task. Next: report_task_result.",
		false, func(ctx context.Context, actor domain.Actor, in evidenceArgs) (any, error) {
			id, err := taskID(in.Task)
			if err != nil {
				return nil, err
			}
			a, err := s.svc.RecordArtifact(ctx, actor, id, service.ArtifactInput{
				Kind: domain.ArtifactKind(strings.ToLower(strings.TrimSpace(in.Kind))), Ref: in.Ref, Summary: in.Summary,
			})
			if err != nil {
				return nil, err
			}
			return s.withProject(ctx, in.Project, obj{"task": tid(id), "evidence": artifacts([]domain.Artifact{a})[0]}), nil
		})

	addTool(srv, s, "report_task_result",
		"Finish a task: note + evidence + done in one step. If anything fails (for example no evidence) nothing is written. Next: get_next_tasks.",
		false, func(ctx context.Context, actor domain.Actor, in resultArgs) (any, error) {
			id, err := taskID(in.Task)
			if err != nil {
				return nil, err
			}
			r := service.TaskResult{Summary: in.Summary, ExpectedVersion: in.ExpectedVersion}
			for _, e := range in.Evidence {
				r.Artifacts = append(r.Artifacts, service.ArtifactInput{
					Kind: domain.ArtifactKind(strings.ToLower(strings.TrimSpace(e.Kind))), Ref: e.Ref, Summary: e.Summary,
				})
			}
			t, arts, err := s.svc.ReportTaskResult(ctx, actor, id, r)
			if err != nil {
				return nil, err
			}
			return s.withProject(ctx, in.Project, obj{"task": line(t), "evidence_added": artifacts(arts)}), nil
		})
}
