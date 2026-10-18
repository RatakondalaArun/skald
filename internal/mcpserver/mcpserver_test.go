package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"skald/internal/domain"
	"skald/internal/service"
	"skald/internal/store"
)

// ---- harness ----------------------------------------------------------

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func noEnv(string) string { return "" }

func openSvc(t *testing.T, dbPath string) *service.Service {
	t.Helper()
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return service.New(db, service.WithEnv(noEnv))
}

type client struct {
	t  *testing.T
	cs *mcp.ClientSession
}

// startServe runs Serve over in-process pipes and connects an SDK client.
func startServe(t *testing.T, svc *service.Service, clientName string, opts ...Option) *client {
	t.Helper()
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	opts = append([]Option{WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))}, opts...)
	go func() {
		err := Serve(ctx, svc, c2sR, s2cW, opts...)
		s2cW.Close()
		done <- err
	}()
	c := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: "1"}, nil)
	cs, err := c.Connect(ctx, &mcp.IOTransport{Reader: s2cR, Writer: c2sW}, nil)
	if err != nil {
		cancel()
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		cs.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v on clean disconnect", err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("Serve did not return after the client disconnected")
		}
		cancel()
	})
	return &client{t: t, cs: cs}
}

type reply struct {
	text  string
	isErr bool
	m     map[string]any
}

func (c *client) call(name string, args any) reply {
	c.t.Helper()
	res, err := c.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return reply{text: err.Error(), isErr: true}
	}
	var text string
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	r := reply{text: text, isErr: res.IsError}
	if !res.IsError {
		if err := json.Unmarshal([]byte(text), &r.m); err != nil {
			c.t.Fatalf("%s: result is not a JSON object: %q", name, text)
		}
	}
	return r
}

func (c *client) ok(name string, args any) map[string]any {
	c.t.Helper()
	r := c.call(name, args)
	if r.isErr {
		c.t.Fatalf("%s(%v) failed: %s", name, args, r.text)
	}
	return r.m
}

func (c *client) fail(name string, args any, want ...string) string {
	c.t.Helper()
	r := c.call(name, args)
	if !r.isErr {
		c.t.Fatalf("%s(%v) succeeded, want error; got %s", name, args, r.text)
	}
	low := strings.ToLower(r.text)
	for _, w := range want {
		if !strings.Contains(low, strings.ToLower(w)) {
			c.t.Fatalf("%s error %q does not mention %q", name, r.text, w)
		}
	}
	return r.text
}

func items(m map[string]any, key string) []map[string]any {
	raw, _ := m[key].([]any)
	out := make([]map[string]any, len(raw))
	for i, v := range raw {
		out[i], _ = v.(map[string]any)
	}
	return out
}

func str(m map[string]any, key string) string { s, _ := m[key].(string); return s }

type env struct {
	svc  *service.Service
	repo string
	c    *client
	db   string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "home", "skald.db")
	repo := filepath.Join(dir, "repo")
	mustMkdir(t, repo)
	svc := openSvc(t, dbPath)
	return &env{svc: svc, repo: repo, db: dbPath, c: startServe(t, svc, "test-client", WithCwd(repo))}
}

func twoTaskPlan() map[string]any {
	return map[string]any{"features": []any{
		map[string]any{"title": "Core", "tasks": []any{
			map[string]any{"ref": "a", "title": "Task A", "idempotency_key": "k-a", "priority": "high"},
			map[string]any{"ref": "b", "title": "Task B", "idempotency_key": "k-b", "depends_on": []any{"a"}},
		}},
	}}
}

// ---- tool surface -----------------------------------------------------

func TestToolListAndSchemas(t *testing.T) {
	e := newEnv(t)
	res, err := e.c.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"create_project", "get_project", "get_project_progress", "create_plan", "create_feature", "list_features",
		"create_task", "get_task", "list_tasks", "get_next_tasks", "start_task", "update_task_status",
		"add_task_note", "add_task_dependency", "record_verification", "report_task_result", "get_document", "put_document",
		"sync_github",
	}
	var got []string
	props := map[string]map[string]any{}
	for _, tl := range res.Tools {
		got = append(got, tl.Name)
		b, _ := json.Marshal(tl.InputSchema)
		var sc struct {
			Properties map[string]any `json:"properties"`
		}
		_ = json.Unmarshal(b, &sc)
		props[tl.Name] = sc.Properties
		if tl.Description == "" {
			t.Errorf("tool %s has no description", tl.Name)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v\nwant    %v", got, want)
	}
	for name, p := range props {
		if name == "create_project" {
			continue // names the project it creates (slug) instead of selecting one
		}
		if _, ok := p["project"]; !ok {
			t.Errorf("tool %s has no optional project argument", name)
		}
	}
	for _, name := range []string{"get_task", "start_task", "add_task_note", "record_verification", "report_task_result"} {
		if _, ok := props[name]["task"]; !ok {
			t.Errorf("tool %s has no task argument: %v", name, props[name])
		}
	}
	for _, tl := range res.Tools {
		if tl.Name == "create_plan" && !strings.Contains(tl.Description, "idempotency_key") {
			t.Errorf("create_plan description must mention idempotency_key: %q", tl.Description)
		}
		if tl.Name == "create_plan" && !strings.Contains(strings.ToLower(tl.Description), "evidence") {
			t.Errorf("create_plan description must mention evidence: %q", tl.Description)
		}
	}
}

func TestWorkflowPrompt(t *testing.T) {
	e := newEnv(t)
	res, err := e.c.cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "skald_workflow"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) == 0 {
		t.Fatal("prompt has no messages")
	}
	text := res.Messages[0].Content.(*mcp.TextContent).Text
	for _, tool := range []string{"create_plan", "get_next_tasks", "start_task", "add_task_note", "record_verification", "report_task_result"} {
		if !strings.Contains(text, tool) {
			t.Errorf("workflow prompt does not mention %s", tool)
		}
	}
}

// ---- full session -----------------------------------------------------

func TestFullPlanningSession(t *testing.T) {
	e := newEnv(t)
	c := e.c

	proj := c.ok("create_project", map[string]any{"name": "School Management", "description": "demo"})
	if str(proj, "project") != "school-management" || str(proj, "id") != "P-1" {
		t.Fatalf("create_project = %v", proj)
	}

	plan := map[string]any{"features": []any{
		map[string]any{"title": "Setup", "weight": 2, "tasks": []any{
			map[string]any{"ref": "init", "title": "Init repo", "idempotency_key": "k-init", "priority": "urgent"},
			map[string]any{"ref": "schema", "title": "Schema", "idempotency_key": "k-schema", "depends_on": []any{"init"}},
		}},
		map[string]any{"title": "API", "tasks": []any{
			map[string]any{"ref": "endpoints", "title": "Endpoints", "idempotency_key": "k-endpoints", "depends_on": []any{"schema"}},
			map[string]any{"ref": "auth", "title": "Auth", "idempotency_key": "k-auth", "depends_on": []any{"schema"}},
		}},
		map[string]any{"title": "Release", "tasks": []any{
			map[string]any{"ref": "docs", "title": "Docs", "idempotency_key": "k-docs", "depends_on": []any{"endpoints", "auth"}},
			map[string]any{"ref": "ship", "title": "Ship", "idempotency_key": "k-ship", "depends_on": []any{"docs"}},
		}},
	}}
	created := c.ok("create_plan", plan)
	if len(items(created, "tasks")) != 6 || len(items(created, "features")) != 3 || str(created, "project") != "school-management" {
		t.Fatalf("create_plan = %v", created)
	}
	for _, tk := range items(created, "tasks") {
		if !strings.HasPrefix(str(tk, "id"), "T-") || tk["created"] != true {
			t.Fatalf("plan task = %v", tk)
		}
	}

	// Re-running the plan creates nothing.
	again := c.ok("create_plan", plan)
	for _, tk := range items(again, "tasks") {
		if tk["created"] != false {
			t.Fatalf("re-run created %v", tk)
		}
	}

	if got := c.ok("get_project", nil); got["percent"].(float64) != 0 || got["require_evidence"] != true {
		t.Fatalf("get_project before work = %v", got)
	}

	done := 0
	for i := 0; i < 20; i++ {
		next := c.ok("get_next_tasks", nil)
		ts := items(next, "tasks")
		if len(ts) == 0 {
			break
		}
		id := str(ts[0], "id")
		started := c.ok("start_task", map[string]any{"task": id})
		if str(started["task"].(map[string]any), "status") != "in_progress" || str(started, "project") != "school-management" {
			t.Fatalf("start_task = %v", started)
		}
		c.ok("add_task_note", map[string]any{"task": id, "text": "working on " + id})
		if done%2 == 0 {
			c.ok("record_verification", map[string]any{"task": id, "kind": "commit", "ref": fmt.Sprintf("sha%d", done), "summary": "implemented"})
			c.ok("report_task_result", map[string]any{"task": id, "summary": "done"})
		} else {
			res := c.ok("report_task_result", map[string]any{
				"task": id, "summary": "done",
				"evidence": []any{map[string]any{"kind": "test_report", "ref": "go test ./...", "summary": "green"}},
			})
			if str(res["task"].(map[string]any), "status") != "done" || len(items(res, "evidence_added")) != 1 {
				t.Fatalf("report_task_result = %v", res)
			}
		}
		done++
	}
	if done != 6 {
		t.Fatalf("completed %d tasks, want 6", done)
	}

	prog := c.ok("get_project_progress", nil)
	if prog["percent"].(float64) != 100 {
		t.Fatalf("progress = %v", prog)
	}
	for _, f := range items(prog, "features") {
		if str(f, "status") != "done" {
			t.Fatalf("feature not done: %v", f)
		}
	}
	if next := c.ok("get_next_tasks", nil); len(items(next, "tasks")) != 0 || str(next, "note") == "" {
		t.Fatalf("next after completion = %v", next)
	}

	// Read tools on the finished project.
	task := c.ok("get_task", map[string]any{"task": "T-1"})
	if str(task, "status") != "done" || len(items(task, "evidence")) == 0 || len(items(task, "events")) == 0 {
		t.Fatalf("get_task = %v", task)
	}
	if lt := c.ok("list_tasks", map[string]any{"status": []any{"done"}}); len(items(lt, "tasks")) != 6 || lt["truncated"] != nil {
		t.Fatalf("list_tasks = %v", lt)
	}
	if lt := c.ok("list_tasks", map[string]any{"limit": 2}); len(items(lt, "tasks")) != 2 || lt["truncated"] != true || str(lt, "note") == "" {
		t.Fatalf("truncated list_tasks = %v", lt)
	}
	if lf := c.ok("list_features", nil); len(items(lf, "features")) != 3 {
		t.Fatalf("list_features = %v", lf)
	}

	// Documents are versioned.
	c.ok("put_document", map[string]any{"kind": "architecture", "title": "Overview", "body": "v1"})
	v2 := c.ok("put_document", map[string]any{"kind": "architecture", "title": "Overview", "body": "v2"})
	if v2["version"].(float64) != 2 {
		t.Fatalf("put_document = %v", v2)
	}
	cur := c.ok("get_document", map[string]any{"kind": "architecture", "title": "Overview"})
	old := c.ok("get_document", map[string]any{"kind": "architecture", "title": "Overview", "version": 1})
	if str(cur, "body") != "v2" || str(old, "body") != "v1" {
		t.Fatalf("documents: cur=%v old=%v", cur, old)
	}
	if ld := c.ok("get_document", nil); len(items(ld, "documents")) != 1 {
		t.Fatalf("document list = %v", ld)
	}

	// Every change is attributed to the MCP client.
	page, err := e.svc.EventsSince(context.Background(), 1, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) == 0 {
		t.Fatal("no events recorded")
	}
	for _, ev := range page.Events {
		if !strings.HasPrefix(string(ev.Actor), "claude:test-client/") {
			t.Fatalf("event %d actor = %q", ev.ID, ev.Actor)
		}
	}
}

func TestFeatureTaskAndDependencyTools(t *testing.T) {
	e := newEnv(t)
	c := e.c
	c.ok("create_project", map[string]any{"name": "Demo"})
	f := c.ok("create_feature", map[string]any{"title": "Auth", "weight": 3})
	fID := str(f, "id")
	if fID != "F-1" {
		t.Fatalf("create_feature = %v", f)
	}
	a := c.ok("create_task", map[string]any{"feature": fID, "title": "A", "idempotency_key": "ka", "priority": "low"})
	b := c.ok("create_task", map[string]any{"feature": fID, "title": "B", "idempotency_key": "kb"})
	if a["created"] != true || b["created"] != true {
		t.Fatalf("create_task = %v %v", a, b)
	}
	if dup := c.ok("create_task", map[string]any{"feature": fID, "title": "A", "idempotency_key": "ka"}); dup["created"] != false {
		t.Fatalf("duplicate create_task = %v", dup)
	}
	aID, bID := str(a["task"].(map[string]any), "id"), str(b["task"].(map[string]any), "id")
	dep := c.ok("add_task_dependency", map[string]any{"task": bID, "depends_on": aID})
	if dep["added"] != true || str(dep, "project") != "demo" {
		t.Fatalf("add_task_dependency = %v", dep)
	}
	g := c.ok("get_task", map[string]any{"task": bID})
	if g["ready"] != false || len(items(g, "depends_on")) != 1 || len(g["unmet"].([]any)) != 1 {
		t.Fatalf("get_task B = %v", g)
	}
	// Block, unblock, cancel via update_task_status.
	c.ok("start_task", map[string]any{"task": aID})
	blk := c.ok("update_task_status", map[string]any{"task": aID, "status": "blocked", "blocked_reason": "waiting on API key"})
	if str(blk["task"].(map[string]any), "blocked_reason") != "waiting on API key" {
		t.Fatalf("blocked = %v", blk)
	}
	if p := c.ok("get_project_progress", nil); len(items(p, "blocked")) != 1 {
		t.Fatalf("progress should list the blocked task: %v", p)
	}
	c.ok("update_task_status", map[string]any{"task": aID, "status": "in_progress"})
	c.ok("update_task_status", map[string]any{"task": aID, "status": "cancelled"})
	if r := c.ok("get_next_tasks", nil); len(items(r, "tasks")) != 1 || str(items(r, "tasks")[0], "id") != bID {
		t.Fatalf("cancelled dependency should free B: %v", r)
	}
}

// ---- error paths ------------------------------------------------------

func TestErrorPaths(t *testing.T) {
	e := newEnv(t)
	c := e.c
	c.ok("create_project", map[string]any{"name": "Errors"})
	created := c.ok("create_plan", twoTaskPlan())
	ids := map[string]string{}
	for _, tk := range items(created, "tasks") {
		ids[str(tk, "ref")] = str(tk, "id")
	}
	a, b := ids["a"], ids["b"]

	t.Run("dependency-gated start", func(t *testing.T) {
		msg := c.fail("start_task", map[string]any{"task": b}, "cannot start", "unfinished", a)
		if !strings.Contains(msg, b) {
			t.Errorf("message should name the task: %q", msg)
		}
	})

	t.Run("evidence required", func(t *testing.T) {
		c.ok("start_task", map[string]any{"task": a})
		c.fail("update_task_status", map[string]any{"task": a, "status": "done"}, "evidence")
		c.fail("report_task_result", map[string]any{"task": a, "summary": "done"}, "evidence")
		// Nothing was written by the failed report: still no note, still in progress.
		g := c.ok("get_task", map[string]any{"task": a})
		if str(g, "status") != "in_progress" || g["evidence"] != nil {
			t.Fatalf("failed report must write nothing: %v", g)
		}
		for _, ev := range items(g, "events") {
			if str(ev, "type") == "task.note" {
				t.Fatalf("failed report left a note: %v", ev)
			}
		}
	})

	t.Run("dependency cycle", func(t *testing.T) {
		msg := c.fail("add_task_dependency", map[string]any{"task": a, "depends_on": b}, "cycle")
		if !strings.Contains(msg, a) || !strings.Contains(msg, b) {
			t.Errorf("cycle message should show the path: %q", msg)
		}
	})

	t.Run("plan cycle", func(t *testing.T) {
		c.fail("create_plan", map[string]any{"features": []any{map[string]any{"title": "Loop", "tasks": []any{
			map[string]any{"ref": "x", "title": "X", "idempotency_key": "lx", "depends_on": []any{"y"}},
			map[string]any{"ref": "y", "title": "Y", "idempotency_key": "ly", "depends_on": []any{"x"}},
		}}}}, "circular", "x", "y")
		if lf := c.ok("list_features", nil); len(items(lf, "features")) != 1 {
			t.Fatalf("rolled-back plan left features behind: %v", lf)
		}
	})

	t.Run("bad ids", func(t *testing.T) {
		c.fail("get_task", map[string]any{"task": "X-9"}, "task")
		c.fail("get_task", map[string]any{"task": "banana"}, "task")
		c.fail("get_task", map[string]any{"task": "F-1"}, "feature")
		c.fail("start_task", map[string]any{"task": "T-0"}, "task")
		c.fail("create_task", map[string]any{"feature": "T-1", "title": "x"}, "feature")
		c.fail("get_task", map[string]any{"task": "T-999"}, "not found")
	})

	t.Run("bad enums", func(t *testing.T) {
		c.fail("update_task_status", map[string]any{"task": a, "status": "finished"}, "status")
		c.fail("create_plan", map[string]any{"features": []any{map[string]any{"title": "F", "tasks": []any{
			map[string]any{"ref": "z", "title": "Z", "priority": "asap"},
		}}}}, "priority")
		c.fail("list_tasks", map[string]any{"status": []any{"nope"}}, "status")
		c.fail("record_verification", map[string]any{"task": a, "kind": "vibes", "ref": "x"}, "kind")
	})

	t.Run("unknown project", func(t *testing.T) {
		c.fail("get_project", map[string]any{"project": "nope"}, "not found")
		c.fail("get_next_tasks", map[string]any{"project": "nope"}, "not found")
	})

	t.Run("version conflict", func(t *testing.T) {
		msg := c.fail("update_task_status", map[string]any{"task": a, "status": "blocked", "blocked_reason": "x", "expected_version": 1}, "expected_version")
		if !strings.Contains(msg, "in_progress") {
			t.Errorf("conflict message should show the current state: %q", msg)
		}
	})

	t.Run("blocked needs a reason", func(t *testing.T) {
		c.fail("update_task_status", map[string]any{"task": a, "status": "blocked"}, "reason")
	})
}

func TestAmbiguousProject(t *testing.T) {
	dir := t.TempDir()
	svc := openSvc(t, filepath.Join(dir, "skald.db"))
	other := filepath.Join(dir, "elsewhere")
	mustMkdir(t, other)
	c := startServe(t, svc, "test-client", WithCwd(other))
	for _, name := range []string{"Alpha", "Beta"} {
		repo := filepath.Join(dir, strings.ToLower(name))
		mustMkdir(t, repo)
		c.ok("create_project", map[string]any{"name": name, "repo_path": repo})
	}
	c.fail("get_project", nil, "alpha", "beta")
	if got := c.ok("get_project", map[string]any{"project": "beta"}); str(got, "project") != "beta" {
		t.Fatalf("explicit project = %v", got)
	}
	// Task-ID tools do not need a project: they work even when none can be guessed.
	c.ok("create_plan", map[string]any{"project": "alpha", "features": []any{map[string]any{"title": "F", "tasks": []any{
		map[string]any{"ref": "a", "title": "A", "idempotency_key": "ka"},
	}}}})
	started := c.ok("start_task", map[string]any{"task": "T-1"})
	if started["project"] != nil {
		t.Fatalf("project must be omitted when it cannot be resolved: %v", started)
	}
}

// ---- stdout hygiene ---------------------------------------------------

func TestStdoutCarriesOnlyJSONRPC(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	mustMkdir(t, repo)
	svc := openSvc(t, filepath.Join(dir, "skald.db"))

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	stderr := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	served := make(chan error, 1)
	go func() {
		err := Serve(context.Background(), svc, inR, outW, WithLogger(logger), WithCwd(repo))
		outW.Close()
		served <- err
	}()

	var mu sync.Mutex
	var lines []string
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			mu.Lock()
			lines = append(lines, sc.Text())
			mu.Unlock()
		}
	}()

	send := func(s string) {
		if _, err := fmt.Fprintln(inW, s); err != nil {
			t.Fatalf("write to server: %v", err)
		}
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}}`)
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_project","arguments":{"name":"Raw"}}}`)
	send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_task","arguments":{"task":"bogus"}}}`)
	send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`)
	send(`{"jsonrpc":"2.0","id":6,"method":"prompts/get","params":{"name":"skald_workflow"}}`)
	send(`{"jsonrpc":"2.0","id":7,"method":"ping"}`)

	waitFor := func(n int) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			got := len(lines)
			mu.Unlock()
			if got >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %d output lines", n)
	}
	waitFor(7)

	inW.Close()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve = %v, want nil after stdin EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after stdin closed")
	}
	<-readDone

	mu.Lock()
	defer mu.Unlock()
	seen := map[float64]bool{}
	for _, l := range lines {
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *float64        `json:"id"`
			Method  string          `json:"method"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		dec := json.NewDecoder(strings.NewReader(l))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&msg); err != nil {
			t.Fatalf("stdout line is not a JSON-RPC message: %q (%v)", l, err)
		}
		if msg.JSONRPC != "2.0" {
			t.Fatalf("stdout line without jsonrpc 2.0: %q", l)
		}
		if msg.ID != nil {
			seen[*msg.ID] = true
			if msg.Result == nil && msg.Error == nil {
				t.Fatalf("response without result or error: %q", l)
			}
		}
		if strings.Contains(l, "level=") {
			t.Fatalf("a log line leaked onto stdout: %q", l)
		}
	}
	for id := 1; id <= 7; id++ {
		if !seen[float64(id)] {
			t.Errorf("no response for request %d; lines: %v", id, lines)
		}
	}
	if !strings.Contains(stderr.String(), "tool call") {
		t.Errorf("expected debug logs on the injected stderr logger, got %q", stderr.String())
	}
}

// ---- concurrency ------------------------------------------------------

func TestConcurrentServers(t *testing.T) {
	const n = 3
	const perClient = 5
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "skald.db")
	repo := filepath.Join(dir, "repo")
	mustMkdir(t, repo)

	// Separate store handles, like separate `skald mcp` processes.
	clients := make([]*client, n)
	svcs := make([]*service.Service, n)
	for i := range clients {
		svcs[i] = openSvc(t, dbPath)
		clients[i] = startServe(t, svcs[i], fmt.Sprintf("client-%d", i), WithCwd(repo))
	}
	clients[0].ok("create_project", map[string]any{"name": "Shared"})

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := clients[i]
			if err := runClient(c, i, perClient); err != nil {
				errs <- fmt.Errorf("client %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	prog := clients[0].ok("get_project_progress", nil)
	if prog["percent"].(float64) != 100 {
		t.Fatalf("progress after concurrent sessions = %v", prog)
	}
	if lt := clients[1].ok("list_tasks", map[string]any{"status": []any{"done"}}); len(items(lt, "tasks")) != n*perClient {
		t.Fatalf("done tasks = %d, want %d", len(items(lt, "tasks")), n*perClient)
	}
	page, err := svcs[2].EventsSince(context.Background(), 1, 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	actors := map[domain.Actor]bool{}
	var last int64
	for _, ev := range page.Events {
		if ev.ID <= last {
			t.Fatalf("event ids not strictly increasing: %d after %d", ev.ID, last)
		}
		last = ev.ID
		actors[ev.Actor] = true
	}
	for i := 0; i < n; i++ {
		found := false
		for a := range actors {
			if strings.HasPrefix(string(a), fmt.Sprintf("claude:client-%d/", i)) {
				found = true
			}
		}
		if !found {
			t.Errorf("no events from client-%d; actors = %v", i, actors)
		}
	}
}

// runClient plans its own feature and works only its own tasks.
func runClient(c *client, idx, count int) error {
	call := func(name string, args any) (map[string]any, error) {
		r := c.call(name, args)
		if r.isErr {
			return nil, fmt.Errorf("%s: %s", name, r.text)
		}
		return r.m, nil
	}
	var tasks []any
	for k := 0; k < count; k++ {
		task := map[string]any{"ref": fmt.Sprintf("t%d", k), "title": fmt.Sprintf("c%d task %d", idx, k), "idempotency_key": fmt.Sprintf("c%d-t%d", idx, k)}
		if k > 0 {
			task["depends_on"] = []any{fmt.Sprintf("t%d", k-1)}
		}
		tasks = append(tasks, task)
	}
	created, err := call("create_plan", map[string]any{"features": []any{map[string]any{"title": fmt.Sprintf("Feature %d", idx), "tasks": tasks}}})
	if err != nil {
		return err
	}
	mine := map[string]bool{}
	for _, tk := range items(created, "tasks") {
		mine[str(tk, "id")] = true
	}
	for done := 0; done < count; {
		next, err := call("get_next_tasks", map[string]any{"limit": 50})
		if err != nil {
			return err
		}
		progressed := false
		for _, tk := range items(next, "tasks") {
			id := str(tk, "id")
			if !mine[id] {
				continue
			}
			if _, err := call("start_task", map[string]any{"task": id}); err != nil {
				return err
			}
			if _, err := call("add_task_note", map[string]any{"task": id, "text": "working"}); err != nil {
				return err
			}
			if _, err := call("report_task_result", map[string]any{
				"task": id, "summary": "ok",
				"evidence": []any{map[string]any{"kind": "commit", "ref": "sha-" + id}},
			}); err != nil {
				return err
			}
			done++
			progressed = true
		}
		if !progressed {
			return fmt.Errorf("no ready task of mine after %d/%d done: %v", done, count, next)
		}
	}
	return nil
}

// ---- helpers ----------------------------------------------------------

func TestSlugifyAndPriority(t *testing.T) {
	for in, want := range map[string]string{
		"School Management": "school-management",
		"  My_App v2!! ":    "my-app-v2",
		"---":               "",
		"Ünïcode name":      "n-code-name",
		"already-a-slug":    "already-a-slug",
	} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]int{"urgent": 0, "HIGH": 1, " normal ": 2, "low": 3} {
		p, err := parsePriority(in)
		if err != nil || p == nil || *p != want {
			t.Errorf("parsePriority(%q) = %v, %v; want %d", in, p, err, want)
		}
	}
	if p, err := parsePriority(""); p != nil || err != nil {
		t.Errorf("empty priority = %v, %v", p, err)
	}
	if _, err := parsePriority("asap"); err == nil {
		t.Error("expected an error for an unknown priority")
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}
