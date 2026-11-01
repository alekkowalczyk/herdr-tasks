package board

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/claudelog"
	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/linear"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// linearTimeout bounds one refresh of the Linear issues.
const linearTimeout = 15 * time.Second

// hub keeps the board current.
//   - A worker per herdr session reads it and records what it read. Each
//     reads on its own, so a session that hangs holds up nothing else.
//   - One builder makes the board from the latest of each, and publishes it
//     when it changed.
//   - Streams are told there's a new board and send the latest when they
//     can, so a slow page holds up nothing either.
type hub struct {
	mu        sync.Mutex
	workers   map[string]*worker  // by session name
	snaps     map[string]Snapshot // each session's latest read
	issues    map[string]linear.Issue
	linearKey bool
	streams   map[chan struct{}]bool

	board atomic.Pointer[published]
	dirty chan struct{} // build the board again
	built chan struct{} // a new board is out

	// reads counts the session reads under way. Run doesn't wait for them,
	// as one may hang; tests do, before they remove the stores read.
	reads sync.WaitGroup
}

// published is a board as /api/board and the streams send it. It doesn't
// change once it's out.
type published struct {
	seq   uint64
	json  []byte
	board Board
}

// worker reads one herdr session.
type worker struct {
	name   string
	conn   SessionConn
	cancel context.CancelFunc
	kick   chan struct{} // read now
	missed int           // ticks in a row the session was left out of the list
}

// graceTicks is how many ticks in a row a session may be left out of the
// list before it leaves the board. Until then it stays, marked stale, as a
// herdr too busy to take connections can be left out. A session whose
// socket is gone leaves at once.
const graceTicks = 3

func newHub(version string) *hub {
	h := &hub{
		workers: map[string]*worker{},
		snaps:   map[string]Snapshot{},
		streams: map[chan struct{}]bool{},
		dirty:   make(chan struct{}, 1),
		built:   make(chan struct{}, 1),
	}
	h.publish(Build(nil, nil, nil, false, version))
	return h
}

// signal wakes whoever waits on c, unless it's due to wake already. Every
// channel signalled holds one.
func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// current is the board that's out.
func (h *hub) current() *published { return h.board.Load() }

// publish puts out b, unless it's the board that's out already.
func (h *hub) publish(b Board) {
	data, err := json.Marshal(b)
	if err != nil {
		log.Printf("board: %v", err)
		return
	}
	p := &published{seq: 1, json: data, board: b}
	if old := h.board.Load(); old != nil {
		if bytes.Equal(old.json, data) {
			return
		}
		p.seq = old.seq + 1
	}
	h.board.Store(p)
	h.mu.Lock()
	for c := range h.streams {
		signal(c)
	}
	h.mu.Unlock()
	signal(h.built)
}

// join signals the channel it returns each time a new board is out, until
// leave is called.
func (h *hub) join() (c chan struct{}, leave func()) {
	c = make(chan struct{}, 1)
	h.mu.Lock()
	h.streams[c] = true
	h.mu.Unlock()
	return c, func() {
		h.mu.Lock()
		delete(h.streams, c)
		h.mu.Unlock()
	}
}

// conn is a running session by name.
func (h *hub) conn(name string) (SessionConn, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if w := h.workers[name]; w != nil {
		return w.conn, true
	}
	return nil, false
}

// refresh has a session read again now.
func (h *hub) refresh(name string) {
	h.mu.Lock()
	w := h.workers[name]
	h.mu.Unlock()
	if w != nil {
		signal(w.kick)
	}
}

// record keeps w's latest read, unless w has been stopped.
func (h *hub) record(w *worker, snap Snapshot) {
	h.mu.Lock()
	ok := h.workers[w.name] == w
	if ok {
		h.snaps[w.name] = snap
	}
	h.mu.Unlock()
	if ok {
		signal(h.dirty)
	}
}

// setLinear records whether there's a Linear key and, unless fetched is nil,
// the issues fetched. Without a key there are none.
func (h *hub) setLinear(key bool, fetched map[string]linear.Issue) {
	h.mu.Lock()
	switch {
	case !key:
		h.issues = nil
	case fetched != nil:
		h.issues = fetched
	}
	h.linearKey = key
	h.mu.Unlock()
	signal(h.dirty)
}

// wanted is the Linear issues on the board's cards that aren't done, by the
// ID in their links.
func (h *hub) wanted() []string {
	var ids []string
	for _, s := range h.current().board.Sessions {
		for _, c := range s.Cards {
			if c.Column == string(task.GDone) {
				continue
			}
			for _, l := range c.Links {
				// Not by l.Linear.ID: the fetched issues are keyed by the ID
				// asked for, and Linear may answer with another identifier
				// (an issue that moved teams).
				if id, ok := task.LinearID(l.URL); ok && !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}

// runHub keeps the board current until ctx ends.
func (s *Server) runHub(ctx context.Context) {
	go s.watch(ctx)
	go s.builder(ctx)
	go s.followLinear(ctx)
}

// watch looks for herdr sessions that started or ended, now and every
// s.tick. It shuts the server down when the board is turned off or moved to
// another port, or when no session has run for s.idle.
func (s *Server) watch(ctx context.Context) {
	t := time.NewTicker(s.tick)
	defer t.Stop()
	seen := time.Now()
	var problem string
	for {
		if why := s.check(ctx, &seen, &problem); why != "" {
			log.Printf("shutting down: %s", why)
			s.cancel()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// check reads the settings and looks for sessions. It says why the server
// should shut down, if it should.
func (s *Server) check(ctx context.Context, seen *time.Time, problem *string) string {
	if s.cfg.Options != nil {
		opts, err := s.cfg.Options()
		switch {
		case err != nil:
			logOnce(problem, fmt.Sprintf("reading config: %v", err))
		case !opts.Board:
			return "the board is turned off"
		case opts.Port() != s.cfg.Port:
			return fmt.Sprintf("the board moved to port %d", opts.Port())
		default:
			*problem = ""
		}
	}
	var conns []SessionConn
	if s.cfg.Sessions != nil {
		conns = s.cfg.Sessions.List()
	}
	if s.track(ctx, conns) > 0 {
		*seen = time.Now()
	} else if time.Since(*seen) >= s.idle {
		return fmt.Sprintf("no herdr session for %v", s.idle)
	}
	return ""
}

// track starts a worker for each new session, and stops the workers of
// sessions that ended: those left out of the list graceTicks times in a row,
// or whose socket is gone. One left out fewer times stays, marked stale. It
// returns how many sessions it kept.
func (s *Server) track(ctx context.Context, conns []SessionConn) int {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	listed := map[string]bool{}
	for _, c := range conns {
		name := c.Name()
		listed[name] = true
		if w := h.workers[name]; w != nil {
			if w.missed > 0 {
				log.Printf("session %s: listed again", name)
				w.missed = 0
				signal(w.kick) // to show it isn't stale
			}
			continue
		}
		wctx, cancel := context.WithCancel(ctx)
		w := &worker{name: name, conn: c, cancel: cancel, kick: make(chan struct{}, 1)}
		h.workers[name] = w
		log.Printf("session %s: found", name)
		go s.work(wctx, w)
	}
	for name, w := range h.workers {
		if listed[name] {
			continue
		}
		w.missed++
		if w.missed < graceTicks && !w.conn.Ended() {
			if w.missed == 1 {
				log.Printf("session %s: not listed; kept for now", name)
			}
			if snap, ok := h.snaps[name]; ok && !snap.Stale {
				snap.Stale = true
				h.snaps[name] = snap
				signal(h.dirty)
			}
			continue
		}
		log.Printf("session %s: gone", name)
		w.cancel()
		delete(h.workers, name)
		delete(h.snaps, name)
		signal(h.dirty)
	}
	return len(h.workers)
}

// read is the outcome of reading a session once.
type read struct {
	snap Snapshot
	err  error
}

// goRead reads w's session in a goroutine, and sends what it read to done.
// Once ctx has ended, it reads nothing and returns false.
func (h *hub) goRead(ctx context.Context, w *worker, done chan<- read) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	h.reads.Add(1)
	go func() {
		defer h.reads.Done()
		done <- readSession(w.name, w.conn)
	}()
	return true
}

func readSession(name string, c SessionConn) read {
	live, err := c.Live()
	if err != nil {
		return read{err: err}
	}
	panes, err := c.Panes()
	if err != nil {
		return read{err: err}
	}
	st, err := c.Store().Load()
	if err != nil {
		return read{err: err}
	}
	return read{snap: Snapshot{Name: name, State: st, Live: live, Panes: panes}}
}

// work keeps one session's snapshot. It reads the session every s.poll,
// s.quiet after herdr's events stop coming, and when kicked. A read that
// fails, or takes longer than s.timeout, leaves the last good snapshot,
// marked stale. A read that hangs is left to finish, and no other starts
// meanwhile.
func (s *Server) work(ctx context.Context, w *worker) {
	events := make(chan struct{}, 1)
	go s.listen(ctx, w, events)
	poll := time.NewTicker(s.poll)
	defer poll.Stop()
	quiet := time.NewTimer(s.quiet)
	quiet.Stop()

	done := make(chan read, 1)
	var late <-chan time.Time // fires when the read under way takes too long
	reading, again := false, false
	snap := Snapshot{Name: w.name}
	var failing string
	// start reads the session. With a read under way, it reads again once
	// that's done, if soon is set.
	start := func(soon bool) {
		if reading {
			again = again || soon
			return
		}
		if s.hub.goRead(ctx, w, done) {
			reading, late = true, time.After(s.timeout)
		}
	}
	stale := func(err error) {
		snap.Stale = true
		s.hub.record(w, snap)
		logOnce(&failing, fmt.Sprintf("session %s: %v", w.name, err))
	}

	start(false)
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			start(false)
		case <-w.kick:
			start(true)
		case <-events:
			quiet.Reset(s.quiet)
		case <-quiet.C:
			start(true)
		case r := <-done:
			reading, late = false, nil
			if r.err != nil {
				stale(r.err)
			} else {
				snap = r.snap
				s.hub.record(w, snap)
				if failing != "" {
					log.Printf("session %s: answers again", w.name)
					failing = ""
				}
			}
			if again {
				again = false
				start(false)
			}
		case <-late:
			late = nil
			stale(fmt.Errorf("no answer within %v", s.timeout))
		}
	}
}

// listen signals events for each herdr event, and subscribes again after
// s.retry when the connection ends. herdr replays old events when a
// subscription starts, so they only say when to read again.
func (s *Server) listen(ctx context.Context, w *worker, events chan struct{}) {
	var failing string
	for {
		err := w.conn.Subscribe(ctx, func(herdr.Event) { signal(events) })
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logOnce(&failing, fmt.Sprintf("session %s events: %v", w.name, err))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.retry):
		}
	}
}

// builder makes the board each time something it's made from changes.
func (s *Server) builder(ctx context.Context) {
	h := s.hub
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.dirty:
		}
		h.mu.Lock()
		snaps := make([]Snapshot, 0, len(h.snaps))
		for _, snap := range h.snaps {
			snaps = append(snaps, snap)
		}
		issues, key := h.issues, h.linearKey
		h.mu.Unlock()
		h.publish(Build(snaps, s.summary, issues, key, s.cfg.Version))
	}
}

// summary is what a Claude pane's session log says.
func (s *Server) summary(p herdr.Pane) (claudelog.Summary, bool) {
	if s.cfg.Claude == nil {
		return claudelog.Summary{}, false
	}
	sum, ok, err := s.cfg.Claude.Get(p.Session.Value)
	return sum, ok && err == nil
}

// followLinear keeps the status of the Linear issues on cards that aren't
// done. It asks Linear every s.linearEvery, and as soon as the board has an
// issue it didn't ask about. When Linear fails, the board keeps the status
// it had.
func (s *Server) followLinear(ctx context.Context) {
	t := time.NewTicker(s.linearEvery)
	defer t.Stop()
	var asked []string
	var failing string
	refresh := func() {
		asked = s.hub.wanted()
		var c *linear.Client
		ok := false
		if s.cfg.Linear != nil {
			c, ok = s.cfg.Linear()
		}
		if !ok || c == nil {
			s.hub.setLinear(false, nil)
			return
		}
		if len(asked) == 0 {
			s.hub.setLinear(true, nil)
			return
		}
		lctx, cancel := context.WithTimeout(ctx, linearTimeout)
		issues, err := c.Issues(lctx, asked)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logOnce(&failing, err.Error())
		} else {
			failing = ""
		}
		s.hub.setLinear(true, issues)
	}

	refresh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh()
		case <-s.hub.built:
			if slices.ContainsFunc(s.hub.wanted(), func(id string) bool { return !slices.Contains(asked, id) }) {
				refresh()
			}
		}
	}
}

// logOnce logs msg unless it's the last one logged.
func logOnce(last *string, msg string) {
	if msg != *last {
		log.Print(msg)
		*last = msg
	}
}
