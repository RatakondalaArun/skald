package github

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOAuth serves the device flow: the token request answers with the
// queued errors first, then a token.
type fakeOAuth struct {
	mu      sync.Mutex
	pending []string // errors to answer the token request with, in order
	polls   int
	form    url.Values
}

func (f *fakeOAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, "/login/") && r.Header.Get("Accept") != "application/json" {
		http.Error(w, "want json", http.StatusNotAcceptable)
		return
	}
	switch r.URL.Path {
	case "/login/device/code":
		f.form = r.PostForm
		if r.PostForm.Get("client_id") == "disabled" {
			_, _ = io.WriteString(w, `{"error":"device_flow_disabled"}`)
			return
		}
		_, _ = io.WriteString(w, `{"device_code":"dc1","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`)
	case "/login/oauth/access_token":
		f.polls++
		if r.PostForm.Get("device_code") != "dc1" || r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
			_, _ = io.WriteString(w, `{"error":"bad_request"}`)
			return
		}
		if len(f.pending) > 0 {
			e := f.pending[0]
			f.pending = f.pending[1:]
			_, _ = io.WriteString(w, `{"error":"`+e+`"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"gho_x","token_type":"bearer","scope":"read:project,repo"}`)
	case "/user":
		if r.Header.Get("Authorization") != "Bearer gho_x" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"login":"octocat"}`)
	default:
		http.NotFound(w, r)
	}
}

func testOAuth(t *testing.T, f *fakeOAuth, clientID string) (*OAuth, *[]time.Duration, string) {
	t.Helper()
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	o := NewOAuth(clientID, ts.URL)
	var waits []time.Duration
	o.sleep = func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return ctx.Err() }
	return o, &waits, ts.URL
}

func TestDeviceFlow(t *testing.T) {
	f := &fakeOAuth{pending: []string{"authorization_pending", "slow_down", "authorization_pending"}}
	o, waits, apiURL := testOAuth(t, f, "cid")
	dc, err := o.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dc.UserCode != "ABCD-1234" || f.form.Get("client_id") != "cid" || f.form.Get("scope") != LoginScopes {
		t.Fatalf("start: %+v %v", dc, f.form)
	}
	path := filepath.Join(t.TempDir(), "sub", "github.json")
	tok, err := o.Finish(context.Background(), dc, apiURL, path)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "gho_x" || tok.Login != "octocat" || f.polls != 4 {
		t.Fatalf("token %+v after %d polls", tok, f.polls)
	}
	// slow_down adds five seconds to the interval from then on.
	want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second, 10 * time.Second}
	if len(*waits) != len(want) {
		t.Fatalf("waits: %v", *waits)
	}
	for i := range want {
		if (*waits)[i] != want[i] {
			t.Fatalf("waits: %v", *waits)
		}
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("saved token mode: %v %v", fi, err)
	}
	got, err := LoadToken(path)
	if err != nil || got.Token != "gho_x" || got.Login != "octocat" {
		t.Fatalf("load: %+v %v", got, err)
	}
	if removed, err := RemoveToken(path); !removed || err != nil {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if removed, err := RemoveToken(path); removed || err != nil {
		t.Fatalf("remove twice: %v %v", removed, err)
	}
	if got, err := LoadToken(path); err != nil || got.Token != "" {
		t.Fatalf("load missing: %+v %v", got, err)
	}
}

func TestDeviceFlowErrors(t *testing.T) {
	if _, err := NewOAuth("", "").Start(context.Background()); err != ErrNoClientID {
		t.Fatalf("no client id: %v", err)
	}
	o, _, _ := testOAuth(t, &fakeOAuth{}, "disabled")
	if _, err := o.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "device flow is not enabled") {
		t.Fatalf("disabled: %v", err)
	}
	for code, want := range map[string]string{"access_denied": "cancelled", "expired_token": "expired"} {
		o, _, _ := testOAuth(t, &fakeOAuth{pending: []string{code}}, "cid")
		dc, err := o.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.Wait(context.Background(), dc); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", code, err)
		}
	}
	// A code past its expiry stops polling without asking GitHub.
	f := &fakeOAuth{}
	o, _, _ = testOAuth(t, f, "cid")
	if _, err := o.Wait(context.Background(), DeviceCode{Code: "dc1", Interval: 5, Expires: time.Now().Add(-time.Second)}); err == nil || f.polls != 0 {
		t.Fatalf("expired locally: %v, %d polls", err, f.polls)
	}
}

func TestWebURL(t *testing.T) {
	for in, want := range map[string]string{"": "https://github.com", DefaultAPIURL: "https://github.com", "https://ghe.example.com/api/v3/": "https://ghe.example.com"} {
		if got := WebURL(in); got != want {
			t.Errorf("WebURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveTokenOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SKALD_HOME", home)
	t.Setenv("PATH", t.TempDir()) // no gh
	t.Setenv(EnvToken, "")
	t.Setenv(EnvGHToken, "")
	if tok, src := ResolveToken(); tok != "" || src != "" {
		t.Fatalf("nothing set: %q %q", tok, src)
	}
	if err := SaveToken(filepath.Join(home, "github.json"), Token{Token: "saved"}); err != nil {
		t.Fatal(err)
	}
	if tok, src := ResolveToken(); tok != "saved" || src != SourceLogin {
		t.Fatalf("saved: %q %q", tok, src)
	}
	t.Setenv(EnvGHToken, "gh")
	if tok, src := ResolveToken(); tok != "gh" || src != EnvGHToken {
		t.Fatalf("GH_TOKEN: %q %q", tok, src)
	}
	t.Setenv(EnvToken, "env")
	if tok, src := ResolveToken(); tok != "env" || src != EnvToken {
		t.Fatalf("GITHUB_TOKEN: %q %q", tok, src)
	}
}

func TestClientID(t *testing.T) {
	old := DefaultClientID
	t.Cleanup(func() { DefaultClientID = old })
	DefaultClientID = "built-in"
	t.Setenv(EnvClientID, "")
	if ClientID() != "built-in" {
		t.Fatal(ClientID())
	}
	t.Setenv(EnvClientID, " from-env ")
	if ClientID() != "from-env" {
		t.Fatal(ClientID())
	}
}
