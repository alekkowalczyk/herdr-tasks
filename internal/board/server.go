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
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/claudelog"
	"github.com/alekkowalczyk/herdr-tasks/internal/linear"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
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
// from. Sessions, Linear, Options, Claude and Now may be nil: no sessions,
// no Linear key, settings never checked, no recaps, and time.Now.
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
	// board off or move it to another port.
	Options func() (reconcile.Options, error)
	// Claude reads the session logs of Claude panes.
	Claude *claudelog.Cache
	Now    func() time.Time
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

	handler http.Handler
	hub     *hub
	terms   terminals

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
	page, err := fs.Sub(web, "web")
	if err != nil {
		panic(err) // web is embedded
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", pageTypes[".html"])
		http.ServeFileFS(w, r, page, "index.html")
	})
	mux.Handle("GET /static/", http.StripPrefix("/static", files(page)))
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("POST /api/login-code", s.loginCode)
	mux.HandleFunc("POST /api/session", s.session)
	mux.HandleFunc("POST /api/shutdown", s.shutdown)
	mux.HandleFunc("GET /api/board", s.serveBoard)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("POST /api/task", s.changeTask)
	mux.HandleFunc("POST /api/open", s.openLink)
	mux.HandleFunc("GET /api/tab/stream", s.tabStream)
	mux.HandleFunc("POST /api/pane/input", s.paneInput)
	s.handler = auth.Wrap(mux, adminOnly)
	return s
}

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

func (s *Server) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

// Run serves on ln until ctx ends, POST /api/shutdown, or the hub decides
// the server isn't needed. It records the server in board.json while it
// runs. Shutting down removes board.json, ends every stream, waits up to 2 s
// for requests to finish, then closes what's left.
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	defer runtime.KeepAlive(s.lock)
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

func (s *Server) loginCode(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"code": s.auth.LoginCode(time.Now())})
}

// session swaps a login code for the browser key.
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "That isn't a sign-in request.")
		return
	}
	key, ok := s.auth.Redeem(req.Code, time.Now())
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

// serveBoard answers with the board that's out. It never builds one.
func (s *Server) serveBoard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(s.hub.current().json)
}

// events streams the board: as it is on connect, then each time it changes.
// It pings while nothing does, and ends when the page goes away or the
// server shuts down.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	wake, leave := s.hub.join()
	defer leave()
	var sent uint64
	// send sends the board that's out, unless it was the last one sent.
	send := func() error {
		p := s.hub.current()
		if p.seq == sent {
			return nil
		}
		sent = p.seq
		if _, err := fmt.Fprintf(w, "event: board\ndata: %s\n\n", p.json); err != nil {
			return err
		}
		return rc.Flush()
	}
	if send() != nil {
		return
	}
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-wake:
			if send() != nil {
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
	Session string `json:"session"`
	Tab     string `json:"tab"`
	Action  string `json:"action"` // stage, priority, focus or unlink
	Stage   string `json:"stage"`  // stage: the column the card goes to
	Reason  string `json:"reason"` // stage: what an In review or Paused task waits for; empty keeps it
	On      bool   `json:"on"`     // priority
	URL     string `json:"url"`    // unlink
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
// read again, so the board shows the change at once.
func (s *Server) changeTask(w http.ResponseWriter, r *http.Request) {
	var req taskRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "That isn't a task request.")
		return
	}
	err := s.doTask(req)
	var te *taskError
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.As(err, &te):
		writeError(w, te.status, te.msg)
	case errors.Is(err, reconcile.ErrNoTab):
		writeError(w, http.StatusNotFound, "That tab is gone.")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) doTask(req taskRequest) error {
	if req.Session == "" || req.Tab == "" {
		return &taskError{http.StatusBadRequest, "A task request needs a session and a tab."}
	}
	var change func(*task.Task) error
	if req.Action != "focus" {
		var err error
		if change, err = s.taskChange(req); err != nil {
			return err
		}
	}
	conn, ok := s.hub.conn(req.Session)
	if !ok {
		return &taskError{http.StatusNotFound, fmt.Sprintf("Session %s isn't running.", req.Session)}
	}
	defer s.hub.refresh(req.Session)
	if change == nil {
		return conn.Focus(req.Tab)
	}
	return conn.Change(req.Tab, change)
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
			// RemoveLink is given its number, as a URL could also read as
			// a number or an issue ID.
			i := slices.IndexFunc(t.Links, func(l task.Link) bool { return l.URL == req.URL })
			if i < 0 {
				return &taskError{http.StatusNotFound, "That link isn't on the task."}
			}
			_, err := t.RemoveLink(strconv.Itoa(i + 1))
			return err
		}, nil
	}
	return nil, &taskError{http.StatusBadRequest, fmt.Sprintf("There's no action %q.", req.Action)}
}

// openRequest is what POST /api/open takes.
type openRequest struct {
	Session string `json:"session"`
	Tab     string `json:"tab"`
	URL     string `json:"url"`
}

// openLink opens a task's local page in this machine's browser, as browsers
// won't open file:// links from a web page. It opens only a link the task
// has as saved now, that is a file on this machine, and that is still a
// regular .html or .htm file once symlinks are followed.
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
	conn, ok := s.hub.conn(req.Session)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Session %s isn't running.", req.Session))
		return
	}
	// Not the board's copy, which may be a few seconds old. A session whose
	// tasks stay locked gets s.timeout, as the hub's reads do, and the wait
	// ends with the request: nothing is left waiting on the lock.
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	st, err := conn.Store().LoadContext(ctx)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			err = fmt.Errorf("no answer within %v", s.timeout)
		case s.ctx.Err() != nil:
			err = errors.New("the board is shutting down")
		}
		writeError(w, http.StatusInternalServerError, "Couldn't read the task: "+err.Error())
		return
	}
	t := st.Tasks[req.Tab]
	if t == nil || !slices.ContainsFunc(t.Links, func(l task.Link) bool { return l.URL == req.URL }) {
		writeError(w, http.StatusNotFound, "That link isn't on the task.")
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
	if err := openPath(resolved); err != nil {
		writeError(w, http.StatusInternalServerError, "Couldn't open it: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// isPage reports whether a path names an .html or .htm file.
func isPage(p string) bool {
	ext := strings.ToLower(filepath.Ext(p))
	return ext == ".html" || ext == ".htm"
}

// opener is the command that opens a local page in the default browser:
// open on macOS, xdg-open elsewhere. Tests swap it.
var opener = func(path string) *exec.Cmd {
	if runtime.GOOS == "darwin" {
		return exec.Command("/usr/bin/open", path)
	}
	return exec.Command("xdg-open", path)
}

// openWait is how long openPath waits for the opener to finish.
const openWait = 3 * time.Second

// openPath opens a local page with opener, and waits up to openWait for it
// to finish. An opener that fails gives the first line of its stderr as the
// reason. One still running by then is taken to have worked, as xdg-open
// can run as long as the browser it starts, and it's reaped when it ends.
// Tests swap it.
var openPath = func(path string) error {
	cmd := opener(path)
	stderr := &capped{max: 4 << 10}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, stderr
	// A browser it starts may keep its stderr open. Wait doesn't wait for
	// that once the opener has exited.
	cmd.WaitDelay = 500 * time.Millisecond
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, exec.ErrWaitDelay) {
			return nil
		}
		line, _, _ := strings.Cut(strings.TrimSpace(stderr.buf.String()), "\n")
		if line = strings.TrimSpace(line); line != "" {
			return errors.New(line)
		}
		return err
	case <-time.After(openWait):
		return nil
	}
}

// capped keeps the first max bytes written to it, and drops the rest.
type capped struct {
	max int
	buf bytes.Buffer
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
