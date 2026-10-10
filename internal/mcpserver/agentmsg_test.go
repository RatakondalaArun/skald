package mcpserver

import (
	"strings"
	"testing"

	"skald/internal/domain"
)

func TestAgentMessageEvidenceHidesHumanOverride(t *testing.T) {
	msg := agentMessage(&domain.EvidenceRequiredError{TaskID: 6})
	for _, want := range []string{"T-6", "record_verification", "report_task_result"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "--force") {
		t.Errorf("agent message must not mention force: %q", msg)
	}
}

func TestAgentMessageGatedStartNamesNextTool(t *testing.T) {
	msg := agentMessage(&domain.InvalidTransitionError{TaskID: 8, From: domain.TaskTodo, To: domain.TaskInProgress, UnmetDeps: []int64{7}})
	if !strings.Contains(msg, "get_next_tasks") || !strings.Contains(msg, "T-7") {
		t.Errorf("unexpected message %q", msg)
	}
	plain := agentMessage(&domain.InvalidTransitionError{TaskID: 8, From: domain.TaskTodo, To: domain.TaskDone})
	if strings.Contains(plain, "get_next_tasks") {
		t.Errorf("hint only applies to dependency-gated starts: %q", plain)
	}
}
