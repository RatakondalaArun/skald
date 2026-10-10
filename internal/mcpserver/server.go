// Package mcpserver exposes the service layer to Claude Code over MCP.
//
// Protocol messages go to the writer given to Serve (stdout in production)
// and nothing else is ever written there: logs go through slog to stderr.
// All use of the MCP SDK lives in this package.
package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"skald/internal/domain"
	"skald/internal/service"
)

const instructions = `Skald tracks this project's plan. Workflow: get_project, create_plan (give every task an idempotency_key), then loop get_next_tasks -> start_task -> add_task_note -> record_verification -> report_task_result. Progress is computed from task state; you cannot set it. A task cannot be completed without evidence.`

type config struct {
	cwd     string
	logger  *slog.Logger
	version string
}

// Option customises Serve.
type Option func(*config)

// WithCwd sets the directory used to resolve the project when a tool call
// names none. The default is the process working directory.
func WithCwd(dir string) Option { return func(c *config) { c.cwd = dir } }

// WithLogger sets the logger. It must not write to the protocol stream; the
// default writes to stderr.
func WithLogger(l *slog.Logger) Option { return func(c *config) { c.logger = l } }

// WithVersion sets the server version reported to clients.
func WithVersion(v string) Option { return func(c *config) { c.version = v } }

type server struct {
	svc       *service.Service
	cwd       string
	log       *slog.Logger
	sessionID string // fallback when the transport has no session ID (stdio)
}

func newConfig(opts []Option) config {
	c := config{version: "dev"}
	for _, o := range opts {
		o(&c)
	}
	if c.logger == nil {
		c.logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return c
}

func newServer(svc *service.Service, c config) *mcp.Server {
	var b [4]byte
	_, _ = rand.Read(b[:])
	s := &server{svc: svc, cwd: c.cwd, log: c.logger, sessionID: hex.EncodeToString(b[:])}

	srv := mcp.NewServer(
		&mcp.Implementation{Name: "skald", Version: c.version},
		&mcp.ServerOptions{Instructions: instructions, Logger: c.logger},
	)
	s.registerRead(srv)
	s.registerWrite(srv)
	s.registerWork(srv)
	s.registerPrompts(srv)
	return srv
}

// Serve runs the MCP server on stdin/stdout until the client disconnects or
// ctx is cancelled. A clean disconnect returns nil.
func Serve(ctx context.Context, svc *service.Service, stdin io.Reader, stdout io.Writer, opts ...Option) error {
	srv := newServer(svc, newConfig(opts))
	t := &mcp.IOTransport{Reader: io.NopCloser(stdin), Writer: nopWriteCloser{stdout}}
	err := srv.Run(ctx, t)
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, mcp.ErrConnectionClosed) {
		return nil
	}
	return err
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// actor identifies the calling MCP session in the event log.
func (s *server) actor(req *mcp.CallToolRequest) domain.Actor {
	client, session := "", ""
	if ci := req.ClientInfo(); ci != nil {
		client = ci.Name
	}
	if req.Session != nil {
		session = req.Session.ID()
	}
	if session == "" {
		session = s.sessionID
	}
	if len(session) > 8 {
		session = session[:8]
	}
	return domain.NewAgentActor(client, session)
}

func (s *server) ref(project string) service.ProjectRef {
	return service.ProjectRef{Slug: project, Cwd: s.cwd}
}
