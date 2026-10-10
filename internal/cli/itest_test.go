package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"skald/internal/store"
)

var skaldBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "skald-cli-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	skaldBin = filepath.Join(dir, "skald")
	if runtime.GOOS == "windows" {
		skaldBin += ".exe"
	}
	build := exec.Command("go", "build", "-o", skaldBin, "skald/cmd/skald")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// sandbox is a temp SKALD_HOME and a repository directory to run from.
type sandbox struct {
	t    *testing.T
	home string
	repo string
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	root := t.TempDir()
	s := &sandbox{t: t, home: filepath.Join(root, "home"), repo: filepath.Join(root, "repo")}
	if err := os.MkdirAll(s.repo, 0o755); err != nil {
		t.Fatal(err)
	}
	return s
}

type result struct {
	stdout, stderr string
	code           int
}

// runIn runs skald in dir with extra environment (KEY=VALUE).
func (s *sandbox) runIn(dir string, extraEnv []string, args ...string) result {
	s.t.Helper()
	cmd := exec.Command(skaldBin, args...)
	cmd.Dir = dir
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SKALD_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(append(env, "SKALD_HOME="+s.home), extraEnv...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			s.t.Fatalf("running skald %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return result{out.String(), errb.String(), code}
}

func (s *sandbox) run(args ...string) result { return s.runIn(s.repo, nil, args...) }

// ok runs skald and fails the test unless it exits 0.
func (s *sandbox) ok(args ...string) result {
	s.t.Helper()
	r := s.run(args...)
	if r.code != 0 {
		s.t.Fatalf("skald %v exited %d\nstdout: %s\nstderr: %s", args, r.code, r.stdout, r.stderr)
	}
	return r
}

// fail runs skald and fails the test unless it exits with code want. It also
// checks that nothing was written to stdout on failure.
func (s *sandbox) fail(want int, args ...string) result {
	s.t.Helper()
	r := s.run(args...)
	if r.code != want {
		s.t.Fatalf("skald %v exited %d, want %d\nstdout: %s\nstderr: %s", args, r.code, want, r.stdout, r.stderr)
	}
	if r.stdout != "" {
		s.t.Errorf("skald %v wrote to stdout on failure: %q", args, r.stdout)
	}
	return r
}

func decode(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, s)
	}
}

type taskOut struct {
	ID      string `json:"id"`
	Feature string `json:"feature"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Version int64  `json:"version"`
}

func (s *sandbox) task(id string) taskOut {
	s.t.Helper()
	var d taskOut
	decode(s.t, s.ok("task", "show", id, "--json").stdout, &d)
	return d
}

func contains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(strings.ToLower(got), strings.ToLower(want)) {
		t.Errorf("output does not contain %q:\n%s", want, got)
	}
}

func TestVersion(t *testing.T) {
	s := newSandbox(t)
	r := s.ok("--version")
	if strings.TrimSpace(r.stdout) == "" || r.stderr != "" {
		t.Errorf("version: stdout=%q stderr=%q", r.stdout, r.stderr)
	}
}

func TestInitIsIdempotent(t *testing.T) {
	s := newSandbox(t)
	var first, second struct {
		Database      string `json:"database"`
		SchemaVersion int    `json:"schema_version"`
	}
	decode(t, s.ok("init", "--json").stdout, &first)
	if first.SchemaVersion < 1 || !strings.HasPrefix(first.Database, s.home) {
		t.Fatalf("unexpected init output: %+v", first)
	}
	s.ok("project", "create", "Keep Me")
	r := s.ok("init", "--json")
	decode(t, r.stdout, &second)
	if first != second {
		t.Errorf("init changed between runs: %+v vs %+v", first, second)
	}
	// Existing data survives a second init.
	contains(t, s.ok("project", "list").stdout, "keep-me")

	info, err := os.Stat(s.home)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Errorf("home mode = %v, want 0700", info.Mode().Perm())
	}
}

func TestPlanToDoneSession(t *testing.T) {
	s := newSandbox(t)
	s.ok("init")

	var p struct {
		ID       string `json:"id"`
		Slug     string `json:"slug"`
		RepoPath string `json:"repo_path"`
	}
	decode(t, s.ok("project", "create", "School Management", "--json").stdout, &p)
	if p.ID != "P-1" || p.Slug != "school-management" || p.RepoPath == "" {
		t.Fatalf("project = %+v", p)
	}

	plan, _ := filepath.Abs("testdata/plan.json")
	var imp struct {
		TasksCreated      int `json:"tasks_created"`
		FeaturesCreated   int `json:"features_created"`
		DependenciesAdded int `json:"dependencies_added"`
		Tasks             map[string]struct {
			ID      string `json:"id"`
			Created bool   `json:"created"`
		} `json:"tasks"`
	}
	decode(t, s.ok("plan", "import", plan, "--json").stdout, &imp)
	if imp.TasksCreated != 4 || imp.FeaturesCreated != 2 || imp.DependenciesAdded != 4 {
		t.Fatalf("import = %+v", imp)
	}
	idOf := func(ref string) string { return imp.Tasks[ref].ID }

	// Only A can start.
	var next struct {
		Tasks []taskOut `json:"tasks"`
		Total int       `json:"total"`
	}
	decode(t, s.ok("next", "--json").stdout, &next)
	if next.Total != 1 || next.Tasks[0].ID != idOf("a") {
		t.Fatalf("next = %+v", next)
	}
	// B is gated by A, with an actionable message.
	r := s.fail(1, "task", "start", idOf("b"))
	contains(t, r.stderr, idOf("a"))

	// Work every task in dependency order, recording evidence first.
	for _, ref := range []string{"a", "b", "c", "d"} {
		id := idOf(ref)
		s.ok("task", "start", id)
		s.ok("task", "note", id, "working", "on", ref)
		s.ok("evidence", "add", id, "--kind", "commit", "--ref", "sha-"+ref, "--summary", "did "+ref)
		s.ok("task", "done", id)
		if got := s.task(id); got.Status != "done" {
			t.Fatalf("%s status = %s", id, got.Status)
		}
	}

	var st struct {
		Progress struct {
			Fraction float64 `json:"fraction"`
		} `json:"progress"`
		Counts struct {
			Done  int `json:"done"`
			Total int `json:"total"`
		} `json:"counts"`
		Features []struct {
			Status string `json:"status"`
		} `json:"features"`
		ReadyCount int `json:"ready_count"`
	}
	decode(t, s.ok("status", "--json").stdout, &st)
	if st.Progress.Fraction != 1 || st.Counts.Done != 4 || st.ReadyCount != 0 {
		t.Fatalf("status = %+v", st)
	}
	for _, f := range st.Features {
		if f.Status != "done" {
			t.Errorf("feature status = %s", f.Status)
		}
	}
	contains(t, s.ok("status").stdout, "100%")
	contains(t, s.ok("next").stdout, "nothing is ready")

	// The log holds the whole history in order, with the human actor.
	var lg struct {
		Events []struct {
			ID    int64  `json:"id"`
			Actor string `json:"actor"`
			Type  string `json:"type"`
		} `json:"events"`
		Cursor int64 `json:"cursor"`
	}
	decode(t, s.ok("log", "--since", "0", "--limit", "1000", "--json").stdout, &lg)
	if len(lg.Events) < 20 {
		t.Fatalf("only %d events", len(lg.Events))
	}
	var prev int64
	for _, e := range lg.Events {
		if e.ID <= prev {
			t.Fatalf("events not increasing: %d after %d", e.ID, prev)
		}
		prev = e.ID
		if e.Actor != "human" {
			t.Errorf("actor = %q", e.Actor)
		}
	}
	if lg.Cursor != prev {
		t.Errorf("cursor = %d, want %d", lg.Cursor, prev)
	}
	// Paging: since the middle returns only the rest.
	mid := lg.Events[len(lg.Events)/2].ID
	var rest struct {
		Events []struct {
			ID int64 `json:"id"`
		} `json:"events"`
	}
	decode(t, s.ok("log", "--since", fmt.Sprint(mid), "--limit", "1000", "--json").stdout, &rest)
	if len(rest.Events) == 0 || rest.Events[0].ID != mid+1 {
		t.Errorf("events after %d start at %v", mid, rest.Events)
	}
	// Recent events: the last three, oldest first.
	var recent struct {
		Events []struct {
			ID int64 `json:"id"`
		} `json:"events"`
	}
	decode(t, s.ok("log", "--limit", "3", "--json").stdout, &recent)
	if len(recent.Events) != 3 || recent.Events[2].ID != prev {
		t.Errorf("recent = %v, want last id %d", recent.Events, prev)
	}
}

func TestPlanImportRerunIsIdempotent(t *testing.T) {
	s := newSandbox(t)
	s.ok("project", "create", "Demo")
	plan, _ := filepath.Abs("testdata/plan.json")
	s.ok("plan", "import", plan)
	before := s.ok("log", "--all-projects", "--since", "0", "--limit", "1000", "--json").stdout

	var res struct {
		TasksCreated      int `json:"tasks_created"`
		TasksReused       int `json:"tasks_reused"`
		FeaturesCreated   int `json:"features_created"`
		DependenciesAdded int `json:"dependencies_added"`
	}
	decode(t, s.ok("plan", "import", plan, "--json").stdout, &res)
	if res.TasksCreated != 0 || res.FeaturesCreated != 0 || res.DependenciesAdded != 0 || res.TasksReused != 4 {
		t.Errorf("rerun = %+v", res)
	}
	after := s.ok("log", "--all-projects", "--since", "0", "--limit", "1000", "--json").stdout
	if before != after {
		t.Error("re-running the plan wrote new events")
	}
	// Also from stdin.
	data, _ := os.ReadFile(plan)
	cmd := exec.Command(skaldBin, "plan", "import", "-", "--json")
	cmd.Dir = s.repo
	cmd.Env = append(os.Environ(), "SKALD_HOME="+s.home)
	cmd.Stdin = bytes.NewReader(data)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("stdin import: %v\n%s", err, out)
	}
}

func TestDocsExamplePlanImports(t *testing.T) {
	s := newSandbox(t)
	s.ok("project", "create", "Example")
	example, _ := filepath.Abs("../../docs/plan-example.json")
	s.ok("plan", "import", example)
}

func TestPlanImportErrors(t *testing.T) {
	s := newSandbox(t)
	s.ok("project", "create", "Demo")

	cyc, _ := filepath.Abs("testdata/plan_cycle.json")
	r := s.fail(1, "plan", "import", cyc)
	contains(t, r.stderr, "cycle")
	if got := s.ok("task", "list", "--json").stdout; strings.Contains(got, `"id"`) {
		t.Errorf("cyclic plan left tasks behind: %s", got)
	}

	unk, _ := filepath.Abs("testdata/plan_unknown_field.json")
	r = s.fail(2, "plan", "import", unk)
	contains(t, r.stderr, "taks")

	s.fail(1, "plan", "import", filepath.Join(s.home, "missing.json"))
}

func TestErrorMessagesAndExitCodes(t *testing.T) {
	s := newSandbox(t)
	s.ok("project", "create", "Demo")
	s.ok("feature", "add", "Core")
	s.ok("task", "add", "One", "--feature", "F-1")
	s.ok("task", "add", "Two", "--feature", "F-1")

	// Dependency cycle.
	s.ok("task", "dep", "add", "T-2", "T-1")
	r := s.fail(1, "task", "dep", "add", "T-1", "T-2")
	contains(t, r.stderr, "cycle")
	contains(t, r.stderr, "T-1")

	// Evidence required, then forced by a human, and recorded.
	s.ok("task", "start", "T-1")
	r = s.fail(1, "task", "done", "T-1")
	contains(t, r.stderr, "evidence")
	s.ok("task", "done", "T-1", "--force")
	contains(t, s.ok("log", "--limit", "5").stdout, "forced")

	// Invalid transition: done -> blocked.
	r = s.fail(1, "task", "block", "T-1", "--reason", "nope")
	contains(t, r.stderr, "done")

	// Reopen works from done.
	s.ok("task", "reopen", "T-1")
	if got := s.task("T-1"); got.Status != "todo" {
		t.Errorf("status = %s", got.Status)
	}

	// Version conflict.
	r = s.fail(1, "task", "start", "T-1", "--version", "1")
	contains(t, r.stderr, "version")

	// Not found, in another project's namespace too.
	r = s.fail(1, "task", "show", "T-99")
	contains(t, r.stderr, "T-99")
	r = s.fail(1, "project", "show", "ghost")
	contains(t, r.stderr, "ghost")
}

func TestAgentActorCannotForce(t *testing.T) {
	s := newSandbox(t)
	s.ok("project", "create", "Demo")
	s.ok("feature", "add", "Core")
	s.ok("task", "add", "One", "--feature", "F-1")
	agent := []string{"SKALD_ACTOR=claude:test/1"}
	if r := s.runIn(s.repo, agent, "task", "start", "T-1"); r.code != 0 {
		t.Fatalf("start: %s", r.stderr)
	}
	r := s.runIn(s.repo, agent, "task", "done", "T-1", "--force")
	if r.code != 1 || !strings.Contains(strings.ToLower(r.stderr), "evidence") {
		t.Errorf("agent force: code=%d stderr=%s", r.code, r.stderr)
	}
	r = s.runIn(s.repo, []string{"SKALD_ACTOR=bad actor"}, "next")
	if r.code != 2 {
		t.Errorf("bad actor exit = %d", r.code)
	}
	// The agent's name is recorded.
	contains(t, s.ok("log", "--limit", "3").stdout, "claude:test/1")
}

func TestAmbiguousProject(t *testing.T) {
	s := newSandbox(t)
	s.ok("project", "create", "Alpha", "--no-repo")
	s.ok("project", "create", "Beta", "--no-repo")

	r := s.fail(1, "status")
	contains(t, r.stderr, "alpha")
	contains(t, r.stderr, "beta")

	// Resolved by flag, by environment, and by project argument.
	contains(t, s.ok("status", "-p", "beta").stdout, "Beta")
	r2 := s.runIn(s.repo, []string{"SKALD_PROJECT=alpha"}, "status")
	if r2.code != 0 || !strings.Contains(r2.stdout, "Alpha") {
		t.Errorf("env resolution: %d %s %s", r2.code, r2.stdout, r2.stderr)
	}
	contains(t, s.ok("project", "show", "alpha").stdout, "Alpha")
}

func TestProjectResolvesFromWorkingDirectory(t *testing.T) {
	s := newSandbox(t)
	s.ok("project", "create", "Linked")
	s.ok("project", "create", "Unlinked", "--no-repo")
	sub := filepath.Join(s.repo, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	r := s.runIn(sub, nil, "status")
	if r.code != 0 || !strings.Contains(r.stdout, "Linked") {
		t.Fatalf("walk-up resolution: %d %s %s", r.code, r.stdout, r.stderr)
	}
	// A second project cannot claim the same repository.
	r = s.fail(1, "project", "create", "Again")
	contains(t, r.stderr, "linked")
	// Archiving releases it.
	s.ok("project", "archive", "linked")
	s.ok("project", "create", "Again")
}

func TestUsageErrors(t *testing.T) {
	s := newSandbox(t)
	s.ok("project", "create", "Demo")
	s.ok("feature", "add", "Core")
	cases := [][]string{
		{"frobnicate"},
		{"task", "bogus"},
		{"task", "start"},
		{"task", "start", "T-1", "T-2"},
		{"task", "start", "F-1"},
		{"task", "start", "banana"},
		{"task", "add", "No feature"},
		{"task", "add", "Bad priority", "--feature", "F-1", "--priority", "9"},
		{"task", "list", "--status", "weird"},
		{"task", "block", "T-1"},
		{"evidence", "add", "T-1"},
		{"evidence", "add", "T-1", "--kind", "nope", "--ref", "x"},
		{"--no-such-flag"},
		{"project", "create"},
		{"project", "create", "X", "--repo", "/tmp", "--no-repo"},
	}
	for _, args := range cases {
		r := s.fail(2, args...)
		if strings.TrimSpace(r.stderr) == "" {
			t.Errorf("skald %v: empty error message", args)
		}
	}
}

func TestTaskLifecycleCommands(t *testing.T) {
	s := newSandbox(t)
	s.ok("project", "create", "Demo")
	s.ok("feature", "add", "Core", "--weight", "2")
	s.ok("task", "add", "Alpha", "--feature", "f-1", "--priority", "p0", "--criteria", "works")
	s.ok("task", "add", "Beta", "--feature", "1", "--key", "beta-key")
	// The same key returns the existing task.
	var again taskOut
	decode(t, s.ok("task", "add", "Beta again", "--feature", "F-1", "--key", "beta-key", "--json").stdout, &again)
	if again.ID != "T-2" {
		t.Errorf("idempotent add returned %s", again.ID)
	}

	// Priority orders next/list: T-1 is urgent.
	var list struct {
		Tasks []taskOut `json:"tasks"`
	}
	decode(t, s.ok("task", "list", "--json").stdout, &list)
	if len(list.Tasks) != 2 || list.Tasks[0].ID != "T-1" {
		t.Errorf("list order = %+v", list.Tasks)
	}

	// Block and unblock: a started task returns to in_progress.
	s.ok("task", "start", "T-1")
	s.ok("task", "block", "T-1", "--reason", "waiting for keys")
	contains(t, s.ok("status").stdout, "waiting for keys")
	s.ok("task", "unblock", "T-1")
	if got := s.task("T-1"); got.Status != "in_progress" {
		t.Errorf("after unblock: %s", got.Status)
	}
	// A never-started task returns to todo.
	s.ok("task", "block", "T-2", "--reason", "later")
	s.ok("task", "unblock", "T-2")
	if got := s.task("T-2"); got.Status != "todo" {
		t.Errorf("after unblock: %s", got.Status)
	}

	// Dependencies: remove and cancel.
	s.ok("task", "dep", "add", "T-2", "T-1")
	contains(t, s.ok("task", "dep", "add", "T-2", "T-1").stdout, "already")
	contains(t, s.ok("task", "show", "T-2").stdout, "waiting on  T-1")
	s.ok("task", "dep", "remove", "T-2", "T-1")
	s.ok("task", "cancel", "T-2")
	var counts struct {
		Counts struct {
			Cancelled int `json:"cancelled"`
		} `json:"counts"`
	}
	decode(t, s.ok("status", "--json").stdout, &counts)
	if counts.Counts.Cancelled != 1 {
		t.Errorf("cancelled = %d", counts.Counts.Cancelled)
	}

	// Filters.
	decode(t, s.ok("task", "list", "--status", "in-progress", "--json").stdout, &list)
	if len(list.Tasks) != 1 || list.Tasks[0].ID != "T-1" {
		t.Errorf("status filter = %+v", list.Tasks)
	}
	contains(t, s.ok("feature", "list").stdout, "Core")
	contains(t, s.ok("project", "show").stdout, "Demo")
	contains(t, s.ok("project", "list").stdout, "demo")
}

func TestBackupIsConsistent(t *testing.T) {
	s := newSandbox(t)
	s.ok("project", "create", "Demo")
	plan, _ := filepath.Abs("testdata/plan.json")
	s.ok("plan", "import", plan)

	var b struct {
		Path  string `json:"path"`
		Bytes int64  `json:"bytes"`
	}
	decode(t, s.ok("backup", "--json").stdout, &b)
	if !strings.HasPrefix(b.Path, filepath.Join(s.home, "backups")) || !strings.HasPrefix(filepath.Base(b.Path), "skald-") || b.Bytes == 0 {
		t.Fatalf("backup = %+v", b)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(b.Path); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("backup mode: %v %v", info, err)
		}
	}
	// A second backup in the same second gets its own file.
	var b2 struct {
		Path string `json:"path"`
	}
	decode(t, s.ok("backup", "--json").stdout, &b2)
	if b2.Path == b.Path {
		t.Error("second backup overwrote the first")
	}
	s.fail(2, "backup", "--out", b.Path)

	db, err := store.Open(context.Background(), b.Path)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer db.Close()
	var check string
	if err := db.Reader().QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check = %q, %v", check, err)
	}
	var tasks int
	if err := db.Reader().QueryRow("SELECT count(*) FROM tasks").Scan(&tasks); err != nil || tasks != 4 {
		t.Errorf("backup has %d tasks, want 4 (%v)", tasks, err)
	}
}

func TestProjectLink(t *testing.T) {
	s := newSandbox(t)
	s.ok("project", "create", "Unlinked", "--no-repo")
	s.ok("project", "create", "Other", "--no-repo")
	// Without a link, a project is not resolved from the directory.
	s.fail(1, "task", "list")
	r := s.ok("project", "link", "unlinked")
	contains(t, r.stdout, "linked")
	// Two projects exist but only one is linked: cwd resolution picks it.
	r = s.ok("status")
	contains(t, r.stdout, "Unlinked")
	// Another project cannot take the same directory.
	r = s.fail(1, "project", "link", "other")
	contains(t, r.stderr, "already linked")
	r = s.fail(2, "project", "link", "other", "--unlink", "--repo", s.repo)
	contains(t, r.stderr, "cannot be used together")
	// Unlink, then the other project can take it.
	r = s.ok("project", "link", "unlinked", "--unlink")
	contains(t, r.stdout, "unlinked")
	s.ok("project", "link", "other")
}
