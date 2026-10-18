package service

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"skald/internal/domain"
	"skald/internal/github"
	"skald/internal/store"
)

var (
	ghCtx = context.Background()
	ghT0  = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	ghRep = github.Repo{Owner: "Acme", Name: "Rocket"}
)

type ghClose struct {
	Number  int
	Reason  string
	Comment string
}

// ghFake is an in-memory GitHub.
type ghFake struct {
	snap       github.Snapshot
	fetchErr   error
	closed     []ghClose
	milestones []int // closed milestones
	msErr      error
}

func (f *ghFake) CloseMilestone(_ context.Context, _ github.Repo, n int) error {
	if f.msErr != nil {
		return f.msErr
	}
	f.milestones = append(f.milestones, n)
	for i := range f.snap.Milestones {
		if f.snap.Milestones[i].Number == n {
			f.snap.Milestones[i].State = "CLOSED"
		}
	}
	return nil
}

func (f *ghFake) FetchRepo(_ context.Context, repo github.Repo) (github.Snapshot, error) {
	if f.fetchErr != nil {
		return github.Snapshot{}, f.fetchErr
	}
	s := f.snap
	s.Repo = repo
	return s, nil
}

func (f *ghFake) CloseIssue(_ context.Context, _ github.Repo, n int, reason, comment string) error {
	f.closed = append(f.closed, ghClose{n, reason, comment})
	for i := range f.snap.Issues {
		if f.snap.Issues[i].Number == n {
			f.snap.Issues[i].State, f.snap.Issues[i].StateReason = "CLOSED", strings.ToUpper(reason)
		}
	}
	return nil
}

type ghEnv struct {
	svc  *Service
	db   *store.DB
	fake *ghFake
	p    domain.Project
	ref  ProjectRef
}

func ghNew(t *testing.T) *ghEnv {
	t.Helper()
	db, err := store.Open(ghCtx, filepath.Join(t.TempDir(), "skald.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &ghEnv{db: db, fake: &ghFake{snap: ghSnapshot()}, ref: ProjectRef{Slug: "rocket"}}
	err = db.WriteTx(ghCtx, func(tx *sql.Tx) (err error) {
		e.p, err = store.CreateProject(ghCtx, tx, store.Meta{Actor: domain.Human, Now: ghT0}, domain.Project{Slug: "rocket", Name: "Rocket", RepoPath: "/src/rocket"})
		return
	})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = New(db,
		WithClock(func() time.Time { return ghT0 }),
		WithEnv(func(string) string { return "" }),
		WithWorkdir(func() (string, error) { return "/nowhere", nil }),
		WithGitHub(func() (GitHubAPI, error) { return e.fake, nil }),
		WithGitRemote(func(dir string) (github.Repo, error) {
			if dir == "/src/rocket" {
				return ghRep, nil
			}
			return github.Repo{}, errors.New("no remote")
		}),
	)
	return e
}

func ghSnapshot() github.Snapshot {
	return github.Snapshot{
		Milestones: []github.Milestone{
			{Number: 1, Title: "v1.0", Description: "first release", State: "OPEN"},
			{Number: 2, Title: "v2.0", State: "OPEN"},
			{Number: 3, Title: "Empty", State: "CLOSED"}, // no issues: no feature
		},
		Issues: []github.Issue{
			{Number: 1, Title: "Login page", Body: "Users sign in", State: "CLOSED", StateReason: "COMPLETED", URL: "https://github.com/Acme/Rocket/issues/1", Milestone: 1,
				Labels:       []string{"P0"},
				PullRequests: []github.PullRequest{{Number: 10, Title: "Add login", URL: "https://github.com/Acme/Rocket/pull/10", State: "MERGED", MergeCommit: "abcdef1234567"}}},
			{Number: 2, Title: "Signup", State: "OPEN", URL: "https://github.com/Acme/Rocket/issues/2", Milestone: 1,
				PullRequests: []github.PullRequest{{Number: 11, Title: "WIP signup", URL: "https://github.com/Acme/Rocket/pull/11", State: "OPEN"}}},
			{Number: 3, Title: "Old idea", State: "CLOSED", StateReason: "NOT_PLANNED", URL: "https://github.com/Acme/Rocket/issues/3", Milestone: 2},
			{Number: 4, Title: "Waiting on vendor", State: "OPEN", URL: "https://github.com/Acme/Rocket/issues/4", Milestone: 2, BoardStatus: []string{"Blocked"}},
			{Number: 5, Title: "Docs", State: "OPEN", URL: "https://github.com/Acme/Rocket/issues/5", Labels: []string{"priority: low"}},
			{Number: 6, Title: "Board work", State: "OPEN", URL: "https://github.com/Acme/Rocket/issues/6", Milestone: 2, BoardStatus: []string{"In Progress"}},
		},
		Discussions: []github.Discussion{{Number: 7, Title: "Roadmap", URL: "https://github.com/Acme/Rocket/discussions/7", Category: "Ideas"}},
	}
}

func (e *ghEnv) sync(t *testing.T, o GitHubSyncOptions) GitHubSyncResult {
	t.Helper()
	res, err := e.svc.SyncGitHub(ghCtx, domain.Human, e.ref, o)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return res
}

func (e *ghEnv) task(t *testing.T, issue int) domain.Task {
	t.Helper()
	var task domain.Task
	err := e.db.Reader().QueryRowContext(ghCtx, `SELECT id FROM tasks WHERE idempotency_key = ?`, GitHubIssueKey(ghRep, issue)).Scan(&task.ID)
	if err != nil {
		t.Fatalf("task for issue %d: %v", issue, err)
	}
	task, err = store.GetTask(ghCtx, e.db.Reader(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (e *ghEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.Reader().QueryRowContext(ghCtx, q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGitHubSyncInitialPull(t *testing.T) {
	e := ghNew(t)
	res := e.sync(t, GitHubSyncOptions{})

	if res.Repo != "Acme/Rocket" || res.Issues != 6 || res.Milestones != 3 || res.Discussions != 1 {
		t.Fatalf("counts: %+v", res)
	}
	if res.FeaturesCreated != 3 || res.TasksCreated != 6 || len(res.Skipped) != 0 {
		t.Fatalf("created: %+v", res)
	}

	feats, err := store.ListFeatures(ghCtx, e.db.Reader(), e.p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, f := range feats {
		titles = append(titles, f.Title)
	}
	if got := strings.Join(titles, ","); got != "v1.0,v2.0,No milestone" {
		t.Fatalf("features = %s", got)
	}

	t1 := e.task(t, 1)
	if t1.Status != domain.TaskDone || t1.Title != "#1 Login page" || t1.Priority != 0 || t1.FeatureID != feats[0].ID {
		t.Fatalf("issue 1 task: %+v", t1)
	}
	arts, _ := store.ListArtifacts(ghCtx, e.db.Reader(), t1.ID)
	if len(arts) != 1 || arts[0].Kind != domain.ArtifactPR || arts[0].Ref != "https://github.com/Acme/Rocket/pull/10" || !strings.Contains(arts[0].Summary, "abcdef1") {
		t.Fatalf("issue 1 evidence: %+v", arts)
	}
	if st := e.task(t, 2).Status; st != domain.TaskInProgress {
		t.Fatalf("issue 2 with open PR: %s", st)
	}
	if st := e.task(t, 3).Status; st != domain.TaskCancelled {
		t.Fatalf("issue 3 not planned: %s", st)
	}
	if t4 := e.task(t, 4); t4.Status != domain.TaskBlocked || t4.BlockedReason != boardBlockedReason {
		t.Fatalf("issue 4 board blocked: %+v", t4)
	}
	if t5 := e.task(t, 5); t5.Status != domain.TaskTodo || t5.Priority != 3 || t5.FeatureID != feats[2].ID {
		t.Fatalf("issue 5: %+v", t5)
	}
	if st := e.task(t, 6).Status; st != domain.TaskInProgress {
		t.Fatalf("issue 6 board in progress: %s", st)
	}

	_, doc, err := store.GetDocument(ghCtx, e.db.Reader(), e.p.ID, GitHubDocKind, GitHubDiscussionsTitle)
	if err != nil || !strings.Contains(doc.Body, "#7 Roadmap [Ideas] (open) https://github.com/Acme/Rocket/discussions/7") {
		t.Fatalf("discussions doc: %v %q", err, doc.Body)
	}

	p, _ := store.GetProject(ghCtx, e.db.Reader(), e.p.ID)
	if p.Settings.GitHub == nil || p.Settings.GitHub.Repo != "Acme/Rocket" || len(p.Settings.GitHub.Milestones) != 3 || !p.Settings.RequireEvidence {
		t.Fatalf("settings: %+v", p.Settings)
	}

	// Progress stays computed: issue 1 is the only done task.
	rep, err := e.svc.ProjectProgress(ghCtx, e.ref)
	if err != nil || rep.Counts.Done != 1 || rep.Counts.Cancelled != 1 {
		t.Fatalf("progress: %+v %v", rep.Counts, err)
	}
}

func TestGitHubSyncIsIdempotent(t *testing.T) {
	e := ghNew(t)
	e.sync(t, GitHubSyncOptions{})
	events := e.count(t, `SELECT COUNT(*) FROM events`)
	res := e.sync(t, GitHubSyncOptions{})
	if res.FeaturesCreated+res.FeaturesUpdated+res.TasksCreated+res.TasksUpdated+res.EvidenceAdded != 0 || len(res.StatusChanges) != 0 || res.DiscussionsDoc {
		t.Fatalf("second sync changed something: %+v", res)
	}
	if got := e.count(t, `SELECT COUNT(*) FROM events`); got != events {
		t.Fatalf("second sync wrote %d events", got-events)
	}
	if got := e.count(t, `SELECT COUNT(*) FROM tasks`); got != 6 {
		t.Fatalf("tasks = %d", got)
	}
}

func TestGitHubSyncFollowsChanges(t *testing.T) {
	e := ghNew(t)
	e.sync(t, GitHubSyncOptions{})

	e.fake.snap.Milestones[0].Title = "v1.0 GA"
	e.fake.snap.Issues[1].Title = "Signup flow"
	e.fake.snap.Issues[4].Milestone = 1 // Docs moves into v1.0
	// Signup closes without a merged PR; the board unblocks issue 4.
	e.fake.snap.Issues[1].State, e.fake.snap.Issues[1].StateReason = "CLOSED", "COMPLETED"
	e.fake.snap.Issues[3].BoardStatus = []string{"Todo"}
	e.fake.snap.Discussions = append(e.fake.snap.Discussions, github.Discussion{Number: 8, Title: "Q&A", URL: "u", Answered: true})

	res := e.sync(t, GitHubSyncOptions{})
	if res.FeaturesUpdated != 1 || res.TasksUpdated != 2 || !res.DiscussionsDoc {
		t.Fatalf("updates: %+v", res)
	}
	t2 := e.task(t, 2)
	if t2.Title != "#2 Signup flow" || t2.Status != domain.TaskDone {
		t.Fatalf("issue 2: %+v", t2)
	}
	arts, _ := store.ListArtifacts(ghCtx, e.db.Reader(), t2.ID)
	if len(arts) != 1 || arts[0].Kind != domain.ArtifactLink || arts[0].Ref != "https://github.com/Acme/Rocket/issues/2" {
		t.Fatalf("issue 2 evidence: %+v", arts)
	}
	feats, _ := store.ListFeatures(ghCtx, e.db.Reader(), e.p.ID)
	if feats[0].Title != "v1.0 GA" || e.task(t, 5).FeatureID != feats[0].ID {
		t.Fatalf("feature rename/move: %+v", feats)
	}
	if t4 := e.task(t, 4); t4.Status != domain.TaskTodo {
		t.Fatalf("issue 4 should be unblocked to todo: %+v", t4)
	}
}

func TestGitHubSyncKeepsLocalBlocker(t *testing.T) {
	e := ghNew(t)
	e.sync(t, GitHubSyncOptions{})
	t5 := e.task(t, 5)
	if _, err := e.svc.UpdateTaskStatus(ghCtx, domain.Human, t5.ID, StatusChange{To: domain.TaskBlocked, BlockedReason: "needs design"}); err != nil {
		t.Fatal(err)
	}
	e.fake.snap.Issues[4].BoardStatus = []string{"In Progress"}
	e.sync(t, GitHubSyncOptions{})
	if got := e.task(t, 5); got.Status != domain.TaskBlocked || got.BlockedReason != "needs design" {
		t.Fatalf("local blocker overwritten: %+v", got)
	}
}

func TestGitHubSyncDryRunWritesNothing(t *testing.T) {
	e := ghNew(t)
	before := e.count(t, `SELECT COUNT(*) FROM events`)
	res := e.sync(t, GitHubSyncOptions{DryRun: true, Push: true})
	if !res.DryRun || res.TasksCreated != 6 || len(res.StatusChanges) == 0 {
		t.Fatalf("dry run should report the changes: %+v", res)
	}
	if got := e.count(t, `SELECT COUNT(*) FROM events`); got != before {
		t.Fatalf("dry run wrote %d events", got-before)
	}
	if e.count(t, `SELECT COUNT(*) FROM tasks`) != 0 || len(e.fake.closed) != 0 {
		t.Fatal("dry run wrote tasks or pushed")
	}
}

func TestGitHubSyncPushClosesFinishedIssues(t *testing.T) {
	e := ghNew(t)
	e.sync(t, GitHubSyncOptions{})

	// Finish issue 6 in Skald with evidence, and cancel issue 5.
	t6 := e.task(t, 6)
	if _, _, err := e.svc.ReportTaskResult(ghCtx, domain.Human, t6.ID, TaskResult{Artifacts: []ArtifactInput{{Kind: domain.ArtifactCommit, Ref: "f00ba47", Summary: "board work"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateTaskStatus(ghCtx, domain.Human, e.task(t, 5).ID, StatusChange{To: domain.TaskCancelled}); err != nil {
		t.Fatal(err)
	}

	// Without push the difference is reported, and the task is not reopened.
	res := e.sync(t, GitHubSyncOptions{})
	if len(res.Diverged) != 2 || e.task(t, 6).Status != domain.TaskDone {
		t.Fatalf("diverged: %+v", res.Diverged)
	}

	res = e.sync(t, GitHubSyncOptions{Push: true})
	if len(res.Pushed) != 2 || len(res.Diverged) != 0 || len(res.PushFailed) != 0 {
		t.Fatalf("push: %+v", res)
	}
	if len(e.fake.closed) != 2 {
		t.Fatalf("closed = %+v", e.fake.closed)
	}
	for _, c := range e.fake.closed {
		switch c.Number {
		case 5:
			if c.Reason != "not_planned" {
				t.Fatalf("issue 5 reason %q", c.Reason)
			}
		case 6:
			if c.Reason != "completed" || !strings.Contains(c.Comment, "commit: f00ba47") {
				t.Fatalf("issue 6 close: %+v", c)
			}
		default:
			t.Fatalf("closed unexpected issue %d", c.Number)
		}
	}
	if n := e.count(t, `SELECT COUNT(*) FROM events WHERE type = 'task.note' AND entity_id = ? AND payload LIKE '%Closed Acme/Rocket#6%'`, t6.ID); n != 1 {
		t.Fatalf("push note on T-6: %d", n)
	}
	// Now GitHub agrees; nothing more to push.
	if res := e.sync(t, GitHubSyncOptions{Push: true}); len(res.Pushed)+len(res.Diverged) != 0 {
		t.Fatalf("third sync: %+v", res)
	}
}

func TestGitHubSyncSkipsIssueThatBreaksARule(t *testing.T) {
	e := ghNew(t)
	e.sync(t, GitHubSyncOptions{})
	// Docs (#5) depends on Board work (#6), then Docs is closed on GitHub.
	if _, err := e.svc.AddDependency(ghCtx, domain.Human, e.task(t, 5).ID, e.task(t, 6).ID); err != nil {
		t.Fatal(err)
	}
	e.fake.snap.Issues[4].State, e.fake.snap.Issues[4].StateReason = "CLOSED", "COMPLETED"
	e.fake.snap.Issues[4].Title = "Docs v2"
	e.fake.snap.Issues[0].Title = "Login page v2"

	res := e.sync(t, GitHubSyncOptions{})
	if len(res.Skipped) != 1 || res.Skipped[0].Issue != 5 || res.Skipped[0].TaskID != e.task(t, 5).ID {
		t.Fatalf("skipped: %+v", res.Skipped)
	}
	// The skipped issue is rolled back whole (title too); the rest applied.
	if got := e.task(t, 5); got.Status != domain.TaskTodo || got.Title != "#5 Docs" {
		t.Fatalf("issue 5 should be untouched: %+v", got)
	}
	if got := e.task(t, 1); got.Title != "#1 Login page v2" {
		t.Fatalf("issue 1 not updated: %+v", got)
	}
}

func TestGitHubSyncErrors(t *testing.T) {
	e := ghNew(t)
	// No link, no remote.
	e.svc.gitRemote = func(string) (github.Repo, error) { return github.Repo{}, errors.New("none") }
	if _, err := e.svc.SyncGitHub(ghCtx, domain.Human, e.ref, GitHubSyncOptions{}); domain.KindOf(err) != domain.KindValidation || !strings.Contains(err.Error(), "owner/name") {
		t.Fatalf("no repo: %v", err)
	}
	if _, err := e.svc.SyncGitHub(ghCtx, domain.Human, e.ref, GitHubSyncOptions{Repo: "not a repo"}); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("bad repo: %v", err)
	}
	// API failure writes nothing.
	e.fake.fetchErr = &github.APIError{Status: 401}
	if _, err := e.svc.SyncGitHub(ghCtx, domain.Human, e.ref, GitHubSyncOptions{Repo: "acme/rocket"}); domain.KindOf(err) != domain.KindValidation || !strings.Contains(err.Error(), "token") {
		t.Fatalf("api error: %v", err)
	}
	if e.count(t, `SELECT COUNT(*) FROM tasks`) != 0 {
		t.Fatal("failed sync wrote tasks")
	}
	// No token.
	e.svc.newGitHub = defaultGitHub
	t.Setenv(github.EnvToken, "")
	t.Setenv(github.EnvGHToken, "")
	t.Setenv("PATH", "")
	if _, err := e.svc.SyncGitHub(ghCtx, domain.Human, e.ref, GitHubSyncOptions{Repo: "acme/rocket"}); domain.KindOf(err) != domain.KindValidation || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("no token: %v", err)
	}
}

func TestGitHubLinkAndUnlink(t *testing.T) {
	e := ghNew(t)
	p, repo, err := e.svc.LinkGitHub(ghCtx, domain.Human, e.ref, "")
	if err != nil || repo != ghRep || p.Settings.GitHub.Repo != "Acme/Rocket" {
		t.Fatalf("link from remote: %v %+v", err, p.Settings)
	}
	if _, repo, err = e.svc.LinkGitHub(ghCtx, domain.Human, e.ref, "https://github.com/other/thing.git"); err != nil || repo.String() != "other/thing" {
		t.Fatalf("link explicit: %v %v", err, repo)
	}
	p, err = e.svc.UnlinkGitHub(ghCtx, domain.Human, e.ref)
	if err != nil || p.Settings.GitHub != nil || !p.Settings.RequireEvidence {
		t.Fatalf("unlink: %v %+v", err, p.Settings)
	}
}

func TestGitHubHelpers(t *testing.T) {
	if got := GitHubIssueKey(ghRep, 12); got != "github:acme/rocket#12" {
		t.Fatal(got)
	}
	if got := GitHubIssueURL("github:acme/rocket#12"); got != "https://github.com/acme/rocket/issues/12" {
		t.Fatal(got)
	}
	for _, k := range []string{"", "auth-login", "github:nope", "github:a/b#x"} {
		if GitHubIssueURL(k) != "" {
			t.Fatalf("%q should not be a GitHub key", k)
		}
	}
	for labels, want := range map[string]int{"P0": 0, "p1": 1, "priority: high": 1, "Priority/Low": 3, "priority-critical": 0, "bug,P2": 2} {
		got := priorityFromLabels(strings.Split(labels, ","))
		if got == nil || *got != want {
			t.Fatalf("%s: got %v want %d", labels, got, want)
		}
	}
	if priorityFromLabels([]string{"bug", "P9"}) != nil {
		t.Fatal("non-priority labels matched")
	}
	if boardState([]string{"In review"}) != domain.TaskInProgress || boardState([]string{"Done"}) != "" || boardState([]string{"In progress", "Blocked"}) != domain.TaskBlocked {
		t.Fatal("boardState")
	}
}

func TestGitHubSyncReopensCancelledTaskClosedAsCompleted(t *testing.T) {
	e := ghNew(t)
	e.sync(t, GitHubSyncOptions{})
	// #3 was closed as not planned; later it is reconsidered and shipped.
	e.fake.snap.Issues[2].StateReason = "COMPLETED"
	e.fake.snap.Issues[2].PullRequests = []github.PullRequest{{Number: 30, URL: "https://github.com/Acme/Rocket/pull/30", State: "MERGED"}}
	e.sync(t, GitHubSyncOptions{})
	t3 := e.task(t, 3)
	arts, _ := store.ListArtifacts(ghCtx, e.db.Reader(), t3.ID)
	if t3.Status != domain.TaskDone || len(arts) != 1 || arts[0].Kind != domain.ArtifactPR {
		t.Fatalf("issue 3: %+v %+v", t3, arts)
	}
}

func TestGitHubLinkSurvivesExport(t *testing.T) {
	e := ghNew(t)
	e.sync(t, GitHubSyncOptions{})
	x, err := e.svc.Export(ghCtx, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gh := x.Projects[0].Settings.GitHub
	if gh == nil || gh.Repo != "Acme/Rocket" || len(gh.Milestones) != 3 {
		t.Fatalf("exported settings: %+v", x.Projects[0].Settings)
	}
}

func TestGitHubSyncPushClosesDoneMilestones(t *testing.T) {
	e := ghNew(t)
	e.sync(t, GitHubSyncOptions{})
	// v1.0 (milestone 1) holds #1 (done) and #2 (in progress). Finish #2 here.
	t2 := e.task(t, 2)
	if _, _, err := e.svc.ReportTaskResult(ghCtx, domain.Human, t2.ID, TaskResult{Artifacts: []ArtifactInput{{Kind: domain.ArtifactCommit, Ref: "beef123"}}}); err != nil {
		t.Fatal(err)
	}

	res := e.sync(t, GitHubSyncOptions{})
	if len(res.MilestonesDiverged) != 1 || res.MilestonesDiverged[0].Milestone != 1 || len(e.fake.milestones) != 0 {
		t.Fatalf("without push: %+v", res.MilestonesDiverged)
	}

	res = e.sync(t, GitHubSyncOptions{Push: true, DryRun: true})
	if len(res.MilestonesPushed) != 1 || res.MilestonesPushed[0].Note != "would be closed" || len(e.fake.milestones)+len(e.fake.closed) != 0 {
		t.Fatalf("dry run: %+v", res)
	}

	res = e.sync(t, GitHubSyncOptions{Push: true})
	if len(res.Pushed) != 1 || res.Pushed[0].Issue != 2 {
		t.Fatalf("issue push: %+v", res.Pushed)
	}
	if len(res.MilestonesPushed) != 1 || len(e.fake.milestones) != 1 || e.fake.milestones[0] != 1 || len(res.MilestonesDiverged) != 0 {
		t.Fatalf("milestone push: %+v %v", res, e.fake.milestones)
	}
	// v2.0 still has open work and stays open; nothing is left to push.
	if res := e.sync(t, GitHubSyncOptions{Push: true}); len(res.MilestonesPushed)+len(res.MilestonesDiverged) != 0 {
		t.Fatalf("second push: %+v", res)
	}
}

func TestGitHubSyncReportsMilestonePushFailure(t *testing.T) {
	e := ghNew(t)
	// #2 is synced done, reopened on GitHub, then cancelled here, so its
	// milestone (v1.0) is done here while #2 is open on GitHub.
	e.fake.snap.Issues[1].State = "CLOSED"
	e.sync(t, GitHubSyncOptions{})
	e.fake.snap.Issues[1].State = "OPEN"
	e.fake.snap.Issues[1].PullRequests = nil
	if _, err := e.svc.UpdateTaskStatus(ghCtx, domain.Human, e.task(t, 2).ID, StatusChange{To: domain.TaskTodo}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateTaskStatus(ghCtx, domain.Human, e.task(t, 2).ID, StatusChange{To: domain.TaskCancelled}); err != nil {
		t.Fatal(err)
	}
	e.fake.msErr = &github.APIError{Status: 403, Message: "nope"}
	res := e.sync(t, GitHubSyncOptions{Push: true})
	if len(res.Pushed) != 1 || len(res.MilestonesPushFailed) != 1 || !strings.Contains(res.MilestonesPushFailed[0].Note, "403") {
		t.Fatalf("push: %+v", res)
	}
}
