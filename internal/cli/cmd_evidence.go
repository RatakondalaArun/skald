package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"skald/internal/domain"
	"skald/internal/service"
)

func newEvidenceCmd(a *app) *cobra.Command {
	var kind, ref, summary string
	add := &cobra.Command{
		Use:   "add <task>",
		Short: "Attach verification evidence to a task",
		Long: `Attaches evidence that a task is really done: a commit, pull request, test
report, link or manual check. Kinds: commit, pr, test_report, link, manual.`,
		Example: `  skald evidence add T-3 --kind commit --ref 3f2a9c1 --summary "login endpoint"
  skald evidence add T-3 --kind test_report --ref ci/run/812`,
		Args: exactArgs(1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			id, err := taskArg(args[0])
			if err != nil {
				return err
			}
			k := domain.ArtifactKind(kind)
			if kind == "" || !k.Valid() {
				return usagef("--kind must be one of commit, pr, test_report, link, manual")
			}
			if ref == "" {
				return usagef("--ref is required (a commit SHA, PR URL, report path or link)")
			}
			art, err := e.svc.RecordArtifact(e.ctx, e.actor, id, service.ArtifactInput{Kind: k, Ref: ref, Summary: summary})
			if err != nil {
				return err
			}
			return e.emit(toArtifact(art), func(w io.Writer) {
				fmt.Fprintf(w, "recorded %s on %s: %s %s\n", domain.FormatID(domain.EntityArtifact, art.ID), taskID(id), art.Kind, art.Ref)
			})
		}),
	}
	add.Flags().StringVar(&kind, "kind", "", "commit, pr, test_report, link or manual (required)")
	add.Flags().StringVar(&ref, "ref", "", "SHA, URL, path or other reference (required)")
	add.Flags().StringVar(&summary, "summary", "", "what this evidence shows")
	return group("evidence", "Attach evidence to tasks", add)
}
