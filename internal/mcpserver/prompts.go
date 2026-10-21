package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const workflowText = `Use Skald to plan and track this project.

1. get_project to see what exists. If there is no project, create_project. If it has notes, list_notes to catch up on what earlier sessions left in the scratchpad.
2. Plan: create_plan with features, tasks, depends_on refs and an idempotency_key on every task. Keep tasks small enough to verify. Put requirements and decisions in put_document.
3. Work loop: get_next_tasks, start_task on the first, then implement.
4. While working: add_task_note for progress, findings and decisions. If you are stuck, update_task_status to blocked with blocked_reason. Use add_note for anything worth remembering that is not about one task's progress: gotchas, open questions, ideas, where you left off.
5. Prove it: record_verification for each piece of evidence (commit SHA, test report, PR link). Finish with report_task_result (summary, plus evidence if not recorded yet). A task cannot be done without evidence, and progress is computed from task state, never set by you.
6. Repeat from step 3 until get_next_tasks is empty, then check get_project_progress.`

func (s *server) registerPrompts(srv *mcp.Server) {
	srv.AddPrompt(&mcp.Prompt{
		Name:        "skald_workflow",
		Description: "How to plan and track work in Skald: plan, pick the next task, start, note, verify, report.",
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{
			Description: "Skald workflow",
			Messages:    []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: workflowText}}},
		}, nil
	})
}
