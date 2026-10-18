// Package service holds Skald's use cases. The CLI, MCP server and web server
// all call it; none of them touch SQL. See docs/ARCHITECTURE.md section 6.
package service

import (
	"os"
	"time"

	"skald/internal/domain"
	"skald/internal/github"
	"skald/internal/store"
)

// Service runs use cases against one database.
type Service struct {
	db     *store.DB
	now    func() time.Time
	getenv func(string) string
	getwd  func() (string, error)

	newGitHub func() (GitHubAPI, error)
	gitRemote func(dir string) (github.Repo, error)
}

// Option customises a Service, mainly for tests.
type Option func(*Service)

// WithClock replaces the time source.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithEnv replaces environment lookup (used for SKALD_PROJECT).
func WithEnv(getenv func(string) string) Option { return func(s *Service) { s.getenv = getenv } }

// WithWorkdir replaces the working-directory lookup used by project
// resolution.
func WithWorkdir(getwd func() (string, error)) Option { return func(s *Service) { s.getwd = getwd } }

// New returns a Service over db.
func New(db *store.DB, opts ...Option) *Service {
	s := &Service{
		db:     db,
		now:    func() time.Time { return time.Now().UTC() },
		getenv: os.Getenv,
		getwd:  os.Getwd,

		newGitHub: defaultGitHub,
		gitRemote: github.OriginRepo,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// meta builds the store.Meta for a change made by actor right now.
func (s *Service) meta(actor domain.Actor) store.Meta {
	return store.Meta{Actor: actor, Now: s.now()}
}
