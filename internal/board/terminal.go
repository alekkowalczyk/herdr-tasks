package board

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/strip"
)

// A page shows a task tab's terminals from GET /api/tab/stream, and types
// into them with POST /api/pane/input.
//   - One poller per tab reads it for every page showing it: the layout
//     every s.layoutEvery, and the screen of each pane shown every
//     s.screenEvery. herdr has no event for new output, so it polls, and
//     sends a screen only when it changed.
//   - Each page has a queue that keeps only the latest layout and the latest
//     screen of each pane, so a slow page holds up nothing.
//   - The task strip is left out: the card shows what it does.

const (
	// maxOps is the most ops one input request may hold.
	maxOps = 64
	// maxInput is the most text, keys and paste one input request may hold,
	// in bytes.
	maxInput = 64 << 10
)

// termLayout is a layout event: where the tab's panes are, in herdr's
// cells. When zoomed, it has the focused pane alone, filling the area.
type termLayout struct {
	Zoomed bool       `json:"zoomed"`
	Area   herdr.Rect `json:"area"`
	Panes  []termPane `json:"panes"`
}

// termPane is a pane of a layout. Cols and Rows are its terminal's size.
type termPane struct {
	Pane    string `json:"pane"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
	Agent   string `json:"agent"`
	Title   string `json:"title"`
	Name    string `json:"name,omitempty"` // the pane's name in herdr
	Focused bool   `json:"focused"`
}

// termScreen is a screen event: what a pane shows.
type termScreen struct {
	Pane     string `json:"pane"`
	Cols     int    `json:"cols"`
	Rows     int    `json:"rows"`
	ANSI     string `json:"ansi"`
	Scrolled bool   `json:"scrolled"` // scrolled back in herdr
}

// tabKey names a tab of a herdr session, and paneKey a pane.
type tabKey struct{ session, tab string }
type paneKey struct{ session, pane string }

// terminals is the tabs pages show, and the panes being typed into.
type terminals struct {
	mu      sync.Mutex
	pollers map[tabKey]*poller
	typing  map[paneKey]*paneLock
}

// paneLock lets one input request at a time type into a pane.
type paneLock struct {
	mu    sync.Mutex
	users int // requests holding it or waiting for it
}

// poller reads one tab for every page showing it.
type poller struct {
	key    tabKey
	conn   SessionConn
	cancel context.CancelFunc
	alive  func() bool   // whether the board still has the session p reads
	gone   chan struct{} // closed once the session is gone

	// Under terminals.mu: the timer that closes the poller once no page has
	// shown the tab for s.linger, and how many pages have joined, so a
	// timer set before one joined doesn't close it.
	idle  *time.Timer
	joins int

	failing string // the last problem logged

	mu      sync.Mutex
	viewers map[*viewer]bool
	panes   map[string]bool       // every pane in the tab but the strip
	shown   []string              // the panes the layout shows, in its order
	geo     map[string]paneGeo    // by pane shown
	layout  []byte                // the latest layout event; nil until the first read
	screens map[string]paneScreen // the latest screen event of each pane shown
}

// paneGeo is what the latest layout read says about a pane shown.
type paneGeo struct {
	cols, rows int // inside its rect: the least its terminal can be
	scrolled   bool
}

// paneScreen is a pane's latest screen event, and its size.
type paneScreen struct {
	event        []byte
	widest, rows int
}

// streamEvent is an event for a page.
type streamEvent struct {
	name string
	data []byte
}

// viewer is one page's queue: the latest layout and the latest screen of
// each pane that it hasn't been sent yet.
type viewer struct {
	wake chan struct{} // signalled when there's something to send

	mu      sync.Mutex
	layout  []byte
	screens map[string][]byte
	order   []string // the panes in screens, in the order they came
}

func newViewer() *viewer {
	return &viewer{wake: make(chan struct{}, 1), screens: map[string][]byte{}}
}

func (v *viewer) putLayout(data []byte) {
	v.mu.Lock()
	v.layout = data
	v.mu.Unlock()
	signal(v.wake)
}

func (v *viewer) putScreen(pane string, data []byte) {
	v.mu.Lock()
	if _, ok := v.screens[pane]; !ok {
		v.order = append(v.order, pane)
	}
	v.screens[pane] = data
	v.mu.Unlock()
	signal(v.wake)
}

// take empties the queue. The layout comes first, so the page knows every
// pane it's then sent the screen of.
func (v *viewer) take() []streamEvent {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []streamEvent
	if v.layout != nil {
		out = append(out, streamEvent{"layout", v.layout})
		v.layout = nil
	}
	for _, pane := range v.order {
		out = append(out, streamEvent{"screen", v.screens[pane]})
	}
	clear(v.screens)
	v.order = v.order[:0]
	return out
}

// tabStream streams a task tab's terminals: the layout and the screen of
// each pane on connect, then each that changes. It pings while nothing
// does, and ends when the page goes away or the server shuts down. When the
// tab's session is gone, it sends a gone event, and ends.
func (s *Server) tabStream(w http.ResponseWriter, r *http.Request) {
	session, tab := r.URL.Query().Get("session"), r.URL.Query().Get("tab")
	if session == "" || tab == "" {
		writeError(w, http.StatusBadRequest, "A tab stream needs a session and a tab.")
		return
	}
	conn, ok := s.hub.conn(session)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Session %s isn't running.", session))
		return
	}
	if !s.onBoard(session, tab) {
		writeError(w, http.StatusNotFound, "That tab isn't on the board.")
		return
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}
	p, v := s.watchTab(tabKey{session, tab}, conn)
	defer s.unwatchTab(p, v)
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-v.wake:
			for _, ev := range v.take() {
				if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data); err != nil {
					return
				}
			}
			if rc.Flush() != nil {
				return
			}
		case <-t.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case <-p.gone:
			data, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("Session %s isn't running.", session)})
			if _, err := fmt.Fprintf(w, "event: gone\ndata: %s\n\n", data); err == nil {
				rc.Flush()
			}
			return
		}
	}
}

// onBoard reports whether a tab has a card on the board that's out: it's a
// task's tab, not a tool's, in a space that isn't hidden.
func (s *Server) onBoard(session, tab string) bool {
	for _, ss := range s.hub.current().board.Sessions {
		if ss.Name == session {
			return slices.ContainsFunc(ss.Cards, func(c Card) bool { return c.Tab == tab })
		}
	}
	return false
}

// watchTab adds a page to the tab's poller, starting one if there's none.
// The page's queue starts with the latest layout and every pane's screen.
func (s *Server) watchTab(key tabKey, conn SessionConn) (*poller, *viewer) {
	t := &s.terms
	t.mu.Lock()
	defer t.mu.Unlock()
	v := newViewer()
	p := t.pollers[key]
	if p == nil {
		ctx, cancel := context.WithCancel(s.ctx)
		p = &poller{key: key, conn: conn, cancel: cancel, gone: make(chan struct{}), viewers: map[*viewer]bool{v: true}, screens: map[string]paneScreen{}}
		p.alive = func() bool {
			// A session that ended and started again has another conn. Both
			// kinds of SessionConn compare by pointer.
			c, ok := s.hub.conn(key.session)
			return ok && c == conn
		}
		p.joins++
		t.pollers[key] = p
		go p.run(ctx, t, s.layoutEvery, s.screenEvery)
		return p, v
	}
	if p.idle != nil {
		p.idle.Stop()
		p.idle = nil
	}
	p.joins++
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.layout != nil {
		v.putLayout(p.layout)
		for _, id := range p.shown {
			if sc, ok := p.screens[id]; ok {
				v.putScreen(id, sc.event)
			}
		}
	}
	p.viewers[v] = true
	return p, v
}

// unwatchTab takes a page off its tab's poller. When it was the last, the
// poller keeps reading for s.linger, in case the page comes back, as when
// it reloads.
func (s *Server) unwatchTab(p *poller, v *viewer) {
	t := &s.terms
	t.mu.Lock()
	defer t.mu.Unlock()
	p.mu.Lock()
	delete(p.viewers, v)
	left := len(p.viewers)
	p.mu.Unlock()
	if left > 0 {
		return
	}
	joins := p.joins
	p.idle = time.AfterFunc(s.linger, func() { s.closePoller(p, joins) })
}

// closePoller stops p, unless a page has joined it since it had joins.
func (s *Server) closePoller(p *poller, joins int) {
	t := &s.terms
	t.mu.Lock()
	defer t.mu.Unlock()
	if p.joins != joins || t.pollers[p.key] != p {
		return
	}
	p.cancel()
	delete(t.pollers, p.key)
}

// run reads the tab until ctx ends, or until the session is gone: the
// layout first, then the screens of the panes it shows. While the layout
// can't be read, neither are the screens.
func (p *poller) run(ctx context.Context, t *terminals, layoutEvery, screenEvery time.Duration) {
	read, gone := p.readLayout()
	if gone {
		p.end(t)
		return
	}
	if read {
		p.readScreens(ctx)
	}
	layout := time.NewTicker(layoutEvery)
	defer layout.Stop()
	screens := time.NewTicker(screenEvery)
	defer screens.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-layout.C:
			if ctx.Err() != nil {
				continue
			}
			if read, gone = p.readLayout(); gone {
				p.end(t)
				return
			}
		case <-screens.C:
			if read {
				p.readScreens(ctx)
			}
		}
	}
}

// end stops a poller whose session is gone, and tells its pages. It's no
// longer the tab's poller first, so a page that comes later is told the
// session isn't running, rather than joining it.
func (p *poller) end(t *terminals) {
	t.mu.Lock()
	if t.pollers[p.key] == p {
		delete(t.pollers, p.key)
	}
	t.mu.Unlock()
	p.cancel()
	close(p.gone)
}

// readLayout reads which panes the tab has and where they are, and sends
// the layout to the pages when it changed. A tab that's gone has no panes.
// It reports whether it read the layout, and whether the session is gone:
// herdr doesn't answer, and the board has let the session go.
func (p *poller) readLayout() (read, gone bool) {
	all, err := p.conn.Panes()
	if err != nil {
		if p.alive != nil && !p.alive() {
			return false, true
		}
		p.problem(err)
		return false, false
	}
	var mine []herdr.Pane
	for _, pn := range all {
		if pn.TabID == p.key.tab && pn.Label != strip.Label {
			mine = append(mine, pn)
		}
	}
	var lay *herdr.TabLayout
	if len(mine) > 0 {
		if lay, err = p.conn.TabLayout(mine[0].ID); err != nil {
			p.problem(err)
			return false, false
		}
	}
	p.failing = ""

	panes := map[string]bool{}
	byID := map[string]herdr.Pane{}
	for _, pn := range mine {
		panes[pn.ID] = true
		byID[pn.ID] = pn
	}
	type placed struct {
		pane    herdr.Pane
		rect    herdr.Rect
		focused bool
	}
	var shown []placed
	ev := termLayout{Panes: []termPane{}}
	if lay != nil {
		ev.Area = lay.Area
		// herdr gives a zoomed tab's panes their unzoomed rects.
		if pn, ok := byID[lay.Focused]; ok && lay.Zoomed {
			ev.Zoomed = true
			shown = append(shown, placed{pn, lay.Area, true})
		} else {
			for _, lp := range lay.Panes {
				if pn, ok := byID[lp.PaneID]; ok {
					shown = append(shown, placed{pn, lp.Rect, lp.Focused})
				}
			}
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	geo := map[string]paneGeo{}
	var order []string
	for _, sp := range shown {
		id := sp.pane.ID
		g := paneGeo{cols: max(sp.rect.Width-2, 0), rows: sp.pane.Scroll.Rows, scrolled: sp.pane.Scroll.Offset > 0}
		if g.rows <= 0 {
			g.rows = max(sp.rect.Height-2, 0)
		}
		geo[id] = g
		order = append(order, id)
		sc := p.screens[id]
		ev.Panes = append(ev.Panes, termPane{
			Pane: id, X: sp.rect.X, Y: sp.rect.Y, Width: sp.rect.Width, Height: sp.rect.Height,
			Cols: max(g.cols, sc.widest), Rows: max(g.rows, sc.rows),
			Agent: sp.pane.Agent, Title: sp.pane.Title, Name: sp.pane.Label, Focused: sp.focused,
		})
	}
	for id := range p.screens {
		if _, ok := geo[id]; !ok {
			delete(p.screens, id)
		}
	}
	p.panes, p.shown, p.geo = panes, order, geo
	data, err := json.Marshal(ev)
	if err != nil || bytes.Equal(data, p.layout) {
		return true, false
	}
	p.layout = data
	for v := range p.viewers {
		v.putLayout(data)
	}
	return true, false
}

// readScreens reads each pane shown, and sends the pages each screen that
// changed: its text, its size, or whether it's scrolled back.
func (p *poller) readScreens(ctx context.Context) {
	p.mu.Lock()
	shown, geo := p.shown, p.geo
	p.mu.Unlock()
	for _, id := range shown {
		if ctx.Err() != nil {
			return
		}
		text, err := p.conn.ReadScreen(id)
		if err != nil {
			continue // the pane may have just closed; the next layout read finds out
		}
		text = cleanScreen(text)
		widest, rows := screenSize(text)
		g := geo[id]
		data, err := json.Marshal(termScreen{Pane: id, Cols: max(g.cols, widest), Rows: max(g.rows, rows), ANSI: text, Scrolled: g.scrolled})
		if err != nil {
			continue
		}
		p.mu.Lock()
		if !bytes.Equal(p.screens[id].event, data) {
			p.screens[id] = paneScreen{event: data, widest: widest, rows: rows}
			for v := range p.viewers {
				v.putScreen(id, data)
			}
		}
		p.mu.Unlock()
	}
}

func (p *poller) problem(err error) {
	logOnce(&p.failing, fmt.Sprintf("session %s tab %s: %v", p.key.session, p.key.tab, err))
}

// notSGR matches what cleanScreen replaces: a control character other than
// \t, \r and \n, C1 ones included, and an escape that doesn't start an SGR
// code. An SGR code matches too, whole, and is kept.
var notSGR = regexp.MustCompile(`\x1b(?:\[[0-9:;]*m)?|[\x00-\x08\x0b\x0c\x0e-\x1a\x1c-\x1f\x7f-\x{9f}]`)

// cleanScreen keeps what herdr means a screen to hold: text, rows joined by
// \r\n, and SGR codes. A pane's program can print characters that herdr
// keeps as they are, such as U+009B, which the browser's terminal takes for
// an escape: a query it would answer, or a command to clear the screen or
// report the mouse. Each becomes U+FFFD, which takes the cell it took.
func cleanScreen(text string) string {
	return notSGR.ReplaceAllStringFunc(text, func(m string) string {
		if strings.HasPrefix(m, "\x1b[") {
			return m
		}
		return "\uFFFD"
	})
}

// screenSize is how wide a pane's screen is at its widest row, in cells,
// and how many rows it has. herdr leaves out trailing blank rows, and
// trailing spaces that aren't styled, so the terminal may be bigger.
func screenSize(text string) (widest, rows int) {
	if text == "" {
		return 0, 0
	}
	for _, row := range strings.Split(text, "\r\n") {
		widest = max(widest, ansi.StringWidth(row))
		rows++
	}
	return widest, rows
}

// inputRequest is what POST /api/pane/input takes: ops to apply to a pane,
// in order.
type inputRequest struct {
	Session string    `json:"session"`
	Pane    string    `json:"pane"`
	Ops     []inputOp `json:"ops"`
}

// inputOp is one of: text typed as it is, keys pressed by herdr's names for
// them, or text pasted.
type inputOp struct {
	Text  *string  `json:"text"`
	Keys  []string `json:"keys"`
	Paste *string  `json:"paste"`
}

// checkOps says what's wrong with an input request's ops, if anything, and
// the status to answer.
func checkOps(ops []inputOp) (int, string) {
	if len(ops) == 0 || len(ops) > maxOps {
		return http.StatusBadRequest, fmt.Sprintf("Send 1 to %d ops at a time.", maxOps)
	}
	total := 0
	for _, op := range ops {
		kinds, empty := 0, false
		if op.Text != nil {
			kinds++
			total += len(*op.Text)
			empty = *op.Text == ""
		}
		if op.Keys != nil {
			kinds++
			for _, k := range op.Keys {
				total += len(k)
			}
			empty = len(op.Keys) == 0
		}
		if op.Paste != nil {
			kinds++
			total += len(*op.Paste)
			empty = *op.Paste == ""
		}
		if kinds != 1 || empty {
			return http.StatusBadRequest, "Each op is some text, keys or a paste."
		}
	}
	if total > maxInput {
		return http.StatusRequestEntityTooLarge, "That's more than 64 KiB to type at once."
	}
	return 0, ""
}

// paneInput types into a pane of a task's tab: the ops in order, with no
// other request's in between. When one fails, the rest are dropped.
func (s *Server) paneInput(w http.ResponseWriter, r *http.Request) {
	var req inputRequest
	// JSON escapes can make the body several times longer than what it holds.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*maxInput)).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "That's more than 64 KiB to type at once.")
		} else {
			writeError(w, http.StatusBadRequest, "That isn't an input request.")
		}
		return
	}
	if req.Session == "" || req.Pane == "" {
		writeError(w, http.StatusBadRequest, "An input request needs a session and a pane.")
		return
	}
	if status, msg := checkOps(req.Ops); status != 0 {
		writeError(w, status, msg)
		return
	}
	conn, ok := s.hub.conn(req.Session)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Session %s isn't running.", req.Session))
		return
	}
	tab, err := s.paneTab(req.Session, conn, req.Pane)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": herdrMessage(err), "applied": 0})
		return
	}
	if tab == "" || !s.onBoard(req.Session, tab) {
		writeError(w, http.StatusForbidden, "You can only type into the panes of a task's tab.")
		return
	}
	unlock := s.lockPane(paneKey{req.Session, req.Pane})
	defer unlock()
	for i, op := range req.Ops {
		var err error
		switch {
		case op.Text != nil:
			err = conn.TypeText(req.Pane, *op.Text)
		case op.Keys != nil:
			err = conn.SendKeys(req.Pane, op.Keys...)
		default:
			err = conn.Paste(req.Pane, *op.Paste)
		}
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": herdrMessage(err), "applied": i})
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// paneTab is the tab a pane is in, or "" when there's no such pane or it's
// the task strip. A tab's poller knows its panes; for others, it asks herdr.
func (s *Server) paneTab(session string, conn SessionConn, pane string) (string, error) {
	t := &s.terms
	t.mu.Lock()
	for key, p := range t.pollers {
		if key.session != session {
			continue
		}
		p.mu.Lock()
		in := p.panes[pane]
		p.mu.Unlock()
		if in {
			t.mu.Unlock()
			return key.tab, nil
		}
	}
	t.mu.Unlock()
	panes, err := conn.Panes()
	if err != nil {
		return "", err
	}
	for _, p := range panes {
		if p.ID == pane && p.Label != strip.Label {
			return p.TabID, nil
		}
	}
	return "", nil
}

// lockPane waits until no other request types into the pane, and returns
// the func that lets the next one.
func (s *Server) lockPane(key paneKey) (unlock func()) {
	t := &s.terms
	t.mu.Lock()
	l := t.typing[key]
	if l == nil {
		l = &paneLock{}
		t.typing[key] = l
	}
	l.users++
	t.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		t.mu.Lock()
		defer t.mu.Unlock()
		l.users--
		if l.users == 0 {
			delete(t.typing, key)
		}
	}
}

// herdrMessage is what herdr said went wrong.
func herdrMessage(err error) string {
	var he *herdr.Error
	if errors.As(err, &he) && he.Message != "" {
		return he.Message
	}
	return err.Error()
}
