// Package web serves the dashboard: a JSON API over the service layer, a
// server-sent-events stream of the change feed, and the embedded static UI.
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"skald/internal/domain"
	"skald/internal/service"
)

//go:embed static
var staticFS embed.FS

// EventSource streams events with id > afterID in increasing id order for one
// project (0 means all) until ctx is cancelled. internal/watch.Subscribe fits
// once it is given the database.
type EventSource func(ctx context.Context, projectID, afterID int64) <-chan domain.Event

// Config configures a Server.
type Config struct {
	Service *service.Service
	Events  EventSource
	// Insecure turns off the Host check for non-loopback listening.
	Insecure bool
	// Port pins the allowed Host port. Serve sets it from the listener.
	Port int
	// Token is the mutation token; a random one is generated when empty.
	Token string
	// Version is shown in the page.
	Version string
	Logger  *slog.Logger
	// Keepalive is the SSE comment interval (default 15s).
	Keepalive time.Duration
}

// Server is the dashboard HTTP server.
type Server struct {
	cfg     Config
	token   string
	log     *slog.Logger
	index   []byte
	enrich  *enricher
	boundTo atomic.Int64
	handler http.Handler
}

// New builds a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Service == nil {
		return nil, errors.New("web: Config.Service is required")
	}
	if cfg.Events == nil {
		return nil, errors.New("web: Config.Events is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Keepalive <= 0 {
		cfg.Keepalive = 15 * time.Second
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	tok := cfg.Token
	if tok == "" {
		var err error
		if tok, err = newToken(); err != nil {
			return nil, fmt.Errorf("web: generate token: %w", err)
		}
	}
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, token: tok, log: cfg.Logger, enrich: newEnricher(cfg.Service)}
	s.boundTo.Store(int64(cfg.Port))
	s.index = []byte(strings.NewReplacer("{{TOKEN}}", tok, "{{VERSION}}", html.EscapeString(cfg.Version)).Replace(string(raw)))
	s.handler = securityHeaders(s.guard(s.routes()))
	return s, nil
}

// Token returns the mutation token embedded in the served page.
func (s *Server) Token() string { return s.token }

// Handler returns the full handler, including security middleware.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) port() int { return int(s.boundTo.Load()) }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(sub)
	mux.HandleFunc("GET /{$}", s.serveIndex)
	mux.Handle("GET /static/", http.StripPrefix("/static/", noCache(files)))

	mux.HandleFunc("GET /api/projects", s.apiProjects)
	mux.HandleFunc("GET /api/projects/{slug}", s.apiProject)
	mux.HandleFunc("GET /api/projects/{slug}/features", s.apiFeatures)
	mux.HandleFunc("GET /api/projects/{slug}/board", s.apiBoard)
	mux.HandleFunc("GET /api/projects/{slug}/tasks", s.apiTasks)
	mux.HandleFunc("GET /api/projects/{slug}/tasks/{id}", s.apiTask)
	mux.HandleFunc("GET /api/projects/{slug}/next", s.apiNext)
	mux.HandleFunc("GET /api/projects/{slug}/documents", s.apiDocuments)
	mux.HandleFunc("GET /api/projects/{slug}/document", s.apiDocument)
	mux.HandleFunc("GET /api/projects/{slug}/events", s.apiProjectEvents)
	mux.HandleFunc("GET /api/events", s.apiEvents)
	mux.HandleFunc("POST /api/projects/{slug}/tasks/{id}/unblock", s.mutating(s.apiTaskAction("unblock")))
	mux.HandleFunc("POST /api/projects/{slug}/tasks/{id}/cancel", s.mutating(s.apiTaskAction("cancel")))
	mux.HandleFunc("POST /api/projects/{slug}/tasks/{id}/reopen", s.mutating(s.apiTaskAction("reopen")))
	mux.HandleFunc("GET /events", s.serveSSE)
	mux.HandleFunc("GET /api/", func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, http.StatusNotFound, "not_found", "No such endpoint. See docs/ARCHITECTURE.md section 9 for the API.")
	})
	return withWriteDeadline(mux)
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(s.index)
}

func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

// withWriteDeadline gives ordinary requests a write deadline. The SSE handler
// manages its own deadlines, so the server-level WriteTimeout stays unset.
func withWriteDeadline(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(20 * time.Second))
		}
		h.ServeHTTP(w, r)
	})
}

// Serve serves on ln until ctx is cancelled, then shuts down gracefully:
// open SSE streams are ended first so Shutdown does not wait on them.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if s.port() == 0 {
		if ta, ok := ln.Addr().(*net.TCPAddr); ok {
			s.boundTo.Store(int64(ta.Port))
		}
	}
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	cancelBase()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		_ = srv.Close()
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
