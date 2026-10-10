package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"skald/internal/domain"
	"skald/internal/service"
	"skald/internal/store"
	"skald/internal/watch"
)

// realServer is a Server fed by the real watch.Subscribe, like skald serve.
func (e *wtEnv) realServer() *Server {
	e.t.Helper()
	srv, err := New(Config{
		Service: e.svc, Keepalive: time.Hour, Logger: wtQuietLogger(),
		Events: func(ctx context.Context, projectID, afterID int64) <-chan domain.Event {
			return watch.Subscribe(ctx, e.db, projectID, afterID, watch.WithInterval(10*time.Millisecond))
		},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return srv
}

// otherProcess opens a second handle on the same database file, so its writes
// reach the server the way writes from the CLI or an MCP process do.
func (e *wtEnv) otherProcess() *service.Service {
	e.t.Helper()
	db, err := store.Open(wtCtx, e.db.Path())
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { db.Close() })
	return service.New(db)
}

func wtAwaitConnected(t *testing.T, c *sseClient) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case l := <-c.lines:
			if l == ": connected" {
				return
			}
		case <-deadline:
			t.Fatal("stream never connected")
		}
	}
}

func TestEndToEndLiveStreamFromAnotherProcess(t *testing.T) {
	e := wtNew(t)
	srv := e.realServer()
	ts := httptestServer(t, srv)
	other := e.otherProcess()
	agent := domain.NewAgentActor("claude-code", "s1s1s1s1")

	c := wtSSE(t, ts.URL+"/events?project=demo", nil)
	defer c.close()
	wtAwaitConnected(t, c)

	if _, err := other.StartTask(wtCtx, agent, 1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := other.AddTaskNote(wtCtx, agent, 1, "working on it"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.RecordArtifact(wtCtx, agent, 1, service.ArtifactInput{Kind: domain.ArtifactCommit, Ref: "abc123"}); err != nil {
		t.Fatal(err)
	}
	if _, err := other.CompleteTask(wtCtx, agent, 1, service.CompleteOptions{}); err != nil {
		t.Fatal(err)
	}
	var types []string
	var lastID int64
	for i := 0; i < 4; i++ {
		ev, ok := c.next(3 * time.Second)
		if !ok {
			t.Fatalf("stream ended after %v", types)
		}
		m := wtDecode(t, ev)
		id := int64(m["id"].(float64))
		if id <= lastID {
			t.Fatalf("ids must increase: %d after %d", id, lastID)
		}
		lastID = id
		if m["actor"] != string(agent) || m["title"] != "Schema" || m["task"] != "T-1" || m["project"] != "demo" {
			t.Fatalf("event = %v", m)
		}
		types = append(types, m["type"].(string))
	}
	if got := strings.Join(types, ","); got != "task.status_changed,task.note,artifact.added,task.status_changed" {
		t.Fatalf("types = %s", got)
	}

	// A change made from the dashboard itself arrives on the stream too, as human:web.
	if _, err := other.UpdateTaskStatus(wtCtx, domain.Human, 4, service.StatusChange{To: domain.TaskBlocked, BlockedReason: "keys"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.next(3 * time.Second); !ok {
		t.Fatal("no block event")
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/projects/demo/tasks/T-4/unblock", strings.NewReader(`{}`))
	req.Header.Set("Origin", ts.URL)
	req.Header.Set(TokenHeader, srv.Token())
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("unblock over HTTP: %v %v", res, err)
	}
	res.Body.Close()
	ev, ok := c.next(3 * time.Second)
	if !ok {
		t.Fatal("no unblock event")
	}
	if m := wtDecode(t, ev); m["actor"] != "human:web" || m["task"] != "T-4" {
		t.Fatalf("event = %v", m)
	}
}

func TestEndToEndReconnectGetsMissedEventsOnce(t *testing.T) {
	e := wtNew(t)
	ts := httptestServer(t, e.realServer())
	other := e.otherProcess()

	first := wtSSE(t, ts.URL+"/events?project=demo", nil)
	wtAwaitConnected(t, first)
	if _, err := other.StartTask(wtCtx, domain.Human, 3, 0); err != nil {
		t.Fatal(err)
	}
	ev, ok := first.next(3 * time.Second)
	if !ok {
		t.Fatal("no first event")
	}
	lastSeen := ev.ID
	first.close()

	// While disconnected: three more events.
	for i := 0; i < 3; i++ {
		if _, err := other.AddTaskNote(wtCtx, domain.Human, 3, fmt.Sprintf("while away %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	second := wtSSE(t, ts.URL+"/events?project=demo", map[string]string{"Last-Event-ID": lastSeen})
	defer second.close()
	var got []string
	for i := 0; i < 3; i++ {
		ev, ok := second.next(3 * time.Second)
		if !ok {
			t.Fatalf("got only %v", got)
		}
		m := wtDecode(t, ev)
		got = append(got, m["payload"].(map[string]any)["text"].(string))
	}
	if strings.Join(got, ",") != "while away 0,while away 1,while away 2" {
		t.Fatalf("replay = %v", got)
	}
	if ev, ok := second.next(200 * time.Millisecond); ok {
		t.Fatalf("event repeated or invented: %v", ev)
	}
}

func TestServeShutsDownGracefullyWithOpenStreams(t *testing.T) {
	e := wtNew(t)
	srv := e.realServer()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(wtCtx)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	base := "http://" + ln.Addr().String()

	// Serve pins the Host port to the listener's.
	req, _ := http.NewRequest("GET", base+"/api/projects", nil)
	req.Host = "evil.example:" + fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 403 {
		t.Fatalf("bad host over a real listener: %v %v", res, err)
	}
	if res, err := http.Get(base + "/api/projects"); err != nil || res.StatusCode != 200 {
		t.Fatalf("good host: %v %v", res, err)
	}

	streams := []*sseClient{wtSSE(t, base+"/events", nil), wtSSE(t, base+"/events", nil)}
	for _, s := range streams {
		wtAwaitConnected(t, s)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Serve did not return after shutdown with open SSE streams")
	}
	for _, s := range streams {
		if _, ok := s.next(2 * time.Second); ok {
			t.Error("stream still delivering after shutdown")
		}
	}
	if _, err := http.Get(base + "/"); err == nil {
		t.Error("server still accepting connections after shutdown")
	}
}
