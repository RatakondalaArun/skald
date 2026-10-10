package cli_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// proc is a long-running skald process whose stdout is read line by line.
type proc struct {
	t      *testing.T
	cmd    *exec.Cmd
	lines  chan string
	stderr *bytes.Buffer
	done   chan struct{}
}

func (s *sandbox) start(args ...string) *proc {
	s.t.Helper()
	cmd := exec.Command(skaldBin, args...)
	cmd.Dir = s.repo
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SKALD_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "SKALD_HOME="+s.home)
	p := &proc{t: s.t, cmd: cmd, lines: make(chan string, 1024), stderr: &bytes.Buffer{}, done: make(chan struct{})}
	cmd.Stderr = p.stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		s.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	go func() {
		defer close(p.lines)
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
	}()
	s.t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	return p
}

// next returns the next stdout line or fails after timeout.
func (p *proc) next(timeout time.Duration) string {
	p.t.Helper()
	select {
	case l, ok := <-p.lines:
		if !ok {
			p.t.Fatalf("process ended while waiting for output\nstderr: %s", p.stderr.String())
		}
		return l
	case <-time.After(timeout):
		p.t.Fatalf("no output within %v\nstderr: %s", timeout, p.stderr.String())
	}
	return ""
}

// waitFor reads lines until one satisfies match, returning every line read.
func (p *proc) waitFor(timeout time.Duration, match func(string) bool) []string {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	var seen []string
	for {
		l := p.next(time.Until(deadline))
		seen = append(seen, l)
		if match(l) {
			return seen
		}
	}
}

// interrupt sends SIGINT and returns the exit code.
func (p *proc) interrupt() int {
	p.t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGINT); err != nil {
		p.t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- p.cmd.Wait() }()
	select {
	case err := <-waited:
		close(p.done)
		if err == nil {
			return 0
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		p.t.Fatal(err)
	case <-time.After(5 * time.Second):
		p.cmd.Process.Kill()
		p.t.Fatal("process did not exit after SIGINT")
	}
	return -1
}

// planSandbox has a project with the testdata plan imported and returns the id
// of a task that is ready to start.
func planSandbox(t *testing.T) (*sandbox, string) {
	t.Helper()
	s := newSandbox(t)
	s.ok("init")
	s.ok("project", "create", "Follow Demo")
	plan, _ := filepath.Abs("testdata/plan.json")
	s.ok("plan", "import", plan)
	var next struct {
		Tasks []taskOut `json:"tasks"`
	}
	decode(t, s.ok("next", "--json").stdout, &next)
	if len(next.Tasks) == 0 {
		t.Fatal("no ready task in the test plan")
	}
	return s, next.Tasks[0].ID
}

func TestLogFollowStreamsEventsFromAnotherProcess(t *testing.T) {
	s, task := planSandbox(t)
	p := s.start("log", "--follow", "--limit", "5")

	p.next(5 * time.Second) // the backlog is printing, so the follower is up
	start := time.Now()
	s.ok("task", "note", task, "marker-one")
	s.ok("task", "note", task, "marker-two")
	seen := p.waitFor(5*time.Second, func(l string) bool { return strings.Contains(l, "marker-one") })
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("event took %v to arrive", d)
	}
	rest := p.waitFor(5*time.Second, func(l string) bool { return strings.Contains(l, "marker-two") })
	if len(rest) != 1 {
		t.Fatalf("lines between the two notes: %v", rest)
	}

	// Ids in the stream only go up.
	prev := int64(0)
	for _, l := range append(seen, rest...) {
		id, err := strconv.ParseInt(strings.Fields(l)[0], 10, 64)
		if err != nil {
			t.Fatalf("line does not start with an event id: %q", l)
		}
		if id <= prev {
			t.Fatalf("event id %d after %d", id, prev)
		}
		prev = id
	}

	if code := p.interrupt(); code != 0 {
		t.Fatalf("exit code %d after SIGINT\nstderr: %s", code, p.stderr.String())
	}
	if p.stderr.Len() != 0 {
		t.Fatalf("stderr: %s", p.stderr.String())
	}
}

func TestLogFollowJSONIsOneObjectPerLine(t *testing.T) {
	s, task := planSandbox(t)
	p := s.start("log", "--follow", "--json", "--limit", "1")

	check := func(l string) map[string]any {
		var ev map[string]any
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatalf("line is not a JSON object: %q", l)
		}
		return ev
	}
	check(p.next(5 * time.Second)) // backlog
	s.ok("task", "note", task, "json-marker")
	lines := p.waitFor(5*time.Second, func(l string) bool { return strings.Contains(l, "json-marker") })
	ev := check(lines[len(lines)-1])
	if ev["type"] != "task.note" {
		t.Fatalf("event = %v", ev)
	}
	if code := p.interrupt(); code != 0 {
		t.Fatalf("exit code %d", code)
	}
}

func TestLogFollowSinceReplaysFromCursor(t *testing.T) {
	s, _ := planSandbox(t)
	p := s.start("log", "--follow", "--since", "0", "--json")
	for want := 1; want <= 3; want++ {
		var ev struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal([]byte(p.next(5*time.Second)), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.ID != int64(want) {
			t.Fatalf("replayed event id %d, want %d", ev.ID, want)
		}
	}
	if code := p.interrupt(); code != 0 {
		t.Fatalf("exit code %d", code)
	}
}

func TestStatusWatchPrintsNewSnapshotOnChange(t *testing.T) {
	s, task := planSandbox(t)
	p := s.start("status", "--watch")

	isHeader := func(l string) bool { return strings.HasPrefix(l, "P-1") }
	p.waitFor(5*time.Second, isHeader)
	// Drain the rest of the first snapshot, then change the project from
	// another process.
	time.Sleep(300 * time.Millisecond)
	s.ok("task", "start", task)

	// stdout is a pipe here, so the second snapshot follows a blank line.
	lines := p.waitFor(5*time.Second, isHeader)
	if len(lines) < 2 || lines[len(lines)-2] != "" {
		t.Fatalf("second snapshot is not preceded by a blank line: %q", lines)
	}
	var sawInProgress bool
	for _, l := range p.waitFor(5*time.Second, func(l string) bool { return strings.Contains(l, "in progress") }) {
		sawInProgress = sawInProgress || strings.Contains(l, "1 in progress")
	}
	if !sawInProgress {
		t.Fatal("second snapshot does not show the started task")
	}
	if code := p.interrupt(); code != 0 {
		t.Fatalf("exit code %d\nstderr: %s", code, p.stderr.String())
	}
	if strings.Contains(p.stderr.String(), "\x1b") {
		t.Fatal("escape sequences written without a terminal")
	}
}

func TestStatusWatchRejectsJSON(t *testing.T) {
	s, _ := planSandbox(t)
	r := s.fail(2, "status", "--watch", "--json")
	contains(t, r.stderr, "--watch")
}
