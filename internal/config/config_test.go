package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHomeEnvOverride(t *testing.T) {
	t.Setenv("SKALD_HOME", "/tmp/custom-skald")
	h, err := Home()
	if err != nil || h != "/tmp/custom-skald" {
		t.Fatalf("Home() = %q, %v", h, err)
	}
	p, _ := DBPath()
	if p != "/tmp/custom-skald/skald.db" {
		t.Fatalf("DBPath() = %q", p)
	}
}

func TestHomeDefault(t *testing.T) {
	t.Setenv("SKALD_HOME", "")
	u, _ := os.UserHomeDir()
	h, err := Home()
	if err != nil || h != filepath.Join(u, ".skald") {
		t.Fatalf("Home() = %q, %v", h, err)
	}
}

func TestServePaths(t *testing.T) {
	t.Setenv("SKALD_HOME", "/tmp/custom-skald")
	if p, _ := ServeStatePath(); p != "/tmp/custom-skald/serve.json" {
		t.Fatalf("ServeStatePath() = %q", p)
	}
	if p, _ := ServeLogPath(); p != "/tmp/custom-skald/serve.log" {
		t.Fatalf("ServeLogPath() = %q", p)
	}
}
