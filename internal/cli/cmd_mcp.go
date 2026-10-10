package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"skald/internal/mcpserver"
)

func newMCPCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run the MCP server on stdin/stdout (for Claude Code)",
		Long: `Run the MCP server over stdio. Stdout carries protocol messages only; logs go to stderr.

Register it with Claude Code from the repository you want to track:
  claude mcp add skald -- skald mcp`,
		Args: noArgs,
		RunE: a.do(func(e *env, c *cobra.Command, _ []string) error {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return mcpserver.Serve(ctx, e.svc, c.InOrStdin(), os.Stdout,
				mcpserver.WithCwd(wd), mcpserver.WithVersion(Version))
		}),
	}
}
