package domain

import (
	"math"
	"testing"
)

func tk(s TaskStatus, w int) Task { return Task{Status: s, Weight: w} }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestFeatureProgress(t *testing.T) {
	tests := []struct {
		name        string
		tasks       []Task
		done, total int
		frac        float64
	}{
		{"empty", nil, 0, 0, 0},
		{"all todo", []Task{tk(TaskTodo, 1), tk(TaskTodo, 1)}, 0, 2, 0},
		{"half done", []Task{tk(TaskDone, 1), tk(TaskTodo, 1)}, 1, 2, 0.5},
		{"all done", []Task{tk(TaskDone, 2), tk(TaskDone, 3)}, 5, 5, 1},
		{"all cancelled", []Task{tk(TaskCancelled, 4)}, 0, 0, 0},
		{"cancelled excluded", []Task{tk(TaskDone, 1), tk(TaskCancelled, 9)}, 1, 1, 1},
		{"weights", []Task{tk(TaskDone, 3), tk(TaskTodo, 1)}, 3, 4, 0.75},
		{"in progress and blocked do not count", []Task{tk(TaskInProgress, 1), tk(TaskBlocked, 1), tk(TaskDone, 2)}, 2, 4, 0.5},
	}
	for _, tt := range tests {
		p := FeatureProgress(tt.tasks)
		if p.Done != tt.done || p.Total != tt.total || !near(p.Fraction, tt.frac) {
			t.Errorf("%s: got %+v, want done=%d total=%d frac=%v", tt.name, p, tt.done, tt.total, tt.frac)
		}
		if !near(p.Percent(), tt.frac*100) {
			t.Errorf("%s: percent %v", tt.name, p.Percent())
		}
	}
}

func fw(id int64, w int, st FeatureStatus, tasks ...Task) FeatureWithTasks {
	return FeatureWithTasks{Feature: Feature{ID: id, Weight: w, Status: st}, Tasks: tasks}
}

func TestProjectProgress(t *testing.T) {
	tests := []struct {
		name     string
		features []FeatureWithTasks
		frac     float64
		done     int
		total    int
	}{
		{"no features", nil, 0, 0, 0},
		{"empty feature counts at zero", []FeatureWithTasks{fw(1, 1, FeaturePlanned)}, 0, 0, 1},
		{"empty feature holds figure back",
			[]FeatureWithTasks{fw(1, 1, FeaturePlanned, tk(TaskDone, 1)), fw(2, 1, FeaturePlanned)}, 0.5, 1, 2},
		{"all cancelled feature excluded",
			[]FeatureWithTasks{fw(1, 1, FeaturePlanned, tk(TaskDone, 1)), fw(2, 5, FeaturePlanned, tk(TaskCancelled, 1))}, 1, 1, 1},
		{"cancelled feature excluded",
			[]FeatureWithTasks{fw(1, 1, FeaturePlanned, tk(TaskDone, 1)), fw(2, 8, FeatureCancelled, tk(TaskTodo, 1))}, 1, 1, 1},
		{"only cancelled features", []FeatureWithTasks{fw(1, 3, FeatureCancelled)}, 0, 0, 0},
		{"nested weighting",
			// F1 weight 3, 1/2 done by task weight (2 of 4 => 0.5); F2 weight 1, fully done.
			// (3*0.5 + 1*1) / 4 = 0.625
			[]FeatureWithTasks{
				fw(1, 3, FeaturePlanned, tk(TaskDone, 2), tk(TaskTodo, 2)),
				fw(2, 1, FeaturePlanned, tk(TaskDone, 1)),
			}, 0.625, 1, 4},
		{"task weights inside a feature",
			[]FeatureWithTasks{fw(1, 1, FeaturePlanned, tk(TaskDone, 9), tk(TaskTodo, 1))}, 0.9, 0, 1},
		{"all done",
			[]FeatureWithTasks{fw(1, 2, FeaturePlanned, tk(TaskDone, 1)), fw(2, 3, FeaturePlanned, tk(TaskDone, 1))}, 1, 5, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := ProjectProgress(tt.features)
			if !near(r.Progress.Fraction, tt.frac) || r.Progress.Done != tt.done || r.Progress.Total != tt.total {
				t.Errorf("got %+v, want frac=%v done=%d total=%d", r.Progress, tt.frac, tt.done, tt.total)
			}
			if len(r.Features) != len(tt.features) {
				t.Errorf("entries = %d, want %d", len(r.Features), len(tt.features))
			}
		})
	}
}

func TestProjectProgressEntries(t *testing.T) {
	r := ProjectProgress([]FeatureWithTasks{
		fw(1, 1, FeaturePlanned, tk(TaskDone, 1)),
		fw(2, 1, FeaturePlanned, tk(TaskCancelled, 1)),
		fw(3, 1, FeatureCancelled),
		fw(4, 1, FeaturePlanned),
	})
	want := []struct {
		id      int64
		status  FeatureStatus
		counted bool
	}{
		{1, FeatureDone, true},
		{2, FeaturePlanned, false},
		{3, FeatureCancelled, false},
		{4, FeaturePlanned, true},
	}
	for i, w := range want {
		e := r.Features[i]
		if e.FeatureID != w.id || e.Status != w.status || e.Counted != w.counted {
			t.Errorf("entry %d = %+v, want %+v", i, e, w)
		}
	}
}
