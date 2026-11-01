package slack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ErrChecking is a check that can't start because another runs.
var ErrChecking = errors.New("Slack is being checked right now; try again when that's done")

// lockRetry is how often a fetch that waits tries the lock again.
const lockRetry = 100 * time.Millisecond

// LockChecks takes the lock that keeps the watcher and fetches from checking
// at once, across processes: an exclusive flock on slack/check.lock in the
// state folder dir. The lock belongs to the open file, so two in one process
// exclude each other too. Without wait, a lock that's held is ErrChecking.
// With wait, it tries again every 100 ms until it gets the lock, or ctx ends,
// which is ErrChecking too. unlock releases it.
func LockChecks(ctx context.Context, dir string, wait bool) (unlock func(), err error) {
	folder := filepath.Join(dir, "slack")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(folder, 0o700)
	f, err := os.OpenFile(filepath.Join(folder, "check.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(0o600)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			// Once only: a second unlock could reach another file that took
			// the closed one's descriptor.
			var once sync.Once
			return func() {
				once.Do(func() {
					_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
					f.Close()
				})
			}, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case !errors.Is(err, syscall.EWOULDBLOCK):
			f.Close()
			return nil, err
		case !wait:
			f.Close()
			return nil, ErrChecking
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ErrChecking
		case <-time.After(lockRetry):
		}
	}
}
