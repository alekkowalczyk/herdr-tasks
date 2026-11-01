package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/tailnet"
)

// Tailscale is what the board asks of the tailscale command, as tailnet.CLI
// does it.
type Tailscale interface {
	// Self is this machine's name on the tailnet, and its owner.
	Self(ctx context.Context) (tailnet.Self, error)
	// Served maps each HTTPS port tailscale serve serves to its target.
	Served(ctx context.Context) (map[int]string, error)
	// Serve shares target with the tailnet at port, until Off.
	Serve(ctx context.Context, port int, target string) error
	Off(ctx context.Context, port int) error
}

const (
	// recordFile, in the server's folder, is the tailscale serve entries
	// herdr-tasks made.
	recordFile = "tailnet.json"
	// socketsDir, in the server's folder, holds the sockets tailscale serve
	// sends requests to. Only you can reach them.
	socketsDir  = "sockets"
	boardSocket = "board.sock"
	// tailnetEvery is how often the server reads Tailscale again while
	// nothing changes, and how long it waits after a failure.
	tailnetEvery = time.Minute
	// maxSocketPath is what a Unix socket's path has to stay under on macOS.
	// Linux allows a few bytes more.
	maxSocketPath = 104
)

// tailnetRecord is the tailscale serve entries herdr-tasks made. Only these
// are ever changed or taken out, and only while Tailscale still serves them
// as recorded.
type tailnetRecord struct {
	Board  *sharedEntry  `json:"board,omitempty"`
	Shares []sharedEntry `json:"shares,omitempty"`
}

// sharedEntry is one entry herdr-tasks made: the HTTPS port, its target as
// Tailscale reports it, and when it was made.
type sharedEntry struct {
	Port   int       `json:"port"`
	Target string    `json:"target"`
	At     time.Time `json:"at"`
}

// socketPath is the path of the socket called name.
func socketPath(stateDir, name string) string {
	return filepath.Join(serverDir(stateDir), socketsDir, name)
}

// loadRecord reads the record. With none, it's fs.ErrNotExist.
func loadRecord(stateDir string) (tailnetRecord, error) {
	var rec tailnetRecord
	path := serverFile(stateDir, recordFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return tailnetRecord{}, fmt.Errorf("%s: %w", path, err)
	}
	return rec, nil
}

// saveRecord writes the record, or removes it when it holds nothing.
func saveRecord(stateDir string, rec tailnetRecord) error {
	path := serverFile(stateDir, recordFile)
	if rec.Board == nil && len(rec.Shares) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(path, string(b)+"\n", false)
}

// unshare takes e out of tailscale serve if Tailscale still serves it as
// recorded, and says whether it did. Otherwise the port is someone else's
// now, and is left alone.
func unshare(ctx context.Context, ts Tailscale, served map[int]string, e sharedEntry) (bool, error) {
	if got, ok := served[e.Port]; !ok || got != e.Target {
		return false, nil
	}
	return true, ts.Off(ctx, e.Port)
}

// Unshare takes out the tailscale serve entries herdr-tasks recorded, the
// board's and the shared ports', and then the record, as `remove` does once
// the server has stopped. An entry Tailscale no longer serves as recorded is
// left alone, and one that couldn't be taken out stays recorded. With
// nothing recorded, it asks Tailscale nothing.
func Unshare(stateDir string, ts Tailscale) error {
	rec, err := loadRecord(stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx := context.Background()
	var served map[int]string
	if rec.Board != nil || len(rec.Shares) > 0 {
		if served, err = ts.Served(ctx); err != nil {
			return err
		}
	}
	var kept tailnetRecord
	var errs []error
	if rec.Board != nil {
		if _, err := unshare(ctx, ts, served, *rec.Board); err != nil {
			errs = append(errs, err)
			kept.Board = rec.Board
		}
	}
	for _, e := range rec.Shares {
		if _, err := unshare(ctx, ts, served, e); err != nil {
			errs = append(errs, err)
			kept.Shares = append(kept.Shares, e)
		}
	}
	if err := saveRecord(stateDir, kept); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// tailnetState is the board's door on the tailnet, and what the header says
// of it.
type tailnetState struct {
	// mu is held while the server syncs with Tailscale, and to open or close
	// the door.
	mu     sync.Mutex
	on     bool      // the board and board_tailnet, as the last sync found them
	next   time.Time // when to read Tailscale again, while they stay so
	door   *tailnetDoor
	closed bool   // the server is shutting down: the door stays closed
	said   string // the last problem logged

	// view changes as a whole, so a request reads it without waiting for a
	// sync.
	view atomic.Pointer[tailnetView]
}

// tailnetView is what the header shows of the tailnet, and who owns this
// machine.
type tailnetView struct {
	line  *TailnetLine // nil while board_tailnet is off
	owner string
}

// tailnetDoor serves the tailnet door on the board's socket.
type tailnetDoor struct {
	srv    *http.Server
	cancel context.CancelFunc // ends its requests
	done   chan struct{}      // closed once it stops serving
}

// tailnetLine is the board on the tailnet, for the header: nil while
// board_tailnet is off.
func (s *Server) tailnetLine() *TailnetLine {
	if v := s.tailnet.view.Load(); v != nil {
		return v.line
	}
	return nil
}

// tailnetOrigin is the board's address on the tailnet while it's shared,
// and "" otherwise: the Origin the tailnet door takes.
func (s *Server) tailnetOrigin() string {
	if l := s.tailnetLine(); l != nil {
		return l.Address
	}
	return ""
}

// tailnetOwner is the login Tailscale last said owns this machine.
func (s *Server) tailnetOwner() string {
	if v := s.tailnet.view.Load(); v != nil {
		return v.owner
	}
	return ""
}

// showTailnet puts out what the header shows of the tailnet, and has the
// board built again with it.
func (s *Server) showTailnet(line *TailnetLine, owner string) {
	s.tailnet.view.Store(&tailnetView{line: line, owner: owner})
	signal(s.hub.dirty)
}

// syncTailnet shares the board with the tailnet while the board and
// board_tailnet are on, and takes it off when either is turned off. check
// runs it each tick. A change in the settings acts at once; otherwise it
// reads Tailscale once a minute at most, and after a failure, tries again a
// minute later. With board_tailnet off and nothing recorded, it asks
// Tailscale nothing.
func (s *Server) syncTailnet(ctx context.Context, opts reconcile.Options) {
	if s.cfg.Tailscale == nil {
		return
	}
	t := &s.tailnet
	t.mu.Lock()
	defer t.mu.Unlock()
	on := opts.Board && opts.BoardTailnet
	if on == t.on && s.now().Before(t.next) {
		return
	}
	t.on, t.next = on, s.now().Add(tailnetEvery)
	if !on {
		s.unshareBoard(ctx)
		return
	}
	line, owner := s.shareBoard(ctx)
	if line.Problem != "" {
		logOnce(&t.said, "tailnet: "+line.Problem)
	} else {
		logOnce(&t.said, "tailnet: the board is at "+line.Address)
	}
	s.showTailnet(line, owner)
}

// shareBoard listens on the board's socket, and has Tailscale serve it at
// the board's port, unless Tailscale serves that port for something else.
// It says where the board is on the tailnet, or why it isn't there, and who
// owns this machine. When Tailscale can't be read, it keeps the address it
// had, with the problem.
func (s *Server) shareBoard(ctx context.Context) (*TailnetLine, string) {
	ts, port := s.cfg.Tailscale, s.cfg.Port
	sock := socketPath(s.cfg.StateDir, boardSocket)
	var was tailnetView
	if v := s.tailnet.view.Load(); v != nil {
		was = *v
	}
	failed := func(err error) *TailnetLine {
		return &TailnetLine{Problem: "Couldn't share the board with your tailnet: " + err.Error()}
	}
	if err := s.openDoor(sock); err != nil {
		return failed(err), was.owner
	}
	self, err := ts.Self(ctx)
	var served map[int]string
	if err == nil {
		served, err = ts.Served(ctx)
	}
	if err != nil {
		line := failed(err)
		if was.line != nil {
			line.Address = was.line.Address
		}
		return line, was.owner
	}
	if self.DNSName == "" {
		return failed(errors.New("Tailscale gives this machine no name")), self.Owner
	}
	address := tailnetAddress(self.DNSName, port)
	rec := s.tailnetRecord()
	if b := rec.Board; b != nil && b.Port != port {
		// The board moved from that port.
		if _, err := unshare(ctx, ts, served, *b); err != nil {
			return failed(fmt.Errorf("couldn't stop sharing port %d: %w", b.Port, err)), self.Owner
		}
		rec.Board = nil
		if err := saveRecord(s.cfg.StateDir, rec); err != nil {
			return failed(err), self.Owner
		}
		log.Printf("tailnet: the board left port %d", b.Port)
	}
	target := "unix:" + sock
	got, listed := served[port]
	switch {
	case rec.Board != nil && listed && got == rec.Board.Target:
		return &TailnetLine{Address: address}, self.Owner
	case listed && got == target:
		// The board's own entry, though the record of it was lost.
		rec.Board = &sharedEntry{Port: port, Target: got, At: s.now()}
	case listed:
		return &TailnetLine{Problem: fmt.Sprintf("Port %d is already shared with your tailnet for something else.", port)}, self.Owner
	default:
		if err := ts.Serve(ctx, port, target); err != nil {
			return failed(err), self.Owner
		}
		// Recorded as Tailscale reports it, which may not be how it was
		// given, so it's still the board's own on the next look.
		if after, err := ts.Served(ctx); err == nil && after[port] != "" {
			target = after[port]
		}
		rec.Board = &sharedEntry{Port: port, Target: target, At: s.now()}
	}
	if err := saveRecord(s.cfg.StateDir, rec); err != nil {
		return &TailnetLine{Address: address, Problem: "Couldn't record the board's tailscale serve entry: " + err.Error()}, self.Owner
	}
	return &TailnetLine{Address: address}, self.Owner
}

// unshareBoard stops listening on the board's socket, and takes the board's
// entry out of tailscale serve, if Tailscale still serves it as recorded.
// An entry that isn't the board's any more is only forgotten. When that
// fails, it's kept, to try again.
func (s *Server) unshareBoard(ctx context.Context) {
	t := &s.tailnet
	t.closeDoor()
	s.showTailnet(nil, "")
	rec := s.tailnetRecord()
	b := rec.Board
	if b == nil {
		return
	}
	served, err := s.cfg.Tailscale.Served(ctx)
	off := false
	if err == nil {
		off, err = unshare(ctx, s.cfg.Tailscale, served, *b)
	}
	if err == nil {
		rec.Board = nil
		err = saveRecord(s.cfg.StateDir, rec)
	}
	if err != nil {
		logOnce(&t.said, "tailnet: couldn't take the board off the tailnet: "+err.Error())
		return
	}
	t.said = ""
	if off {
		log.Printf("tailnet: stopped sharing the board at port %d", b.Port)
	} else {
		log.Printf("tailnet: port %d serves something else now; left it alone", b.Port)
	}
}

// tailnetRecord is the record, or an empty one when there's none or it
// can't be read. An empty record makes the board take back only an entry
// for its own socket, never anyone else's.
func (s *Server) tailnetRecord() tailnetRecord {
	rec, err := loadRecord(s.cfg.StateDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		logOnce(&s.tailnet.said, "tailnet: "+err.Error())
	}
	return rec
}

// openDoor listens on the board's socket and serves the tailnet door there,
// unless it does already.
func (s *Server) openDoor(sock string) error {
	t := &s.tailnet
	if d := t.door; d != nil {
		select {
		case <-d.done: // it stopped on its own, so it opens again
			t.closeDoor()
		default:
			return nil
		}
	}
	if t.closed || s.ctx.Err() != nil {
		return errors.New("the board is shutting down")
	}
	ln, err := listenSocket(sock)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(s.ctx)
	d := &tailnetDoor{
		srv: &http.Server{
			Handler:           s.TailnetHandler(s.tailnetOrigin),
			ReadHeaderTimeout: 10 * time.Second,
			// No WriteTimeout: streams stay open, until the door closes or
			// the server shuts down.
			BaseContext: func(net.Listener) context.Context { return ctx },
		},
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go func() {
		defer close(d.done)
		if err := d.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			log.Printf("tailnet: %v", err)
		}
	}()
	t.door = d
	return nil
}

// closeDoor stops listening on the socket, which goes with it. Its streams
// end at once, and other requests get 2 s to finish.
func (t *tailnetState) closeDoor() {
	d := t.door
	if d == nil {
		return
	}
	t.door = nil
	d.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if d.srv.Shutdown(ctx) != nil {
		d.srv.Close()
	}
	<-d.done
}

// closeTailnet closes the tailnet door for good, as the server shuts down.
// Tailscale keeps serving the board's entry, so the next server is on the
// tailnet again at once.
func (s *Server) closeTailnet() {
	t := &s.tailnet
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	t.closeDoor()
}

// listenSocket listens on the Unix socket at path, which only you can
// reach: the socket is 0600, in a folder that's 0700. What a server that was
// killed left there, a socket or a plain file, is removed first; anything
// else is left alone.
func listenSocket(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s isn't a folder", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket && !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("something that isn't its socket is in the way at %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if errors.Is(err, syscall.EINVAL) && len(path) >= maxSocketPath {
		return nil, fmt.Errorf("the path of its socket is too long for this system (%d bytes): %s", len(path), path)
	}
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// tailnetAddress is the board's address on the tailnet, at the machine's
// name, with the port unless it's 443.
func tailnetAddress(name string, port int) string {
	if port == 443 {
		return "https://" + name
	}
	return "https://" + net.JoinHostPort(name, strconv.Itoa(port))
}
