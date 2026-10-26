package cli_test

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestServeRefusesNonLoopbackAddress(t *testing.T) {
	s := newSandbox(t)
	for _, addr := range []string{"0.0.0.0:7331", ":7331", "192.168.1.20:7331"} {
		r := s.fail(2, "serve", "--addr", addr)
		contains(t, r.stderr, "--insecure-listen")
		contains(t, r.stderr, "loopback")
	}
	r := s.fail(2, "serve", "--addr", "not-an-address")
	contains(t, r.stderr, "host:port")
}

func TestServeReportsPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	s := newSandbox(t)
	r := s.fail(1, "serve", "--addr", ln.Addr().String())
	contains(t, r.stderr, "cannot listen")
	contains(t, r.stderr, "--addr")
}

var servingLine = regexp.MustCompile(`skald dashboard: (http://\S+)`)

func TestServeRunsAndStopsOnSignal(t *testing.T) {
	s := newSandbox(t)
	s.ok("init")
	s.ok("project", "create", "Serve Demo")
	example, _ := filepath.Abs("../../docs/plan-example.json")
	s.ok("plan", "import", example)

	cmd := exec.Command(skaldBin, "serve", "--addr", "127.0.0.1:0")
	cmd.Dir = s.repo
	cmd.Env = append(os.Environ(), "SKALD_HOME="+s.home)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	lines := make(chan string, 16)
	var errLog strings.Builder
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			errLog.WriteString(sc.Text() + "\n")
			lines <- sc.Text()
		}
		close(lines)
	}()
	var base string
	deadline := time.After(10 * time.Second)
	for base == "" {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("serve exited before announcing its address: %s", errLog.String())
			}
			if m := servingLine.FindStringSubmatch(l); m != nil {
				base = m[1]
			}
		case <-deadline:
			t.Fatal("serve did not announce its address")
		}
	}

	res, err := http.Get(base + "/api/projects")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(body), `"slug":"serve-demo"`) {
		t.Fatalf("GET /api/projects = %d %s", res.StatusCode, body)
	}
	req, _ := http.NewRequest("GET", base+"/api/projects", nil)
	req.Host = "evil.example"
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 403 {
		t.Fatalf("forged Host = %v %v", res, err)
	}
	if res, err := http.Get(base + "/"); err != nil || res.StatusCode != 200 {
		t.Fatalf("index = %v %v", res, err)
	}

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("serve exited with %v after SIGINT\n%s", err, errLog.String())
		}
	case <-time.After(6 * time.Second):
		t.Fatal("serve did not stop after SIGINT")
	}
	if stdout.Len() != 0 {
		t.Errorf("serve wrote to stdout: %q", stdout.String())
	}
}

func TestServeBackgroundStartStatusStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("background mode is not supported on Windows")
	}
	s := newSandbox(t)
	s.ok("init")
	s.ok("project", "create", "Serve Demo")

	r := s.ok("serve", "--background", "--addr", "127.0.0.1:0", "--json")
	var st struct {
		Running bool   `json:"running"`
		PID     int    `json:"pid"`
		URL     string `json:"url"`
		Log     string `json:"log"`
	}
	decode(t, r.stdout, &st)
	t.Cleanup(func() { s.run("serve", "stop") })
	if !st.Running || st.PID <= 0 || !strings.HasPrefix(st.URL, "http://127.0.0.1:") || st.Log != filepath.Join(s.home, "serve.log") {
		t.Fatalf("start = %+v", st)
	}
	res, err := http.Get(st.URL + "/api/projects")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(body), `"slug":"serve-demo"`) {
		t.Fatalf("GET /api/projects = %d %s", res.StatusCode, body)
	}

	r = s.ok("serve", "status")
	contains(t, r.stdout, st.URL)
	contains(t, r.stdout, strconv.Itoa(st.PID))

	r = s.fail(1, "serve", "--background", "--addr", "127.0.0.1:0")
	contains(t, r.stderr, "already running")
	contains(t, r.stderr, "serve stop")

	r = s.ok("serve", "stop")
	contains(t, r.stdout, "stopped")
	if _, err := http.Get(st.URL + "/api/projects"); err == nil {
		t.Fatal("dashboard still answers after stop")
	}
	if _, err := os.Stat(filepath.Join(s.home, "serve.json")); !os.IsNotExist(err) {
		t.Fatalf("state file left after stop: %v", err)
	}
	log, _ := os.ReadFile(st.Log)
	contains(t, string(log), "skald dashboard stopped")

	r = s.run("serve", "status")
	if r.code != 1 {
		t.Fatalf("status after stop exited %d", r.code)
	}
	contains(t, r.stdout, "no background dashboard")
	r = s.ok("serve", "stop")
	contains(t, r.stdout, "no background dashboard")
}

func TestServeBackgroundReportsStartupFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("background mode is not supported on Windows")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	s := newSandbox(t)
	r := s.fail(1, "serve", "--background", "--addr", ln.Addr().String())
	contains(t, r.stderr, "exited before it was ready")
	contains(t, r.stderr, "cannot listen")
	if _, err := os.Stat(filepath.Join(s.home, "serve.json")); !os.IsNotExist(err) {
		t.Fatalf("state file left after a failed start: %v", err)
	}
}

func TestServeBackgroundStillChecksAddress(t *testing.T) {
	s := newSandbox(t)
	r := s.fail(2, "serve", "--background", "--addr", "0.0.0.0:7331")
	contains(t, r.stderr, "--insecure-listen")
}
