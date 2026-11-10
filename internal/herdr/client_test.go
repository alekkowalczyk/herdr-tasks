package herdr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

type request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// fakeHerdr listens on a unix socket like herdr. Each connection reads one
// request, records it, and hands the connection to answer.
type fakeHerdr struct {
	client *Client
	mu     sync.Mutex
	reqs   []request
}

func newFake(t *testing.T, answer func(req request, conn net.Conn)) *fakeHerdr {
	t.Helper()
	// t.TempDir paths can pass the unix socket path limit on macOS.
	dir, err := os.MkdirTemp("", "herdr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "herdr.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeHerdr{client: &Client{SocketPath: path}}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					return
				}
				var req request
				if err := json.Unmarshal(line, &req); err != nil {
					return
				}
				f.mu.Lock()
				f.reqs = append(f.reqs, req)
				f.mu.Unlock()
				answer(req, conn)
			}()
		}
	}()
	return f
}

func (f *fakeHerdr) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.reqs...)
}

// send writes msg as one JSON line, as herdr does.
func send(conn net.Conn, msg string) {
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(msg)); err != nil {
		panic(err)
	}
	b.WriteByte('\n')
	conn.Write(b.Bytes())
}

func TestPanesDecodeAgentFields(t *testing.T) {
	f := newFake(t, func(req request, conn net.Conn) {
		send(conn, `{"id":"herdr-tasks","result":{"type":"pane_list","panes":[
			{"pane_id":"w1:p4","terminal_id":"term_1","workspace_id":"w1","tab_id":"w1:t2","focused":true,
			 "cwd":"/src/acme-web","foreground_cwd":"/src/acme-web/.claude/worktrees/fix-login","agent":"claude",
			 "terminal_title":"✳ Fix the login form","terminal_title_stripped":"Fix the login form",
			 "agent_status":"working","agent_session":{"source":"herdr:claude","agent":"claude","kind":"id","value":"0b5c-session"},
			 "scroll":{"offset_from_bottom":3,"max_offset_from_bottom":120,"viewport_rows":38},"revision":6},
			{"pane_id":"w1:p5","terminal_id":"term_2","workspace_id":"w1","tab_id":"w1:t2","focused":false,
			 "cwd":"/src/acme-web","label":"Task strip","agent_status":"unknown",
			 "agent_session":{"source":"herdr:claude","agent":"claude","kind":"id","value":"77aa-session"},
			 "scroll":{"offset_from_bottom":0,"max_offset_from_bottom":0,"viewport_rows":7},"revision":1}]}}`)
	})
	panes, err := f.client.Panes()
	if err != nil {
		t.Fatal(err)
	}
	want := []Pane{
		{
			ID: "w1:p4", TabID: "w1:t2", WorkspaceID: "w1", Focused: true,
			Agent: "claude", AgentStatus: "working",
			Session: &AgentSession{Source: "herdr:claude", Agent: "claude", Kind: "id", Value: "0b5c-session"},
			Title:   "Fix the login form", Cwd: "/src/acme-web",
			Scroll: Scroll{Offset: 3, Max: 120, Rows: 38},
		},
		{
			ID: "w1:p5", TabID: "w1:t2", WorkspaceID: "w1", Label: "Task strip",
			AgentStatus: "unknown",
			Session:     &AgentSession{Source: "herdr:claude", Agent: "claude", Kind: "id", Value: "77aa-session"},
			Cwd:         "/src/acme-web",
			Scroll:      Scroll{Rows: 7},
		},
	}
	if !reflect.DeepEqual(panes, want) {
		t.Errorf("Panes =\n%+v\nwant\n%+v", panes, want)
	}
	if got := f.requests(); len(got) != 1 || got[0].Method != "pane.list" {
		t.Errorf("requests = %+v, want one pane.list", got)
	}
}

func TestOpenPluginPaneEnvSendsEnv(t *testing.T) {
	f := newFake(t, func(req request, conn net.Conn) {
		send(conn, `{"id":"herdr-tasks","result":{"type":"plugin_pane_opened"}}`)
	})
	env := map[string]string{"HERDR_TASKS_MESSAGE": "Board is off."}
	if err := f.client.OpenPluginPaneEnv("herdr-tasks", "overview", env); err != nil {
		t.Fatal(err)
	}
	if err := f.client.OpenPluginPane("herdr-tasks", "overview"); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want 2", len(reqs))
	}
	var withEnv, without map[string]any
	if err := json.Unmarshal(reqs[0].Params, &withEnv); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(reqs[1].Params, &without); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"plugin_id":  "herdr-tasks",
		"entrypoint": "overview",
		"focus":      true,
		"env":        map[string]any{"HERDR_TASKS_MESSAGE": "Board is off."},
	}
	if reqs[0].Method != "plugin.pane.open" || !reflect.DeepEqual(withEnv, want) {
		t.Errorf("request = %s %v, want plugin.pane.open %v", reqs[0].Method, withEnv, want)
	}
	delete(want, "env")
	if reqs[1].Method != "plugin.pane.open" || !reflect.DeepEqual(without, want) {
		t.Errorf("request = %s %v, want plugin.pane.open %v", reqs[1].Method, without, want)
	}
}

func TestSubscribeDeliversEventsUntilCancelled(t *testing.T) {
	hungUp := make(chan struct{})
	f := newFake(t, func(req request, conn net.Conn) {
		send(conn, `{"id":"herdr-tasks","result":{"type":"subscription_started"}}`)
		send(conn, `{"data":{"tab_id":"w1:t2","type":"tab_closed","workspace_id":"w1"},"event":"tab_closed"}`)
		send(conn, `{"data":{"pane":{"pane_id":"w1:p4","agent":"claude","agent_status":"working"},"type":"pane_updated"},"event":"pane_updated"}`)
		io.Copy(io.Discard, conn) // until the client hangs up
		close(hungUp)
	})
	subs := []Subscription{{Type: "tab.closed"}, {Type: "pane.agent_status_changed", PaneID: "w1:p4"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, 10)
	errc := make(chan error, 1)
	go func() {
		errc <- f.client.Subscribe(ctx, subs, func(ev Event) { events <- ev })
	}()

	var got []string
	for len(got) < 2 {
		select {
		case ev := <-events:
			got = append(got, ev.Event)
			if ev.Event == "tab_closed" {
				var d struct {
					TabID string `json:"tab_id"`
				}
				if err := json.Unmarshal(ev.Data, &d); err != nil || d.TabID != "w1:t2" {
					t.Errorf("tab_closed data = %s", ev.Data)
				}
			}
		case err := <-errc:
			t.Fatalf("Subscribe returned early: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatalf("got events %v, want two", got)
		}
	}
	if want := []string{"tab_closed", "pane_updated"}; !reflect.DeepEqual(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}

	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Subscribe = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe didn't return after cancel")
	}
	select {
	case <-hungUp:
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe left the connection open")
	}

	reqs := f.requests()
	if len(reqs) != 1 || reqs[0].Method != "events.subscribe" {
		t.Fatalf("requests = %+v, want one events.subscribe", reqs)
	}
	var params map[string]any
	if err := json.Unmarshal(reqs[0].Params, &params); err != nil {
		t.Fatal(err)
	}
	wantParams := map[string]any{"subscriptions": []any{
		map[string]any{"type": "tab.closed"},
		map[string]any{"type": "pane.agent_status_changed", "pane_id": "w1:p4"},
	}}
	if !reflect.DeepEqual(params, wantParams) {
		t.Errorf("params = %v, want %v", params, wantParams)
	}
}

func TestSubscribeReturnsWhenHerdrCloses(t *testing.T) {
	f := newFake(t, func(req request, conn net.Conn) {
		send(conn, `{"id":"herdr-tasks","result":{"type":"subscription_started"}}`)
		send(conn, `{"data":{"tab_id":"w1:t2","type":"tab_closed","workspace_id":"w1"},"event":"tab_closed"}`)
	})
	var got []string
	errc := make(chan error, 1)
	go func() {
		errc <- f.client.Subscribe(context.Background(), []Subscription{{Type: "tab.closed"}}, func(ev Event) {
			got = append(got, ev.Event)
		})
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, io.EOF) {
			t.Errorf("Subscribe = %v, want io.EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe didn't return when herdr closed the connection")
	}
	if want := []string{"tab_closed"}; !reflect.DeepEqual(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

func TestSubscribeReturnsHerdrsError(t *testing.T) {
	f := newFake(t, func(req request, conn net.Conn) {
		send(conn, `{"id":"","error":{"code":"invalid_request","message":"missing field pane_id"}}`)
	})
	err := f.client.Subscribe(context.Background(), []Subscription{{Type: "pane.agent_status_changed"}}, func(Event) {
		t.Error("got an event, want none")
	})
	var herr *Error
	if !errors.As(err, &herr) || herr.Code != "invalid_request" {
		t.Errorf("Subscribe = %v, want herdr's invalid_request", err)
	}
}

func TestSessionSockets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := filepath.Join(home, ".config", "herdr")
	for _, name := range []string{"acme", "work"} {
		if err := os.MkdirAll(filepath.Join(base, "sessions", name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "sessions", name, "herdr.sock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/acme/herdr.sock")
	want := []string{
		filepath.Join(base, "herdr.sock"),
		filepath.Join(base, "sessions", "acme", "herdr.sock"),
		filepath.Join(base, "sessions", "work", "herdr.sock"),
		"/tmp/acme/herdr.sock",
	}
	if got := SessionSockets(); !reflect.DeepEqual(got, want) {
		t.Errorf("SessionSockets = %q, want %q", got, want)
	}
}

func TestListening(t *testing.T) {
	// t.TempDir paths can pass the unix socket path limit on macOS.
	dir, err := os.MkdirTemp("", "herdr")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "herdr.sock")
	if Listening(path, time.Second) {
		t.Error("no socket: listening")
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	// It never accepts a connection, as a herdr that's busy: that still
	// counts, and doesn't take the timeout.
	start := time.Now()
	if !Listening(path, time.Second) {
		t.Error("a busy server: not listening")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("Listening took %v", d)
	}
	// A herdr that ended and left its socket.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if Listening(path, time.Second) {
		t.Error("a socket nothing listens on: listening")
	}
}

func TestReadScreenAsksForVisibleANSI(t *testing.T) {
	f := newFake(t, func(req request, conn net.Conn) {
		send(conn, `{"id":"herdr-tasks","result":{"type":"pane_read","read":{"pane_id":"w1:p4","workspace_id":"w1","tab_id":"w1:t2",
			"source":"visible","format":"ansi","text":"\u001b[0m\u001b[1mhello\r\n$ ls","revision":0,"truncated":false}}}`)
	})
	text, err := f.client.ReadScreen("w1:p4")
	if err != nil {
		t.Fatal(err)
	}
	if want := "\x1b[0m\x1b[1mhello\r\n$ ls"; text != want {
		t.Errorf("ReadScreen = %q, want %q", text, want)
	}
	reqs := f.requests()
	if len(reqs) != 1 || reqs[0].Method != "pane.read" {
		t.Fatalf("requests = %+v, want one pane.read", reqs)
	}
	var params map[string]any
	if err := json.Unmarshal(reqs[0].Params, &params); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"pane_id": "w1:p4", "source": "visible", "format": "ansi", "strip_ansi": false}
	if !reflect.DeepEqual(params, want) {
		t.Errorf("params = %v, want %v", params, want)
	}
}

func TestTypeTextSendKeysPaste(t *testing.T) {
	f := newFake(t, func(req request, conn net.Conn) {
		if req.Method == "pane.send_keys" && bytes.Contains(req.Params, []byte("bogus")) {
			send(conn, `{"id":"herdr-tasks","error":{"code":"invalid_key","message":"unsupported key bogus"}}`)
			return
		}
		send(conn, `{"id":"herdr-tasks","result":{"type":"ok"}}`)
	})
	if err := f.client.TypeText("w1:p4", "ls\r"); err != nil {
		t.Fatal(err)
	}
	if err := f.client.SendKeys("w1:p4", "ctrl+c", "enter"); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Paste("w1:p4", "one\rtwo"); err != nil {
		t.Fatal(err)
	}
	var herr *Error
	if err := f.client.SendKeys("w1:p4", "a", "bogus"); !errors.As(err, &herr) || herr.Message != "unsupported key bogus" {
		t.Errorf("SendKeys bogus = %v, want herdr's invalid_key", err)
	}

	want := []struct {
		method string
		params map[string]any
	}{
		// Raw, with no Enter added.
		{"pane.send_text", map[string]any{"pane_id": "w1:p4", "text": "ls\r"}},
		{"pane.send_keys", map[string]any{"pane_id": "w1:p4", "keys": []any{"ctrl+c", "enter"}}},
		// No keys, so herdr only wraps it in bracketed paste.
		{"pane.send_input", map[string]any{"pane_id": "w1:p4", "text": "one\rtwo"}},
		{"pane.send_keys", map[string]any{"pane_id": "w1:p4", "keys": []any{"a", "bogus"}}},
	}
	reqs := f.requests()
	if len(reqs) != len(want) {
		t.Fatalf("got %d requests, want %d: %+v", len(reqs), len(want), reqs)
	}
	for i, w := range want {
		var params map[string]any
		if err := json.Unmarshal(reqs[i].Params, &params); err != nil {
			t.Fatal(err)
		}
		if reqs[i].Method != w.method || !reflect.DeepEqual(params, w.params) {
			t.Errorf("request %d = %s %v, want %s %v", i, reqs[i].Method, params, w.method, w.params)
		}
	}
}

// ZoomPane sets the zoom rather than toggling it, so two clicks on the same
// button can't undo each other.
func TestZoomPaneSetsTheZoom(t *testing.T) {
	f := newFake(t, func(req request, conn net.Conn) {
		send(conn, `{"id":"herdr-tasks","result":{"type":"pane_zoom","zoom":{"changed":true,"zoom_changed":true,"focus_changed":false,
			"pane_id":"w1:p4","focused_pane_id":"w1:p4","zoomed":true,"layout":{}}}}`)
	})
	if err := f.client.ZoomPane("w1:p4", true); err != nil {
		t.Fatal(err)
	}
	if err := f.client.ZoomPane("w1:p4", false); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want 2: %+v", len(reqs), reqs)
	}
	for i, mode := range []string{"on", "off"} {
		var params map[string]any
		if err := json.Unmarshal(reqs[i].Params, &params); err != nil {
			t.Fatal(err)
		}
		if want := map[string]any{"pane_id": "w1:p4", "mode": mode}; reqs[i].Method != "pane.zoom" || !reflect.DeepEqual(params, want) {
			t.Errorf("request %d = %s %v, want pane.zoom %v", i, reqs[i].Method, params, want)
		}
	}
}

func TestTabLayoutDecodesFocusedAndRects(t *testing.T) {
	f := newFake(t, func(req request, conn net.Conn) {
		send(conn, `{"id":"herdr-tasks","result":{"type":"pane_layout","layout":{"workspace_id":"w1","tab_id":"w1:t2",
			"zoomed":true,"area":{"x":26,"y":1,"width":162,"height":49},"focused_pane_id":"w1:p5",
			"panes":[{"pane_id":"w1:p4","focused":false,"rect":{"x":26,"y":1,"width":81,"height":49}},
			         {"pane_id":"w1:p5","focused":true,"rect":{"x":107,"y":1,"width":81,"height":49}}],
			"splits":[{"id":"s1","direction":"right","ratio":0.5,"rect":{"x":26,"y":1,"width":162,"height":49}}]}}}`)
	})
	got, err := f.client.TabLayout("w1:p4")
	if err != nil {
		t.Fatal(err)
	}
	want := &TabLayout{
		TabID: "w1:t2", Zoomed: true, Focused: "w1:p5",
		Area: Rect{X: 26, Y: 1, Width: 162, Height: 49},
		Panes: []LayoutPane{
			{PaneID: "w1:p4", Rect: Rect{X: 26, Y: 1, Width: 81, Height: 49}},
			{PaneID: "w1:p5", Focused: true, Rect: Rect{X: 107, Y: 1, Width: 81, Height: 49}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TabLayout =\n%+v\nwant\n%+v", got, want)
	}
	reqs := f.requests()
	if len(reqs) != 1 || reqs[0].Method != "pane.layout" || string(reqs[0].Params) != `{"pane_id":"w1:p4"}` {
		t.Errorf("requests = %+v, want one pane.layout for w1:p4", reqs)
	}
}

func TestCreateCallsDontFocus(t *testing.T) {
	f := newFake(t, func(req request, conn net.Conn) {
		switch req.Method {
		case "workspace.create":
			send(conn, `{"result":{"workspace":{"workspace_id":"w2"},"tab":{"tab_id":"w2:t1","workspace_id":"w2"},"root_pane":{"pane_id":"w2:p1"}}}`)
		case "tab.create":
			send(conn, `{"result":{"tab":{"tab_id":"w2:t2","workspace_id":"w2"},"root_pane":{"pane_id":"w2:p4"}}}`)
		}
	})
	ws, err := f.client.CreateWorkspace("Slack", "/home/u/acme-harness")
	if err != nil {
		t.Fatal(err)
	}
	if want := (Created{WorkspaceID: "w2", TabID: "w2:t1", PaneID: "w2:p1"}); ws != want {
		t.Errorf("workspace = %+v, want %+v", ws, want)
	}
	tab, err := f.client.CreateTab("w2", "Dana", "/home/u/acme-harness")
	if err != nil {
		t.Fatal(err)
	}
	if want := (Created{WorkspaceID: "w2", TabID: "w2:t2", PaneID: "w2:p4"}); tab != want {
		t.Errorf("tab = %+v, want %+v", tab, want)
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if got, want := string(reqs[0].Params), `{"cwd":"/home/u/acme-harness","focus":false,"label":"Slack"}`; got != want {
		t.Errorf("workspace.create params = %s, want %s", got, want)
	}
	if got, want := string(reqs[1].Params), `{"cwd":"/home/u/acme-harness","focus":false,"label":"Dana","workspace_id":"w2"}`; got != want {
		t.Errorf("tab.create params = %s, want %s", got, want)
	}
}

func TestStartAgentSendsArgsAndWaits(t *testing.T) {
	old := callDeadline
	callDeadline = 50 * time.Millisecond
	t.Cleanup(func() { callDeadline = old })

	f := newFake(t, func(req request, conn net.Conn) {
		var p struct {
			Name string `json:"name"`
		}
		json.Unmarshal(req.Params, &p)
		if p.Name == "slack-notready" {
			send(conn, `{"error":{"code":"agent_not_ready","message":"blocked by a dialog"}}`)
			return
		}
		time.Sleep(200 * time.Millisecond)
		send(conn, `{"result":{"agent":{"name":"slack-1a2b3c4d"},"argv":["claude"]}}`)
	})
	args := []string{"--permission-mode", "plan", "hello"}
	if err := f.client.StartAgent("slack-1a2b3c4d", "claude", "w2:p1", args, time.Second); err != nil {
		t.Fatalf("StartAgent after a delay longer than the default deadline: %v", err)
	}
	reqs := f.requests()
	if reqs[0].Method != "agent.start" {
		t.Fatalf("method = %s", reqs[0].Method)
	}
	want := `{"args":["--permission-mode","plan","hello"],"kind":"claude","name":"slack-1a2b3c4d","pane_id":"w2:p1","timeout_ms":1000}`
	if got := string(reqs[0].Params); got != want {
		t.Errorf("params = %s, want %s", got, want)
	}
	// A plain Call still obeys the default deadline.
	if err := f.client.Call("pane.list", nil, nil); err == nil {
		t.Error("Call outlived the default deadline")
	}

	err := f.client.StartAgent("slack-notready", "claude", "w2:p1", nil, time.Second)
	var he *Error
	if !errors.As(err, &he) || he.Code != "agent_not_ready" {
		t.Errorf("err = %v, want *Error agent_not_ready", err)
	}
}

func TestProcessesDecodesArgv(t *testing.T) {
	f := newFake(t, func(req request, conn net.Conn) {
		send(conn, `{"result":{"process_info":{"foreground_processes":[
			{"pid":4242,"name":"claude","argv":["claude","--permission-mode","plan"],"cmdline":"claude --permission-mode plan","cwd":"/home/u/acme-harness"},
			{"pid":4300,"name":"node","argv":["node","x.js"]}]}}}`)
	})
	got, err := f.client.Processes("w2:p1")
	if err != nil {
		t.Fatal(err)
	}
	want := []Process{
		{PID: 4242, Name: "claude", Argv: []string{"claude", "--permission-mode", "plan"}},
		{PID: 4300, Name: "node", Argv: []string{"node", "x.js"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("processes = %+v, want %+v", got, want)
	}
	if p := string(f.requests()[0].Params); p != `{"pane_id":"w2:p1"}` {
		t.Errorf("params = %s", p)
	}
}

func TestMoveToNewTabInAWorkspace(t *testing.T) {
	changed := true
	f := newFake(t, func(req request, conn net.Conn) {
		if changed {
			send(conn, `{"result":{"move_result":{"changed":true,"pane":{"pane_id":"w3:p7","tab_id":"w3:t2"},"created_tab":true}}}`)
		} else {
			send(conn, `{"result":{"move_result":{"changed":false,"reason":"already there"}}}`)
		}
	})
	m, err := f.client.MovePaneToNewTab("w2:p1", "w3", "Dana: ship Friday?")
	if err != nil {
		t.Fatal(err)
	}
	if want := (Moved{PaneID: "w3:p7", TabID: "w3:t2"}); m != want {
		t.Errorf("moved = %+v, want %+v", m, want)
	}
	want := `{"destination":{"label":"Dana: ship Friday?","type":"new_tab","workspace_id":"w3"},"focus":false,"pane_id":"w2:p1"}`
	if got := string(f.requests()[0].Params); got != want {
		t.Errorf("params = %s, want %s", got, want)
	}

	m, err = f.client.MovePaneNextTo("w2:p1", "w3:t2", "w3:p7", "right", 0.5)
	if err != nil || m.PaneID != "w3:p7" {
		t.Errorf("MovePaneNextTo = %+v, %v", m, err)
	}

	changed = false
	_, err = f.client.MovePaneToNewTab("w2:p1", "w3", "x")
	var nm *ErrNotMoved
	if !errors.As(err, &nm) {
		t.Errorf("err = %v, want *ErrNotMoved", err)
	}
}
