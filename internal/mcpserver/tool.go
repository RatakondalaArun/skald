package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"skald/internal/domain"
	"skald/internal/service"
)

// obj is a terse JSON response body.
type obj map[string]any

type handler[In any] func(ctx context.Context, actor domain.Actor, in In) (any, error)

// addTool registers a tool whose result is compact JSON text. Errors become
// tool errors (isError) carrying the service's next-step message verbatim.
func addTool[In any](srv *mcp.Server, s *server, name, desc string, readOnly bool, h handler[In]) {
	tool := &mcp.Tool{Name: name, Description: desc}
	if readOnly {
		tool.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: true}
	}
	mcp.AddTool(srv, tool, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		actor := s.actor(req)
		s.log.Debug("tool call", "tool", name, "actor", actor)
		v, err := h(ctx, actor, in)
		if err != nil {
			return nil, nil, s.toolErr(name, err)
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, nil, s.toolErr(name, err)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
	})
}

// toolErr passes domain errors through verbatim and hides everything else
// behind a generic message (the detail goes to the log).
func (s *server) toolErr(tool string, err error) error {
	var de domain.Error
	if errors.As(err, &de) {
		s.log.Debug("tool error", "tool", tool, "kind", de.Kind().String())
		return errors.New(de.Message())
	}
	s.log.Error("tool failed", "tool", tool, "err", err)
	return fmt.Errorf("internal error in %s: %v. Retry the call; if it keeps failing, check that the skald database is reachable (run `skald status`)", tool, err)
}

// ---- ids -------------------------------------------------------------

func taskID(s string) (int64, error)    { return domain.ParseID(domain.EntityTask, s) }
func featureID(s string) (int64, error) { return domain.ParseID(domain.EntityFeature, s) }

func tid(id int64) string { return domain.FormatID(domain.EntityTask, id) }
func fid(id int64) string { return domain.FormatID(domain.EntityFeature, id) }
func pid(id int64) string { return domain.FormatID(domain.EntityProject, id) }
func aid(id int64) string { return domain.FormatID(domain.EntityArtifact, id) }

func taskIDs(ids []int64) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = tid(id)
	}
	return out
}

// ---- priority --------------------------------------------------------

var priorityNames = []string{"urgent", "high", "normal", "low"}

func priorityName(p int) string {
	if p >= 0 && p < len(priorityNames) {
		return priorityNames[p]
	}
	return fmt.Sprint(p)
}

// parsePriority maps a name to the stored number; "" means default (nil).
func parsePriority(s string) (*int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return nil, nil
	}
	for i, n := range priorityNames {
		if s == n {
			return &i, nil
		}
	}
	return nil, &domain.ValidationError{Field: "priority", Problem: fmt.Sprintf("%q is not a priority", s), Hint: "Use urgent, high, normal or low."}
}

// ---- rendering -------------------------------------------------------

func pct(f float64) float64 { return math.Round(f*1000) / 10 }

func tm(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func counts(c service.TaskCounts) obj {
	return obj{"todo": c.Todo, "in_progress": c.InProgress, "blocked": c.Blocked, "done": c.Done, "cancelled": c.Cancelled}
}

func statusCounts(c service.StatusCounts) obj {
	return obj{
		"todo": c[domain.TaskTodo], "in_progress": c[domain.TaskInProgress], "blocked": c[domain.TaskBlocked],
		"done": c[domain.TaskDone], "cancelled": c[domain.TaskCancelled],
	}
}

type taskLine struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Feature  string `json:"feature,omitempty"`
	Priority string `json:"priority,omitempty"`
	Assignee string `json:"assignee,omitempty"`
	Version  int64  `json:"version,omitempty"`
	Reason   string `json:"blocked_reason,omitempty"`
}

func line(t domain.Task) taskLine {
	return taskLine{
		ID: tid(t.ID), Title: t.Title, Status: string(t.Status), Feature: fid(t.FeatureID),
		Priority: priorityName(t.Priority), Assignee: t.Assignee, Version: t.Version, Reason: t.BlockedReason,
	}
}

func lines(ts []domain.Task) []taskLine {
	out := make([]taskLine, len(ts))
	for i, t := range ts {
		out[i] = line(t)
	}
	return out
}

func artifacts(as []domain.Artifact) []obj {
	out := make([]obj, len(as))
	for i, a := range as {
		o := obj{"id": aid(a.ID), "kind": string(a.Kind), "ref": a.Ref}
		if a.Summary != "" {
			o["summary"] = a.Summary
		}
		out[i] = o
	}
	return out
}

func progressBody(rep service.ProgressReport) obj {
	feats := make([]obj, len(rep.Features))
	for i, f := range rep.Features {
		feats[i] = obj{
			"id": fid(f.Feature.ID), "title": f.Feature.Title, "status": string(f.Status),
			"percent": pct(f.Progress.Fraction), "tasks": counts(f.Counts),
		}
	}
	blocked := make([]obj, len(rep.Blocked))
	for i, b := range rep.Blocked {
		blocked[i] = obj{"id": tid(b.Task.ID), "title": b.Task.Title, "reason": b.Reason}
	}
	stale := make([]obj, len(rep.Stale))
	for i, t := range rep.Stale {
		stale[i] = obj{"id": tid(t.ID), "title": t.Title, "assignee": t.Assignee}
	}
	o := obj{
		"project": rep.Project.Slug, "percent": pct(rep.Progress.Fraction),
		"tasks": counts(rep.Counts), "ready": rep.ReadyCount, "features": feats,
	}
	if len(blocked) > 0 {
		o["blocked"] = blocked
	}
	if len(stale) > 0 {
		o["stale"] = stale
	}
	return o
}

// listNote is added to truncated lists.
const listNote = "more results exist; narrow the filters or pass offset"
