package domain

import (
	"errors"
	"testing"
	"time"
)

var allStatuses = []TaskStatus{TaskTodo, TaskInProgress, TaskBlocked, TaskDone, TaskCancelled}

func TestCheckTransitionTable(t *testing.T) {
	legal := map[[2]TaskStatus]bool{
		{TaskTodo, TaskInProgress}:      true,
		{TaskTodo, TaskBlocked}:         true,
		{TaskTodo, TaskCancelled}:       true,
		{TaskInProgress, TaskBlocked}:   true,
		{TaskInProgress, TaskDone}:      true,
		{TaskInProgress, TaskCancelled}: true,
		{TaskBlocked, TaskInProgress}:   true,
		{TaskBlocked, TaskTodo}:         true,
		{TaskBlocked, TaskCancelled}:    true,
		{TaskDone, TaskTodo}:            true,
		{TaskCancelled, TaskTodo}:       true,
	}
	// Context that satisfies every gate, so only the table decides.
	ok := TransitionContext{TaskID: 7, BlockedReason: "waiting", HasEvidence: true, RequireEvidence: true}
	for _, from := range allStatuses {
		for _, to := range allStatuses {
			err := CheckTransition(from, to, ok)
			if legal[[2]TaskStatus{from, to}] {
				if err != nil {
					t.Errorf("%s -> %s should be legal, got %v", from, to, err)
				}
				continue
			}
			var it *InvalidTransitionError
			if !errors.As(err, &it) {
				t.Errorf("%s -> %s should be an InvalidTransitionError, got %v", from, to, err)
				continue
			}
			if it.From != from || it.To != to || it.TaskID != 7 {
				t.Errorf("%s -> %s: wrong fields %+v", from, to, it)
			}
		}
	}
}

func TestCheckTransitionGates(t *testing.T) {
	tests := []struct {
		name     string
		from, to TaskStatus
		ctx      TransitionContext
		want     ErrorKind
		unmet    []int64
	}{
		{"block needs reason", TaskTodo, TaskBlocked, TransitionContext{}, KindValidation, nil},
		{"block from in_progress needs reason", TaskInProgress, TaskBlocked, TransitionContext{}, KindValidation, nil},
		{"block with reason", TaskInProgress, TaskBlocked, TransitionContext{BlockedReason: "x"}, KindUnknown, nil},
		{"start with unmet deps", TaskTodo, TaskInProgress, TransitionContext{TaskID: 3, UnmetDeps: []int64{1, 2}}, KindInvalidTransition, []int64{1, 2}},
		{"start with no unmet deps", TaskTodo, TaskInProgress, TransitionContext{}, KindUnknown, nil},
		{"unblock ignores deps", TaskBlocked, TaskInProgress, TransitionContext{UnmetDeps: []int64{1}}, KindUnknown, nil},
		{"done needs evidence", TaskInProgress, TaskDone, TransitionContext{RequireEvidence: true}, KindEvidenceRequired, nil},
		{"done with evidence", TaskInProgress, TaskDone, TransitionContext{RequireEvidence: true, HasEvidence: true}, KindUnknown, nil},
		{"done forced", TaskInProgress, TaskDone, TransitionContext{RequireEvidence: true, Force: true}, KindUnknown, nil},
		{"done when evidence not required", TaskInProgress, TaskDone, TransitionContext{}, KindUnknown, nil},
		{"force does not bypass table", TaskTodo, TaskDone, TransitionContext{Force: true}, KindInvalidTransition, nil},
		{"force does not bypass deps", TaskTodo, TaskInProgress, TransitionContext{Force: true, UnmetDeps: []int64{9}}, KindInvalidTransition, []int64{9}},
		{"same status", TaskTodo, TaskTodo, TransitionContext{}, KindInvalidTransition, nil},
		{"unknown target", TaskTodo, TaskStatus("weird"), TransitionContext{}, KindValidation, nil},
		{"unknown source", TaskStatus("weird"), TaskTodo, TransitionContext{}, KindValidation, nil},
		{"reopen done", TaskDone, TaskTodo, TransitionContext{}, KindUnknown, nil},
		{"reopen cancelled", TaskCancelled, TaskTodo, TransitionContext{}, KindUnknown, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckTransition(tt.from, tt.to, tt.ctx)
			if got := KindOf(err); got != tt.want {
				t.Fatalf("kind = %v, want %v (err: %v)", got, tt.want, err)
			}
			if tt.unmet != nil {
				var it *InvalidTransitionError
				if !errors.As(err, &it) || len(it.UnmetDeps) != len(tt.unmet) {
					t.Fatalf("expected unmet deps %v, got %v", tt.unmet, err)
				}
			}
		})
	}
}

func TestCheckTransitionCopiesUnmetDeps(t *testing.T) {
	deps := []int64{1, 2}
	err := CheckTransition(TaskTodo, TaskInProgress, TransitionContext{UnmetDeps: deps})
	deps[0] = 99
	var it *InvalidTransitionError
	if !errors.As(err, &it) || it.UnmetDeps[0] != 1 {
		t.Fatalf("error should not alias caller slice: %v", err)
	}
}

func TestAllowedTransitionsReturnsCopy(t *testing.T) {
	a := AllowedTransitions(TaskTodo)
	a[0] = TaskDone
	if AllowedTransitions(TaskTodo)[0] != TaskInProgress {
		t.Fatal("AllowedTransitions leaked internal slice")
	}
	if AllowedTransitions(TaskStatus("nope")) != nil && len(AllowedTransitions(TaskStatus("nope"))) != 0 {
		t.Fatal("unknown status should have no transitions")
	}
}

func TestReadyDiamond(t *testing.T) {
	// A <- B, A <- C, B <- D, C <- D  (D depends on B and C, which depend on A).
	mk := func(id int64, s TaskStatus) Task { return Task{ID: id, Status: s} }
	tests := []struct {
		name string
		self Task
		deps []Task
		want bool
	}{
		{"root todo no deps", mk(1, TaskTodo), nil, true},
		{"root in_progress not ready", mk(1, TaskInProgress), nil, false},
		{"B ready when A done", mk(2, TaskTodo), []Task{mk(1, TaskDone)}, true},
		{"B not ready when A in progress", mk(2, TaskTodo), []Task{mk(1, TaskInProgress)}, false},
		{"D not ready when only B done", mk(4, TaskTodo), []Task{mk(2, TaskDone), mk(3, TaskTodo)}, false},
		{"D not ready when C blocked", mk(4, TaskTodo), []Task{mk(2, TaskDone), mk(3, TaskBlocked)}, false},
		{"D ready when B done and C cancelled", mk(4, TaskTodo), []Task{mk(2, TaskDone), mk(3, TaskCancelled)}, true},
		{"D ready when both done", mk(4, TaskTodo), []Task{mk(2, TaskDone), mk(3, TaskDone)}, true},
		{"done task not ready", mk(4, TaskDone), []Task{mk(2, TaskDone)}, false},
		{"blocked task not ready", mk(4, TaskBlocked), nil, false},
	}
	for _, tt := range tests {
		if got := Ready(tt.self, tt.deps); got != tt.want {
			t.Errorf("%s: Ready = %v, want %v", tt.name, got, tt.want)
		}
	}
	got := UnmetDependencies([]Task{mk(2, TaskDone), mk(3, TaskTodo), mk(5, TaskBlocked), mk(6, TaskCancelled)})
	if len(got) != 2 || got[0] != 3 || got[1] != 5 {
		t.Errorf("UnmetDependencies = %v, want [3 5]", got)
	}
}

func TestStale(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	task := func(s TaskStatus, age time.Duration) Task { return Task{Status: s, UpdatedAt: now.Add(-age)} }
	tests := []struct {
		name      string
		task      Task
		threshold time.Duration
		want      bool
	}{
		{"fresh", task(TaskInProgress, time.Hour), 0, false},
		{"just under default", task(TaskInProgress, 4*time.Hour-time.Second), 0, false},
		{"exactly default", task(TaskInProgress, 4*time.Hour), 0, true},
		{"over default", task(TaskInProgress, 9*time.Hour), 0, true},
		{"custom threshold", task(TaskInProgress, 31*time.Minute), 30 * time.Minute, true},
		{"negative threshold uses default", task(TaskInProgress, time.Hour), -1, false},
		{"todo never stale", task(TaskTodo, 100*time.Hour), 0, false},
		{"blocked never stale", task(TaskBlocked, 100*time.Hour), 0, false},
		{"done never stale", task(TaskDone, 100*time.Hour), 0, false},
	}
	for _, tt := range tests {
		if got := Stale(tt.task, now, tt.threshold); got != tt.want {
			t.Errorf("%s: Stale = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestDeriveFeatureStatus(t *testing.T) {
	ts := func(ss ...TaskStatus) []Task {
		out := make([]Task, len(ss))
		for i, s := range ss {
			out[i] = Task{Status: s}
		}
		return out
	}
	tests := []struct {
		name   string
		stored FeatureStatus
		tasks  []Task
		want   FeatureStatus
	}{
		{"no tasks", FeaturePlanned, nil, FeaturePlanned},
		{"all todo", FeaturePlanned, ts(TaskTodo, TaskTodo), FeaturePlanned},
		{"one in progress", FeaturePlanned, ts(TaskTodo, TaskInProgress), FeatureActive},
		{"one done one todo", FeaturePlanned, ts(TaskDone, TaskTodo), FeatureActive},
		{"one blocked", FeaturePlanned, ts(TaskBlocked), FeatureActive},
		{"all done", FeaturePlanned, ts(TaskDone, TaskDone), FeatureDone},
		{"done plus cancelled", FeaturePlanned, ts(TaskDone, TaskCancelled), FeatureDone},
		{"all cancelled", FeaturePlanned, ts(TaskCancelled), FeaturePlanned},
		{"stored cancelled wins", FeatureCancelled, ts(TaskDone), FeatureCancelled},
		{"stale stored done is ignored", FeatureDone, ts(TaskTodo), FeaturePlanned},
		{"stale stored active is ignored", FeatureActive, nil, FeaturePlanned},
	}
	for _, tt := range tests {
		if got := DeriveFeatureStatus(tt.stored, tt.tasks); got != tt.want {
			t.Errorf("%s: got %s, want %s", tt.name, got, tt.want)
		}
	}
}
