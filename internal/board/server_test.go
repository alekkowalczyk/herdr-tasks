package board

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/assistant"
	"github.com/alekkowalczyk/herdr-tasks/internal/claudelog"
	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/linear"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/slack"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// fakeSessions are the herdr sessions a test server finds running.
type fakeSessions struct {
	mu    sync.Mutex
	conns []*fakeConn
}

func running(conns ...*fakeConn) *fakeSessions { return &fakeSessions{conns: conns} }

func (f *fakeSessions) List() []SessionConn {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []SessionConn
	for _, c := range f.conns {
		out = append(out, c)
	}
	return out
}

func (f *fakeSessions) set(conns ...*fakeConn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns = conns
}

// fakeConn is one herdr session: its tabs and panes in memory, and its tasks
// in a store of its own.
type fakeConn struct {
	name   string
	st     *store.Store
	events chan herdr.Event // what Subscribe streams
	stuck  chan struct{}    // when set, Live waits until it's closed
	slowOp time.Duration    // how long each op typed into a pane takes

	mu      sync.Mutex
	live    *herdr.Live
	panes   []herdr.Pane
	focused []string
	ended   bool
	down    bool // herdr doesn't answer: Panes and the terminals' calls fail

	// Terminals: each tab's layout, what each pane shows, how many times
	// each pane was read, and what was typed, as "<pane> <op> <value>".
	layouts map[string]*herdr.TabLayout
	screens map[string]string
	reads   map[string]int
	typed   []string

	// Moves and Slack actions: what was asked, as "<action> <tab>" and a
	// move's workspace; the tab and error Move answers; and, when hold is
	// set, Research waits until it's closed.
	asked   []string
	newTab  string
	moveErr error
	hold    chan struct{}
	// What Assistant answers: its tab, and an error.
	assistantTab string
	assistantErr error
}

func newConn(t *testing.T, name string, live *herdr.Live, tasks ...*task.Task) *fakeConn {
	t.Helper()
	c := &fakeConn{name: name, st: &store.Store{Dir: t.TempDir(), Session: name}, events: make(chan herdr.Event, 16), live: live}
	err := c.st.Update(func(s *store.State) error {
		for _, tk := range tasks {
			s.Tasks[tk.TabID] = tk
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *fakeConn) Name() string { return c.name }

func (c *fakeConn) Ended() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ended
}

// end has the session end, as when herdr removes its socket.
func (c *fakeConn) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ended = true
}

func (c *fakeConn) Live() (*herdr.Live, error) {
	if c.stuck != nil {
		<-c.stuck
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live, nil
}

func (c *fakeConn) Panes() ([]herdr.Pane, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return nil, errDown
	}
	return c.panes, nil
}

// errDown is what a call to a herdr that doesn't answer returns.
var errDown = errors.New("dial unix herdr.sock: connect: no such file or directory")

func (c *fakeConn) Store() *store.Store { return c.st }

func (c *fakeConn) Change(tabID string, fn func(*task.Task) error) error {
	return c.st.Update(func(s *store.State) error {
		t, ok := s.Tasks[tabID]
		if !ok {
			return fmt.Errorf("%w %s", reconcile.ErrNoTab, tabID)
		}
		return fn(t)
	})
}

func (c *fakeConn) Move(tabID, workspace string) (string, error) {
	c.ask("move " + tabID + " " + workspace)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.newTab, c.moveErr
}

func (c *fakeConn) Research(tabID string) error {
	c.ask("research " + tabID)
	if c.hold != nil {
		<-c.hold
	}
	return nil
}

func (c *fakeConn) Continue(tabID string) error {
	c.ask("continue " + tabID)
	return nil
}

func (c *fakeConn) Assistant(about string) (string, error) {
	c.ask("assistant " + about)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.assistantTab, c.assistantErr
}

func (c *fakeConn) ask(what string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, what)
}

// actions is what Move, Research, Continue and ZoomPane were asked.
func (c *fakeConn) actions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.asked)
}

func (c *fakeConn) Focus(tabID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.focused = append(c.focused, tabID)
	return nil
}

func (c *fakeConn) Subscribe(ctx context.Context, fn func(herdr.Event)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-c.events:
			fn(ev)
		}
	}
}

// save changes a stored task behind the board's back, as a strip or the
// CLI would.
func (c *fakeConn) save(t *testing.T, tabID string, fn func(*task.Task)) {
	t.Helper()
	if err := c.st.Update(func(s *store.State) error { fn(s.Tasks[tabID]); return nil }); err != nil {
		t.Fatal(err)
	}
}

// nudge sends a herdr event.
func (c *fakeConn) nudge() { c.events <- herdr.Event{Event: "tab_renamed"} }

// stored is the task as saved.
func (c *fakeConn) stored(t *testing.T, tabID string) *task.Task {
	t.Helper()
	s, err := c.st.Load()
	if err != nil {
		t.Fatal(err)
	}
	tk, ok := s.Tasks[tabID]
	if !ok {
		t.Fatalf("no task for %s", tabID)
	}
	return tk
}

// oneSpace is herdr with one space, "acme", holding tabs.
func oneSpace(tabs ...herdr.Tab) *herdr.Live {
	for i := range tabs {
		tabs[i].WorkspaceID = "w1"
	}
	return &herdr.Live{Workspaces: []herdr.Workspace{{ID: "w1", Label: "acme"}}, Tabs: tabs}
}

// testServer is a board server running in the test.
type testServer struct {
	*Server
	port    int
	key     string // the browser key
	stopped chan struct{}
	err     error // what Run returned, once stopped is closed

	mu   sync.Mutex
	opts reconcile.Options
}

var plain = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second}

// startServer runs a server with cfg, timed for tests: sessions are read
// once at the start and then only when something asks. Unless cfg says
// otherwise, it's in a temp state dir, and the board is on, at the port the
// server listens on, until the test says otherwise. tune may change the
// timings before it starts.
func startServer(t *testing.T, cfg Config, tune func(*Server)) *testServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{port: ln.Addr().(*net.TCPAddr).Port, stopped: make(chan struct{})}
	auth := NewAuth(testToken)
	ts.key = auth.key
	if cfg.StateDir == "" {
		cfg.StateDir = t.TempDir()
	}
	if cfg.Port == 0 {
		cfg.Port = ts.port
	}
	cfg.Version = testVersion
	ts.opts = reconcile.Options{Board: true, BoardPort: cfg.Port}
	if cfg.Options == nil {
		cfg.Options = func() (reconcile.Options, error) {
			ts.mu.Lock()
			defer ts.mu.Unlock()
			return ts.opts, nil
		}
	}
	ts.Server = NewServer(cfg, auth, nil)
	ts.tick, ts.poll, ts.quiet, ts.timeout, ts.retry = 50*time.Millisecond, time.Hour, 20*time.Millisecond, time.Second, 50*time.Millisecond
	if tune != nil {
		tune(ts.Server)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ts.err = ts.Run(ctx, ln)
		close(ts.stopped)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-ts.stopped:
		case <-time.After(5 * time.Second):
			t.Error("the server didn't stop")
			return
		}
		// Reads still under way use stores in the test's temp dirs, which
		// go next. Reads start under the hub's lock, and none starts once
		// the server has stopped, so after taking the lock all are counted.
		settled := make(chan struct{})
		go func() {
			ts.hub.mu.Lock()
			ts.hub.mu.Unlock()
			ts.hub.reads.Wait()
			close(settled)
		}()
		select {
		case <-settled:
		case <-time.After(5 * time.Second):
			t.Error("a session read never finished")
		}
	})
	return ts
}

func (ts *testServer) setOptions(fn func(*reconcile.Options)) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	fn(&ts.opts)
}

func (ts *testServer) url(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", ts.port, path)
}

// post sends body to /api/task, and returns the status and the error text.
func (ts *testServer) post(t *testing.T, body string) (int, string) {
	t.Helper()
	return ts.postTo(t, "/api/task", body)
}

// postTo sends body to path with the browser key, and returns the status
// and the error text.
func (ts *testServer) postTo(t *testing.T, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", ts.url(path), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ts.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := plain.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var reply struct{ Error string }
	b, _ := io.ReadAll(resp.Body)
	if len(b) > 0 {
		if err := json.Unmarshal(b, &reply); err != nil {
			t.Fatalf("answer %d %q: %v", resp.StatusCode, b, err)
		}
	}
	return resp.StatusCode, reply.Error
}

// act asks for a task action.
func (ts *testServer) act(t *testing.T, req map[string]any) (int, string) {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return ts.post(t, string(b))
}

// board gets /api/board.
func (ts *testServer) board(t *testing.T) (Board, []byte) {
	t.Helper()
	var list SessionList
	ts.getJSON(t, "/api/board", &list)
	all := Board{Version: list.Version, Sessions: []Session{}}
	for _, e := range list.Sessions {
		var one Board
		ts.getJSON(t, "/api/board?session="+url.QueryEscape(e.Name), &one)
		all.LinearKey = one.LinearKey
		all.SlackSession = one.SlackSession
		all.SlackWaiting = one.SlackWaiting
		if one.Slack != nil {
			all.Slack = one.Slack
		}
		all.Sessions = append(all.Sessions, one.Sessions...)
	}
	raw, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	return all, raw
}

// getJSON reads the JSON at path into v, signed in.
func (ts *testServer) getJSON(t *testing.T, path string, v any) {
	t.Helper()
	req, err := http.NewRequest("GET", ts.url(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ts.key)
	resp, err := plain.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%s: %s %q %s", path, resp.Status, resp.Header.Get("Content-Type"), raw)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s %s: %v", path, raw, err)
	}
}

// shows waits up to 3 s for /api/board to have a session's card, as it
// does once the session has been found and read.
func (ts *testServer) shows(t *testing.T, session, tab string) (Board, []byte) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		b, raw := ts.board(t)
		if hasCard(session, tab)(b) {
			return b, raw
		}
		if time.Now().After(deadline) {
			t.Fatalf("no card %s %s within 3s: %s", session, tab, raw)
		}
	}
}

// sseEvent is one event a stream sent.
type sseEvent struct{ name, data string }

// sse opens the stream at path and passes on each event it sends, until the
// stream ends, the func it returns is called, or the test ends.
func (ts *testServer) sse(t *testing.T, path string) (<-chan sseEvent, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", ts.url(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ts.key)
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stop := func() {
		cancel()
		resp.Body.Close()
	}
	t.Cleanup(stop)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%s: %s %q", path, resp.Status, resp.Header.Get("Content-Type"))
	}
	events := make(chan sseEvent, 100)
	go func() {
		defer close(events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.name != "" {
					events <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return events, stop
}

// stream passes on the whole board each time it changes, every session in
// it, as the hub has it. The page's own streams are one session's.
func (ts *testServer) stream(t *testing.T) <-chan Board {
	t.Helper()
	wake, leave := ts.hub.join()
	boards := make(chan Board, 100)
	done := make(chan struct{})
	t.Cleanup(func() { close(done); leave() })
	go func() {
		defer close(boards)
		var sent uint64
		for {
			if p := ts.hub.current(); p.seq != sent {
				sent = p.seq
				boards <- p.board
			}
			select {
			case <-wake:
			case <-done:
				return
			}
		}
	}()
	return boards
}

// waitFor reads boards until one passes ok, for up to 3 s.
func waitFor(t *testing.T, boards <-chan Board, what string, ok func(Board) bool) Board {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case b, open := <-boards:
			if !open {
				t.Fatalf("the stream ended before %s", what)
			}
			if ok(b) {
				return b
			}
		case <-deadline:
			t.Fatalf("no board with %s within 3s", what)
		}
	}
}

func findCard(b Board, session, tab string) (Card, bool) {
	for _, s := range b.Sessions {
		for _, c := range s.Cards {
			if s.Name == session && c.Tab == tab {
				return c, true
			}
		}
	}
	return Card{}, false
}

func hasCard(session, tab string) func(Board) bool {
	return func(b Board) bool {
		_, ok := findCard(b, session, tab)
		return ok
	}
}

func writeLog(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBoardEndpointReturnsThePublishedBoard(t *testing.T) {
	const sid = "0b5e1c2a-7d3f-4e8a-9c61-2f4d8a1b3c5e"
	claude := t.TempDir()
	writeLog(t, filepath.Join(claude, "projects", "-home-acme-web", sid+".jsonl"),
		`{"type":"ai-title","aiTitle":"Fixing the login page"}`,
		`{"type":"system","subtype":"away_summary","content":"You're fixing the login page. Next: the tests.","timestamp":"2026-10-31T08:30:00Z"}`)
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1", AgentStatus: "working"}),
		&task.Task{TabID: "w1:t1", Title: "Login page", Stage: task.Active, Since: now,
			Links: []task.Link{{URL: "https://linear.app/acme/issue/ABC-12/fix-the-login-page", Added: now}}})
	work.panes = []herdr.Pane{{ID: "w1:p1", TabID: "w1:t1", Agent: "claude", AgentStatus: "working",
		Session: &herdr.AgentSession{Agent: "claude", Kind: "id", Value: sid}}}
	ts := startServer(t, Config{Sessions: running(work), Claude: claudelog.NewCache(claude)}, nil)

	b, raw := ts.shows(t, "work", "w1:t1")
	if b.Version != testVersion || b.LinearKey || len(b.Sessions) != 1 {
		t.Fatalf("board = %s", raw)
	}
	c, _ := findCard(b, "work", "w1:t1")
	if c.Title != "Login page" || c.Column != "running" || len(c.Links) != 1 || c.Links[0].Linear == nil || c.Links[0].Linear.ID != "ABC-12" {
		t.Fatalf("card = %+v", c)
	}
	if len(c.Agents) != 1 || c.Agents[0].Title != "Fixing the login page" || c.Agents[0].Recap != "You're fixing the login page. Next: the tests." {
		t.Fatalf("agents = %+v", c.Agents)
	}

	// The state file and the session log change, but nothing has the board
	// built again, so /api/board answers with the board already out.
	work.save(t, "w1:t1", func(tk *task.Task) { tk.Note = "Ship it" })
	f, err := os.OpenFile(filepath.Join(claude, "projects", "-home-acme-web", sid+".jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, `{"type":"system","subtype":"away_summary","content":"The tests pass.","timestamp":"2026-10-31T08:45:00Z"}`)
	f.Close()
	if _, again := ts.board(t); !bytes.Equal(again, raw) {
		t.Errorf("board changed without a build:\n%s\nwas\n%s", again, raw)
	}
}

// sessionStream opens the page's stream of one session's board, and passes
// on each board it sends.
func (ts *testServer) sessionStream(t *testing.T, session string) <-chan Board {
	t.Helper()
	events, _ := ts.sse(t, "/api/events?session="+url.QueryEscape(session))
	boards := make(chan Board, 100)
	go func() {
		defer close(boards)
		for ev := range events {
			if ev.name != "board" {
				t.Errorf("event %q on a session's stream", ev.name)
				return
			}
			var b Board
			if err := json.Unmarshal([]byte(ev.data), &b); err != nil {
				t.Errorf("board event %q: %v", ev.data, err)
				return
			}
			boards <- b
		}
	}()
	return boards
}

func TestEventsSendsBoardOnConnectAndOnChange(t *testing.T) {
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Title: "Login page", Stage: task.Active, Since: now})
	ts := startServer(t, Config{Sessions: running(work)}, nil)
	ts.shows(t, "work", "w1:t1")
	boards := ts.sessionStream(t, "work")
	select {
	case b := <-boards:
		// The session may not have been read yet.
		if !hasCard("work", "w1:t1")(b) {
			waitFor(t, boards, "the card", hasCard("work", "w1:t1"))
		}
	case <-time.After(time.Second):
		t.Fatal("no board on connect")
	}

	// A herdr event has the session read again.
	work.save(t, "w1:t1", func(tk *task.Task) { tk.Note = "Ship it" })
	work.nudge()
	waitFor(t, boards, "the note", func(b Board) bool {
		c, _ := findCard(b, "work", "w1:t1")
		return c.Note == "Ship it"
	})

	// Reading it again when nothing changed sends nothing.
	work.nudge()
	select {
	case b := <-boards:
		t.Errorf("a board came without a change: %+v", b)
	case <-time.After(500 * time.Millisecond):
	}

	// A page that connects now gets the board as it is.
	select {
	case b := <-ts.sessionStream(t, "work"):
		if c, _ := findCard(b, "work", "w1:t1"); c.Note != "Ship it" {
			t.Errorf("first board on a new stream = %+v", b)
		}
	case <-time.After(time.Second):
		t.Fatal("no board on connect")
	}
}

func TestTaskStageMovesAndKeepsReason(t *testing.T) {
	later := now.Add(time.Hour)
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1", AgentStatus: "working"}), &task.Task{TabID: "w1:t1", Stage: task.Active, Since: now})
	ts := startServer(t, Config{Sessions: running(work), Now: func() time.Time { return later }}, nil)
	boards := ts.stream(t)
	waitFor(t, boards, "the card", hasCard("work", "w1:t1"))
	move := func(stage, reason string) {
		t.Helper()
		code, msg := ts.act(t, map[string]any{"session": "work", "tab": "w1:t1", "action": "stage", "stage": stage, "reason": reason})
		if code != http.StatusNoContent {
			t.Fatalf("stage %s: %d %s", stage, code, msg)
		}
	}
	shows := func(column, reason string) {
		t.Helper()
		waitFor(t, boards, column+" "+reason, func(b Board) bool {
			c, _ := findCard(b, "work", "w1:t1")
			return c.Column == column && c.Reason == reason
		})
	}

	move("paused", "PR review")
	if tk := work.stored(t, "w1:t1"); tk.Stage != task.Paused || tk.Reason != "PR review" || !tk.Since.Equal(later) {
		t.Fatalf("after paused: %+v", tk)
	}
	// An action has the session read again, so the board shows it at once.
	shows("paused", "PR review")

	move("review", "")
	if tk := work.stored(t, "w1:t1"); tk.Stage != task.Review || tk.Reason != "PR review" {
		t.Fatalf("after review: %+v", tk)
	}
	shows("review", "PR review")

	move("done", "")
	if tk := work.stored(t, "w1:t1"); tk.Stage != task.Done || tk.Reason != "" {
		t.Fatalf("after done: %+v", tk)
	}
	shows("done", "")

	// Asking, Your turn and Working are all Active; the agent picks the column.
	for _, col := range []string{"asking", "yourturn", "running"} {
		move("parked", "")
		move(col, "")
		if tk := work.stored(t, "w1:t1"); tk.Stage != task.Active {
			t.Fatalf("after %s: %+v", col, tk)
		}
	}
	shows("running", "")
}

func TestTaskStageRejectsUnknownStage(t *testing.T) {
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Stage: task.Active, Since: now})
	ts := startServer(t, Config{Sessions: running(work)}, nil)
	ts.shows(t, "work", "w1:t1")
	for _, stage := range []string{"tool", "active", "Done", "", "later"} {
		code, msg := ts.act(t, map[string]any{"session": "work", "tab": "w1:t1", "action": "stage", "stage": stage})
		if code != http.StatusBadRequest || msg == "" {
			t.Errorf("stage %q: %d %q, want 400 with an error", stage, code, msg)
		}
	}
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"session":"work","tab":"w1:t1","action":"rename"}`, 400},
		{`{"session":"work","tab":"w1:t1"}`, 400},
		{`{"session":"work","action":"stage","stage":"done"}`, 400},
		{`not json`, 400},
		{`{"session":"home","tab":"w1:t1","action":"stage","stage":"done"}`, 404},
		{`{"session":"work","tab":"w1:t9","action":"stage","stage":"done"}`, 404},
	} {
		if code, msg := ts.post(t, c.body); code != c.want || msg == "" {
			t.Errorf("%s: %d %q, want %d with an error", c.body, code, msg, c.want)
		}
	}
	if tk := work.stored(t, "w1:t1"); tk.Stage != task.Active || !tk.Since.Equal(now) {
		t.Errorf("task changed: %+v", tk)
	}
}

func TestPriorityOnDoneIs409(t *testing.T) {
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}, herdr.Tab{ID: "w1:t2"}),
		&task.Task{TabID: "w1:t1", Stage: task.Done, Since: now},
		&task.Task{TabID: "w1:t2", Stage: task.Active, Since: now})
	ts := startServer(t, Config{Sessions: running(work)}, nil)
	ts.shows(t, "work", "w1:t1")
	code, msg := ts.act(t, map[string]any{"session": "work", "tab": "w1:t1", "action": "priority", "on": true})
	if code != http.StatusConflict || !strings.Contains(msg, "done") {
		t.Errorf("priority on a done task: %d %q, want 409", code, msg)
	}
	if work.stored(t, "w1:t1").Priority {
		t.Error("the done task is a priority")
	}
	if code, msg := ts.act(t, map[string]any{"session": "work", "tab": "w1:t2", "action": "priority", "on": true}); code != http.StatusNoContent {
		t.Fatalf("priority on: %d %s", code, msg)
	}
	if !work.stored(t, "w1:t2").Priority {
		t.Error("priority didn't turn on")
	}
	if code, msg := ts.act(t, map[string]any{"session": "work", "tab": "w1:t2", "action": "priority", "on": false}); code != http.StatusNoContent {
		t.Fatalf("priority off: %d %s", code, msg)
	}
	if work.stored(t, "w1:t2").Priority {
		t.Error("priority didn't turn off")
	}
}

func TestUnlinkDismissesLinearIssue(t *testing.T) {
	issue := "https://linear.app/acme/issue/ABC-12/fix-the-login-page"
	pr := "https://github.com/acme/web/pull/7"
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Stage: task.Active, Since: now,
		Links: []task.Link{{URL: pr, Added: now}, {URL: issue, By: "claude", Added: now}}})
	ts := startServer(t, Config{Sessions: running(work)}, nil)
	ts.shows(t, "work", "w1:t1")
	if code, msg := ts.act(t, map[string]any{"session": "work", "tab": "w1:t1", "action": "unlink", "url": issue}); code != http.StatusNoContent {
		t.Fatalf("unlink: %d %s", code, msg)
	}
	tk := work.stored(t, "w1:t1")
	if len(tk.Links) != 1 || tk.Links[0].URL != pr || !reflect.DeepEqual(tk.DismissedIssues, []string{"ABC-12"}) {
		t.Fatalf("after unlink: links %+v, dismissed %v", tk.Links, tk.DismissedIssues)
	}
	// The board names a link by its URL only, never by its place in a list
	// that may have changed.
	for _, url := range []string{issue, "1", "ABC-12", ""} {
		if code, _ := ts.act(t, map[string]any{"session": "work", "tab": "w1:t1", "action": "unlink", "url": url}); code != http.StatusNotFound {
			t.Errorf("unlink %q: %d, want 404", url, code)
		}
	}
	if tk := work.stored(t, "w1:t1"); len(tk.Links) != 1 {
		t.Errorf("links = %+v", tk.Links)
	}
}

func TestFocusShowsTheTabInHerdr(t *testing.T) {
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Stage: task.Active, Since: now})
	ts := startServer(t, Config{Sessions: running(work)}, nil)
	ts.shows(t, "work", "w1:t1")
	if code, msg := ts.act(t, map[string]any{"session": "work", "tab": "w1:t1", "action": "focus"}); code != http.StatusNoContent {
		t.Fatalf("focus: %d %s", code, msg)
	}
	work.mu.Lock()
	defer work.mu.Unlock()
	if !reflect.DeepEqual(work.focused, []string{"w1:t1"}) {
		t.Errorf("focused %v", work.focused)
	}
}

// actReply asks for a task action, and returns the status and the answer.
func (ts *testServer) actReply(t *testing.T, req map[string]any) (int, map[string]string) {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequest("POST", ts.url("/api/task"), bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+ts.key)
	r.Header.Set("Content-Type", "application/json")
	resp, err := plain.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	reply := map[string]string{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &reply); err != nil {
			t.Fatalf("answer %d %q: %v", resp.StatusCode, raw, err)
		}
	}
	return resp.StatusCode, reply
}

// slackTask is a task made from Dana Reed's message in #billing, with its
// research where research says.
func slackTask(tab string, research task.Research) *task.Task {
	a := &task.SlackAsk{Key: "C0123ABCD/1793450000.000100", Channel: "C0123ABCD", Place: "#billing", Author: "Dana Reed",
		TS: "1793450000.000100", At: now, Permalink: "https://acme.slack.com/archives/C0123ABCD/p1793450000000100", Research: research}
	if research == task.ResearchLocked || research == task.ResearchOpen {
		a.SessionID = "0b5e1c2a-7d3f-4e8a-9c61-2f4d8a1b3c5e"
	}
	return &task.Task{TabID: tab, Title: "Dana Reed: refund for ABC-12", Stage: task.Active, Since: now, Slack: a}
}

func TestMoveAction(t *testing.T) {
	live := &herdr.Live{
		Workspaces: []herdr.Workspace{{ID: "w1", Label: "billing"}, {ID: "w2", Label: "infra"}},
		Tabs:       []herdr.Tab{{ID: "w1:t1", WorkspaceID: "w1"}},
	}
	work := newConn(t, "work", live, &task.Task{TabID: "w1:t1", Stage: task.Active, Since: now})
	work.newTab = "w2:t4"
	ts := startServer(t, Config{Sessions: running(work)}, nil)
	ts.shows(t, "work", "w1:t1")
	move := map[string]any{"session": "work", "tab": "w1:t1", "action": "move", "workspace": "w2"}

	code, reply := ts.actReply(t, move)
	if code != http.StatusOK || !reflect.DeepEqual(reply, map[string]string{"tab": "w2:t4"}) {
		t.Fatalf("move: %d %v, want 200 with the new tab", code, reply)
	}
	if got := work.actions(); !reflect.DeepEqual(got, []string{"move w1:t1 w2"}) {
		t.Fatalf("asked %v", got)
	}

	// A move that went wrong partway answers with the new tab all the same,
	// so the page follows the card there.
	partway := "Moved 1 of 2 panes; the rest are still in the old tab: no pane w1:p2"
	work.mu.Lock()
	work.moveErr = errors.New(partway)
	work.mu.Unlock()
	code, reply = ts.actReply(t, move)
	if code != http.StatusInternalServerError || !reflect.DeepEqual(reply, map[string]string{"tab": "w2:t4", "error": partway}) {
		t.Errorf("move partway: %d %v", code, reply)
	}
	// One that moved nothing has no tab.
	work.mu.Lock()
	work.newTab, work.moveErr = "", errors.New(`No workspace "docs"; there are: billing, infra`)
	work.mu.Unlock()
	code, reply = ts.actReply(t, move)
	if code != http.StatusInternalServerError || !reflect.DeepEqual(reply, map[string]string{"error": `No workspace "docs"; there are: billing, infra`}) {
		t.Errorf("move nowhere: %d %v", code, reply)
	}

	// A move needs a workspace, and a task in the tab.
	for _, c := range []struct {
		req  map[string]any
		want int
	}{
		{map[string]any{"session": "work", "tab": "w1:t1", "action": "move"}, http.StatusBadRequest},
		{map[string]any{"session": "work", "tab": "w1:t1", "action": "move", "workspace": " "}, http.StatusBadRequest},
		{map[string]any{"session": "work", "tab": "w1:t9", "action": "move", "workspace": "w2"}, http.StatusNotFound},
	} {
		if code, msg := ts.act(t, c.req); code != c.want || msg == "" {
			t.Errorf("%v: %d %q, want %d with an error", c.req, code, msg, c.want)
		}
	}
	if got := work.actions(); len(got) != 3 {
		t.Errorf("asked %v, want three moves", got)
	}
}

func TestSlackActionsNeedASlackTask(t *testing.T) {
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Stage: task.Active, Since: now})
	ts := startServer(t, Config{Sessions: running(work)}, nil)
	ts.shows(t, "work", "w1:t1")
	for _, c := range []struct{ action, want string }{
		{"research", "Only a Slack task has research."},
		{"continue", "No locked research session in this tab."},
		{"mute", "Only a Slack task's channel can be muted."},
	} {
		if code, msg := ts.act(t, map[string]any{"session": "work", "tab": "w1:t1", "action": c.action}); code != http.StatusConflict || msg != c.want {
			t.Errorf("%s on a plain task: %d %q, want 409 %q", c.action, code, msg, c.want)
		}
		if code, msg := ts.act(t, map[string]any{"session": "work", "tab": "w1:t9", "action": c.action}); code != http.StatusNotFound || msg == "" {
			t.Errorf("%s on a tab without a task: %d %q, want 404", c.action, code, msg)
		}
	}
	if got := work.actions(); len(got) != 0 {
		t.Errorf("asked %v", got)
	}
	// Nothing was muted, and no card from Slack had the board read the
	// watcher's state.
	if _, err := os.Stat(filepath.Join(ts.cfg.StateDir, "slack")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the slack folder: %v, want none", err)
	}
}

// Research starts while it hasn't, or was held or failed, and continues
// while it's locked. Otherwise the answer is the strip's.
func TestResearchAndContinueOnASlackTask(t *testing.T) {
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}, herdr.Tab{ID: "w1:t2"}, herdr.Tab{ID: "w1:t3"}, herdr.Tab{ID: "w1:t4"}, herdr.Tab{ID: "w1:t5"}),
		slackTask("w1:t1", ""), slackTask("w1:t2", task.ResearchHeld), slackTask("w1:t3", task.ResearchFailed),
		slackTask("w1:t4", task.ResearchLocked), slackTask("w1:t5", task.ResearchOpen))
	ts := startServer(t, Config{Sessions: running(work)}, nil)
	ts.shows(t, "work", "w1:t1")
	for _, c := range []struct {
		action, tab string
		want        int
		msg         string
	}{
		{"research", "w1:t1", http.StatusNoContent, ""},
		{"research", "w1:t2", http.StatusNoContent, ""},
		{"research", "w1:t3", http.StatusNoContent, ""},
		{"research", "w1:t4", http.StatusConflict, "Research already started."},
		{"research", "w1:t5", http.StatusConflict, "Research already started."},
		{"continue", "w1:t4", http.StatusNoContent, ""},
		{"continue", "w1:t1", http.StatusConflict, "No locked research session in this tab."},
		{"continue", "w1:t5", http.StatusConflict, "No locked research session in this tab."},
	} {
		if code, msg := ts.act(t, map[string]any{"session": "work", "tab": c.tab, "action": c.action}); code != c.want || msg != c.msg {
			t.Errorf("%s %s: %d %q, want %d %q", c.action, c.tab, code, msg, c.want, c.msg)
		}
	}
	want := []string{"research w1:t1", "research w1:t2", "research w1:t3", "continue w1:t4"}
	if got := work.actions(); !reflect.DeepEqual(got, want) {
		t.Errorf("asked %v, want %v", got, want)
	}
}

// openAssistant posts body to /api/assistant.
func (ts *testServer) openAssistant(t *testing.T, body string) (int, assistantReply) {
	t.Helper()
	req, err := http.NewRequest("POST", ts.url("/api/assistant"), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ts.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := plain.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var reply assistantReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		t.Fatalf("answer %d: %v", resp.StatusCode, err)
	}
	return resp.StatusCode, reply
}

// The board opens the session's assistant, or asks it about a task, and
// answers with its tab. It answers with the tab when something went wrong
// there too, so the page shows it.
func TestAssistantOpensAndAsks(t *testing.T) {
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Stage: task.Active, Since: now})
	work.assistantTab = "w1:t2"
	ts := startServer(t, Config{Sessions: running(work)}, nil)
	ts.shows(t, "work", "w1:t1")

	if code, reply := ts.openAssistant(t, `{"session":"work"}`); code != http.StatusOK || reply != (assistantReply{Tab: "w1:t2"}) {
		t.Errorf("open: %d %+v", code, reply)
	}
	if code, reply := ts.openAssistant(t, `{"session":"work","about":"w1:t1"}`); code != http.StatusOK || reply.Tab != "w1:t2" {
		t.Errorf("ask: %d %+v", code, reply)
	}
	if want := []string{"assistant ", "assistant w1:t1"}; !slices.Equal(work.actions(), want) {
		t.Errorf("asked %q, want %q", work.actions(), want)
	}

	work.assistantErr = assistant.ErrAsking
	if code, reply := ts.openAssistant(t, `{"session":"work","about":"w1:t1"}`); code != http.StatusConflict || reply != (assistantReply{Tab: "w1:t2", Error: assistant.ErrAsking.Error()}) {
		t.Errorf("while Claude asks: %d %+v", code, reply)
	}
	work.assistantErr = errors.New("Claude didn't start in the assistant's tab: no shell")
	if code, reply := ts.openAssistant(t, `{"session":"work"}`); code != http.StatusInternalServerError || reply.Tab != "w1:t2" || reply.Error != work.assistantErr.Error() {
		t.Errorf("when Claude didn't start: %d %+v", code, reply)
	}
	work.assistantTab, work.assistantErr = "", fmt.Errorf("%w w1:t9", reconcile.ErrNoTab)
	if code, reply := ts.openAssistant(t, `{"session":"work","about":"w1:t9"}`); code != http.StatusNotFound || reply != (assistantReply{Error: "That tab is gone."}) {
		t.Errorf("about a tab that's gone: %d %+v", code, reply)
	}
	for body, want := range map[string]int{`{}`: 400, `ask it`: 400, `{"session":"home"}`: 404} {
		if code, reply := ts.openAssistant(t, body); code != want || reply.Error == "" || reply.Tab != "" {
			t.Errorf("%s: %d %+v, want %d with an error", body, code, reply, want)
		}
	}
	if n := len(work.actions()); n != 5 {
		t.Errorf("asked %d times, want 5", n)
	}
}

// Starting research waits for Claude to be ready, which can take minutes.
// The request waits as long as it takes, and the board goes on meanwhile.
func TestSlowResearchStartHoldsNothingUp(t *testing.T) {
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}, herdr.Tab{ID: "w1:t2"}),
		slackTask("w1:t1", ""), &task.Task{TabID: "w1:t2", Stage: task.Active, Since: now})
	work.hold = make(chan struct{})
	ts := startServer(t, Config{Sessions: running(work)}, nil)
	ts.shows(t, "work", "w1:t1")

	answered := make(chan int, 1)
	go func() {
		r, _ := http.NewRequest("POST", ts.url("/api/task"), strings.NewReader(`{"session":"work","tab":"w1:t1","action":"research"}`))
		r.Header.Set("Authorization", "Bearer "+ts.key)
		resp, err := plain.Do(r)
		if err != nil {
			answered <- 0
			return
		}
		resp.Body.Close()
		answered <- resp.StatusCode
	}()
	for deadline := time.Now().Add(3 * time.Second); !slices.Contains(work.actions(), "research w1:t1"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("research never started")
		}
	}
	// Longer than a session read may take.
	time.Sleep(ts.timeout + 100*time.Millisecond)
	if code, msg := ts.act(t, map[string]any{"session": "work", "tab": "w1:t2", "action": "priority", "on": true}); code != http.StatusNoContent {
		t.Fatalf("priority while research starts: %d %s", code, msg)
	}
	boards := ts.stream(t)
	waitFor(t, boards, "the priority", func(b Board) bool {
		c, _ := findCard(b, "work", "w1:t2")
		return c.Priority
	})
	select {
	case code := <-answered:
		t.Fatalf("research answered %d before it started", code)
	default:
	}
	close(work.hold)
	select {
	case code := <-answered:
		if code != http.StatusNoContent {
			t.Errorf("research: %d, want 204", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("research never answered")
	}
}

func TestMuteAction(t *testing.T) {
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}), slackTask("w1:t1", task.ResearchLocked))
	ts := startServer(t, Config{Sessions: running(work)}, nil)
	boards := ts.stream(t)
	muted := func(want bool) func(Board) bool {
		return func(b Board) bool {
			c, _ := findCard(b, "work", "w1:t1")
			return c.Slack != nil && c.Slack.Muted == want
		}
	}
	waitFor(t, boards, "the Slack card", muted(false))

	if code, msg := ts.act(t, map[string]any{"session": "work", "tab": "w1:t1", "action": "mute"}); code != http.StatusNoContent {
		t.Fatalf("mute: %d %s", code, msg)
	}
	st, err := slack.Load(ts.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []slack.Mute{{ID: "C0123ABCD", Name: "#billing"}}; !reflect.DeepEqual(st.Muted, want) {
		t.Fatalf("muted = %+v, want %+v", st.Muted, want)
	}
	waitFor(t, boards, "the card muted", muted(true))

	// Muting it again changes nothing.
	if code, msg := ts.act(t, map[string]any{"session": "work", "tab": "w1:t1", "action": "mute"}); code != http.StatusNoContent {
		t.Fatalf("mute again: %d %s", code, msg)
	}
	if st, _ := slack.Load(ts.cfg.StateDir); len(st.Muted) != 1 {
		t.Errorf("muted = %+v", st.Muted)
	}

	// Unmuted with the CLI, the card shows it once the board is built again.
	if err := slack.Update(ts.cfg.StateDir, func(s *slack.State) error { s.Unmute("C0123ABCD"); return nil }); err != nil {
		t.Fatal(err)
	}
	ts.Refresh()
	waitFor(t, boards, "the card unmuted", muted(false))
	if got := work.actions(); len(got) != 0 {
		t.Errorf("asked %v", got)
	}
}

// openLink asks the server to open a link of a task in session "work".
func (ts *testServer) openLink(t *testing.T, tab, url string) (int, string) {
	t.Helper()
	b, err := json.Marshal(map[string]string{"session": "work", "tab": tab, "url": url})
	if err != nil {
		t.Fatal(err)
	}
	return ts.postTo(t, "/api/open", string(b))
}

// writeFiles writes each file, by name, into a new temp dir, and returns
// their paths.
func writeFiles(t *testing.T, files map[string]string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	paths := map[string]string{}
	for name, text := range files {
		paths[name] = filepath.Join(dir, name)
		if err := os.WriteFile(paths[name], []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

func TestOpenOnlyOpensThisTasksPages(t *testing.T) {
	f := writeFiles(t, map[string]string{
		"plan.html":  "<!doctype html><title>Plan</title>",
		"other.html": "<!doctype html><title>Other</title>",
		"run.sh":     "echo hi",
		"id_acme":    "not a real key",
		"gone.html":  "<!doctype html><title>Gone</title>",
	})
	symlink := filepath.Join(filepath.Dir(f["id_acme"]), "key.html")
	if err := os.Symlink(f["id_acme"], symlink); err != nil {
		t.Fatal(err)
	}
	file := task.FileURL
	onAnotherHost := "file://nas" + f["plan.html"]
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}, herdr.Tab{ID: "w1:t2"}),
		&task.Task{TabID: "w1:t1", Stage: task.Active, Since: now, Links: []task.Link{
			{URL: file(f["plan.html"]), By: "claude", Added: now},
			{URL: file(f["run.sh"]), Added: now},
			{URL: onAnotherHost, Added: now},
			{URL: file(symlink), Added: now},
			{URL: file(f["gone.html"]), Added: now},
			{URL: "https://acme.github.io/demo/", Added: now},
		}},
		&task.Task{TabID: "w1:t2", Stage: task.Active, Since: now, Links: []task.Link{{URL: file(f["other.html"]), Added: now}}})
	if err := os.Remove(f["gone.html"]); err != nil {
		t.Fatal(err)
	}
	ts := startServer(t, Config{Sessions: running(work)}, nil)
	ts.shows(t, "work", "w1:t1")

	for _, c := range []struct {
		what, url string
		want      int
	}{
		{"another task's file", file(f["other.html"]), 404},
		{"another form of the page's URL", "file://localhost" + f["plan.html"], 404},
		{"a .sh file", file(f["run.sh"]), 400},
		{"a file on another host", onAnotherHost, 400},
		{"a web page", "https://acme.github.io/demo/", 400},
		{"a .html symlink to a key file", file(symlink), 404},
		{"a deleted file", file(f["gone.html"]), 404},
	} {
		if code, msg := ts.openLink(t, "w1:t1", c.url); code != c.want || msg == "" {
			t.Errorf("%s: %d %q, want %d with an error", c.what, code, msg, c.want)
		}
	}
	for _, c := range []struct {
		body string
		want int
	}{
		{`not json`, 400},
		{`{"session":"work","url":"` + file(f["plan.html"]) + `"}`, 400},
		{`{"session":"home","tab":"w1:t1","url":"` + file(f["plan.html"]) + `"}`, 404},
		{`{"session":"work","tab":"w1:t9","url":"` + file(f["plan.html"]) + `"}`, 404},
	} {
		if code, msg := ts.postTo(t, "/api/open", c.body); code != c.want || msg == "" {
			t.Errorf("%s: %d %q, want %d with an error", c.body, code, msg, c.want)
		}
	}

	if code, msg := ts.openLink(t, "w1:t1", file(f["plan.html"])); code != http.StatusOK {
		t.Fatalf("the task's page: %d %s", code, msg)
	}

	// It goes by the task as saved now, not as the board last read it.
	work.save(t, "w1:t1", func(tk *task.Task) { tk.Links = tk.Links[1:] })
	if code, _ := ts.openLink(t, "w1:t1", file(f["plan.html"])); code != http.StatusNotFound {
		t.Errorf("a page removed since the board read the task: %d, want 404", code)
	}
}

// A session whose tasks stay locked, as while a sync waits on herdr, gets
// the usual time to answer an open. The read ends with the request: none is
// left waiting on the lock, however often you click.
func TestOpenGivesUpOnALockedSession(t *testing.T) {
	page := writeFiles(t, map[string]string{"plan.html": "<!doctype html><title>Plan</title>"})["plan.html"]
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}),
		&task.Task{TabID: "w1:t1", Stage: task.Active, Since: now, Links: []task.Link{{URL: task.FileURL(page), Added: now}}})
	ts := startServer(t, Config{Sessions: running(work)}, func(s *Server) { s.timeout = 200 * time.Millisecond })
	ts.shows(t, "work", "w1:t1")

	held, err := os.OpenFile(filepath.Join(work.st.Dir, "work.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		start := time.Now()
		code, msg := ts.openLink(t, "w1:t1", task.FileURL(page))
		if code != http.StatusInternalServerError || msg != "Couldn't read the task: no answer within 200ms" {
			t.Errorf("open with the session locked: %d %q", code, msg)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("the open took %v", d)
		}
	}
	if n := waitingReads(); n > 0 {
		t.Errorf("%d reads are still waiting on the lock", n)
	}
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if code, msg := ts.openLink(t, "w1:t1", task.FileURL(page)); code != http.StatusOK || msg != "" {
		t.Errorf("open with the lock free: %d %q", code, msg)
	}
}

// waitingReads is how many goroutines are reading a store, other than the
// hub's reads.
func waitingReads() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	count := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "store.(*Store).Load") && !strings.Contains(g, "board.readSession") {
			count++
		}
	}
	return count
}

func TestOpenNeedsTheKey(t *testing.T) {
	page := writeFiles(t, map[string]string{"plan.html": "<!doctype html><title>Plan</title>"})["plan.html"]
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}),
		&task.Task{TabID: "w1:t1", Stage: task.Active, Since: now, Links: []task.Link{{URL: task.FileURL(page), Added: now}}})
	ts := startServer(t, Config{Sessions: running(work)}, nil)
	ts.shows(t, "work", "w1:t1")
	body, err := json.Marshal(map[string]string{"session": "work", "tab": "w1:t1", "url": task.FileURL(page)})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what   string
		header map[string]string
		want   int
	}{
		{"no key", nil, 401},
		{"a wrong key", map[string]string{"Authorization": "Bearer " + strings.Repeat("0", 64)}, 401},
		{"a foreign origin", map[string]string{"Authorization": "Bearer " + ts.key, "Origin": fmt.Sprintf("http://127.0.0.1:%d", ts.port+1)}, 403},
		{"a cross-site fetch", map[string]string{"Authorization": "Bearer " + ts.key, "Sec-Fetch-Site": "cross-site"}, 403},
	} {
		req, err := http.NewRequest("POST", ts.url("/api/open"), bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range c.header {
			req.Header.Set(k, v)
		}
		resp, err := plain.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.what, resp.StatusCode, c.want)
		}
	}
	if code, msg := ts.openLink(t, "w1:t1", task.FileURL(page)); code != http.StatusOK {
		t.Fatalf("with the key: %d %s", code, msg)
	}
}

func TestServerExitsWhenBoardTurnedOff(t *testing.T) {
	for name, change := range map[string]func(*reconcile.Options){
		"board off":    func(o *reconcile.Options) { o.Board = false },
		"another port": func(o *reconcile.Options) { o.BoardPort++ },
	} {
		t.Run(name, func(t *testing.T) {
			work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Stage: task.Active, Since: now})
			ts := startServer(t, Config{Sessions: running(work)}, nil)
			if got := status(t, ts.port, "/api/health", ts.key); got != http.StatusOK {
				t.Fatalf("health: %d", got)
			}
			ts.setOptions(change)
			select {
			case <-ts.stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("the server is still running")
			}
			if ts.err != nil {
				t.Errorf("Run = %v", ts.err)
			}
			if _, err := os.Stat(filepath.Join(ts.cfg.StateDir, "board", "board.json")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("board.json is still there: %v", err)
			}
		})
	}
}

func TestServerStaysUpForSlackAlone(t *testing.T) {
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Stage: task.Active, Since: now})
	ts := startServer(t, Config{Sessions: running(work)}, nil)
	ts.setOptions(func(o *reconcile.Options) { o.Board, o.SlackWatch = false, true })
	select {
	case <-ts.stopped:
		t.Fatal("the server stopped with the Slack watcher on")
	case <-time.After(500 * time.Millisecond): // 10 ticks
	}
	seen, problem := time.Now(), ""
	if why := ts.check(context.Background(), &seen, &problem); why != "" {
		t.Errorf("check = %q with the Slack watcher on", why)
	}

	ts.setOptions(func(o *reconcile.Options) { o.SlackWatch = false })
	if why := ts.check(context.Background(), &seen, &problem); why != "the board and the Slack watcher are turned off" {
		t.Errorf("check = %q", why)
	}
	select {
	case <-ts.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the server is still running with both turned off")
	}
}

func TestBoardCarriesTheSlackLine(t *testing.T) {
	var mu sync.Mutex
	status := slack.Status{On: true, Problem: true, Text: "Slack: can't reach Slack since 14:20. Run /mcp in Claude Code to sign in again"}
	setStatus := func(s slack.Status) {
		mu.Lock()
		defer mu.Unlock()
		status = s
	}
	// The Slack line is shown on the session Slack asks go to.
	ts := startServer(t, Config{Sessions: running(newConn(t, herdr.DefaultSession, oneSpace())), Slack: func() slack.Status {
		mu.Lock()
		defer mu.Unlock()
		return status
	}}, nil)
	boards := ts.stream(t)
	waitFor(t, boards, "the Slack line", func(b Board) bool { return b.Slack != nil })
	_, raw := ts.board(t)
	var got struct {
		Slack map[string]any `json:"slack"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Slack["text"] != status.Text || got.Slack["problem"] != true {
		t.Errorf("slack = %v in %s", got.Slack, raw)
	}

	// The watcher's status changes, and Refresh has the board show it.
	setStatus(slack.Status{On: true, Text: "Slack: last check 14:32"})
	ts.Refresh()
	waitFor(t, boards, "the new Slack line", func(b Board) bool {
		return b.Slack != nil && b.Slack.Text == "Slack: last check 14:32" && !b.Slack.Problem
	})

	// Turned off, the board has no Slack line.
	setStatus(slack.Status{Text: "Slack: off"})
	ts.Refresh()
	waitFor(t, boards, "no Slack line", func(b Board) bool { return b.Slack == nil })
	if _, raw := ts.board(t); bytes.Contains(raw, []byte(`"slack"`)) {
		t.Errorf("board = %s", raw)
	}
}

// slackPost posts body to path, with the browser key unless key is false,
// and with ctx, and returns the status and the answer, or why there's none.
func (ts *testServer) slackPost(ctx context.Context, path, body string, key bool) (int, map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", ts.url(path), strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if key {
		req.Header.Set("Authorization", "Bearer "+ts.key)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := plain.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	reply := map[string]string{}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return resp.StatusCode, nil, fmt.Errorf("answer %d %q: %v", resp.StatusCode, raw, err)
	}
	return resp.StatusCode, reply, nil
}

// postSlack is slackPost with the key, failing the test when there's no
// answer.
func (ts *testServer) postSlack(t *testing.T, path, body string) (int, map[string]string) {
	t.Helper()
	code, reply, err := ts.slackPost(context.Background(), path, body, true)
	if err != nil {
		t.Fatal(err)
	}
	return code, reply
}

// slackStatus is a Slack watcher's status that a test changes.
type slackStatus struct {
	mu sync.Mutex
	s  slack.Status
}

func (s *slackStatus) get() slack.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.s
}

func (s *slackStatus) set(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.s.Text = text
}

// slackText is whether a board's Slack line reads text.
func slackText(text string) func(Board) bool {
	return func(b Board) bool { return b.Slack != nil && b.Slack.Text == text }
}

// fetchResult is what a test's fetch gives, and what the watcher's status
// reads after it.
type fetchResult struct {
	text, status string
	err          error
}

// TestFetchEndpoint: Fetch runs one fetch at a time, on the server's context
// with 15 minutes to go, so it finishes when the page goes away. It answers
// with what the fetch did, and the board is built again.
func TestFetchEndpoint(t *testing.T) {
	status := &slackStatus{s: slack.Status{Text: "Slack: off", Fetch: true}}
	started := make(chan context.Context, 4)
	results := make(chan fetchResult)
	ended := make(chan error, 4) // the fetch's ctx.Err() as it ended
	fetch := func(ctx context.Context) (string, error) {
		started <- ctx
		select {
		case r := <-results:
			status.set(r.status)
			ended <- ctx.Err()
			return r.text, r.err
		case <-ctx.Done():
			ended <- ctx.Err()
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
			ended <- errors.New("never answered")
			return "", errors.New("never answered")
		}
	}
	ts := startServer(t, Config{Sessions: running(), Slack: status.get, SlackFetch: fetch}, nil)
	boards := ts.stream(t)
	waitFor(t, boards, "the Slack line", slackText("Slack: off"))

	// No key, no fetch.
	if code, _, err := ts.slackPost(context.Background(), "/api/slack/fetch", "{}", false); err != nil || code != http.StatusUnauthorized {
		t.Errorf("without the key: %d %v, want 401", code, err)
	}

	answered := make(chan map[string]string, 1)
	go func() {
		code, reply, err := ts.slackPost(context.Background(), "/api/slack/fetch", "{}", true)
		if err != nil || code != http.StatusOK {
			t.Errorf("fetch: %d %v %v, want 200", code, reply, err)
		}
		answered <- reply
	}()
	var ctx context.Context
	select {
	case ctx = <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the fetch never started")
	}
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) < 14*time.Minute || time.Until(dl) > 15*time.Minute {
		t.Errorf("the fetch's deadline: %v %v, want 15 minutes from now", dl, ok)
	}
	// A second, while the first runs, is turned away.
	if code, reply := ts.postSlack(t, "/api/slack/fetch", "{}"); code != http.StatusConflict || reply["error"] != "A fetch is already running." {
		t.Errorf("a second fetch: %d %v, want 409", code, reply)
	}
	summary := "Fetched from 08:30: 2 new tasks, 2 with research."
	results <- fetchResult{text: summary, status: "Slack: last check 09:01"}
	select {
	case reply := <-answered:
		if !reflect.DeepEqual(reply, map[string]string{"text": summary}) {
			t.Errorf("fetch answered %v", reply)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the fetch never answered")
	}
	if err := <-ended; err != nil {
		t.Errorf("the fetch ended with its context %v", err)
	}
	if len(started) != 0 {
		t.Errorf("%d more fetches started", len(started))
	}
	waitFor(t, boards, "the Slack line after the fetch", slackText("Slack: last check 09:01"))

	// The page goes away mid-fetch: the fetch goes on, and finishes. The
	// server sees a request go only once it has read its body, so this one
	// has none.
	rctx, cancel := context.WithCancel(context.Background())
	gone := make(chan error, 1)
	go func() {
		_, _, err := ts.slackPost(rctx, "/api/slack/fetch", "", true)
		gone <- err
	}()
	select {
	case ctx = <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the fetch never started")
	}
	cancel()
	if err := <-gone; err == nil {
		t.Fatal("the request wasn't canceled")
	}
	time.Sleep(200 * time.Millisecond) // for the server to see the page go
	if ctx.Err() != nil {
		t.Fatalf("the fetch's context ended with the request: %v", ctx.Err())
	}
	results <- fetchResult{text: summary, status: "Slack: last check 09:02"}
	if err := <-ended; err != nil {
		t.Errorf("the fetch ended with its context %v", err)
	}
	waitFor(t, boards, "the Slack line after the fetch", slackText("Slack: last check 09:02"))

	// A fetch that fails says why.
	go func() {
		<-started
		results <- fetchResult{err: errors.New("can't fetch: waiting for a herdr session"), status: "Slack: off"}
	}()
	if code, reply := ts.postSlack(t, "/api/slack/fetch", "{}"); code != http.StatusBadGateway || reply["error"] != "can't fetch: waiting for a herdr session" {
		t.Errorf("a failed fetch: %d %v, want 502 with why", code, reply)
	}
	<-ended
	waitFor(t, boards, "the Slack line after the failed fetch", slackText("Slack: off"))

	// Without Slack, there's nothing to fetch.
	bare := startServer(t, Config{Sessions: running()}, nil)
	if code, reply := bare.postSlack(t, "/api/slack/fetch", "{}"); code != http.StatusNotFound || reply["error"] == "" {
		t.Errorf("fetch without Slack: %d %v, want 404", code, reply)
	}
}

// TestTakeEndpoint: Take makes the task for a skipped ask by its message
// ID, and answers with where it went. The board is built again after it.
func TestTakeEndpoint(t *testing.T) {
	status := &slackStatus{s: slack.Status{On: true, Fetch: true, Text: "Slack: last check 09:00"}}
	var mu sync.Mutex
	var took []string
	take := func(ctx context.Context, id string) (string, error) {
		mu.Lock()
		took = append(took, id)
		mu.Unlock()
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the take has no deadline")
		}
		switch id {
		case "C0123ABCD/1793454730.123456":
			status.set("Slack: took it")
			return "Took it: new tab w2:t1, with research", nil
		case "D0RUI0001/1793370600.000100":
			return "", fmt.Errorf("%w %s", slack.ErrNoSkippedAsk, id)
		}
		return "", errors.New("can't take it: waiting for a herdr session")
	}
	ts := startServer(t, Config{Sessions: running(), Slack: status.get, SlackTake: take}, nil)
	boards := ts.stream(t)
	waitFor(t, boards, "the Slack line", slackText("Slack: last check 09:00"))

	code, reply := ts.postSlack(t, "/api/slack/take", `{"id": "C0123ABCD/1793454730.123456"}`)
	if code != http.StatusOK || !reflect.DeepEqual(reply, map[string]string{"text": "Took it: new tab w2:t1, with research"}) {
		t.Errorf("take: %d %v", code, reply)
	}
	waitFor(t, boards, "the Slack line after the take", slackText("Slack: took it"))

	if code, reply := ts.postSlack(t, "/api/slack/take", `{"id": "D0RUI0001/1793370600.000100"}`); code != http.StatusNotFound || reply["error"] == "" {
		t.Errorf("an ask that isn't skipped: %d %v, want 404", code, reply)
	}
	if code, reply := ts.postSlack(t, "/api/slack/take", `{"id": "C0123ABCD/1793450000.000100"}`); code != http.StatusBadGateway || reply["error"] != "can't take it: waiting for a herdr session" {
		t.Errorf("a take that fails: %d %v, want 502 with why", code, reply)
	}
	for _, body := range []string{`{}`, `{"id": " "}`, `not json`} {
		if code, reply := ts.postSlack(t, "/api/slack/take", body); code != http.StatusBadRequest || reply["error"] == "" {
			t.Errorf("take %s: %d %v, want 400", body, code, reply)
		}
	}
	if code, _, err := ts.slackPost(context.Background(), "/api/slack/take", `{"id": "C0123ABCD/1793454730.123456"}`, false); err != nil || code != http.StatusUnauthorized {
		t.Errorf("without the key: %d %v, want 401", code, err)
	}
	mu.Lock()
	want := []string{"C0123ABCD/1793454730.123456", "D0RUI0001/1793370600.000100", "C0123ABCD/1793450000.000100"}
	if !reflect.DeepEqual(took, want) {
		t.Errorf("took %q, want %q", took, want)
	}
	mu.Unlock()

	bare := startServer(t, Config{Sessions: running()}, nil)
	if code, reply := bare.postSlack(t, "/api/slack/take", `{"id": "C0123ABCD/1793454730.123456"}`); code != http.StatusNotFound || reply["error"] == "" {
		t.Errorf("take without Slack: %d %v, want 404", code, reply)
	}
}

// noHerdr is a herdr session that can't be reached.
type noHerdr struct{ slack.Herdr }

func (noHerdr) Live() (*herdr.Live, error) { return nil, errors.New("herdr isn't running") }

// TestTakeEndpointIgnoresExpiredAsks: through the watcher's own Take, an
// ask skipped over 7 days ago, which the list no longer shows, can't be
// taken: it's a 404, as any ask that isn't on the list.
func TestTakeEndpointIgnoresExpiredAsks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	old := skippedAsk(time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC), false, "<@U0ME00001> old news")
	if err := slack.Update(dir, func(s *slack.State) error {
		s.UserID = "U0ME00001"
		s.Skip(old, slack.Skip{Why: "old news"}, now.Add(-8*24*time.Hour))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := &slack.Watcher{
		Dir: dir, Exe: "/home/u/bin/herdr-tasks",
		Options: func() (reconcile.Options, error) {
			return reconcile.Options{SlackSession: "work", SlackWorkspace: "Slack", SlackCwd: dir, SlackEvery: "5m", SlackHours: "always", SlackMaxNew: 4}, nil
		},
		Sessions: func() []slack.Session {
			return []slack.Session{{Name: "work", Herdr: noHerdr{}, Store: &store.Store{Dir: dir, Session: "work"}}}
		},
		Now: func() time.Time { return now },
	}
	take := func(ctx context.Context, id string) (string, error) {
		d, err := w.Take(ctx, id)
		if err != nil {
			return "", err
		}
		return d.TookLine(), nil
	}
	status := &slackStatus{s: slack.Status{On: true, Fetch: true, Text: "Slack: last check 09:00"}}
	ts := startServer(t, Config{Sessions: running(), Slack: status.get, SlackTake: take}, nil)
	for _, id := range []string{old.ID(), "1"} {
		code, reply := ts.postSlack(t, "/api/slack/take", `{"id": "`+id+`"}`)
		if code != http.StatusNotFound || reply["error"] != "That ask isn't on the skipped list any more." {
			t.Errorf("take %s, skipped 8 days ago: %d %v, want 404", id, code, reply)
		}
	}
}

func TestServerExitsWithoutSessions(t *testing.T) {
	quick := func(s *Server) { s.idle = 300 * time.Millisecond }
	start := time.Now()
	ts := startServer(t, Config{Sessions: running()}, quick)
	select {
	case <-ts.stopped:
		if d := time.Since(start); d < 300*time.Millisecond {
			t.Errorf("stopped after %v, before the idle time", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the server is still running without a session")
	}

	// It stays while a session runs, and goes once none does. The session
	// leaves the board first.
	work := newConn(t, "work", oneSpace())
	sessions := running(work)
	ts = startServer(t, Config{Sessions: sessions}, quick)
	boards := ts.stream(t)
	waitFor(t, boards, "the session", func(b Board) bool { return len(b.Sessions) == 1 })
	select {
	case <-ts.stopped:
		t.Fatal("the server stopped with a session running")
	case <-time.After(800 * time.Millisecond):
	}
	sessions.set()
	waitFor(t, boards, "no session", func(b Board) bool { return len(b.Sessions) == 0 })
	select {
	case <-ts.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the server is still running after the session ended")
	}
}

func TestOneWedgedSessionDoesntFreezeOthers(t *testing.T) {
	stuck := newConn(t, "stuck", oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Stage: task.Active, Since: now})
	stuck.stuck = make(chan struct{})
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Stage: task.Active, Since: now})
	ts := startServer(t, Config{Sessions: running(stuck, work)}, func(s *Server) { s.timeout = 300 * time.Millisecond })
	// Before the server's cleanup, which waits for every read to finish.
	t.Cleanup(func() { close(stuck.stuck) })
	boards := ts.stream(t)
	waitFor(t, boards, "work's card", hasCard("work", "w1:t1"))

	if code, msg := ts.act(t, map[string]any{"session": "work", "tab": "w1:t1", "action": "stage", "stage": "parked"}); code != http.StatusNoContent {
		t.Fatalf("stage: %d %s", code, msg)
	}
	waitFor(t, boards, "work's card parked", func(b Board) bool {
		c, _ := findCard(b, "work", "w1:t1")
		return c.Column == "parked"
	})
	// The stuck session is on the board, marked stale.
	waitFor(t, boards, "the stuck session marked stale", func(b Board) bool {
		return slices.ContainsFunc(b.Sessions, func(s Session) bool { return s.Name == "stuck" && s.Stale })
	})
}

// A session left out of one listing stays, marked stale, as herdr may only
// have been too busy to take the connection. It goes once it's been left out
// graceTicks times in a row, or at once when it surely ended.
func TestSessionLeftOutOfTheListStaysAWhile(t *testing.T) {
	newSession := func(name string) *fakeConn {
		return newConn(t, name, oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Stage: task.Active, Since: now})
	}
	work, away, ended := newSession("work"), newSession("away"), newSession("ended")
	sessions := running(work, away, ended)
	// The server looks for sessions once as it starts; the test does the
	// ticks after that.
	ts := startServer(t, Config{Sessions: sessions}, func(s *Server) { s.tick = time.Hour })
	tick := func() { ts.track(ts.ctx, sessions.List()) }
	kept := func(name string) bool {
		_, ok := ts.hub.conn(name)
		return ok
	}
	boards := ts.stream(t)
	waitFor(t, boards, "every session's card", func(b Board) bool {
		return hasCard("work", "w1:t1")(b) && hasCard("away", "w1:t1")(b) && hasCard("ended", "w1:t1")(b)
	})

	ended.end()
	sessions.set(work)
	tick()
	waitFor(t, boards, "away stale, with its card, and ended gone", func(b Board) bool {
		s, ok := sessionNamed(b, "away")
		_, gone := sessionNamed(b, "ended")
		return ok && s.Stale && hasCard("away", "w1:t1")(b) && !gone
	})
	for range graceTicks - 2 {
		tick()
	}
	if !kept("away") {
		t.Fatalf("away went after %d ticks left out", graceTicks-1)
	}

	// Listed again, it starts over, and shows as it is at once.
	sessions.set(work, away)
	tick()
	waitFor(t, boards, "away no longer stale", func(b Board) bool {
		s, ok := sessionNamed(b, "away")
		return ok && !s.Stale
	})
	sessions.set(work)
	for range graceTicks - 1 {
		tick()
	}
	if !kept("away") {
		t.Fatal("away went before its grace was up")
	}
	tick()
	waitFor(t, boards, "away gone", func(b Board) bool {
		_, ok := sessionNamed(b, "away")
		return !ok && hasCard("work", "w1:t1")(b)
	})
}

func TestSlowStreamDoesntBlockOthers(t *testing.T) {
	// Each board is half a megabyte, so a page that stops reading soon fills
	// what the connection can hold.
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Stage: task.Active, Since: now,
		Note: strings.Repeat("x", 512<<10)})
	ts := startServer(t, Config{Sessions: running(work)}, nil)

	// A stream with a small window that's never read past its headers.
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) { _ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096) })
	}}
	slow, err := d.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", ts.port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slow.Close() })
	fmt.Fprintf(slow, "GET /api/events HTTP/1.1\r\nHost: 127.0.0.1:%d\r\nAuthorization: Bearer %s\r\n\r\n", ts.port, ts.key)
	r := bufio.NewReaderSize(slow, 16)
	if line, err := r.ReadString('\n'); err != nil || !strings.HasPrefix(line, "HTTP/1.1 200") {
		t.Fatalf("slow stream: %q %v", line, err)
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}

	fast := ts.stream(t)
	waitFor(t, fast, "the card", hasCard("work", "w1:t1"))
	for i := range 16 {
		start := time.Now()
		stage := []string{"parked", "paused"}[i%2]
		if code, msg := ts.act(t, map[string]any{"session": "work", "tab": "w1:t1", "action": "stage", "stage": stage, "reason": fmt.Sprint(i)}); code != http.StatusNoContent {
			t.Fatalf("stage: %d %s", code, msg)
		}
		want := ""
		if stage == "paused" {
			want = fmt.Sprint(i)
		}
		waitFor(t, fast, stage+" "+want, func(b Board) bool {
			c, _ := findCard(b, "work", "w1:t1")
			return c.Column == stage && c.Reason == want
		})
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("change %d took %v to reach the other stream", i, d)
		}
	}
	start := time.Now()
	ts.board(t)
	if d := time.Since(start); d > time.Second {
		t.Errorf("/api/board took %v", d)
	}
}

func TestLinearStatusForOpenCards(t *testing.T) {
	var mu sync.Mutex
	var asked [][]string
	failing := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Variables map[string]string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("Linear request: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		var ids []string
		data := map[string]any{}
		for alias, id := range req.Variables {
			ids = append(ids, id)
			data[alias] = map[string]any{"identifier": id, "title": "Fix " + id, "url": "https://linear.app/acme/issue/" + id,
				"state": map[string]any{"name": "In Progress", "type": "started", "color": "#f2c94c"}}
		}
		sort.Strings(ids)
		asked = append(asked, ids)
		if failing {
			http.Error(w, "down for a moment", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer api.Close()
	calls := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(asked)
	}

	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}, herdr.Tab{ID: "w1:t2"}),
		&task.Task{TabID: "w1:t1", Stage: task.Active, Since: now, Links: []task.Link{{URL: "https://linear.app/acme/issue/ABC-12/fix-the-login-page", Added: now}}},
		&task.Task{TabID: "w1:t2", Stage: task.Done, Since: now, Links: []task.Link{{URL: "https://linear.app/acme/issue/ABC-13", Added: now}}})
	key := func() (*linear.Client, bool) { return &linear.Client{Key: "lin_api_test", Endpoint: api.URL}, true }
	// issue is how a card shows a Linear issue.
	issue := func(b Board, tab, id string) *linear.Issue {
		c, _ := findCard(b, "work", tab)
		for _, l := range c.Links {
			if l.Linear != nil && l.Linear.ID == id {
				return l.Linear
			}
		}
		return nil
	}

	// The board asks about an issue as soon as it's on an open card, without
	// waiting for the next refresh.
	ts := startServer(t, Config{Sessions: running(work), Linear: key}, func(s *Server) { s.linearEvery = time.Hour })
	boards := ts.stream(t)
	b := waitFor(t, boards, "ABC-12's status", func(b Board) bool {
		is := issue(b, "w1:t1", "ABC-12")
		return is != nil && is.State == "In Progress"
	})
	if !b.LinearKey {
		t.Error("linearKey = false")
	}
	// A done card's issue isn't asked about.
	if is := issue(b, "w1:t2", "ABC-13"); is == nil || *is != (linear.Issue{ID: "ABC-13"}) {
		t.Errorf("the done card's issue = %+v", is)
	}
	mu.Lock()
	if !reflect.DeepEqual(asked, [][]string{{"ABC-12"}}) {
		t.Errorf("asked Linear for %v", asked)
	}
	failing = true
	mu.Unlock()

	// When Linear fails, the board keeps the status it had.
	n := calls()
	work.save(t, "w1:t1", func(tk *task.Task) { tk.AddLink("https://linear.app/acme/issue/ABC-14", "", "", now) })
	work.nudge()
	waitFor(t, boards, "ABC-14", func(b Board) bool { return issue(b, "w1:t1", "ABC-14") != nil })
	for deadline := time.Now().Add(3 * time.Second); calls() == n; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("Linear wasn't asked about ABC-14")
		}
	}
	b, _ = ts.board(t)
	if is := issue(b, "w1:t1", "ABC-12"); is == nil || is.State != "In Progress" || !b.LinearKey {
		t.Errorf("after Linear failed: %+v", is)
	}
	if is := issue(b, "w1:t1", "ABC-14"); is == nil || *is != (linear.Issue{ID: "ABC-14"}) {
		t.Errorf("ABC-14 = %+v", is)
	}

	// It asks again every linearEvery.
	mu.Lock()
	failing = false
	mu.Unlock()
	ts = startServer(t, Config{Sessions: running(work), Linear: key}, func(s *Server) { s.linearEvery = 50 * time.Millisecond })
	n = calls()
	for deadline := time.Now().Add(3 * time.Second); calls() < n+4; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("Linear asked %d times in 3s", calls()-n)
		}
	}

	// Without a key, the board says so, and shows the ID alone.
	noKey := func() (*linear.Client, bool) { return nil, false }
	ts = startServer(t, Config{Sessions: running(work), Linear: noKey}, nil)
	b = waitFor(t, ts.stream(t), "the card", hasCard("work", "w1:t1"))
	if is := issue(b, "w1:t1", "ABC-12"); b.LinearKey || is == nil || *is != (linear.Issue{ID: "ABC-12"}) {
		t.Errorf("without a key: linearKey %v, issue %+v", b.LinearKey, is)
	}
}

// An issue that moved to another team has a new identifier, and Linear
// answers its old one with it. The board asks by the ID in the link, as the
// fetched issues are keyed by it, so it asks once rather than in a loop.
func TestLinearIssueWithANewIdentifier(t *testing.T) {
	var mu sync.Mutex
	var asked [][]string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Variables map[string]string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("Linear request: %v", err)
		}
		var ids []string
		data := map[string]any{}
		for alias, id := range req.Variables {
			ids = append(ids, id)
			now := id
			if id == "ABC-12" {
				now = "XYZ-9"
			}
			data[alias] = map[string]any{"identifier": now, "title": "Fix " + now, "url": "https://linear.app/acme/issue/" + now,
				"state": map[string]any{"name": "In Progress", "type": "started", "color": "#f2c94c"}}
		}
		sort.Strings(ids)
		mu.Lock()
		asked = append(asked, ids)
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer api.Close()

	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}),
		&task.Task{TabID: "w1:t1", Stage: task.Active, Since: now, Links: []task.Link{{URL: "https://linear.app/acme/issue/ABC-12/fix-the-login-page", Added: now}}})
	key := func() (*linear.Client, bool) { return &linear.Client{Key: "lin_api_test", Endpoint: api.URL}, true }
	ts := startServer(t, Config{Sessions: running(work), Linear: key}, func(s *Server) { s.linearEvery = time.Hour })
	waitFor(t, ts.stream(t), "the fetched status", func(b Board) bool {
		c, _ := findCard(b, "work", "w1:t1")
		return len(c.Links) == 1 && c.Links[0].Linear != nil && c.Links[0].Linear.ID == "XYZ-9" && c.Links[0].Linear.State == "In Progress"
	})
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(asked, [][]string{{"ABC-12"}}) {
		n := len(asked)
		t.Errorf("asked Linear %d times: %v", n, asked[:min(n, 6)])
	}
}

func TestPageNeedsNoKey(t *testing.T) {
	ts := startServer(t, Config{Sessions: running()}, nil)
	for path, want := range map[string]int{"/": 200, "/work/": 200, "/-/": 404, "/-/static/": 404, "/static/": 200, "/nope/x": 404} {
		resp, err := plain.Get(ts.url(path))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s: %d, want %d", path, resp.StatusCode, want)
		}
		if want == 200 && !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Errorf("GET %s: Content-Type %q", path, resp.Header.Get("Content-Type"))
		}
	}
}

// get reads path with the browser key, and returns the status and the body.
func (ts *testServer) get(t *testing.T, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("GET", ts.url(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ts.key)
	resp, err := plain.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

// twoRunning is a server with the sessions called names, each with a card,
// and waits until all are on the board.
func twoRunning(t *testing.T, names ...string) *testServer {
	t.Helper()
	var conns []*fakeConn
	for _, name := range names {
		conns = append(conns, newConn(t, name, oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Title: "Login page", Stage: task.Active, Since: now}))
	}
	ts := startServer(t, Config{Sessions: running(conns...)}, nil)
	for _, name := range names {
		ts.shows(t, name, "w1:t1")
	}
	return ts
}

func TestServeBoardBySession(t *testing.T) {
	ts := twoRunning(t, "work", "side")
	st, raw := ts.get(t, "/api/board?session=side")
	var b Board
	if err := json.Unmarshal(raw, &b); err != nil || st != 200 || len(b.Sessions) != 1 || b.Sessions[0].Name != "side" {
		t.Fatalf("?session=side: %d %s (%v)", st, raw, err)
	}
	st, raw = ts.get(t, "/api/board?session=gone")
	if want := `{"error":"Session gone isn't running."}`; st != 404 || strings.TrimSpace(string(raw)) != want {
		t.Errorf("?session=gone: %d %s, want 404 %s", st, raw, want)
	}
	st, raw = ts.get(t, "/api/board")
	want := `{"version":"` + testVersion + `","sessions":[{"name":"side","cards":1,"stale":false},{"name":"work","cards":1,"stale":false}]}`
	if st != 200 || string(raw) != want {
		t.Errorf("without a session: %d %s, want %s", st, raw, want)
	}
}

func TestEventsBySession(t *testing.T) {
	ts := twoRunning(t, "work", "side")
	events, _ := ts.sse(t, "/api/events?session=work")
	select {
	case ev := <-events:
		var b Board
		if err := json.Unmarshal([]byte(ev.data), &b); ev.name != "board" || err != nil || len(b.Sessions) != 1 || b.Sessions[0].Name != "work" {
			t.Errorf("first event = %q %s (%v)", ev.name, ev.data, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
	events, _ = ts.sse(t, "/api/events")
	select {
	case ev := <-events:
		var l SessionList
		if err := json.Unmarshal([]byte(ev.data), &l); ev.name != "sessions" || err != nil || len(l.Sessions) != 2 {
			t.Errorf("first event = %q %s (%v)", ev.name, ev.data, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
	if st, raw := ts.get(t, "/api/events?session=gone"); st != 404 || !strings.Contains(string(raw), "Session gone isn't running.") {
		t.Errorf("?session=gone: %d %s", st, raw)
	}
}

// Another session's changes don't send a session's stream anything.
func TestEventsBySessionSkipsOtherSessions(t *testing.T) {
	ts := twoRunning(t, "work", "side")
	boards := ts.sessionStream(t, "work")
	select {
	case <-boards:
	case <-time.After(2 * time.Second):
		t.Fatal("no board on connect")
	}
	ts.hub.mu.Lock()
	side := ts.hub.workers["side"].conn.(*fakeConn)
	ts.hub.mu.Unlock()
	side.save(t, "w1:t1", func(tk *task.Task) { tk.Note = "Ship it" })
	side.nudge()
	waitFor(t, ts.stream(t), "side's note", func(b Board) bool {
		c, _ := findCard(b, "side", "w1:t1")
		return c.Note == "Ship it"
	})
	select {
	case b := <-boards:
		t.Errorf("work's stream got a board for side's change: %+v", b)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestSessionPagesServeIndex(t *testing.T) {
	ts := startServer(t, Config{Sessions: running()}, nil)
	noFollow := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for path, want := range map[string]int{
		"/": 200, "/work/": 200, "/work": 308, "/-/static/app.js": 200, "/static/app.js": 404,
		"/-": 404, "/-/": 404, "/work/x": 404,
	} {
		resp, err := noFollow.Get(ts.url(path))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s: %d, want %d", path, resp.StatusCode, want)
		}
		if want == 200 && path != "/-/static/app.js" && !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Errorf("GET %s: Content-Type %q", path, resp.Header.Get("Content-Type"))
		}
		if want == 308 && resp.Header.Get("Location") != "/work/" {
			t.Errorf("GET %s: Location %q", path, resp.Header.Get("Location"))
		}
	}
	// /api/ is the API's, which wants the key; with it, there's no page.
	for _, path := range []string{"/api", "/api/"} {
		if st, _ := ts.get(t, path); st != 404 {
			t.Errorf("GET %s with the key: %d, want 404", path, st)
		}
	}
	resp, err := noFollow.Get(ts.url("/my%20work?code=abc"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 308 || resp.Header.Get("Location") != "/my%20work/?code=abc" {
		t.Errorf("GET /my%%20work?code=abc: %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestBoardPathEscapesSessionName(t *testing.T) {
	ts := twoRunning(t, "my work")
	resp, err := plain.Get(ts.url("/my%20work/"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("GET /my%%20work/: %d", resp.StatusCode)
	}
	var b Board
	ts.getJSON(t, "/api/board?session=my%20work", &b)
	if len(b.Sessions) != 1 || b.Sessions[0].Name != "my work" {
		t.Errorf("?session=my%%20work = %+v", b)
	}
}

func TestBoardPathRoutesUnicodeSessionName(t *testing.T) {
	ts := twoRunning(t, "zażółć")
	resp, err := plain.Get(ts.url("/" + url.PathEscape("zażółć") + "/"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("GET /zażółć/: %d", resp.StatusCode)
	}
	var b Board
	ts.getJSON(t, "/api/board?session="+url.QueryEscape("zażółć"), &b)
	if len(b.Sessions) != 1 || b.Sessions[0].Name != "zażółć" {
		t.Errorf("?session=zażółć = %+v", b)
	}
}

// The settings' slack_session reaches the board, the list and the Slack line.
func TestSlackSessionFromSettings(t *testing.T) {
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Title: "Login page", Stage: task.Active, Since: now})
	side := newConn(t, "side", oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Title: "Billing", Stage: task.Active, Since: now})
	ts := startServer(t, Config{Sessions: running(work, side), Slack: func() slack.Status {
		return slack.Status{On: true, Text: "Slack: last check 14:32"}
	}}, nil)
	ts.setOptions(func(o *reconcile.Options) { o.SlackSession = "work" })
	ts.Refresh()
	ts.shows(t, "work", "w1:t1")
	ts.shows(t, "side", "w1:t1")
	var l SessionList
	for deadline := time.Now().Add(3 * time.Second); l.Slack != "work" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		ts.getJSON(t, "/api/board", &l)
	}
	if l.Slack != "work" {
		t.Fatalf("the list's slack = %q, want work", l.Slack)
	}
	var w, s Board
	ts.getJSON(t, "/api/board?session=work", &w)
	ts.getJSON(t, "/api/board?session=side", &s)
	if w.Slack == nil || s.Slack != nil || w.SlackSession != "work" || s.SlackWaiting != "" {
		t.Errorf("work: %+v, side: %+v", w, s)
	}
}

func TestSlackSessionFallsBackToDefault(t *testing.T) {
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Title: "Login page", Stage: task.Active, Since: now})
	line := func() slack.Status { return slack.Status{On: true, Text: "Slack: last check 14:32"} }
	ts := startServer(t, Config{Sessions: running(work), Slack: line}, func(s *Server) {
		var calls atomic.Int32
		s.cfg.Options = func() (reconcile.Options, error) {
			// The first read is the server's own check, which must work. The
			// hub's goroutines read too, so calls are counted atomically.
			if calls.Add(1) > 1 {
				return reconcile.Options{}, errors.New("unreadable")
			}
			return reconcile.Options{Board: true, BoardPort: s.cfg.Port, SlackSession: "work"}, nil
		}
	})
	ts.shows(t, "work", "w1:t1")
	var l SessionList
	ts.getJSON(t, "/api/board", &l)
	if l.Slack != "default" {
		t.Errorf("with unreadable settings, slack = %q, want default", l.Slack)
	}
}

// The list is streamed again when it changes.
func TestSessionsEventSendsAgainWhenListChanges(t *testing.T) {
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Title: "Login page", Stage: task.Active, Since: now})
	ts := startServer(t, Config{Sessions: running(work)}, nil)
	ts.shows(t, "work", "w1:t1")
	events, _ := ts.sse(t, "/api/events")
	next := func(what string, ok func(SessionList) bool) {
		t.Helper()
		for deadline := time.After(3 * time.Second); ; {
			select {
			case ev := <-events:
				var l SessionList
				if err := json.Unmarshal([]byte(ev.data), &l); err != nil || ev.name != "sessions" {
					t.Fatalf("event %q %s (%v)", ev.name, ev.data, err)
				}
				if ok(l) {
					return
				}
			case <-deadline:
				t.Fatalf("no sessions event with %s", what)
			}
		}
	}
	next("work", func(l SessionList) bool { return len(l.Sessions) == 1 })
	side := newConn(t, "side", oneSpace(herdr.Tab{ID: "w1:t1"}), &task.Task{TabID: "w1:t1", Title: "Billing", Stage: task.Active, Since: now})
	ts.cfg.Sessions.(*fakeSessions).set(work, side)
	next("a second session", func(l SessionList) bool { return len(l.Sessions) == 2 })
	work.save(t, "w1:t1", func(*task.Task) {})
	if err := work.st.Update(func(st *store.State) error {
		st.Tasks["w1:t2"] = &task.Task{TabID: "w1:t2", Title: "More", Stage: task.Active, Since: now}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	work.mu.Lock()
	work.live.Tabs = append(work.live.Tabs, herdr.Tab{ID: "w1:t2", WorkspaceID: "w1"})
	work.mu.Unlock()
	work.nudge()
	next("two cards in work", func(l SessionList) bool {
		for _, e := range l.Sessions {
			if e.Name == "work" {
				return e.Cards == 2
			}
		}
		return false
	})
}
