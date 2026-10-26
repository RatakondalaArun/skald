package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStateRoundTripAndClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.json")
	if _, err := Read(path); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Read(missing) = %v, want ErrNotRunning", err)
	}
	want := State{PID: os.Getpid(), URL: "http://127.0.0.1:1", Log: "x.log", Started: time.Now().UTC().Truncate(time.Second)}
	if err := Write(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Status(path)
	if err != nil || got.PID != want.PID || got.URL != want.URL || !got.Started.Equal(want.Started) {
		t.Fatalf("Status = %+v, %v", got, err)
	}
	Clear(path, want.PID+1)
	if _, err := Read(path); err != nil {
		t.Fatalf("Clear with another pid removed the record: %v", err)
	}
	Clear(path, want.PID)
	if _, err := Read(path); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Clear did not remove the record: %v", err)
	}
}

func TestStatusRemovesStaleRecord(t *testing.T) {
	if !supported {
		t.Skip("background mode is not supported here")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "serve.json")
	if err := Write(path, State{PID: cmd.Process.Pid}); err != nil {
		t.Fatal(err)
	}
	if _, err := Status(path); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Status(exited pid) = %v, want ErrNotRunning", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale record left behind: %v", err)
	}
}

func TestReadRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.json")
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil || errors.Is(err, ErrNotRunning) {
		t.Fatalf("Read(garbage) = %v", err)
	}
}

func TestStartReportsEarlyExitWithLog(t *testing.T) {
	if !supported {
		t.Skip("background mode is not supported here")
	}
	dir := t.TempDir()
	sp := Spec{Exe: "/bin/sh", Args: []string{"-c", "echo boom >&2; exit 3"}, Dir: dir,
		Log: filepath.Join(dir, "serve.log"), State: filepath.Join(dir, "serve.json"), Wait: 5 * time.Second}
	_, err := Start(sp)
	if err == nil {
		t.Fatal("Start succeeded for a process that exits")
	}
	if got := err.Error(); !contains(got, "exited before it was ready") || !contains(got, "boom") {
		t.Fatalf("error = %q", got)
	}
}

func TestStartTimesOutAndKills(t *testing.T) {
	if !supported {
		t.Skip("background mode is not supported here")
	}
	dir := t.TempDir()
	sp := Spec{Exe: "/bin/sh", Args: []string{"-c", "sleep 30"}, Dir: dir,
		Log: filepath.Join(dir, "serve.log"), State: filepath.Join(dir, "serve.json"), Wait: 200 * time.Millisecond}
	if _, err := Start(sp); err == nil || !contains(err.Error(), "not ready") {
		t.Fatalf("Start = %v, want a not-ready error", err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
