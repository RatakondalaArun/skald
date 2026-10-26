// Package daemon runs a skald command as a detached background process and
// tracks it with a small state file, so a later command can find and stop it.
// It knows nothing about what the process does; the dashboard uses it for
// `skald serve --background`.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// State is what a background process records about itself once it is ready.
type State struct {
	PID     int       `json:"pid"`
	URL     string    `json:"url"`
	Log     string    `json:"log"`
	Started time.Time `json:"started"`
	Version string    `json:"version,omitempty"`
}

// ErrNotRunning means no background process is recorded, or the recorded one
// has exited.
var ErrNotRunning = errors.New("not running")

// ErrRunning is returned by Start, with the running process's state, when
// one is already recorded.
var ErrRunning = errors.New("already running")

// maxLog is the size at which the log is rotated to <log>.1 on the next start.
const maxLog = 1 << 20

// Read returns the recorded state, or ErrNotRunning when there is none.
func Read(path string) (State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, ErrNotRunning
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil || s.PID <= 0 {
		return State{}, fmt.Errorf("%s is not a valid state file; delete it if no background dashboard is running", path)
	}
	return s, nil
}

// Write records s atomically, so a reader never sees a half-written file.
func Write(path string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Clear removes the state file if it still belongs to pid. A process calls it
// on the way out; checking the pid keeps it from deleting a newer process's
// record.
func Clear(path string, pid int) {
	if s, err := Read(path); err == nil && s.PID == pid {
		os.Remove(path)
	}
}

// Status returns the state of the running process. A record whose process
// has exited is stale: it is removed and ErrNotRunning returned.
func Status(path string) (State, error) {
	s, err := Read(path)
	if err != nil {
		return State{}, err
	}
	if !alive(s.PID) {
		Clear(path, s.PID)
		return State{}, ErrNotRunning
	}
	return s, nil
}

// Spec describes a process to start in the background.
type Spec struct {
	Exe   string   // binary to run
	Args  []string // its arguments
	Dir   string   // working directory
	Log   string   // stdout and stderr are appended here
	State string   // the process writes its State here when ready
	// Wait is how long to wait for the process to write its state.
	Wait time.Duration
}

// Start launches the process detached from this terminal and waits until it
// records its state. If it exits first, or does not get ready in time, the
// error includes the end of its log.
func Start(sp Spec) (State, error) {
	if !supported {
		return State{}, errors.New("running in the background is not supported on this platform; run it in a separate terminal instead")
	}
	if s, err := Status(sp.State); err == nil {
		return s, ErrRunning
	} else if !errors.Is(err, ErrNotRunning) {
		return State{}, err
	}
	rotate(sp.Log)
	logf, err := os.OpenFile(sp.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return State{}, err
	}
	defer logf.Close()
	start, _ := logf.Seek(0, 2)
	fmt.Fprintf(logf, "--- %s starting: %s %s\n", time.Now().Format(time.RFC3339), filepath.Base(sp.Exe), strings.Join(sp.Args, " "))

	cmd := exec.Command(sp.Exe, sp.Args...)
	cmd.Dir = sp.Dir
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = detached()
	if err := cmd.Start(); err != nil {
		return State{}, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	wait := sp.Wait
	if wait <= 0 {
		wait = 10 * time.Second
	}
	deadline := time.After(wait)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-exited:
			// It may have recorded its state and then failed; either way it is gone.
			Clear(sp.State, cmd.Process.Pid)
			return State{}, fmt.Errorf("it exited before it was ready (%v):\n%s", err, tail(sp.Log, start))
		case <-deadline:
			_ = cmd.Process.Kill()
			Clear(sp.State, cmd.Process.Pid)
			return State{}, fmt.Errorf("it was not ready after %s, so it was stopped:\n%s", wait, tail(sp.Log, start))
		case <-tick.C:
			if s, err := Read(sp.State); err == nil && s.PID == cmd.Process.Pid {
				// Leave the child running; it is in its own session and outlives us.
				_ = cmd.Process.Release()
				return s, nil
			}
		}
	}
}

// Stop asks the running process to exit and waits up to wait for it. If it is
// still there after that, it is killed.
func Stop(path string, wait time.Duration) (State, error) {
	if !supported {
		return State{}, ErrNotRunning
	}
	s, err := Status(path)
	if err != nil {
		return State{}, err
	}
	if err := terminate(s.PID); err != nil {
		return s, fmt.Errorf("cannot stop pid %d: %w", s.PID, err)
	}
	if !waitGone(s.PID, wait) {
		_ = kill(s.PID)
		if !waitGone(s.PID, 2*time.Second) {
			return s, fmt.Errorf("pid %d did not exit", s.PID)
		}
	}
	Clear(path, s.PID)
	return s, nil
}

func waitGone(pid int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return !alive(pid)
}

// rotate moves a log that has grown past maxLog to <log>.1.
func rotate(path string) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLog {
		_ = os.Rename(path, path+".1")
	}
}

// tail returns the last lines the process wrote to its log after offset.
func tail(path string, offset int64) string {
	b, err := os.ReadFile(path)
	if err != nil || offset > int64(len(b)) {
		return "(no log: " + path + ")"
	}
	lines := strings.Split(strings.TrimRight(string(b[offset:]), "\n"), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return strings.Join(lines, "\n") + "\n(full log: " + path + ")"
}
