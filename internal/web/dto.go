package web

import (
	"encoding/json"
	"time"

	"skald/internal/domain"
	"skald/internal/service"
)

// The API speaks short IDs ("T-42") and snake_case JSON. These types are the
// wire format; the domain types stay free of JSON concerns.

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func tsp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return ts(*t)
}

type countsDTO struct {
	Todo       int `json:"todo"`
	InProgress int `json:"in_progress"`
	Blocked    int `json:"blocked"`
	Done       int `json:"done"`
	Cancelled  int `json:"cancelled"`
	Total      int `json:"total"`
}

func countsOf(c service.TaskCounts) countsDTO {
	return countsDTO{c.Todo, c.InProgress, c.Blocked, c.Done, c.Cancelled, c.Total}
}

func statusCountsOf(c service.StatusCounts) countsDTO {
	return countsDTO{c[domain.TaskTodo], c[domain.TaskInProgress], c[domain.TaskBlocked], c[domain.TaskDone], c[domain.TaskCancelled], c.Total()}
}

type progressDTO struct {
	Done     int     `json:"done"`
	Total    int     `json:"total"`
	Fraction float64 `json:"fraction"`
}

func progressOf(p domain.Progress) progressDTO { return progressDTO{p.Done, p.Total, p.Fraction} }

type projectDTO struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status"`
	RepoPath    string `json:"repo_path,omitempty"`
	GitHub      string `json:"github,omitempty"` // linked repository, owner/name
	CreatedAt   string `json:"created_at"`
}

func projectOf(p domain.Project) projectDTO {
	d := projectDTO{
		ID: domain.FormatID(domain.EntityProject, p.ID), Slug: p.Slug, Name: p.Name,
		Description: p.Description, Status: string(p.Status), RepoPath: p.RepoPath, CreatedAt: ts(p.CreatedAt),
	}
	if p.Settings.GitHub != nil {
		d.GitHub = p.Settings.GitHub.Repo
	}
	return d
}

type overviewDTO struct {
	Project  projectDTO  `json:"project"`
	Progress progressDTO `json:"progress"`
	Counts   countsDTO   `json:"counts"`
	Features int         `json:"features"`
	Ready    int         `json:"ready"`
}

type featureDTO struct {
	ID          string      `json:"id"`
	Title       string      `json:"title"`
	Description string      `json:"description,omitempty"`
	Status      string      `json:"status"`
	Weight      int         `json:"weight"`
	Position    int         `json:"position"`
	Progress    progressDTO `json:"progress"`
	Counts      countsDTO   `json:"counts"`
	Counted     bool        `json:"counted"`
	// GitHubMilestone and GitHubURL name the milestone the feature stands for.
	GitHubMilestone int    `json:"github_milestone,omitempty"`
	GitHubURL       string `json:"github_url,omitempty"`
}

func (d *featureDTO) linkGitHub(l service.GitHubLinks, featureID int64) {
	d.GitHubMilestone, d.GitHubURL = l.Milestone(featureID)
}

type taskDTO struct {
	ID                 string   `json:"id"`
	N                  int64    `json:"n"`
	Feature            string   `json:"feature"`
	Title              string   `json:"title"`
	Description        string   `json:"description,omitempty"`
	AcceptanceCriteria string   `json:"acceptance_criteria,omitempty"`
	Status             string   `json:"status"`
	Priority           int      `json:"priority"`
	Weight             int      `json:"weight"`
	Assignee           string   `json:"assignee,omitempty"`
	BlockedReason      string   `json:"blocked_reason,omitempty"`
	Tags               []string `json:"tags"`
	Version            int64    `json:"version"`
	Ready              bool     `json:"ready,omitempty"`
	Stale              bool     `json:"stale,omitempty"`
	StartedAt          string   `json:"started_at,omitempty"`
	CompletedAt        string   `json:"completed_at,omitempty"`
	UpdatedAt          string   `json:"updated_at"`
	CreatedAt          string   `json:"created_at"`
	GitHub             string   `json:"github,omitempty"`       // issue URL
	GitHubIssue        int      `json:"github_issue,omitempty"` // issue number
}

// taskOf builds a task's DTO. Without the project's links only tasks synced
// from GitHub get their issue; taskIn finds the issues Skald created too.
func taskOf(t domain.Task) taskDTO { return taskIn(service.GitHubLinks{}, t) }

func taskIn(l service.GitHubLinks, t domain.Task) taskDTO {
	d := taskDTO{
		ID: domain.FormatID(domain.EntityTask, t.ID), N: t.ID,
		Feature: domain.FormatID(domain.EntityFeature, t.FeatureID),
		Title:   t.Title, Description: t.Description, AcceptanceCriteria: t.AcceptanceCriteria,
		Status: string(t.Status), Priority: t.Priority, Weight: t.Weight, Assignee: t.Assignee,
		BlockedReason: t.BlockedReason, Tags: t.Tags, Version: t.Version,
		StartedAt: tsp(t.StartedAt), CompletedAt: tsp(t.CompletedAt),
		UpdatedAt: ts(t.UpdatedAt), CreatedAt: ts(t.CreatedAt),
	}
	d.GitHubIssue, d.GitHub = l.Issue(t)
	return d
}

func tasksIn(l service.GitHubLinks, ts []domain.Task) []taskDTO {
	out := make([]taskDTO, 0, len(ts))
	for _, t := range ts {
		out = append(out, taskIn(l, t))
	}
	return out
}

type artifactDTO struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Ref       string `json:"ref"`
	Summary   string `json:"summary,omitempty"`
	Actor     string `json:"actor"`
	CreatedAt string `json:"created_at"`
}

type documentDTO struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Version int    `json:"version"`
}

func documentOf(d domain.Document) documentDTO {
	return documentDTO{ID: domain.FormatID(domain.EntityDocument, d.ID), Kind: d.Kind, Title: d.Title, Version: d.CurrentVersion}
}

type eventDTO struct {
	ID         int64           `json:"id"`
	Project    string          `json:"project,omitempty"`
	EntityType string          `json:"entity_type"`
	Entity     string          `json:"entity"`
	Type       string          `json:"type"`
	Actor      string          `json:"actor"`
	Task       string          `json:"task,omitempty"`  // task the event is about, when there is one
	Title      string          `json:"title,omitempty"` // that task's title
	Payload    json.RawMessage `json:"payload"`
	At         string          `json:"at"`
}

type errorBody struct {
	Error struct {
		Kind        string   `json:"kind"`
		Message     string   `json:"message"`
		CurrentTask *taskDTO `json:"current_task,omitempty"`
	} `json:"error"`
}
