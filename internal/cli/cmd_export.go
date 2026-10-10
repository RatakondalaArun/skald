package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"skald/internal/service"
)

func newExportCmd(a *app) *cobra.Command {
	var all bool
	var out string
	c := &cobra.Command{
		Use:   "export",
		Short: "Write a project (or everything) as JSON",
		Long: `Writes a versioned JSON snapshot of one project, or of every project with
--all: features, tasks, dependencies, evidence, documents with all versions, and
the full event log. Every entity has its raw "id" and its "short_id" (T-42).
The snapshot is read in one transaction, so it is consistent even while Claude
Code is writing.

Output goes to stdout, or to --out (mode 0600). The output is always JSON;
The global --json flag is accepted and changes nothing.

Load an export into a fresh database with "skald import".`,
		Example: `  skald export -p skald --out skald.json
  skald export --all > everything.json`,
		Args: noArgs,
		RunE: a.do(func(e *env, c *cobra.Command, _ []string) error {
			opts := service.ExportOptions{SkaldVersion: Version}
			if all && e.ref.Slug != "" {
				return usagef("use either --all or --project, not both")
			}
			if !all {
				p, err := e.svc.ResolveProject(e.ctx, e.ref)
				if err != nil {
					return err
				}
				opts.Project = p.ID
			}
			x, err := e.svc.Export(e.ctx, opts)
			if err != nil {
				return err
			}
			if out == "" {
				return x.Write(e.out)
			}
			if _, err := os.Stat(out); err == nil {
				return usagef("%s already exists; choose another --out", out)
			}
			if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			if err := x.Write(f); err != nil {
				f.Close()
				os.Remove(out)
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			fmt.Fprintf(e.errw, "exported %s: %d projects, %d tasks, %d events to %s\n", x.Scope, len(x.Projects), len(x.Tasks), len(x.Events), out)
			return nil
		}),
	}
	c.Flags().BoolVar(&all, "all", false, "export every project")
	c.Flags().StringVar(&out, "out", "", "write to this file instead of stdout")
	return c
}

func newImportCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "import <file>",
		Short: "Load a skald export into an empty database",
		Long: `Loads a file written by "skald export" into an EMPTY database, keeping every
id and the whole event log. A database that already has projects, tasks or
events is refused and left untouched. To restore elsewhere, point SKALD_HOME at
a new directory first. Use "-" to read standard input.`,
		Example: `  SKALD_HOME=/tmp/restore skald import skald.json`,
		Args:    exactArgs(1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			var r io.Reader = c.InOrStdin()
			if args[0] != "-" {
				f, err := os.Open(args[0])
				if err != nil {
					return err
				}
				defer f.Close()
				r = f
			}
			x, err := service.ParseExport(r)
			if err != nil {
				return err
			}
			if err := e.svc.ImportExport(e.ctx, x); err != nil {
				return err
			}
			res := struct {
				Projects int `json:"projects"`
				Features int `json:"features"`
				Tasks    int `json:"tasks"`
				Events   int `json:"events"`
			}{len(x.Projects), len(x.Features), len(x.Tasks), len(x.Events)}
			return e.emit(res, func(w io.Writer) {
				fmt.Fprintf(w, "imported %d projects, %d features, %d tasks, %d events\n", res.Projects, res.Features, res.Tasks, res.Events)
			})
		}),
	}
}
