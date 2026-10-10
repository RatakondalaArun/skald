package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"skald/internal/domain"
	"skald/internal/service"
)

func TestAPIReads(t *testing.T) {
	e := wtNew(t)
	// Move T-1 along so the data is not all "todo".
	if _, err := e.svc.StartTask(wtCtx, domain.Human, 1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddTaskNote(wtCtx, domain.Human, 1, "halfway"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.PutDocument(wtCtx, domain.Human, service.ProjectRef{Slug: "demo"}, "architecture", "Overview", "Hello body"); err != nil {
		t.Fatal(err)
	}

	t.Run("projects", func(t *testing.T) {
		code, m := e.get("/api/projects")
		list, _ := m["projects"].([]any)
		if code != 200 || len(list) != 1 {
			t.Fatalf("%d %v", code, m)
		}
		o := list[0].(map[string]any)
		p := o["project"].(map[string]any)
		if p["id"] != "P-1" || p["slug"] != "demo" || o["features"].(float64) != 2 {
			t.Fatalf("project = %v", o)
		}
		if c := o["counts"].(map[string]any); c["in_progress"].(float64) != 1 || c["todo"].(float64) != 3 {
			t.Fatalf("counts = %v", c)
		}
	})
	t.Run("project summary", func(t *testing.T) {
		code, m := e.get("/api/projects/demo")
		if code != 200 {
			t.Fatalf("%d %v", code, m)
		}
		feats := m["features"].([]any)
		if len(feats) != 2 || feats[0].(map[string]any)["id"] != "F-1" || feats[0].(map[string]any)["status"] != "active" {
			t.Fatalf("features = %v", feats)
		}
		if m["ready_count"].(float64) != 1 { // T-1 is started, T-2 waits for it, so only T-3 is ready
			t.Fatalf("ready_count = %v", m["ready_count"])
		}
		docs := m["documents"].([]any)
		if len(docs) != 1 || docs[0].(map[string]any)["title"] != "Overview" {
			t.Fatalf("documents = %v", docs)
		}
	})
	t.Run("features", func(t *testing.T) {
		code, m := e.get("/api/projects/demo/features")
		if code != 200 || len(m["features"].([]any)) != 2 {
			t.Fatalf("%d %v", code, m)
		}
	})
	t.Run("board has ready flags and priority order", func(t *testing.T) {
		code, m := e.get("/api/projects/demo/board")
		if code != 200 {
			t.Fatalf("%d %v", code, m)
		}
		tasks := m["tasks"].([]any)
		if len(tasks) != 4 || m["capped"] != false {
			t.Fatalf("tasks = %v", tasks)
		}
		ready := map[string]bool{}
		for _, x := range tasks {
			tk := x.(map[string]any)
			if tk["ready"] == true {
				ready[tk["id"].(string)] = true
			}
			if tk["description"] != nil {
				t.Errorf("board tasks should not carry long text: %v", tk)
			}
		}
		if !ready["T-3"] || ready["T-2"] || ready["T-1"] {
			t.Errorf("ready = %v, want only T-3", ready)
		}
	})
	t.Run("tasks with filters", func(t *testing.T) {
		code, m := e.get("/api/projects/demo/tasks?status=todo,in_progress&feature=F-1&limit=5")
		if code != 200 || len(m["tasks"].([]any)) != 2 {
			t.Fatalf("%d %v", code, m)
		}
		code, m = e.get("/api/projects/demo/tasks?ready=1")
		if code != 200 || len(m["tasks"].([]any)) != 1 {
			t.Fatalf("ready: %d %v", code, m)
		}
		code, m = e.get("/api/projects/demo/tasks?limit=1")
		if code != 200 || m["truncated"] != true {
			t.Fatalf("truncated: %d %v", code, m)
		}
	})
	t.Run("task detail", func(t *testing.T) {
		code, m := e.get("/api/projects/demo/tasks/T-1")
		if code != 200 {
			t.Fatalf("%d %v", code, m)
		}
		tk := m["task"].(map[string]any)
		if tk["status"] != "in_progress" || tk["description"] != "Create the tables." || tk["acceptance_criteria"] != "Migration runs." {
			t.Fatalf("task = %v", tk)
		}
		if m["feature"].(map[string]any)["title"] != "Accounts" || len(m["dependents"].([]any)) != 1 {
			t.Fatalf("detail = %v", m)
		}
		evs := m["events"].([]any)
		if len(evs) < 3 {
			t.Fatalf("events = %v", evs)
		}
		if first := evs[0].(map[string]any); first["type"] != "task.note" || first["title"] != "Schema" || first["task"] != "T-1" || first["project"] != "demo" {
			t.Fatalf("newest event = %v", first)
		}
		// A dependent shows what it is waiting for.
		_, m2 := e.get("/api/projects/demo/tasks/2")
		if un := m2["unmet"].([]any); len(un) != 1 || un[0] != "T-1" {
			t.Fatalf("unmet = %v", m2["unmet"])
		}
	})
	t.Run("next", func(t *testing.T) {
		code, m := e.get("/api/projects/demo/next?limit=5")
		if code != 200 || m["total"].(float64) != 1 {
			t.Fatalf("%d %v", code, m)
		}
	})
	t.Run("documents", func(t *testing.T) {
		code, m := e.get("/api/projects/demo/documents")
		if code != 200 || len(m["documents"].([]any)) != 1 {
			t.Fatalf("%d %v", code, m)
		}
		code, m = e.get("/api/projects/demo/document?kind=architecture&title=Overview")
		if code != 200 || m["body"] != "Hello body" || m["version"].(float64) != 1 {
			t.Fatalf("%d %v", code, m)
		}
	})
	t.Run("events recent and paged", func(t *testing.T) {
		code, m := e.get("/api/projects/demo/events?limit=3")
		evs := m["events"].([]any)
		if code != 200 || len(evs) != 3 {
			t.Fatalf("%d %v", code, m)
		}
		ids := wtIDs(evs)
		if !(ids[0] < ids[1] && ids[1] < ids[2]) {
			t.Fatalf("recent events must be oldest first: %v", ids)
		}
		latest, _ := e.svc.LatestEventID(wtCtx, 0)
		if ids[2] != latest {
			t.Fatalf("recent window should end at the newest event %d, got %v", latest, ids)
		}
		// Forward paging from a cursor, across the whole log.
		var seen []int64
		cursor := int64(0)
		for i := 0; i < 50; i++ {
			_, pg := e.get(fmt.Sprintf("/api/events?project=demo&cursor=%d&limit=4", cursor))
			got := wtIDs(pg["events"].([]any))
			seen = append(seen, got...)
			cursor = int64(pg["cursor"].(float64))
			if pg["more"] != true {
				break
			}
		}
		for i := 1; i < len(seen); i++ {
			if seen[i] != seen[i-1]+1 && seen[i] <= seen[i-1] {
				t.Fatalf("paging gave out-of-order ids: %v", seen)
			}
		}
		if int64(len(seen)) < latest-1 { // project-scoped, but every event here belongs to the one project
			t.Fatalf("paged %d events, expected about %d", len(seen), latest)
		}
	})
	t.Run("recent events widen the window past other projects' noise", func(t *testing.T) {
		if _, err := e.svc.CreateProject(wtCtx, domain.Human, service.CreateProjectInput{Slug: "noise", Name: "Noise"}); err != nil {
			t.Fatal(err)
		}
		nf, err := e.svc.CreateFeature(wtCtx, domain.Human, service.ProjectRef{Slug: "noise"}, service.CreateFeatureInput{Title: "Noise feature"})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 60; i++ {
			if _, _, err := e.svc.CreateTask(wtCtx, domain.Human, service.ProjectRef{Slug: "noise"}, service.CreateTaskInput{FeatureID: nf.ID, Title: fmt.Sprintf("n%d", i)}); err != nil {
				t.Fatal(err)
			}
		}
		_, m := e.get("/api/projects/demo/events?limit=2")
		if len(m["events"].([]any)) != 2 {
			t.Fatalf("expected the last 2 demo events despite 60 newer events elsewhere: %v", m)
		}
	})
	t.Run("projects without archived", func(t *testing.T) {
		code, m := e.get("/api/projects?archived=1")
		if code != 200 || len(m["projects"].([]any)) < 1 {
			t.Fatalf("%d %v", code, m)
		}
	})
}

func wtIDs(evs []any) []int64 {
	out := make([]int64, len(evs))
	for i, x := range evs {
		out[i] = int64(x.(map[string]any)["id"].(float64))
	}
	return out
}

func TestAPIErrors(t *testing.T) {
	e := wtNew(t)
	cases := []struct {
		name, path string
		status     int
		kind       string
	}{
		{"unknown project", "/api/projects/nope", 404, "not_found"},
		{"unknown project board", "/api/projects/nope/board", 404, "not_found"},
		{"unknown task", "/api/projects/demo/tasks/T-99", 404, "not_found"},
		{"bad task id", "/api/projects/demo/tasks/banana", 400, "validation"},
		{"feature id used as task id", "/api/projects/demo/tasks/F-1", 400, "validation"},
		{"bad status filter", "/api/projects/demo/tasks?status=sleeping", 400, "validation"},
		{"bad feature filter", "/api/projects/demo/tasks?feature=xyz", 400, "validation"},
		{"bad limit", "/api/projects/demo/tasks?limit=-1", 400, "validation"},
		{"limit too large", "/api/projects/demo/next?limit=9999", 400, "validation"},
		{"bad cursor", "/api/projects/demo/events?cursor=abc", 400, "validation"},
		{"missing document", "/api/projects/demo/document?kind=x&title=y", 404, "not_found"},
		{"unknown endpoint", "/api/nothing", 404, "not_found"},
		{"events for unknown project", "/api/events?project=nope", 404, "not_found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, m := e.get(c.path)
			if code != c.status || wtErrKind(m) != c.kind {
				t.Fatalf("got %d %v, want %d %s", code, m, c.status, c.kind)
			}
			if wtErrMsg(m) == "" {
				t.Error("error body has no message")
			}
		})
	}
}

// Every domain error kind maps to its documented status, and an unknown error
// becomes a 500 that does not leak its text.
func TestFailMapping(t *testing.T) {
	e := wtNew(t)
	task := domain.Task{ID: 7, Status: domain.TaskTodo, Version: 4, Title: "Seven"}
	cases := []struct {
		name   string
		err    error
		status int
		kind   string
	}{
		{"not found", &domain.NotFoundError{Entity: domain.EntityTask, ID: 9}, 404, "not_found"},
		{"validation", &domain.ValidationError{Field: "x", Problem: "bad"}, 400, "validation"},
		{"conflict", &domain.ConflictError{Entity: domain.EntityTask, ID: 7, Expected: 1, Current: 4, CurrentTask: &task}, 409, "conflict"},
		{"invalid transition", &domain.InvalidTransitionError{TaskID: 7, From: "todo", To: "done"}, 422, "invalid_transition"},
		{"evidence required", &domain.EvidenceRequiredError{TaskID: 7}, 422, "evidence_required"},
		{"dependency cycle", &domain.DependencyCycleError{Path: []int64{1, 2, 1}}, 422, "dependency_cycle"},
		{"wrapped domain error", fmt.Errorf("wrap: %w", &domain.NotFoundError{Entity: domain.EntityTask, ID: 1}), 404, "not_found"},
		{"unknown error", errors.New("pq: password=hunter2 connection refused"), 500, "internal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			e.srv.fail(rec, httptest.NewRequest("GET", "/x", nil), c.err)
			m := wtJSON(t, rec.Body.Bytes())
			if rec.Code != c.status || wtErrKind(m) != c.kind {
				t.Fatalf("got %d %v, want %d %s", rec.Code, m, c.status, c.kind)
			}
			if c.status == 500 && strings.Contains(rec.Body.String(), "hunter2") {
				t.Fatalf("500 leaked internals: %s", rec.Body)
			}
			if c.kind == "conflict" {
				cur := m["error"].(map[string]any)["current_task"].(map[string]any)
				if cur["id"] != "T-7" || cur["version"].(float64) != 4 {
					t.Fatalf("conflict body should carry the current task: %v", m)
				}
			}
			var de domain.Error
			if c.status != 500 && errors.As(c.err, &de) && wtErrMsg(m) != de.Message() {
				t.Errorf("message = %q, want the domain message %q", wtErrMsg(m), de.Message())
			}
		})
	}
}

func TestTaskActions(t *testing.T) {
	e := wtNew(t)
	ctx := wtCtx

	// Cancel a todo task.
	code, m := e.post("/api/projects/demo/tasks/T-3/cancel", `{}`)
	if code != 200 || m["task"].(map[string]any)["status"] != "cancelled" {
		t.Fatalf("cancel: %d %v", code, m)
	}
	// Reopen it, with a matching expected_version.
	v := int64(m["task"].(map[string]any)["version"].(float64))
	code, m = e.post("/api/projects/demo/tasks/T-3/reopen", fmt.Sprintf(`{"expected_version": %d}`, v))
	if code != 200 || m["task"].(map[string]any)["status"] != "todo" {
		t.Fatalf("reopen: %d %v", code, m)
	}
	// A stale expected_version is a conflict carrying the current task.
	code, m = e.post("/api/projects/demo/tasks/T-3/cancel", fmt.Sprintf(`{"expected_version": %d}`, v))
	if code != 409 || wtErrKind(m) != "conflict" {
		t.Fatalf("stale version: %d %v", code, m)
	}
	if cur := m["error"].(map[string]any)["current_task"].(map[string]any); cur["status"] != "todo" {
		t.Fatalf("current_task = %v", cur)
	}
	// Reopening a task that is not done or cancelled is an invalid transition.
	code, m = e.post("/api/projects/demo/tasks/T-1/reopen", `{}`)
	if code != 422 || wtErrKind(m) != "invalid_transition" {
		t.Fatalf("reopen todo: %d %v", code, m)
	}
	// Unblock: a task that was started returns to in_progress.
	if _, err := e.svc.StartTask(ctx, domain.Human, 1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateTaskStatus(ctx, domain.Human, 1, service.StatusChange{To: domain.TaskBlocked, BlockedReason: "waiting on keys"}); err != nil {
		t.Fatal(err)
	}
	code, m = e.post("/api/projects/demo/tasks/T-1/unblock", `{}`)
	if code != 200 || m["task"].(map[string]any)["status"] != "in_progress" {
		t.Fatalf("unblock started task: %d %v", code, m)
	}
	// Unblock of a never-started task goes back to todo.
	if _, err := e.svc.UpdateTaskStatus(ctx, domain.Human, 4, service.StatusChange{To: domain.TaskBlocked, BlockedReason: "external"}); err != nil {
		t.Fatal(err)
	}
	code, m = e.post("/api/projects/demo/tasks/T-4/unblock", `{}`)
	if code != 200 || m["task"].(map[string]any)["status"] != "todo" {
		t.Fatalf("unblock fresh task: %d %v", code, m)
	}
	// Bad bodies and ids.
	if code, m = e.post("/api/projects/demo/tasks/T-3/cancel", `{"nope": 1}`); code != 400 {
		t.Fatalf("unknown field: %d %v", code, m)
	}
	if code, m = e.post("/api/projects/demo/tasks/T-3/cancel", `{"expected_version": -2}`); code != 400 {
		t.Fatalf("negative version: %d %v", code, m)
	}
	if code, m = e.post("/api/projects/demo/tasks/T-99/cancel", `{}`); code != 404 {
		t.Fatalf("missing task: %d %v", code, m)
	}
	// A task of another project is not found through this project's URL.
	if _, err := e.svc.CreateProject(ctx, domain.Human, service.CreateProjectInput{Slug: "other", Name: "Other"}); err != nil {
		t.Fatal(err)
	}
	if code, m = e.post("/api/projects/other/tasks/T-3/cancel", `{}`); code != 404 {
		t.Fatalf("cross-project: %d %v", code, m)
	}
	// Changes are attributed to the dashboard actor.
	_, m = e.get("/api/projects/demo/tasks/T-3")
	found := false
	for _, x := range m["events"].([]any) {
		if x.(map[string]any)["actor"] == "human:web" {
			found = true
		}
	}
	if !found {
		t.Errorf("no event by human:web in %v", m["events"])
	}
}

func TestIndexAndStatic(t *testing.T) {
	e := wtNew(t)
	ts := e.server()
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, res)
	if res.StatusCode != 200 || !strings.Contains(body, `name="skald-token" content="`+e.srv.Token()+`"`) {
		t.Fatalf("index should embed the token: %d %s", res.StatusCode, body)
	}
	if strings.Contains(body, "{{") {
		t.Error("unreplaced placeholder in index")
	}
	// CSP forbids inline script, so the page must not need any.
	for _, bad := range []string{"onclick=", "onload=", "javascript:"} {
		if strings.Contains(body, bad) {
			t.Errorf("index contains %q", bad)
		}
	}
	for _, tag := range strings.Split(body, "<script")[1:] {
		if !strings.Contains(strings.SplitN(tag, ">", 2)[0], "src=") {
			t.Error("inline <script> in index")
		}
	}
	for _, p := range []string{"/static/app.js", "/static/views.js", "/static/util.js", "/static/style.css"} {
		r, err := http.Get(ts.URL + p)
		if err != nil || r.StatusCode != 200 {
			t.Errorf("%s: %v %v", p, r, err)
			continue
		}
		r.Body.Close()
	}
	if r, _ := http.Get(ts.URL + "/nope"); r.StatusCode != 404 {
		t.Errorf("/nope = %d", r.StatusCode)
	}
}

func readAll(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return sb.String()
		}
	}
}
