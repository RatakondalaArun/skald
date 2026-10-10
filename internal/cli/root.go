// Package cli defines the skald command tree.
package cli

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

// Version is set at build time with -ldflags "-X skald/internal/cli.Version=...".
var Version = "dev"

// NewRootCmd builds the root command.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "skald",
		Short:         "Local-first project tracker for coding agents",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("{{.Version}}\n")
	return root
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	// Logs go to stderr only; stdout is reserved for command output and MCP protocol.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := NewRootCmd().Execute(); err != nil {
		slog.Error(err.Error())
		return 1
	}
	return 0
}
