package cli

import (
	"encoding/json"
	"time"

	"skald/internal/domain"
	"skald/internal/service"
)

// JSON shapes printed by --json. IDs are short IDs ("T-42"); times are UTC
// RFC 3339.

type projectJSON struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status"`
	RepoPath    string `json:"repo_path,omitempty"`
	Settings    struct {
		RequireEvidence bool `json:"require_evidence"`
	} `json:"settings"`
	CreatedAt string `json:"created_at"`
}

type progressJSON struct {
	Done     int     `json:"done"`
	Total    int     `json:"total"`
	Fraction float64 `json:"fraction"`
	Percent  float64 `json:"percent"`
}

type countsJSON struct {
	Todo       int `json:"todo"`
	InProgress int `json:"in_progress"`
	Blocked    int `json:"blocked"`
	Done       int `json:"done"`
	Cancelled  int `json:"cancelled"`
	Total      int `json:"total"`
}

type featureJSON struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Description string        `json:"description,omitempty"`
	Status      string        `json:"status"`
	Weight      int           `json:"weight"`
	Position    int           `json:"position"`
	Progress    *progressJSON `json:"progress,omitempty"`
	Counts      *countsJSON   `json:"counts,omitempty"`
	Counted     *bool         `json:"counted,omitempty"`
}

type taskJSON struct {
	ID                 string   `json:"id"`
	Feature            string   `json:"feature"`
	Title              string   `json:"title"`
	Description        string   `json:"description,omitempty"`
	AcceptanceCriteria string   `json:"acceptance_criteria,omitempty"`
	Status             string   `json:"status"`
	Priority           int      `json:"priority"`
	Weight             int      `json:"weight"`
	Assignee           string   `json:"assignee,omitempty"`
	BlockedReason      string   `json:"blocked_reason,omitempty"`
	Tags               []string `json:"tags,omitempty"`
	Version            int64    `json:"version"`
	StartedAt          string   `json:"started_at,omitempty"`
	CompletedAt        string   `json:"completed_at,omitempty"`
	UpdatedAt          string   `json:"updated_at"`
}

type artifactJSON struct {
	ID        string `json:"id"`
	Task      string `json:"task"`
	Kind      string `json:"kind"`
	Ref       string `json:"ref"`
	Summary   string `json:"summary,omitempty"`
	Actor     string `json:"actor"`
	CreatedAt string `json:"created_at"`
}

type eventJSON struct {
	ID        int64           `json:"id"`
	Project   string          `json:"project"`
	Entity    string          `json:"entity"`
	Type      string          `json:"type"`
	Actor     string          `json:"actor"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"created_at"`
}

type taskDetailJSON struct {
	taskJSON
	FeatureTitle      string         `json:"feature_title"`
	Ready             bool           `json:"ready"`
	Stale             bool           `json:"stale"`
	UnmetDependencies []string       `json:"unmet_dependencies"`
	DependsOn         []taskJSON     `json:"depends_on"`
	Dependents        []taskJSON     `json:"dependents"`
	Artifacts         []artifactJSON `json:"artifacts"`
	RecentEvents      []eventJSON    `json:"recent_events"`
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func tsp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return ts(*t)
}

func toProject(p domain.Project) projectJSON {
	j := projectJSON{
		ID: projectID(p.ID), Slug: p.Slug, Name: p.Name, Description: p.Description,
		Status: string(p.Status), RepoPath: p.RepoPath, CreatedAt: ts(p.CreatedAt),
	}
	j.Settings.RequireEvidence = p.Settings.RequireEvidence
	return j
}

func toProgress(p domain.Progress) progressJSON {
	return progressJSON{Done: p.Done, Total: p.Total, Fraction: p.Fraction, Percent: p.Percent()}
}

func toCounts(c service.StatusCounts) countsJSON {
	return countsJSON{
		Todo: c[domain.TaskTodo], InProgress: c[domain.TaskInProgress], Blocked: c[domain.TaskBlocked],
		Done: c[domain.TaskDone], Cancelled: c[domain.TaskCancelled], Total: c.Total(),
	}
}

func toTaskCounts(c service.TaskCounts) countsJSON {
	return countsJSON{
		Todo: c.Todo, InProgress: c.InProgress, Blocked: c.Blocked,
		Done: c.Done, Cancelled: c.Cancelled, Total: c.Total,
	}
}

func toFeature(f domain.Feature) featureJSON {
	return featureJSON{
		ID: featureID(f.ID), Title: f.Title, Description: f.Description,
		Status: string(f.Status), Weight: f.Weight, Position: f.Position,
	}
}

func toTask(t domain.Task) taskJSON {
	return taskJSON{
		ID: taskID(t.ID), Feature: featureID(t.FeatureID), Title: t.Title, Description: t.Description,
		AcceptanceCriteria: t.AcceptanceCriteria, Status: string(t.Status), Priority: t.Priority,
		Weight: t.Weight, Assignee: t.Assignee, BlockedReason: t.BlockedReason, Version: t.Version,
		Tags: t.Tags, StartedAt: tsp(t.StartedAt), CompletedAt: tsp(t.CompletedAt), UpdatedAt: ts(t.UpdatedAt),
	}
}

func toTasks(ts []domain.Task) []taskJSON {
	out := make([]taskJSON, 0, len(ts))
	for _, t := range ts {
		out = append(out, toTask(t))
	}
	return out
}

func toArtifact(a domain.Artifact) artifactJSON {
	return artifactJSON{
		ID: domain.FormatID(domain.EntityArtifact, a.ID), Task: taskID(a.TaskID), Kind: string(a.Kind),
		Ref: a.Ref, Summary: a.Summary, Actor: a.Actor.String(), CreatedAt: ts(a.CreatedAt),
	}
}

func toEvent(e domain.Event) eventJSON {
	payload := e.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	return eventJSON{
		ID: e.ID, Project: projectID(e.ProjectID), Entity: domain.FormatID(e.EntityType, e.EntityID),
		Type: string(e.Type), Actor: e.Actor.String(), Payload: payload, CreatedAt: ts(e.CreatedAt),
	}
}

func toEvents(es []domain.Event) []eventJSON {
	out := make([]eventJSON, 0, len(es))
	for _, e := range es {
		out = append(out, toEvent(e))
	}
	return out
}

func toTaskDetail(d service.TaskDetail) taskDetailJSON {
	j := taskDetailJSON{
		taskJSON: toTask(d.Task), FeatureTitle: d.Feature.Title, Ready: d.Ready, Stale: d.Stale,
		UnmetDependencies: []string{}, DependsOn: toTasks(d.Dependencies), Dependents: toTasks(d.Dependents),
		Artifacts: []artifactJSON{}, RecentEvents: toEvents(d.RecentEvents),
	}
	for _, id := range d.UnmetDependencies {
		j.UnmetDependencies = append(j.UnmetDependencies, taskID(id))
	}
	for _, a := range d.Artifacts {
		j.Artifacts = append(j.Artifacts, toArtifact(a))
	}
	return j
}
