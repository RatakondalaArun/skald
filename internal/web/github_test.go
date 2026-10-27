package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"skald/internal/domain"
	"skald/internal/github"
	"skald/internal/service"
	"skald/internal/store"
)

type wtGitHub struct{}

func (wtGitHub) FetchRepo(context.Context, github.Repo) (github.Snapshot, error) {
	return github.Snapshot{
		Milestones: []github.Milestone{{Number: 3, Title: "Launch", State: "OPEN"}},
		Issues:     []github.Issue{{Number: 9, Title: "Dark mode", State: "OPEN", URL: "https://github.com/acme/rocket/issues/9", Milestone: 3}},
	}, nil
}
func (wtGitHub) CloseIssue(context.Context, github.Repo, int, string, string) error { return nil }
func (wtGitHub) CloseMilestone(context.Context, github.Repo, int) error             { return nil }
func (wtGitHub) CreateMilestone(context.Context, github.Repo, string, string) (int, error) {
	return 0, nil
}
func (wtGitHub) CreateIssue(context.Context, github.Repo, github.NewIssue) (int, string, error) {
	return 42, "https://github.com/acme/rocket/issues/42", nil
}

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
	if tk := task["task"].(map[string]any); tk["github"] != "https://github.com/acme/rocket/issues/9" || tk["github_issue"] != float64(9) {
		t.Fatalf("task github: %v", tk)
	}
	if f := proj["features"].([]any)[0].(map[string]any); f["github_milestone"] != float64(3) || f["github_url"] != "https://github.com/acme/rocket/milestone/3" {
		t.Fatalf("feature github: %v", f)
	}

	// A task that started in Skald shows the issue a sync created for it.
	feat := proj["features"].([]any)[0].(map[string]any)["id"].(string)
	fid, _ := domain.ParseID(domain.EntityFeature, feat)
	if _, _, err := svc.CreateTask(wtCtx, domain.Human, service.ProjectRef{Slug: "rocket"}, service.CreateTaskInput{FeatureID: fid, Title: "Docs"}); err != nil {
		t.Fatal(err)
	}
	if code, body := e.post("/api/projects/rocket/github/sync", `{"create_issues":true}`); code != http.StatusOK {
		t.Fatalf("create issues: %d %v", code, body)
	}
	_, board := e.get("/api/projects/rocket/board")
	issues := map[string]any{}
	for _, x := range board["tasks"].([]any) {
		tk := x.(map[string]any)
		issues[tk["id"].(string)] = tk["github_issue"]
	}
	if issues["T-1"] != float64(9) || issues["T-2"] != float64(42) {
		t.Fatalf("board issues: %v", issues)
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

func TestGitHubSignIn(t *testing.T) {
	approved := make(chan struct{})
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/device/code":
			_, _ = io.WriteString(w, `{"device_code":"dc","user_code":"WXYZ-0001","verification_uri":"https://github.com/login/device","expires_in":900,"interval":1}`)
		case "/login/oauth/access_token":
			select {
			case <-approved:
				_, _ = io.WriteString(w, `{"access_token":"gho_web","scope":"repo"}`)
			default:
				_, _ = io.WriteString(w, `{"error":"authorization_pending"}`)
			}
		case "/user":
			_, _ = io.WriteString(w, `{"login":"octocat"}`)
		}
	}))
	defer gh.Close()
	home := t.TempDir()
	t.Setenv("SKALD_HOME", home)
	t.Setenv("PATH", t.TempDir()) // no gh CLI
	t.Setenv(github.EnvToken, "")
	t.Setenv(github.EnvGHToken, "")
	t.Setenv(github.EnvAPIURL, gh.URL)
	t.Setenv(github.EnvClientID, "")
	builtIn := github.DefaultClientID
	github.DefaultClientID = "" // a build without an OAuth App
	t.Cleanup(func() { github.DefaultClientID = builtIn })
	e := wtNew(t)

	_, st := e.get("/api/github/auth")
	if st["configured"] != false || st["signed_in"] != false {
		t.Fatalf("unconfigured: %v", st)
	}
	if code, body := e.post("/api/github/login", `{}`); code != http.StatusBadRequest || wtErrKind(body) != "github" {
		t.Fatalf("login without client id: %d %v", code, body)
	}

	t.Setenv(github.EnvClientID, "cid")
	code, st := e.post("/api/github/login", `{}`)
	pending, _ := st["pending"].(map[string]any)
	if code != http.StatusOK || pending["user_code"] != "WXYZ-0001" || st["signed_in"] != false {
		t.Fatalf("login: %d %v", code, st)
	}
	close(approved)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, st = e.get("/api/github/auth")
		if st["pending"] == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sign-in never finished: %v", st)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st["signed_in"] != true || st["login"] != "octocat" || st["source"] != github.SourceLogin || st["error"] != nil {
		t.Fatalf("signed in: %v", st)
	}
	if b, err := os.ReadFile(filepath.Join(home, "github.json")); err != nil || !strings.Contains(string(b), "gho_web") {
		t.Fatalf("saved token: %s %v", b, err)
	}
	// The token itself never reaches the page.
	if raw, _ := json.Marshal(st); strings.Contains(string(raw), "gho_web") {
		t.Fatalf("token leaked: %s", raw)
	}

	if code, st = e.post("/api/github/logout", `{}`); code != http.StatusOK || st["signed_in"] != false {
		t.Fatalf("logout: %d %v", code, st)
	}
	if _, err := os.Stat(filepath.Join(home, "github.json")); !os.IsNotExist(err) {
		t.Fatalf("token file after logout: %v", err)
	}
}
