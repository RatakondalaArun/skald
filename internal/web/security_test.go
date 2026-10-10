package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckListenAddr(t *testing.T) {
	ok := []string{"127.0.0.1:7331", "localhost:7331", "[::1]:7331", "127.0.0.2:0", "LOCALHOST:80"}
	for _, a := range ok {
		if err := CheckListenAddr(a, false); err != nil {
			t.Errorf("%s should be allowed: %v", a, err)
		}
	}
	refused := []string{"0.0.0.0:7331", ":7331", "[::]:7331", "192.168.1.20:7331", "10.0.0.5:80", "example.com:80", "myhost:7331"}
	for _, a := range refused {
		err := CheckListenAddr(a, false)
		if err == nil || !strings.Contains(err.Error(), "--insecure-listen") {
			t.Errorf("%s should be refused with a hint about --insecure-listen, got %v", a, err)
		}
		if err := CheckListenAddr(a, true); err != nil {
			t.Errorf("%s should be allowed with insecure: %v", a, err)
		}
	}
	for _, a := range []string{"7331", "127.0.0.1", "127.0.0.1:http", "127.0.0.1:99999", ""} {
		if err := CheckListenAddr(a, true); err == nil {
			t.Errorf("%q should be invalid even with insecure", a)
		}
	}
}

func TestHostCheck(t *testing.T) {
	e := wtNew(t, func(c *Config) { c.Port = 7331 })
	allowed := []string{"127.0.0.1:7331", "localhost:7331", "LocalHost:7331", "[::1]:7331", "127.0.0.9:7331"}
	denied := []string{
		"evil.example:7331", "evil.example", "127.0.0.1.evil.example:7331", "localhost.evil.example:7331",
		"127.0.0.1:8080", "localhost", "127.0.0.1", "192.168.1.5:7331", "[::1]", "", "0.0.0.0:7331",
		"localhost:7331@evil.example", "evil.example#localhost:7331",
	}
	for _, path := range []string{"/", "/api/projects", "/events", "/static/app.js"} {
		for _, h := range allowed {
			req := httptest.NewRequest("GET", path, nil)
			req.Host = h
			rec := httptest.NewRecorder()
			if path == "/events" {
				// Only check the guard, not the stream: a bad project fails fast after the guard.
				req = httptest.NewRequest("GET", path+"?project=nope", nil)
				req.Host = h
			}
			e.srv.Handler().ServeHTTP(rec, req)
			if rec.Code == http.StatusForbidden {
				t.Errorf("%s with Host %q should pass the host check, got %d", path, h, rec.Code)
			}
		}
		for _, h := range denied {
			req := httptest.NewRequest("GET", path, nil)
			req.Host = h
			rec := httptest.NewRecorder()
			e.srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden || wtErrKind(wtJSON(t, rec.Body.Bytes())) != "forbidden_host" {
				t.Errorf("%s with Host %q = %d %s, want forbidden_host", path, h, rec.Code, rec.Body)
			}
		}
	}
}

func TestHostCheckUnpinnedPortAcceptsAnyLoopbackPort(t *testing.T) {
	e := wtNew(t)
	for _, h := range []string{"127.0.0.1:1", "localhost:65000"} {
		if !e.srv.hostAllowed(h) {
			t.Errorf("%s should be allowed when no port is pinned", h)
		}
	}
	if e.srv.hostAllowed("evil.example:7331") {
		t.Error("a non-loopback name must never be allowed")
	}
}

func TestInsecureSkipsHostCheckOnly(t *testing.T) {
	e := wtNew(t, func(c *Config) { c.Insecure = true })
	if !e.srv.hostAllowed("192.168.1.20:7331") {
		t.Error("insecure mode should accept the host the server is reached by")
	}
	// Mutations still need origin and token.
	req := e.postReq("/api/projects/demo/tasks/T-3/cancel", `{}`)
	req.Host = "192.168.1.20:7331"
	req.Header.Del("Origin")
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("missing Origin in insecure mode = %d", rec.Code)
	}
}

func TestMutationGuards(t *testing.T) {
	path := "/api/projects/demo/tasks/T-3/cancel"
	cases := []struct {
		name   string
		mutate func(*http.Request, *wtEnv)
		status int
		kind   string
	}{
		{"no origin", func(r *http.Request, _ *wtEnv) { r.Header.Del("Origin") }, 403, "forbidden_origin"},
		{"null origin", func(r *http.Request, _ *wtEnv) { r.Header.Set("Origin", "null") }, 403, "forbidden_origin"},
		{"forged origin", func(r *http.Request, _ *wtEnv) { r.Header.Set("Origin", "https://evil.example") }, 403, "forbidden_origin"},
		{"origin with another port", func(r *http.Request, _ *wtEnv) { r.Header.Set("Origin", "http://127.0.0.1:9999") }, 403, "forbidden_origin"},
		{"origin on another loopback name", func(r *http.Request, _ *wtEnv) { r.Header.Set("Origin", "http://localhost:7331") }, 403, "forbidden_origin"},
		{"garbage origin", func(r *http.Request, _ *wtEnv) { r.Header.Set("Origin", "::::") }, 403, "forbidden_origin"},
		{"cross-site fetch metadata", func(r *http.Request, _ *wtEnv) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403, "forbidden_origin"},
		{"same-site fetch metadata", func(r *http.Request, _ *wtEnv) { r.Header.Set("Sec-Fetch-Site", "same-site") }, 403, "forbidden_origin"},
		{"no token", func(r *http.Request, _ *wtEnv) { r.Header.Del(TokenHeader) }, 403, "forbidden_token"},
		{"wrong token", func(r *http.Request, e *wtEnv) { r.Header.Set(TokenHeader, e.srv.Token()+"x") }, 403, "forbidden_token"},
		{"empty token", func(r *http.Request, _ *wtEnv) { r.Header.Set(TokenHeader, "") }, 403, "forbidden_token"},
		{"form content type", func(r *http.Request, _ *wtEnv) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, 415, "unsupported_media_type"},
		{"bad host", func(r *http.Request, _ *wtEnv) {
			r.Host = "evil.example:7331"
			r.Header.Set("Origin", "http://evil.example:7331")
		}, 403, "forbidden_host"},
		{"valid", func(*http.Request, *wtEnv) {}, 200, ""},
		{"valid same-origin fetch metadata", func(r *http.Request, _ *wtEnv) { r.Header.Set("Sec-Fetch-Site", "same-origin") }, 200, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := wtNew(t, func(c *Config) { c.Port = 7331 })
			req := e.postReq(path, `{}`)
			c.mutate(req, e)
			rec := httptest.NewRecorder()
			e.srv.Handler().ServeHTTP(rec, req)
			m := wtJSON(t, rec.Body.Bytes())
			if rec.Code != c.status || wtErrKind(m) != c.kind {
				t.Fatalf("got %d %v, want %d %q", rec.Code, m, c.status, c.kind)
			}
			// A rejected request must not have changed anything.
			if c.status != 200 {
				_, tk := e.get("/api/projects/demo/tasks/T-3")
				if tk["task"].(map[string]any)["status"] != "todo" {
					t.Fatal("rejected request changed the task")
				}
			}
		})
	}
}

func TestReadOnlyMethodsAndNoCORS(t *testing.T) {
	e := wtNew(t)
	for _, m := range []string{"PUT", "DELETE", "PATCH"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(m, "/api/projects/demo", nil)
		req.Host = "127.0.0.1:7331"
		e.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != 405 {
			t.Errorf("%s = %d, want 405", m, rec.Code)
		}
	}
	// A GET must not be able to mutate, even with the right headers.
	req := httptest.NewRequest("GET", "/api/projects/demo/tasks/T-3/cancel", nil)
	req.Host = "127.0.0.1:7331"
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Error("GET on an action endpoint must not succeed")
	}
	// No CORS headers: cross-origin pages cannot read responses.
	req = httptest.NewRequest("GET", "/api/projects", nil)
	req.Host = "127.0.0.1:7331"
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("API must not send CORS headers")
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := wtNew(t)
	for _, p := range []string{"/", "/api/projects", "/static/app.js", "/nope"} {
		req := httptest.NewRequest("GET", p, nil)
		req.Host = "127.0.0.1:7331"
		rec := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, req)
		h := rec.Header()
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q lacks %q", p, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: CSP allows unsafe script/style: %s", p, csp)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: missing hardening headers: %v", p, h)
		}
	}
	// API and index responses are not cached.
	for _, p := range []string{"/", "/api/projects"} {
		req := httptest.NewRequest("GET", p, nil)
		req.Host = "127.0.0.1:7331"
		rec := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, req)
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s Cache-Control = %q", p, rec.Header().Get("Cache-Control"))
		}
	}
}

func TestTokenIsRandomPerServer(t *testing.T) {
	a, b := wtNew(t), wtNew(t)
	if a.srv.Token() == "" || a.srv.Token() == b.srv.Token() || len(a.srv.Token()) < 32 {
		t.Fatalf("tokens: %q %q", a.srv.Token(), b.srv.Token())
	}
}
