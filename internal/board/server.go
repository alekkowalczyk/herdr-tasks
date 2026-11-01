package board

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/claudelog"
	"github.com/alekkowalczyk/herdr-tasks/internal/linear"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/slack"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// pingEvery keeps an idle stream open through anything that times it out.
const pingEvery = 25 * time.Second

// web is the page. It holds no data, so it's served without a key.
//
//go:embed web
var web embed.FS

// pageTypes are the types the page's files are served with, so they don't
// depend on the system's MIME tables.
var pageTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".svg":  "image/svg+xml",
}

// Config is what the server knows about itself, and where the board comes
// from. Sessions, Linear, Options, Claude, Slack, SlackFetch, SlackTake and
// Now may be nil: no sessions, no Linear key, settings never checked, no
// recaps, no Slack line, no fetch, no take, and time.Now.
type Config struct {
	StateDir string // the plugin's state dir; board.json goes in its board folder
	Port     int
	Version  string
	Build    string // BuildID of the running binary
	Exe      string // the running binary, symlinks resolved

	Sessions Sessions
	// Linear gives a client for Linear's API, or false when there's no key.
	// It's asked on each refresh, so a key added later is picked up. It
	// may keep the key between refreshes.
	Linear func() (*linear.Client, bool)
	// Options reads the settings. The server shuts down when they turn the
	// board and the Slack watcher off, or move the board to another port.
	Options func() (reconcile.Options, error)
	// Claude reads the session logs of Claude panes.
	Claude *claudelog.Cache
	// Slack is the Slack watcher's status. The board shows it while it's on
	// or offers a fetch.
	Slack func() slack.Status
	// SlackFetch checks Slack for asks now, as `slack fetch` does, and says
	// what it did.
	SlackFetch func(ctx context.Context) (string, error)
	// SlackTake makes the task for a skipped ask, by its message ID, and
	// says where it went, as `slack take` does. An ask that isn't on the
	// list is slack.ErrNoSkippedAsk.
	SlackTake func(ctx context.Context, id string) (string, error)
	Now       func() time.Time
	// TailnetOwner is the Tailscale login that owns this machine: "" when
	// it's unknown or the machine is tagged, and nil for none. A request on
	// the tailnet door with that login signs in without a login code. With
	// Tailscale set, nil is the owner Tailscale names.
	TailnetOwner func() string
	// Tailscale shares the board with the tailnet while board_tailnet is
	// on. nil never does.
	Tailscale Tailscale
}

// Server is the board's web server, for every running herdr session.
type Server struct {
	cfg  Config
	auth *Auth
	// lock is board.lock. The server keeps it until the process exits, so no
	// second server starts; holding it here keeps the garbage collector from
	// closing it.
	lock *os.File

	// ctx ends when the server shuts down. Every request's context comes from
	// it, and everything long-lived ends with it.
	ctx    context.Context
	cancel context.CancelFunc

	handler  http.Handler // the loopback door
	mux      *http.ServeMux
	hub      *hub
	terms    terminals
	fetching atomic.Bool // a fetch from the board runs
	pages    pages
	tailnet  tailnetState // the board's door on the tailnet

	// How often things happen and how long they may take. Tests shorten them.
	tick        time.Duration // look for sessions and read the settings
	idle        time.Duration // shut down after this long without a session
	poll        time.Duration // read each session
	quiet       time.Duration // read a session once its events stop for this long
	timeout     time.Duration // a session read that takes longer counts as failed
	retry       time.Duration // subscribe to a session's events again
	linearEvery time.Duration // ask Linear about the issues on the board
	layoutEvery time.Duration // read the layout of a tab a page shows
	screenEvery time.Duration // read the screens of a tab a page shows
	linger      time.Duration // keep reading a tab after the last page showing it goes
}

func NewServer(cfg Config, auth *Auth, lock *os.File) *Server {
	s := &Server{
		cfg: cfg, auth: auth, lock: lock, hub: newHub(cfg.Version),
		terms:       terminals{pollers: map[tabKey]*poller{}, typing: map[paneKey]*paneLock{}},
		tick:        10 * time.Second,
		idle:        3 * time.Minute,
		poll:        2 * time.Second,
		quiet:       300 * time.Millisecond,
		timeout:     5 * time.Second,
		retry:       2 * time.Second,
		linearEvery: time.Minute,
		layoutEvery: time.Second,
		screenEvery: 150 * time.Millisecond,
		linger:      5 * time.Second,
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if cfg.Tailscale != nil && cfg.TailnetOwner == nil {
		s.cfg.TailnetOwner = s.tailnetOwner
	}
	page, err := fs.Sub(web, "web")
	if err != nil {
		panic(err) // web is embedded
	}
	mux := http.NewServeMux()
	index := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", pageTypes[".html"])
		http.ServeFileFS(w, r, page, "index.html")
	}
	mux.HandleFunc("GET /{$}", index)
	// /-/ is for the board's own paths, and /api/ is the API, which wants
	// the key, so sessions called - or api get no board.
	mux.HandleFunc("GET /{session}/{$}", func(w http.ResponseWriter, r *http.Request) {
		if reserved(r.PathValue("session")) {
			http.NotFound(w, r)
			return
		}
		index(w, r)
	})
	mux.HandleFunc("GET /{session}", func(w http.ResponseWriter, r *http.Request) {
		if reserved(r.PathValue("session")) {
			http.NotFound(w, r)
			return
		}
		to := r.URL.EscapedPath() + "/"
		if r.URL.RawQuery != "" {
			to += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, to, http.StatusPermanentRedirect)
	})
	mux.HandleFunc("GET /-/page/{id}/{path...}", s.pages.serve)
	mux.Handle("GET /-/static/", http.StripPrefix("/-/static", files(page)))
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("POST /api/login-code", s.loginCode)
	mux.HandleFunc("POST /api/session", s.session)
	mux.HandleFunc("POST /api/shutdown", s.shutdown)
	mux.HandleFunc("GET /api/board", s.serveBoard)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("POST /api/task", s.changeTask)
	mux.HandleFunc("POST /api/open", s.openLink)
	mux.HandleFunc("POST /api/share", s.shareLink)
	mux.HandleFunc("POST /api/unshare", s.stopSharing)
	mux.HandleFunc("POST /api/slack/fetch", s.slackFetch)
	mux.HandleFunc("POST /api/slack/take", s.slackTake)
	mux.HandleFunc("GET /api/tab/stream", s.tabStream)
	mux.HandleFunc("POST /api/pane/input", s.paneInput)
	s.mux = mux
	s.handler = auth.Wrap(mux, adminOnly)
	return s
}

// TailnetHandler is the board's tailnet door, for requests that come
// through tailscale serve. origin is the board's address on the tailnet.
func (s *Server) TailnetHandler(origin func() string) http.Handler {
	return s.auth.WrapTailnet(s.mux, adminOnly, origin)
}

// reserved reports whether name is a first path segment the board uses for
// itself, so no session's page can be there.
func reserved(name string) bool { return name == "-" || name == "api" }

// adminOnly picks the routes only the CLI may use.
func adminOnly(path string) bool {
	return path == "/api/login-code" || path == "/api/shutdown"
}

// files serves the page's files, but no list of them.
func files(page fs.FS) http.Handler {
	fsrv := http.FileServerFS(page)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		if t, ok := pageTypes[path.Ext(r.URL.Path)]; ok {
			w.Header().Set("Content-Type", t)
		}
		fsrv.ServeHTTP(w, r)
	})
}

func (s *Server) meta() Meta {
	return Meta{PID: os.Getpid(), Port: s.cfg.Port, Version: s.cfg.Version, Build: s.cfg.Build, Exe: s.cfg.Exe}
}

// Refresh has the board built again, so it shows what changed outside the
// sessions, such as the Slack watcher's status.
func (s *Server) Refresh() { signal(s.hub.dirty) }

func (s *Server) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

// Run serves on ln until ctx ends, POST /api/shutdown, or the hub decides
// the server isn't needed. It records the server in board.json while it
// runs. Shutting down removes board.json, ends every stream, waits up to 2 s
// for requests to finish, then closes what's left, the tailnet door too.
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	defer runtime.KeepAlive(s.lock)
	// After cancel, which ends a tailscale call that a sync under way waits
	// on.
	defer s.closeTailnet()
	stop := context.AfterFunc(ctx, s.cancel)
	defer stop()
	defer s.cancel()

	// Written before serving, so whoever gets an answer finds it.
	if err := writeMeta(s.cfg.StateDir, s.meta()); err != nil {
		ln.Close()
		return err
	}
	defer os.Remove(serverFile(s.cfg.StateDir, metaFile))
	s.runHub(s.ctx)

	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: streams stay open.
		BaseContext: func(net.Listener) context.Context { return s.ctx },
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return err
	case <-s.ctx.Done():
	}
	// Before the port goes: the lock stays taken until the process exits,
	// and meanwhile another program may take the port. A CLI that found
	// board.json would send it the admin token; without it, it waits for
	// the lock.
	os.Remove(serverFile(s.cfg.StateDir, metaFile))
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if srv.Shutdown(sctx) != nil {
		srv.Close()
	}
	<-served
	return nil
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, healthReply{App: "herdr-tasks", Meta: s.meta()})
}

// loginCode makes a login code, and says where the board is on the tailnet
// while it's shared there, and why it isn't there, or can't be checked,
// when that goes wrong.
func (s *Server) loginCode(w http.ResponseWriter, r *http.Request) {
	reply := map[string]string{"code": s.auth.LoginCode(time.Now())}
	if l := s.tailnetLine(); l != nil {
		if l.Address != "" {
			reply["tailnet"] = l.Address
		}
		if l.Problem != "" {
			reply["tailnetProblem"] = l.Problem
		}
	}
	writeJSON(w, http.StatusOK, reply)
}

// session swaps a login code for the browser key. On the tailnet door, the
// machine's owner needs no code: no code, and the owner's login, is enough.
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	// No body is no code, for the owner's sign-in.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "That isn't a sign-in request.")
		return
	}
	var key string
	ok := false
	if req.Code != "" {
		key, ok = s.auth.Redeem(req.Code, time.Now())
	} else if login := loginOf(r.Context()); login != "" && s.cfg.TailnetOwner != nil && s.cfg.TailnetOwner() == login {
		key, ok = s.auth.BrowserKey(), true
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, signInAgain)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": key})
}

// shutdown answers first, then shuts the server down.
func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusAccepted)
	_ = http.NewResponseController(w).Flush()
	go s.cancel()
}

// serveBoard answers with the board that's out: the session's, or without
// one the list of running sessions. It never builds one.
func (s *Server) serveBoard(w http.ResponseWriter, r *http.Request) {
	p := s.hub.current()
	data := p.sessionList()
	if name := r.URL.Query().Get("session"); name != "" {
		var ok bool
		if data, ok = p.forSession(name); !ok {
			writeError(w, http.StatusNotFound, "Session "+name+" isn't running.")
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// events streams the session's board, or without one the list of running
// sessions: as it is on connect, then each time it changes. It pings while
// nothing does, and ends when the page goes away, the session stops, or the
// server shuts down.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("session")
	// what is the event's name and data, from the board that's out.
	what := func(p *published) (string, []byte, bool) {
		if name == "" {
			return "sessions", p.sessionList(), true
		}
		data, ok := p.forSession(name)
		return "board", data, ok
	}
	if _, _, ok := what(s.hub.current()); !ok {
		writeError(w, http.StatusNotFound, "Session "+name+" isn't running.")
		return
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	wake, leave := s.hub.join()
	defer leave()
	var sent uint64
	var last []byte
	// send sends the board that's out, unless it was the last one sent or
	// holds nothing new for this stream. It's false once the session is gone.
	send := func() (bool, error) {
		p := s.hub.current()
		if p.seq == sent {
			return true, nil
		}
		sent = p.seq
		event, data, ok := what(p)
		if !ok {
			return false, nil
		}
		if bytes.Equal(data, last) {
			return true, nil
		}
		last = data
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false, err
		}
		return true, rc.Flush()
	}
	if ok, err := send(); !ok || err != nil {
		return
	}
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-wake:
			if ok, err := send(); !ok || err != nil {
				return
			}
		case <-t.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

// taskRequest is what POST /api/task takes.
type taskRequest struct {
	Session   string `json:"session"`
	Tab       string `json:"tab"`
	Action    string `json:"action"`    // stage, priority, focus, unlink, move, research, continue or mute
	Stage     string `json:"stage"`     // stage: the column the card goes to
	Reason    string `json:"reason"`    // stage: what an In review or Paused task waits for; empty keeps it
	On        bool   `json:"on"`        // priority
	URL       string `json:"url"`       // unlink
	Workspace string `json:"workspace"` // move: the workspace's ID or label
}

// moveReply is what POST /api/task answers a move with: the task's new tab,
// and, when the move went wrong partway, what went wrong.
type moveReply struct {
	Tab   string `json:"tab"`
	Error string `json:"error,omitempty"`
}

// taskError is a task request that can't be done, with the status to answer.
type taskError struct {
	status int
	msg    string
}

func (e *taskError) Error() string { return e.msg }

// columnStages is the stage a card dropped on each column gets. Asking, Your
// turn and Working are all Active: the agent decides which one shows it.
var columnStages = map[task.Group]task.Stage{
	task.Asking: task.Active, task.YourTurn: task.Active, task.Running: task.Active,
	task.GReview: task.Review, task.GPaused: task.Paused, task.GParked: task.Parked, task.GDone: task.Done,
}

// changeTask changes a task or shows its tab in herdr, and has the session
// read again, so the board shows the change at once. A move answers with
// the task's new tab.
//
// Starting research waits for Claude to be ready, a few minutes at most,
// and the request waits with it: the server has no write timeout, as its
// streams stay open.
func (s *Server) changeTask(w http.ResponseWriter, r *http.Request) {
	var req taskRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "That isn't a task request.")
		return
	}
	tab, err := s.doTask(r.Context(), req)
	var te *taskError
	switch {
	case err == nil && tab != "":
		writeJSON(w, http.StatusOK, moveReply{Tab: tab})
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case tab != "":
		// The task moved, but not all of it went well. The page follows it
		// all the same.
		writeJSON(w, http.StatusInternalServerError, moveReply{Tab: tab, Error: err.Error()})
	case errors.As(err, &te):
		writeError(w, te.status, te.msg)
	case errors.Is(err, reconcile.ErrNoTab):
		writeError(w, http.StatusNotFound, "That tab is gone.")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// doTask does a task request. A move returns the task's new tab.
func (s *Server) doTask(ctx context.Context, req taskRequest) (string, error) {
	if req.Session == "" || req.Tab == "" {
		return "", &taskError{http.StatusBadRequest, "A task request needs a session and a tab."}
	}
	var change func(*task.Task) error
	switch req.Action {
	case "focus", "research", "continue", "mute":
	case "move":
		if strings.TrimSpace(req.Workspace) == "" {
			return "", &taskError{http.StatusBadRequest, "A move needs a workspace."}
		}
	default:
		var err error
		if change, err = s.taskChange(req); err != nil {
			return "", err
		}
	}
	conn, ok := s.hub.conn(req.Session)
	if !ok {
		return "", &taskError{http.StatusNotFound, fmt.Sprintf("Session %s isn't running.", req.Session)}
	}
	defer s.hub.refresh(req.Session)
	switch req.Action {
	case "focus":
		return "", conn.Focus(req.Tab)
	case "move", "research", "continue", "mute":
		return s.taskAction(ctx, conn, req)
	}
	return "", conn.Change(req.Tab, change)
}

// taskAction moves a task to another workspace, or does one of a Slack
// task's actions. It reads the task as saved now first, as the page's copy
// may be out of date, and refuses an action the task can't take with what
// the strip says.
func (s *Server) taskAction(ctx context.Context, conn SessionConn, req taskRequest) (string, error) {
	tasks, err := s.loadTasks(ctx, conn)
	if err != nil {
		return "", err
	}
	t := tasks[req.Tab]
	if t == nil {
		return "", &taskError{http.StatusNotFound, "That tab is gone."}
	}
	a := t.Slack
	switch req.Action {
	case "move":
		return conn.Move(req.Tab, strings.TrimSpace(req.Workspace))
	case "research":
		switch {
		case a == nil:
			return "", &taskError{http.StatusConflict, "Only a Slack task has research."}
		case !a.CanStart():
			return "", &taskError{http.StatusConflict, "Research already started."}
		}
		return "", conn.Research(req.Tab)
	case "continue":
		if a == nil || !a.CanContinue() {
			return "", &taskError{http.StatusConflict, "No locked research session in this tab."}
		}
		return "", conn.Continue(req.Tab)
	}
	// mute: the watcher keeps muted channels in the state folder.
	if a == nil || a.Channel == "" {
		return "", &taskError{http.StatusConflict, "Only a Slack task's channel can be muted."}
	}
	err = slack.Update(s.cfg.StateDir, func(st *slack.State) error {
		st.Mute(a.Channel, a.Place)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("Couldn't mute: %w", err)
	}
	s.Refresh()
	return "", nil
}

// loadTasks reads a session's tasks as saved now: not the board's copy,
// which may be a few seconds old. A session whose tasks stay locked gets
// s.timeout, as the hub's reads do, and the wait ends with the request:
// nothing is left waiting on the lock.
func (s *Server) loadTasks(ctx context.Context, conn SessionConn) (map[string]*task.Task, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	st, err := conn.Store().LoadContext(ctx)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			err = fmt.Errorf("no answer within %v", s.timeout)
		case s.ctx.Err() != nil:
			err = errors.New("the board is shutting down")
		}
		return nil, errors.New("Couldn't read the task: " + err.Error())
	}
	return st.Tasks, nil
}

// taskChange is what an action other than focus does to the task.
func (s *Server) taskChange(req taskRequest) (func(*task.Task) error, error) {
	switch req.Action {
	case "stage":
		stage, ok := columnStages[task.Group(req.Stage)]
		if !ok {
			return nil, &taskError{http.StatusBadRequest, fmt.Sprintf("There's no column %q.", req.Stage)}
		}
		reason := strings.TrimSpace(req.Reason)
		return func(t *task.Task) error {
			t.SetStage(stage, reason, s.now())
			return nil
		}, nil
	case "priority":
		return func(t *task.Task) error {
			if err := t.SetPriority(req.On); err != nil {
				return &taskError{http.StatusConflict, err.Error()}
			}
			return nil
		}, nil
	case "unlink":
		return func(t *task.Task) error {
			// By its URL only: the page's list of links may be out of date.
			// RemoveLink is given its number, in task.Grouped order, as a
			// URL could also read as a number or an issue ID.
			i := slices.IndexFunc(task.Grouped(t.Links), func(l task.Link) bool { return l.URL == req.URL })
			if i < 0 {
				return &taskError{http.StatusNotFound, "That link isn't on the task."}
			}
			_, err := t.RemoveLink(strconv.Itoa(i + 1))
			return err
		}, nil
	}
	return nil, &taskError{http.StatusBadRequest, fmt.Sprintf("There's no action %q.", req.Action)}
}

// fetchTimeout is how long a fetch or a take from the board may take, as
// long as `slack fetch` waits: it waits for a check under way, and with
// slack_topic, a check runs a topic check for each of up to 20 asks.
const fetchTimeout = 15 * time.Minute

// errTooLong is why a fetch or a take ran out of time: it took over
// fetchTimeout.
var errTooLong = fmt.Errorf("it took over %d minutes", fetchTimeout/time.Minute)

// slackFetch checks Slack for asks now, and answers with what the fetch did,
// or why it failed. One runs at a time. It runs on the server's context,
// not the request's, so closing the page doesn't stop it; the request
// waits for it.
func (s *Server) slackFetch(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SlackFetch == nil {
		writeError(w, http.StatusNotFound, "This board doesn't fetch from Slack.")
		return
	}
	if !s.fetching.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, "A fetch is already running.")
		return
	}
	text, err := s.fetch()
	s.Refresh()
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": text})
}

// fetch runs SlackFetch, and lets another start once it's done.
func (s *Server) fetch() (string, error) {
	defer s.fetching.Store(false)
	ctx, cancel := context.WithTimeoutCause(s.ctx, fetchTimeout, errTooLong)
	defer cancel()
	return s.cfg.SlackFetch(ctx)
}

// takeRequest is what POST /api/slack/take takes.
type takeRequest struct {
	ID string `json:"id"` // the skipped ask's message ID
}

// slackTake makes the task for a skipped ask, and answers
// with where it went, or why it didn't. Like a fetch, it waits for a check
// under way, and runs on the server's context, so closing the page doesn't
// stop it.
func (s *Server) slackTake(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SlackTake == nil {
		writeError(w, http.StatusNotFound, "This board doesn't take Slack asks.")
		return
	}
	var req takeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeError(w, http.StatusBadRequest, "A take needs the ask's message ID.")
		return
	}
	ctx, cancel := context.WithTimeoutCause(s.ctx, fetchTimeout, errTooLong)
	defer cancel()
	text, err := s.cfg.SlackTake(ctx, strings.TrimSpace(req.ID))
	s.Refresh()
	switch {
	case errors.Is(err, slack.ErrNoSkippedAsk):
		writeError(w, http.StatusNotFound, "That ask isn't on the skipped list any more.")
	case err != nil:
		writeError(w, http.StatusBadGateway, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"text": text})
	}
}

// openRequest is what POST /api/open and POST /api/share take: a link on a
// task.
type openRequest struct {
	Session string `json:"session"`
	Tab     string `json:"tab"`
	URL     string `json:"url"`
}

// openLink gives the address the board serves a task's local page at, as
// browsers won't open file:// links from a web page, and a board seen from
// another machine has no use for opening it here. It does so only for a link
// the task has as saved now, that is a file on this machine, and that is
// still a regular .html or .htm file once symlinks are followed.
func (s *Server) openLink(w http.ResponseWriter, r *http.Request) {
	var req openRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "That isn't an open request.")
		return
	}
	if req.Session == "" || req.Tab == "" {
		writeError(w, http.StatusBadRequest, "An open request needs a session and a tab.")
		return
	}
	if e := s.linkOnTask(r.Context(), req.Session, req.Tab, req.URL); e != nil {
		writeError(w, e.status, e.msg)
		return
	}
	p, ok := task.FilePath(req.URL)
	if !ok || !isPage(p) {
		writeError(w, http.StatusBadRequest, "Only local .html pages open this way.")
		return
	}
	resolved, err := filepath.EvalSymlinks(p)
	var fi os.FileInfo
	if err == nil {
		fi, err = os.Lstat(resolved)
	}
	if err != nil || !fi.Mode().IsRegular() || !isPage(resolved) {
		writeError(w, http.StatusNotFound, "That file is gone.")
		return
	}
	if why := refused(filepath.Base(resolved), fi.Size()); why != "" {
		writeError(w, http.StatusBadRequest, why)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"href": s.pages.href(resolved)})
}

// linkOnTask says why a link isn't on a task as saved now, or nil when it
// is.
func (s *Server) linkOnTask(ctx context.Context, session, tab, url string) *taskError {
	conn, ok := s.hub.conn(session)
	if !ok {
		return &taskError{http.StatusNotFound, fmt.Sprintf("Session %s isn't running.", session)}
	}
	tasks, err := s.loadTasks(ctx, conn)
	if err != nil {
		return &taskError{http.StatusInternalServerError, err.Error()}
	}
	t := tasks[tab]
	if t == nil || !slices.ContainsFunc(t.Links, func(l task.Link) bool { return l.URL == url }) {
		return &taskError{http.StatusNotFound, "That link isn't on the task."}
	}
	return nil
}

// isPage reports whether a path names an .html or .htm file.
func isPage(p string) bool {
	ext := strings.ToLower(filepath.Ext(p))
	return ext == ".html" || ext == ".htm"
}
