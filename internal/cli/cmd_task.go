package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"skald/internal/domain"
	"skald/internal/service"
)

func newTaskCmd(a *app) *cobra.Command {
	return group("task", "Create tasks and move them through their lifecycle",
		newTaskAddCmd(a), newTaskListCmd(a), newTaskShowCmd(a),
		newTaskStartCmd(a), newTaskDoneCmd(a), newTaskBlockCmd(a), newTaskUnblockCmd(a),
		newTaskCancelCmd(a), newTaskReopenCmd(a), newTaskNoteCmd(a), newTaskDepCmd(a))
}

func newTaskAddCmd(a *app) *cobra.Command {
	var feature, desc, criteria, priority, key string
	var weight, position int
	c := &cobra.Command{
		Use:     "add <title>",
		Short:   "Add a task to a feature",
		Example: `  skald task add "Login form" --feature F-1 --priority 1 --criteria "Rejects bad passwords"`,
		Args:    exactArgs(1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			if feature == "" {
				return usagef("--feature is required (list features with `skald feature list`)")
			}
			fid, err := featureArg(feature)
			if err != nil {
				return err
			}
			in := service.CreateTaskInput{
				FeatureID: fid, Title: args[0], Description: desc, AcceptanceCriteria: criteria,
				Weight: weight, Position: position, IdempotencyKey: key,
			}
			if priority != "" {
				p, err := parsePriority(priority)
				if err != nil {
					return err
				}
				in.Priority = &p
			}
			t, created, err := e.svc.CreateTask(e.ctx, e.actor, e.ref, in)
			if err != nil {
				return err
			}
			return e.emit(toTask(t), func(w io.Writer) {
				verb := "added"
				if !created {
					verb = "already exists:"
				}
				fmt.Fprintf(w, "%s %s %s\n", verb, taskID(t.ID), t.Title)
			})
		}),
	}
	c.Flags().StringVar(&feature, "feature", "", "feature ID, such as F-1 (required)")
	c.Flags().StringVar(&desc, "description", "", "task description")
	c.Flags().StringVar(&criteria, "criteria", "", "acceptance criteria")
	c.Flags().StringVar(&priority, "priority", "", "0 (urgent) to 3 (low); default 2")
	c.Flags().IntVar(&weight, "weight", 1, "relative weight in feature progress (1-10)")
	c.Flags().IntVar(&position, "position", 0, "position in the feature (0 = last)")
	c.Flags().StringVar(&key, "key", "", "idempotency key: repeating the same key returns the existing task")
	return c
}

func newTaskListCmd(a *app) *cobra.Command {
	var feature string
	var statuses []string
	var ready bool
	var limit, offset int
	c := &cobra.Command{
		Use:   "list",
		Short: "List tasks, most urgent first",
		Example: `  skald task list --status in_progress
  skald task list --ready --feature F-2`,
		Args: noArgs,
		RunE: a.do(func(e *env, c *cobra.Command, _ []string) error {
			f := service.ListTasksFilter{ReadyOnly: ready, Limit: limit, Offset: offset}
			if feature != "" {
				id, err := featureArg(feature)
				if err != nil {
					return err
				}
				f.FeatureID = id
			}
			for _, s := range statuses {
				st := domain.TaskStatus(strings.ReplaceAll(strings.ToLower(s), "-", "_"))
				if !st.Valid() {
					return usagef("unknown status %q (use todo, in_progress, blocked, done, cancelled)", s)
				}
				f.Statuses = append(f.Statuses, st)
			}
			l, err := e.svc.ListTasks(e.ctx, e.actor, e.ref, f)
			if err != nil {
				return err
			}
			out := struct {
				Tasks     []taskJSON `json:"tasks"`
				Truncated bool       `json:"truncated"`
			}{toTasks(l.Tasks), l.Truncated}
			err = e.emit(out, func(w io.Writer) { writeTaskTable(w, l.Tasks) })
			if err == nil && l.Truncated && !e.json {
				fmt.Fprintf(e.errw, "more tasks exist; use --offset %d to see them\n", offset+len(l.Tasks))
			}
			return err
		}),
	}
	c.Flags().StringVar(&feature, "feature", "", "only tasks of this feature")
	c.Flags().StringSliceVar(&statuses, "status", nil, "only these statuses (repeatable)")
	c.Flags().BoolVar(&ready, "ready", false, "only tasks that can start now")
	c.Flags().IntVar(&limit, "limit", 0, "maximum tasks (0 = default 50, max 200)")
	c.Flags().IntVar(&offset, "offset", 0, "skip this many tasks")
	return c
}

func writeTaskTable(w io.Writer, tasks []domain.Task) {
	if len(tasks) == 0 {
		fmt.Fprintln(w, "no matching tasks")
		return
	}
	t := table(w)
	fmt.Fprintln(t, "ID\tSTATUS\tPRI\tFEATURE\tTITLE")
	for _, k := range tasks {
		fmt.Fprintf(t, "%s\t%s\tp%d\t%s\t%s\n", taskID(k.ID), k.Status, k.Priority, featureID(k.FeatureID), trunc(k.Title, 70))
	}
	t.Flush()
}

func newTaskShowCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "show <task>",
		Short: "Show a task with dependencies, evidence and history",
		Args:  exactArgs(1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			id, err := taskArg(args[0])
			if err != nil {
				return err
			}
			d, err := e.svc.GetTask(e.ctx, e.actor, e.ref, id)
			if err != nil {
				return err
			}
			return e.emit(toTaskDetail(d), func(w io.Writer) { writeTaskDetail(w, d) })
		}),
	}
}

func writeTaskDetail(w io.Writer, d service.TaskDetail) {
	t := d.Task
	fmt.Fprintf(w, "%s  %s\n", taskID(t.ID), t.Title)
	fmt.Fprintf(w, "status      %s (version %d)\n", t.Status, t.Version)
	fmt.Fprintf(w, "feature     %s %s\n", featureID(d.Feature.ID), d.Feature.Title)
	fmt.Fprintf(w, "priority    p%d    weight %d\n", t.Priority, t.Weight)
	if t.Assignee != "" {
		fmt.Fprintf(w, "assignee    %s\n", t.Assignee)
	}
	if t.StartedAt != nil {
		fmt.Fprintf(w, "started     %s\n", ts(*t.StartedAt))
	}
	if t.CompletedAt != nil {
		fmt.Fprintf(w, "completed   %s\n", ts(*t.CompletedAt))
	}
	if t.BlockedReason != "" {
		fmt.Fprintf(w, "blocked     %s\n", t.BlockedReason)
	}
	switch {
	case t.Status == domain.TaskTodo && d.Ready:
		fmt.Fprintln(w, "ready       yes")
	case len(d.UnmetDependencies) > 0 && !t.Status.Terminal():
		ids := make([]string, len(d.UnmetDependencies))
		for i, id := range d.UnmetDependencies {
			ids[i] = taskID(id)
		}
		fmt.Fprintf(w, "waiting on  %s\n", strings.Join(ids, ", "))
	}
	if d.Stale {
		fmt.Fprintln(w, "stale       in progress with no activity for over 4 hours")
	}
	if t.Description != "" {
		fmt.Fprintf(w, "\nDescription\n%s\n", indent(t.Description))
	}
	if t.AcceptanceCriteria != "" {
		fmt.Fprintf(w, "\nAcceptance criteria\n%s\n", indent(t.AcceptanceCriteria))
	}
	if len(d.Dependencies) > 0 {
		fmt.Fprintln(w, "\nDepends on")
		for _, k := range d.Dependencies {
			fmt.Fprintf(w, "  %s  %s [%s]\n", taskID(k.ID), trunc(k.Title, 60), k.Status)
		}
	}
	if len(d.Dependents) > 0 {
		fmt.Fprintln(w, "\nBlocks")
		for _, k := range d.Dependents {
			fmt.Fprintf(w, "  %s  %s [%s]\n", taskID(k.ID), trunc(k.Title, 60), k.Status)
		}
	}
	if len(d.Artifacts) > 0 {
		fmt.Fprintln(w, "\nEvidence")
		for _, a := range d.Artifacts {
			line := fmt.Sprintf("  %s  %s %s", domain.FormatID(domain.EntityArtifact, a.ID), a.Kind, a.Ref)
			if a.Summary != "" {
				line += " - " + trunc(a.Summary, 60)
			}
			fmt.Fprintln(w, line+"  ("+a.Actor.String()+")")
		}
	}
	if len(d.RecentEvents) > 0 {
		fmt.Fprintln(w, "\nRecent activity")
		tw := table(w)
		for _, ev := range d.RecentEvents {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", ev.CreatedAt.UTC().Format("2006-01-02 15:04:05"), ev.Actor, ev.Type, trunc(string(ev.Payload), 60))
		}
		tw.Flush()
	}
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}

// printTask prints the task after a change.
func printTask(e *env, verb string, t domain.Task) error {
	return e.emit(toTask(t), func(w io.Writer) {
		fmt.Fprintf(w, "%s %s: %s (now %s)\n", taskID(t.ID), verb, t.Title, t.Status)
	})
}

// versionFlag adds the optimistic-concurrency flag.
func versionFlag(c *cobra.Command, v *int64) {
	c.Flags().Int64Var(v, "version", 0, "fail unless the task is at this version (see `task show`)")
}

func newTaskStartCmd(a *app) *cobra.Command {
	var version int64
	c := &cobra.Command{
		Use:   "start <task>",
		Short: "Start a task (all of its dependencies must be done)",
		Args:  exactArgs(1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			id, err := taskArg(args[0])
			if err != nil {
				return err
			}
			t, err := e.svc.StartTask(e.ctx, e.actor, id, version)
			if err != nil {
				return err
			}
			return printTask(e, "started", t)
		}),
	}
	versionFlag(c, &version)
	return c
}

func newTaskDoneCmd(a *app) *cobra.Command {
	var version int64
	var note string
	var force bool
	c := &cobra.Command{
		Use:   "done <task>",
		Short: "Complete a task (needs evidence unless the project waives it)",
		Long: `Completes a task. When the project requires evidence (the default), at least
one artifact must be attached first: see 'skald evidence add'. --force skips
that check; it is for humans only and is recorded in the task's history.`,
		Example: `  skald evidence add T-3 --kind test_report --ref ci/run/812
  skald task done T-3 --note "all acceptance criteria met"`,
		Args: exactArgs(1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			id, err := taskArg(args[0])
			if err != nil {
				return err
			}
			t, err := e.svc.CompleteTask(e.ctx, e.actor, id, service.CompleteOptions{ExpectedVersion: version, Note: note, Force: force})
			if err != nil {
				return err
			}
			return printTask(e, "completed", t)
		}),
	}
	c.Flags().StringVar(&note, "note", "", "add a note to the task as it completes")
	c.Flags().BoolVar(&force, "force", false, "complete without evidence (humans only)")
	versionFlag(c, &version)
	return c
}

func newTaskBlockCmd(a *app) *cobra.Command {
	var reason string
	var version int64
	c := &cobra.Command{
		Use:     "block <task> --reason <text>",
		Short:   "Record a blocker that stops a task",
		Example: `  skald task block T-4 --reason "waiting for the payments sandbox keys"`,
		Args:    exactArgs(1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			id, err := taskArg(args[0])
			if err != nil {
				return err
			}
			if strings.TrimSpace(reason) == "" {
				return usagef("--reason is required")
			}
			t, err := e.svc.UpdateTaskStatus(e.ctx, e.actor, id, service.StatusChange{To: domain.TaskBlocked, BlockedReason: reason, ExpectedVersion: version})
			if err != nil {
				return err
			}
			return printTask(e, "blocked", t)
		}),
	}
	c.Flags().StringVar(&reason, "reason", "", "what is blocking the task (required)")
	versionFlag(c, &version)
	return c
}

func newTaskUnblockCmd(a *app) *cobra.Command {
	var version int64
	var to string
	c := &cobra.Command{
		Use:   "unblock <task>",
		Short: "Clear a blocker",
		Long: `Clears a blocker. The task returns to in_progress if it had been started, and
to todo otherwise; use --to to choose.`,
		Args: exactArgs(1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			id, err := taskArg(args[0])
			if err != nil {
				return err
			}
			var target domain.TaskStatus
			switch to {
			case "":
				d, err := e.svc.GetTask(e.ctx, e.actor, e.ref, id)
				if err != nil {
					return err
				}
				target = domain.TaskTodo
				if d.Task.StartedAt != nil {
					target = domain.TaskInProgress
				}
			case "todo":
				target = domain.TaskTodo
			case "in_progress":
				target = domain.TaskInProgress
			default:
				return usagef("--to must be todo or in_progress")
			}
			t, err := e.svc.UpdateTaskStatus(e.ctx, e.actor, id, service.StatusChange{To: target, ExpectedVersion: version})
			if err != nil {
				return err
			}
			return printTask(e, "unblocked", t)
		}),
	}
	c.Flags().StringVar(&to, "to", "", "todo or in_progress (default: where it was before)")
	versionFlag(c, &version)
	return c
}

func newTaskCancelCmd(a *app) *cobra.Command {
	var version int64
	c := &cobra.Command{
		Use:   "cancel <task>",
		Short: "Cancel a task (it no longer counts toward progress or blocks others)",
		Args:  exactArgs(1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			id, err := taskArg(args[0])
			if err != nil {
				return err
			}
			t, err := e.svc.UpdateTaskStatus(e.ctx, e.actor, id, service.StatusChange{To: domain.TaskCancelled, ExpectedVersion: version})
			if err != nil {
				return err
			}
			return printTask(e, "cancelled", t)
		}),
	}
	versionFlag(c, &version)
	return c
}

func newTaskReopenCmd(a *app) *cobra.Command {
	var version int64
	c := &cobra.Command{
		Use:   "reopen <task>",
		Short: "Put a done or cancelled task back to todo",
		Args:  exactArgs(1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			id, err := taskArg(args[0])
			if err != nil {
				return err
			}
			t, err := e.svc.UpdateTaskStatus(e.ctx, e.actor, id, service.StatusChange{To: domain.TaskTodo, ExpectedVersion: version})
			if err != nil {
				return err
			}
			return printTask(e, "reopened", t)
		}),
	}
	versionFlag(c, &version)
	return c
}

func newTaskNoteCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "note <task> <text...>",
		Short:   "Add a progress note to a task",
		Example: `  skald task note T-2 "schema drafted, waiting on review"`,
		Args:    minArgs(2),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			id, err := taskArg(args[0])
			if err != nil {
				return err
			}
			text := strings.Join(args[1:], " ")
			ev, err := e.svc.AddTaskNote(e.ctx, e.actor, id, text)
			if err != nil {
				return err
			}
			out := struct {
				Task  string `json:"task"`
				Event int64  `json:"event"`
			}{taskID(id), ev}
			return e.emit(out, func(w io.Writer) { fmt.Fprintf(w, "noted on %s\n", taskID(id)) })
		}),
	}
}

func newTaskDepCmd(a *app) *cobra.Command {
	dep := func(use, short string, add bool) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  exactArgs(2),
			RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
				id, err := taskArg(args[0])
				if err != nil {
					return err
				}
				on, err := taskArg(args[1])
				if err != nil {
					return err
				}
				var changed bool
				if add {
					changed, err = e.svc.AddDependency(e.ctx, e.actor, id, on)
				} else {
					changed, err = e.svc.RemoveDependency(e.ctx, e.actor, id, on)
				}
				if err != nil {
					return err
				}
				out := struct {
					Task      string `json:"task"`
					DependsOn string `json:"depends_on"`
					Changed   bool   `json:"changed"`
				}{taskID(id), taskID(on), changed}
				return e.emit(out, func(w io.Writer) {
					switch {
					case add && changed:
						fmt.Fprintf(w, "%s now depends on %s\n", taskID(id), taskID(on))
					case add:
						fmt.Fprintf(w, "%s already depends on %s\n", taskID(id), taskID(on))
					case changed:
						fmt.Fprintf(w, "%s no longer depends on %s\n", taskID(id), taskID(on))
					default:
						fmt.Fprintf(w, "%s did not depend on %s\n", taskID(id), taskID(on))
					}
				})
			}),
		}
	}
	return group("dep", "Add or remove task dependencies",
		dep("add <task> <depends-on>", "Make <task> wait for <depends-on>", true),
		dep("remove <task> <depends-on>", "Remove a dependency", false))
}
