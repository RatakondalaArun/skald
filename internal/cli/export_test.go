package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// expSeed makes a project with a plan and some history in s.
func expSeed(t *testing.T, s *sandbox) {
	t.Helper()
	s.ok("project", "create", "Demo")
	plan, _ := filepath.Abs("testdata/plan.json")
	s.ok("plan", "import", plan)
	s.ok("task", "start", "T-1")
	s.ok("task", "note", "T-1", "halfway there")
	s.ok("evidence", "add", "T-1", "--kind", "commit", "--ref", "abc123")
	s.ok("task", "done", "T-1")
}

// expBlank drops the one field that differs between two exports.
func expBlank(t *testing.T, raw string) string {
	t.Helper()
	var m map[string]json.RawMessage
	decode(t, raw, &m)
	m["exported_at"] = json.RawMessage(`""`)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExportToStdout(t *testing.T) {
	s := newSandbox(t)
	expSeed(t, s)

	r := s.ok("export")
	if r.stderr != "" {
		t.Errorf("stderr = %q, want empty", r.stderr)
	}
	var x struct {
		Format        string `json:"format"`
		FormatVersion int    `json:"format_version"`
		SkaldVersion  string `json:"skald_version"`
		SchemaVersion int    `json:"schema_version"`
		Scope         string `json:"scope"`
		Projects      []struct {
			ID      int64
			ShortID string `json:"short_id"`
			Slug    string
		} `json:"projects"`
		Tasks []struct {
			ID      int64
			ShortID string `json:"short_id"`
			Status  string
		} `json:"tasks"`
		Artifacts []struct{ Ref string } `json:"artifacts"`
		Events    []struct {
			ID    int64
			Type  string
			Actor string
		} `json:"events"`
	}
	decode(t, r.stdout, &x)
	if x.Format != "skald-export" || x.FormatVersion != 1 || x.SkaldVersion != "dev" || x.SchemaVersion < 1 || x.Scope != "project:demo" {
		t.Fatalf("header = %+v", x)
	}
	if len(x.Projects) != 1 || x.Projects[0].ShortID != "P-1" || len(x.Tasks) != 4 || x.Tasks[0].ShortID != "T-1" || x.Tasks[0].Status != "done" {
		t.Fatalf("content = %+v", x)
	}
	if len(x.Artifacts) != 1 || x.Artifacts[0].Ref != "abc123" || len(x.Events) == 0 || x.Events[0].Actor != "human" {
		t.Fatalf("artifacts/events = %+v", x)
	}
	// --json is the global flag; it is accepted and changes nothing.
	if got := s.ok("export", "--json").stdout; expBlank(t, got) != expBlank(t, r.stdout) {
		t.Error("--json changed the output")
	}
}

func TestExportToFile(t *testing.T) {
	s := newSandbox(t)
	expSeed(t, s)
	out := filepath.Join(t.TempDir(), "nested", "demo.json")

	r := s.ok("export", "--out", out)
	if r.stdout != "" {
		t.Errorf("stdout = %q, want empty with --out", r.stdout)
	}
	contains(t, r.stderr, "exported project:demo")
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	decode(t, string(data), &m)
	if m["format"] != "skald-export" {
		t.Errorf("file = %v", m["format"])
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, %v; want 0600", info, err)
		}
	}
	// An existing file is never overwritten.
	s.fail(2, "export", "--out", out)
	if after, _ := os.ReadFile(out); string(after) != string(data) {
		t.Error("existing file was modified")
	}
}

func TestExportScopeFlags(t *testing.T) {
	s := newSandbox(t)
	expSeed(t, s)
	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if r := s.runIn(other, nil, "project", "create", "Second"); r.code != 0 {
		t.Fatalf("create second: %s", r.stderr)
	}

	// Two active projects and no hint: ambiguous, and it says which.
	r := s.runIn(t.TempDir(), nil, "export")
	if r.code != 1 || r.stdout != "" {
		t.Fatalf("ambiguous export: code %d stdout %q", r.code, r.stdout)
	}
	contains(t, r.stderr, "demo")
	contains(t, r.stderr, "second")

	var one, all struct {
		Scope    string `json:"scope"`
		Projects []any  `json:"projects"`
	}
	decode(t, s.ok("export", "-p", "second").stdout, &one)
	decode(t, s.ok("export", "--all").stdout, &all)
	if one.Scope != "project:second" || len(one.Projects) != 1 || all.Scope != "all" || len(all.Projects) != 2 {
		t.Fatalf("one = %+v, all = %+v", one, all)
	}
	s.fail(2, "export", "--all", "-p", "demo")
	s.fail(1, "export", "-p", "nope")
}

func TestImportRoundTrip(t *testing.T) {
	src := newSandbox(t)
	expSeed(t, src)
	for _, args := range [][]string{{"export"}, {"export", "--all"}} {
		file := filepath.Join(t.TempDir(), "export.json")
		first := src.ok(append(args, "--out", file)...)
		_ = first
		original, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}

		dst := newSandbox(t)
		r := dst.ok("import", file)
		contains(t, r.stdout, "imported 1 projects, 2 features, 4 tasks")

		again := filepath.Join(t.TempDir(), "again.json")
		dst.ok(append(args, "--out", again)...)
		second, _ := os.ReadFile(again)
		if expBlank(t, string(original)) != expBlank(t, string(second)) {
			t.Fatalf("%v: export after import differs from the original", args)
		}

		// The imported database works: status shows the same progress and the
		// task history is intact.
		if got, want := dst.ok("status", "--json").stdout, src.ok("status", "--json").stdout; got != want {
			t.Errorf("%v: status differs after import\n%s\n%s", args, got, want)
		}
		dst.ok("task", "start", "T-2")

		// A second import into the same database is refused and changes nothing.
		before := dst.ok("log", "--json", "--limit", "1000").stdout
		f := dst.fail(1, "import", file)
		contains(t, f.stderr, "fresh")
		if after := dst.ok("log", "--json", "--limit", "1000").stdout; after != before {
			t.Error("refused import changed the event log")
		}
	}
}

func TestImportErrors(t *testing.T) {
	s := newSandbox(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"format":"something-else","format_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := s.fail(1, "import", bad)
	contains(t, r.stderr, "skald-export")
	junk := filepath.Join(dir, "junk.json")
	if err := os.WriteFile(junk, []byte(strings.Repeat("x", 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	s.fail(1, "import", junk)
	s.fail(1, "import", filepath.Join(dir, "missing.json"))
	s.fail(2, "import")
	// None of the failed imports left anything behind.
	if got := s.ok("project", "list", "--json").stdout; strings.Contains(got, "slug") {
		t.Errorf("failed imports left projects behind: %s", got)
	}
}
