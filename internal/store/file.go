package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// File is one JSON file in the state folder, with the same lock and atomic
// write the session files use. Store is built on it.
type File struct {
	Dir  string
	Name string
}

// Path is the JSON file.
func (f File) Path() string { return filepath.Join(f.Dir, f.Name+".json") }

func (f File) lockPath() string { return filepath.Join(f.Dir, f.Name+".lock") }

// openLock opens the file's lock file. The state folder holds notes, the
// share keys of artifact links, and the board's token, so it's made yours
// only, as is the lock file: also when an older version or herdr made them
// readable by others. That part is best effort, as the lock works either way.
func (f File) openLock() (*os.File, error) {
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(f.Dir, 0o700)
	l, err := os.OpenFile(f.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	_ = l.Chmod(0o600)
	return l, nil
}

func (f File) lock(how int) (func(), error) {
	l, err := f.openLock()
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(l.Fd()), how); err != nil {
		l.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(l.Fd()), syscall.LOCK_UN)
		l.Close()
	}, nil
}

// lockShared takes a shared lock, but stops waiting when ctx ends.
func (f File) lockShared(ctx context.Context) (func(), error) {
	l, err := f.openLock()
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(l.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			l.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			l.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return func() {
		_ = syscall.Flock(int(l.Fd()), syscall.LOCK_UN)
		l.Close()
	}, nil
}

// read fills v from the file. A missing file leaves v as it is.
func (f File) read(v any) error {
	b, err := os.ReadFile(f.Path())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("reading %s: %w", f.Path(), err)
	}
	return nil
}

// write saves v as a new file, readable by you only (CreateTemp makes it
// 0600), that takes the old one's place. So a file an older version left
// readable by others is replaced with one that isn't.
func (f File) write(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	t, err := os.CreateTemp(f.Dir, f.Name+".*.tmp")
	if err != nil {
		return err
	}
	_, err = t.Write(append(b, '\n'))
	if cerr := t.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(t.Name(), f.Path())
	}
	if err != nil {
		os.Remove(t.Name())
	}
	return err
}

// Load reads the file into v under a shared lock. A missing file leaves v
// as it is.
func (f File) Load(v any) error {
	unlock, err := f.lock(syscall.LOCK_SH)
	if err != nil {
		return err
	}
	defer unlock()
	return f.read(v)
}

// Update reads the file into v, runs fn, and saves v, under an exclusive
// lock. Nothing is saved if fn returns an error.
func (f File) Update(v any, fn func() error) error {
	unlock, err := f.lock(syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlock()
	if err := f.read(v); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	return f.write(v)
}
