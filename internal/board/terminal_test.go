package board

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/strip"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

func (c *fakeConn) ReadScreen(paneID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reads == nil {
		c.reads = map[string]int{}
	}
	c.reads[paneID]++
	if c.down {
		return "", errDown
	}
	text, ok := c.screens[paneID]
	if !ok {
		return "", &herdr.Error{Code: "pane_not_found", Message: "no pane " + paneID}
	}
	return text, nil
}

func (c *fakeConn) TabLayout(paneID string) (*herdr.TabLayout, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return nil, errDown
	}
	for _, p := range c.panes {
		if l, ok := c.layouts[p.TabID]; ok && p.ID == paneID {
			cp := *l
			cp.Panes = slices.Clone(l.Panes)
			return &cp, nil
		}
	}
	return nil, &herdr.Error{Code: "pane_not_found", Message: "no pane " + paneID}
}

func (c *fakeConn) TypeText(paneID, text string) error { return c.input(paneID, "text", text) }
func (c *fakeConn) Paste(paneID, text string) error    { return c.input(paneID, "paste", text) }

// SendKeys checks every key first, as herdr does, and types none when one
// is wrong.
func (c *fakeConn) SendKeys(paneID string, keys ...string) error {
	if slices.Contains(keys, "bogus") {
		return &herdr.Error{Code: "invalid_key", Message: "unsupported key bogus"}
	}
	return c.input(paneID, "keys", strings.Join(keys, " "))
}

func (c *fakeConn) input(pane, op, value string) error {
	time.Sleep(c.slowOp)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.typed = append(c.typed, pane+" "+op+" "+value)
	return nil
}

// show has a pane show text.
func (c *fakeConn) show(pane, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.screens[pane] = text
}

// scroll has a pane scrolled back by rows.
func (c *fakeConn) scroll(pane string, rows int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.panes {
		if c.panes[i].ID == pane {
			c.panes[i].Scroll.Offset = rows
		}
	}
}

// zoom zooms a tab in on a pane.
func (c *fakeConn) zoom(tab, pane string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.layouts[tab].Zoomed, c.layouts[tab].Focused = true, pane
}

// stopAnswering has herdr stop answering, as when it quits.
func (c *fakeConn) stopAnswering() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.down = true
}

func (c *fakeConn) readsOf(pane string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads[pane]
}

func (c *fakeConn) typedOps() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.typed)
}

// termTab is a session with a task tab, w1:t1, holding a Claude pane, a
// shell and the task strip; and a tool tab, w1:t2, holding a shell.
func termTab(t *testing.T) *fakeConn {
	t.Helper()
	work := newConn(t, "work", oneSpace(herdr.Tab{ID: "w1:t1"}, herdr.Tab{ID: "w1:t2"}),
		&task.Task{TabID: "w1:t1", Title: "Login page", Stage: task.Active, Since: now},
		&task.Task{TabID: "w1:t2", Stage: task.Tool, Since: now})
	work.panes = []herdr.Pane{
		{ID: "w1:p1", TabID: "w1:t1", Agent: "claude", Title: "Fixing the login page", Scroll: herdr.Scroll{Rows: 10}},
		{ID: "w1:p2", TabID: "w1:t1", Scroll: herdr.Scroll{Rows: 10}},
		{ID: "w1:p3", TabID: "w1:t1", Label: strip.Label, Scroll: herdr.Scroll{Rows: 6}},
		{ID: "w1:p4", TabID: "w1:t2", Scroll: herdr.Scroll{Rows: 20}},
	}
	work.layouts = map[string]*herdr.TabLayout{
		"w1:t1": {TabID: "w1:t1", Focused: "w1:p1", Area: herdr.Rect{X: 26, Y: 1, Width: 80, Height: 20},
			Panes: []herdr.LayoutPane{
				{PaneID: "w1:p1", Focused: true, Rect: herdr.Rect{X: 26, Y: 1, Width: 42, Height: 12}},
				{PaneID: "w1:p2", Rect: herdr.Rect{X: 68, Y: 1, Width: 38, Height: 12}},
				{PaneID: "w1:p3", Rect: herdr.Rect{X: 26, Y: 13, Width: 80, Height: 8}},
			}},
		"w1:t2": {TabID: "w1:t2", Focused: "w1:p4", Area: herdr.Rect{X: 26, Y: 1, Width: 80, Height: 20},
			Panes: []herdr.LayoutPane{{PaneID: "w1:p4", Focused: true, Rect: herdr.Rect{X: 26, Y: 1, Width: 80, Height: 20}}}},
	}
	work.screens = map[string]string{
		"w1:p1": "\x1b[0m\x1b[1mhello\x1b[0m\r\nworld",
		"w1:p2": "$ ls",
		"w1:p3": "Login page · Working",
		"w1:p4": "$ top",
	}
	return work
}

// quickTerms reads tabs often, and closes a poller soon after its last page
// goes.
func quickTerms(s *Server) {
	s.layoutEvery, s.screenEvery, s.linger = 50*time.Millisecond, 20*time.Millisecond, 300*time.Millisecond
}

// tabStream opens the stream of a tab's terminals.
func (ts *testServer) tabStream(t *testing.T, session, tab string) (<-chan sseEvent, func()) {
	t.Helper()
	return ts.sse(t, "/api/tab/stream?session="+url.QueryEscape(session)+"&tab="+url.QueryEscape(tab))
}

// nextEvent waits up to 3 s for a stream's next event.
func nextEvent(t *testing.T, events <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("the stream ended")
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no event within 3s")
	}
	return sseEvent{}
}

func nextLayout(t *testing.T, events <-chan sseEvent) termLayout {
	t.Helper()
	ev := nextEvent(t, events)
	var l termLayout
	if ev.name != "layout" || json.Unmarshal([]byte(ev.data), &l) != nil {
		t.Fatalf("got %s %s, want a layout", ev.name, ev.data)
	}
	return l
}

func nextScreen(t *testing.T, events <-chan sseEvent) termScreen {
	t.Helper()
	ev := nextEvent(t, events)
	var s termScreen
	if ev.name != "screen" || json.Unmarshal([]byte(ev.data), &s) != nil {
		t.Fatalf("got %s %s, want a screen", ev.name, ev.data)
	}
	return s
}

// quiet checks a stream sends nothing for d.
func quiet(t *testing.T, events <-chan sseEvent, d time.Duration) {
	t.Helper()
	select {
	case ev := <-events:
		t.Errorf("got %s %s, want nothing", ev.name, ev.data)
	case <-time.After(d):
	}
}

// opening reads what a stream sends first: the layout, then the screen of
// each pane in it.
func opening(t *testing.T, events <-chan sseEvent) (termLayout, []termScreen) {
	t.Helper()
	l := nextLayout(t, events)
	var screens []termScreen
	for range l.Panes {
		screens = append(screens, nextScreen(t, events))
	}
	return l, screens
}

// watching is how many tabs have a poller.
func (ts *testServer) watching() int {
	ts.terms.mu.Lock()
	defer ts.terms.mu.Unlock()
	return len(ts.terms.pollers)
}

type inputReply struct {
	Error   string `json:"error"`
	Applied int    `json:"applied"`
}

// input posts body to /api/pane/input. It doesn't fail the test, so it can
// run in a goroutine.
func (ts *testServer) input(body any) (int, inputReply, error) {
	var r io.Reader
	if s, ok := body.(string); ok {
		r = strings.NewReader(s)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, inputReply{}, err
		}
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequest("POST", ts.url("/api/pane/input"), r)
	if err != nil {
		return 0, inputReply{}, err
	}
	req.Header.Set("Authorization", "Bearer "+ts.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := plain.Do(req)
	if err != nil {
		return 0, inputReply{}, err
	}
	defer resp.Body.Close()
	var reply inputReply
	b, _ := io.ReadAll(resp.Body)
	if len(b) > 0 {
		if err := json.Unmarshal(b, &reply); err != nil {
			return resp.StatusCode, reply, fmt.Errorf("answer %d %q: %v", resp.StatusCode, b, err)
		}
	}
	return resp.StatusCode, reply, nil
}

// typeInto types ops into a pane, and returns the status.
func (ts *testServer) typeInto(t *testing.T, pane string, ops ...map[string]any) (int, inputReply) {
	t.Helper()
	code, reply, err := ts.input(map[string]any{"session": "work", "pane": pane, "ops": ops})
	if err != nil {
		t.Fatal(err)
	}
	return code, reply
}

func TestStreamSendsLayoutThenScreens(t *testing.T) {
	work := termTab(t)
	ts := startServer(t, Config{Sessions: running(work)}, quickTerms)
	ts.shows(t, "work", "w1:t1")
	events, _ := ts.tabStream(t, "work", "w1:t1")

	ev := nextEvent(t, events)
	if ev.name != "layout" {
		t.Fatalf("first event = %s %s, want the layout", ev.name, ev.data)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(ev.data), &raw); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, raw); got != "area panes zoomed" {
		t.Errorf("layout keys = %s", got)
	}
	if got := keys(t, raw["area"]); got != "height width x y" {
		t.Errorf("area keys = %s", got)
	}
	if got := keys(t, raw["panes"].([]any)[0]); got != "agent cols focused height pane rows title width x y" {
		t.Errorf("pane keys = %s", got)
	}
	var l termLayout
	if err := json.Unmarshal([]byte(ev.data), &l); err != nil {
		t.Fatal(err)
	}
	want := termLayout{Area: herdr.Rect{X: 26, Y: 1, Width: 80, Height: 20}, Panes: []termPane{
		{Pane: "w1:p1", X: 26, Y: 1, Width: 42, Height: 12, Cols: 40, Rows: 10, Agent: "claude", Title: "Fixing the login page", Focused: true},
		{Pane: "w1:p2", X: 68, Y: 1, Width: 38, Height: 12, Cols: 36, Rows: 10},
	}}
	if !reflect.DeepEqual(l, want) {
		t.Errorf("layout =\n%+v\nwant\n%+v", l, want)
	}

	// Then each pane's screen, in the layout's order.
	ev = nextEvent(t, events)
	raw = nil
	if err := json.Unmarshal([]byte(ev.data), &raw); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, raw); ev.name != "screen" || got != "ansi cols pane rows scrolled" {
		t.Errorf("event %s keys = %s", ev.name, got)
	}
	var first termScreen
	if err := json.Unmarshal([]byte(ev.data), &first); err != nil {
		t.Fatal(err)
	}
	screens := []termScreen{first, nextScreen(t, events)}
	wantScreens := []termScreen{
		{Pane: "w1:p1", Cols: 40, Rows: 10, ANSI: "\x1b[0m\x1b[1mhello\x1b[0m\r\nworld"},
		{Pane: "w1:p2", Cols: 36, Rows: 10, ANSI: "$ ls"},
	}
	if !reflect.DeepEqual(screens, wantScreens) {
		t.Errorf("screens =\n%+v\nwant\n%+v", screens, wantScreens)
	}
}

func TestStreamSkipsUnchangedScreens(t *testing.T) {
	work := termTab(t)
	ts := startServer(t, Config{Sessions: running(work)}, quickTerms)
	ts.shows(t, "work", "w1:t1")
	events, _ := ts.tabStream(t, "work", "w1:t1")
	opening(t, events)

	// Nothing changed, so nothing is sent, though the screens are read again
	// and again.
	n := work.readsOf("w1:p1")
	quiet(t, events, 300*time.Millisecond)
	if got := work.readsOf("w1:p1"); got < n+5 {
		t.Errorf("w1:p1 read %d times in 300ms", got-n)
	}

	// New output in one pane sends that pane's screen alone.
	work.show("w1:p2", "$ ls\r\nREADME.md")
	if s := nextScreen(t, events); s != (termScreen{Pane: "w1:p2", Cols: 36, Rows: 10, ANSI: "$ ls\r\nREADME.md"}) {
		t.Errorf("screen = %+v", s)
	}
	quiet(t, events, 300*time.Millisecond)

	// Scrolled back in herdr, a pane is sent again, though its text is the
	// same.
	work.scroll("w1:p1", 4)
	if s := nextScreen(t, events); s != (termScreen{Pane: "w1:p1", Cols: 40, Rows: 10, ANSI: "\x1b[0m\x1b[1mhello\x1b[0m\r\nworld", Scrolled: true}) {
		t.Errorf("screen = %+v", s)
	}
	quiet(t, events, 300*time.Millisecond)
}

func TestSecondViewerGetsCurrentScreens(t *testing.T) {
	work := termTab(t)
	ts := startServer(t, Config{Sessions: running(work)}, quickTerms)
	ts.shows(t, "work", "w1:t1")
	first, _ := ts.tabStream(t, "work", "w1:t1")
	l1, s1 := opening(t, first)

	// Nothing changes in herdr, yet the second page gets every screen.
	second, _ := ts.tabStream(t, "work", "w1:t1")
	l2, s2 := opening(t, second)
	if !reflect.DeepEqual(l2, l1) || !reflect.DeepEqual(s2, s1) {
		t.Errorf("second page got\n%+v %+v\nwant\n%+v %+v", l2, s2, l1, s1)
	}
	if len(s2) != 2 {
		t.Errorf("second page got %d screens, want 2", len(s2))
	}
	quiet(t, first, 200*time.Millisecond)
	if n := ts.watching(); n != 1 {
		t.Errorf("%d pollers, want one for both pages", n)
	}
}

func TestStreamLeavesOutTheStrip(t *testing.T) {
	work := termTab(t)
	ts := startServer(t, Config{Sessions: running(work)}, quickTerms)
	ts.shows(t, "work", "w1:t1")
	events, _ := ts.tabStream(t, "work", "w1:t1")
	l, screens := opening(t, events)
	for _, p := range l.Panes {
		if p.Pane == "w1:p3" {
			t.Errorf("the layout has the strip: %+v", l)
		}
	}
	work.show("w1:p3", "Login page · Your turn")
	quiet(t, events, 300*time.Millisecond)
	for _, s := range screens {
		if s.Pane == "w1:p3" {
			t.Errorf("the strip's screen was sent: %+v", s)
		}
	}
	if n := work.readsOf("w1:p3"); n != 0 {
		t.Errorf("the strip was read %d times", n)
	}
}

func TestZoomedTabShowsTheFocusedPane(t *testing.T) {
	work := termTab(t)
	work.zoom("w1:t1", "w1:p2")
	ts := startServer(t, Config{Sessions: running(work)}, quickTerms)
	ts.shows(t, "work", "w1:t1")
	events, _ := ts.tabStream(t, "work", "w1:t1")
	l, screens := opening(t, events)
	// herdr gives the panes' unzoomed rects, so the pane fills the area.
	want := termLayout{Zoomed: true, Area: herdr.Rect{X: 26, Y: 1, Width: 80, Height: 20}, Panes: []termPane{
		{Pane: "w1:p2", X: 26, Y: 1, Width: 80, Height: 20, Cols: 78, Rows: 10, Focused: true},
	}}
	if !reflect.DeepEqual(l, want) {
		t.Errorf("layout =\n%+v\nwant\n%+v", l, want)
	}
	if want := []termScreen{{Pane: "w1:p2", Cols: 78, Rows: 10, ANSI: "$ ls"}}; !reflect.DeepEqual(screens, want) {
		t.Errorf("screens = %+v, want %+v", screens, want)
	}
	quiet(t, events, 200*time.Millisecond)
	if n := work.readsOf("w1:p1"); n != 0 {
		t.Errorf("the hidden pane was read %d times", n)
	}
}

func TestColsUseTheWidestRow(t *testing.T) {
	for _, c := range []struct {
		ansi         string
		widest, rows int
	}{
		{"", 0, 0},
		{"$ ls", 4, 1},
		// SGR codes take no room; wide characters take two cells.
		{"\x1b[0m\x1b[1mab\x1b[0m\r\n日本語\r\n\x1b[0m\x1b[38;2;1;2;3m\x1b[4:3mabcd\x1b[0m", 6, 3},
		// A blank row in the middle still counts.
		{"a\r\n\r\nb", 1, 3},
	} {
		if w, r := screenSize(c.ansi); w != c.widest || r != c.rows {
			t.Errorf("screenSize(%q) = %d, %d, want %d, %d", c.ansi, w, r, c.widest, c.rows)
		}
	}

	// w1:p1's rect is 42 wide, which leaves 40 inside its border; a 50-wide
	// rule means its terminal is wider than that.
	work := termTab(t)
	ts := startServer(t, Config{Sessions: running(work)}, quickTerms)
	ts.shows(t, "work", "w1:t1")
	events, _ := ts.tabStream(t, "work", "w1:t1")
	opening(t, events)
	rule := "\x1b[0m\x1b[38;5;8m" + strings.Repeat("─", 50) + "\x1b[0m"
	work.show("w1:p1", "hello\r\n"+rule)
	// The screen, and the layout read next, in either order.
	for range 2 {
		ev := nextEvent(t, events)
		switch ev.name {
		case "screen":
			var s termScreen
			if err := json.Unmarshal([]byte(ev.data), &s); err != nil || s.Pane != "w1:p1" || s.Cols != 50 || s.Rows != 10 {
				t.Errorf("screen = %s, want w1:p1 at 50x10", ev.data)
			}
		case "layout":
			var l termLayout
			if err := json.Unmarshal([]byte(ev.data), &l); err != nil || len(l.Panes) != 2 || l.Panes[0].Cols != 50 || l.Panes[1].Cols != 36 {
				t.Errorf("layout = %s, want w1:p1 50 cols and w1:p2 36", ev.data)
			}
		default:
			t.Errorf("got %s %s", ev.name, ev.data)
		}
	}
}

// A pane's program can print characters the browser's terminal would take
// for escapes, which it would answer by typing into the pane. They're sent
// as U+FFFD, and SGR codes as they are.
func TestScreensAreCleaned(t *testing.T) {
	for in, want := range map[string]string{
		"":                        "",
		"\x1b[0m\x1b[1mhi\x1b[0m": "\x1b[0m\x1b[1mhi\x1b[0m",
		"\x1b[38:2::1:2:3;4:3mx":  "\x1b[38:2::1:2:3;4:3mx",
		"a\r\nb\tc":               "a\r\nb\tc",
		// C1 controls: CSI (DA1, CPR), DCS (DECRQSS) and OSC, ended by ST.
		"A[\u009bc]":          "A[\uFFFDc]",
		"B[\u009b6n]":         "B[\uFFFD6n]",
		"C[\u0090$qm\u009c]":  "C[\uFFFD$qm\uFFFD]",
		"D[\u009d11;?\u009c]": "D[\uFFFD11;?\uFFFD]",
		"\u0080\u009f":        "\uFFFD\uFFFD",
		// C0 controls and DEL.
		"\x00\x07\x08\x10\x11\x7f": "\uFFFD\uFFFD\uFFFD\uFFFD\uFFFD\uFFFD",
		// Escapes that aren't SGR codes: their ESC goes.
		"\x1b[c":             "\uFFFD[c",
		"\x1b[2J\x1b[?1000h": "\uFFFD[2J\uFFFD[?1000h",
		"\x1b]11;?\x07":      "\uFFFD]11;?\uFFFD",
		"\x1bP$qm\x1b\\":     "\uFFFDP$qm\uFFFD\\",
		"\x1b":               "\uFFFD",
		"日本語 é ─":            "日本語 é ─",
	} {
		if got := cleanScreen(in); got != want {
			t.Errorf("cleanScreen(%q) = %q, want %q", in, got, want)
		}
	}

	work := termTab(t)
	work.screens["w1:p2"] = "$ printf '\u009bc'\r\n\u009bc"
	ts := startServer(t, Config{Sessions: running(work)}, quickTerms)
	ts.shows(t, "work", "w1:t1")
	events, _ := ts.tabStream(t, "work", "w1:t1")
	_, screens := opening(t, events)
	if want := (termScreen{Pane: "w1:p2", Cols: 36, Rows: 10, ANSI: "$ printf '\uFFFDc'\r\n\uFFFDc"}); len(screens) != 2 || screens[1] != want {
		t.Errorf("screens = %+v, want w1:p2's as %+v", screens, want)
	}
}

// When herdr quits, the board lets the session go, and a page showing one
// of its tabs is told so. Then nothing reads the tab.
func TestStreamEndsWhenTheSessionGoes(t *testing.T) {
	work := termTab(t)
	sessions := running(work)
	ts := startServer(t, Config{Sessions: sessions}, quickTerms)
	ts.shows(t, "work", "w1:t1")
	events, _ := ts.tabStream(t, "work", "w1:t1")
	opening(t, events)

	// While herdr doesn't answer but the board still has the session, the
	// page waits, and the screens aren't read: the layout read finds out
	// first.
	work.stopAnswering()
	time.Sleep(150 * time.Millisecond) // three layout reads
	n := work.readsOf("w1:p1")
	quiet(t, events, 300*time.Millisecond)
	if got := work.readsOf("w1:p1"); got != n {
		t.Errorf("w1:p1 read %d times while herdr didn't answer", got-n)
	}
	if ts.watching() != 1 {
		t.Errorf("%d pollers, want 1", ts.watching())
	}

	// herdr's socket goes, so the board lets the session go.
	work.end()
	sessions.set()
	ev := nextEvent(t, events)
	if ev.name != "gone" || ev.data != `{"error":"Session work isn't running."}` {
		t.Errorf("got %s %s, want gone", ev.name, ev.data)
	}
	select {
	case ev, ok := <-events:
		if ok {
			t.Errorf("got %s %s, want the stream to end", ev.name, ev.data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the stream is still open 3s after the session went")
	}
	if n := ts.watching(); n != 0 {
		t.Errorf("%d pollers once the session went", n)
	}
	time.Sleep(50 * time.Millisecond) // for a read under way to finish
	n = work.readsOf("w1:p1")
	time.Sleep(200 * time.Millisecond)
	if got := work.readsOf("w1:p1"); got != n {
		t.Errorf("w1:p1 read %d times after the session went", got-n)
	}
	// A page that comes now is told the session isn't running.
	if code := status(t, ts.port, "/api/tab/stream?session=work&tab=w1:t1", ts.key); code != http.StatusNotFound {
		t.Errorf("a stream of the gone session: %d, want 404", code)
	}
}

func TestInputAppliesOpsInOrder(t *testing.T) {
	work := termTab(t)
	// Each op takes a while, so two requests would interleave if they could.
	work.slowOp = 5 * time.Millisecond
	ts := startServer(t, Config{Sessions: running(work)}, quickTerms)
	ts.shows(t, "work", "w1:t1")

	ops := func(who string) []map[string]any {
		return []map[string]any{{"text": who + "1"}, {"keys": []string{who, "enter"}}, {"paste": who + "3"}, {"text": who + "4"}}
	}
	var wg sync.WaitGroup
	for _, who := range []string{"a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, reply, err := ts.input(map[string]any{"session": "work", "pane": "w1:p1", "ops": ops(who)})
			if err != nil || code != http.StatusNoContent {
				t.Errorf("request %s: %d %+v %v", who, code, reply, err)
			}
		}()
	}
	wg.Wait()

	typed := func(who string) []string {
		return []string{"w1:p1 text " + who + "1", "w1:p1 keys " + who + " enter", "w1:p1 paste " + who + "3", "w1:p1 text " + who + "4"}
	}
	got := work.typedOps()
	if !slices.Equal(got, append(typed("a"), typed("b")...)) && !slices.Equal(got, append(typed("b"), typed("a")...)) {
		t.Errorf("typed %q, want one request's ops in order, then the other's", got)
	}
}

func TestInputStopsAtTheFirstFailure(t *testing.T) {
	work := termTab(t)
	ts := startServer(t, Config{Sessions: running(work)}, quickTerms)
	ts.shows(t, "work", "w1:t1")
	code, reply := ts.typeInto(t, "w1:p1", map[string]any{"text": "x"}, map[string]any{"keys": []string{"bogus"}}, map[string]any{"text": "y"})
	if code != http.StatusBadGateway || reply != (inputReply{Error: "unsupported key bogus", Applied: 1}) {
		t.Errorf("answer %d %+v, want 502 with herdr's message and 1 applied", code, reply)
	}
	if got := work.typedOps(); !slices.Equal(got, []string{"w1:p1 text x"}) {
		t.Errorf("typed %q", got)
	}
}

func TestInputRejectsStripPane(t *testing.T) {
	work := termTab(t)
	ts := startServer(t, Config{Sessions: running(work)}, quickTerms)
	ts.shows(t, "work", "w1:t1")
	refused := func(when string) {
		t.Helper()
		// The strip, a pane in a tool tab, and a pane that isn't there.
		for _, pane := range []string{"w1:p3", "w1:p4", "w1:p9"} {
			if code, reply := ts.typeInto(t, pane, map[string]any{"text": "x"}); code != http.StatusForbidden || reply.Error == "" {
				t.Errorf("%s: typing into %q: %d %+v, want 403", when, pane, code, reply)
			}
		}
	}

	// With no page showing the tab, the board asks herdr where the pane is.
	refused("unwatched")
	// With one, it knows.
	events, _ := ts.tabStream(t, "work", "w1:t1")
	opening(t, events)
	refused("watched")
	if got := work.typedOps(); len(got) != 0 {
		t.Errorf("typed %q", got)
	}
	if code, reply := ts.typeInto(t, "w1:p2", map[string]any{"text": "ls"}, map[string]any{"keys": []string{"enter"}}); code != http.StatusNoContent {
		t.Errorf("typing into the shell: %d %+v", code, reply)
	}
	if got := work.typedOps(); !slices.Equal(got, []string{"w1:p2 text ls", "w1:p2 keys enter"}) {
		t.Errorf("typed %q", got)
	}
}

func TestInputChecksTheRequest(t *testing.T) {
	work := termTab(t)
	ts := startServer(t, Config{Sessions: running(work)}, quickTerms)
	ts.shows(t, "work", "w1:t1")
	text := map[string]any{"text": "x"}
	for _, c := range []struct {
		what string
		body any
		want int
	}{
		{"no ops", map[string]any{"session": "work", "pane": "w1:p1", "ops": []any{}}, 400},
		{"65 ops", map[string]any{"session": "work", "pane": "w1:p1", "ops": slices.Repeat([]any{text}, 65)}, 400},
		{"an empty op", map[string]any{"session": "work", "pane": "w1:p1", "ops": []any{map[string]any{}}}, 400},
		{"an op of two kinds", map[string]any{"session": "work", "pane": "w1:p1", "ops": []any{map[string]any{"text": "x", "keys": []string{"enter"}}}}, 400},
		{"empty text", map[string]any{"session": "work", "pane": "w1:p1", "ops": []any{map[string]any{"text": ""}}}, 400},
		{"no keys", map[string]any{"session": "work", "pane": "w1:p1", "ops": []any{map[string]any{"keys": []string{}}}}, 400},
		{"no pane", map[string]any{"session": "work", "ops": []any{text}}, 400},
		{"no session", map[string]any{"pane": "w1:p1", "ops": []any{text}}, 400},
		{"not JSON", "type this", 400},
		{"too much", map[string]any{"session": "work", "pane": "w1:p1", "ops": []any{map[string]any{"paste": strings.Repeat("x", 64<<10+1)}}}, 413},
		{"another session", map[string]any{"session": "home", "pane": "w1:p1", "ops": []any{text}}, 404},
	} {
		code, reply, err := ts.input(c.body)
		if err != nil {
			t.Fatal(err)
		}
		if code != c.want || reply.Error == "" {
			t.Errorf("%s: %d %+v, want %d with an error", c.what, code, reply, c.want)
		}
	}
	if got := work.typedOps(); len(got) != 0 {
		t.Errorf("typed %q", got)
	}
	// 64 KiB is fine.
	if code, reply := ts.typeInto(t, "w1:p1", map[string]any{"paste": strings.Repeat("x", 64<<10)}); code != http.StatusNoContent {
		t.Errorf("a 64 KiB paste: %d %+v", code, reply)
	}
}

func TestPollerClosesAfterTheLastViewerLeaves(t *testing.T) {
	work := termTab(t)
	ts := startServer(t, Config{Sessions: running(work)}, quickTerms)
	ts.shows(t, "work", "w1:t1")
	if code := status(t, ts.port, "/api/tab/stream?session=work&tab=w1:t2", ts.key); code != http.StatusNotFound {
		t.Errorf("a tool tab's stream: %d, want 404", code)
	}
	first, closeFirst := ts.tabStream(t, "work", "w1:t1")
	opening(t, first)
	second, closeSecond := ts.tabStream(t, "work", "w1:t1")
	opening(t, second)

	closeFirst()
	time.Sleep(500 * time.Millisecond) // past linger
	if n := ts.watching(); n != 1 {
		t.Fatalf("%d pollers while a page shows the tab", n)
	}

	// A page that comes back soon finds the poller still there.
	closeSecond()
	time.Sleep(100 * time.Millisecond)
	if n := ts.watching(); n != 1 {
		t.Fatalf("%d pollers just after the last page went, want it kept a while", n)
	}
	third, closeThird := ts.tabStream(t, "work", "w1:t1")
	opening(t, third)
	time.Sleep(500 * time.Millisecond)
	if n := ts.watching(); n != 1 {
		t.Fatalf("%d pollers with a page back", n)
	}

	closeThird()
	for deadline := time.Now().Add(3 * time.Second); ts.watching() != 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the poller is still there 3s after the last page went")
		}
	}
	time.Sleep(50 * time.Millisecond) // for a read under way to finish
	n := work.readsOf("w1:p1")
	time.Sleep(200 * time.Millisecond)
	if got := work.readsOf("w1:p1"); got != n {
		t.Errorf("w1:p1 read %d times after the poller closed", got-n)
	}
}
