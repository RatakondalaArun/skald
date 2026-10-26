package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"skald/internal/service"
)

const githubSyncHelp = `Pulls a GitHub repository into the project:

  milestone              -> feature (issues without one go to "No milestone")
  issue                  -> task, keyed github:<owner>/<name>#<number>
  merged linked PR       -> pr evidence on the issue's task
  issue closed           -> task done (as completed) or cancelled (not planned)
  open PR or board "In progress" -> task started
  board "Blocked"        -> task blocked
  P0..P3 / priority:<x>  -> task priority
  discussions            -> the document github/Discussions (links only)

Re-running is safe: nothing is created twice, and a sync with no changes on
GitHub writes nothing. Titles, descriptions and features of synced tasks follow
GitHub. Completing a task still goes through the evidence gate: the merged PRs,
or a link to the closed issue, are recorded first.

GitHub never reopens a task. A task finished in Skald whose issue is still open
is reported; --push closes that issue on GitHub with a comment listing the
evidence (as not planned for a cancelled task). It then closes each open
milestone whose feature is done here, once none of its issues is left open.

--create-issues pushes work that started in Skald: every task without a GitHub
issue (cancelled ones aside) gets one, and every feature without a milestone
gets one. Each link is saved as soon as GitHub returns it, so re-running never
duplicates, and the next pull keeps those tasks as they are. With --push too,
the new issues of tasks already done are closed with their evidence.

The repository is --repo, else the project's link (skald github link), else the
origin remote of the project's directory. The token comes from GITHUB_TOKEN,
GH_TOKEN, skald github login, or the GitHub CLI (gh auth login). For GitHub
Enterprise set SKALD_GITHUB_API_URL=https://<host>/api/v3.`

func newGitHubCmd(a *app) *cobra.Command {
	var unlink bool
	link := &cobra.Command{
		Use:   "link [owner/name]",
		Short: "Link the project to a GitHub repository",
		Long: `Links the project to a GitHub repository. Without an argument the origin
remote of the project's directory is used. --unlink removes the link; synced
tasks stay as they are.`,
		Example: "  skald github link acme/rocket\n  skald github link --unlink",
		Args:    rangeArgs(0, 1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			if unlink {
				if len(args) > 0 {
					return usagef("--unlink takes no repository")
				}
				p, err := e.svc.UnlinkGitHub(e.ctx, e.actor, e.ref)
				if err != nil {
					return err
				}
				return e.emit(map[string]any{"project": p.Slug, "repo": nil}, func(w io.Writer) {
					fmt.Fprintf(w, "%s is no longer linked to GitHub\n", p.Slug)
				})
			}
			spec := ""
			if len(args) == 1 {
				spec = args[0]
			}
			p, repo, err := e.svc.LinkGitHub(e.ctx, e.actor, e.ref, spec)
			if err != nil {
				return err
			}
			return e.emit(map[string]any{"project": p.Slug, "repo": repo.String()}, func(w io.Writer) {
				fmt.Fprintf(w, "%s linked to github.com/%s; run `skald github sync` to pull it\n", p.Slug, repo)
			})
		}),
	}
	link.Flags().BoolVar(&unlink, "unlink", false, "remove the link")

	var o service.GitHubSyncOptions
	sync := &cobra.Command{
		Use:     "sync",
		Short:   "Pull milestones, issues, PRs and discussions from GitHub",
		Long:    githubSyncHelp,
		Example: "  skald github sync --dry-run\n  skald github sync --repo acme/rocket\n  skald github sync --push",
		Args:    noArgs,
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			res, err := e.svc.SyncGitHub(e.ctx, e.actor, e.ref, o)
			if err != nil {
				return err
			}
			return e.emit(toGitHubSync(res), func(w io.Writer) { printGitHubSync(w, res) })
		}),
	}
	sync.Flags().StringVar(&o.Repo, "repo", "", "owner/name (default: the project's link, then the git origin)")
	sync.Flags().BoolVar(&o.Push, "push", false, "close GitHub issues whose tasks are done or cancelled here, and milestones whose features are done")
	sync.Flags().BoolVar(&o.CreateIssues, "create-issues", false, "create GitHub issues and milestones for tasks and features that have none")
	sync.Flags().BoolVar(&o.DryRun, "dry-run", false, "show what would change without writing anything")

	return group("github", "Sync with GitHub issues, milestones, pull requests and discussions",
		newGitHubLoginCmd(a), newGitHubLogoutCmd(a), newGitHubAuthStatusCmd(a), link, sync)
}

type githubNoteJSON struct {
	Issue int    `json:"issue"`
	Task  string `json:"task,omitempty"`
	Note  string `json:"note"`
}

type githubChangeJSON struct {
	Issue int    `json:"issue"`
	Task  string `json:"task"`
	From  string `json:"from"`
	To    string `json:"to"`
}

type githubSyncJSON struct {
	Project         string                `json:"project"`
	Repo            string                `json:"repo"`
	DryRun          bool                  `json:"dry_run"`
	Milestones      int                   `json:"milestones"`
	Issues          int                   `json:"issues"`
	Discussions     int                   `json:"discussions"`
	FeaturesCreated int                   `json:"features_created"`
	FeaturesUpdated int                   `json:"features_updated"`
	TasksCreated    int                   `json:"tasks_created"`
	TasksUpdated    int                   `json:"tasks_updated"`
	EvidenceAdded   int                   `json:"evidence_added"`
	DiscussionsDoc  bool                  `json:"discussions_updated"`
	StatusChanges   []githubChangeJSON    `json:"status_changes"`
	Skipped         []githubNoteJSON      `json:"skipped"`
	Diverged        []githubNoteJSON      `json:"diverged"`
	Pushed          []githubNoteJSON      `json:"pushed"`
	PushFailed      []githubNoteJSON      `json:"push_failed"`
	Warnings        []string              `json:"warnings"`
	Created         []githubNoteJSON      `json:"created"`
	CreateFailed    []githubNoteJSON      `json:"create_failed"`
	MilestonesNew   []githubMilestoneJSON `json:"milestones_created"`
	MilestonePush   struct {
		Diverged   []githubMilestoneJSON `json:"diverged"`
		Pushed     []githubMilestoneJSON `json:"pushed"`
		PushFailed []githubMilestoneJSON `json:"push_failed"`
	} `json:"milestone_push"`
}

type githubMilestoneJSON struct {
	Milestone int    `json:"milestone"`
	Title     string `json:"title"`
	Feature   string `json:"feature"`
	Note      string `json:"note"`
}

func toGitHubMilestones(ms []service.GitHubMilestoneNote) []githubMilestoneJSON {
	out := make([]githubMilestoneJSON, len(ms))
	for i, m := range ms {
		out[i] = githubMilestoneJSON{Milestone: m.Milestone, Title: m.Title, Feature: featureID(m.FeatureID), Note: m.Note}
	}
	return out
}

func toGitHubNotes(ns []service.GitHubIssueNote) []githubNoteJSON {
	out := make([]githubNoteJSON, len(ns))
	for i, n := range ns {
		out[i] = githubNoteJSON{Issue: n.Issue, Note: n.Note}
		if n.TaskID != 0 {
			out[i].Task = taskID(n.TaskID)
		}
	}
	return out
}

func toGitHubSync(r service.GitHubSyncResult) githubSyncJSON {
	j := githubSyncJSON{
		Project: r.ProjectSlug, Repo: r.Repo, DryRun: r.DryRun,
		Milestones: r.Milestones, Issues: r.Issues, Discussions: r.Discussions,
		FeaturesCreated: r.FeaturesCreated, FeaturesUpdated: r.FeaturesUpdated,
		TasksCreated: r.TasksCreated, TasksUpdated: r.TasksUpdated, EvidenceAdded: r.EvidenceAdded,
		DiscussionsDoc: r.DiscussionsDoc, StatusChanges: make([]githubChangeJSON, len(r.StatusChanges)),
		Skipped: toGitHubNotes(r.Skipped), Diverged: toGitHubNotes(r.Diverged),
		Pushed: toGitHubNotes(r.Pushed), PushFailed: toGitHubNotes(r.PushFailed),
		Warnings: append([]string{}, r.Warnings...),
	}
	j.Created, j.CreateFailed = toGitHubNotes(r.Created), toGitHubNotes(r.CreateFailed)
	j.MilestonesNew = toGitHubMilestones(r.MilestonesCreated)
	j.MilestonePush.Diverged = toGitHubMilestones(r.MilestonesDiverged)
	j.MilestonePush.Pushed = toGitHubMilestones(r.MilestonesPushed)
	j.MilestonePush.PushFailed = toGitHubMilestones(r.MilestonesPushFailed)
	for i, c := range r.StatusChanges {
		j.StatusChanges[i] = githubChangeJSON{Issue: c.Issue, Task: taskID(c.TaskID), From: string(c.From), To: string(c.To)}
	}
	return j
}

func printGitHubSync(w io.Writer, r service.GitHubSyncResult) {
	verb := "synced"
	if r.DryRun {
		verb = "dry run, nothing written:"
	}
	fmt.Fprintf(w, "%s %s from github.com/%s: %d milestones, %d issues, %d discussions\n",
		r.ProjectSlug, verb, r.Repo, r.Milestones, r.Issues, r.Discussions)
	fmt.Fprintf(w, "features: %d new, %d updated; tasks: %d new, %d updated; evidence added: %d\n",
		r.FeaturesCreated, r.FeaturesUpdated, r.TasksCreated, r.TasksUpdated, r.EvidenceAdded)
	if len(r.StatusChanges) > 0 {
		t := table(w)
		fmt.Fprintln(t, "ISSUE\tTASK\tSTATUS\t")
		for _, c := range r.StatusChanges {
			fmt.Fprintf(t, "#%d\t%s\t%s -> %s\t\n", c.Issue, taskID(c.TaskID), c.From, c.To)
		}
		t.Flush()
	}
	notes := func(title string, ns []service.GitHubIssueNote) {
		if len(ns) == 0 {
			return
		}
		fmt.Fprintln(w, title)
		for _, n := range ns {
			task := ""
			if n.TaskID != 0 {
				task = " " + taskID(n.TaskID)
			}
			fmt.Fprintf(w, "  #%d%s: %s\n", n.Issue, task, n.Note)
		}
	}
	for _, msg := range r.Warnings {
		fmt.Fprintln(w, "warning: "+msg)
	}
	notes("skipped:", r.Skipped)
	if len(r.Created) > 0 {
		fmt.Fprintln(w, "issues created on GitHub:")
		for _, n := range r.Created {
			if n.Issue == 0 {
				fmt.Fprintf(w, "  %s: %s\n", taskID(n.TaskID), n.Note)
			} else {
				fmt.Fprintf(w, "  %s -> #%d\n", taskID(n.TaskID), n.Issue)
			}
		}
	}
	notes("issue creation failed:", r.CreateFailed)
	notes("pushed to GitHub:", r.Pushed)
	notes("push failed:", r.PushFailed)
	if len(r.Diverged) > 0 {
		notes("finished here but open on GitHub (sync with --push to close them):", r.Diverged)
	}
	milestones := func(title string, ms []service.GitHubMilestoneNote) {
		if len(ms) == 0 {
			return
		}
		fmt.Fprintln(w, title)
		for _, m := range ms {
			fmt.Fprintf(w, "  milestone %d %q (%s): %s\n", m.Milestone, m.Title, featureID(m.FeatureID), m.Note)
		}
	}
	milestones("milestones created on GitHub:", r.MilestonesCreated)
	milestones("milestones pushed to GitHub:", r.MilestonesPushed)
	milestones("milestone push failed:", r.MilestonesPushFailed)
	milestones("milestones done here but open on GitHub:", r.MilestonesDiverged)
}
