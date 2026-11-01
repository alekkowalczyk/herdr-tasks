package slack

import (
	"log"
	"os"
	"path/filepath"
	"sync"
)

// maxLog is how big slack.log grows before it's moved to slack.log.1.
const maxLog = 1 << 20

// OpenLog is the watcher's log, <dir>/slack.log, readable by you only. The
// file is made with its first line. A line that would take it over 1 MiB
// first moves it to slack.log.1, in place of the one there.
func OpenLog(dir string) *log.Logger {
	return log.New(&logFile{path: filepath.Join(dir, "slack.log")}, "", log.LstdFlags)
}

// logFile appends to a file, and moves it aside when it's full.
type logFile struct {
	path string
	mu   sync.Mutex
	f    *os.File
	size int64
}

func (l *logFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		if err := l.open(); err != nil {
			return 0, err
		}
	}
	if l.size > 0 && l.size+int64(len(p)) > maxLog {
		l.f.Close()
		l.f = nil
		if err := os.Rename(l.path, l.path+".1"); err != nil {
			return 0, err
		}
		if err := l.open(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *logFile) open() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_ = f.Chmod(0o600)
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}
