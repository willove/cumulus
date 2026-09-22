package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadValidatesListen(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("empty config must be refused (listen is required)")
	}
	if err := os.WriteFile(p, []byte(`{"listen":"127.0.0.1:0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
}
