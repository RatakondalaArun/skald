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
