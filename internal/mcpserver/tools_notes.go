package mcpserver

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"skald/internal/domain"
	"skald/internal/service"
)

// noteSnippet is how much of a body list_notes shows; get_note has the rest.
const noteSnippet = 300

type addNoteArgs struct {
	projectArg
	Body    string `json:"body" jsonschema:"the note: a finding, open question, gotcha or where you left off"`
	Task    string `json:"task,omitempty" jsonschema:"link to this task, e.g. T-42 (its feature is linked too)"`
	Feature string `json:"feature,omitempty" jsonschema:"link to this feature, e.g. F-3"`
}

type listNotesArgs struct {
	projectArg
	Task    string `json:"task,omitempty" jsonschema:"only notes linked to this task"`
	Feature string `json:"feature,omitempty" jsonschema:"only notes linked to this feature (includes its tasks' notes)"`
	Query   string `json:"query,omitempty" jsonschema:"only notes whose text contains this"`
	Author  string `json:"author,omitempty" jsonschema:"only notes by this actor, e.g. human"`
	Limit   int    `json:"limit,omitempty" jsonschema:"max results, default 20, cap 100"`
	Offset  int    `json:"offset,omitempty" jsonschema:"skip this many results"`
}

type getNoteArgs struct {
	projectArg
	Note string `json:"note" jsonschema:"note ID such as N-7"`
}

func noteID(s string) (int64, error) { return domain.ParseID(domain.EntityNote, s) }
func nid(id int64) string            { return domain.FormatID(domain.EntityNote, id) }

// optID parses an optional short ID; "" is 0.
func optID(parse func(string) (int64, error), s string) (int64, error) {
	if strings.TrimSpace(s) == "" {
		return 0, nil
	}
	return parse(s)
}

func noteBody(n domain.Note, full bool) obj {
	o := obj{"id": nid(n.ID), "author": string(n.Actor), "at": tm(n.CreatedAt)}
	if n.TaskID != 0 {
		o["task"] = tid(n.TaskID)
	}
	if n.FeatureID != 0 {
		o["feature"] = fid(n.FeatureID)
	}
	body := n.Body
	if !full && utf8.RuneCountInString(body) > noteSnippet {
		body = string([]rune(body)[:noteSnippet-1]) + "…"
		o["clipped"] = true
	}
	o["body"] = body
	return o
}

func (s *server) registerNotes(srv *mcp.Server) {
	addTool(srv, s, "add_note",
		"Write to the project's scratchpad: findings, open questions, gotchas, or where you left off, for yourself and later sessions. "+
			"Optionally link a task or feature. Notes are append-only and never change progress. Next: keep working; list_notes at session start.",
		false, func(ctx context.Context, actor domain.Actor, in addNoteArgs) (any, error) {
			t, err := optID(taskID, in.Task)
			if err != nil {
				return nil, err
			}
			f, err := optID(featureID, in.Feature)
			if err != nil {
				return nil, err
			}
			p, err := s.svc.ResolveProject(ctx, s.ref(in.Project))
			if err != nil {
				return nil, err
			}
			n, err := s.svc.AddNote(ctx, actor, service.ProjectRef{Slug: p.Slug, Cwd: s.cwd}, service.AddNoteInput{Body: in.Body, TaskID: t, FeatureID: f})
			if err != nil {
				return nil, err
			}
			o := obj{"project": p.Slug, "id": nid(n.ID)}
			if n.TaskID != 0 {
				o["task"] = tid(n.TaskID)
			}
			if n.FeatureID != 0 {
				o["feature"] = fid(n.FeatureID)
			}
			return o, nil
		})

	addTool(srv, s, "list_notes",
		"Read the project's scratchpad, newest first. Filter by task, feature, author or text (query). Long notes are clipped; get_note returns the full text.",
		true, func(ctx context.Context, actor domain.Actor, in listNotesArgs) (any, error) {
			t, err := optID(taskID, in.Task)
			if err != nil {
				return nil, err
			}
			f, err := optID(featureID, in.Feature)
			if err != nil {
				return nil, err
			}
			list, err := s.svc.ListNotes(ctx, s.ref(in.Project), service.NoteFilter{
				TaskID: t, FeatureID: f, Actor: domain.Actor(strings.TrimSpace(in.Author)),
				Query: in.Query, Limit: in.Limit, Offset: in.Offset,
			})
			if err != nil {
				return nil, err
			}
			out := make([]obj, len(list.Notes))
			for i, n := range list.Notes {
				out[i] = noteBody(n, false)
			}
			o := obj{"project": list.Project.Slug, "notes": out}
			if list.Truncated {
				o["truncated"] = true
				o["note"] = listNote
			}
			return o, nil
		})

	addTool(srv, s, "get_note",
		"Read one scratchpad note in full. Next: add_note to follow up (notes are never edited).",
		true, func(ctx context.Context, actor domain.Actor, in getNoteArgs) (any, error) {
			id, err := noteID(in.Note)
			if err != nil {
				return nil, err
			}
			p, err := s.svc.ResolveProject(ctx, s.ref(in.Project))
			if err != nil {
				return nil, err
			}
			n, err := s.svc.GetNote(ctx, service.ProjectRef{Slug: p.Slug, Cwd: s.cwd}, id)
			if err != nil {
				return nil, err
			}
			o := noteBody(n, true)
			o["project"] = p.Slug
			return o, nil
		})
}
