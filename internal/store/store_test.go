package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// openFiles is how many files this process has open.
func openFiles(t *testing.T) int {
	t.Helper()
	dir, err := os.Open("/dev/fd")
	if err != nil {
		t.Skipf("can't count open files: %v", err)
	}
	defer dir.Close()
	fds, err := dir.Readdirnames(-1)
	if err != nil {
		t.Skipf("can't count open files: %v", err)
	}
	return len(fds)
}

// While another process holds the lock, as a sync does while it waits on
// herdr, LoadContext gives up when its context ends, and leaves nothing
// waiting on the lock: no goroutine and no open file.
func TestLoadContextGivesUpOnAHeldLock(t *testing.T) {
	s := &Store{Dir: t.TempDir(), Session: "acme"}
	err := s.Update(func(st *State) error {
		st.Tasks["w1:t1"] = &task.Task{TabID: "w1:t1", Title: "ABC-12 checkout"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A lock of its own: flock locks belong to an open file, not a process.
	held, err := os.OpenFile(s.lockPath(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	before := openFiles(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.LoadContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("LoadContext with the lock held = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("giving up took %v", d)
	}
	if after := openFiles(t); after != before {
		t.Errorf("%d files open after giving up, want %d", after, before)
	}
	gone, stop := context.WithCancel(context.Background())
	stop()
	if _, err := s.LoadContext(gone); !errors.Is(err, context.Canceled) {
		t.Errorf("LoadContext with an ended context = %v, want Canceled", err)
	}

	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	st, err := s.LoadContext(context.Background())
	if err != nil || st.Tasks["w1:t1"] == nil || st.Tasks["w1:t1"].Title != "ABC-12 checkout" {
		t.Fatalf("LoadContext once the lock is free = %+v, %v; want the task", st, err)
	}
	// It let go of its shared lock: a writer gets the lock at once.
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Errorf("after LoadContext, the lock is still taken: %v", err)
	}
}

// The state holds notes and the share keys of artifact links, so only you
// can read it, including state an older version left readable by others.
func TestStateIsYoursOnly(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "state"), Session: "acme"}
	// As an older version left it.
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.Path(), s.lockPath()} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	err := s.Update(func(st *State) error {
		st.Tasks["w1:t1"] = &task.Task{TabID: "w1:t1", Note: "see https://claude.ai/artifact/00000000-0000-4000-8000-000000000001?sk=abc"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]os.FileMode{s.Dir: 0o700, s.Path(): 0o600, s.lockPath(): 0o600}
	for p, want := range modes {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s is %o, want %o", filepath.Base(p), got, want)
		}
	}
	// A new folder is yours only too, and no temp file is left behind.
	fresh := &Store{Dir: filepath.Join(t.TempDir(), "new"), Session: "acme"}
	if err := fresh.Update(func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{fresh.Dir: 0o700, fresh.Path(): 0o600} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("new %s is %o, want %o", filepath.Base(p), got, want)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(s.Dir, "*.tmp")); len(left) > 0 {
		t.Errorf("temp files left: %q", left)
	}
}
