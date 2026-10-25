package mcpserver

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"skald/internal/domain"
	"skald/internal/service"
)

// maxListLimit caps list results to keep responses token-lean.
const maxListLimit = 100

type projectArg struct {
	Project string `json:"project,omitempty" jsonschema:"project slug; omit to use the repo you are in"`
}

type taskArg struct {
	projectArg
	Task string `json:"task" jsonschema:"task ID such as T-42"`
}

type listTasksArgs struct {
	projectArg
	Feature string   `json:"feature,omitempty" jsonschema:"only tasks of this feature, e.g. F-3"`
	Status  []string `json:"status,omitempty" jsonschema:"only these statuses: todo, in_progress, blocked, done, cancelled"`
	Ready   bool     `json:"ready,omitempty" jsonschema:"only ready tasks (todo, all dependencies done)"`
	Tags    []string `json:"tags,omitempty" jsonschema:"only tasks carrying every one of these tags"`
	Limit   int      `json:"limit,omitempty" jsonschema:"max results, default 50, cap 100"`
	Offset  int      `json:"offset,omitempty" jsonschema:"skip this many results"`
}

type nextArgs struct {
	projectArg
	Limit int `json:"limit,omitempty" jsonschema:"max tasks, default 10, cap 50"`
}

type getDocArgs struct {
	projectArg
	Kind    string `json:"kind,omitempty" jsonschema:"document kind, e.g. requirements, architecture, decision, convention"`
	Title   string `json:"title,omitempty" jsonschema:"document title; with kind returns the body, omit both to list documents"`
	Version int    `json:"version,omitempty" jsonschema:"older version number; default current"`
}

func (s *server) registerRead(srv *mcp.Server) {
	addTool(srv, s, "get_project",
		"Project overview: progress, features, blocked work, documents and the evidence rule. Call at session start. Next: get_next_tasks or create_plan.",
		true, func(ctx context.Context, actor domain.Actor, in projectArg) (any, error) {
			sum, err := s.svc.ProjectSummary(ctx, s.ref(in.Project))
			if err != nil {
				return nil, err
			}
			o := progressBody(sum.ProgressReport)
			o["name"] = sum.Project.Name
			o["status"] = string(sum.Project.Status)
			o["require_evidence"] = sum.Project.Settings.RequireEvidence
			if sum.Project.Description != "" {
				o["description"] = sum.Project.Description
			}
			if sum.Project.RepoPath != "" {
				o["repo"] = sum.Project.RepoPath
			}
			docs := make([]obj, len(sum.Documents))
			for i, d := range sum.Documents {
				docs[i] = obj{"kind": d.Kind, "title": d.Title, "version": d.CurrentVersion}
			}
			if len(docs) > 0 {
				o["documents"] = docs
			}
			return o, nil
		})

	addTool(srv, s, "get_project_progress",
		"Derived progress per feature plus blocked and stale tasks. Progress is computed from task state and cannot be set. Next: get_next_tasks.",
		true, func(ctx context.Context, actor domain.Actor, in projectArg) (any, error) {
			rep, err := s.svc.ProjectProgress(ctx, s.ref(in.Project))
			if err != nil {
				return nil, err
			}
			return progressBody(rep), nil
		})

	addTool(srv, s, "list_features",
		"List features with derived status and progress. Next: list_tasks or create_task.",
		true, func(ctx context.Context, actor domain.Actor, in projectArg) (any, error) {
			p, err := s.svc.ResolveProject(ctx, s.ref(in.Project))
			if err != nil {
				return nil, err
			}
			fs, err := s.svc.ListFeatures(ctx, actor, service.ProjectRef{Slug: p.Slug, Cwd: s.cwd})
			if err != nil {
				return nil, err
			}
			out := make([]obj, len(fs))
			for i, f := range fs {
				out[i] = obj{
					"id": fid(f.Feature.ID), "title": f.Feature.Title, "status": string(f.Feature.Status),
					"weight": f.Feature.Weight, "percent": pct(f.Progress.Fraction), "tasks": statusCounts(f.Counts),
				}
			}
			return obj{"project": p.Slug, "features": out}, nil
		})

	addTool(srv, s, "get_task",
		"One task with description, acceptance criteria, dependencies, evidence and recent notes/events. Next: start_task if ready.",
		true, func(ctx context.Context, actor domain.Actor, in taskArg) (any, error) {
			id, err := taskID(in.Task)
			if err != nil {
				return nil, err
			}
			p, err := s.svc.ResolveProject(ctx, s.ref(in.Project))
			if err != nil {
				return nil, err
			}
			d, err := s.svc.GetTask(ctx, actor, service.ProjectRef{Slug: p.Slug, Cwd: s.cwd}, id)
			if err != nil {
				return nil, err
			}
			t := d.Task
			o := obj{
				"project": p.Slug, "id": tid(t.ID), "title": t.Title, "status": string(t.Status),
				"feature": fid(t.FeatureID), "feature_title": d.Feature.Title,
				"priority": priorityName(t.Priority), "weight": t.Weight, "version": t.Version,
				"ready": d.Ready, "stale": d.Stale,
			}
			if t.Description != "" {
				o["description"] = t.Description
			}
			if t.AcceptanceCriteria != "" {
				o["acceptance_criteria"] = t.AcceptanceCriteria
			}
			if len(t.Tags) > 0 {
				o["tags"] = t.Tags
			}
			if u := service.GitHubIssueURL(t.IdempotencyKey); u != "" {
				o["github"] = u
			}
			if t.Assignee != "" {
				o["assignee"] = t.Assignee
			}
			if t.BlockedReason != "" {
				o["blocked_reason"] = t.BlockedReason
			}
			if len(d.Dependencies) > 0 {
				deps := make([]obj, len(d.Dependencies))
				for i, dep := range d.Dependencies {
					deps[i] = obj{"id": tid(dep.ID), "title": dep.Title, "status": string(dep.Status)}
				}
				o["depends_on"] = deps
			}
			if len(d.UnmetDependencies) > 0 {
				o["unmet"] = taskIDs(d.UnmetDependencies)
			}
			if len(d.Dependents) > 0 {
				ids := make([]int64, len(d.Dependents))
				for i, dep := range d.Dependents {
					ids[i] = dep.ID
				}
				o["blocks"] = taskIDs(ids)
			}
			if len(d.Artifacts) > 0 {
				o["evidence"] = artifacts(d.Artifacts)
			}
			const maxEvents = 10
			evs := d.RecentEvents
			if len(evs) > maxEvents {
				evs = evs[:maxEvents]
			}
			if len(evs) > 0 {
				out := make([]obj, len(evs))
				for i, e := range evs {
					ev := obj{"type": string(e.Type), "actor": string(e.Actor), "at": tm(e.CreatedAt)}
					if len(e.Payload) > 0 && strings.TrimSpace(string(e.Payload)) != "{}" {
						ev["data"] = json.RawMessage(e.Payload)
					}
					out[i] = ev
				}
				o["events"] = out
			}
			return o, nil
		})

	addTool(srv, s, "list_tasks",
		"List tasks, filterable by feature, status, tags or ready. Capped; says when truncated. Next: get_task or start_task.",
		true, func(ctx context.Context, actor domain.Actor, in listTasksArgs) (any, error) {
			p, err := s.svc.ResolveProject(ctx, s.ref(in.Project))
			if err != nil {
				return nil, err
			}
			f := service.ListTasksFilter{ReadyOnly: in.Ready, Tags: in.Tags, Offset: in.Offset, Limit: in.Limit}
			if f.Limit == 0 {
				f.Limit = service.DefaultListLimit
			}
			if f.Limit > maxListLimit {
				f.Limit = maxListLimit
			}
			if in.Feature != "" {
				if f.FeatureID, err = featureID(in.Feature); err != nil {
					return nil, err
				}
			}
			for _, st := range in.Status {
				ts := domain.TaskStatus(strings.ToLower(strings.TrimSpace(st)))
				if !ts.Valid() {
					return nil, &domain.ValidationError{Field: "status", Problem: "unknown status " + st, Hint: "Use todo, in_progress, blocked, done or cancelled."}
				}
				f.Statuses = append(f.Statuses, ts)
			}
			res, err := s.svc.ListTasks(ctx, actor, service.ProjectRef{Slug: p.Slug, Cwd: s.cwd}, f)
			if err != nil {
				return nil, err
			}
			o := obj{"project": p.Slug, "tasks": lines(res.Tasks)}
			if res.Truncated {
				o["truncated"] = true
				o["note"] = listNote
			}
			return o, nil
		})

	addTool(srv, s, "list_tags",
		"Tags used in the project with how many tasks carry each, most used first. Next: list_tasks with tags.",
		true, func(ctx context.Context, actor domain.Actor, in projectArg) (any, error) {
			p, err := s.svc.ResolveProject(ctx, s.ref(in.Project))
			if err != nil {
				return nil, err
			}
			tags, err := s.svc.ListTags(ctx, actor, service.ProjectRef{Slug: p.Slug, Cwd: s.cwd})
			if err != nil {
				return nil, err
			}
			out := make([]obj, len(tags))
			for i, t := range tags {
				out[i] = obj{"tag": t.Tag, "tasks": t.Tasks}
			}
			return obj{"project": p.Slug, "tags": out}, nil
		})

	addTool(srv, s, "get_next_tasks",
		"Ready tasks (todo, every dependency done), most urgent first. Next: start_task on the first one.",
		true, func(ctx context.Context, actor domain.Actor, in nextArgs) (any, error) {
			limit := in.Limit
			if limit > service.MaxNextLimit {
				limit = service.MaxNextLimit
			}
			res, err := s.svc.NextTasks(ctx, s.ref(in.Project), limit)
			if err != nil {
				return nil, err
			}
			o := obj{"project": res.Project.Slug, "tasks": lines(res.Tasks), "ready_total": res.Total}
			if res.Truncated {
				o["truncated"] = true
				o["note"] = "more ready tasks exist; finish these first or pass a larger limit"
			}
			if len(res.Tasks) == 0 {
				o["note"] = "nothing is ready; check get_project_progress for blocked or in-progress work"
			}
			return o, nil
		})

	addTool(srv, s, "get_document",
		"Read a project document (requirements, architecture, decisions, conventions). With no kind/title, lists documents. Next: put_document to update.",
		true, func(ctx context.Context, actor domain.Actor, in getDocArgs) (any, error) {
			p, err := s.svc.ResolveProject(ctx, s.ref(in.Project))
			if err != nil {
				return nil, err
			}
			ref := service.ProjectRef{Slug: p.Slug, Cwd: s.cwd}
			if in.Title == "" {
				docs, err := s.svc.ListDocuments(ctx, ref, in.Kind)
				if err != nil {
					return nil, err
				}
				out := make([]obj, len(docs))
				for i, d := range docs {
					out[i] = obj{"kind": d.Kind, "title": d.Title, "version": d.CurrentVersion}
				}
				return obj{"project": p.Slug, "documents": out}, nil
			}
			doc, ver, err := s.svc.GetDocument(ctx, ref, in.Kind, in.Title, in.Version)
			if err != nil {
				return nil, err
			}
			return obj{
				"project": p.Slug, "kind": doc.Kind, "title": doc.Title, "version": ver.Version,
				"current_version": doc.CurrentVersion, "body": ver.Body,
			}, nil
		})
}
