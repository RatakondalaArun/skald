package service

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"skald/internal/domain"
	"skald/internal/store"
)

// List sizes for ListTasks. A limit of 0 means DefaultListLimit; larger values
// are capped at MaxListLimit.
const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// recentEventLimit is how many events GetTask returns.
const recentEventLimit = 20

// StatusCounts counts a project's or feature's tasks by stored status.
type StatusCounts map[domain.TaskStatus]int

// Total returns the number of tasks across all statuses.
func (c StatusCounts) Total() int {
	n := 0
	for _, v := range c {
		n += v
	}
	return n
}

func countByStatus(tasks []domain.Task) StatusCounts {
	c := StatusCounts{}
	for _, t := range tasks {
		c[t.Status]++
	}
	return c
}

// ProjectOverview is a project with its derived progress.
type ProjectOverview struct {
	Project  domain.Project
	Progress domain.ProjectProgressResult
	Counts   StatusCounts
	Features int // number of features, including cancelled ones
	Ready    int // todo tasks whose dependencies are all done or cancelled
}

func overviewOf(snap store.Snapshot) ProjectOverview {
	return ProjectOverview{
		Project:  snap.Project,
		Progress: domain.ProjectProgress(snap.FeaturesWithTasks()),
		Counts:   countByStatus(snap.Tasks),
		Features: len(snap.Features),
		Ready:    len(snap.ReadyTasks()),
	}
}

// CreateProjectInput is the data for CreateProject.
type CreateProjectInput struct {
	Slug        string
	Name        string
	Description string

	// RepoPath links the project to a repository. It is normalised with
	// NormalizeRepoPath before it is stored.
	RepoPath string
	// LinkCwd links the project to Cwd (or the working directory when Cwd is
	// empty) when RepoPath is empty. It is how `skald project create` run
	// from a repository root links the project to that repository.
	LinkCwd bool
	Cwd     string
}

// CreateProject creates a project with default settings. The repo path is
// stored normalised; a repo that already belongs to another active project is
// a ValidationError that names that project.
func (s *Service) CreateProject(ctx context.Context, actor domain.Actor, in CreateProjectInput) (domain.Project, error) {
	repo := strings.TrimSpace(in.RepoPath)
	if repo == "" && in.LinkCwd {
		repo = in.Cwd
		if repo == "" {
			wd, err := s.getwd()
			if err != nil {
				return domain.Project{}, fmt.Errorf("get working directory: %w", err)
			}
			repo = wd
		}
	}
	if repo != "" {
		norm, err := NormalizeRepoPath(repo)
		if err != nil {
			return domain.Project{}, err
		}
		repo = norm
	}

	var out domain.Project
	err := s.db.WriteTx(ctx, func(tx *sql.Tx) error {
		if repo != "" {
			if other, err := store.GetProjectByRepoPath(ctx, tx, repo); err == nil {
				return &domain.ValidationError{
					Field:   "repo_path",
					Problem: fmt.Sprintf("%s is already the repository of project %q", repo, other.Slug),
					Hint:    "Use that project, or archive it first with `skald project archive` to reuse the repository.",
				}
			}
		}
		p, err := store.CreateProject(ctx, tx, s.meta(actor), domain.Project{
			Slug:        in.Slug,
			Name:        in.Name,
			Description: in.Description,
			RepoPath:    repo,
		})
		if err != nil {
			return err
		}
		out = p
		return nil
	})
	return out, err
}

// GetProject resolves the project (see ResolveProject) and returns it with
// its derived progress, task counts and ready count, read from one consistent
// snapshot. The actor is accepted for a uniform signature and is not used by
// reads today.
func (s *Service) GetProject(ctx context.Context, actor domain.Actor, ref ProjectRef) (ProjectOverview, error) {
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return ProjectOverview{}, err
	}
	snap, err := s.db.ReadSnapshot(ctx, p.ID)
	if err != nil {
		return ProjectOverview{}, err
	}
	return overviewOf(snap), nil
}

// ListProjects returns every active project (and archived ones when
// includeArchived is true), ordered by slug, each with its summary.
func (s *Service) ListProjects(ctx context.Context, actor domain.Actor, includeArchived bool) ([]ProjectOverview, error) {
	var out []ProjectOverview
	err := s.db.ReadTx(ctx, func(tx *sql.Tx) error {
		projects, err := store.ListProjects(ctx, tx, includeArchived)
		if err != nil {
			return err
		}
		for _, p := range projects {
			snap, err := store.LoadSnapshot(ctx, tx, p.ID)
			if err != nil {
				return err
			}
			out = append(out, overviewOf(snap))
		}
		return nil
	})
	return out, err
}

// ArchiveProject archives a project. Archiving frees the project's repo_path
// so the repository can be linked to a new project: the archived project's
// repo_path becomes NULL and a project.updated event records the released
// path ({"released_repo_path": "..."}), so the history keeps the link.
// Archiving an archived project is a no-op.
func (s *Service) ArchiveProject(ctx context.Context, actor domain.Actor, projectID int64) (domain.Project, error) {
	var out domain.Project
	err := s.db.WriteTx(ctx, func(tx *sql.Tx) error {
		m := s.meta(actor)
		cur, err := store.GetProject(ctx, tx, projectID)
		if err != nil {
			return err
		}
		if cur.Status == domain.ProjectArchived {
			out = cur
			return nil
		}
		out, err = store.ArchiveProject(ctx, tx, m, projectID)
		return err
	})
	return out, err
}

// LinkProject links an active project to a repository directory, or unlinks it
// when repoPath is empty. The path is normalised the same way as at creation.
// A path owned by another active project is a ValidationError naming it.
func (s *Service) LinkProject(ctx context.Context, actor domain.Actor, projectID int64, repoPath string) (domain.Project, error) {
	norm := ""
	if strings.TrimSpace(repoPath) != "" {
		var err error
		if norm, err = NormalizeRepoPath(repoPath); err != nil {
			return domain.Project{}, &domain.ValidationError{Field: "repo", Problem: err.Error(), Hint: "Pass an existing directory."}
		}
	}
	var out domain.Project
	err := s.db.WriteTx(ctx, func(tx *sql.Tx) error {
		cur, err := store.GetProject(ctx, tx, projectID)
		if err != nil {
			return err
		}
		if err := ensureProjectActive(cur); err != nil {
			return err
		}
		if norm != "" {
			if owner, err := store.GetProjectByRepoPath(ctx, tx, norm); err == nil && owner.ID != projectID {
				return &domain.ValidationError{Field: "repo", Problem: fmt.Sprintf("%s is already linked to project %q", norm, owner.Slug), Hint: "Archive that project or unlink it first."}
			}
		}
		out, err = store.SetProjectRepoPath(ctx, tx, s.meta(actor), projectID, norm)
		return err
	})
	return out, store.Classify(s.db.Path(), err)
}

// ensureProjectActive refuses changes to archived projects.
func ensureProjectActive(p domain.Project) error {
	if p.Status == domain.ProjectArchived {
		return &domain.ValidationError{
			Field:   "project",
			Problem: fmt.Sprintf("project %q is archived", p.Slug),
			Hint:    "Create a new project, or work in an active one.",
		}
	}
	return nil
}

// CreateFeatureInput is the data for CreateFeature. Zero Weight means 1 and zero
// Position means the next free position.
type CreateFeatureInput struct {
	Title       string
	Description string
	Weight      int
	Position    int
}

// CreateFeature adds a feature to the resolved project.
func (s *Service) CreateFeature(ctx context.Context, actor domain.Actor, ref ProjectRef, in CreateFeatureInput) (domain.Feature, error) {
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return domain.Feature{}, err
	}
	if err := ensureProjectActive(p); err != nil {
		return domain.Feature{}, err
	}
	var out domain.Feature
	err = s.db.WriteTx(ctx, func(tx *sql.Tx) error {
		f, err := store.CreateFeature(ctx, tx, s.meta(actor), domain.Feature{
			ProjectID:   p.ID,
			Title:       in.Title,
			Description: in.Description,
			Weight:      in.Weight,
			Position:    in.Position,
		})
		out = f
		return err
	})
	return out, err
}

// FeatureSummary is a feature with its derived status and progress.
type FeatureSummary struct {
	Feature  domain.Feature // Status is the derived status
	Progress domain.Progress
	Counts   StatusCounts
}

// ListFeatures returns the resolved project's features in position order.
// Each feature's Status is derived from its tasks (cancelled stays
// cancelled).
func (s *Service) ListFeatures(ctx context.Context, actor domain.Actor, ref ProjectRef) ([]FeatureSummary, error) {
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	snap, err := s.db.ReadSnapshot(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	var out []FeatureSummary
	for _, ft := range snap.FeaturesWithTasks() {
		f := ft.Feature
		f.Status = domain.DeriveFeatureStatus(f.Status, ft.Tasks)
		out = append(out, FeatureSummary{
			Feature:  f,
			Progress: domain.FeatureProgress(ft.Tasks),
			Counts:   countByStatus(ft.Tasks),
		})
	}
	return out, nil
}

// CreateTaskInput is the data for CreateTask. Zero values mean defaults: Priority
// nil is 2, Weight 0 is 1, Position 0 is the next free position in the
// feature.
type CreateTaskInput struct {
	FeatureID          int64
	Title              string
	Description        string
	AcceptanceCriteria string
	Priority           *int
	Weight             int
	Position           int
	// IdempotencyKey makes retries safe: repeating a key in a project returns
	// the existing task with created == false.
	IdempotencyKey string
	// Tags are free-form labels; see domain.NormalizeTag for the rules.
	Tags []string
}

// CreateTask adds a task to a feature of the resolved project. The feature
// must belong to that project and must not be cancelled. It returns whether a
// new task was created (false when the idempotency key matched an existing
// task).
func (s *Service) CreateTask(ctx context.Context, actor domain.Actor, ref ProjectRef, in CreateTaskInput) (task domain.Task, created bool, err error) {
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return domain.Task{}, false, err
	}
	if err := ensureProjectActive(p); err != nil {
		return domain.Task{}, false, err
	}
	tags, err := domain.NormalizeTags(in.Tags)
	if err != nil {
		return domain.Task{}, false, err
	}
	err = s.db.WriteTx(ctx, func(tx *sql.Tx) error {
		// A repeated idempotency key returns the existing task even if its
		// feature has since been cancelled, so retries stay safe.
		if in.IdempotencyKey == "" {
			f, err := store.GetFeature(ctx, tx, in.FeatureID)
			if err != nil {
				return err
			}
			if f.ProjectID == p.ID && f.Status == domain.FeatureCancelled {
				return &domain.ValidationError{
					Field:   "feature",
					Problem: domain.FormatID(domain.EntityFeature, f.ID) + " is cancelled",
					Hint:    "Add the task to an active feature.",
				}
			}
		}
		task, created, err = store.CreateTask(ctx, tx, s.meta(actor), store.TaskInput{
			ProjectID:          p.ID,
			FeatureID:          in.FeatureID,
			Title:              in.Title,
			Description:        in.Description,
			AcceptanceCriteria: in.AcceptanceCriteria,
			Priority:           in.Priority,
			Weight:             in.Weight,
			Position:           in.Position,
			IdempotencyKey:     in.IdempotencyKey,
			Tags:               tags,
		})
		return err
	})
	if err != nil {
		return domain.Task{}, false, err
	}
	return task, created, nil
}

// TaskDetail is one task with everything around it.
type TaskDetail struct {
	Task         domain.Task
	Feature      domain.Feature
	Dependencies []domain.Task // tasks this one depends on
	Dependents   []domain.Task // tasks that depend on this one
	Artifacts    []domain.Artifact
	// RecentEvents are the task's latest events, newest first.
	RecentEvents []domain.Event

	// Derived, never stored.
	Ready             bool
	Stale             bool
	UnmetDependencies []int64 // dependency task IDs not yet done or cancelled
}

// GetTask returns a task of the resolved project with its dependencies,
// dependents, artifacts, recent events and derived ready/stale flags, read
// from one consistent snapshot. A task from another project is NotFound.
func (s *Service) GetTask(ctx context.Context, actor domain.Actor, ref ProjectRef, taskID int64) (TaskDetail, error) {
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return TaskDetail{}, err
	}
	var d TaskDetail
	err = s.db.ReadTx(ctx, func(tx *sql.Tx) error {
		if d.Task, err = store.GetTaskInProject(ctx, tx, p.ID, taskID); err != nil {
			return err
		}
		if d.Feature, err = store.GetFeature(ctx, tx, d.Task.FeatureID); err != nil {
			return err
		}
		if d.Dependencies, err = store.ListDependencyTasks(ctx, tx, taskID); err != nil {
			return err
		}
		ids, err := store.ListDependents(ctx, tx, taskID)
		if err != nil {
			return err
		}
		for _, id := range ids {
			dep, err := store.GetTask(ctx, tx, id)
			if err != nil {
				return err
			}
			d.Dependents = append(d.Dependents, dep)
		}
		if d.Artifacts, err = store.ListArtifacts(ctx, tx, taskID); err != nil {
			return err
		}
		d.RecentEvents, err = store.TaskEvents(ctx, tx, taskID, recentEventLimit)
		return err
	})
	if err != nil {
		return TaskDetail{}, err
	}
	d.UnmetDependencies = domain.UnmetDependencies(d.Dependencies)
	d.Ready = domain.Ready(d.Task, d.Dependencies)
	d.Stale = domain.Stale(d.Task, s.now(), 0)
	return d, nil
}

// ListTasksFilter selects tasks for ListTasks. Zero values mean no filter.
type ListTasksFilter struct {
	FeatureID int64
	Statuses  []domain.TaskStatus
	ReadyOnly bool     // todo tasks whose dependencies are all done or cancelled
	Tags      []string // tasks carrying every one of these tags
	Limit     int      // 0 means DefaultListLimit; capped at MaxListLimit
	Offset    int
}

// TaskList is a page of tasks.
type TaskList struct {
	Tasks []domain.Task // priority, then feature position, then task position
	// Truncated is true when more tasks match than were returned; ask again
	// with a larger Offset or narrower filters.
	Truncated bool
}

// ListTasks lists tasks of the resolved project, ordered by priority, feature
// position and task position.
func (s *Service) ListTasks(ctx context.Context, actor domain.Actor, ref ProjectRef, f ListTasksFilter) (TaskList, error) {
	if f.Limit < 0 || f.Offset < 0 {
		return TaskList{}, &domain.ValidationError{Field: "limit", Problem: "limit and offset must not be negative", Hint: "Use 0 for the default page size."}
	}
	limit := f.Limit
	if limit == 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	tags, err := domain.NormalizeTags(f.Tags)
	if err != nil {
		return TaskList{}, err
	}
	p, err := s.ResolveProject(ctx, ref)
	if err != nil {
		return TaskList{}, err
	}
	var tasks []domain.Task
	err = s.db.ReadTx(ctx, func(tx *sql.Tx) error {
		if f.FeatureID != 0 {
			feat, err := store.GetFeature(ctx, tx, f.FeatureID)
			if err != nil {
				return err
			}
			if feat.ProjectID != p.ID {
				return &domain.NotFoundError{Entity: domain.EntityFeature, ID: f.FeatureID}
			}
		}
		// Ask for one extra row to learn whether the page was cut short.
		tasks, err = store.ListTasks(ctx, tx, store.TaskFilter{
			ProjectID: p.ID,
			FeatureID: f.FeatureID,
			Statuses:  f.Statuses,
			ReadyOnly: f.ReadyOnly,
			Tags:      tags,
			Limit:     limit + 1,
			Offset:    f.Offset,
		})
		return err
	})
	if err != nil {
		return TaskList{}, err
	}
	list := TaskList{Tasks: tasks}
	if len(tasks) > limit {
		list.Tasks, list.Truncated = tasks[:limit], true
	}
	return list, nil
}
