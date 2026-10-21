package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScratchpadAPI(t *testing.T) {
	e := wtNew(t)
	if code, m := e.get("/api/projects/demo/notes"); code != 200 || len(m["notes"].([]any)) != 0 || m["truncated"] != false {
		t.Fatalf("empty notes: %d %v", code, m)
	}
	code, m := e.post("/api/projects/demo/notes", `{"body": "tokens expire after 15 minutes"}`)
	if code != 200 {
		t.Fatalf("add note: %d %v", code, m)
	}
	n := m["note"].(map[string]any)
	if n["id"] != "N-1" || n["actor"] != string(webActor) || n["task"] != nil {
		t.Fatalf("note = %v", n)
	}
	code, m = e.post("/api/projects/demo/notes", `{"body": "Login needs rate limits", "task": "T-2"}`)
	if n := m["note"].(map[string]any); code != 200 || n["task"] != "T-2" || n["feature"] != "F-1" {
		t.Fatalf("add task note: %d %v", code, m)
	}

	_, m = e.get("/api/projects/demo/notes")
	notes := m["notes"].([]any)
	if len(notes) != 2 || notes[0].(map[string]any)["id"] != "N-2" {
		t.Fatalf("notes = %v", notes)
	}
	for q, want := range map[string]int{"task=T-2": 1, "feature=F-2": 0, "q=TOKENS": 1, "author=human": 0, "limit=1": 1} {
		_, m := e.get("/api/projects/demo/notes?" + q)
		if got := len(m["notes"].([]any)); got != want {
			t.Errorf("?%s: %d notes, want %d", q, got, want)
		}
	}
	if _, m := e.get("/api/projects/demo/notes?limit=1"); m["truncated"] != true {
		t.Errorf("limit=1 should be truncated: %v", m)
	}
	if _, m := e.get("/api/projects/demo"); m["note_count"] != float64(2) {
		t.Errorf("project note_count = %v", m["note_count"])
	}

	// The change feed names the task a note is about.
	_, m = e.get("/api/projects/demo/events?limit=1")
	ev := m["events"].([]any)[0].(map[string]any)
	if ev["type"] != "note.added" || ev["entity"] != "N-2" || ev["task"] != "T-2" || ev["title"] != "Login" {
		t.Errorf("latest event = %v", ev)
	}

	for body, want := range map[string]int{
		`{"body": "  "}`:                  400,
		`{"body": "x", "task": "F-1"}`:    400,
		`{"body": "x", "task": "T-99"}`:   404,
		`{"body": "x", "feature": "T-1"}`: 400,
	} {
		if code, m := e.post("/api/projects/demo/notes", body); code != want {
			t.Errorf("POST %s = %d %v, want %d", body, code, m, want)
		}
	}
	if code, _ := e.get("/api/projects/demo/notes?task=bogus"); code != 400 {
		t.Errorf("bad task filter = %d", code)
	}
	if code, _ := e.get("/api/projects/nope/notes"); code != 404 {
		t.Errorf("unknown project = %d", code)
	}

	// Adding a note is a mutation: it needs the token.
	req := e.postReq("/api/projects/demo/notes", `{"body": "sneaky"}`)
	req.Header.Del(TokenHeader)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("POST without token = %d", rec.Code)
	}
	if _, m := e.get("/api/projects/demo/notes"); len(m["notes"].([]any)) != 2 {
		t.Errorf("a rejected POST wrote a note: %v", m)
	}
}

func TestScratchpadTabIsServed(t *testing.T) {
	e := wtNew(t)
	srv := e.server()
	res, err := srv.Client().Get(srv.URL + "/static/views.js")
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, res); !strings.Contains(body, "'Scratchpad'") {
		t.Error("views.js has no Scratchpad tab")
	}
}
