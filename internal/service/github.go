package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"skald/internal/domain"
	"skald/internal/github"
	"skald/internal/store"
)

// GitHubAPI is what a sync needs from GitHub. *github.Client implements it;
// tests pass a fake through WithGitHub.
type GitHubAPI interface {
	FetchRepo(ctx context.Context, repo github.Repo) (github.Snapshot, error)
	CloseIssue(ctx context.Context, repo github.Repo, number int, reason, comment string) error
	CloseMilestone(ctx context.Context, repo github.Repo, number int) error
	CreateMilestone(ctx context.Context, repo github.Repo, title, description string) (int, error)
	CreateIssue(ctx context.Context, repo github.Repo, in github.NewIssue) (number int, url string, err error)
}

// WithGitHub replaces how a sync gets its GitHub client. The default reads
// the token from GITHUB_TOKEN, GH_TOKEN or the GitHub CLI.
func WithGitHub(f func() (GitHubAPI, error)) Option { return func(s *Service) { s.newGitHub = f } }

// WithGitRemote replaces how a repository is found from a project's linked
// directory. The default reads the git origin remote.
func WithGitRemote(f func(dir string) (github.Repo, error)) Option {
	return func(s *Service) { s.gitRemote = f }
}

func defaultGitHub() (GitHubAPI, error) {
	c, err := github.NewFromEnv()
	if err != nil {
		return nil, &domain.ValidationError{Field: "github", Problem: "no GitHub token found", Hint: "Set GITHUB_TOKEN (or GH_TOKEN), or sign in with `gh auth login`, then sync again."}
	}
	return c, nil
}

// GitHubIssueKey is the idempotency key of the task that mirrors an issue.
func GitHubIssueKey(repo github.Repo, number int) string {
	return fmt.Sprintf("github:%s#%d", repo.Key(), number)
}

// GitHubIssueURL returns the issue URL a task was synced from, or "" when the
// task did not come from GitHub. It is derived from the idempotency key.
func GitHubIssueURL(key string) string {
	rest, ok := strings.CutPrefix(key, "github:")
	if !ok {
		return ""
	}
	repo, num, ok := strings.Cut(rest, "#")
	if !ok || !strings.Contains(repo, "/") {
		return ""
	}
	if _, err := strconv.Atoi(num); err != nil {
		return ""
	}
	return "https://github.com/" + repo + "/issues/" + num
}

// noMilestone is the Milestones key of the feature that holds issues without
// a milestone.
const (
	noMilestone      = "0"
	noMilestoneTitle = "No milestone"
	// boardBlockedReason is the blocked reason a sync sets from a Projects
	// board, so a later sync knows it may clear it.
	boardBlockedReason = "Marked blocked on the GitHub project board"
	// GitHubDocKind and GitHubDiscussionsTitle name the document that lists
	// the repository's discussions.
	GitHubDocKind          = "github"
	GitHubDiscussionsTitle = "Discussions"
)

// LinkGitHub links a project to a GitHub repository. An empty spec reads the
// origin remote of the project's linked directory. Relinking to another
// repository forgets the milestone mapping; tasks keep their keys.
func (s *Service) LinkGitHub(ctx context.Context, actor domain.Actor, ref ProjectRef, spec string) (domain.Project, github.Repo, error) {
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return domain.Project{}, github.Repo{}, err
	}
	if err := ensureProjectActive(p); err != nil {
		return domain.Project{}, github.Repo{}, err
	}
	repo, err := s.githubRepo(p, spec)
	if err != nil {
		return domain.Project{}, github.Repo{}, err
	}
	err = s.db.WriteTx(ctx, func(tx *sql.Tx) error {
		cur, err := store.GetProject(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		set := cur.Settings
		if set.GitHub != nil && strings.EqualFold(set.GitHub.Repo, repo.String()) {
			return nil
		}
		set.GitHub = &domain.GitHubSettings{Repo: repo.String()}
		p, err = store.UpdateProjectSettings(ctx, tx, s.meta(actor), p.ID, set)
		return err
	})
	if err != nil {
		return domain.Project{}, github.Repo{}, err
	}
	return p, repo, nil
}

// UnlinkGitHub removes a project's GitHub link. Synced tasks stay as they are.
func (s *Service) UnlinkGitHub(ctx context.Context, actor domain.Actor, ref ProjectRef) (domain.Project, error) {
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return domain.Project{}, err
	}
	if p.Settings.GitHub == nil {
		return p, nil
	}
	err = s.db.WriteTx(ctx, func(tx *sql.Tx) error {
		set := p.Settings
		set.GitHub = nil
		p, err = store.UpdateProjectSettings(ctx, tx, s.meta(actor), p.ID, set)
		return err
	})
	return p, err
}

// githubRepo picks the repository: spec, then the project's link, then the
// origin remote of its directory.
func (s *Service) githubRepo(p domain.Project, spec string) (github.Repo, error) {
	if spec = strings.TrimSpace(spec); spec != "" {
		r, err := github.ParseRepo(spec)
		if err != nil {
			return github.Repo{}, &domain.ValidationError{Field: "repo", Problem: err.Error(), Hint: "Pass the repository as owner/name."}
		}
		return r, nil
	}
	if p.Settings.GitHub != nil && p.Settings.GitHub.Repo != "" {
		return github.ParseRepo(p.Settings.GitHub.Repo)
	}
	if p.RepoPath != "" {
		if r, err := s.gitRemote(p.RepoPath); err == nil {
			return r, nil
		}
	}
	return github.Repo{}, &domain.ValidationError{
		Field:   "repo",
		Problem: fmt.Sprintf("project %q is not linked to a GitHub repository", p.Slug),
		Hint:    "Pass the repository as owner/name (skald github link owner/name), or link the project to a checkout whose origin is on GitHub.",
	}
}

// GitHubSyncOptions tunes SyncGitHub.
type GitHubSyncOptions struct {
	// Repo overrides the linked repository ("owner/name"). A real sync stores
	// it as the project's link.
	Repo string
	// Push closes GitHub issues whose tasks are done or cancelled in Skald,
	// then milestones whose features are done.
	Push bool
	// CreateIssues creates a GitHub issue for every task that has none (except
	// cancelled ones), and a milestone for every feature that has none. Done
	// tasks get their issue closed straight away when Push is also set.
	CreateIssues bool
	// DryRun reads GitHub and reports what would change without writing to
	// the database or to GitHub.
	DryRun bool
}

// GitHubSyncResult says what a sync did, or would do on a dry run.
type GitHubSyncResult struct {
	ProjectSlug string
	Repo        string
	DryRun      bool

	Milestones  int // read from GitHub
	Issues      int
	Discussions int

	FeaturesCreated int
	FeaturesUpdated int
	TasksCreated    int
	TasksUpdated    int
	EvidenceAdded   int
	DiscussionsDoc  bool // the discussions document got a new version

	StatusChanges []GitHubStatusChange
	// Skipped lists issues that could not be applied, with the reason; the
	// rest of the sync went ahead.
	Skipped []GitHubIssueNote
	// Diverged lists tasks finished in Skald whose issues are still open on
	// GitHub. A sync with Push closes them.
	Diverged []GitHubIssueNote
	// Pushed lists issues closed on GitHub (or that would be, on a dry run).
	Pushed []GitHubIssueNote
	// PushFailed lists issues the push could not close.
	PushFailed []GitHubIssueNote
	// MilestonesDiverged lists open milestones whose features are done in
	// Skald. A sync with Push closes them, once their open issues are closed.
	MilestonesDiverged []GitHubMilestoneNote
	// MilestonesPushed lists milestones closed on GitHub (or that would be, on
	// a dry run); MilestonesPushFailed those the push could not close.
	MilestonesPushed     []GitHubMilestoneNote
	MilestonesPushFailed []GitHubMilestoneNote
	// Created lists issues created for Skald tasks (Issue is 0 on a dry run),
	// CreateFailed the tasks whose issue could not be created.
	Created      []GitHubIssueNote
	CreateFailed []GitHubIssueNote
	// MilestonesCreated lists milestones created for Skald features.
	MilestonesCreated []GitHubMilestoneNote
	// Warnings are things the sync could not read; the rest went ahead.
	Warnings []string
}

// GitHubStatusChange is one task status change caused by GitHub.
type GitHubStatusChange struct {
	TaskID int64
	Issue  int
	From   domain.TaskStatus
	To     domain.TaskStatus
}

// GitHubMilestoneNote ties a milestone to its feature.
type GitHubMilestoneNote struct {
	Milestone int
	Title     string
	FeatureID int64
	Note      string

	openIssues []int // still open on GitHub when the sync read it
}

// GitHubIssueNote ties an issue to a task with a short explanation.
type GitHubIssueNote struct {
	Issue  int
	TaskID int64 // 0 when no task exists
	Note   string
}

// errDryRun rolls back a dry run's transaction.
var errDryRun = errors.New("dry run")

// SyncGitHub pulls a GitHub repository into the project, and with Push closes
// issues whose tasks were finished in Skald.
//
// Mapping:
//   - each milestone that has issues becomes a feature (issues without one go
//     to a "No milestone" feature); renames follow the milestone.
//   - each issue becomes a task with idempotency key
//     "github:<owner>/<name>#<number>", so re-running creates nothing new.
//     Title, description and feature follow GitHub; a priority label (P0..P3,
//     priority:high, ...) sets the priority.
//   - merged pull requests linked to close an issue become pr evidence.
//   - an issue closed as completed completes its task through the normal
//     state machine. Evidence is never forced: the merged pull requests, or a
//     link to the closed issue, are recorded first. An issue closed as not
//     planned cancels its task.
//   - an open issue with an open linked pull request, or "In progress" on a
//     Projects board, starts its task if its dependencies allow it.
//   - discussions are listed in the document github/Discussions.
//
// GitHub never reopens a task: a task finished in Skald whose issue is open is
// reported in Diverged, and Push closes that issue instead.
//
// Everything is read from GitHub before the single write transaction starts,
// and pushes happen after it commits. Each issue is applied under a
// savepoint, so one issue that breaks a rule is skipped, not fatal.
func (s *Service) SyncGitHub(ctx context.Context, actor domain.Actor, ref ProjectRef, o GitHubSyncOptions) (GitHubSyncResult, error) {
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return GitHubSyncResult{}, err
	}
	if err := ensureProjectActive(p); err != nil {
		return GitHubSyncResult{}, err
	}
	repo, err := s.githubRepo(p, o.Repo)
	if err != nil {
		return GitHubSyncResult{}, err
	}
	api, err := s.newGitHub()
	if err != nil {
		return GitHubSyncResult{}, err
	}
	snap, err := api.FetchRepo(ctx, repo)
	if err != nil {
		return GitHubSyncResult{}, githubErr(err)
	}

	var res GitHubSyncResult
	err = s.db.WriteTx(ctx, func(tx *sql.Tx) error {
		res = GitHubSyncResult{
			ProjectSlug: p.Slug, Repo: repo.String(), DryRun: o.DryRun,
			Milestones: len(snap.Milestones), Issues: len(snap.Issues), Discussions: len(snap.Discussions),
		}
		if snap.NoBoardStatus {
			res.Warnings = append(res.Warnings, "Projects board status was not read: the token lacks the read:project scope (gh auth refresh -s read:project).")
		}
		if err := s.applyGitHub(ctx, tx, s.meta(actor), p.ID, repo, snap, &res); err != nil {
			return err
		}
		if o.DryRun {
			return errDryRun
		}
		return nil
	})
	if err != nil && !errors.Is(err, errDryRun) {
		return GitHubSyncResult{}, err
	}

	if o.CreateIssues {
		if err := s.createGitHubIssues(ctx, actor, api, p.ID, repo, &res); err != nil {
			return res, err
		}
	}
	if o.Push {
		s.pushGitHub(ctx, actor, api, repo, &res)
	}
	return res, nil
}

// githubErr turns a GitHub failure into a validation error whose message says
// what to fix; the sync wrote nothing.
func githubErr(err error) error {
	var ae *github.APIError
	if errors.As(err, &ae) {
		return &domain.ValidationError{Field: "github", Problem: ae.Error(), Hint: "Nothing was changed. Fix the problem and sync again."}
	}
	return err
}

func (s *Service) applyGitHub(ctx context.Context, tx *sql.Tx, m store.Meta, projectID int64, repo github.Repo, snap github.Snapshot, res *GitHubSyncResult) error {
	project, err := store.GetProject(ctx, tx, projectID)
	if err != nil {
		return err
	}
	settings := project.Settings
	before, _ := json.Marshal(settings.GitHub)
	gh := &domain.GitHubSettings{Repo: repo.String(), Milestones: map[string]int64{}}
	if settings.GitHub != nil && strings.EqualFold(settings.GitHub.Repo, repo.String()) {
		for k, v := range settings.GitHub.Milestones {
			gh.Milestones[k] = v
		}
		if len(settings.GitHub.Issues) > 0 {
			gh.Issues = map[string]int{}
			for k, v := range settings.GitHub.Issues {
				gh.Issues[k] = v
			}
		}
	}
	// Issues Skald created: issue number -> task.
	linked := map[int]int64{}
	for k, n := range gh.Issues {
		if id, err := strconv.ParseInt(k, 10, 64); err == nil {
			linked[n] = id
		}
	}

	features, err := store.ListFeatures(ctx, tx, projectID)
	if err != nil {
		return err
	}
	featByID := map[int64]domain.Feature{}
	for _, f := range features {
		featByID[f.ID] = f
	}

	// Pass 1: a feature for every milestone that has issues.
	used := map[string]bool{}
	for _, is := range snap.Issues {
		used[strconv.Itoa(is.Milestone)] = true
	}
	type msInfo struct{ title, desc string }
	wanted := map[string]msInfo{}
	if used[noMilestone] {
		wanted[noMilestone] = msInfo{title: noMilestoneTitle, desc: "Issues from " + repo.String() + " that have no milestone."}
	}
	for _, ms := range snap.Milestones {
		k := strconv.Itoa(ms.Number)
		if used[k] {
			wanted[k] = msInfo{title: ms.Title, desc: ms.Description}
		}
	}
	keys := make([]string, 0, len(wanted))
	for k := range wanted {
		keys = append(keys, k)
	}
	// Milestones in number order, "No milestone" last.
	sort.Slice(keys, func(i, j int) bool {
		a, _ := strconv.Atoi(keys[i])
		b, _ := strconv.Atoi(keys[j])
		if a == 0 || b == 0 {
			return b == 0 && a != 0
		}
		return a < b
	})
	featureOf := map[string]int64{}
	for _, k := range keys {
		w := wanted[k]
		if id, ok := gh.Milestones[k]; ok {
			if f, ok := featByID[id]; ok && f.ProjectID == projectID && f.Status != domain.FeatureCancelled {
				patch := store.FeaturePatch{}
				if f.Title != w.title {
					patch.Title = &w.title
				}
				if f.Description != w.desc {
					patch.Description = &w.desc
				}
				if patch.Title != nil || patch.Description != nil {
					if _, err := store.UpdateFeature(ctx, tx, m, f.ID, patch); err != nil {
						return err
					}
					res.FeaturesUpdated++
				}
				featureOf[k] = f.ID
				continue
			}
		}
		f, err := store.CreateFeature(ctx, tx, m, domain.Feature{ProjectID: projectID, Title: w.title, Description: w.desc})
		if err != nil {
			return err
		}
		res.FeaturesCreated++
		gh.Milestones[k] = f.ID
		featureOf[k] = f.ID
	}

	// Pass 2: issues, each under its own savepoint.
	for _, is := range snap.Issues {
		if _, err := tx.ExecContext(ctx, `SAVEPOINT gh_issue`); err != nil {
			return err
		}
		var (
			out    GitHubSyncResult
			taskID int64
		)
		err := s.applyIssue(ctx, tx, m, project, repo, is, featureOf[strconv.Itoa(is.Milestone)], linked[is.Number], &out, &taskID)
		if err != nil {
			if _, rerr := tx.ExecContext(ctx, `ROLLBACK TO gh_issue`); rerr != nil {
				return rerr
			}
			var de domain.Error
			if !errors.As(err, &de) {
				return err
			}
			res.Skipped = append(res.Skipped, GitHubIssueNote{Issue: is.Number, TaskID: taskID, Note: de.Message()})
		} else {
			res.TasksCreated += out.TasksCreated
			res.TasksUpdated += out.TasksUpdated
			res.EvidenceAdded += out.EvidenceAdded
			res.StatusChanges = append(res.StatusChanges, out.StatusChanges...)
			res.Diverged = append(res.Diverged, out.Diverged...)
		}
		if _, err := tx.ExecContext(ctx, `RELEASE gh_issue`); err != nil {
			return err
		}
	}

	// Open milestones whose features are now done here.
	open := map[int][]int{}
	for _, is := range snap.Issues {
		if !is.Closed() {
			open[is.Milestone] = append(open[is.Milestone], is.Number)
		}
	}
	for _, ms := range snap.Milestones {
		fid, ok := featureOf[strconv.Itoa(ms.Number)]
		if !ok || strings.EqualFold(ms.State, "CLOSED") {
			continue
		}
		f, err := store.GetFeature(ctx, tx, fid)
		if err != nil {
			return err
		}
		tasks, err := store.ListTasks(ctx, tx, store.TaskFilter{ProjectID: projectID, FeatureID: fid})
		if err != nil {
			return err
		}
		if domain.DeriveFeatureStatus(f.Status, tasks) == domain.FeatureDone {
			res.MilestonesDiverged = append(res.MilestonesDiverged, GitHubMilestoneNote{
				Milestone: ms.Number, Title: ms.Title, FeatureID: fid, Note: "done here, open on GitHub", openIssues: open[ms.Number],
			})
		}
	}

	// Discussions: one document listing them, versioned only when it changes.
	if body := discussionsBody(repo, snap.Discussions); body != "" {
		_, cur, err := store.GetDocument(ctx, tx, projectID, GitHubDocKind, GitHubDiscussionsTitle)
		if err != nil && domain.KindOf(err) != domain.KindNotFound {
			return err
		}
		if err != nil || cur.Body != body {
			if _, _, err := store.PutDocument(ctx, tx, m, projectID, GitHubDocKind, GitHubDiscussionsTitle, body); err != nil {
				return err
			}
			res.DiscussionsDoc = true
		}
	}

	after, _ := json.Marshal(gh)
	if string(before) != string(after) {
		settings.GitHub = gh
		if _, err := store.UpdateProjectSettings(ctx, tx, m, projectID, settings); err != nil {
			return err
		}
	}
	return nil
}

// applyIssue mirrors one issue onto its task. It sets *taskID as soon as the
// task is known, so a skipped issue can still name its task.
//
// linkedTask is the task Skald created this issue for (0 when none); it is
// used instead of looking the task up by key.
func (s *Service) applyIssue(ctx context.Context, tx *sql.Tx, m store.Meta, project domain.Project, repo github.Repo, is github.Issue, featureID, linkedTask int64, out *GitHubSyncResult, taskID *int64) error {
	title := fmt.Sprintf("#%d %s", is.Number, strings.TrimSpace(is.Title))
	desc := strings.TrimSpace(skaldMarker.ReplaceAllString(is.Body, ""))
	prio := priorityFromLabels(is.Labels)
	var (
		task    domain.Task
		created bool
		err     error
	)
	if linkedTask != 0 {
		task, err = store.GetTaskInProject(ctx, tx, project.ID, linkedTask)
	}
	if linkedTask == 0 || domain.KindOf(err) == domain.KindNotFound {
		task, created, err = store.CreateTask(ctx, tx, m, store.TaskInput{
			ProjectID: project.ID, FeatureID: featureID, Title: title, Description: desc,
			Priority: prio, IdempotencyKey: GitHubIssueKey(repo, is.Number),
		})
	}
	if err != nil {
		return err
	}
	if created {
		out.TasksCreated++
	} else {
		patch := store.TaskPatch{}
		if task.Title != title {
			patch.Title = &title
		}
		if task.Description != desc {
			patch.Description = &desc
		}
		if prio != nil && *prio != task.Priority {
			patch.Priority = prio
		}
		if task.FeatureID != featureID {
			patch.FeatureID = &featureID
		}
		if patch != (store.TaskPatch{}) {
			if task, err = store.UpdateTaskFields(ctx, tx, m, task.ID, 0, patch); err != nil {
				return err
			}
			out.TasksUpdated++
		}
	}

	*taskID = task.ID

	// Merged pull requests that close the issue are evidence.
	if err := s.addPREvidence(ctx, tx, m, task, is, out); err != nil {
		return err
	}

	move := func(to domain.TaskStatus, reason, note string) error {
		from := task.Status
		t, err := s.transitionTx(ctx, tx, m, task.ID, StatusChange{To: to, BlockedReason: reason, Note: note})
		if err != nil {
			return err
		}
		task = t
		out.StatusChanges = append(out.StatusChanges, GitHubStatusChange{TaskID: task.ID, Issue: is.Number, From: from, To: to})
		return nil
	}
	ref := fmt.Sprintf("%s#%d", repo, is.Number)

	switch {
	case is.ClosedAsCompleted():
		if task.Status == domain.TaskDone {
			return nil
		}
		if task.Status == domain.TaskCancelled {
			if err := move(domain.TaskTodo, "", "Reopened: "+ref+" was closed as completed on GitHub"); err != nil {
				return err
			}
			// Evidence is refused on cancelled tasks; add it now.
			if err := s.addPREvidence(ctx, tx, m, task, is, out); err != nil {
				return err
			}
		}
		if task.Status != domain.TaskInProgress {
			if err := move(domain.TaskInProgress, "", "Started: "+ref+" was closed on GitHub"); err != nil {
				return err
			}
		}
		n, err := store.CountArtifacts(ctx, tx, task.ID)
		if err != nil {
			return err
		}
		if n == 0 {
			// No merged pull request: the closed issue itself is the record.
			if _, err := s.recordArtifactTx(ctx, tx, m, task.ID, ArtifactInput{
				Kind: domain.ArtifactLink, Ref: is.URL, Summary: "Issue closed as completed on GitHub",
			}); err != nil {
				return err
			}
			out.EvidenceAdded++
		}
		return move(domain.TaskDone, "", "Closed as completed on GitHub ("+ref+")")

	case is.Closed():
		if task.Status.Terminal() {
			return nil
		}
		return move(domain.TaskCancelled, "", fmt.Sprintf("Closed as %s on GitHub (%s)", strings.ToLower(strings.ReplaceAll(is.StateReason, "_", " ")), ref))
	}

	// Open on GitHub.
	switch task.Status {
	case domain.TaskDone, domain.TaskCancelled:
		out.Diverged = append(out.Diverged, GitHubIssueNote{Issue: is.Number, TaskID: task.ID, Note: fmt.Sprintf("%s here, open on GitHub", task.Status)})
		return nil
	}
	board := boardState(is.BoardStatus)
	switch {
	case board == domain.TaskBlocked:
		if task.Status == domain.TaskBlocked {
			return nil
		}
		return move(domain.TaskBlocked, boardBlockedReason, "")
	case task.Status == domain.TaskBlocked:
		// Only clear a blocker the board set; a blocker someone reported in
		// Skald stays until they clear it.
		if task.BlockedReason != boardBlockedReason {
			return nil
		}
		to := domain.TaskTodo
		if task.StartedAt != nil || board == domain.TaskInProgress || hasOpenPR(is) {
			to = domain.TaskInProgress
		}
		return move(to, "", "Unblocked on the GitHub project board")
	case task.Status == domain.TaskTodo && (board == domain.TaskInProgress || hasOpenPR(is)):
		deps, err := store.ListDependencyTasks(ctx, tx, task.ID)
		if err != nil {
			return err
		}
		if len(domain.UnmetDependencies(deps)) > 0 {
			return nil // waiting on prerequisites; leave it in todo
		}
		why := "in progress on the GitHub project board"
		if hasOpenPR(is) {
			why = "a pull request is open for it on GitHub"
		}
		return move(domain.TaskInProgress, "", "Started: "+why)
	}
	return nil
}

func (s *Service) addPREvidence(ctx context.Context, tx *sql.Tx, m store.Meta, task domain.Task, is github.Issue, out *GitHubSyncResult) error {
	if task.Status == domain.TaskCancelled {
		return nil
	}
	var have map[string]bool
	for _, pr := range is.PullRequests {
		if !pr.Merged() || pr.URL == "" {
			continue
		}
		if have == nil {
			arts, err := store.ListArtifacts(ctx, tx, task.ID)
			if err != nil {
				return err
			}
			have = map[string]bool{}
			for _, a := range arts {
				have[a.Ref] = true
			}
		}
		if have[pr.URL] {
			continue
		}
		summary := fmt.Sprintf("PR #%d merged: %s", pr.Number, pr.Title)
		if pr.MergeCommit != "" {
			summary += " (" + shortSHA(pr.MergeCommit) + ")"
		}
		if _, err := s.recordArtifactTx(ctx, tx, m, task.ID, ArtifactInput{Kind: domain.ArtifactPR, Ref: pr.URL, Summary: summary}); err != nil {
			return err
		}
		have[pr.URL] = true
		out.EvidenceAdded++
	}
	return nil
}

// skaldMarker is the hidden trailer Skald puts on the issues it creates. A
// pull strips it so it never lands in the task's description.
var skaldMarker = regexp.MustCompile(`\s*<!-- skald:T-\d+ -->\s*$`)

func skaldMarkerFor(taskID int64) string {
	return fmt.Sprintf("<!-- skald:%s -->", domain.FormatID(domain.EntityTask, taskID))
}

// createGitHubIssues creates an issue for every task with no GitHub issue,
// and a milestone for every feature with none. Each link is saved as soon as
// GitHub returns it, so a failure part-way never leads to duplicates.
func (s *Service) createGitHubIssues(ctx context.Context, actor domain.Actor, api GitHubAPI, projectID int64, repo github.Repo, res *GitHubSyncResult) error {
	var (
		project  domain.Project
		features []domain.Feature
		tasks    []domain.Task
	)
	err := s.db.ReadTx(ctx, func(tx *sql.Tx) (err error) {
		if project, err = store.GetProject(ctx, tx, projectID); err != nil {
			return err
		}
		if features, err = store.ListFeatures(ctx, tx, projectID); err != nil {
			return err
		}
		tasks, err = store.ListTasks(ctx, tx, store.TaskFilter{ProjectID: projectID})
		return err
	})
	if err != nil {
		return err
	}
	gh := project.Settings.GitHub
	if gh == nil || !strings.EqualFold(gh.Repo, repo.String()) {
		// A dry run rolled the link back; nothing is linked yet.
		gh = &domain.GitHubSettings{Repo: repo.String()}
	}
	milestoneOf := map[int64]int{} // feature -> milestone number
	for k, fid := range gh.Milestones {
		n, _ := strconv.Atoi(k)
		milestoneOf[fid] = n
	}
	byFeature := map[int64][]domain.Task{}
	for _, t := range tasks {
		byFeature[t.FeatureID] = append(byFeature[t.FeatureID], t)
	}

	for _, f := range features {
		if f.Status == domain.FeatureCancelled {
			continue
		}
		var todo []domain.Task
		for _, t := range byFeature[f.ID] {
			// Tasks synced from any GitHub repository already have an issue.
			if t.Status == domain.TaskCancelled || strings.HasPrefix(t.IdempotencyKey, "github:") || gh.Issues[strconv.FormatInt(t.ID, 10)] != 0 {
				continue
			}
			todo = append(todo, t)
		}
		if len(todo) == 0 {
			continue
		}
		ms, mapped := milestoneOf[f.ID]
		if !mapped {
			note := GitHubMilestoneNote{Title: f.Title, FeatureID: f.ID}
			if res.DryRun {
				note.Note = "would be created"
				res.MilestonesCreated = append(res.MilestonesCreated, note)
			} else {
				n, err := api.CreateMilestone(ctx, repo, f.Title, f.Description)
				if err != nil {
					// Without the milestone the feature's tasks would land in
					// none; report them and move on.
					for _, t := range todo {
						res.CreateFailed = append(res.CreateFailed, GitHubIssueNote{TaskID: t.ID, Note: "milestone for " + f.Title + ": " + githubErr(err).Error()})
					}
					continue
				}
				if err := s.saveGitHubLink(ctx, actor, projectID, repo, func(g *domain.GitHubSettings) { g.Milestones[strconv.Itoa(n)] = f.ID }); err != nil {
					return err
				}
				ms, note.Milestone, note.Note = n, n, "created"
				res.MilestonesCreated = append(res.MilestonesCreated, note)
			}
		}
		for _, t := range todo {
			in := github.NewIssue{Title: t.Title, Body: strings.TrimSpace(t.Description + "\n\n" + skaldMarkerFor(t.ID)), Milestone: ms}
			if t.Priority != 2 {
				in.Labels = []string{fmt.Sprintf("P%d", t.Priority)}
			}
			if res.DryRun {
				res.Created = append(res.Created, GitHubIssueNote{TaskID: t.ID, Note: "would be created"})
				continue
			}
			n, url, err := api.CreateIssue(ctx, repo, in)
			if err != nil {
				res.CreateFailed = append(res.CreateFailed, GitHubIssueNote{TaskID: t.ID, Note: githubErr(err).Error()})
				continue
			}
			if err := s.saveGitHubLink(ctx, actor, projectID, repo, func(g *domain.GitHubSettings) { g.Issues[strconv.FormatInt(t.ID, 10)] = n }); err != nil {
				return err
			}
			_ = s.db.WriteTx(ctx, func(tx *sql.Tx) error {
				_, err := store.AddTaskNote(ctx, tx, s.meta(actor), t.ID, fmt.Sprintf("Created %s#%d on GitHub: %s", repo, n, url))
				return err
			})
			res.Created = append(res.Created, GitHubIssueNote{Issue: n, TaskID: t.ID, Note: "created"})
			if t.Status == domain.TaskDone {
				res.Diverged = append(res.Diverged, GitHubIssueNote{Issue: n, TaskID: t.ID, Note: "done here, open on GitHub"})
			}
		}
	}
	return nil
}

// saveGitHubLink records part of the project's GitHub link in its own short
// write transaction.
func (s *Service) saveGitHubLink(ctx context.Context, actor domain.Actor, projectID int64, repo github.Repo, fn func(*domain.GitHubSettings)) error {
	return s.db.WriteTx(ctx, func(tx *sql.Tx) error {
		p, err := store.GetProject(ctx, tx, projectID)
		if err != nil {
			return err
		}
		set := p.Settings
		if set.GitHub == nil || !strings.EqualFold(set.GitHub.Repo, repo.String()) {
			set.GitHub = &domain.GitHubSettings{Repo: repo.String()}
		} else {
			cp := *set.GitHub
			set.GitHub = &cp
		}
		g := set.GitHub
		ms, is := map[string]int64{}, map[string]int{}
		for k, v := range g.Milestones {
			ms[k] = v
		}
		for k, v := range g.Issues {
			is[k] = v
		}
		g.Milestones, g.Issues = ms, is
		fn(g)
		_, err = store.UpdateProjectSettings(ctx, tx, s.meta(actor), projectID, set)
		return err
	})
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func hasOpenPR(is github.Issue) bool {
	for _, pr := range is.PullRequests {
		if pr.Open() {
			return true
		}
	}
	return false
}

// boardState reads Projects board Status values: anything mentioning
// "block" is blocked, "progress", "doing" or "review" is in progress.
// Anything else (Todo, Backlog, Done, ...) returns "".
func boardState(values []string) domain.TaskStatus {
	var st domain.TaskStatus
	for _, v := range values {
		v = strings.ToLower(v)
		switch {
		case strings.Contains(v, "block"):
			return domain.TaskBlocked
		case strings.Contains(v, "progress"), strings.Contains(v, "doing"), strings.Contains(v, "review"):
			st = domain.TaskInProgress
		}
	}
	return st
}

var (
	labelP    = regexp.MustCompile(`^(?:priority[:/ _-]*)?p([0-3])$`)
	labelName = regexp.MustCompile(`^priority[:/ _-]*(urgent|critical|high|medium|normal|low)$`)
)

// priorityFromLabels maps P0..P3 and priority:<name> labels to a priority.
// nil means no priority label.
func priorityFromLabels(labels []string) *int {
	for _, l := range labels {
		l = strings.ToLower(strings.TrimSpace(l))
		if m := labelP.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[1])
			return &n
		}
		if m := labelName.FindStringSubmatch(l); m != nil {
			n := map[string]int{"urgent": 0, "critical": 0, "high": 1, "medium": 2, "normal": 2, "low": 3}[m[1]]
			return &n
		}
	}
	return nil
}

func discussionsBody(repo github.Repo, ds []github.Discussion) string {
	if len(ds) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Discussions in %s, synced from GitHub. Read-only: edit them on GitHub.\n\n", repo)
	for _, d := range ds {
		state := "open"
		switch {
		case d.Closed:
			state = "closed"
		case d.Answered:
			state = "answered"
		}
		cat := ""
		if d.Category != "" {
			cat = " [" + d.Category + "]"
		}
		fmt.Fprintf(&b, "- #%d %s%s (%s) %s\n", d.Number, d.Title, cat, state, d.URL)
	}
	return b.String()
}

// pushGitHub closes the issues of tasks finished in Skald. It runs after the
// sync committed; each closed issue gets a note on its task.
func (s *Service) pushGitHub(ctx context.Context, actor domain.Actor, api GitHubAPI, repo github.Repo, res *GitHubSyncResult) {
	diverged := res.Diverged
	res.Diverged = nil
	for _, d := range diverged {
		var (
			task domain.Task
			arts []domain.Artifact
		)
		err := s.db.ReadTx(ctx, func(tx *sql.Tx) (err error) {
			if task, err = store.GetTask(ctx, tx, d.TaskID); err != nil {
				return err
			}
			arts, err = store.ListArtifacts(ctx, tx, d.TaskID)
			return err
		})
		if err != nil {
			res.PushFailed = append(res.PushFailed, GitHubIssueNote{Issue: d.Issue, TaskID: d.TaskID, Note: err.Error()})
			res.Diverged = append(res.Diverged, d)
			continue
		}
		reason, comment := "completed", fmt.Sprintf("Completed in Skald as %s.", domain.FormatID(domain.EntityTask, task.ID))
		if task.Status == domain.TaskCancelled {
			reason, comment = "not_planned", fmt.Sprintf("Cancelled in Skald (%s).", domain.FormatID(domain.EntityTask, task.ID))
		} else if len(arts) > 0 {
			comment += "\n\nEvidence:\n"
			for _, a := range arts {
				line := fmt.Sprintf("- %s: %s", a.Kind, a.Ref)
				if a.Summary != "" {
					line += " (" + a.Summary + ")"
				}
				comment += line + "\n"
			}
		}
		note := GitHubIssueNote{Issue: d.Issue, TaskID: d.TaskID, Note: "closed as " + strings.ReplaceAll(reason, "_", " ")}
		if res.DryRun {
			note.Note = "would be " + note.Note
			res.Pushed = append(res.Pushed, note)
			continue
		}
		if err := api.CloseIssue(ctx, repo, d.Issue, reason, comment); err != nil {
			res.PushFailed = append(res.PushFailed, GitHubIssueNote{Issue: d.Issue, TaskID: d.TaskID, Note: githubErr(err).Error()})
			res.Diverged = append(res.Diverged, d)
			continue
		}
		res.Pushed = append(res.Pushed, note)
		_ = s.db.WriteTx(ctx, func(tx *sql.Tx) error {
			_, err := store.AddTaskNote(ctx, tx, s.meta(actor), d.TaskID, fmt.Sprintf("Closed %s#%d on GitHub as %s", repo, d.Issue, strings.ReplaceAll(reason, "_", " ")))
			return err
		})
	}

	// Milestones last, and only once none of their issues is left open.
	closed := map[int]bool{}
	for _, p := range res.Pushed {
		closed[p.Issue] = true
	}
	milestones := res.MilestonesDiverged
	res.MilestonesDiverged = nil
	for _, ms := range milestones {
		var left []string
		for _, n := range ms.openIssues {
			if !closed[n] {
				left = append(left, fmt.Sprintf("#%d", n))
			}
		}
		if len(left) > 0 {
			ms.Note = "not closed: " + strings.Join(left, ", ") + " still open on GitHub"
			res.MilestonesDiverged = append(res.MilestonesDiverged, ms)
			continue
		}
		if res.DryRun {
			ms.Note = "would be closed"
			res.MilestonesPushed = append(res.MilestonesPushed, ms)
			continue
		}
		if err := api.CloseMilestone(ctx, repo, ms.Milestone); err != nil {
			ms.Note = githubErr(err).Error()
			res.MilestonesPushFailed = append(res.MilestonesPushFailed, ms)
			continue
		}
		ms.Note = "closed"
		res.MilestonesPushed = append(res.MilestonesPushed, ms)
	}
}
