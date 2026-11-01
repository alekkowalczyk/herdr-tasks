package slack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockChecksExcludes(t *testing.T) {
	dir := t.TempDir()
	unlock, err := LockChecks(context.Background(), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockChecks(context.Background(), dir, false); !errors.Is(err, ErrChecking) {
		t.Errorf("a second lock = %v, want ErrChecking", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := LockChecks(ctx, dir, true); !errors.Is(err, ErrChecking) {
		t.Errorf("waiting past the context = %v, want ErrChecking", err)
	}
	if waited := time.Since(start); waited < 250*time.Millisecond {
		t.Errorf("gave up after %v, want it to wait for the context's 300 ms", waited)
	}
	if ErrChecking.Error() != "Slack is being checked right now; try again when that's done" {
		t.Errorf("ErrChecking = %q", ErrChecking)
	}

	// The lock file and its folder are yours only.
	for path, want := range map[string]os.FileMode{
		filepath.Join(dir, "slack"):               0o700 | os.ModeDir,
		filepath.Join(dir, "slack", "check.lock"): 0o600,
	} {
		if fi, err := os.Stat(path); err != nil {
			t.Error(err)
		} else if fi.Mode() != want {
			t.Errorf("%s is %v, want %v", path, fi.Mode(), want)
		}
	}

	unlock()
	again, err := LockChecks(context.Background(), dir, false)
	if err != nil {
		t.Fatalf("after unlock = %v, want the lock", err)
	}
	again()
}

func TestLockChecksWaits(t *testing.T) {
	dir := t.TempDir()
	unlock, err := LockChecks(context.Background(), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(150*time.Millisecond, unlock)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	got, err := LockChecks(ctx, dir, true)
	if err != nil {
		t.Fatalf("LockChecks = %v, want the lock once it's released", err)
	}
	defer got()
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Errorf("got the lock after %v, while it was held", waited)
	}
}
