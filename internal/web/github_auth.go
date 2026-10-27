package web

import (
	"context"
	"net/http"
	"os"
	"sync"
	"time"

	"skald/internal/config"
	"skald/internal/github"
)

// ghLogin is the dashboard's GitHub sign-in: at most one device flow at a
// time, finished in the background while the page polls apiGitHubAuth.
type ghLogin struct {
	mu      sync.Mutex
	pending *github.DeviceCode
	err     string
	cancel  context.CancelFunc
}

type ghAuthDTO struct {
	Configured bool          `json:"configured"` // an OAuth App client ID is set
	SignedIn   bool          `json:"signed_in"`  // a sync would find a token
	Source     string        `json:"source,omitempty"`
	Login      string        `json:"login,omitempty"` // known for skald github login only
	Pending    *ghPendingDTO `json:"pending,omitempty"`
	Error      string        `json:"error,omitempty"` // why the last sign-in failed
}

type ghPendingDTO struct {
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresAt       string `json:"expires_at"`
}

func (s *Server) ghAuth() ghAuthDTO {
	d := ghAuthDTO{Configured: github.ClientID() != ""}
	tok, source := github.ResolveToken()
	d.SignedIn, d.Source = tok != "", source
	if source == github.SourceLogin {
		if path, err := config.GitHubTokenPath(); err == nil {
			if t, err := github.LoadToken(path); err == nil {
				d.Login = t.Login
			}
		}
	}
	s.gh.mu.Lock()
	defer s.gh.mu.Unlock()
	if dc := s.gh.pending; dc != nil {
		d.Pending = &ghPendingDTO{UserCode: dc.UserCode, VerificationURI: dc.VerificationURI, ExpiresAt: ts(dc.Expires)}
	}
	d.Error = s.gh.err
	return d
}

// apiGitHubAuth reports which GitHub token a sync would use and any sign-in
// in progress. It never returns the token.
func (s *Server) apiGitHubAuth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.ghAuth())
}

// apiGitHubLogin starts the device flow and finishes it in the background:
// the page shows the code, the user approves it on GitHub, and the token is
// saved like `skald github login` does.
func (s *Server) apiGitHubLogin(w http.ResponseWriter, r *http.Request) {
	path, err := config.GitHubTokenPath()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	apiURL := os.Getenv(github.EnvAPIURL)
	o := github.NewOAuth(github.ClientID(), apiURL)
	dc, err := o.Start(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "github", err.Error())
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), dc.Expires.Add(time.Minute))
	s.gh.mu.Lock()
	if s.gh.cancel != nil {
		s.gh.cancel()
	}
	s.gh.pending, s.gh.err, s.gh.cancel = &dc, "", cancel
	s.gh.mu.Unlock()
	go func() {
		defer cancel()
		_, err := o.Finish(ctx, dc, apiURL, path)
		s.gh.mu.Lock()
		defer s.gh.mu.Unlock()
		if s.gh.pending != &dc {
			return // replaced by a newer sign-in or a sign-out
		}
		s.gh.pending, s.gh.cancel = nil, nil
		if err != nil {
			s.gh.err = err.Error()
			s.log.Warn("GitHub sign-in failed", "err", err)
		}
	}()
	writeJSON(w, http.StatusOK, s.ghAuth())
}

// apiGitHubLogout forgets the saved token and stops a sign-in in progress.
func (s *Server) apiGitHubLogout(w http.ResponseWriter, r *http.Request) {
	s.gh.mu.Lock()
	if s.gh.cancel != nil {
		s.gh.cancel()
	}
	s.gh.pending, s.gh.err, s.gh.cancel = nil, "", nil
	s.gh.mu.Unlock()
	path, err := config.GitHubTokenPath()
	if err == nil {
		_, err = github.RemoveToken(path)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.ghAuth())
}
