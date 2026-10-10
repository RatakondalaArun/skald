package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"skald/internal/domain"
	"skald/internal/store"
)

// EnvProject is the environment variable that selects a project by slug.
const EnvProject = "SKALD_PROJECT"

// ProjectRef says which project a request is about. Both fields are optional.
type ProjectRef struct {
	Slug string // explicit project slug
	Cwd  string // directory to resolve from; empty means the process working directory
}

// NormalizeRepoPath returns the absolute, symlink-resolved form of path. This
// is the form stored in projects.repo_path and compared during resolution, so
// macOS paths like /var/... and /private/var/... match.
func NormalizeRepoPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", &domain.ValidationError{Field: "path", Problem: err.Error()}
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", &domain.ValidationError{Field: "path", Problem: fmt.Sprintf("%s cannot be resolved (%v)", path, err), Hint: "Use a directory that exists."}
	}
	return filepath.Clean(resolved), nil
}

// ResolveProject picks the project a command or MCP session acts on, in this
// order:
//
//  1. ref.Slug (an unknown slug is a NotFoundError)
//  2. the SKALD_PROJECT environment variable (same)
//  3. the nearest parent of ref.Cwd (or the working directory) that is the
//     repo_path of an active project
//  4. the only active project, when there is exactly one
//
// Otherwise it returns a ValidationError that lists the candidate slugs.
func (s *Service) ResolveProject(ctx context.Context, ref ProjectRef) (domain.Project, error) {
	p, err := s.resolveProject(ctx, ref)
	return p, store.Classify(s.db.Path(), err)
}

func (s *Service) resolveProject(ctx context.Context, ref ProjectRef) (domain.Project, error) {
	db := s.db.Reader()

	if slug := strings.TrimSpace(ref.Slug); slug != "" {
		return store.GetProjectBySlug(ctx, db, slug)
	}
	if slug := strings.TrimSpace(s.getenv(EnvProject)); slug != "" {
		return store.GetProjectBySlug(ctx, db, slug)
	}

	dir := ref.Cwd
	if dir == "" {
		wd, err := s.getwd()
		if err != nil {
			return domain.Project{}, fmt.Errorf("get working directory: %w", err)
		}
		dir = wd
	}
	if resolved, err := NormalizeRepoPath(dir); err == nil {
		for cur := resolved; ; {
			p, err := store.GetProjectByRepoPath(ctx, db, cur)
			switch {
			case err == nil && p.Status == domain.ProjectActive:
				return p, nil
			case err != nil && !errors.Is(err, domain.ErrNotFound):
				return domain.Project{}, err
			}
			parent := filepath.Dir(cur)
			if parent == cur {
				break
			}
			cur = parent
		}
	}

	active, err := store.ListProjects(ctx, db, false)
	if err != nil {
		return domain.Project{}, err
	}
	switch len(active) {
	case 0:
		return domain.Project{}, &domain.ValidationError{
			Field:   "project",
			Problem: "no project found for this directory and no projects exist",
			Hint:    "Create one with `skald project create <slug> <name>` from the repository root.",
		}
	case 1:
		return active[0], nil
	}
	slugs := make([]string, len(active))
	for i, p := range active {
		slugs[i] = p.Slug
	}
	sort.Strings(slugs)
	return domain.Project{}, &domain.ValidationError{
		Field:   "project",
		Problem: "cannot tell which project to use: this directory is not inside a project's repository and several projects exist (" + strings.Join(slugs, ", ") + ")",
		Hint:    "Pass a project slug, set SKALD_PROJECT, or run from inside the project's repository.",
	}
}
