package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"skald/internal/config"
	"skald/internal/daemon"
	"skald/internal/domain"
	"skald/internal/watch"
	"skald/internal/web"
)

func newServeCmd(a *app) *cobra.Command {
	var (
		addr       string
		open       bool
		insecure   bool
		background bool
		child      bool
	)
	c := &cobra.Command{
		Use:   "serve",
		Short: "Serve the web dashboard",
		Long: `Serves the dashboard and its JSON API from this machine. It shows the same data
as the CLI and updates live while Claude Code works through the MCP server.

The address must be a loopback address (default 127.0.0.1:7331). skald has no
login, so --insecure-listen is required to listen anywhere else.

--background starts the dashboard detached from the terminal and returns once
it is listening. Its output goes to serve.log in the data directory. Use
'skald serve status' to see where it is and 'skald serve stop' to stop it.`,
		Example: `  skald serve
  skald serve --background --open
  skald serve status
  skald serve stop`,
		Args: noArgs,
		RunE: a.do(func(e *env, c *cobra.Command, _ []string) error {
			if err := web.CheckListenAddr(addr, insecure); err != nil {
				return usagef("%v", err)
			}
			if background {
				return startBackground(e, addr, insecure, open)
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
			if child {
				// Record where we are only once we are listening; the parent
				// waits for this file.
				state, err := config.ServeStatePath()
				if err != nil {
					return err
				}
				logPath, _ := config.ServeLogPath()
				if err := daemon.Write(state, daemon.State{PID: os.Getpid(), URL: url, Log: logPath, Started: time.Now().UTC(), Version: Version}); err != nil {
					return fmt.Errorf("cannot record the background dashboard in %s: %w", state, err)
				}
				defer daemon.Clear(state, os.Getpid())
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
	c.Flags().BoolVarP(&background, "background", "d", false, "run detached from the terminal (see 'skald serve status' and 'skald serve stop')")
	c.Flags().BoolVar(&child, "background-child", false, "")
	_ = c.Flags().MarkHidden("background-child")
	c.AddCommand(newServeStatusCmd(a), newServeStopCmd(a))
	return c
}

// serveStatus is the JSON shape of 'serve status' and 'serve stop'.
type serveStatus struct {
	Running bool       `json:"running"`
	PID     int        `json:"pid,omitempty"`
	URL     string     `json:"url,omitempty"`
	Log     string     `json:"log,omitempty"`
	Started *time.Time `json:"started,omitempty"`
	Version string     `json:"version,omitempty"`
}

func statusOf(s daemon.State, running bool) serveStatus {
	st := serveStatus{Running: running, PID: s.PID, URL: s.URL, Log: s.Log, Version: s.Version}
	if !s.Started.IsZero() {
		st.Started = &s.Started
	}
	return st
}

// startBackground re-runs this binary as 'serve --background-child' detached
// from the terminal and returns once the child is listening.
func startBackground(e *env, addr string, insecure, open bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, err := config.Home()
	if err != nil {
		return err
	}
	state, err := config.ServeStatePath()
	if err != nil {
		return err
	}
	logPath, err := config.ServeLogPath()
	if err != nil {
		return err
	}
	args := []string{"serve", "--background-child", "--addr", addr}
	if insecure {
		args = append(args, "--insecure-listen")
	}
	s, err := daemon.Start(daemon.Spec{Exe: exe, Args: args, Dir: home, Log: logPath, State: state})
	if errors.Is(err, daemon.ErrRunning) {
		return fmt.Errorf("a background dashboard is already running at %s (pid %d). Stop it with 'skald serve stop' first", s.URL, s.PID)
	}
	if err != nil {
		return fmt.Errorf("background dashboard: %w", err)
	}
	if insecure {
		fmt.Fprintln(e.errw, "warning: --insecure-listen is on; anyone who can reach this port can change your tasks")
	}
	if open {
		openBrowser(s.URL)
	}
	return e.emit(statusOf(s, true), func(w io.Writer) {
		fmt.Fprintf(w, "skald dashboard running in the background: %s (pid %d)\n", s.URL, s.PID)
		fmt.Fprintf(w, "log: %s\nstop it with: skald serve stop\n", s.Log)
	})
}

// plainEnv is enough for commands that print but never open the database.
func (a *app) plainEnv(c *cobra.Command) *env {
	return &env{json: a.json, out: c.OutOrStdout(), errw: c.ErrOrStderr()}
}

func newServeStatusCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether a background dashboard is running",
		Long:  "Shows the URL, pid and log of the dashboard started with 'skald serve --background'. Exits 1 when none is running.",
		Args:  noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			e := a.plainEnv(c)
			state, err := config.ServeStatePath()
			if err != nil {
				return err
			}
			s, err := daemon.Status(state)
			if errors.Is(err, daemon.ErrNotRunning) {
				_ = e.emit(serveStatus{}, func(w io.Writer) {
					fmt.Fprintln(w, "no background dashboard is running; start one with: skald serve --background")
				})
				return silentError{}
			}
			if err != nil {
				return err
			}
			return e.emit(statusOf(s, true), func(w io.Writer) {
				t := table(w)
				fmt.Fprintf(t, "url\t%s\n", s.URL)
				fmt.Fprintf(t, "pid\t%d\n", s.PID)
				fmt.Fprintf(t, "started\t%s\n", s.Started.Local().Format(time.RFC3339))
				if s.Version != "" {
					fmt.Fprintf(t, "version\t%s\n", s.Version)
				}
				fmt.Fprintf(t, "log\t%s\n", s.Log)
				t.Flush()
			})
		},
	}
}

func newServeStopCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the background dashboard",
		Long:  "Stops the dashboard started with 'skald serve --background'. It is asked to shut down cleanly and killed if it has not exited after 5 seconds. Stopping when none is running is not an error.",
		Args:  noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			e := a.plainEnv(c)
			state, err := config.ServeStatePath()
			if err != nil {
				return err
			}
			s, err := daemon.Stop(state, 5*time.Second)
			if errors.Is(err, daemon.ErrNotRunning) {
				return e.emit(serveStatus{}, func(w io.Writer) {
					fmt.Fprintln(w, "no background dashboard is running")
				})
			}
			if err != nil {
				return err
			}
			return e.emit(statusOf(s, false), func(w io.Writer) {
				fmt.Fprintf(w, "stopped the background dashboard at %s (pid %d)\n", s.URL, s.PID)
			})
		},
	}
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
