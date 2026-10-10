package domain

import "time"

// StaleThreshold is how long an in-progress task may go untouched before it
// is reported as stale.
const StaleThreshold = 4 * time.Hour

// transitions is the table from ARCHITECTURE 4.1. Order is the order shown in
// error messages.
var transitions = map[TaskStatus][]TaskStatus{
	TaskTodo:       {TaskInProgress, TaskBlocked, TaskCancelled},
	TaskInProgress: {TaskBlocked, TaskDone, TaskCancelled},
	TaskBlocked:    {TaskInProgress, TaskTodo, TaskCancelled},
	TaskDone:       {TaskTodo},
	TaskCancelled:  {TaskTodo},
}

// AllowedTransitions lists the statuses a task in status from may move to.
func AllowedTransitions(from TaskStatus) []TaskStatus {
	return append([]TaskStatus(nil), transitions[from]...)
}

// TransitionContext carries the facts the service has gathered for a status
// change. The domain function stays pure.
type TransitionContext struct {
	TaskID          int64
	BlockedReason   string  // required when moving to blocked
	UnmetDeps       []int64 // dependencies that are not done or cancelled
	HasEvidence     bool    // task has at least one artifact
	RequireEvidence bool    // project setting require_evidence
	Force           bool    // human override of the evidence gate only
}

// CheckTransition decides whether a task may move from one status to another.
// It returns a typed domain error (InvalidTransitionError, ValidationError or
// EvidenceRequiredError) or nil. Force bypasses only the evidence gate, never
// the transition table or dependency check.
func CheckTransition(from, to TaskStatus, c TransitionContext) error {
	if !from.Valid() {
		return &ValidationError{Field: "status", Problem: "unknown status " + quote(string(from))}
	}
	if !to.Valid() {
		return &ValidationError{Field: "status", Problem: "unknown status " + quote(string(to)), Hint: "Use one of: todo, in_progress, blocked, done, cancelled."}
	}
	legal := false
	for _, s := range transitions[from] {
		if s == to {
			legal = true
			break
		}
	}
	if !legal {
		reason := ""
		if from == to {
			reason = "It is already " + string(from)
		}
		return &InvalidTransitionError{TaskID: c.TaskID, From: from, To: to, Reason: reason}
	}
	switch {
	case from == TaskTodo && to == TaskInProgress && len(c.UnmetDeps) > 0:
		return &InvalidTransitionError{TaskID: c.TaskID, From: from, To: to, UnmetDeps: append([]int64(nil), c.UnmetDeps...)}
	case to == TaskBlocked && c.BlockedReason == "":
		return &ValidationError{
			Field:   "blocked_reason",
			Problem: "a reason is required when blocking a task",
			Hint:    "Say what is blocking it, such as the missing decision or external dependency.",
		}
	case to == TaskDone && c.RequireEvidence && !c.HasEvidence && !c.Force:
		return &EvidenceRequiredError{TaskID: c.TaskID}
	}
	return nil
}

// UnmetDependencies returns the IDs of deps that are not done or cancelled.
func UnmetDependencies(deps []Task) []int64 {
	var out []int64
	for _, d := range deps {
		if !d.Status.Terminal() {
			out = append(out, d.ID)
		}
	}
	return out
}

// Ready reports whether t is todo and all of its dependencies are done or
// cancelled. deps are the tasks t depends on.
func Ready(t Task, deps []Task) bool {
	return t.Status == TaskTodo && len(UnmetDependencies(deps)) == 0
}

// Stale reports whether t is in progress and has not been updated for at
// least threshold (StaleThreshold when threshold <= 0).
func Stale(t Task, now time.Time, threshold time.Duration) bool {
	if threshold <= 0 {
		threshold = StaleThreshold
	}
	return t.Status == TaskInProgress && now.Sub(t.UpdatedAt) >= threshold
}

// DeriveFeatureStatus computes a feature's status from its tasks (4.2). A
// stored cancelled status wins. Otherwise: no non-cancelled tasks or only todo
// tasks is planned; all non-cancelled tasks done is done; anything else is
// active.
func DeriveFeatureStatus(stored FeatureStatus, tasks []Task) FeatureStatus {
	if stored == FeatureCancelled {
		return FeatureCancelled
	}
	live, done, started := 0, 0, 0
	for _, t := range tasks {
		if t.Status == TaskCancelled {
			continue
		}
		live++
		if t.Status == TaskDone {
			done++
		}
		if t.Status != TaskTodo {
			started++
		}
	}
	switch {
	case live == 0:
		return FeaturePlanned
	case done == live:
		return FeatureDone
	case started > 0:
		return FeatureActive
	}
	return FeaturePlanned
}

func quote(s string) string { return `"` + s + `"` }
