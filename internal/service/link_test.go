package service

import (
	"testing"

	"skald/internal/domain"
)

func TestLinkProject(t *testing.T) {
	e := projANew(t)
	dir := projADir(t)
	norm, _ := NormalizeRepoPath(dir)
	p, _ := e.project(t, "unlinked")

	got, err := e.svc.LinkProject(projACtx, domain.Human, p.ID, dir)
	if err != nil || got.RepoPath != norm {
		t.Fatalf("link = %+v, %v (want %s)", got, err, norm)
	}
	// Relinking to the same path is a no-op with no extra event.
	before := len(e.events(t, p.ID))
	if _, err := e.svc.LinkProject(projACtx, domain.Human, p.ID, dir); err != nil {
		t.Fatal(err)
	}
	if after := len(e.events(t, p.ID)); after != before {
		t.Fatalf("events %d -> %d on a no-op link", before, after)
	}
	// Resolution now finds it from the directory.
	res, err := e.svc.ResolveProject(projACtx, ProjectRef{Cwd: dir})
	if err != nil || res.ID != p.ID {
		t.Fatalf("resolve = %+v, %v", res, err)
	}
	// Unlink.
	got, err = e.svc.LinkProject(projACtx, domain.Human, p.ID, "")
	if err != nil || got.RepoPath != "" {
		t.Fatalf("unlink = %+v, %v", got, err)
	}
}

func TestLinkProjectRefusesPathOwnedByAnotherProject(t *testing.T) {
	e := projANew(t)
	dir := projADir(t)
	owner := projAMust[domain.Project](t)(e.svc.CreateProject(projACtx, domain.Human, CreateProjectInput{Slug: "owner", Name: "Owner", RepoPath: dir}))
	other, _ := e.project(t, "other")
	_, err := e.svc.LinkProject(projACtx, domain.Human, other.ID, dir)
	projAKind(t, err, domain.KindValidation)
	// After archiving the owner the path is free.
	if _, err := e.svc.ArchiveProject(projACtx, domain.Human, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.LinkProject(projACtx, domain.Human, other.ID, dir); err != nil {
		t.Fatal(err)
	}
}

func TestLinkProjectRefusesArchivedAndBadPath(t *testing.T) {
	e := projANew(t)
	p, _ := e.project(t, "gone")
	if _, err := e.svc.LinkProject(projACtx, domain.Human, p.ID, "/no/such/dir/anywhere"); err == nil {
		t.Fatal("a missing directory must be refused")
	}
	if _, err := e.svc.ArchiveProject(projACtx, domain.Human, p.ID); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.LinkProject(projACtx, domain.Human, p.ID, projADir(t))
	projAKind(t, err, domain.KindValidation)
}
