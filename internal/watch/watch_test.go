package watch_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"skald/internal/domain"
	"skald/internal/service"
	"skald/internal/store"
	"skald/internal/watch"
)

const fastPoll = 20 * time.Millisecond

// TestMain doubles as the writer process: the tests re-exec the test binary
// with SKALD_WATCH_WRITER set to write notes into a shared database.
func TestMain(m *testing.M) {
	if name := os.Getenv("SKALD_WATCH_WRITER"); name != "" {
		os.Exit(runWriter(name))
	}
	os.Exit(m.Run())
}

func runWriter(name string) int {
	ctx := context.Background()
	db, err := store.Open(ctx, os.Getenv("SKALD_WATCH_DB"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "writer open:", err)
		return 1
	}
	defer db.Close()
	taskID, _ := strconv.ParseInt(os.Getenv("SKALD_WATCH_TASK"), 10, 64)
	count, _ := strconv.Atoi(os.Getenv("SKALD_WATCH_COUNT"))
	svc := service.New(db)
	for i := 0; i < count; i++ {
		if _, err := svc.AddTaskNote(ctx, domain.Actor("writer-"+name), taskID, fmt.Sprintf("%s-%d", name, i)); err != nil {
			fmt.Fprintln(os.Stderr, "writer note:", err)
			return 1
		}
	}
	return 0
}

type fixture struct {
	db      *store.DB
	svc     *service.Service
	path    string
	project domain.Project
	task    domain.Task
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "skald.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc := service.New(db)
	f := &fixture{db: db, svc: svc, path: path}
	f.project, f.task = f.addProject(t, "alpha")
	return f
}

func (f *fixture) addProject(t *testing.T, slug string) (domain.Project, domain.Task) {
	t.Helper()
	ctx := context.Background()
	p, err := f.svc.CreateProject(ctx, domain.Human, service.CreateProjectInput{Slug: slug, Name: slug})
	if err != nil {
		t.Fatal(err)
	}
	ref := service.ProjectRef{Slug: slug}
	feat, err := f.svc.CreateFeature(ctx, domain.Human, ref, service.CreateFeatureInput{Title: "feature"})
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := f.svc.CreateTask(ctx, domain.Human, ref, service.CreateTaskInput{FeatureID: feat.ID, Title: "task"})
	if err != nil {
		t.Fatal(err)
	}
	return p, task
}

func (f *fixture) note(t *testing.T, task domain.Task, text string) {
	t.Helper()
	if _, err := f.svc.AddTaskNote(context.Background(), domain.Human, task.ID, text); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) latest(t *testing.T) int64 {
	t.Helper()
	id, err := f.svc.LatestEventID(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// spawn starts a writer process and returns a function that waits for it.
func (f *fixture) spawn(t *testing.T, name string, count int) func() {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		"SKALD_WATCH_WRITER="+name,
		"SKALD_WATCH_DB="+f.path,
		"SKALD_WATCH_TASK="+strconv.FormatInt(f.task.ID, 10),
		"SKALD_WATCH_COUNT="+strconv.Itoa(count),
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := cmd.Wait(); err != nil {
			t.Errorf("writer %s: %v", name, err)
		}
	}
}

func noteText(ev domain.Event) string {
	var p struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(ev.Payload, &p)
	return p.Text
}

// collect reads n events or fails after timeout.
func collect(t *testing.T, ch <-chan domain.Event, n int, timeout time.Duration) []domain.Event {
	t.Helper()
	var got []domain.Event
	deadline := time.After(timeout)
	for len(got) < n {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %d of %d events", len(got), n)
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out after %v with %d of %d events", timeout, len(got), n)
		}
	}
	return got
}

func assertIncreasing(t *testing.T, events []domain.Event) {
	t.Helper()
	for i := 1; i < len(events); i++ {
		if events[i].ID <= events[i-1].ID {
			t.Fatalf("event ids not strictly increasing at %d: %d then %d", i, events[i-1].ID, events[i].ID)
		}
	}
}

func assertQuiet(t *testing.T, ch <-chan domain.Event, d time.Duration) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("unexpected extra event %d (%s)", ev.ID, ev.Type)
		}
		t.Fatal("channel closed unexpectedly")
	case <-time.After(d):
	}
}

func TestSameProcessWritesAreSeen(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := watch.Subscribe(ctx, f.db, f.project.ID, f.latest(t), watch.WithInterval(fastPoll))

	start := time.Now()
	f.note(t, f.task, "hello")
	got := collect(t, ch, 1, time.Second)
	if got[0].Type != domain.EventTaskNote || noteText(got[0]) != "hello" {
		t.Fatalf("got %s %q", got[0].Type, noteText(got[0]))
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
	assertQuiet(t, ch, 100*time.Millisecond)
}

func TestReplayFromZeroAndResumeFromCursor(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		f.note(t, f.task, fmt.Sprintf("n%d", i))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	all := collect(t, watch.Subscribe(ctx, f.db, 0, 0, watch.WithInterval(fastPoll)), int(f.latest(t)), 2*time.Second)
	assertIncreasing(t, all)
	if all[0].ID != 1 {
		t.Fatalf("first replayed id = %d", all[0].ID)
	}

	mid := all[2].ID
	resumed := collect(t, watch.Subscribe(ctx, f.db, 0, mid, watch.WithInterval(fastPoll)), len(all)-3, 2*time.Second)
	if resumed[0].ID != all[3].ID {
		t.Fatalf("resume started at %d, want %d", resumed[0].ID, all[3].ID)
	}
	if resumed[len(resumed)-1].ID != all[len(all)-1].ID {
		t.Fatalf("resume ended at %d", resumed[len(resumed)-1].ID)
	}
}

func TestProjectFilter(t *testing.T) {
	f := newFixture(t)
	_, otherTask := f.addProject(t, "beta")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := watch.Subscribe(ctx, f.db, f.project.ID, f.latest(t), watch.WithInterval(fastPoll))

	f.note(t, otherTask, "other")
	f.note(t, f.task, "mine")
	f.note(t, otherTask, "other again")
	got := collect(t, ch, 1, time.Second)
	if noteText(got[0]) != "mine" || got[0].ProjectID != f.project.ID {
		t.Fatalf("got project %d %q", got[0].ProjectID, noteText(got[0]))
	}
	assertQuiet(t, ch, 150*time.Millisecond)

	all := watch.Subscribe(ctx, f.db, 0, f.latest(t), watch.WithInterval(fastPoll))
	f.note(t, otherTask, "x")
	f.note(t, f.task, "y")
	both := collect(t, all, 2, time.Second)
	if both[0].ProjectID == both[1].ProjectID {
		t.Fatal("project 0 should deliver events of every project")
	}
}

func TestSeparateProcessWritesArriveWithinASecond(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := watch.Subscribe(ctx, f.db, f.project.ID, f.latest(t))

	// Let the watcher settle on its first data_version, then write elsewhere.
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	wait := f.spawn(t, "solo", 20)
	first := collect(t, ch, 1, time.Second)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("first event after %v", d)
	}
	rest := collect(t, ch, 19, 5*time.Second)
	wait()

	got := append(first, rest...)
	assertIncreasing(t, got)
	for i, ev := range got {
		if want := fmt.Sprintf("solo-%d", i); noteText(ev) != want {
			t.Fatalf("event %d text = %q, want %q", i, noteText(ev), want)
		}
		if ev.Actor != "writer-solo" {
			t.Fatalf("actor = %q", ev.Actor)
		}
	}
	assertQuiet(t, ch, 200*time.Millisecond)
}

func TestBurstsFromThreeProcessesArriveExactlyOnce(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := watch.Subscribe(ctx, f.db, f.project.ID, f.latest(t), watch.WithInterval(fastPoll))

	const writers, per = 3, 150
	var waits []func()
	for i := 0; i < writers; i++ {
		waits = append(waits, f.spawn(t, fmt.Sprintf("w%d", i), per))
	}
	got := collect(t, ch, writers*per, 30*time.Second)
	for _, w := range waits {
		w()
	}

	assertIncreasing(t, got)
	seen := map[string]bool{}
	for _, ev := range got {
		txt := noteText(ev)
		if seen[txt] {
			t.Fatalf("duplicate delivery of %q", txt)
		}
		seen[txt] = true
	}
	for i := 0; i < writers; i++ {
		for j := 0; j < per; j++ {
			if !seen[fmt.Sprintf("w%d-%d", i, j)] {
				t.Fatalf("missing w%d-%d", i, j)
			}
		}
	}
	// Ids are contiguous: no event of this project was skipped.
	for i := 1; i < len(got); i++ {
		if got[i].ID != got[i-1].ID+1 {
			t.Fatalf("gap between ids %d and %d", got[i-1].ID, got[i].ID)
		}
	}
	assertQuiet(t, ch, 200*time.Millisecond)
}

func TestSlowConsumerGetsEverythingInOrder(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := watch.Subscribe(ctx, f.db, f.project.ID, f.latest(t), watch.WithInterval(fastPoll))

	const n = 700 // more than the channel buffer and one query batch
	for i := 0; i < n; i++ {
		f.note(t, f.task, strconv.Itoa(i))
	}
	first := collect(t, ch, 10, 2*time.Second)
	time.Sleep(400 * time.Millisecond) // consumer stalls; watcher must wait, not drop
	f.note(t, f.task, "late")
	rest := collect(t, ch, n-10+1, 10*time.Second)

	got := append(first, rest...)
	assertIncreasing(t, got)
	for i := 0; i < n; i++ {
		if noteText(got[i]) != strconv.Itoa(i) {
			t.Fatalf("event %d = %q", i, noteText(got[i]))
		}
	}
	if noteText(got[n]) != "late" {
		t.Fatalf("last = %q", noteText(got[n]))
	}
}

func TestCancelClosesChannelAndReleasesResources(t *testing.T) {
	f := newFixture(t)
	before := runtime.NumGoroutine()

	for round := 0; round < 5; round++ {
		ctx, cancel := context.WithCancel(context.Background())
		ch := watch.Subscribe(ctx, f.db, 0, 0, watch.WithInterval(fastPoll))
		collect(t, ch, 1, time.Second) // watcher is up and holds a connection
		cancel()
		drainClosed(t, ch)
	}

	// A watcher blocked on a consumer that never reads must also stop.
	for i := 0; i < 200; i++ {
		f.note(t, f.task, strconv.Itoa(i))
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := watch.Subscribe(ctx, f.db, 0, 0, watch.WithInterval(fastPoll))
	time.Sleep(200 * time.Millisecond) // buffer fills, watcher blocks on send
	cancel()
	drainClosed(t, ch)

	deadline := time.Now().Add(2 * time.Second)
	for {
		inUse := f.db.Reader().Stats().InUse
		g := runtime.NumGoroutine()
		if inUse == 0 && g <= before+1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("leak: %d reader connections in use, goroutines %d (was %d)", inUse, g, before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// drainClosed discards buffered events and fails if the channel stays open.
func drainClosed(t *testing.T, ch <-chan domain.Event) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("channel not closed after cancel")
		}
	}
}

func TestStartsAfterLatestSeesOnlyNewEvents(t *testing.T) {
	f := newFixture(t)
	f.note(t, f.task, "old")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := watch.Subscribe(ctx, f.db, f.project.ID, f.latest(t), watch.WithInterval(fastPoll))
	assertQuiet(t, ch, 200*time.Millisecond)
	f.note(t, f.task, "new")
	if got := collect(t, ch, 1, time.Second); noteText(got[0]) != "new" {
		t.Fatalf("got %q", noteText(got[0]))
	}
}
