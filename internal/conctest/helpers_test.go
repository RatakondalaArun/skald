package conctest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// lockedBuffer is a bytes.Buffer that can be written by exec's copying
// goroutine while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// harness is the shared sandbox: one binary, one SKALD_HOME, one working
// directory that is not a repository (so projects are chosen with
// SKALD_PROJECT).
type harness struct {
	t    *testing.T
	bin  string
	home string
	dir  string

	mu      sync.Mutex
	stderrs map[string][]string // process label -> stderr text per run
	expect  map[string]int64    // event type -> events the successful writes must have produced
	failed  []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{
		t:       t,
		bin:     filepath.Join(root, "skald"),
		home:    filepath.Join(root, "home"),
		dir:     filepath.Join(root, "work"),
		stderrs: map[string][]string{},
		expect:  map[string]int64{},
	}
	if err := os.MkdirAll(h.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	h.build(moduleRoot(t))
	return h
}

// moduleRoot finds the directory holding go.mod, starting at the test's
// working directory (the package directory).
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

func (h *harness) build(root string) {
	h.t.Helper()
	args := []string{"build"}
	if os.Getenv("SKALD_CONC_RACE") == "1" {
		args = append(args, "-race")
	}
	args = append(args, "-o", h.bin, "./cmd/skald")
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("building skald: %v\n%s", err, out)
	}
}

// env returns the environment for a skald process. SKALD_* variables from the
// outer environment are dropped so the test never touches a real database.
func (h *harness) env(actor string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SKALD_") {
			env = append(env, kv)
		}
	}
	env = append(env, "SKALD_HOME="+h.home, "SKALD_PROJECT=conc")
	if actor != "" {
		env = append(env, "SKALD_ACTOR="+actor)
	}
	return env
}

// fail records a problem found in a worker goroutine.
func (h *harness) fail(format string, a ...any) {
	h.mu.Lock()
	h.failed = append(h.failed, fmt.Sprintf(format, a...))
	h.mu.Unlock()
}

// want records that a successful write must have appended n events of typ.
func (h *harness) want(typ string, n int64) {
	h.mu.Lock()
	h.expect[typ] += n
	h.mu.Unlock()
}

func (h *harness) recordStderr(label, s string) {
	h.mu.Lock()
	h.stderrs[label] = append(h.stderrs[label], s)
	h.mu.Unlock()
}

// run executes one short-lived skald command and returns stdout. A non-zero
// exit is reported through the error and the harness failure list.
func (h *harness) run(actor string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, args...)
	cmd.Dir = h.dir
	cmd.Env = h.env(actor)
	var stdout bytes.Buffer
	stderr := &lockedBuffer{}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	h.recordStderr("cli:"+actor, stderr.String())
	if err != nil {
		err = fmt.Errorf("skald %s: %v\nstderr: %s", strings.Join(args, " "), err, stderr.String())
		h.fail("%v", err)
		return "", err
	}
	return stdout.String(), nil
}

// runJSON runs a command with --json and decodes stdout into v.
func (h *harness) runJSON(actor string, v any, args ...string) error {
	out, err := h.run(actor, append(args, "--json")...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(out), v); err != nil {
		err = fmt.Errorf("skald %s: bad JSON %q: %v", strings.Join(args, " "), out, err)
		h.fail("%v", err)
		return err
	}
	return nil
}

// proc is a long-running skald process.
type proc struct {
	label  string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr *lockedBuffer
	done   chan struct{}
	exit   error
}

// start launches a long-running process. stdin is only connected when
// withStdin is true.
func (h *harness) start(label, actor string, withStdin bool, args ...string) *proc {
	h.t.Helper()
	cmd := exec.Command(h.bin, args...)
	cmd.Dir = h.dir
	cmd.Env = h.env(actor)
	p := &proc{label: label, cmd: cmd, stderr: &lockedBuffer{}, done: make(chan struct{})}
	cmd.Stderr = p.stderr
	var err error
	if withStdin {
		if p.stdin, err = cmd.StdinPipe(); err != nil {
			h.t.Fatal(err)
		}
	}
	// An io.Pipe (not StdoutPipe) so Wait can run at any time without
	// truncating what the reader has not consumed yet.
	pr, pw := io.Pipe()
	p.stdout = pr
	cmd.Stdout = pw
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("starting %s: %v", label, err)
	}
	go func() {
		p.exit = cmd.Wait()
		_ = pw.Close()
		close(p.done)
	}()
	h.t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

// stop sends sig and waits for a clean exit.
func (h *harness) stop(p *proc, sig syscall.Signal, timeout time.Duration) {
	h.t.Helper()
	select {
	case <-p.done:
	default:
		_ = p.cmd.Process.Signal(sig)
	}
	h.finish(p, timeout)
}

// finish waits for p to exit on its own, then records its stderr and checks
// for exit status 0.
func (h *harness) finish(p *proc, timeout time.Duration) {
	h.t.Helper()
	select {
	case <-p.done:
	case <-time.After(timeout):
		_ = p.cmd.Process.Kill()
		<-p.done
		h.fail("%s did not exit within %v\nstderr: %s", p.label, timeout, p.stderr.String())
		return
	}
	h.recordStderr(p.label, p.stderr.String())
	if p.exit != nil {
		h.fail("%s exited abnormally: %v\nstderr: %s", p.label, p.exit, p.stderr.String())
	}
}

// waitStderr waits until the process has printed a line matching re and
// returns the first submatch.
func (p *proc) waitStderr(re *regexp.Regexp, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m := re.FindStringSubmatch(p.stderr.String()); m != nil {
			return m[1], nil
		}
		select {
		case <-p.done:
			return "", fmt.Errorf("%s exited before printing %v\nstderr: %s", p.label, re, p.stderr.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	return "", fmt.Errorf("%s: no line matching %v within %v\nstderr: %s", p.label, re, timeout, p.stderr.String())
}

// feed collects the event ids a consumer has seen, in arrival order.
type feed struct {
	mu  sync.Mutex
	ids []int64
	bad []string
}

func (f *feed) add(id int64) {
	f.mu.Lock()
	f.ids = append(f.ids, id)
	f.mu.Unlock()
}

func (f *feed) problem(s string) {
	f.mu.Lock()
	f.bad = append(f.bad, s)
	f.mu.Unlock()
}

func (f *feed) last() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ids) == 0 {
		return 0
	}
	return f.ids[len(f.ids)-1]
}

func (f *feed) snapshot() ([]int64, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.ids...), append([]string(nil), f.bad...)
}

// readJSONLines feeds every JSON object on r (one per line) with an "id"
// field into f. It returns when r ends.
func readJSONLines(r io.Reader, f *feed) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil || ev.ID == 0 {
			f.problem(fmt.Sprintf("not an event line: %q", sc.Text()))
			continue
		}
		f.add(ev.ID)
	}
}
