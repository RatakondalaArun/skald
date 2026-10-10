package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"skald/internal/service"
)

func newFeatureCmd(a *app) *cobra.Command {
	return group("feature", "Add and list features", newFeatureAddCmd(a), newFeatureListCmd(a))
}

func newFeatureAddCmd(a *app) *cobra.Command {
	var desc string
	var weight, position int
	c := &cobra.Command{
		Use:     "add <title>",
		Short:   "Add a feature to the project",
		Example: `  skald feature add "Authentication" --weight 3`,
		Args:    exactArgs(1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			f, err := e.svc.CreateFeature(e.ctx, e.actor, e.ref, service.CreateFeatureInput{
				Title: args[0], Description: desc, Weight: weight, Position: position,
			})
			if err != nil {
				return err
			}
			return e.emit(toFeature(f), func(w io.Writer) {
				fmt.Fprintf(w, "added %s %s (weight %d)\n", featureID(f.ID), f.Title, f.Weight)
			})
		}),
	}
	c.Flags().StringVar(&desc, "description", "", "feature description")
	c.Flags().IntVar(&weight, "weight", 1, "relative weight in project progress (1-10)")
	c.Flags().IntVar(&position, "position", 0, "display position (0 = next)")
	return c
}

func newFeatureListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List features with progress",
		Args:  noArgs,
		RunE: a.do(func(e *env, c *cobra.Command, _ []string) error {
			feats, err := e.svc.ListFeatures(e.ctx, e.actor, e.ref)
			if err != nil {
				return err
			}
			out := make([]featureJSON, 0, len(feats))
			for _, f := range feats {
				j := toFeature(f.Feature)
				p, cn := toProgress(f.Progress), toCounts(f.Counts)
				j.Progress, j.Counts = &p, &cn
				out = append(out, j)
			}
			return e.emit(out, func(w io.Writer) {
				if len(feats) == 0 {
					fmt.Fprintln(w, "no features; add one with `skald feature add <title>`")
					return
				}
				t := table(w)
				fmt.Fprintln(t, "ID\tTITLE\tSTATUS\tWEIGHT\tPROGRESS\tTASKS")
				for _, f := range feats {
					fmt.Fprintf(t, "%s\t%s\t%s\t%d\t%s %s\t%d/%d\n", featureID(f.Feature.ID), f.Feature.Title, f.Feature.Status,
						f.Feature.Weight, bar(f.Progress.Fraction, 10), pct(f.Progress.Fraction),
						f.Counts["done"], f.Counts.Total()-f.Counts["cancelled"])
				}
				t.Flush()
			})
		}),
	}
}
