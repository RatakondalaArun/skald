package mcpserver

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"skald/internal/domain"
	"skald/internal/service"
)

type createProjectArgs struct {
	Name        string `json:"name" jsonschema:"project name"`
	Slug        string `json:"slug,omitempty" jsonschema:"short unique slug; derived from the name when omitted"`
	Description string `json:"description,omitempty" jsonschema:"what the project is"`
	RepoPath    string `json:"repo_path,omitempty" jsonschema:"repository directory; defaults to the current repo"`
}

type planArgs struct {
	projectArg
	Features []planFeature `json:"features" jsonschema:"features in order, each with its tasks"`
}

type planFeature struct {
	Title       string     `json:"title" jsonschema:"feature title, unique within the plan"`
	Description string     `json:"description,omitempty"`
	Weight      int        `json:"weight,omitempty" jsonschema:"relative size 1-10, default 1"`
	Tasks       []planTask `json:"tasks" jsonschema:"tasks of this feature"`
}

type planTask struct {
	Ref                string   `json:"ref" jsonschema:"local name, unique in the plan, used by depends_on"`
	Title              string   `json:"title"`
	Description        string   `json:"description,omitempty"`
	AcceptanceCriteria string   `json:"acceptance_criteria,omitempty" jsonschema:"how to tell the task is done"`
	Priority           string   `json:"priority,omitempty" jsonschema:"urgent, high, normal (default) or low"`
	Weight             int      `json:"weight,omitempty" jsonschema:"relative size 1-10, default 1"`
	DependsOn          []string `json:"depends_on,omitempty" jsonschema:"refs of tasks in this plan that must finish first"`
	DependsOnExisting  []string `json:"depends_on_existing,omitempty" jsonschema:"IDs (T-n) of existing tasks that must finish first"`
	IdempotencyKey     string   `json:"idempotency_key,omitempty" jsonschema:"stable unique key; makes retries and re-runs safe"`
	Tags               []string `json:"tags,omitempty" jsonschema:"free-form labels such as backend or ui"`
}

type createFeatureArgs struct {
	projectArg
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Weight      int    `json:"weight,omitempty" jsonschema:"relative size 1-10, default 1"`
}

type createTaskArgs struct {
	projectArg
	Feature            string   `json:"feature" jsonschema:"feature ID such as F-3"`
	Title              string   `json:"title"`
	Description        string   `json:"description,omitempty"`
	AcceptanceCriteria string   `json:"acceptance_criteria,omitempty"`
	Priority           string   `json:"priority,omitempty" jsonschema:"urgent, high, normal (default) or low"`
	Weight             int      `json:"weight,omitempty" jsonschema:"relative size 1-10, default 1"`
	IdempotencyKey     string   `json:"idempotency_key,omitempty" jsonschema:"stable unique key; repeating it returns the existing task"`
	Tags               []string `json:"tags,omitempty" jsonschema:"free-form labels such as backend or ui"`
}

type tagArgs struct {
	projectArg
	Task   string   `json:"task" jsonschema:"task ID such as T-42"`
	Add    []string `json:"add,omitempty" jsonschema:"tags to add"`
	Remove []string `json:"remove,omitempty" jsonschema:"tags to remove"`
}

type depArgs struct {
	projectArg
	Task      string `json:"task" jsonschema:"the task that must wait, e.g. T-5"`
	DependsOn string `json:"depends_on" jsonschema:"the prerequisite task, e.g. T-4"`
}

type putDocArgs struct {
	projectArg
	Kind  string `json:"kind" jsonschema:"requirements, architecture, decision or convention"`
	Title string `json:"title"`
	Body  string `json:"body" jsonschema:"full new text (creates the next version)"`
}

func (s *server) registerWrite(srv *mcp.Server) {
	addTool(srv, s, "create_project",
		"Create a project linked to the current repo. Next: create_plan.",
		false, func(ctx context.Context, actor domain.Actor, in createProjectArgs) (any, error) {
			slug := strings.TrimSpace(in.Slug)
			if slug == "" {
				slug = slugify(in.Name)
			}
			p, err := s.svc.CreateProject(ctx, actor, service.CreateProjectInput{
				Slug: slug, Name: in.Name, Description: in.Description,
				RepoPath: in.RepoPath, LinkCwd: in.RepoPath == "", Cwd: s.cwd,
			})
			if err != nil {
				return nil, err
			}
			o := obj{"project": p.Slug, "id": pid(p.ID), "name": p.Name, "require_evidence": p.Settings.RequireEvidence}
			if p.RepoPath != "" {
				o["repo"] = p.RepoPath
			}
			return o, nil
		})

	addTool(srv, s, "create_plan",
		"Create features, tasks and dependencies in one transaction; nothing is saved if any part is invalid. "+
			"Give each task a local ref and list prerequisites in depends_on. Give EVERY task an idempotency_key so retries and re-runs never duplicate work. "+
			"Tasks can only be completed later with verification evidence. Next: get_next_tasks.",
		false, func(ctx context.Context, actor domain.Actor, in planArgs) (any, error) {
			plan := service.PlanInput{}
			for _, f := range in.Features {
				pf := service.PlanFeature{Title: f.Title, Description: f.Description, Weight: f.Weight}
				for _, t := range f.Tasks {
					prio, err := parsePriority(t.Priority)
					if err != nil {
						return nil, err
					}
					pt := service.PlanTask{
						Ref: t.Ref, Title: t.Title, Description: t.Description, AcceptanceCriteria: t.AcceptanceCriteria,
						Priority: prio, Weight: t.Weight, DependsOn: t.DependsOn, IdempotencyKey: t.IdempotencyKey, Tags: t.Tags,
					}
					for _, e := range t.DependsOnExisting {
						id, err := taskID(e)
						if err != nil {
							return nil, err
						}
						pt.DependsOnExisting = append(pt.DependsOnExisting, id)
					}
					pf.Tasks = append(pf.Tasks, pt)
				}
				plan.Features = append(plan.Features, pf)
			}
			res, err := s.svc.CreatePlan(ctx, actor, s.ref(in.Project), plan)
			if err != nil {
				return nil, err
			}
			feats := make([]obj, len(res.Features))
			for i, f := range res.Features {
				feats[i] = obj{"id": fid(f.FeatureID), "title": f.Title, "created": f.Created}
			}
			var tasks []obj
			for _, f := range in.Features {
				for _, t := range f.Tasks {
					r := res.Tasks[t.Ref]
					tasks = append(tasks, obj{"ref": t.Ref, "id": tid(r.TaskID), "feature": fid(r.FeatureID), "created": r.Created})
				}
			}
			return obj{
				"project": res.ProjectSlug, "features": feats, "tasks": tasks,
				"created": obj{"features": res.FeaturesCreated, "tasks": res.TasksCreated, "dependencies": res.DependenciesAdded},
				"reused":  obj{"features": res.FeaturesReused, "tasks": res.TasksReused},
			}, nil
		})

	addTool(srv, s, "create_feature",
		"Add one feature. For several features and tasks at once use create_plan. Next: create_task.",
		false, func(ctx context.Context, actor domain.Actor, in createFeatureArgs) (any, error) {
			p, err := s.svc.ResolveProject(ctx, s.ref(in.Project))
			if err != nil {
				return nil, err
			}
			f, err := s.svc.CreateFeature(ctx, actor, service.ProjectRef{Slug: p.Slug, Cwd: s.cwd},
				service.CreateFeatureInput{Title: in.Title, Description: in.Description, Weight: in.Weight})
			if err != nil {
				return nil, err
			}
			return obj{"project": p.Slug, "id": fid(f.ID), "title": f.Title, "weight": f.Weight}, nil
		})

	addTool(srv, s, "create_task",
		"Add one task to a feature. Set idempotency_key to make retries safe. Next: add_task_dependency or get_next_tasks.",
		false, func(ctx context.Context, actor domain.Actor, in createTaskArgs) (any, error) {
			fidn, err := featureID(in.Feature)
			if err != nil {
				return nil, err
			}
			prio, err := parsePriority(in.Priority)
			if err != nil {
				return nil, err
			}
			p, err := s.svc.ResolveProject(ctx, s.ref(in.Project))
			if err != nil {
				return nil, err
			}
			t, created, err := s.svc.CreateTask(ctx, actor, service.ProjectRef{Slug: p.Slug, Cwd: s.cwd}, service.CreateTaskInput{
				FeatureID: fidn, Title: in.Title, Description: in.Description, AcceptanceCriteria: in.AcceptanceCriteria,
				Priority: prio, Weight: in.Weight, IdempotencyKey: in.IdempotencyKey, Tags: in.Tags,
			})
			if err != nil {
				return nil, err
			}
			return obj{"project": p.Slug, "task": line(t), "created": created}, nil
		})

	addTool(srv, s, "tag_task",
		"Add or remove tags on a task. Tags are lowercase labels (a-z, 0-9, - _ . : /). Next: list_tasks with tags.",
		false, func(ctx context.Context, actor domain.Actor, in tagArgs) (any, error) {
			id, err := taskID(in.Task)
			if err != nil {
				return nil, err
			}
			p, err := s.svc.ResolveProject(ctx, s.ref(in.Project))
			if err != nil {
				return nil, err
			}
			t, err := s.svc.TagTask(ctx, actor, service.ProjectRef{Slug: p.Slug, Cwd: s.cwd}, id, in.Add, in.Remove)
			if err != nil {
				return nil, err
			}
			return obj{"project": p.Slug, "task": line(t)}, nil
		})

	addTool(srv, s, "add_task_dependency",
		"Make `task` wait for `depends_on`. Rejects cycles and tasks from other projects. Next: get_next_tasks.",
		false, func(ctx context.Context, actor domain.Actor, in depArgs) (any, error) {
			a, err := taskID(in.Task)
			if err != nil {
				return nil, err
			}
			b, err := taskID(in.DependsOn)
			if err != nil {
				return nil, err
			}
			added, err := s.svc.AddDependency(ctx, actor, a, b)
			if err != nil {
				return nil, err
			}
			return s.withProject(ctx, in.Project, obj{"task": tid(a), "depends_on": tid(b), "added": added}), nil
		})

	addTool(srv, s, "put_document",
		"Save the next version of a project document (requirements, architecture, decision, convention). Older versions stay readable. Next: get_next_tasks.",
		false, func(ctx context.Context, actor domain.Actor, in putDocArgs) (any, error) {
			p, err := s.svc.ResolveProject(ctx, s.ref(in.Project))
			if err != nil {
				return nil, err
			}
			doc, ver, err := s.svc.PutDocument(ctx, actor, service.ProjectRef{Slug: p.Slug, Cwd: s.cwd}, in.Kind, in.Title, in.Body)
			if err != nil {
				return nil, err
			}
			return obj{"project": p.Slug, "kind": doc.Kind, "title": doc.Title, "version": ver.Version}, nil
		})
}

// withProject adds the project slug to o when it can be resolved without
// guessing. Task-ID tools do not need a project, so a failure to resolve one
// (for example several candidates) is not an error here.
func (s *server) withProject(ctx context.Context, project string, o obj) obj {
	if p, err := s.svc.ResolveProject(ctx, s.ref(project)); err == nil {
		o["project"] = p.Slug
	}
	return o
}

func slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if b.Len() > 0 && !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
