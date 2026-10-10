package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/spf13/cobra"

	"skald/internal/domain"
	"skald/internal/watch"
	"skald/internal/web"
)

func newServeCmd(a *app) *cobra.Command {
	var (
		addr     string
		open     bool
		insecure bool
	)
	c := &cobra.Command{
		Use:   "serve",
		Short: "Serve the web dashboard",
		Long: `Serves the dashboard and its JSON API from this machine. It shows the same data
as the CLI and updates live while Claude Code works through the MCP server.

The address must be a loopback address (default 127.0.0.1:7331). skald has no
login, so --insecure-listen is required to listen anywhere else.`,
		Args: noArgs,
		RunE: a.do(func(e *env, c *cobra.Command, _ []string) error {
			if err := web.CheckListenAddr(addr, insecure); err != nil {
				return usagef("%v", err)
			}
			srv, err := web.New(web.Config{
				Service:  e.svc,
				Insecure: insecure,
				Version:  Version,
				Events: func(ctx context.Context, projectID, afterID int64) <-chan domain.Event {
					return watch.Subscribe(ctx, e.db, projectID, afterID)
				},
			})
			if err != nil {
				return err
			}
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return fmt.Errorf("cannot listen on %s: %w. Pick another port with --addr 127.0.0.1:<port>", addr, err)
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			url := "http://" + ln.Addr().String()
			fmt.Fprintf(e.errw, "skald dashboard: %s (database %s)\n", url, e.db.Path())
			if insecure {
				fmt.Fprintln(e.errw, "warning: --insecure-listen is on; anyone who can reach this port can change your tasks")
			}
			if open {
				openBrowser(url)
			}
			err = srv.Serve(ctx, ln)
			fmt.Fprintln(e.errw, "skald dashboard stopped")
			return err
		}),
	}
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:7331", "address to listen on (loopback only)")
	c.Flags().BoolVar(&open, "open", false, "open the dashboard in your browser")
	c.Flags().BoolVar(&insecure, "insecure-listen", false, "allow a non-loopback --addr (no login: use only on a network you trust)")
	return c
}

// openBrowser makes a best effort; failure only means the user opens the URL.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if cmd.Start() == nil {
		go func() { _ = cmd.Wait() }()
	}
}
