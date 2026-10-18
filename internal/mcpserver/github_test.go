package mcpserver

import (
	"context"
	"path/filepath"
	"testing"

	"skald/internal/domain"
	"skald/internal/github"
	"skald/internal/service"
	"skald/internal/store"
)

type fakeGitHub struct{ snap github.Snapshot }

func (f *fakeGitHub) FetchRepo(context.Context, github.Repo) (github.Snapshot, error) {
	return f.snap, nil
}
func (f *fakeGitHub) CloseIssue(context.Context, github.Repo, int, string, string) error { return nil }

func TestSyncGitHubTool(t *testing.T) {
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "skald.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	fake := &fakeGitHub{snap: github.Snapshot{Issues: []github.Issue{
		{Number: 3, Title: "Fix crash", State: "CLOSED", StateReason: "COMPLETED", URL: "https://github.com/acme/rocket/issues/3",
			PullRequests: []github.PullRequest{{Number: 4, URL: "https://github.com/acme/rocket/pull/4", State: "MERGED"}}},
	}}}
	svc := service.New(db, service.WithEnv(noEnv),
		service.WithGitHub(func() (service.GitHubAPI, error) { return fake, nil }))
	if _, err := svc.CreateProject(context.Background(), domain.Human, service.CreateProjectInput{Name: "Rocket", Slug: "rocket"}); err != nil {
		t.Fatal(err)
	}
	c := startServe(t, svc, "claude-code")

	c.fail("sync_github", map[string]any{"project": "rocket"}, "owner/name")

	got := c.ok("sync_github", map[string]any{"project": "rocket", "repo": "acme/rocket"})
	if got["tasks_created"].(float64) != 1 || got["evidence_added"].(float64) != 1 {
		t.Fatalf("sync: %v", got)
	}
	changes := got["status_changes"].([]any)
	last := changes[len(changes)-1].(map[string]any)
	if last["to"] != "done" {
		t.Fatalf("changes: %v", changes)
	}
	task := c.ok("get_task", map[string]any{"project": "rocket", "task": last["task"]})
	if task["github"] != "https://github.com/acme/rocket/issues/3" || task["status"] != "done" {
		t.Fatalf("get_task: %v", task)
	}
	// Repeating is a no-op.
	again := c.ok("sync_github", map[string]any{"project": "rocket"})
	if again["tasks_created"].(float64) != 0 || again["status_changes"] != nil {
		t.Fatalf("second sync: %v", again)
	}
}
