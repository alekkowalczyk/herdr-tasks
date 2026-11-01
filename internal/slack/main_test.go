package slack

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain puts a fake claude, Claude Code 2.1.316, first on PATH, so a
// test that starts research, and reads claude's version, never reaches the
// one you have installed. Tests that need another claude put their own on
// PATH.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fake-claude")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\necho '2.1.316 (Claude Code)'\n"), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
