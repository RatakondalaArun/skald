package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"skald/internal/config"
	"skald/internal/domain"
	"skald/internal/service"
	"skald/internal/store"
)

// usageError is a mistake in how the command was invoked. It exits with 2.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return &usageError{msg: fmt.Sprintf(format, a...)} }

// Argument validators that report usage errors (cobra's own return plain errors).

func noArgs(c *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usagef("unknown command %q for %q", args[0], c.CommandPath())
	}
	return nil
}

func exactArgs(n int) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if len(args) != n {
			return usagef("%s takes %d argument(s), got %d\nUsage: %s", c.CommandPath(), n, len(args), c.UseLine())
		}
		return nil
	}
}

func rangeArgs(min, max int) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if len(args) < min || len(args) > max {
			return usagef("%s takes %d to %d argument(s), got %d\nUsage: %s", c.CommandPath(), min, max, len(args), c.UseLine())
		}
		return nil
	}
}

func minArgs(n int) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if len(args) < n {
			return usagef("%s needs at least %d argument(s), got %d\nUsage: %s", c.CommandPath(), n, len(args), c.UseLine())
		}
		return nil
	}
}

// group makes a command that only holds subcommands.
func group(use, short string, subs ...*cobra.Command) *cobra.Command {
	c := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  noArgs,
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	c.AddCommand(subs...)
	return c
}

// app holds the global flags.
type app struct {
	project string
	json    bool
}

// env is what a command handler works with: an open service plus settings.
type env struct {
	ctx   context.Context
	svc   *service.Service
	db    *store.DB
	actor domain.Actor
	ref   service.ProjectRef
	json  bool
	out   io.Writer
	errw  io.Writer
}

// handler is a command body.
type handler func(e *env, c *cobra.Command, args []string) error

// do wraps a handler: it opens the database, builds the service and closes
// the database afterwards.
func (a *app) do(h handler) func(*cobra.Command, []string) error {
	return func(c *cobra.Command, args []string) error {
		actor := domain.Human
		if v := os.Getenv(envActor); v != "" {
			actor = domain.Actor(v)
			if !actor.Valid() {
				return usagef("%s=%q is not a valid actor (no spaces)", envActor, v)
			}
		}
		path, err := config.DBPath()
		if err != nil {
			return err
		}
		ctx := c.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		db, err := store.Open(ctx, path)
		if err != nil {
			return err
		}
		defer db.Close()
		e := &env{
			ctx:   ctx,
			svc:   service.New(db),
			db:    db,
			actor: actor,
			ref:   service.ProjectRef{Slug: a.project},
			json:  a.json,
			out:   c.OutOrStdout(),
			errw:  c.ErrOrStderr(),
		}
		return h(e, c, args)
	}
}

// emit prints v as indented JSON when --json is set, otherwise calls text.
func (e *env) emit(v any, text func(w io.Writer)) error {
	if e.json {
		enc := json.NewEncoder(e.out)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	text(e.out)
	return nil
}

// table returns a tabwriter for aligned text output; call Flush when done.
func table(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

// idArg parses a short ID given on the command line. A bad ID is a usage
// error.
func idArg(entity domain.EntityType, s string) (int64, error) {
	id, err := domain.ParseID(entity, s)
	if err != nil {
		return 0, &usageError{msg: err.Error()}
	}
	return id, nil
}

func taskArg(s string) (int64, error)    { return idArg(domain.EntityTask, s) }
func featureArg(s string) (int64, error) { return idArg(domain.EntityFeature, s) }

func taskID(id int64) string    { return domain.FormatID(domain.EntityTask, id) }
func featureID(id int64) string { return domain.FormatID(domain.EntityFeature, id) }
func projectID(id int64) string { return domain.FormatID(domain.EntityProject, id) }

// parsePriority accepts 0-3 or p0-p3.
func parsePriority(s string) (int, error) {
	t := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "p")
	if len(t) != 1 || t[0] < '0' || t[0] > '3' {
		return 0, usagef("priority %q must be 0 (urgent) to 3 (low), or p0 to p3", s)
	}
	return int(t[0] - '0'), nil
}

// bar renders a fixed-width ASCII progress bar.
func bar(fraction float64, width int) string {
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	n := int(fraction*float64(width) + 0.5)
	return "[" + strings.Repeat("#", n) + strings.Repeat("-", width-n) + "]"
}

func pct(fraction float64) string { return fmt.Sprintf("%3.0f%%", fraction*100) }

// trunc shortens s to at most n runes, adding an ellipsis.
func trunc(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
