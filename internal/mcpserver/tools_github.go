package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"skald/internal/domain"
	"skald/internal/service"
)

type syncGitHubArgs struct {
	projectArg
	Repo   string `json:"repo,omitempty" jsonschema:"owner/name; omit to use the project's link or the repo's git origin"`
	Push   bool   `json:"push,omitempty" jsonschema:"also close GitHub issues whose tasks are done or cancelled here, and milestones whose features are done"`
	DryRun bool   `json:"dry_run,omitempty" jsonschema:"report what would change without writing anything"`
}

func (s *server) registerGitHub(srv *mcp.Server) {
	addTool(srv, s, "sync_github",
		"Pull GitHub milestones (features), issues (tasks), merged PRs (evidence) and discussions into the project. Safe to repeat. Closed issues complete tasks through the evidence gate. push closes issues finished here and milestones whose features are done. Next: get_project_progress.",
		false, func(ctx context.Context, actor domain.Actor, in syncGitHubArgs) (any, error) {
			res, err := s.svc.SyncGitHub(ctx, actor, s.ref(in.Project), service.GitHubSyncOptions{Repo: in.Repo, Push: in.Push, DryRun: in.DryRun})
			if err != nil {
				return nil, err
			}
			o := obj{
				"project": res.ProjectSlug, "repo": res.Repo, "issues": res.Issues, "milestones": res.Milestones,
				"features_created": res.FeaturesCreated, "tasks_created": res.TasksCreated, "tasks_updated": res.TasksUpdated,
				"evidence_added": res.EvidenceAdded,
			}
			if len(res.Warnings) > 0 {
				o["warnings"] = res.Warnings
			}
			if res.DryRun {
				o["dry_run"] = true
			}
			if len(res.StatusChanges) > 0 {
				ch := make([]obj, len(res.StatusChanges))
				for i, c := range res.StatusChanges {
					ch[i] = obj{"task": tid(c.TaskID), "issue": c.Issue, "to": string(c.To)}
				}
				o["status_changes"] = ch
			}
			for key, ns := range map[string][]service.GitHubIssueNote{
				"skipped": res.Skipped, "diverged": res.Diverged, "pushed": res.Pushed, "push_failed": res.PushFailed,
			} {
				if len(ns) == 0 {
					continue
				}
				out := make([]obj, len(ns))
				for i, n := range ns {
					out[i] = obj{"issue": n.Issue, "note": n.Note}
					if n.TaskID != 0 {
						out[i]["task"] = tid(n.TaskID)
					}
				}
				o[key] = out
			}
			for key, ms := range map[string][]service.GitHubMilestoneNote{
				"milestones_diverged": res.MilestonesDiverged, "milestones_pushed": res.MilestonesPushed, "milestones_push_failed": res.MilestonesPushFailed,
			} {
				if len(ms) == 0 {
					continue
				}
				out := make([]obj, len(ms))
				for i, m := range ms {
					out[i] = obj{"milestone": m.Milestone, "title": m.Title, "feature": fid(m.FeatureID), "note": m.Note}
				}
				o[key] = out
			}
			return o, nil
		})
}
