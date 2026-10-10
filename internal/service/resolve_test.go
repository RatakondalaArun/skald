package service

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"skald/internal/domain"
	"skald/internal/store"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
)

func newService(t *testing.T, env map[string]string, opts ...Option) (*Service, *store.DB) {
	t.Helper()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "db", "skald.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	all := append([]Option{
		WithClock(func() time.Time { return t0 }),
		WithEnv(func(k string) string { return env[k] }),
		WithWorkdir(func() (string, error) { return "/definitely/not/a/project", nil }),
	}, opts...)
	return New(db, all...), db
}

func addProject(t *testing.T, db *store.DB, slug, repo string) domain.Project {
	t.Helper()
	var p domain.Project
	err := db.WriteTx(ctx, func(tx *sql.Tx) (err error) {
		p, err = store.CreateProject(ctx, tx, store.Meta{Actor: domain.Human, Now: t0}, domain.Project{Slug: slug, Name: slug, RepoPath: repo})
		return
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func archive(t *testing.T, db *store.DB, id int64) {
	t.Helper()
	if err := db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := store.ArchiveProject(ctx, tx, store.Meta{Actor: domain.Human, Now: t0}, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func repoDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	norm, err := NormalizeRepoPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	return norm
}

func TestResolveExplicitSlugWins(t *testing.T) {
	svc, db := newService(t, map[string]string{EnvProject: "from-env"})
	a := addProject(t, db, "alpha", "")
	addProject(t, db, "from-env", "")
	got, err := svc.ResolveProject(ctx, ProjectRef{Slug: "alpha"})
	if err != nil || got.ID != a.ID {
		t.Fatalf("explicit slug: %v %+v", err, got)
	}
	if _, err := svc.ResolveProject(ctx, ProjectRef{Slug: "missing"}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown explicit slug: %v", err)
	}
	// An archived project can still be addressed explicitly.
	archive(t, db, a.ID)
	if got, err := svc.ResolveProject(ctx, ProjectRef{Slug: "alpha"}); err != nil || got.Status != domain.ProjectArchived {
		t.Errorf("archived explicit: %v %+v", err, got)
	}
}

func TestResolveEnv(t *testing.T) {
	svc, db := newService(t, map[string]string{EnvProject: " beta "})
	addProject(t, db, "alpha", "")
	b := addProject(t, db, "beta", "")
	got, err := svc.ResolveProject(ctx, ProjectRef{})
	if err != nil || got.ID != b.ID {
		t.Fatalf("env: %v %+v", err, got)
	}
	// An env slug that does not exist is an error, not a silent fallback.
	svc2, db2 := newService(t, map[string]string{EnvProject: "ghost"})
	addProject(t, db2, "only", "")
	if _, err := svc2.ResolveProject(ctx, ProjectRef{}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown env slug: %v", err)
	}
}

func TestResolveCwdWalkUp(t *testing.T) {
	svc, db := newService(t, nil)
	repoA, repoB := repoDir(t, "a"), repoDir(t, "b")
	a := addProject(t, db, "alpha", repoA)
	b := addProject(t, db, "beta", repoB)

	for _, tc := range []struct {
		cwd  string
		want int64
	}{
		{repoA, a.ID},
		{filepath.Join(repoA, "pkg", "deep"), a.ID},
		{filepath.Join(repoB, "pkg"), b.ID},
	} {
		got, err := svc.ResolveProject(ctx, ProjectRef{Cwd: tc.cwd})
		if err != nil || got.ID != tc.want {
			t.Errorf("cwd %s: %v %+v, want id %d", tc.cwd, err, got, tc.want)
		}
	}

	// The process working directory is used when Cwd is empty.
	svcWd, dbWd := newService(t, nil, WithWorkdir(func() (string, error) { return filepath.Join(repoA, "pkg"), nil }))
	w := addProject(t, dbWd, "walker", repoA)
	if got, err := svcWd.ResolveProject(ctx, ProjectRef{}); err != nil || got.ID != w.ID {
		t.Errorf("working directory: %v %+v", err, got)
	}
}

func TestResolveCwdResolvesSymlinks(t *testing.T) {
	svc, db := newService(t, nil)
	repo := repoDir(t, "real")
	p := addProject(t, db, "linked", repo)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	got, err := svc.ResolveProject(ctx, ProjectRef{Cwd: filepath.Join(link, "pkg")})
	if err != nil || got.ID != p.ID {
		t.Fatalf("through symlink: %v %+v", err, got)
	}
	// Two projects exist, so only the symlink match can have selected this one.
	addProject(t, db, "decoy", "")
	if got, err := svc.ResolveProject(ctx, ProjectRef{Cwd: link}); err != nil || got.ID != p.ID {
		t.Errorf("symlink with decoy: %v %+v", err, got)
	}
}

func TestResolveCwdSkipsArchivedAndPrefersNearest(t *testing.T) {
	svc, db := newService(t, nil)
	outer := repoDir(t, "outer")
	inner := filepath.Join(outer, "pkg")
	o := addProject(t, db, "outer", outer)
	i := addProject(t, db, "inner", inner)
	if got, err := svc.ResolveProject(ctx, ProjectRef{Cwd: filepath.Join(inner, "deep")}); err != nil || got.ID != i.ID {
		t.Fatalf("nearest: %v %+v", err, got)
	}
	archive(t, db, i.ID)
	got, err := svc.ResolveProject(ctx, ProjectRef{Cwd: filepath.Join(inner, "deep")})
	if err != nil || got.ID != o.ID {
		t.Errorf("archived inner should be skipped in favour of outer: %v %+v", err, got)
	}
}

func TestResolveSingleActiveProject(t *testing.T) {
	svc, db := newService(t, nil)
	only := addProject(t, db, "only", "")
	got, err := svc.ResolveProject(ctx, ProjectRef{})
	if err != nil || got.ID != only.ID {
		t.Fatalf("single active: %v %+v", err, got)
	}
	// Archived projects do not count.
	extra := addProject(t, db, "extra", "")
	archive(t, db, extra.ID)
	if got, err := svc.ResolveProject(ctx, ProjectRef{}); err != nil || got.ID != only.ID {
		t.Errorf("archived must not count: %v %+v", err, got)
	}
}

func TestResolveAmbiguousAndEmpty(t *testing.T) {
	svc, db := newService(t, nil)
	_, err := svc.ResolveProject(ctx, ProjectRef{})
	var ve *domain.ValidationError
	if !errors.As(err, &ve) || !strings.Contains(ve.Hint, "skald project create") {
		t.Errorf("no projects: %v", err)
	}
	addProject(t, db, "zeta", "")
	addProject(t, db, "alpha", "")
	_, err = svc.ResolveProject(ctx, ProjectRef{})
	if !errors.As(err, &ve) {
		t.Fatalf("ambiguous: %v", err)
	}
	msg := ve.Message()
	if !strings.Contains(msg, "alpha, zeta") || !strings.Contains(msg, "SKALD_PROJECT") {
		t.Errorf("ambiguous message must list sorted candidates and next steps: %q", msg)
	}
	// An unresolvable cwd falls through to the same ambiguity error.
	if _, err := svc.ResolveProject(ctx, ProjectRef{Cwd: "/no/such/dir"}); !errors.As(err, &ve) {
		t.Errorf("bad cwd: %v", err)
	}
}

func TestNormalizeRepoPath(t *testing.T) {
	dir := t.TempDir()
	want, _ := filepath.EvalSymlinks(dir)
	got, err := NormalizeRepoPath(dir + "/../" + filepath.Base(dir))
	if err != nil || got != want {
		t.Errorf("NormalizeRepoPath = %q, %v; want %q", got, err, want)
	}
	if _, err := NormalizeRepoPath(filepath.Join(dir, "missing")); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("missing path: %v", err)
	}
}
