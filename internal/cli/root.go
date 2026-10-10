// Package cli defines the skald command tree.
package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

// Version is set at build time with -ldflags "-X skald/internal/cli.Version=...".
var Version = "dev"

// Exit codes.
const (
	exitOK      = 0
	exitError   = 1 // runtime or domain error
	exitUsage   = 2 // bad flags, arguments or IDs
	envActor    = "SKALD_ACTOR"
	rootExample = `  skald init
  skald project create "School Management"
  skald plan import plan.json
  skald next
  skald task start T-1
  skald evidence add T-1 --kind commit --ref 3f2a9c1
  skald task done T-1
  skald status`
)

// NewRootCmd builds the root command.
func NewRootCmd() *cobra.Command {
	a := &app{}
	root := &cobra.Command{
		Use:           "skald",
		Short:         "Local-first project tracker for coding agents",
		Example:       rootExample,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          noArgs,
		RunE:          func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{msg: err.Error()} })
	root.PersistentFlags().StringVarP(&a.project, "project", "p", "", "project slug (default: $SKALD_PROJECT, then the project linked to the working directory)")
	root.PersistentFlags().BoolVar(&a.json, "json", false, "print JSON instead of text")

	root.AddCommand(
		newInitCmd(a),
		newProjectCmd(a),
		newFeatureCmd(a),
		newTaskCmd(a),
		newPlanCmd(a),
		newEvidenceCmd(a),
		newNextCmd(a),
		newStatusCmd(a),
		newLogCmd(a),
		newBackupCmd(a),
		newMCPCmd(a),
		newServeCmd(a),
	)
	return root
}

// Execute runs the CLI with the process arguments and returns the exit code.
func Execute() int {
	// Logs go to stderr only; stdout is reserved for command output and MCP protocol.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	return run(NewRootCmd(), os.Stderr)
}

// run executes root and maps its error to an exit code, printing the message
// to stderr. Results go to stdout through the commands themselves.
func run(root *cobra.Command, stderr io.Writer) int {
	err := root.Execute()
	if err == nil {
		return exitOK
	}
	var ue *usageError
	if errors.As(err, &ue) {
		fmt.Fprintf(stderr, "skald: %s\nRun 'skald --help' for usage.\n", err)
		return exitUsage
	}
	fmt.Fprintf(stderr, "skald: %s\n", err)
	return exitError
}
