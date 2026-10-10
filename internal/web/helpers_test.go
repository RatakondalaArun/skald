package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"skald/internal/domain"
	"skald/internal/service"
	"skald/internal/store"
)

var wtCtx = context.Background()

// wtEnv is a real service over a temp database with a seeded project "demo":
//
//	F-1 Accounts: T-1 Schema -> T-2 Login (depends on T-1)
//	F-2 Reports:  T-3 Digest, T-4 Export (depends on T-3)
type wtEnv struct {
	t   *testing.T
	db  *store.DB
	svc *service.Service
	srv *Server
	log *wtLog
}

func wtNew(t *testing.T, mutate ...func(*Config)) *wtEnv {
	t.Helper()
	db, err := store.Open(wtCtx, filepath.Join(t.TempDir(), "db", "skald.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc := service.New(db)
	if _, err := svc.CreateProject(wtCtx, domain.Human, service.CreateProjectInput{Slug: "demo", Name: "Demo"}); err != nil {
		t.Fatal(err)
	}
	prio := 0
	_, err = svc.CreatePlan(wtCtx, domain.Human, service.ProjectRef{Slug: "demo"}, service.PlanInput{Features: []service.PlanFeature{
		{Title: "Accounts", Weight: 2, Tasks: []service.PlanTask{
			{Ref: "a", Title: "Schema", IdempotencyKey: "k1", Priority: &prio, Description: "Create the tables.", AcceptanceCriteria: "Migration runs."},
			{Ref: "b", Title: "Login", IdempotencyKey: "k2", DependsOn: []string{"a"}},
		}},
		{Title: "Reports", Weight: 1, Tasks: []service.PlanTask{
			{Ref: "c", Title: "Digest", IdempotencyKey: "k3"},
			{Ref: "d", Title: "Export", IdempotencyKey: "k4", DependsOn: []string{"c"}},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	e := &wtEnv{t: t, db: db, svc: svc, log: &wtLog{}}
	cfg := Config{Service: svc, Events: e.log.source, Keepalive: time.Hour, Logger: wtQuietLogger()}
	for _, m := range mutate {
		m(&cfg)
	}
	e.srv, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// wtLog is a fake EventSource over an in-memory log. It replays everything
// after afterID, then follows new events, like watch.Subscribe.
type wtLog struct {
	mu       sync.Mutex
	events   []domain.Event
	active   atomic.Int32
	lastAftr atomic.Int64
}

func (l *wtLog) add(es ...domain.Event) {
	l.mu.Lock()
	l.events = append(l.events, es...)
	l.mu.Unlock()
}

func (l *wtLog) source(ctx context.Context, projectID, afterID int64) <-chan domain.Event {
	l.active.Add(1)
	l.lastAftr.Store(afterID)
	ch := make(chan domain.Event)
	go func() {
		defer l.active.Add(-1)
		defer close(ch)
		next := afterID
		for {
			l.mu.Lock()
			var batch []domain.Event
			for _, e := range l.events {
				if e.ID > next && (projectID == 0 || e.ProjectID == projectID) {
					batch = append(batch, e)
				}
			}
			l.mu.Unlock()
			for _, e := range batch {
				select {
				case ch <- e:
					next = e.ID
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	return ch
}

func wtEvent(id int64) domain.Event {
	return domain.Event{
		ID: id, ProjectID: 1, EntityType: domain.EntityTask, EntityID: 1, Type: domain.EventTaskNote,
		Actor: domain.NewAgentActor("claude-code", "abcd1234"), Payload: json.RawMessage(`{"text":"note"}`),
		CreatedAt: time.Date(2026, 10, 10, 9, 0, int(id), 0, time.UTC),
	}
}

// server starts the full handler on a real listener.
func (e *wtEnv) server() *httptest.Server {
	ts := httptest.NewServer(e.srv.Handler())
	e.t.Cleanup(ts.Close)
	return ts
}

func (e *wtEnv) get(path string) (int, map[string]any) {
	e.t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", path, nil)
	req.Host = "127.0.0.1:7331"
	e.srv.Handler().ServeHTTP(rec, req)
	return rec.Code, wtJSON(e.t, rec.Body.Bytes())
}

// post sends a mutating request with every header the dashboard sends.
func (e *wtEnv) post(path, body string) (int, map[string]any) {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, e.postReq(path, body))
	return rec.Code, wtJSON(e.t, rec.Body.Bytes())
}

func (e *wtEnv) postReq(path, body string) *http.Request {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Host = "127.0.0.1:7331"
	req.Header.Set("Origin", "http://127.0.0.1:7331")
	req.Header.Set(TokenHeader, e.srv.Token())
	req.Header.Set("Content-Type", "application/json")
	return req
}

func wtJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if len(b) > 0 && json.Unmarshal(b, &m) != nil {
		t.Fatalf("not JSON: %s", b)
	}
	return m
}

func wtErrKind(m map[string]any) string {
	if er, ok := m["error"].(map[string]any); ok {
		s, _ := er["kind"].(string)
		return s
	}
	return ""
}

func wtErrMsg(m map[string]any) string {
	if er, ok := m["error"].(map[string]any); ok {
		s, _ := er["message"].(string)
		return s
	}
	return ""
}

// sseClient reads server-sent events from a response.
type sseEvent struct {
	ID   string
	Data string
}

type sseClient struct {
	t      *testing.T
	cancel context.CancelFunc
	events chan sseEvent
	lines  chan string // every raw line, including comments
	status int
}

func wtSSE(t *testing.T, url string, headers map[string]string) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(wtCtx)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c := &sseClient{t: t, cancel: cancel, events: make(chan sseEvent, 64), lines: make(chan string, 256), status: res.StatusCode}
	if res.StatusCode != 200 {
		defer res.Body.Close()
		cancel()
		return c
	}
	go func() {
		defer res.Body.Close()
		defer close(c.events)
		sc := bufio.NewScanner(res.Body)
		var cur sseEvent
		for sc.Scan() {
			line := sc.Text()
			select {
			case c.lines <- line:
			default:
			}
			switch {
			case line == "":
				if cur.Data != "" {
					c.events <- cur
				}
				cur = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				cur.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				cur.Data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	t.Cleanup(cancel)
	return c
}

func (c *sseClient) next(d time.Duration) (sseEvent, bool) {
	select {
	case e, ok := <-c.events:
		return e, ok
	case <-time.After(d):
		return sseEvent{}, false
	}
}

func (c *sseClient) close() { c.cancel() }

func wtWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func wtQuietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func httptestServer(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}
