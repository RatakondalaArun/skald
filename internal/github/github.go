// Package github reads a repository's milestones, issues, linked pull
// requests, Projects board status and discussions from the GitHub API, and
// closes issues when Skald pushes status back. It knows nothing about Skald's
// database; internal/service maps what it returns onto projects, features and
// tasks.
//
// Network calls only happen when a user runs a sync. Nothing else in Skald
// talks to GitHub.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// DefaultAPIURL is the REST base for github.com.
const DefaultAPIURL = "https://api.github.com"

// Environment variables read by TokenFromEnv and NewFromEnv.
const (
	EnvToken   = "GITHUB_TOKEN"
	EnvGHToken = "GH_TOKEN"
	EnvAPIURL  = "SKALD_GITHUB_API_URL" // REST base, e.g. https://ghe.example.com/api/v3
)

// Repo names a repository as owner/name.
type Repo struct {
	Owner string
	Name  string
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// Key is the lower-cased owner/name used in idempotency keys, so the same
// repository spelled with different case maps to the same tasks.
func (r Repo) Key() string { return strings.ToLower(r.String()) }

var repoPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ParseRepo accepts "owner/name", a github.com URL, or a git remote URL
// (https or ssh, with or without .git).
func ParseRepo(s string) (Repo, error) {
	s = strings.TrimSpace(s)
	orig := s
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	switch {
	case strings.HasPrefix(s, "git@"):
		// git@github.com:owner/name
		if _, path, ok := strings.Cut(s, ":"); ok {
			s = path
		}
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil {
			return Repo{}, fmt.Errorf("%q is not a repository (%v)", orig, err)
		}
		s = strings.TrimPrefix(u.Path, "/")
	}
	owner, name, ok := strings.Cut(s, "/")
	if !ok || strings.Contains(name, "/") || !repoPart.MatchString(owner) || !repoPart.MatchString(name) {
		return Repo{}, fmt.Errorf("%q is not a repository; use owner/name, such as octocat/hello-world", orig)
	}
	return Repo{Owner: owner, Name: name}, nil
}

// OriginRepo reads the origin remote of the git checkout at dir.
func OriginRepo(dir string) (Repo, error) {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return Repo{}, fmt.Errorf("no git origin remote in %s", dir)
	}
	return ParseRepo(strings.TrimSpace(string(out)))
}

// TokenFromEnv returns GITHUB_TOKEN, then GH_TOKEN, then the token of the
// GitHub CLI (`gh auth token`) if it is installed and signed in.
func TokenFromEnv() string {
	for _, k := range []string{EnvToken, EnvGHToken} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	if out, err := exec.Command("gh", "auth", "token").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

// ErrNoToken is returned by NewFromEnv when no token can be found.
var ErrNoToken = errors.New("no GitHub token: set GITHUB_TOKEN (or GH_TOKEN), or sign in with `gh auth login`")

// Client talks to the GitHub REST and GraphQL APIs.
type Client struct {
	HTTP       *http.Client
	Token      string
	RESTURL    string // e.g. https://api.github.com
	GraphQLURL string // e.g. https://api.github.com/graphql
}

// New returns a client for the REST base apiURL ("" means github.com). For
// GitHub Enterprise pass https://<host>/api/v3; GraphQL is derived from it.
func New(token, apiURL string) *Client {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	apiURL = strings.TrimSuffix(apiURL, "/")
	gql := apiURL + "/graphql"
	if base, ok := strings.CutSuffix(apiURL, "/api/v3"); ok {
		gql = base + "/api/graphql"
	}
	return &Client{HTTP: &http.Client{Timeout: 60 * time.Second}, Token: token, RESTURL: apiURL, GraphQLURL: gql}
}

// NewFromEnv builds a client from TokenFromEnv and SKALD_GITHUB_API_URL.
func NewFromEnv() (*Client, error) {
	tok := TokenFromEnv()
	if tok == "" {
		return nil, ErrNoToken
	}
	return New(tok, os.Getenv(EnvAPIURL)), nil
}

// APIError is a failed GitHub call.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "GitHub rejected the token (401). Check GITHUB_TOKEN or run `gh auth login` again."
	case http.StatusForbidden:
		return "GitHub refused the request (403): " + e.Message + ". The token may lack the repo scope, or the rate limit was hit."
	case http.StatusNotFound:
		return "GitHub returned 404: " + e.Message + ". Check the repository name and that the token can read it."
	}
	if e.Status == 0 {
		return "GitHub: " + e.Message
	}
	return fmt.Sprintf("GitHub returned %d: %s", e.Status, e.Message)
}

func (c *Client) do(ctx context.Context, method, url string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "skald")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &APIError{Message: err.Error()}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return &APIError{Status: resp.StatusCode, Message: err.Error()}
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		return &APIError{Status: resp.StatusCode, Message: e.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// graphql runs one query. GraphQL errors (which arrive with status 200) are
// returned as an *APIError.
func (c *Client) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.do(ctx, http.MethodPost, c.GraphQLURL, map[string]any{"query": query, "variables": vars}, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		e := resp.Errors[0]
		status := 0
		if e.Type == "NOT_FOUND" {
			status = http.StatusNotFound
		}
		return &APIError{Status: status, Message: e.Message}
	}
	return json.Unmarshal(resp.Data, out)
}
