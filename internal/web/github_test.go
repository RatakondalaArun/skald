package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"skald/internal/domain"
	"skald/internal/github"
	"skald/internal/service"
	"skald/internal/store"
)

type wtGitHub struct{}

func (wtGitHub) FetchRepo(context.Context, github.Repo) (github.Snapshot, error) {
	return github.Snapshot{Issues: []github.Issue{{Number: 9, Title: "Dark mode", State: "OPEN", URL: "https://github.com/acme/rocket/issues/9"}}}, nil
}
func (wtGitHub) CloseIssue(context.Context, github.Repo, int, string, string) error { return nil }

func TestGitHubSyncEndpoint(t *testing.T) {
	db, err := store.Open(wtCtx, filepath.Join(t.TempDir(), "skald.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc := service.New(db, service.WithGitHub(func() (service.GitHubAPI, error) { return wtGitHub{}, nil }))
	if _, err := svc.CreateProject(wtCtx, domain.Human, service.CreateProjectInput{Slug: "rocket", Name: "Rocket"}); err != nil {
		t.Fatal(err)
	}
	e := &wtEnv{t: t, db: db, svc: svc, log: &wtLog{}}
	if e.srv, err = New(Config{Service: svc, Events: e.log.source, Keepalive: time.Hour, Logger: wtQuietLogger()}); err != nil {
		t.Fatal(err)
	}

	// Not linked yet: a validation error that says how to link.
	if code, body := e.post("/api/projects/rocket/github/sync", `{}`); code != http.StatusBadRequest || wtErrKind(body) != "validation" {
		t.Fatalf("unlinked: %d %v", code, body)
	}
	if _, _, err := svc.LinkGitHub(wtCtx, domain.Human, service.ProjectRef{Slug: "rocket"}, "acme/rocket"); err != nil {
		t.Fatal(err)
	}
	code, body := e.post("/api/projects/rocket/github/sync", `{}`)
	if code != http.StatusOK || body["tasks_created"].(float64) != 1 || body["repo"] != "acme/rocket" {
		t.Fatalf("sync: %d %v", code, body)
	}
	_, proj := e.get("/api/projects/rocket")
	if proj["project"].(map[string]any)["github"] != "acme/rocket" {
		t.Fatalf("project github: %v", proj["project"])
	}
	_, task := e.get("/api/projects/rocket/tasks/T-1")
	if task["task"].(map[string]any)["github"] != "https://github.com/acme/rocket/issues/9" {
		t.Fatalf("task github: %v", task["task"])
	}
	// The endpoint is a mutation: it needs the token like the others.
	req := e.postReq("/api/projects/rocket/github/sync", `{}`)
	req.Header.Del(TokenHeader)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no token: %d", rec.Code)
	}
}
