package slack

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// fastWaits makes Continue's polling take milliseconds.
func fastWaits(t *testing.T) {
	fastRetries(t)
	p, a, b := pollEvery, termWait, killWait
	pollEvery, termWait, killWait = time.Millisecond, 20*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { pollEvery, termWait, killWait = p, a, b })
}

type signal struct {
	PID int
	Sig syscall.Signal
}

// killer records signals. A process ends on a signal in endsOn; a nil
// endsOn means any signal, and an empty one means none.
type killer struct {
	mu     sync.Mutex
	f      *fakeHerdr
	sent   []signal
	endsOn []syscall.Signal
}

func (k *killer) kill(pid int, sig syscall.Signal) error {
	k.mu.Lock()
	k.sent = append(k.sent, signal{pid, sig})
	k.mu.Unlock()
	if k.endsOn != nil && !slices.Contains(k.endsOn, sig) {
		return nil
	}
	k.f.mu.Lock()
	defer k.f.mu.Unlock()
	for pane, ps := range k.f.procs {
		k.f.procs[pane] = slices.DeleteFunc(ps, func(p herdr.Process) bool { return p.PID == pid })
	}
	return nil
}

func TestContinueEndsTheLockedSessionAndResumes(t *testing.T) {
	fastWaits(t)
	f, st, dir := slackDesk(t)
	d := deliver(t, f, st, dir, danaAsk(), true)
	id := loadTask(t, st, d.Tab).Slack.SessionID
	pane := f.created[0].PaneID
	pid := f.procs[pane][0].PID
	k := &killer{f: f}
	if err := Continue(f, st, slackOpt, d.Tab, k.kill, deliverNow); err != nil {
		t.Fatal(err)
	}
	if want := []signal{{pid, syscall.SIGTERM}}; !slices.Equal(k.sent, want) {
		t.Errorf("signals = %v, want %v", k.sent, want)
	}
	s := f.starts[len(f.starts)-1]
	if s.Name != "slack-"+id[:8]+"-go" || s.Kind != "claude" || s.Pane != pane || s.Timeout != 30*time.Second ||
		!slices.Equal(s.Args, []string{"--resume", id}) {
		t.Errorf("resume start = %+v", s)
	}
	tk := loadTask(t, st, d.Tab)
	last := tk.Log[len(tk.Log)-1]
	if tk.Slack.Research != task.ResearchOpen || last.Who != "you" || last.Text != "Continued the research with your tools" {
		t.Errorf("research %q, last log %+v", tk.Slack.Research, last)
	}
}

func TestContinueKillsOnlyItsOwnSession(t *testing.T) {
	fastWaits(t)
	f, st, dir := slackDesk(t)
	d := deliver(t, f, st, dir, danaAsk(), true)
	id := loadTask(t, st, d.Tab).Slack.SessionID
	own := f.procs[f.created[0].PaneID][0].PID
	// A second pane holds other sessions and a vim.
	f.mu.Lock()
	other := f.newID("w1:p")
	f.panes = append(f.panes, herdr.Pane{ID: other, TabID: d.Tab, WorkspaceID: "w1"})
	f.layouts[d.Tab] = &herdr.Node{Type: "split", Direction: "right", First: f.layouts[d.Tab], Second: leaf(other)}
	f.procs[other] = []herdr.Process{
		{PID: 7001, Name: "claude", Argv: []string{"claude", "--session-id", "00000000-0000-4000-8000-000000000000"}},
		{PID: 7002, Name: "claude", Argv: []string{"claude", "--session-id=" + id + "-other"}},
		{PID: 7003, Name: "vim", Argv: []string{"vim", id}},
	}
	f.mu.Unlock()
	k := &killer{f: f}
	if err := Continue(f, st, slackOpt, d.Tab, k.kill, deliverNow); err != nil {
		t.Fatal(err)
	}
	if len(k.sent) == 0 {
		t.Error("the session wasn't signalled")
	}
	for _, s := range k.sent {
		if s.PID != own {
			t.Errorf("signalled %d, which isn't this session", s.PID)
		}
	}
}

func TestOwnProcessesAcceptsBothForms(t *testing.T) {
	ps := []herdr.Process{
		{PID: 1, Argv: []string{"claude", "--session-id=abc"}},
		{PID: 2, Argv: []string{"claude", "--session-id", "abd"}},
		{PID: 3, Argv: []string{"claude", "--session-id", "abc"}},
		{PID: 4, Argv: []string{"claude", "abc", "--session-id"}},
	}
	got := ownProcesses(ps, "abc")
	if len(got) != 2 || got[0].PID != 1 || got[1].PID != 3 {
		t.Errorf("ownProcesses = %+v", got)
	}
}

func TestContinueAfterYouQuitClaude(t *testing.T) {
	fastWaits(t)
	f, st, dir := slackDesk(t)
	d := deliver(t, f, st, dir, danaAsk(), true)
	id := loadTask(t, st, d.Tab).Slack.SessionID
	pane := f.created[0].PaneID
	f.procs[pane] = nil
	k := &killer{f: f}
	if err := Continue(f, st, slackOpt, d.Tab, k.kill, deliverNow); err != nil {
		t.Fatal(err)
	}
	if len(k.sent) != 0 {
		t.Errorf("signals = %v, want none", k.sent)
	}
	s := f.starts[len(f.starts)-1]
	if s.Pane != pane || !slices.Equal(s.Args, []string{"--resume", id}) {
		t.Errorf("resume start = %+v", s)
	}
	if loadTask(t, st, d.Tab).Slack.Research != task.ResearchOpen {
		t.Error("research isn't open")
	}
}

func TestContinueEscalatesToKill(t *testing.T) {
	fastWaits(t)
	f, st, dir := slackDesk(t)
	d := deliver(t, f, st, dir, danaAsk(), true)
	pid := f.procs[f.created[0].PaneID][0].PID
	k := &killer{f: f, endsOn: []syscall.Signal{syscall.SIGKILL}}
	if err := Continue(f, st, slackOpt, d.Tab, k.kill, deliverNow); err != nil {
		t.Fatal(err)
	}
	if want := []signal{{pid, syscall.SIGTERM}, {pid, syscall.SIGKILL}}; !slices.Equal(k.sent, want) {
		t.Errorf("signals = %v, want %v", k.sent, want)
	}
}

func TestContinueSaysWhenClaudeWontStop(t *testing.T) {
	fastWaits(t)
	f, st, dir := slackDesk(t)
	d := deliver(t, f, st, dir, danaAsk(), true)
	k := &killer{f: f, endsOn: []syscall.Signal{}}
	err := Continue(f, st, slackOpt, d.Tab, k.kill, deliverNow)
	if err == nil || err.Error() != "Claude didn't stop" {
		t.Errorf("err = %v", err)
	}
	if loadTask(t, st, d.Tab).Slack.Research != task.ResearchLocked {
		t.Error("research changed")
	}
}

func TestContinueRefuses(t *testing.T) {
	fastWaits(t)
	for research, want := range map[task.Research]string{
		task.ResearchHeld: "This tab has no locked research session",
		task.ResearchOpen: "You already continued this research session",
	} {
		f, st, dir := slackDesk(t)
		d := deliver(t, f, st, dir, danaAsk(), true)
		if err := st.Update(func(s *store.State) error {
			s.Tasks[d.Tab].Slack.Research = research
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		k := &killer{f: f}
		n := len(f.starts)
		if err := Continue(f, st, slackOpt, d.Tab, k.kill, deliverNow); err == nil || err.Error() != want {
			t.Errorf("research %q: err = %v, want %q", research, err, want)
		}
		if len(k.sent) != 0 || len(f.starts) != n {
			t.Errorf("research %q: signalled or started", research)
		}
	}

	// A busy pane: StartAgent keeps failing.
	f, st, dir := slackDesk(t)
	d := deliver(t, f, st, dir, danaAsk(), true)
	f.procs[f.created[0].PaneID] = nil
	f.startErrs = slices.Repeat([]error{errors.New("pane is busy")}, startTries)
	err := Continue(f, st, slackOpt, d.Tab, (&killer{f: f}).kill, deliverNow)
	if err == nil || !strings.HasPrefix(err.Error(), "The pane isn't at a shell prompt; leave what's running there and try again: ") ||
		!strings.Contains(err.Error(), "pane is busy") || !errors.Is(err, errNotAtPrompt) {
		t.Errorf("err = %v", err)
	}
	if loadTask(t, st, d.Tab).Slack.Research != task.ResearchLocked {
		t.Error("research changed")
	}
}

func TestContinueLaunchedButNotReady(t *testing.T) {
	fastWaits(t)
	f, st, dir := slackDesk(t)
	d := deliver(t, f, st, dir, danaAsk(), true)
	f.procs[f.created[0].PaneID] = nil
	f.startErrs = []error{&herdr.Error{Code: "agent_not_ready", Message: "waiting on a dialog"}}
	n := len(f.starts)
	err := Continue(f, st, slackOpt, d.Tab, (&killer{f: f}).kill, deliverNow)
	var he *herdr.Error
	if !errors.As(err, &he) || he.Code != "agent_not_ready" {
		t.Errorf("err = %v", err)
	}
	if len(f.starts) != n+1 {
		t.Errorf("starts = %d, want one try", len(f.starts)-n)
	}
	if loadTask(t, st, d.Tab).Slack.Research != task.ResearchOpen {
		t.Error("research isn't open")
	}
}

// TestContinueLeavesAnotherSessionAlone: Continue records the research as
// open only on the task whose session it resumed.
func TestContinueLeavesAnotherSessionAlone(t *testing.T) {
	fastWaits(t)
	f, st, dir := slackDesk(t)
	d := deliver(t, f, st, dir, danaAsk(), true)
	const other = "0b5e1c2a-7d3f-4e8a-9c61-2f4d8a1b3c5e"
	f.onStart = func(agentStart) {
		f.mu.Lock()
		f.onStart = nil
		f.mu.Unlock()
		changeTask(t, st, d.Tab, func(tk *task.Task) { tk.Slack.SessionID = other })
	}
	if err := Continue(f, st, slackOpt, d.Tab, (&killer{f: f}).kill, deliverNow); err != nil {
		t.Fatal(err)
	}
	tk := loadTask(t, st, d.Tab)
	if tk.Slack.SessionID != other || tk.Slack.Research != task.ResearchLocked || lastLog(tk).Who == "you" {
		t.Errorf("session %q, research %q, log %+v; want the other session left alone", tk.Slack.SessionID, tk.Slack.Research, lastLog(tk))
	}
}
