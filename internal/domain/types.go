package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ProjectStatus is the stored status of a project.
type ProjectStatus string

const (
	ProjectActive   ProjectStatus = "active"
	ProjectArchived ProjectStatus = "archived"
)

// Valid reports whether s is a known project status.
func (s ProjectStatus) Valid() bool { return s == ProjectActive || s == ProjectArchived }

// FeatureStatus is a feature's status. Only FeatureCancelled and
// FeaturePlanned are meaningful when stored; the rest is derived from tasks
// (see DeriveFeatureStatus).
type FeatureStatus string

const (
	FeaturePlanned   FeatureStatus = "planned"
	FeatureActive    FeatureStatus = "active"
	FeatureDone      FeatureStatus = "done"
	FeatureCancelled FeatureStatus = "cancelled"
)

// Valid reports whether s is a known feature status.
func (s FeatureStatus) Valid() bool {
	switch s {
	case FeaturePlanned, FeatureActive, FeatureDone, FeatureCancelled:
		return true
	}
	return false
}

// TaskStatus is a task's stored status.
type TaskStatus string

const (
	TaskTodo       TaskStatus = "todo"
	TaskInProgress TaskStatus = "in_progress"
	TaskBlocked    TaskStatus = "blocked"
	TaskDone       TaskStatus = "done"
	TaskCancelled  TaskStatus = "cancelled"
)

// Valid reports whether s is a known task status.
func (s TaskStatus) Valid() bool {
	switch s {
	case TaskTodo, TaskInProgress, TaskBlocked, TaskDone, TaskCancelled:
		return true
	}
	return false
}

// Terminal reports whether the task counts as finished for dependency
// purposes (done or cancelled).
func (s TaskStatus) Terminal() bool { return s == TaskDone || s == TaskCancelled }

// ArtifactKind classifies verification evidence.
type ArtifactKind string

const (
	ArtifactCommit     ArtifactKind = "commit"
	ArtifactPR         ArtifactKind = "pr"
	ArtifactTestReport ArtifactKind = "test_report"
	ArtifactLink       ArtifactKind = "link"
	ArtifactManual     ArtifactKind = "manual"
)

// Valid reports whether k is a known artifact kind.
func (k ArtifactKind) Valid() bool {
	switch k {
	case ArtifactCommit, ArtifactPR, ArtifactTestReport, ArtifactLink, ArtifactManual:
		return true
	}
	return false
}

// EntityType names what an event or short ID refers to.
type EntityType string

const (
	EntityProject  EntityType = "project"
	EntityFeature  EntityType = "feature"
	EntityTask     EntityType = "task"
	EntityArtifact EntityType = "artifact"
	EntityDocument EntityType = "document"
)

// Valid reports whether e is a known entity type.
func (e EntityType) Valid() bool {
	switch e {
	case EntityProject, EntityFeature, EntityTask, EntityArtifact, EntityDocument:
		return true
	}
	return false
}

// EventType is a dotted event name such as "task.status_changed".
type EventType string

const (
	EventProjectCreated   EventType = "project.created"
	EventProjectUpdated   EventType = "project.updated"
	EventProjectArchived  EventType = "project.archived"
	EventFeatureCreated   EventType = "feature.created"
	EventFeatureUpdated   EventType = "feature.updated"
	EventFeatureCancelled EventType = "feature.cancelled"
	EventTaskCreated      EventType = "task.created"
	EventTaskUpdated      EventType = "task.updated"
	EventTaskStatus       EventType = "task.status_changed"
	EventTaskNote         EventType = "task.note"
	EventTaskDepAdded     EventType = "task.dependency_added"
	EventTaskDepRemoved   EventType = "task.dependency_removed"
	EventArtifactAdded    EventType = "artifact.added"
	EventDocumentVersion  EventType = "document.version_added"
)

var knownEventTypes = map[EventType]bool{
	EventProjectCreated: true, EventProjectUpdated: true, EventProjectArchived: true,
	EventFeatureCreated: true, EventFeatureUpdated: true, EventFeatureCancelled: true,
	EventTaskCreated: true, EventTaskUpdated: true, EventTaskStatus: true, EventTaskNote: true,
	EventTaskDepAdded: true, EventTaskDepRemoved: true,
	EventArtifactAdded: true, EventDocumentVersion: true,
}

// Valid reports whether t is a known event type.
func (t EventType) Valid() bool { return knownEventTypes[t] }

// Actor identifies who made a change: "human" or "claude:<client>/<session>".
type Actor string

// Human is the default actor for CLI and web changes.
const Human Actor = "human"

const agentPrefix = "claude:"

// NewAgentActor builds the actor for an MCP session. Empty parts become
// "unknown".
func NewAgentActor(client, session string) Actor {
	clean := func(s string) string {
		s = strings.Join(strings.Fields(s), "-")
		s = strings.ReplaceAll(s, "/", "-")
		if s == "" {
			return "unknown"
		}
		return s
	}
	return Actor(agentPrefix + clean(client) + "/" + clean(session))
}

// IsAgent reports whether the actor is an agent session.
func (a Actor) IsAgent() bool { return strings.HasPrefix(string(a), agentPrefix) }

// IsHuman reports whether the actor is a human.
func (a Actor) IsHuman() bool { return !a.IsAgent() && a.Valid() }

// Valid reports whether a is non-empty and has no whitespace.
func (a Actor) Valid() bool {
	return a != "" && strings.IndexFunc(string(a), func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }) < 0
}

func (a Actor) String() string { return string(a) }

// ProjectSettings is the JSON stored in projects.settings.
type ProjectSettings struct {
	// RequireEvidence makes completing a task require at least one artifact.
	RequireEvidence bool `json:"require_evidence"`
	// GitHub links the project to a GitHub repository for `skald github sync`.
	GitHub *GitHubSettings `json:"github,omitempty"`
}

// GitHubSettings is the GitHub link of a project and the sync's memory of
// which feature stands for which milestone. Issues need no entry here: a
// synced task carries the idempotency key "github:<owner>/<name>#<number>".
type GitHubSettings struct {
	// Repo is "owner/name".
	Repo string `json:"repo"`
	// Milestones maps a milestone number (as a string; "0" is "no milestone")
	// to the ID of the feature that holds its issues.
	Milestones map[string]int64 `json:"milestones,omitempty"`
}

// DefaultSettings returns the settings of a new project.
func DefaultSettings() ProjectSettings { return ProjectSettings{RequireEvidence: true} }

// ParseSettings decodes stored settings; keys that are absent keep their
// defaults.
func ParseSettings(raw string) (ProjectSettings, error) {
	s := DefaultSettings()
	if strings.TrimSpace(raw) == "" {
		return s, nil
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return DefaultSettings(), &ValidationError{Field: "settings", Problem: fmt.Sprintf("invalid JSON (%v)", err), Hint: "Settings must be a JSON object such as {\"require_evidence\": true}."}
	}
	return s, nil
}

// Project mirrors the projects table. Empty strings stand for NULL.
type Project struct {
	ID          int64
	Slug        string
	Name        string
	Description string
	Status      ProjectStatus
	RepoPath    string
	Settings    ProjectSettings
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Feature mirrors the features table.
type Feature struct {
	ID          int64
	ProjectID   int64
	Title       string
	Description string
	Status      FeatureStatus
	Weight      int
	Position    int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Task mirrors the tasks table. Nil times stand for NULL.
type Task struct {
	ID                 int64
	ProjectID          int64
	FeatureID          int64
	Title              string
	Description        string
	AcceptanceCriteria string
	Status             TaskStatus
	Priority           int // 0 = urgent .. 3 = low
	Weight             int
	Position           int
	Assignee           string
	BlockedReason      string
	IdempotencyKey     string
	Version            int64
	StartedAt          *time.Time
	CompletedAt        *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Dependency says TaskID cannot start until DependsOnID is done or cancelled.
type Dependency struct {
	TaskID      int64
	DependsOnID int64
}

// Event mirrors the append-only events table; ID is the change cursor.
type Event struct {
	ID         int64
	ProjectID  int64
	EntityType EntityType
	EntityID   int64
	Type       EventType
	Actor      Actor
	Payload    json.RawMessage
	CreatedAt  time.Time
}

// Artifact is verification evidence attached to a task.
type Artifact struct {
	ID        int64
	TaskID    int64
	Kind      ArtifactKind
	Ref       string
	Summary   string
	Actor     Actor
	CreatedAt time.Time
}

// Document is a versioned project document.
type Document struct {
	ID             int64
	ProjectID      int64
	Kind           string
	Title          string
	CurrentVersion int
}

// DocumentVersion is one immutable version of a document.
type DocumentVersion struct {
	DocumentID int64
	Version    int
	Body       string
	Actor      Actor
	CreatedAt  time.Time
}
