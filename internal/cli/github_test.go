package cli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeGitHubAPI serves one milestone, a closed issue with a merged PR and an
// open issue, and records REST writes.
func fakeGitHubAPI(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu     sync.Mutex
		writes []string
		closed bool
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/graphql" {
			mu.Lock()
			writes = append(writes, r.Method+" "+r.URL.Path)
			if r.Method == http.MethodPatch {
				closed = true
			}
			mu.Unlock()
			_, _ = io.WriteString(w, `{}`)
			return
		}
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "milestones("):
			_, _ = io.WriteString(w, `{"data":{"repository":{"milestones":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":1,"title":"Launch","state":"OPEN"}]}}}}`)
		case strings.Contains(req.Query, "issues("):
			mu.Lock()
			state := "OPEN"
			if closed {
				state = "CLOSED"
			}
			mu.Unlock()
			_, _ = io.WriteString(w, `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":false},"nodes":[
			  {"number":1,"title":"Ship it","body":"","state":"CLOSED","stateReason":"COMPLETED","url":"https://github.com/acme/rocket/issues/1","milestone":{"number":1},
			   "labels":{"nodes":[]},"assignees":{"nodes":[]},"projectItems":{"nodes":[]},
			   "closedByPullRequestsReferences":{"nodes":[{"number":5,"title":"ship","url":"https://github.com/acme/rocket/pull/5","state":"MERGED"}]}},
			  {"number":2,"title":"Polish","body":"","state":"`+state+`","stateReason":null,"url":"https://github.com/acme/rocket/issues/2","milestone":{"number":1},
			   "labels":{"nodes":[]},"assignees":{"nodes":[]},"projectItems":{"nodes":[]},"closedByPullRequestsReferences":{"nodes":[]}}]}}}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{"repository":{"hasDiscussionsEnabled":false}}}`)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), writes...)
	}
}

func TestCLIGitHubSync(t *testing.T) {
	ts, writes := fakeGitHubAPI(t)
	s := newSandbox(t)
	s.ok("init")
	s.ok("project", "create", "Rocket", "--slug", "rocket")
	env := []string{"GITHUB_TOKEN=test-token", "SKALD_GITHUB_API_URL=" + ts.URL}

	// Not linked and no git remote: a domain error that says what to do.
	r := s.runIn(s.repo, env, "github", "sync")
	if r.code != 1 || !strings.Contains(r.stderr, "owner/name") {
		t.Fatalf("unlinked sync: %d %s", r.code, r.stderr)
	}

	s.ok("github", "link", "acme/rocket")

	r = s.runIn(s.repo, env, "github", "sync", "--dry-run")
	if r.code != 0 || !strings.Contains(r.stdout, "dry run") || !strings.Contains(r.stdout, "tasks: 2 new") {
		t.Fatalf("dry run: %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if out := s.ok("task", "list", "--json").stdout; strings.Contains(out, "Ship it") {
		t.Fatalf("dry run wrote tasks: %s", out)
	}

	r = s.runIn(s.repo, env, "github", "sync", "--json")
	if r.code != 0 {
		t.Fatalf("sync: %d %s", r.code, r.stderr)
	}
	var res struct {
		TasksCreated  int `json:"tasks_created"`
		EvidenceAdded int `json:"evidence_added"`
		StatusChanges []struct {
			Task string `json:"task"`
			To   string `json:"to"`
		} `json:"status_changes"`
	}
	decode(t, r.stdout, &res)
	if res.TasksCreated != 2 || res.EvidenceAdded != 1 || len(res.StatusChanges) != 2 || res.StatusChanges[1].To != "done" {
		t.Fatalf("sync result: %+v", res)
	}
	done := res.StatusChanges[1].Task
	if got := s.task(done); got.Status != "done" || got.Title != "#1 Ship it" {
		t.Fatalf("synced task: %+v", got)
	}

	// Finish the open issue's task here, then push.
	var list struct {
		Tasks []taskOut `json:"tasks"`
	}
	decode(t, s.ok("task", "list", "--json").stdout, &list)
	var polish string
	for _, tk := range list.Tasks {
		if tk.Title == "#2 Polish" {
			polish = tk.ID
		}
	}
	if polish == "" {
		t.Fatalf("no Polish task: %+v", list)
	}
	s.ok("task", "start", polish)
	s.ok("evidence", "add", polish, "--kind", "commit", "--ref", "abc1234")
	s.ok("task", "done", polish)

	r = s.runIn(s.repo, env, "github", "sync")
	if !strings.Contains(r.stdout, "--push") {
		t.Fatalf("diverged task not reported:\n%s", r.stdout)
	}
	r = s.runIn(s.repo, env, "github", "sync", "--push")
	if r.code != 0 || !strings.Contains(r.stdout, "pushed to GitHub") {
		t.Fatalf("push: %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if w := writes(); len(w) != 2 || w[0] != "POST /repos/acme/rocket/issues/2/comments" || w[1] != "PATCH /repos/acme/rocket/issues/2" {
		t.Fatalf("GitHub writes: %q", w)
	}

	// Bad token: nothing written, the message names the fix.
	r = s.runIn(s.repo, []string{"GITHUB_TOKEN=nope", "SKALD_GITHUB_API_URL=" + ts.URL}, "github", "sync")
	if r.code != 1 || !strings.Contains(r.stderr, "token") {
		t.Fatalf("bad token: %d %s", r.code, r.stderr)
	}
}
