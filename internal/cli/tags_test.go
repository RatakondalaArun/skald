package cli_test

import (
	"slices"
	"testing"
)

func TestTaskTags(t *testing.T) {
	s := newSandbox(t)
	s.ok("init")
	s.ok("project", "create", "Demo")
	s.ok("feature", "add", "Board")

	var a struct {
		ID   string   `json:"id"`
		Tags []string `json:"tags"`
	}
	decode(t, s.ok("task", "add", "Fix scroll", "--feature", "F-1", "--tag", "UI", "--tag", "bug,ui", "--json").stdout, &a)
	if !slices.Equal(a.Tags, []string{"bug", "ui"}) {
		t.Fatalf("task add tags = %v", a.Tags)
	}
	s.ok("task", "add", "Write API", "--feature", "F-1", "--tag", "backend")
	s.ok("task", "add", "Untagged", "--feature", "F-1")

	contains(t, s.ok("task", "list").stdout, "#bug #ui")
	var l struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	decode(t, s.ok("task", "list", "--tag", "ui", "--json").stdout, &l)
	if len(l.Tasks) != 1 || l.Tasks[0].ID != a.ID {
		t.Fatalf("task list --tag ui = %+v", l.Tasks)
	}

	s.ok("task", "edit", a.ID, "--tag", "frontend", "--untag", "bug")
	contains(t, s.ok("task", "show", a.ID).stdout, "tags        frontend, ui")

	r := s.ok("task", "tags")
	contains(t, r.stdout, "TAG")
	contains(t, r.stdout, "frontend")
	var tags struct {
		Tags []struct {
			Tag   string `json:"tag"`
			Tasks int    `json:"tasks"`
		} `json:"tags"`
	}
	decode(t, s.ok("task", "tags", "--json").stdout, &tags)
	if len(tags.Tags) != 3 || tags.Tags[0].Tag != "backend" {
		t.Fatalf("task tags = %+v", tags.Tags)
	}

	contains(t, s.fail(1, "task", "edit", a.ID, "--tag", "two words").stderr, "tag")
}
