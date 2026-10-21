package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"skald/internal/domain"
	"skald/internal/service"
)

type noteJSON struct {
	ID        string `json:"id"`
	Task      string `json:"task,omitempty"`
	Feature   string `json:"feature,omitempty"`
	Body      string `json:"body"`
	Actor     string `json:"actor"`
	CreatedAt string `json:"created_at"`
}

func toNote(n domain.Note) noteJSON {
	j := noteJSON{ID: noteID(n.ID), Body: n.Body, Actor: string(n.Actor), CreatedAt: ts(n.CreatedAt)}
	if n.TaskID != 0 {
		j.Task = taskID(n.TaskID)
	}
	if n.FeatureID != 0 {
		j.Feature = featureID(n.FeatureID)
	}
	return j
}

func noteID(id int64) string { return domain.FormatID(domain.EntityNote, id) }

// optArg parses an optional ID flag; "" is 0.
func optArg(parse func(string) (int64, error), s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return parse(s)
}

// noteLinks renders "T-3, F-1" for a note, or "" when it is not linked.
func noteLinks(n domain.Note) string {
	var parts []string
	if n.TaskID != 0 {
		parts = append(parts, taskID(n.TaskID))
	}
	if n.FeatureID != 0 {
		parts = append(parts, featureID(n.FeatureID))
	}
	return strings.Join(parts, ", ")
}

func newNoteCmd(a *app) *cobra.Command {
	return group("note", "Keep a scratchpad of notes about the project",
		newNoteAddCmd(a), newNoteListCmd(a), newNoteShowCmd(a))
}

func newNoteAddCmd(a *app) *cobra.Command {
	var task, feature string
	c := &cobra.Command{
		Use:   "add <text...>",
		Short: "Add a note to the project's scratchpad",
		Long: `Adds a note to the project's scratchpad: a finding, an open question, or
where you left off. Notes are append-only and never change progress. Link one
to a task or feature with --task or --feature. Use "-" to read standard input.`,
		Example: `  skald note add "auth tokens expire after 15 minutes in staging"
  skald note add --task T-4 "tried bcrypt cost 14, too slow on CI"
  git log -1 --format=%B | skald note add -`,
		Args: minArgs(1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			t, err := optArg(taskArg, task)
			if err != nil {
				return err
			}
			f, err := optArg(featureArg, feature)
			if err != nil {
				return err
			}
			text := strings.Join(args, " ")
			if len(args) == 1 && args[0] == "-" {
				b, err := io.ReadAll(c.InOrStdin())
				if err != nil {
					return err
				}
				text = string(b)
			}
			n, err := e.svc.AddNote(e.ctx, e.actor, e.ref, service.AddNoteInput{Body: text, TaskID: t, FeatureID: f})
			if err != nil {
				return err
			}
			return e.emit(toNote(n), func(w io.Writer) {
				if l := noteLinks(n); l != "" {
					fmt.Fprintf(w, "added %s (%s)\n", noteID(n.ID), l)
					return
				}
				fmt.Fprintf(w, "added %s\n", noteID(n.ID))
			})
		}),
	}
	c.Flags().StringVar(&task, "task", "", "link the note to this task")
	c.Flags().StringVar(&feature, "feature", "", "link the note to this feature")
	return c
}

func newNoteListCmd(a *app) *cobra.Command {
	var task, feature, search, author string
	var limit int
	c := &cobra.Command{
		Use:   "list",
		Short: "List scratchpad notes, newest first",
		Example: `  skald note list
  skald note list --task T-4
  skald note list --search "rate limit" --author human`,
		Args: noArgs,
		RunE: a.do(func(e *env, c *cobra.Command, _ []string) error {
			t, err := optArg(taskArg, task)
			if err != nil {
				return err
			}
			f, err := optArg(featureArg, feature)
			if err != nil {
				return err
			}
			list, err := e.svc.ListNotes(e.ctx, e.ref, service.NoteFilter{
				TaskID: t, FeatureID: f, Actor: domain.Actor(author), Query: search, Limit: limit,
			})
			if err != nil {
				return err
			}
			out := struct {
				Notes     []noteJSON `json:"notes"`
				Truncated bool       `json:"truncated"`
			}{Notes: make([]noteJSON, 0, len(list.Notes)), Truncated: list.Truncated}
			for _, n := range list.Notes {
				out.Notes = append(out.Notes, toNote(n))
			}
			return e.emit(out, func(w io.Writer) {
				if len(list.Notes) == 0 {
					fmt.Fprintln(w, "no notes; add one with `skald note add <text>`")
					return
				}
				tw := table(w)
				fmt.Fprintln(tw, "ID\tWHEN\tBY\tLINK\tNOTE")
				for _, n := range list.Notes {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", noteID(n.ID), n.CreatedAt.UTC().Format("2006-01-02 15:04"), n.Actor, noteLinks(n), trunc(n.Body, 70))
				}
				tw.Flush()
				if list.Truncated {
					fmt.Fprintln(w, "more notes exist; raise --limit or narrow the filters")
				}
			})
		}),
	}
	c.Flags().StringVar(&task, "task", "", "only notes linked to this task")
	c.Flags().StringVar(&feature, "feature", "", "only notes linked to this feature (includes its tasks' notes)")
	c.Flags().StringVar(&search, "search", "", "only notes containing this text")
	c.Flags().StringVar(&author, "author", "", "only notes by this actor, such as human")
	c.Flags().IntVar(&limit, "limit", service.DefaultNoteLimit, fmt.Sprintf("max notes to show (up to %d)", service.MaxNoteLimit))
	return c
}

func newNoteShowCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "show <note>",
		Short:   "Show one note in full",
		Example: `  skald note show N-3`,
		Args:    exactArgs(1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			id, err := idArg(domain.EntityNote, args[0])
			if err != nil {
				return err
			}
			n, err := e.svc.GetNote(e.ctx, e.ref, id)
			if err != nil {
				return err
			}
			return e.emit(toNote(n), func(w io.Writer) {
				fmt.Fprintf(w, "%s by %s at %s\n", noteID(n.ID), n.Actor, ts(n.CreatedAt))
				if l := noteLinks(n); l != "" {
					fmt.Fprintf(w, "linked to %s\n", l)
				}
				fmt.Fprintf(w, "\n%s\n", n.Body)
			})
		}),
	}
}
