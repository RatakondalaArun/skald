package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"skald/internal/config"
	"skald/internal/github"
)

func newGitHubLoginCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Sign in to GitHub in the browser",
		Long: `Signs in to GitHub with the device flow: Skald shows a one-time code, you
enter it at github.com/login/device, and the token is saved in the skald data
directory (github.json, readable by you only). It asks for the repo and
read:project scopes.

GITHUB_TOKEN and GH_TOKEN still win over the saved token; the GitHub CLI
(gh auth token) is used only when neither is set and you have not signed in.
For GitHub Enterprise set SKALD_GITHUB_API_URL=https://<host>/api/v3 first.`,
		Args: noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			e := a.plainEnv(c)
			ctx := c.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			path, err := config.GitHubTokenPath()
			if err != nil {
				return err
			}
			apiURL := os.Getenv(github.EnvAPIURL)
			o := github.NewOAuth(github.ClientID(), apiURL)
			dc, err := o.Start(ctx)
			if err != nil {
				return err
			}
			// The code goes to stderr so --json output stays one document.
			fmt.Fprintf(e.errw, "Open %s and enter the code %s\nWaiting for you to approve it on GitHub...\n", dc.VerificationURI, dc.UserCode)
			t, err := o.Finish(ctx, dc, apiURL, path)
			if err != nil {
				return err
			}
			_, source := github.ResolveToken()
			return e.emit(map[string]any{"login": t.Login, "scopes": t.Scopes, "host": t.Host, "token_used": source}, func(w io.Writer) {
				fmt.Fprintf(w, "signed in to GitHub as %s\n", t.Login)
				if source != github.SourceLogin {
					fmt.Fprintf(w, "note: %s is set and is used instead; unset it to use this sign-in\n", source)
				}
			})
		},
	}
}

func newGitHubLogoutCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Forget the token saved by skald github login",
		Long: `Deletes the token saved by 'skald github login'. GitHub keeps the
authorization until you revoke it at https://github.com/settings/applications.`,
		Args: noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			e := a.plainEnv(c)
			path, err := config.GitHubTokenPath()
			if err != nil {
				return err
			}
			removed, err := github.RemoveToken(path)
			if err != nil {
				return err
			}
			return e.emit(map[string]any{"signed_out": removed}, func(w io.Writer) {
				if !removed {
					fmt.Fprintln(w, "not signed in with skald github login")
					return
				}
				fmt.Fprintln(w, "signed out; revoke the authorization at https://github.com/settings/applications if you no longer need it")
			})
		},
	}
}

func newGitHubAuthStatusCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show which GitHub token a sync would use",
		Args:  noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			e := a.plainEnv(c)
			ctx := c.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			tok, source := github.ResolveToken()
			if tok == "" {
				_ = e.emit(map[string]any{"signed_in": false}, func(w io.Writer) {
					fmt.Fprintln(w, "no GitHub token: run `skald github login`, set GITHUB_TOKEN, or sign in with `gh auth login`")
				})
				return silentError{}
			}
			login, err := github.New(tok, os.Getenv(github.EnvAPIURL)).User(ctx)
			if err != nil {
				return err
			}
			return e.emit(map[string]any{"signed_in": true, "login": login, "token": source}, func(w io.Writer) {
				fmt.Fprintf(w, "signed in to GitHub as %s (token from %s)\n", login, source)
			})
		},
	}
}
