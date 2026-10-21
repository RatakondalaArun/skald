package mcpserver

import (
	"strings"
	"testing"
)

func TestScratchpadNotes(t *testing.T) {
	e := newEnv(t)
	c := e.c
	c.ok("create_project", map[string]any{"name": "Demo"})
	plan := c.ok("create_plan", twoTaskPlan())
	first := items(plan, "tasks")[0]
	taskA, featA := str(first, "id"), str(first, "feature")

	if gp := c.ok("get_project", nil); gp["notes"] != nil {
		t.Fatalf("get_project should omit notes when there are none: %v", gp)
	}
	n1 := c.ok("add_note", map[string]any{"body": "the CI runner has no network"})
	if str(n1, "id") != "N-1" || n1["task"] != nil {
		t.Fatalf("add_note = %v", n1)
	}
	long := strings.Repeat("x", 400)
	n2 := c.ok("add_note", map[string]any{"body": long, "task": taskA})
	if str(n2, "task") != taskA || str(n2, "feature") != featA {
		t.Fatalf("add_note on a task should link its feature: %v", n2)
	}

	ln := c.ok("list_notes", nil)
	notes := items(ln, "notes")
	if len(notes) != 2 || str(notes[0], "id") != "N-2" || notes[0]["clipped"] != true || len([]rune(str(notes[0], "body"))) != noteSnippet {
		t.Fatalf("list_notes = %v", ln)
	}
	if !strings.HasPrefix(str(notes[1], "author"), "claude:test-client/") {
		t.Errorf("notes should be attributed to the MCP session: %v", notes[1])
	}
	if l := c.ok("list_notes", map[string]any{"task": taskA}); len(items(l, "notes")) != 1 {
		t.Errorf("list_notes by task = %v", l)
	}
	if l := c.ok("list_notes", map[string]any{"query": "network"}); len(items(l, "notes")) != 1 || str(items(l, "notes")[0], "id") != "N-1" {
		t.Errorf("list_notes by query = %v", l)
	}
	if l := c.ok("list_notes", map[string]any{"limit": 1}); l["truncated"] != true || str(l, "note") == "" {
		t.Errorf("truncated list_notes = %v", l)
	}
	full := c.ok("get_note", map[string]any{"note": "N-2"})
	if str(full, "body") != long || full["clipped"] != nil {
		t.Errorf("get_note should return the full body: %v", full)
	}
	if gp := c.ok("get_project", nil); gp["notes"] != float64(2) {
		t.Errorf("get_project notes = %v", gp["notes"])
	}

	c.fail("add_note", map[string]any{"body": "  "}, "body")
	c.fail("add_note", map[string]any{"body": "x", "task": "F-1"}, "feature ID")
	c.fail("get_note", map[string]any{"note": "N-99"}, "not found")

	// Notes never move progress.
	if pr := c.ok("get_project_progress", nil); pr["percent"] != float64(0) {
		t.Errorf("progress = %v", pr["percent"])
	}
}
