package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSecretPrefersFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSecret("env-token", path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "file-token" {
		t.Fatalf("got %q, want file token", got)
	}
}

func TestReadSecretUsesValueWhenFileEmpty(t *testing.T) {
	got, err := ReadSecret("  env-token\n", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "env-token" {
		t.Fatalf("got %q, want env token", got)
	}
}

func TestReadSecretMissingFile(t *testing.T) {
	_, err := ReadSecret("env-token", filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("expected missing file error")
	}
	if strings.Contains(err.Error(), "env-token") {
		t.Fatalf("secret leaked in error: %v", err)
	}
}
