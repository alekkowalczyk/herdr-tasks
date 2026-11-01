package board

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/strip"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// Sessions finds the herdr sessions to show.
type Sessions interface {
	// List is the sessions running now, counting those too busy to answer.
	// The board asks every tick, so it should be quick.
	List() []SessionConn
}

// SessionConn is one running herdr session and its tasks.
type SessionConn interface {
	Name() string
	// Ended reports whether the session surely ended, as its socket is gone.
	// A session List leaves out stays on the board, marked stale, for a few
	// ticks, unless it has.
	Ended() bool
	Live() (*herdr.Live, error)
	Panes() ([]herdr.Pane, error)
	Store() *store.Store
	// Change runs fn on a tab's task and syncs herdr, as the CLI does. A tab
	// without a task is reconcile.ErrNoTab.
	Change(tabID string, fn func(*task.Task) error) error
	// Focus shows the tab in herdr.
	Focus(tabID string) error
	// Subscribe calls fn for herdr events that can change the board, until
	// ctx ends or herdr closes the connection.
	Subscribe(ctx context.Context, fn func(herdr.Event)) error

	// ReadScreen is what a pane shows, as ANSI text.
	ReadScreen(paneID string) (string, error)
	// TabLayout is the layout of the tab a pane is in.
	TabLayout(paneID string) (*herdr.TabLayout, error)
	// TypeText, SendKeys and Paste type into a pane, as the herdr client's
	// methods of the same names do.
	TypeText(paneID, text string) error
	SendKeys(paneID string, keys ...string) error
	Paste(paneID, text string) error
}

// HerdrSessions is every herdr session of this user whose server is
// listening, busy or not.
func HerdrSessions() Sessions { return herdrSessions{} }

type herdrSessions struct{}

// probeTimeout is how long List waits to connect to a session.
const probeTimeout = 500 * time.Millisecond

// List connects to each session's socket, all at once, and asks nothing: a
// session that's busy is listed, and holds up none of the others. Its worker
// finds out whether it answers.
func (herdrSessions) List() []SessionConn {
	paths := herdr.SessionSockets()
	up := make([]bool, len(paths))
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			up[i] = herdr.Listening(p, probeTimeout)
		}()
	}
	wg.Wait()
	var out []SessionConn
	for i, p := range paths {
		if up[i] {
			out = append(out, herdrSession{c: &herdr.Client{SocketPath: p}, st: store.New(herdr.SessionKey(p))})
		}
	}
	return out
}

type herdrSession struct {
	c  *herdr.Client
	st *store.Store
}

// boardEvents are the herdr events that can change what the board shows.
var boardEvents = []herdr.Subscription{
	{Type: "tab.created"}, {Type: "tab.closed"}, {Type: "tab.renamed"}, {Type: "tab.moved"},
	{Type: "workspace.created"}, {Type: "workspace.closed"}, {Type: "workspace.renamed"},
	{Type: "pane.created"}, {Type: "pane.closed"}, {Type: "pane.moved"}, {Type: "pane.updated"},
}

func (s herdrSession) Name() string                 { return s.st.Session }
func (s herdrSession) Live() (*herdr.Live, error)   { return s.c.Live() }
func (s herdrSession) Panes() ([]herdr.Pane, error) { return s.c.Panes() }
func (s herdrSession) Store() *store.Store          { return s.st }
func (s herdrSession) Focus(tabID string) error     { return s.c.FocusTab(tabID) }

func (s herdrSession) ReadScreen(paneID string) (string, error) { return s.c.ReadScreen(paneID) }
func (s herdrSession) TabLayout(paneID string) (*herdr.TabLayout, error) {
	return s.c.TabLayout(paneID)
}
func (s herdrSession) TypeText(paneID, text string) error { return s.c.TypeText(paneID, text) }
func (s herdrSession) SendKeys(paneID string, keys ...string) error {
	return s.c.SendKeys(paneID, keys...)
}
func (s herdrSession) Paste(paneID, text string) error { return s.c.Paste(paneID, text) }

// Ended: herdr may also leave its socket when it ends. That one is left out
// of the list for a few ticks before it goes.
func (s herdrSession) Ended() bool {
	_, err := os.Lstat(s.c.SocketPath)
	return errors.Is(err, fs.ErrNotExist)
}

// Change reads the settings each time, as you may have changed them since
// the server started.
func (s herdrSession) Change(tabID string, fn func(*task.Task) error) error {
	opts, err := reconcile.LoadOptions()
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	_, err = reconcile.ChangeTask(strip.Host{Client: s.c}, s.st, opts, time.Now(), tabID, fn)
	return err
}

func (s herdrSession) Subscribe(ctx context.Context, fn func(herdr.Event)) error {
	return s.c.Subscribe(ctx, boardEvents, fn)
}
