package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"skald/internal/domain"
)

func wtDecode(t *testing.T, ev sseEvent) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(ev.Data), &m); err != nil {
		t.Fatalf("bad event data %q: %v", ev.Data, err)
	}
	return m
}

func TestSSEOrderedDelivery(t *testing.T) {
	e := wtNew(t)
	ts := e.server()
	e.log.add(wtEvent(1), wtEvent(2), wtEvent(3))
	// No Last-Event-ID: the stream starts at the newest event the database has,
	// not at the fake log's start, so these three are replayed from "after".
	c := wtSSE(t, ts.URL+"/events", map[string]string{"Last-Event-ID": "0"})
	var ids []string
	for i := 0; i < 3; i++ {
		ev, ok := c.next(2 * time.Second)
		if !ok {
			t.Fatalf("stream ended after %v", ids)
		}
		ids = append(ids, ev.ID)
		m := wtDecode(t, ev)
		if strconv.Itoa(int(m["id"].(float64))) != ev.ID {
			t.Fatalf("the SSE id %s must equal the event id %v", ev.ID, m["id"])
		}
		if m["actor"] != "claude:claude-code/abcd1234" || m["type"] != "task.note" || m["project"] != "demo" {
			t.Fatalf("event = %v", m)
		}
	}
	if strings.Join(ids, ",") != "1,2,3" {
		t.Fatalf("ids = %v", ids)
	}
	// New events arrive live.
	e.log.add(wtEvent(4))
	if ev, ok := c.next(2 * time.Second); !ok || ev.ID != "4" {
		t.Fatalf("live event = %v %v", ev, ok)
	}
}

func TestSSEReconnectReplaysExactlyOnce(t *testing.T) {
	e := wtNew(t)
	ts := e.server()
	e.log.add(wtEvent(1), wtEvent(2), wtEvent(3))
	first := wtSSE(t, ts.URL+"/events", map[string]string{"Last-Event-ID": "0"})
	var last string
	for i := 0; i < 3; i++ {
		ev, ok := first.next(2 * time.Second)
		if !ok {
			t.Fatal("stream ended early")
		}
		last = ev.ID
	}
	first.close()
	wtWaitFor(t, "first subscription to end", func() bool { return e.log.active.Load() == 0 })

	// Events 4 and 5 happen while the client is away.
	e.log.add(wtEvent(4), wtEvent(5))
	second := wtSSE(t, ts.URL+"/events", map[string]string{"Last-Event-ID": last})
	var got []string
	for i := 0; i < 2; i++ {
		ev, ok := second.next(2 * time.Second)
		if !ok {
			t.Fatalf("stream ended, got %v", got)
		}
		got = append(got, ev.ID)
	}
	if strings.Join(got, ",") != "4,5" {
		t.Fatalf("after reconnect got %v, want exactly 4,5", got)
	}
	if e.log.lastAftr.Load() != 3 {
		t.Fatalf("the source must be asked to resume after 3, got %d", e.log.lastAftr.Load())
	}
	// And nothing else is repeated.
	if ev, ok := second.next(150 * time.Millisecond); ok {
		t.Fatalf("unexpected extra event %v", ev)
	}
}

func TestSSEWithoutLastEventIDStartsAtLatest(t *testing.T) {
	e := wtNew(t)
	latest, err := e.svc.LatestEventID(wtCtx, 0)
	if err != nil || latest == 0 {
		t.Fatalf("latest = %d %v", latest, err)
	}
	ts := e.server()
	c := wtSSE(t, ts.URL+"/events", nil)
	defer c.close()
	wtWaitFor(t, "subscription", func() bool { return e.log.active.Load() == 1 })
	if got := e.log.lastAftr.Load(); got != latest {
		t.Fatalf("source asked to start after %d, want the latest event %d", got, latest)
	}
}

func TestSSEQueryAfterAndValidation(t *testing.T) {
	e := wtNew(t)
	ts := e.server()
	e.log.add(wtEvent(1), wtEvent(2))
	c := wtSSE(t, ts.URL+"/events?after=1", nil)
	if ev, ok := c.next(2 * time.Second); !ok || ev.ID != "2" {
		t.Fatalf("?after=1 should resume at 2, got %v %v", ev, ok)
	}
	c.close()

	for _, bad := range []map[string]string{{"Last-Event-ID": "abc"}, {"Last-Event-ID": "-4"}} {
		if c := wtSSE(t, ts.URL+"/events", bad); c.status != 400 {
			t.Errorf("%v => %d, want 400", bad, c.status)
		}
	}
	if c := wtSSE(t, ts.URL+"/events?project=nope", nil); c.status != 404 {
		t.Errorf("unknown project => %d, want 404 (not a stream)", c.status)
	}
}

func TestSSEProjectFilterPassedToSource(t *testing.T) {
	e := wtNew(t)
	ts := e.server()
	other := wtEvent(7)
	other.ProjectID = 99
	e.log.add(wtEvent(1), other)
	c := wtSSE(t, ts.URL+"/events?project=demo&after=0", nil)
	defer c.close()
	if ev, ok := c.next(2 * time.Second); !ok || ev.ID != "1" {
		t.Fatalf("got %v %v", ev, ok)
	}
	if ev, ok := c.next(150 * time.Millisecond); ok {
		t.Fatalf("event of another project leaked: %v", ev)
	}
}

func TestSSEKeepalive(t *testing.T) {
	e := wtNew(t, func(c *Config) { c.Keepalive = 20 * time.Millisecond })
	ts := e.server()
	c := wtSSE(t, ts.URL+"/events?after=0", nil)
	defer c.close()
	deadline := time.After(2 * time.Second)
	for n := 0; n < 3; {
		select {
		case l := <-c.lines:
			if l == ": keepalive" {
				n++
			}
		case <-deadline:
			t.Fatal("no keepalive comments")
		}
	}
}

func TestSSEHeaders(t *testing.T) {
	e := wtNew(t)
	ts := e.server()
	res, err := http.Get(ts.URL + "/events?after=0")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if res.Header.Get("Cache-Control") != "no-cache" || res.Header.Get("Content-Security-Policy") == "" {
		t.Errorf("headers = %v", res.Header)
	}
}

func TestSSEClientDisconnectStopsSource(t *testing.T) {
	e := wtNew(t)
	ts := e.server()
	var clients []*sseClient
	for i := 0; i < 5; i++ {
		clients = append(clients, wtSSE(t, ts.URL+"/events?after=0", nil))
	}
	wtWaitFor(t, "5 subscriptions", func() bool { return e.log.active.Load() == 5 })
	for _, c := range clients {
		c.close()
	}
	wtWaitFor(t, "subscriptions to be cleaned up after disconnect", func() bool { return e.log.active.Load() == 0 })
}

// A source that repeats or reorders ids must not make the client see an id
// twice or out of order.
func TestSSEDropsDuplicatesAndReordering(t *testing.T) {
	e := wtNew(t, func(c *Config) {
		c.Events = func(ctx context.Context, _, _ int64) <-chan domain.Event {
			ch := make(chan domain.Event)
			go func() {
				defer close(ch)
				for _, id := range []int64{1, 2, 2, 1, 3, 3} {
					select {
					case ch <- wtEvent(id):
					case <-ctx.Done():
						return
					}
				}
				<-ctx.Done()
			}()
			return ch
		}
	})
	ts := e.server()
	c := wtSSE(t, ts.URL+"/events?after=0", nil)
	defer c.close()
	var got []string
	for i := 0; i < 3; i++ {
		ev, ok := c.next(2 * time.Second)
		if !ok {
			t.Fatalf("ended with %v", got)
		}
		got = append(got, ev.ID)
	}
	if ev, ok := c.next(150 * time.Millisecond); ok {
		t.Fatalf("extra event %v after %v", ev, got)
	}
	if strings.Join(got, ",") != "1,2,3" {
		t.Fatalf("got %v", got)
	}
}

func TestSSEEndsWhenSourceCloses(t *testing.T) {
	e := wtNew(t, func(c *Config) {
		c.Events = func(context.Context, int64, int64) <-chan domain.Event {
			ch := make(chan domain.Event)
			close(ch)
			return ch
		}
	})
	ts := e.server()
	c := wtSSE(t, ts.URL+"/events?after=0", nil)
	defer c.close()
	if _, ok := c.next(2 * time.Second); ok {
		t.Fatal("expected the stream to end")
	}
}

func TestSSEEnrichesTaskEvents(t *testing.T) {
	e := wtNew(t)
	ts := e.server()
	task, err := e.svc.StartTask(wtCtx, domain.Human, 2-1, 0) // T-1
	if err != nil {
		t.Fatal(err)
	}
	evs, err := e.svc.EventsSince(wtCtx, 0, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	e.log.add(evs.Events...)
	c := wtSSE(t, ts.URL+"/events?after=0&project=demo", nil)
	defer c.close()
	var started map[string]any
	for i := 0; i < len(evs.Events); i++ {
		ev, ok := c.next(2 * time.Second)
		if !ok {
			t.Fatal("stream ended early")
		}
		m := wtDecode(t, ev)
		if m["type"] == "task.status_changed" {
			started = m
		}
	}
	if started == nil {
		t.Fatal("no status event")
	}
	if started["task"] != fmt.Sprintf("T-%d", task.ID) || started["title"] != "Schema" || started["actor"] != "human" {
		t.Fatalf("event = %v", started)
	}
}
