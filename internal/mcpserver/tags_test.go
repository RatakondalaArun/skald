package mcpserver

import (
	"fmt"
	"testing"
)

func tagList(m map[string]any) string { return fmt.Sprint(m["tags"]) }

func TestTagTools(t *testing.T) {
	e := newEnv(t)
	c := e.c
	c.ok("create_project", map[string]any{"name": "Demo"})
	fID := str(c.ok("create_feature", map[string]any{"title": "Auth"}), "id")
	a := c.ok("create_task", map[string]any{"feature": fID, "title": "A", "tags": []any{"UI", "bug"}})["task"].(map[string]any)
	if tagList(a) != "[bug ui]" {
		t.Fatalf("create_task tags = %v", a)
	}
	b := c.ok("create_task", map[string]any{"feature": fID, "title": "B"})["task"].(map[string]any)
	if _, ok := b["tags"]; ok {
		t.Fatalf("untagged task line should omit tags: %v", b)
	}
	bID := str(b, "id")

	tagged := c.ok("tag_task", map[string]any{"task": bID, "add": []any{"ui", "backend"}})["task"].(map[string]any)
	if tagList(tagged) != "[backend ui]" {
		t.Fatalf("tag_task = %v", tagged)
	}
	tagged = c.ok("tag_task", map[string]any{"task": bID, "remove": []any{"backend"}})["task"].(map[string]any)
	if tagList(tagged) != "[ui]" {
		t.Fatalf("tag_task remove = %v", tagged)
	}
	if g := c.ok("get_task", map[string]any{"task": bID}); tagList(g) != "[ui]" {
		t.Fatalf("get_task = %v", g)
	}

	if l := items(c.ok("list_tasks", map[string]any{"tags": []any{"ui"}}), "tasks"); len(l) != 2 {
		t.Fatalf("list_tasks ui = %v", l)
	}
	if l := items(c.ok("list_tasks", map[string]any{"tags": []any{"ui", "bug"}}), "tasks"); len(l) != 1 || str(l[0], "id") != str(a, "id") {
		t.Fatalf("list_tasks ui+bug = %v", l)
	}

	tags := items(c.ok("list_tags", nil), "tags")
	if len(tags) != 2 || str(tags[0], "tag") != "ui" || tags[0]["tasks"] != float64(2) || str(tags[1], "tag") != "bug" {
		t.Fatalf("list_tags = %v", tags)
	}

	c.fail("tag_task", map[string]any{"task": bID, "add": []any{"two words"}}, "tag")
	c.fail("tag_task", map[string]any{"task": "T-99", "add": []any{"x"}}, "T-99")
}
