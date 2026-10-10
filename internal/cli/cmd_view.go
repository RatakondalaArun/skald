package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"skald/internal/domain"
	"skald/internal/service"
)

func newNextCmd(a *app) *cobra.Command {
	var limit int
	c := &cobra.Command{
		Use:   "next",
		Short: "Show the tasks that can start now",
		Long:  "Lists todo tasks whose dependencies are all done or cancelled, most urgent first.",
		Args:  noArgs,
		RunE: a.do(func(e *env, c *cobra.Command, _ []string) error {
			r, err := e.svc.NextTasks(e.ctx, e.ref, limit)
			if err != nil {
				return err
			}
			out := struct {
				Project   string     `json:"project"`
				Tasks     []taskJSON `json:"tasks"`
				Total     int        `json:"total"`
				Truncated bool       `json:"truncated"`
			}{r.Project.Slug, toTasks(r.Tasks), r.Total, r.Truncated}
			return e.emit(out, func(w io.Writer) {
				if len(r.Tasks) == 0 {
					fmt.Fprintln(w, "nothing is ready to start")
					return
				}
				writeTaskTable(w, r.Tasks)
				if r.Truncated {
					fmt.Fprintf(w, "(%d of %d ready tasks shown; use --limit)\n", len(r.Tasks), r.Total)
				}
			})
		}),
	}
	c.Flags().IntVar(&limit, "limit", 0, "maximum tasks (default 10, max 50)")
	return c
}

func newStatusCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show progress, blockers and what is ready",
		Args:  noArgs,
		RunE: a.do(func(e *env, c *cobra.Command, _ []string) error {
			rep, err := e.svc.ProjectProgress(e.ctx, e.ref)
			if err != nil {
				return err
			}
			// Same project, resolved explicitly so both reads agree on it.
			ready, err := e.svc.NextTasks(e.ctx, service.ProjectRef{Slug: rep.Project.Slug}, 10)
			if err != nil {
				return err
			}
			type blockedJSON struct {
				Task   taskJSON `json:"task"`
				Reason string   `json:"reason"`
			}
			out := struct {
				Project  projectJSON   `json:"project"`
				Progress progressJSON  `json:"progress"`
				Counts   countsJSON    `json:"counts"`
				Features []featureJSON `json:"features"`
				Blocked  []blockedJSON `json:"blocked"`
				Ready    []taskJSON    `json:"ready"`
				ReadyCnt int           `json:"ready_count"`
				Stale    []taskJSON    `json:"stale"`
			}{
				Project: toProject(rep.Project), Progress: toProgress(rep.Progress), Counts: toTaskCounts(rep.Counts),
				Features: []featureJSON{}, Blocked: []blockedJSON{}, Ready: toTasks(ready.Tasks),
				ReadyCnt: rep.ReadyCount, Stale: toTasks(rep.Stale),
			}
			for _, f := range rep.Features {
				j := toFeature(f.Feature)
				j.Status = string(f.Status)
				p, cn, counted := toProgress(f.Progress), toTaskCounts(f.Counts), f.Counted
				j.Progress, j.Counts, j.Counted = &p, &cn, &counted
				out.Features = append(out.Features, j)
			}
			for _, b := range rep.Blocked {
				out.Blocked = append(out.Blocked, blockedJSON{toTask(b.Task), b.Reason})
			}
			return e.emit(out, func(w io.Writer) { writeStatus(w, rep, ready) })
		}),
	}
}

func writeStatus(w io.Writer, rep service.ProgressReport, ready service.NextTasksResult) {
	p := rep.Project
	fmt.Fprintf(w, "%s  %s (%s)\n", projectID(p.ID), p.Name, p.Slug)
	active := rep.Counts.Total - rep.Counts.Cancelled
	fmt.Fprintf(w, "progress  %s %s   %d/%d tasks done", bar(rep.Progress.Fraction, 24), pct(rep.Progress.Fraction), rep.Counts.Done, active)
	if rep.Counts.Cancelled > 0 {
		fmt.Fprintf(w, " (%d cancelled)", rep.Counts.Cancelled)
	}
	fmt.Fprintf(w, "\n          %d in progress, %d blocked, %d ready, %d todo\n", rep.Counts.InProgress, rep.Counts.Blocked, rep.ReadyCount, rep.Counts.Todo)

	if len(rep.Features) > 0 {
		fmt.Fprintln(w, "\nFeatures")
		t := table(w)
		for _, f := range rep.Features {
			note := ""
			if !f.Counted {
				note = "  (not counted)"
			}
			fmt.Fprintf(t, "  %s\t%s\t%s %s\t%s\t%d/%d%s\n", featureID(f.Feature.ID), trunc(f.Feature.Title, 40),
				bar(f.Progress.Fraction, 16), pct(f.Progress.Fraction), f.Status, f.Counts.Done, f.Counts.Total-f.Counts.Cancelled, note)
		}
		t.Flush()
	}
	if len(rep.Blocked) > 0 {
		fmt.Fprintf(w, "\nBlocked (%d)\n", len(rep.Blocked))
		for _, b := range rep.Blocked {
			fmt.Fprintf(w, "  %s  %s: %s\n", taskID(b.Task.ID), trunc(b.Task.Title, 50), b.Reason)
		}
	}
	if len(ready.Tasks) > 0 {
		fmt.Fprintf(w, "\nReady (%d)\n", ready.Total)
		for _, k := range ready.Tasks {
			fmt.Fprintf(w, "  %s  p%d  %s\n", taskID(k.ID), k.Priority, trunc(k.Title, 70))
		}
		if ready.Truncated {
			fmt.Fprintf(w, "  ... and %d more (skald next --limit 50)\n", ready.Total-len(ready.Tasks))
		}
	}
	if len(rep.Stale) > 0 {
		fmt.Fprintf(w, "\nStale (in progress, no activity for over 4 hours)\n")
		for _, k := range rep.Stale {
			fmt.Fprintf(w, "  %s  %s  (%s)\n", taskID(k.ID), trunc(k.Title, 50), k.Assignee)
		}
	}
}

func newLogCmd(a *app) *cobra.Command {
	var limit int
	var since int64
	var allProjects bool
	c := &cobra.Command{
		Use:   "log",
		Short: "Show recent activity",
		Long: `Shows the project's change history, oldest first. Without --since it shows the
latest --limit events. With --since N it shows events after cursor N, so a
script can page through the log: the last event's id is the next cursor.`,
		Example: `  skald log --limit 20
  skald log --since 120 --json`,
		Args: noArgs,
		RunE: a.do(func(e *env, c *cobra.Command, _ []string) error {
			if limit <= 0 {
				limit = 30
			}
			var pid int64
			if !allProjects {
				o, err := e.svc.GetProject(e.ctx, e.actor, e.ref)
				if err != nil {
					return err
				}
				pid = o.Project.ID
			}
			var events []domain.Event
			var cursor int64
			var more bool
			if c.Flags().Changed("since") {
				page, err := e.svc.EventsSince(e.ctx, pid, since, limit)
				if err != nil {
					return err
				}
				events, cursor, more = page.Events, page.Cursor, page.More
			} else {
				var err error
				events, err = recentEvents(e, pid, limit)
				if err != nil {
					return err
				}
				cursor = since
				if n := len(events); n > 0 {
					cursor = events[n-1].ID
				}
			}
			out := struct {
				Events []eventJSON `json:"events"`
				Cursor int64       `json:"cursor"`
				More   bool        `json:"more"`
			}{toEvents(events), cursor, more}
			return e.emit(out, func(w io.Writer) {
				if len(events) == 0 {
					fmt.Fprintln(w, "no events")
					return
				}
				t := table(w)
				for _, ev := range events {
					fmt.Fprintf(t, "%d\t%s\t%s\t%s\t%s\t%s\n", ev.ID, ev.CreatedAt.UTC().Format("2006-01-02 15:04:05"), ev.Actor,
						ev.Type, domain.FormatID(ev.EntityType, ev.EntityID), eventDetail(ev))
				}
				t.Flush()
				if more {
					fmt.Fprintf(w, "more events: use --since %d\n", cursor)
				}
			})
		}),
	}
	c.Flags().IntVar(&limit, "limit", 30, "maximum events (max 1000)")
	c.Flags().Int64Var(&since, "since", 0, "show events after this cursor (an event id)")
	c.Flags().BoolVar(&allProjects, "all-projects", false, "show events of every project")
	return c
}

// recentEvents returns the last n events of a project (0 = all projects),
// oldest first. The service pages forward from a cursor, so start near the
// end and widen the window until n events are found.
func recentEvents(e *env, pid int64, n int) ([]domain.Event, error) {
	latest, err := e.svc.LatestEventID(e.ctx, 0)
	if err != nil {
		return nil, err
	}
	window := int64(n) * 2
	for {
		cursor := max(latest-window, 0)
		var events []domain.Event
		for {
			page, err := e.svc.EventsSince(e.ctx, pid, cursor, service.MaxEventLimit)
			if err != nil {
				return nil, err
			}
			events = append(events, page.Events...)
			if !page.More {
				break
			}
			cursor = page.Cursor
		}
		if len(events) >= n || latest-window <= 0 {
			if len(events) > n {
				events = events[len(events)-n:]
			}
			return events, nil
		}
		window *= 4
	}
}

// eventDetail renders an event payload on one line.
func eventDetail(ev domain.Event) string {
	var m map[string]any
	if err := json.Unmarshal(ev.Payload, &m); err != nil || len(m) == 0 {
		return ""
	}
	// Prefer the human-readable fields when present.
	for _, k := range []string{"text", "note", "summary", "title"} {
		if s, ok := m[k].(string); ok && s != "" {
			return trunc(s, 70)
		}
	}
	b, _ := json.Marshal(m)
	return trunc(strings.TrimSpace(string(b)), 70)
}
