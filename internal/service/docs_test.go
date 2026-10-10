package service

import (
	"errors"
	"testing"

	"skald/internal/domain"
)

func TestNextCDocumentVersioning(t *testing.T) {
	e := nextCNew(t)
	ref := ProjectRef{Slug: "demo"}
	agent := domain.NewAgentActor("claude-code", "s1")

	doc, v1, err := e.svc.PutDocument(nextCCtx, domain.Human, ref, "architecture", "Overview", "first")
	if err != nil {
		t.Fatal(err)
	}
	if doc.CurrentVersion != 1 || v1.Version != 1 || v1.Body != "first" || v1.Actor != domain.Human {
		t.Fatalf("v1 = %+v doc = %+v", v1, doc)
	}
	doc, v2, err := e.svc.PutDocument(nextCCtx, agent, ref, "architecture", "Overview", "second")
	if err != nil {
		t.Fatal(err)
	}
	if doc.CurrentVersion != 2 || v2.Version != 2 || v2.Actor != agent || doc.ID != v1.DocumentID {
		t.Fatalf("v2 = %+v doc = %+v", v2, doc)
	}

	_, cur, err := e.svc.GetDocument(nextCCtx, ref, "architecture", "Overview", 0)
	if err != nil || cur.Body != "second" || cur.Version != 2 {
		t.Fatalf("current = %+v, %v", cur, err)
	}
	_, old, err := e.svc.GetDocument(nextCCtx, ref, "architecture", "Overview", 1)
	if err != nil || old.Body != "first" || old.Version != 1 {
		t.Fatalf("v1 = %+v, %v", old, err)
	}
	if _, explicit, err := e.svc.GetDocument(nextCCtx, ref, "architecture", "Overview", 2); err != nil || explicit.Body != "second" {
		t.Fatalf("explicit current = %+v, %v", explicit, err)
	}
	if _, _, err := e.svc.GetDocument(nextCCtx, ref, "architecture", "Overview", 3); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing version err = %v", err)
	}
	var v *domain.ValidationError
	if _, _, err := e.svc.GetDocument(nextCCtx, ref, "architecture", "Overview", -1); !errors.As(err, &v) {
		t.Fatalf("negative version err = %v", err)
	}
	if _, _, err := e.svc.GetDocument(nextCCtx, ref, "architecture", "Missing", 0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing document err = %v", err)
	}
}

func TestNextCDocumentIdentityIsKindAndTitle(t *testing.T) {
	e := nextCNew(t)
	ref := ProjectRef{Slug: "demo"}
	for _, d := range [][3]string{
		{"requirements", "Goals", "r"},
		{"architecture", "Overview", "a1"},
		{"architecture", "Data model", "a2"},
		{"architecture", " Overview ", "a3"}, // trimmed: same document as "Overview"
	} {
		if _, _, err := e.svc.PutDocument(nextCCtx, domain.Human, ref, d[0], d[1], d[2]); err != nil {
			t.Fatal(err)
		}
	}
	all, err := e.svc.ListDocuments(nextCCtx, ref, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("documents = %+v", all)
	}
	// Ordered by kind, then title.
	order := []string{"architecture/Data model", "architecture/Overview", "requirements/Goals"}
	for i, d := range all {
		if d.Kind+"/"+d.Title != order[i] {
			t.Fatalf("order = %+v", all)
		}
	}
	for _, d := range all {
		if d.Kind == "architecture" && d.Title == "Overview" && d.CurrentVersion != 2 {
			t.Fatalf("trimmed title should reuse the document: version %d", d.CurrentVersion)
		}
	}
	arch, err := e.svc.ListDocuments(nextCCtx, ref, "architecture")
	if err != nil || len(arch) != 2 {
		t.Fatalf("architecture docs = %+v, %v", arch, err)
	}
	none, err := e.svc.ListDocuments(nextCCtx, ref, "decisions")
	if err != nil || len(none) != 0 {
		t.Fatalf("decisions docs = %+v, %v", none, err)
	}
}

func TestNextCDocumentsAreScopedToProject(t *testing.T) {
	e := nextCNew(t)
	addProject(t, e.db, "other", "")
	if _, _, err := e.svc.PutDocument(nextCCtx, domain.Human, ProjectRef{Slug: "demo"}, "architecture", "Overview", "demo body"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.GetDocument(nextCCtx, ProjectRef{Slug: "other"}, "architecture", "Overview", 0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want not found in other project", err)
	}
}

func TestNextCDocumentWriteEmitsEvent(t *testing.T) {
	e := nextCNew(t)
	before, _ := e.svc.LatestEventID(nextCCtx, e.p.ID)
	if _, _, err := e.svc.PutDocument(nextCCtx, domain.Human, ProjectRef{Slug: "demo"}, "conventions", "Style", "tabs"); err != nil {
		t.Fatal(err)
	}
	page, err := e.svc.EventsSince(nextCCtx, e.p.ID, before, 10)
	if err != nil || len(page.Events) != 1 || page.Events[0].Type != domain.EventDocumentVersion || page.Events[0].EntityType != domain.EntityDocument {
		t.Fatalf("page = %+v, %v", page, err)
	}
}

func TestNextCDocumentValidation(t *testing.T) {
	e := nextCNew(t)
	ref := ProjectRef{Slug: "demo"}
	var v *domain.ValidationError
	for name, args := range map[string][3]string{
		"empty body":  {"architecture", "Overview", " "},
		"empty title": {"architecture", "", "x"},
		"empty kind":  {"", "Overview", "x"},
	} {
		if _, _, err := e.svc.PutDocument(nextCCtx, domain.Human, ref, args[0], args[1], args[2]); !errors.As(err, &v) {
			t.Errorf("%s: err = %v, want ValidationError", name, err)
		}
	}
	if _, _, err := e.svc.PutDocument(nextCCtx, domain.Human, ProjectRef{Slug: "nope"}, "architecture", "Overview", "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown project err = %v", err)
	}
	// A failed write must leave no document behind.
	if docs, _ := e.svc.ListDocuments(nextCCtx, ref, ""); len(docs) != 0 {
		t.Fatalf("documents = %+v", docs)
	}
}
