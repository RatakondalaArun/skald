package web

import (
	"fmt"
	"testing"
)

func TestTaskTagsAPI(t *testing.T) {
	e := wtNew(t)
	task := func(m map[string]any) map[string]any { return m["task"].(map[string]any) }

	code, m := e.post("/api/projects/demo/tasks/T-3/edit", `{"tags": ["Reports", "email"]}`)
	if code != 200 || fmt.Sprint(task(m)["tags"]) != "[email reports]" {
		t.Fatalf("edit tags: %d %v", code, m)
	}
	if code, m = e.post("/api/projects/demo/tasks/T-1/edit", `{"tags": ["reports"]}`); code != 200 {
		t.Fatalf("edit T-1 tags: %d %v", code, m)
	}
	if code, m = e.post("/api/projects/demo/tasks/T-3/edit", `{"tags": ["two words"]}`); code != 400 {
		t.Fatalf("bad tag: %d %v", code, m)
	}

	// Every task carries a tags array, so the dashboard can filter without checks.
	code, m = e.get("/api/projects/demo/board")
	if code != 200 {
		t.Fatalf("board: %d %v", code, m)
	}
	for _, x := range m["tasks"].([]any) {
		if _, ok := x.(map[string]any)["tags"].([]any); !ok {
			t.Fatalf("board task without tags array: %v", x)
		}
	}

	code, m = e.get("/api/projects/demo/tasks?tag=reports")
	if code != 200 || len(m["tasks"].([]any)) != 2 {
		t.Fatalf("tasks?tag=reports: %d %v", code, m)
	}
	code, m = e.get("/api/projects/demo/tasks?tag=reports,email")
	if code != 200 || len(m["tasks"].([]any)) != 1 {
		t.Fatalf("tasks?tag=reports,email: %d %v", code, m)
	}
	if code, m = e.get("/api/projects/demo/tasks?tag=bad%20tag"); code != 400 {
		t.Fatalf("bad tag filter: %d %v", code, m)
	}

	// Clearing.
	if code, m = e.post("/api/projects/demo/tasks/T-3/edit", `{"tags": []}`); code != 200 || len(task(m)["tags"].([]any)) != 0 {
		t.Fatalf("clear tags: %d %v", code, m)
	}
}
