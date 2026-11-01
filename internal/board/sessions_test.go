package board

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeHerdr is a herdr server on a unix socket, with nothing in it. While
// wedged, it takes connections and reads requests but doesn't answer.
type fakeHerdr struct {
	mu   sync.Mutex
	free chan struct{} // closed while it answers
}

func startHerdr(t *testing.T, path string) *fakeHerdr {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeHerdr{free: make(chan struct{})}
	close(f.free)
	t.Cleanup(func() {
		ln.Close()
		f.unwedge()
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeHerdr) serve(conn net.Conn) {
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	f.mu.Lock()
	free := f.free
	f.mu.Unlock()
	<-free
	var req struct{ Method string }
	_ = json.Unmarshal(line, &req)
	if req.Method == "events.subscribe" {
		fmt.Fprintln(conn, `{"id":"herdr-tasks","result":{"type":"subscription_started"}}`)
		_, _ = io.Copy(io.Discard, conn) // until the board hangs up
		return
	}
	fmt.Fprintln(conn, `{"id":"herdr-tasks","result":{"workspaces":[],"tabs":[],"panes":[]}}`)
}

func (f *fakeHerdr) wedge() {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.free:
		f.free = make(chan struct{})
	default:
	}
}

func (f *fakeHerdr) unwedge() {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.free:
	default:
		close(f.free)
	}
}

func sessionNamed(b Board, name string) (Session, bool) {
	for _, s := range b.Sessions {
		if s.Name == name {
			return s, true
		}
	}
	return Session{}, false
}

// A herdr too busy to answer stays on the board, marked stale, and doesn't
// hold up finding the others.
func TestWedgedHerdrStaysOnTheBoard(t *testing.T) {
	// t.TempDir paths can pass the unix socket path limit on macOS.
	home, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	sock := func(name string) string {
		return filepath.Join(home, ".config", "herdr", "sessions", name, "herdr.sock")
	}

	b := startHerdr(t, sock("b"))
	ts := startServer(t, Config{Sessions: HerdrSessions()}, func(s *Server) {
		s.poll, s.timeout = 100*time.Millisecond, 300*time.Millisecond
	})
	// Before the server's cleanup, which waits for every read to finish.
	t.Cleanup(b.unwedge)
	boards := ts.stream(t)
	waitFor(t, boards, "session b", func(bd Board) bool {
		s, ok := sessionNamed(bd, "b")
		return ok && !s.Stale
	})

	b.wedge()
	waitFor(t, boards, "session b marked stale", func(bd Board) bool {
		s, ok := sessionNamed(bd, "b")
		return ok && s.Stale
	})
	start := time.Now()
	startHerdr(t, sock("c"))
	waitFor(t, boards, "session c", func(bd Board) bool {
		_, ok := sessionNamed(bd, "c")
		return ok
	})
	if d := time.Since(start); d > time.Second {
		t.Errorf("finding session c took %v while b was wedged", d)
	}
	// b stays for as long as it's wedged.
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		bd, raw := ts.board(t)
		if s, ok := sessionNamed(bd, "b"); !ok || !s.Stale {
			t.Fatalf("while b is wedged: %s", raw)
		}
	}

	b.unwedge()
	waitFor(t, boards, "session b answering again", func(bd Board) bool {
		s, ok := sessionNamed(bd, "b")
		return ok && !s.Stale
	})
}
