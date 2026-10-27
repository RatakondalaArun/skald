package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultClientID is the client ID of Skald's GitHub OAuth App (device flow
// enabled), used by `skald github login`. The device flow needs no client
// secret, so the ID is safe to ship in the binary. SKALD_GITHUB_CLIENT_ID
// points Skald at another OAuth App, such as one on GitHub Enterprise.
var DefaultClientID = "Ov23lixZ4zcGp7s30SZ6"

// EnvClientID overrides DefaultClientID.
const EnvClientID = "SKALD_GITHUB_CLIENT_ID"

// LoginScopes are the scopes `skald github login` asks for: repo to read and
// close issues and create milestones, read:project for board status.
const LoginScopes = "repo read:project"

// ErrNoClientID is returned when no OAuth App client ID is configured.
var ErrNoClientID = errors.New("GitHub sign-in is not configured: set " + EnvClientID + " to the client ID of a GitHub OAuth App with device flow enabled, or use GITHUB_TOKEN / `gh auth login`")

// ClientID returns SKALD_GITHUB_CLIENT_ID, else DefaultClientID.
func ClientID() string {
	if v := strings.TrimSpace(os.Getenv(EnvClientID)); v != "" {
		return v
	}
	return DefaultClientID
}

// WebURL returns the web base (https://github.com, or a GitHub Enterprise
// host) for the REST base apiURL ("" means github.com).
func WebURL(apiURL string) string {
	apiURL = strings.TrimSuffix(apiURL, "/")
	if apiURL == "" || apiURL == DefaultAPIURL {
		return "https://github.com"
	}
	if base, ok := strings.CutSuffix(apiURL, "/api/v3"); ok {
		return base
	}
	return apiURL
}

// DeviceCode is the first step of the device flow: the user enters UserCode
// at VerificationURI while Skald polls with Code.
type DeviceCode struct {
	Code            string    `json:"device_code"`
	UserCode        string    `json:"user_code"`
	VerificationURI string    `json:"verification_uri"`
	ExpiresIn       int       `json:"expires_in"`
	Interval        int       `json:"interval"`
	Expires         time.Time `json:"-"`
}

// OAuth runs GitHub's OAuth device flow.
type OAuth struct {
	HTTP     *http.Client
	ClientID string
	WebURL   string // https://github.com or a GitHub Enterprise host
	// sleep waits between polls; tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
}

// NewOAuth returns a device flow for the client ID and the REST base apiURL.
func NewOAuth(clientID, apiURL string) *OAuth {
	return &OAuth{HTTP: &http.Client{Timeout: 30 * time.Second}, ClientID: clientID, WebURL: WebURL(apiURL), sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (o *OAuth) post(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.WebURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "skald")
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return &APIError{Message: err.Error()}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &APIError{Status: resp.StatusCode, Message: err.Error()}
	}
	if resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(b))}
	}
	return json.Unmarshal(b, out)
}

// oauthError is GitHub's error body for the device flow (sent with status 200).
type oauthError struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

func (e oauthError) err() error {
	switch e.Error {
	case "device_flow_disabled":
		return errors.New("device flow is not enabled for this GitHub OAuth App; enable it in the app's settings")
	case "incorrect_client_credentials":
		return errors.New("GitHub does not know this OAuth App client ID; check " + EnvClientID)
	case "expired_token":
		return errors.New("the sign-in code expired; run the sign-in again")
	case "access_denied":
		return errors.New("sign-in was cancelled on GitHub")
	}
	if e.Description != "" {
		return errors.New("GitHub sign-in failed: " + e.Description)
	}
	return errors.New("GitHub sign-in failed: " + e.Error)
}

// Start asks GitHub for a device code.
func (o *OAuth) Start(ctx context.Context) (DeviceCode, error) {
	if o.ClientID == "" {
		return DeviceCode{}, ErrNoClientID
	}
	var resp struct {
		DeviceCode
		oauthError
	}
	if err := o.post(ctx, "/login/device/code", url.Values{"client_id": {o.ClientID}, "scope": {LoginScopes}}, &resp); err != nil {
		return DeviceCode{}, err
	}
	if resp.Error != "" {
		return DeviceCode{}, resp.oauthError.err()
	}
	dc := resp.DeviceCode
	if dc.Code == "" || dc.UserCode == "" {
		return DeviceCode{}, errors.New("GitHub returned no device code")
	}
	if !strings.HasPrefix(dc.VerificationURI, "https://") && !strings.HasPrefix(dc.VerificationURI, "http://") {
		dc.VerificationURI = o.WebURL + "/login/device"
	}
	if dc.Interval <= 0 {
		dc.Interval = 5
	}
	if dc.ExpiresIn <= 0 {
		dc.ExpiresIn = 900
	}
	dc.Expires = time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	return dc, nil
}

// Wait polls until the user approves dc, then returns the access token. It
// stops when the code expires, the user cancels, or ctx is done.
func (o *OAuth) Wait(ctx context.Context, dc DeviceCode) (Token, error) {
	interval := time.Duration(dc.Interval) * time.Second
	form := url.Values{"client_id": {o.ClientID}, "device_code": {dc.Code}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}
	for {
		if err := o.sleep(ctx, interval); err != nil {
			return Token{}, err
		}
		if !dc.Expires.IsZero() && time.Now().After(dc.Expires) {
			return Token{}, oauthError{Error: "expired_token"}.err()
		}
		var resp struct {
			AccessToken string `json:"access_token"`
			Scope       string `json:"scope"`
			Interval    int    `json:"interval"`
			oauthError
		}
		if err := o.post(ctx, "/login/oauth/access_token", form, &resp); err != nil {
			return Token{}, err
		}
		switch resp.Error {
		case "":
			if resp.AccessToken == "" {
				return Token{}, errors.New("GitHub returned no access token")
			}
			return Token{Token: resp.AccessToken, Scopes: resp.Scope, Host: o.WebURL, CreatedAt: time.Now().UTC()}, nil
		case "authorization_pending":
		case "slow_down":
			if resp.Interval > 0 {
				interval = time.Duration(resp.Interval) * time.Second
			} else {
				interval += 5 * time.Second
			}
		default:
			return Token{}, resp.oauthError.err()
		}
	}
}

// Token is a token saved by `skald github login`.
type Token struct {
	Token     string    `json:"token"`
	Login     string    `json:"login,omitempty"`
	Scopes    string    `json:"scopes,omitempty"`
	Host      string    `json:"host,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// SaveToken writes t to path, readable by its owner only.
func SaveToken(path string, t Token) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// LoadToken reads the token saved at path. A missing file is (Token{}, nil).
func LoadToken(path string) (Token, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Token{}, nil
	}
	if err != nil {
		return Token{}, err
	}
	var t Token
	if err := json.Unmarshal(b, &t); err != nil {
		return Token{}, fmt.Errorf("%s is not a saved GitHub token (%v); run `skald github logout`", path, err)
	}
	return t, nil
}

// RemoveToken deletes the token saved at path. It reports whether there was one.
func RemoveToken(path string) (bool, error) {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// User returns the login of the token's owner.
func (c *Client) User(ctx context.Context) (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	if err := c.do(ctx, http.MethodGet, c.RESTURL+"/user", nil, &u); err != nil {
		return "", err
	}
	return u.Login, nil
}

// Finish waits for the user to approve dc, looks up who signed in through the
// REST base apiURL ("" means github.com), and saves the token at path.
func (o *OAuth) Finish(ctx context.Context, dc DeviceCode, apiURL, path string) (Token, error) {
	t, err := o.Wait(ctx, dc)
	if err != nil {
		return Token{}, err
	}
	if t.Login, err = New(t.Token, apiURL).User(ctx); err != nil {
		return Token{}, err
	}
	if err := SaveToken(path, t); err != nil {
		return Token{}, fmt.Errorf("save the GitHub token: %w", err)
	}
	return t, nil
}
