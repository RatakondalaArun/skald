package cli_test

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestNoteCommands(t *testing.T) {
	s := newSandbox(t)
	s.ok("init")
	s.ok("project", "create", "Notes Demo")
	s.ok("feature", "add", "Auth")
	s.ok("task", "add", "Login form", "--feature", "F-1")

	contains(t, s.ok("note", "list").stdout, "no notes")
	contains(t, s.ok("note", "add", "staging", "tokens", "expire", "fast").stdout, "added N-1")
	contains(t, s.ok("note", "add", "--task", "T-1", "bcrypt cost 14 is too slow").stdout, "added N-2 (T-1, F-1)")

	// Standard input with "-".
	cmd := exec.Command(skaldBin, "note", "add", "-", "--feature", "F-1")
	cmd.Dir = s.repo
	cmd.Env = append(os.Environ(), "SKALD_HOME="+s.home, "SKALD_ACTOR=claude:cli/s1")
	cmd.Stdin = strings.NewReader("line one\nline two\n")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("note add -: %v", err)
	}
	contains(t, out.String(), "added N-3 (F-1)")

	var list struct {
		Notes []struct {
			ID      string `json:"id"`
			Task    string `json:"task"`
			Feature string `json:"feature"`
			Body    string `json:"body"`
			Actor   string `json:"actor"`
		} `json:"notes"`
		Truncated bool `json:"truncated"`
	}
	decode(t, s.ok("note", "list", "--json").stdout, &list)
	if len(list.Notes) != 3 || list.Notes[0].ID != "N-3" || list.Notes[0].Body != "line one\nline two" || list.Notes[0].Actor != "claude:cli/s1" {
		t.Fatalf("note list = %+v", list)
	}
	if list.Notes[2].Body != "staging tokens expire fast" || list.Notes[2].Actor != "human" {
		t.Errorf("oldest note = %+v", list.Notes[2])
	}
	decode(t, s.ok("note", "list", "--json", "--task", "T-1").stdout, &list)
	if len(list.Notes) != 1 || list.Notes[0].ID != "N-2" {
		t.Errorf("--task = %+v", list.Notes)
	}
	decode(t, s.ok("note", "list", "--json", "--search", "TOKENS", "--author", "human").stdout, &list)
	if len(list.Notes) != 1 || list.Notes[0].ID != "N-1" {
		t.Errorf("--search --author = %+v", list.Notes)
	}
	decode(t, s.ok("note", "list", "--json", "--limit", "1").stdout, &list)
	if len(list.Notes) != 1 || !list.Truncated {
		t.Errorf("--limit 1 = %+v", list)
	}
	r := s.ok("note", "list")
	contains(t, r.stdout, "N-2")
	contains(t, r.stdout, "T-1, F-1")

	show := s.ok("note", "show", "N-3").stdout
	contains(t, show, "linked to F-1")
	contains(t, show, "line one\nline two")

	// Notes do not move progress.
	contains(t, s.ok("status").stdout, "0%")

	contains(t, s.fail(2, "note", "show", "T-1").stderr, "task ID")
	contains(t, s.fail(2, "note", "add", "x", "--task", "F-1").stderr, "feature ID")
	contains(t, s.fail(1, "note", "show", "N-99").stderr, "not found")
	contains(t, s.fail(1, "note", "add", "x", "--task", "T-9").stderr, "not found")
	s.fail(2, "note", "add")
}
