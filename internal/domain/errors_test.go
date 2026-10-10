package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrorKindsAndIs(t *testing.T) {
	tests := []struct {
		err      error
		kind     ErrorKind
		sentinel error
	}{
		{&NotFoundError{Entity: EntityTask, ID: 42}, KindNotFound, ErrNotFound},
		{&InvalidTransitionError{TaskID: 1, From: TaskDone, To: TaskBlocked}, KindInvalidTransition, ErrInvalidTransition},
		{&ConflictError{Entity: EntityTask, ID: 1, Expected: 2, Current: 3}, KindConflict, ErrConflict},
		{&DependencyCycleError{Path: []int64{1, 2, 1}}, KindDependencyCycle, ErrDependencyCycle},
		{&EvidenceRequiredError{TaskID: 1}, KindEvidenceRequired, ErrEvidenceRequired},
		{&ValidationError{Field: "title", Problem: "is empty"}, KindValidation, ErrValidation},
	}
	for _, tt := range tests {
		t.Run(tt.kind.String(), func(t *testing.T) {
			wrapped := fmt.Errorf("context: %w", tt.err)
			if KindOf(wrapped) != tt.kind {
				t.Errorf("KindOf = %v, want %v", KindOf(wrapped), tt.kind)
			}
			if !errors.Is(wrapped, tt.sentinel) {
				t.Error("errors.Is sentinel failed")
			}
			for _, other := range tests {
				if other.kind != tt.kind && errors.Is(wrapped, other.sentinel) {
					t.Errorf("matched wrong sentinel %v", other.kind)
				}
			}
			var de Error
			if !errors.As(wrapped, &de) || de.Message() != tt.err.Error() {
				t.Error("errors.As to Error failed or Message != Error()")
			}
			if tt.err.Error() == "" {
				t.Error("empty message")
			}
		})
	}
	if KindOf(errors.New("plain")) != KindUnknown || KindOf(nil) != KindUnknown {
		t.Error("non-domain error should be KindUnknown")
	}
	if KindUnknown.String() != "unknown" {
		t.Error("KindUnknown string")
	}
}

func TestErrorMessagesNameNextAction(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want []string
	}{
		{"not found id", &NotFoundError{Entity: EntityTask, ID: 42}, []string{"T-42", "List the tasks"}},
		{"not found ref", &NotFoundError{Entity: EntityProject, Ref: "school"}, []string{`"school"`, "List the projects"}},
		{"not found bare", &NotFoundError{}, []string{"List the items"}},
		{"transition lists allowed", &InvalidTransitionError{TaskID: 5, From: TaskDone, To: TaskBlocked}, []string{"T-5", "done to blocked", "only go to: todo"}},
		{"transition same", &InvalidTransitionError{From: TaskTodo, To: TaskTodo, Reason: "It is already todo"}, []string{"already todo", "in_progress, blocked, cancelled"}},
		{"transition unmet", &InvalidTransitionError{TaskID: 9, From: TaskTodo, To: TaskInProgress, UnmetDeps: []int64{3, 5}}, []string{"T-9", "T-3, T-5", "Finish or cancel"}},
		{"conflict", &ConflictError{Entity: EntityTask, ID: 4, Expected: 3, Current: 5}, []string{"T-4", "version 3", "current is 5", "Re-read", "expected_version=5"}},
		{"conflict with task", &ConflictError{Entity: EntityTask, ID: 4, Expected: 3, Current: 5, CurrentTask: &Task{Status: TaskBlocked}}, []string{"T-4", "version 3", "now blocked at version 5", "expected_version=5"}},
		{"cycle", &DependencyCycleError{Path: []int64{1, 2, 3, 1}}, []string{"T-1 -> T-2 -> T-3 -> T-1", "Remove one dependency"}},
		{"cycle no path", &DependencyCycleError{}, []string{"circular dependency", "Remove one dependency"}},
		{"evidence", &EvidenceRequiredError{TaskID: 8}, []string{"T-8", "Record an artifact", "force"}},
		{"validation", &ValidationError{Field: "weight", Problem: "must be between 1 and 10", Hint: "Pick a smaller number."}, []string{"Invalid weight: must be between 1 and 10.", "Pick a smaller number."}},
		{"validation no field", &ValidationError{Problem: "nothing to update"}, []string{"nothing to update."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := tt.err.Error()
			for _, w := range tt.want {
				if !strings.Contains(msg, w) {
					t.Errorf("message %q lacks %q", msg, w)
				}
			}
		})
	}
}

func TestCheckTransitionMessages(t *testing.T) {
	err := CheckTransition(TaskDone, TaskBlocked, TransitionContext{TaskID: 2, BlockedReason: "x"})
	if !strings.Contains(err.Error(), "T-2") || !strings.Contains(err.Error(), "todo") {
		t.Errorf("unhelpful message: %v", err)
	}
	err = CheckTransition(TaskTodo, TaskBlocked, TransitionContext{})
	if !strings.Contains(err.Error(), "reason") {
		t.Errorf("unhelpful message: %v", err)
	}
}

func TestKindStrings(t *testing.T) {
	seen := map[string]bool{}
	for k := KindUnknown; k <= KindValidation; k++ {
		s := k.String()
		if seen[s] {
			t.Errorf("duplicate kind string %q", s)
		}
		seen[s] = true
	}
}
