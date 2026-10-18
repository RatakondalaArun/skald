package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestParseRepo(t *testing.T) {
	ok := map[string]string{
		"octocat/hello":                          "octocat/hello",
		" octocat/hello.js ":                     "octocat/hello.js",
		"https://github.com/octocat/hello":       "octocat/hello",
		"https://github.com/octocat/hello.git":   "octocat/hello",
		"https://github.com/octocat/hello/":      "octocat/hello",
		"git@github.com:octocat/hello.git":       "octocat/hello",
		"ssh://git@github.com/octocat/hello.git": "octocat/hello",
	}
	for in, want := range ok {
		r, err := ParseRepo(in)
		if err != nil || r.String() != want {
			t.Errorf("ParseRepo(%q) = %v, %v; want %s", in, r, err, want)
		}
	}
	for _, in := range []string{"", "octocat", "a/b/c", "https://github.com/octocat", "own er/name"} {
		if _, err := ParseRepo(in); err == nil {
			t.Errorf("ParseRepo(%q) should fail", in)
		}
	}
	if (Repo{"Acme", "Rocket"}).Key() != "acme/rocket" {
		t.Fatal("Key is not lower-cased")
	}
}

func TestNewDerivesGraphQLURL(t *testing.T) {
	if c := New("t", ""); c.GraphQLURL != "https://api.github.com/graphql" || c.RESTURL != DefaultAPIURL {
		t.Fatalf("github.com: %+v", c)
	}
	if c := New("t", "https://ghe.example.com/api/v3/"); c.GraphQLURL != "https://ghe.example.com/api/graphql" || c.RESTURL != "https://ghe.example.com/api/v3" {
		t.Fatalf("enterprise: %+v", c)
	}
}

func TestIssueStates(t *testing.T) {
	cases := []struct {
		state, reason     string
		closed, completed bool
	}{
		{"OPEN", "", false, false},
		{"CLOSED", "COMPLETED", true, true},
		{"CLOSED", "", true, true},
		{"CLOSED", "NOT_PLANNED", true, false},
		{"CLOSED", "DUPLICATE", true, false},
	}
	for _, c := range cases {
		is := Issue{State: c.state, StateReason: c.reason}
		if is.Closed() != c.closed || is.ClosedAsCompleted() != c.completed {
			t.Errorf("%s/%s: closed=%v completed=%v", c.state, c.reason, is.Closed(), is.ClosedAsCompleted())
		}
	}
}

// fakeGitHub serves canned GraphQL pages and records REST calls.
type fakeGitHub struct {
	mu   sync.Mutex
	rest []string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Bad credentials"}`)
		return
	}
	if r.URL.Path != "/graphql" {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.rest = append(f.rest, r.Method+" "+r.URL.Path+" "+string(b))
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
		return
	}
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Variables["name"] == "missing" {
		_, _ = io.WriteString(w, `{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository"}]}`)
		return
	}
	cursor, _ := req.Variables["cursor"].(string)
	switch {
	case strings.Contains(req.Query, "milestones("):
		_, _ = io.WriteString(w, `{"data":{"repository":{"milestones":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":1,"title":"v1","description":"d","state":"OPEN","url":"m1"}]}}}}`)
	case strings.Contains(req.Query, "issues("):
		if req.Variables["name"] == "noscope" && strings.Contains(req.Query, "projectItems") {
			_, _ = io.WriteString(w, `{"data":{"repository":{"issues":null}},"errors":[{"type":"INSUFFICIENT_SCOPES","message":"Your token has not been granted the required scopes to execute this query. The 'projectItems' field requires one of the following scopes: ['read:project']"}]}`)
			return
		}
		if cursor == "" {
			_, _ = io.WriteString(w, `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":true,"endCursor":"c1"},"nodes":[
			  {"number":1,"title":"A","body":"b","state":"CLOSED","stateReason":"COMPLETED","url":"i1","milestone":{"number":1},
			   "labels":{"nodes":[{"name":"P1"}]},"assignees":{"nodes":[{"login":"ann"}]},
			   "closedByPullRequestsReferences":{"nodes":[{"number":9,"title":"fix","url":"p9","state":"MERGED","mergeCommit":{"oid":"abc"}}]},
			   "projectItems":{"nodes":[{"fieldValueByName":{"name":"Done"}},{"fieldValueByName":null}]}}]}}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":false},"nodes":[
		  {"number":2,"title":"B","body":"","state":"OPEN","stateReason":null,"url":"i2","milestone":null,
		   "labels":{"nodes":[]},"assignees":{"nodes":[]},"closedByPullRequestsReferences":{"nodes":[]},"projectItems":{"nodes":[]}}]}}}}`)
	case strings.Contains(req.Query, "discussions("):
		_, _ = io.WriteString(w, `{"data":{"repository":{"hasDiscussionsEnabled":true,"discussions":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":3,"title":"Q","url":"d3","closed":false,"isAnswered":true,"category":{"name":"Q&A"}}]}}}}`)
	default:
		http.Error(w, "unexpected query", http.StatusBadRequest)
	}
}

func TestFetchRepoPagesAndMaps(t *testing.T) {
	ts := httptest.NewServer(&fakeGitHub{})
	defer ts.Close()
	c := New("tok", ts.URL)
	snap, err := c.FetchRepo(context.Background(), Repo{"acme", "rocket"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Milestones) != 1 || snap.Milestones[0].Title != "v1" {
		t.Fatalf("milestones: %+v", snap.Milestones)
	}
	if len(snap.Issues) != 2 {
		t.Fatalf("issues: %+v", snap.Issues)
	}
	a := snap.Issues[0]
	if a.Milestone != 1 || a.Labels[0] != "P1" || a.Assignees[0] != "ann" || len(a.PullRequests) != 1 || !a.PullRequests[0].Merged() || a.PullRequests[0].MergeCommit != "abc" || len(a.BoardStatus) != 1 || a.BoardStatus[0] != "Done" {
		t.Fatalf("issue A: %+v", a)
	}
	if b := snap.Issues[1]; b.Milestone != 0 || b.Closed() {
		t.Fatalf("issue B: %+v", b)
	}
	if len(snap.Discussions) != 1 || !snap.Discussions[0].Answered || snap.Discussions[0].Category != "Q&A" {
		t.Fatalf("discussions: %+v", snap.Discussions)
	}
}

func TestFetchRepoErrors(t *testing.T) {
	ts := httptest.NewServer(&fakeGitHub{})
	defer ts.Close()
	var ae *APIError
	_, err := New("bad", ts.URL).FetchRepo(context.Background(), Repo{"acme", "rocket"})
	if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized || !strings.Contains(err.Error(), "token") {
		t.Fatalf("bad token: %v", err)
	}
	_, err = New("tok", ts.URL).FetchRepo(context.Background(), Repo{"acme", "missing"})
	if !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
		t.Fatalf("missing repo: %v", err)
	}
}

func TestCloseIssue(t *testing.T) {
	f := &fakeGitHub{}
	ts := httptest.NewServer(f)
	defer ts.Close()
	if err := New("tok", ts.URL).CloseIssue(context.Background(), Repo{"acme", "rocket"}, 4, "completed", "done in skald"); err != nil {
		t.Fatal(err)
	}
	if len(f.rest) != 2 ||
		f.rest[0] != `POST /repos/acme/rocket/issues/4/comments {"body":"done in skald"}` ||
		f.rest[1] != `PATCH /repos/acme/rocket/issues/4 {"state":"closed","state_reason":"completed"}` {
		t.Fatalf("calls: %q", f.rest)
	}
}

func TestFetchRepoWithoutProjectScope(t *testing.T) {
	ts := httptest.NewServer(&fakeGitHub{})
	defer ts.Close()
	snap, err := New("tok", ts.URL).FetchRepo(context.Background(), Repo{"acme", "noscope"})
	if err != nil {
		t.Fatal(err)
	}
	if !snap.NoBoardStatus || len(snap.Issues) != 2 {
		t.Fatalf("snapshot: %+v", snap)
	}
}

func TestCloseMilestone(t *testing.T) {
	f := &fakeGitHub{}
	ts := httptest.NewServer(f)
	defer ts.Close()
	if err := New("tok", ts.URL).CloseMilestone(context.Background(), Repo{"acme", "rocket"}, 3); err != nil {
		t.Fatal(err)
	}
	if len(f.rest) != 1 || f.rest[0] != `PATCH /repos/acme/rocket/milestones/3 {"state":"closed"}` {
		t.Fatalf("calls: %q", f.rest)
	}
}
