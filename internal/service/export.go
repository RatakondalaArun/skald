package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"skald/internal/domain"
	"skald/internal/store"
)

// Export format identifiers. Bump ExportFormatVersion when a field is removed
// or changes meaning; adding fields does not need a bump.
const (
	ExportFormat        = "skald-export"
	ExportFormatVersion = 1
)

// ExportOptions says what to export.
type ExportOptions struct {
	// Project limits the export to one project. Zero means every project.
	Project int64
	// SkaldVersion is recorded in the document header.
	SkaldVersion string
}

// Export is the JSON document written by skald export. Every entity carries
// its raw database id ("id") and its short id ("short_id", such as "T-42");
// references to other entities use raw ids so the document can be imported
// back with ids preserved.
type Export struct {
	Format        string `json:"format"`
	FormatVersion int    `json:"format_version"`
	ExportedAt    string `json:"exported_at"`
	SkaldVersion  string `json:"skald_version"`
	SchemaVersion int    `json:"schema_version"`
	// Scope is "all" or "project:<slug>".
	Scope        string             `json:"scope"`
	Projects     []ExportProject    `json:"projects"`
	Features     []ExportFeature    `json:"features"`
	Tasks        []ExportTask       `json:"tasks"`
	Dependencies []ExportDependency `json:"dependencies"`
	Artifacts    []ExportArtifact   `json:"artifacts"`
	Documents    []ExportDocument   `json:"documents"`
	Events       []ExportEvent      `json:"events"`
}

type ExportProject struct {
	ID          int64                  `json:"id"`
	ShortID     string                 `json:"short_id"`
	Slug        string                 `json:"slug"`
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Status      domain.ProjectStatus   `json:"status"`
	RepoPath    string                 `json:"repo_path"`
	Settings    domain.ProjectSettings `json:"settings"`
	CreatedAt   time.Time              `json:"created_at"`
	UpdatedAt   time.Time              `json:"updated_at"`
}

type ExportFeature struct {
	ID          int64                `json:"id"`
	ShortID     string               `json:"short_id"`
	ProjectID   int64                `json:"project_id"`
	Title       string               `json:"title"`
	Description string               `json:"description"`
	Status      domain.FeatureStatus `json:"status"` // as stored; the derived status is not exported
	Weight      int                  `json:"weight"`
	Position    int                  `json:"position"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
}

type ExportTask struct {
	ID                 int64             `json:"id"`
	ShortID            string            `json:"short_id"`
	ProjectID          int64             `json:"project_id"`
	FeatureID          int64             `json:"feature_id"`
	Title              string            `json:"title"`
	Description        string            `json:"description"`
	AcceptanceCriteria string            `json:"acceptance_criteria"`
	Status             domain.TaskStatus `json:"status"`
	Priority           int               `json:"priority"`
	Weight             int               `json:"weight"`
	Position           int               `json:"position"`
	Assignee           string            `json:"assignee"`
	BlockedReason      string            `json:"blocked_reason"`
	IdempotencyKey     string            `json:"idempotency_key"`
	Tags               []string          `json:"tags"` // absent in exports from before tags; read as none
	Version            int64             `json:"version"`
	StartedAt          *time.Time        `json:"started_at"`
	CompletedAt        *time.Time        `json:"completed_at"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
}

// ExportDependency says task TaskID waits for DependsOnID.
type ExportDependency struct {
	TaskID      int64  `json:"task_id"`
	DependsOnID int64  `json:"depends_on_id"`
	Task        string `json:"task"`       // short id
	DependsOn   string `json:"depends_on"` // short id
}

type ExportArtifact struct {
	ID        int64               `json:"id"`
	ShortID   string              `json:"short_id"`
	TaskID    int64               `json:"task_id"`
	Kind      domain.ArtifactKind `json:"kind"`
	Ref       string              `json:"ref"`
	Summary   string              `json:"summary"`
	Actor     domain.Actor        `json:"actor"`
	CreatedAt time.Time           `json:"created_at"`
}

type ExportDocument struct {
	ID             int64                   `json:"id"`
	ShortID        string                  `json:"short_id"`
	ProjectID      int64                   `json:"project_id"`
	Kind           string                  `json:"kind"`
	Title          string                  `json:"title"`
	CurrentVersion int                     `json:"current_version"`
	Versions       []ExportDocumentVersion `json:"versions"`
}

type ExportDocumentVersion struct {
	Version   int          `json:"version"`
	Body      string       `json:"body"`
	Actor     domain.Actor `json:"actor"`
	CreatedAt time.Time    `json:"created_at"`
}

type ExportEvent struct {
	ID         int64             `json:"id"`
	ProjectID  int64             `json:"project_id"`
	EntityType domain.EntityType `json:"entity_type"`
	EntityID   int64             `json:"entity_id"`
	Entity     string            `json:"entity"` // short id of the entity
	Type       domain.EventType  `json:"type"`
	Actor      domain.Actor      `json:"actor"`
	Payload    json.RawMessage   `json:"payload"`
	CreatedAt  time.Time         `json:"created_at"`
}

// Export returns a consistent snapshot of one project, or of every project,
// read in a single transaction.
func (s *Service) Export(ctx context.Context, opts ExportOptions) (*Export, error) {
	var rows store.ExportRows
	scope := "all"
	err := s.db.ReadTx(ctx, func(tx *sql.Tx) error {
		if opts.Project != 0 {
			p, err := store.GetProject(ctx, tx, opts.Project)
			if err != nil {
				return err
			}
			scope = "project:" + p.Slug
		}
		var err error
		rows, err = store.LoadExportRows(ctx, tx, opts.Project)
		return err
	})
	if err != nil {
		return nil, err
	}
	schema, err := s.db.SchemaVersion(ctx)
	if err != nil {
		return nil, err
	}
	return buildExport(rows, scope, opts.SkaldVersion, schema, s.now()), nil
}

func buildExport(r store.ExportRows, scope, skaldVersion string, schema int, now time.Time) *Export {
	short := domain.FormatID
	x := &Export{
		Format: ExportFormat, FormatVersion: ExportFormatVersion,
		ExportedAt: now.UTC().Format(time.RFC3339), SkaldVersion: skaldVersion, SchemaVersion: schema, Scope: scope,
		// Empty slices, not null, so consumers can always range over them.
		Projects: []ExportProject{}, Features: []ExportFeature{}, Tasks: []ExportTask{},
		Dependencies: []ExportDependency{}, Artifacts: []ExportArtifact{}, Documents: []ExportDocument{}, Events: []ExportEvent{},
	}
	for _, p := range r.Projects {
		x.Projects = append(x.Projects, ExportProject{p.ID, short(domain.EntityProject, p.ID), p.Slug, p.Name, p.Description, p.Status, p.RepoPath, p.Settings, p.CreatedAt, p.UpdatedAt})
	}
	for _, f := range r.Features {
		x.Features = append(x.Features, ExportFeature{f.ID, short(domain.EntityFeature, f.ID), f.ProjectID, f.Title, f.Description, f.Status, f.Weight, f.Position, f.CreatedAt, f.UpdatedAt})
	}
	for _, t := range r.Tasks {
		x.Tasks = append(x.Tasks, ExportTask{t.ID, short(domain.EntityTask, t.ID), t.ProjectID, t.FeatureID, t.Title, t.Description, t.AcceptanceCriteria,
			t.Status, t.Priority, t.Weight, t.Position, t.Assignee, t.BlockedReason, t.IdempotencyKey, t.Tags, t.Version, t.StartedAt, t.CompletedAt, t.CreatedAt, t.UpdatedAt})
	}
	for _, d := range r.Dependencies {
		x.Dependencies = append(x.Dependencies, ExportDependency{d.TaskID, d.DependsOnID, short(domain.EntityTask, d.TaskID), short(domain.EntityTask, d.DependsOnID)})
	}
	for _, a := range r.Artifacts {
		x.Artifacts = append(x.Artifacts, ExportArtifact{a.ID, short(domain.EntityArtifact, a.ID), a.TaskID, a.Kind, a.Ref, a.Summary, a.Actor, a.CreatedAt})
	}
	versions := map[int64][]ExportDocumentVersion{}
	for _, v := range r.DocVersions {
		versions[v.DocumentID] = append(versions[v.DocumentID], ExportDocumentVersion{v.Version, v.Body, v.Actor, v.CreatedAt})
	}
	for _, d := range r.Documents {
		vs := versions[d.ID]
		if vs == nil {
			vs = []ExportDocumentVersion{}
		}
		x.Documents = append(x.Documents, ExportDocument{d.ID, short(domain.EntityDocument, d.ID), d.ProjectID, d.Kind, d.Title, d.CurrentVersion, vs})
	}
	for _, e := range r.Events {
		payload := e.Payload
		if len(payload) == 0 {
			payload = json.RawMessage("{}")
		}
		x.Events = append(x.Events, ExportEvent{e.ID, e.ProjectID, e.EntityType, e.EntityID, short(e.EntityType, e.EntityID), e.Type, e.Actor, payload, e.CreatedAt})
	}
	return x
}

// Write encodes the export as indented JSON.
func (x *Export) Write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(x)
}

// ParseExport decodes and checks an export document. Unknown fields are
// ignored so newer exports with added fields still load.
func ParseExport(r io.Reader) (*Export, error) {
	var x Export
	dec := json.NewDecoder(r)
	if err := dec.Decode(&x); err != nil {
		return nil, &domain.ValidationError{Field: "export", Problem: fmt.Sprintf("is not valid JSON (%v)", err), Hint: "Pass a file written by `skald export`."}
	}
	if x.Format != ExportFormat {
		return nil, &domain.ValidationError{Field: "format", Problem: fmt.Sprintf("is %q, want %q", x.Format, ExportFormat), Hint: "Pass a file written by `skald export`."}
	}
	if x.FormatVersion < 1 || x.FormatVersion > ExportFormatVersion {
		return nil, &domain.ValidationError{Field: "format_version", Problem: fmt.Sprintf("%d is not supported (this skald reads up to %d)", x.FormatVersion, ExportFormatVersion), Hint: "Upgrade skald to import this file."}
	}
	return &x, nil
}

// ImportExport loads an export into an empty database with every id
// preserved, including the event log, in one transaction. A database that
// already holds projects, tasks or events is refused and left untouched.
// No new events are written: the imported event log is the history.
func (s *Service) ImportExport(ctx context.Context, x *Export) error {
	r, err := x.rows()
	if err != nil {
		return err
	}
	return s.db.WriteTx(ctx, func(tx *sql.Tx) error { return store.ImportExportRows(ctx, tx, r) })
}

func (x *Export) rows() (store.ExportRows, error) {
	var r store.ExportRows
	for _, p := range x.Projects {
		if !p.Status.Valid() {
			return r, &domain.ValidationError{Field: "projects.status", Problem: fmt.Sprintf("%q is not a project status", p.Status), Hint: "The file looks edited or corrupt."}
		}
		r.Projects = append(r.Projects, domain.Project{ID: p.ID, Slug: p.Slug, Name: p.Name, Description: p.Description, Status: p.Status, RepoPath: p.RepoPath, Settings: p.Settings, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt})
	}
	for _, f := range x.Features {
		r.Features = append(r.Features, domain.Feature{ID: f.ID, ProjectID: f.ProjectID, Title: f.Title, Description: f.Description, Status: f.Status, Weight: f.Weight, Position: f.Position, CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt})
	}
	for _, t := range x.Tasks {
		r.Tasks = append(r.Tasks, domain.Task{ID: t.ID, ProjectID: t.ProjectID, FeatureID: t.FeatureID, Title: t.Title, Description: t.Description, AcceptanceCriteria: t.AcceptanceCriteria,
			Status: t.Status, Priority: t.Priority, Weight: t.Weight, Position: t.Position, Assignee: t.Assignee, BlockedReason: t.BlockedReason, IdempotencyKey: t.IdempotencyKey,
			Tags: t.Tags, Version: t.Version, StartedAt: t.StartedAt, CompletedAt: t.CompletedAt, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt})
	}
	for _, d := range x.Dependencies {
		r.Dependencies = append(r.Dependencies, domain.Dependency{TaskID: d.TaskID, DependsOnID: d.DependsOnID})
	}
	for _, a := range x.Artifacts {
		r.Artifacts = append(r.Artifacts, domain.Artifact{ID: a.ID, TaskID: a.TaskID, Kind: a.Kind, Ref: a.Ref, Summary: a.Summary, Actor: a.Actor, CreatedAt: a.CreatedAt})
	}
	for _, d := range x.Documents {
		r.Documents = append(r.Documents, domain.Document{ID: d.ID, ProjectID: d.ProjectID, Kind: d.Kind, Title: d.Title, CurrentVersion: d.CurrentVersion})
		for _, v := range d.Versions {
			r.DocVersions = append(r.DocVersions, domain.DocumentVersion{DocumentID: d.ID, Version: v.Version, Body: v.Body, Actor: v.Actor, CreatedAt: v.CreatedAt})
		}
	}
	for _, e := range x.Events {
		if !e.EntityType.Valid() {
			return r, &domain.ValidationError{Field: "events.entity_type", Problem: fmt.Sprintf("%q is not an entity type", e.EntityType), Hint: "The file looks edited or corrupt."}
		}
		r.Events = append(r.Events, domain.Event{ID: e.ID, ProjectID: e.ProjectID, EntityType: e.EntityType, EntityID: e.EntityID, Type: e.Type, Actor: e.Actor, Payload: e.Payload, CreatedAt: e.CreatedAt})
	}
	return r, nil
}
