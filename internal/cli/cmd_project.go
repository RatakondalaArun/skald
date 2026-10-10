package cli

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"skald/internal/service"
)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugify turns a project name into a slug such as "school-management".
func slugify(name string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

func newProjectCmd(a *app) *cobra.Command {
	return group("project", "Create and inspect projects",
		newProjectCreateCmd(a), newProjectListCmd(a), newProjectShowCmd(a), newProjectArchiveCmd(a), newProjectLinkCmd(a))
}

func newProjectCreateCmd(a *app) *cobra.Command {
	var slug, desc, repo string
	var noRepo bool
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a project, linked to the current directory by default",
		Long: `Creates a project. Unless --repo or --no-repo is given, the project is linked
to the current directory so that skald commands (and the MCP server) run
inside it find the project automatically.`,
		Example: `  skald project create "School Management"
  skald project create "API" --slug api --repo ~/code/api`,
		Args: exactArgs(1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			if slug == "" {
				slug = slugify(name)
			}
			if slug == "" {
				return usagef("cannot derive a slug from %q; pass --slug", name)
			}
			if noRepo && repo != "" {
				return usagef("--repo and --no-repo cannot be used together")
			}
			p, err := e.svc.CreateProject(e.ctx, e.actor, service.CreateProjectInput{
				Slug: slug, Name: name, Description: desc, RepoPath: repo, LinkCwd: !noRepo,
			})
			if err != nil {
				return err
			}
			return e.emit(toProject(p), func(w io.Writer) {
				fmt.Fprintf(w, "created %s %s (%s)\n", projectID(p.ID), p.Name, p.Slug)
				if p.RepoPath != "" {
					fmt.Fprintf(w, "linked to %s\n", p.RepoPath)
				}
			})
		}),
	}
	c.Flags().StringVar(&slug, "slug", "", "project slug (default: derived from the name)")
	c.Flags().StringVar(&desc, "description", "", "project description")
	c.Flags().StringVar(&repo, "repo", "", "repository path to link (default: current directory)")
	c.Flags().BoolVar(&noRepo, "no-repo", false, "do not link the project to a directory")
	return c
}

func newProjectListCmd(a *app) *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "list",
		Short: "List projects",
		Args:  noArgs,
		RunE: a.do(func(e *env, c *cobra.Command, _ []string) error {
			list, err := e.svc.ListProjects(e.ctx, e.actor, all)
			if err != nil {
				return err
			}
			type item struct {
				projectJSON
				Progress progressJSON `json:"progress"`
				Counts   countsJSON   `json:"counts"`
				Features int          `json:"features"`
				Ready    int          `json:"ready"`
			}
			items := make([]item, 0, len(list))
			for _, o := range list {
				items = append(items, item{toProject(o.Project), toProgress(o.Progress.Progress), toCounts(o.Counts), o.Features, o.Ready})
			}
			return e.emit(items, func(w io.Writer) {
				if len(list) == 0 {
					fmt.Fprintln(w, "no projects; create one with `skald project create <name>`")
					return
				}
				t := table(w)
				fmt.Fprintln(t, "ID\tSLUG\tNAME\tPROGRESS\tDONE\tREADY\tREPO")
				for _, o := range list {
					name := o.Project.Name
					if o.Project.Status == "archived" {
						name += " (archived)"
					}
					done := o.Counts["done"]
					fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%d/%d\t%d\t%s\n", projectID(o.Project.ID), o.Project.Slug, name,
						pct(o.Progress.Progress.Fraction), done, o.Counts.Total(), o.Ready, o.Project.RepoPath)
				}
				t.Flush()
			})
		}),
	}
	c.Flags().BoolVar(&all, "all", false, "include archived projects")
	return c
}

// refFromArg lets a project slug be given as an argument as well as with -p.
func refFromArg(e *env, args []string) {
	if len(args) == 1 {
		e.ref.Slug = args[0]
	}
}

func newProjectShowCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "show [slug]",
		Short: "Show a project and its features",
		Args:  rangeArgs(0, 1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			refFromArg(e, args)
			o, err := e.svc.GetProject(e.ctx, e.actor, e.ref)
			if err != nil {
				return err
			}
			feats, err := e.svc.ListFeatures(e.ctx, e.actor, service.ProjectRef{Slug: o.Project.Slug})
			if err != nil {
				return err
			}
			fj := make([]featureJSON, 0, len(feats))
			for _, f := range feats {
				j := toFeature(f.Feature)
				p, cn := toProgress(f.Progress), toCounts(f.Counts)
				j.Progress, j.Counts = &p, &cn
				fj = append(fj, j)
			}
			out := struct {
				projectJSON
				Progress progressJSON  `json:"progress"`
				Counts   countsJSON    `json:"counts"`
				Ready    int           `json:"ready"`
				Features []featureJSON `json:"features"`
			}{toProject(o.Project), toProgress(o.Progress.Progress), toCounts(o.Counts), o.Ready, fj}
			return e.emit(out, func(w io.Writer) {
				p := o.Project
				fmt.Fprintf(w, "%s  %s (%s)\n", projectID(p.ID), p.Name, p.Slug)
				if p.Description != "" {
					fmt.Fprintf(w, "%s\n", p.Description)
				}
				fmt.Fprintf(w, "status    %s\n", p.Status)
				if p.RepoPath != "" {
					fmt.Fprintf(w, "repo      %s\n", p.RepoPath)
				}
				fmt.Fprintf(w, "evidence  %s\n", map[bool]string{true: "required to complete tasks", false: "not required"}[p.Settings.RequireEvidence])
				fmt.Fprintf(w, "progress  %s %s  (%d/%d tasks done, %d ready)\n", bar(o.Progress.Progress.Fraction, 20),
					pct(o.Progress.Progress.Fraction), o.Counts["done"], o.Counts.Total()-o.Counts["cancelled"], o.Ready)
				if len(feats) > 0 {
					fmt.Fprintln(w)
					t := table(w)
					fmt.Fprintln(t, "FEATURE\tTITLE\tSTATUS\tPROGRESS\tTASKS")
					for _, f := range feats {
						fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%d/%d\n", featureID(f.Feature.ID), f.Feature.Title, f.Feature.Status,
							pct(f.Progress.Fraction), f.Counts["done"], f.Counts.Total()-f.Counts["cancelled"])
					}
					t.Flush()
				}
			})
		}),
	}
}

func newProjectArchiveCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "archive [slug]",
		Short: "Archive a project and release its repository link",
		Args:  rangeArgs(0, 1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			refFromArg(e, args)
			o, err := e.svc.GetProject(e.ctx, e.actor, e.ref)
			if err != nil {
				return err
			}
			p, err := e.svc.ArchiveProject(e.ctx, e.actor, o.Project.ID)
			if err != nil {
				return err
			}
			return e.emit(toProject(p), func(w io.Writer) {
				fmt.Fprintf(w, "archived %s %s\n", projectID(p.ID), p.Slug)
			})
		}),
	}
}

func newProjectLinkCmd(a *app) *cobra.Command {
	var repo string
	var unlink bool
	c := &cobra.Command{
		Use:   "link [slug]",
		Short: "Link a project to a repository directory (default: the current directory)",
		Args:  rangeArgs(0, 1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			if unlink && repo != "" {
				return usagef("--unlink and --repo cannot be used together")
			}
			refFromArg(e, args)
			o, err := e.svc.GetProject(e.ctx, e.actor, e.ref)
			if err != nil {
				return err
			}
			target := repo
			if !unlink && target == "" {
				if target, err = os.Getwd(); err != nil {
					return err
				}
			}
			p, err := e.svc.LinkProject(e.ctx, e.actor, o.Project.ID, target)
			if err != nil {
				return err
			}
			return e.emit(toProject(p), func(w io.Writer) {
				if p.RepoPath == "" {
					fmt.Fprintf(w, "unlinked %s %s\n", projectID(p.ID), p.Slug)
					return
				}
				fmt.Fprintf(w, "linked %s %s to %s\n", projectID(p.ID), p.Slug, p.RepoPath)
			})
		}),
	}
	c.Flags().StringVar(&repo, "repo", "", "repository directory to link (default: the current directory)")
	c.Flags().BoolVar(&unlink, "unlink", false, "remove the repository link")
	return c
}
