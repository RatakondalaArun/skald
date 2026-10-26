// Package config resolves skald's on-disk locations.
package config

import (
	"os"
	"path/filepath"
)

// Home returns the skald data directory: $SKALD_HOME, else $HOME/.skald.
func Home() (string, error) {
	if h := os.Getenv("SKALD_HOME"); h != "" {
		return h, nil
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(u, ".skald"), nil
}

// DBPath returns the path of the SQLite database inside Home.
func DBPath() (string, error) {
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "skald.db"), nil
}

// ServeStatePath is where a background `skald serve` records its pid and URL.
func ServeStatePath() (string, error) { return inHome("serve.json") }

// ServeLogPath is where a background `skald serve` writes its output.
func ServeLogPath() (string, error) { return inHome("serve.log") }

// GitHubTokenPath is where `skald github login` saves its token.
func GitHubTokenPath() (string, error) { return inHome("github.json") }

func inHome(name string) (string, error) {
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, name), nil
}
