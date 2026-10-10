package conctest

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Workload size. Override with SKALD_CONC_CLI / SKALD_CONC_CYCLES for a
// heavier local run.
const (
	mcpSessions  = 3
	mcpTasks     = 10
	defaultCLI   = 6
	defaultCycle = 25
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

var busyRE = regexp.MustCompile(`(?i)\b(sqlite_busy|busy|locked)\b`)

// TestConcurrency runs CLI processes, MCP sessions, a web server and two
// event followers against one database and checks that no write is lost,
// duplicated or reordered, and that every consumer sees every event once.
func TestConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process concurrency test skipped with -short")
	}
	cliWorkers := envInt("SKALD_CONC_CLI", defaultCLI)
	cycles := envInt("SKALD_CONC_CYCLES", defaultCycle)
	h := newHarness(t)

	// --- Setup: one project, a feature per CLI worker, a shared "hot" task.
	if _, err := h.run("setup", "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run("setup", "project", "create", "conc", "--slug", "conc", "--no-repo"); err != nil {
		t.Fatal(err)
	}
	h.want("project.created", 1)

	type idOnly struct {
		ID string `json:"id"`
	}
	addFeature := func(title string) string {
		var f idOnly
		if err := h.runJSON("setup", &f, "feature", "add", title); err != nil {
			t.Fatal(err)
		}
		h.want("feature.created", 1)
		return f.ID
	}
	hotFeature := addFeature("hot")
	var hot idOnly
	if err := h.runJSON("setup", &hot, "task", "add", "hot task", "--feature", hotFeature, "--key", "hot"); err != nil {
		t.Fatal(err)
	}
	h.want("task.created", 1)
	if _, err := h.run("setup", "task", "start", hot.ID); err != nil {
		t.Fatal(err)
	}
	h.want("task.status_changed", 1)
	features := make([]string, cliWorkers)
	for i := range features {
		features[i] = addFeature(fmt.Sprintf("cli-%d", i))
	}

	// --- Consumers, started before any concurrent write and replaying from
	// event 0, so they must see every event ever written.
	serve := h.start("serve", "", false, "serve", "--addr", "127.0.0.1:0")
	base, err := serve.waitStderr(regexp.MustCompile(`skald dashboard: (http://\S+)`), 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sseCtx, sseCancel := context.WithCancel(context.Background())
	defer sseCancel()
	sseFeed := &feed{}
	sseDone := make(chan struct{})
	go func() {
		defer close(sseDone)
		readSSE(sseCtx, base+"/events?after=0", sseFeed)
	}()

	follow := h.start("log-follow", "", false, "log", "--follow", "--since", "0", "--all-projects", "--json")
	followFeed := &feed{}
	followDone := make(chan struct{})
	go func() {
		defer close(followDone)
		readJSONLines(follow.stdout, followFeed)
	}()

	// --- Concurrent workload.
	var wg sync.WaitGroup
	for i := 0; i < cliWorkers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cliWorker(h, i, features[i], hot.ID, cycles)
		}(i)
	}
	for i := 0; i < mcpSessions; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mcpWorker(h, i, hot.ID)
		}(i)
	}
	wg.Wait()

	// --- Let the consumers catch up, then stop everything.
	db := openDB(t, filepath.Join(h.home, "skald.db"))
	var maxID int64
	if err := db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&maxID); err != nil {
		t.Fatal(err)
	}
	caughtUp := waitFor(20*time.Second, func() bool {
		return sseFeed.last() >= maxID && followFeed.last() >= maxID
	})
	sseCancel()
	<-sseDone
	h.stop(serve, syscall.SIGINT, 20*time.Second)
	h.stop(follow, syscall.SIGINT, 20*time.Second)
	<-followDone
	if !caughtUp {
		t.Errorf("consumers did not catch up to event %d: sse at %d, log --follow at %d", maxID, sseFeed.last(), followFeed.last())
	}

	// --- Assertions.
	h.mu.Lock()
	for _, f := range h.failed {
		t.Errorf("worker failure: %s", f)
	}
	for label, runs := range h.stderrs {
		for _, s := range runs {
			if m := busyRE.FindString(s); m != "" {
				t.Errorf("%s reported %q on stderr:\n%s", label, m, s)
			}
		}
	}
	expected := map[string]int64{}
	var expectedTotal int64
	for typ, n := range h.expect {
		expected[typ] = n
		expectedTotal += n
	}
	h.mu.Unlock()

	t.Logf("%d events from %d CLI workers x %d cycles and %d MCP sessions x %d tasks; sse saw %d, log --follow saw %d",
		maxID, cliWorkers, cycles, mcpSessions, mcpTasks, sseFeed.last(), followFeed.last())
	checkEventLog(t, db, maxID, expected, expectedTotal)
	checkFeed(t, "SSE client", sseFeed, maxID)
	checkFeed(t, "log --follow", followFeed, maxID)
	checkFinalState(t, h, db, cliWorkers*cycles+mcpSessions*mcpTasks, cliWorkers+mcpSessions)

	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Errorf("PRAGMA integrity_check = %q, %v", integrity, err)
	}
}

// cliWorker is one CLI "user": it adds, starts, notes, evidences and
// completes tasks, and also writes to the shared hot task.
func cliWorker(h *harness, i int, feature, hot string, cycles int) {
	actor := fmt.Sprintf("cli-w%d", i)
	for c := 0; c < cycles; c++ {
		var task struct {
			ID string `json:"id"`
		}
		key := fmt.Sprintf("cli%d-%d", i, c)
		if err := h.runJSON(actor, &task, "task", "add", "task "+key, "--feature", feature, "--key", key); err != nil {
			return
		}
		h.want("task.created", 1)
		steps := []struct {
			typ  string
			args []string
		}{
			{"task.status_changed", []string{"task", "start", task.ID}},
			{"task.note", []string{"task", "note", task.ID, "working on " + key}},
			{"task.note", []string{"task", "note", hot, "from " + key}},
			{"artifact.added", []string{"evidence", "add", task.ID, "--kind", "commit", "--ref", key, "--summary", "proof " + key}},
			{"task.status_changed", []string{"task", "done", task.ID}},
		}
		for _, s := range steps {
			if _, err := h.run(actor, s.args...); err != nil {
				return
			}
			h.want(s.typ, 1)
		}
	}
}

// mcpWorker is one Claude-like session: it plans a feature, then works the
// tasks in order, alternating between separate calls and report_task_result.
func mcpWorker(h *harness, i int, hot string) {
	label := fmt.Sprintf("mcp-%d", i)
	c, err := h.newMCP(label)
	if err != nil {
		h.fail("%s: %v", label, err)
		return
	}
	defer c.close()

	type planTask struct {
		Ref            string   `json:"ref"`
		Title          string   `json:"title"`
		DependsOn      []string `json:"depends_on,omitempty"`
		IdempotencyKey string   `json:"idempotency_key"`
	}
	var tasks []planTask
	for n := 0; n < mcpTasks; n++ {
		t := planTask{Ref: fmt.Sprintf("t%d", n), Title: fmt.Sprintf("mcp %d task %d", i, n), IdempotencyKey: fmt.Sprintf("mcp%d-%d", i, n)}
		if n%3 == 1 { // a few dependencies, so cycle checks run under contention
			t.DependsOn = []string{fmt.Sprintf("t%d", n-1)}
		}
		tasks = append(tasks, t)
	}
	text, err := c.call("create_plan", map[string]any{
		"project": "conc",
		"features": []any{map[string]any{
			"title": fmt.Sprintf("mcp-%d", i),
			"tasks": tasks,
		}},
	})
	if err != nil {
		h.fail("%s: %v", label, err)
		return
	}
	var plan struct {
		Created struct {
			Dependencies int64 `json:"dependencies"`
			Features     int64 `json:"features"`
			Tasks        int64 `json:"tasks"`
		} `json:"created"`
		Tasks []struct {
			Ref string `json:"ref"`
			ID  string `json:"id"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(text), &plan); err != nil || len(plan.Tasks) != mcpTasks {
		h.fail("%s: unexpected create_plan result %q (%v)", label, text, err)
		return
	}
	h.want("feature.created", plan.Created.Features)
	h.want("task.created", plan.Created.Tasks)
	h.want("task.dependency_added", plan.Created.Dependencies)

	call := func(tool string, args map[string]any) bool {
		args["project"] = "conc"
		if _, err := c.call(tool, args); err != nil {
			h.fail("%s: %v", label, err)
			return false
		}
		return true
	}
	for n, t := range plan.Tasks {
		if !call("start_task", map[string]any{"task": t.ID}) {
			return
		}
		h.want("task.status_changed", 1)
		if !call("add_task_note", map[string]any{"task": t.ID, "text": "starting " + t.Ref}) {
			return
		}
		h.want("task.note", 1)
		if !call("add_task_note", map[string]any{"task": hot, "text": "from " + label}) {
			return
		}
		h.want("task.note", 1)
		if n%2 == 0 {
			if !call("record_verification", map[string]any{"task": t.ID, "kind": "test_report", "ref": "ci/" + label + "/" + t.Ref, "summary": "tests pass"}) {
				return
			}
			h.want("artifact.added", 1)
			if !call("update_task_status", map[string]any{"task": t.ID, "status": "done"}) {
				return
			}
			h.want("task.status_changed", 1)
		} else {
			if !call("report_task_result", map[string]any{
				"task":     t.ID,
				"summary":  "done " + t.Ref,
				"evidence": []any{map[string]any{"kind": "commit", "ref": label + "-" + t.Ref}},
			}) {
				return
			}
			// note + artifact + completion, in one transaction
			h.want("task.note", 1)
			h.want("artifact.added", 1)
			h.want("task.status_changed", 1)
		}
	}
}

// readSSE reads an event stream and records each event's id, checking that
// the "id:" line matches the id inside the data.
func readSSE(ctx context.Context, url string, f *feed) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		f.problem(err.Error())
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			f.problem("connect: " + err.Error())
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.problem(fmt.Sprintf("status %s", resp.Status))
		return
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var idLine, data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if data != "" {
				var ev struct {
					ID int64 `json:"id"`
				}
				if err := json.Unmarshal([]byte(data), &ev); err != nil || ev.ID == 0 {
					f.problem(fmt.Sprintf("bad SSE data %q", data))
				} else if strconv.FormatInt(ev.ID, 10) != idLine {
					f.problem(fmt.Sprintf("SSE id line %q does not match data id %d", idLine, ev.ID))
				} else {
					f.add(ev.ID)
				}
			}
			idLine, data = "", ""
		case strings.HasPrefix(line, "id:"):
			idLine = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		f.problem("stream: " + err.Error())
	}
}

func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cond()
}

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// checkEventLog verifies the durable log: contiguous ids and exactly the
// events the successful writes should have produced, per type.
func checkEventLog(t *testing.T, db *sql.DB, maxID int64, expected map[string]int64, expectedTotal int64) {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var prev, count int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		if id != prev+1 {
			t.Fatalf("event ids are not contiguous: %d follows %d", id, prev)
		}
		prev = id
		count++
	}
	if count != maxID {
		t.Errorf("event table has %d rows but max id %d", count, maxID)
	}
	got := map[string]int64{}
	trows, err := db.Query(`SELECT type, COUNT(*) FROM events GROUP BY type`)
	if err != nil {
		t.Fatal(err)
	}
	defer trows.Close()
	for trows.Next() {
		var typ string
		var n int64
		if err := trows.Scan(&typ, &n); err != nil {
			t.Fatal(err)
		}
		got[typ] = n
	}
	for typ, want := range expected {
		if got[typ] != want {
			t.Errorf("events of type %s: got %d, want %d", typ, got[typ], want)
		}
	}
	for typ, n := range got {
		if _, ok := expected[typ]; !ok {
			t.Errorf("unexpected %d events of type %s", n, typ)
		}
	}
	if maxID != expectedTotal {
		t.Errorf("total events %d, but successful writes account for %d", maxID, expectedTotal)
	}
}

// checkFeed verifies a consumer saw exactly events 1..maxID, once each, in
// order.
func checkFeed(t *testing.T, name string, f *feed, maxID int64) {
	t.Helper()
	ids, bad := f.snapshot()
	for _, b := range bad {
		t.Errorf("%s: %s", name, b)
	}
	if int64(len(ids)) != maxID {
		t.Errorf("%s received %d events, want %d", name, len(ids), maxID)
	}
	for i, id := range ids {
		if id != int64(i)+1 {
			t.Errorf("%s: event #%d has id %d, want %d (gap, duplicate or reorder)", name, i+1, id, i+1)
			return
		}
	}
}

// checkFinalState verifies the task table agrees with the workload.
func checkFinalState(t *testing.T, h *harness, db *sql.DB, wantDone, wantFeaturesDone int) {
	t.Helper()
	count := func(q string, args ...any) int {
		var n int
		if err := db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM tasks WHERE status = 'done'`); n != wantDone {
		t.Errorf("done tasks = %d, want %d", n, wantDone)
	}
	if n := count(`SELECT COUNT(*) FROM tasks WHERE status = 'in_progress'`); n != 1 {
		t.Errorf("in_progress tasks = %d, want 1 (the hot task)", n)
	}
	if n := count(`SELECT COUNT(*) FROM tasks WHERE status NOT IN ('done', 'in_progress')`); n != 0 {
		t.Errorf("%d tasks are neither done nor the hot task", n)
	}
	if n := count(`SELECT COUNT(*) FROM tasks t WHERE t.status = 'done' AND NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.task_id = t.id)`); n != 0 {
		t.Errorf("%d tasks are done without evidence", n)
	}
	if n := count(`SELECT COUNT(*) FROM tasks WHERE status = 'done' AND completed_at IS NULL`); n != 0 {
		t.Errorf("%d done tasks have no completed_at", n)
	}
	// Every task's version must equal 1 + the number of its status changes.
	if n := count(`SELECT COUNT(*) FROM tasks t WHERE t.version <> 1 + (SELECT COUNT(*) FROM events e WHERE e.entity_type = 'task' AND e.entity_id = t.id AND e.type = 'task.status_changed')`); n != 0 {
		t.Errorf("%d tasks have a version that disagrees with their status events", n)
	}

	// Progress through the real CLI: every feature is done except "hot".
	out, err := h.run("final", "status", "--json")
	if err != nil {
		t.Errorf("status: %v", err)
		return
	}
	var st struct {
		Progress struct {
			Fraction float64 `json:"fraction"`
		} `json:"progress"`
		Counts struct {
			Done  int `json:"done"`
			Total int `json:"total"`
		} `json:"counts"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Errorf("status JSON: %v", err)
		return
	}
	if st.Counts.Done != wantDone || st.Counts.Total != wantDone+1 {
		t.Errorf("status counts = %+v, want %d done of %d", st.Counts, wantDone, wantDone+1)
	}
	want := float64(wantFeaturesDone) / float64(wantFeaturesDone+1)
	if math.Abs(st.Progress.Fraction-want) > 1e-9 {
		t.Errorf("progress fraction = %v, want %v", st.Progress.Fraction, want)
	}
}
