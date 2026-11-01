package slack

import (
	"sync"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/strip"
)

// probeTimeout is how long RunningSessions waits to connect to a session.
const probeTimeout = 500 * time.Millisecond

// Session is a running herdr session: where asks go, and its tasks.
type Session struct {
	Name  string
	Herdr Herdr
	Store *store.Store
}

// RunningSessions is every herdr session of this user whose server is
// listening, busy or not, as the board finds them. It connects to each
// socket, all at once, and asks nothing.
func RunningSessions() []Session {
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
	var out []Session
	for i, p := range paths {
		if up[i] {
			st := store.New(herdr.SessionKey(p))
			out = append(out, Session{Name: st.Session, Herdr: strip.Host{Client: &herdr.Client{SocketPath: p}}, Store: st})
		}
	}
	return out
}
