package ui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// unlinkCase is the race the overview's unlink tests play out: you see the links
// [plan.html, issue #2, issue #3] and pick issue #2, while the Claude hook publishes
// plan.html as an artifact, which takes the file's place in the list.
type unlinkCase struct {
	st        *store.Store
	m         *model
	b, c, art task.Link
	file      task.Link
}

func newUnlinkCase(t *testing.T) unlinkCase {
	t.Helper()
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	const src = "/tmp/acme/plan.html"
	u := unlinkCase{
		st:   st,
		file: task.Link{URL: task.FileURL(src), By: "claude"},
		b:    task.Link{URL: "https://github.com/acme/x/issues/2", By: "claude"},
		c:    task.Link{URL: "https://github.com/acme/x/issues/3", By: "claude"},
		art:  task.Link{URL: "https://claude.ai/code/artifact/00000000-0000-4000-8000-000000000001", Source: src, By: "claude"},
	}
	setLinks(t, st, u.file, u.b, u.c)
	m, err := newModel(Env{Client: fakeHerdr(t), Store: st, CurrentTab: "w1:t1"})
	if err != nil {
		t.Fatal(err)
	}
	m.width, m.height = 140, 30
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	u.m = m
	return u
}

// publish is the hook replacing the file with its artifact.
func (u unlinkCase) publish(t *testing.T) { setLinks(t, u.st, u.b, u.c, u.art) }

// check fails the test unless issue #2 alone was removed and dismissed.
func (u unlinkCase) check(t *testing.T) {
	t.Helper()
	tk := func() *task.Task {
		s, err := u.st.Load()
		if err != nil {
			t.Fatal(err)
		}
		return s.Tasks["w1:t1"]
	}()
	var got []string
	for _, l := range tk.Links {
		got = append(got, l.URL)
	}
	if want := []string{u.c.URL, u.art.URL}; !slices.Equal(got, want) {
		t.Errorf("links = %q, want %q", got, want)
	}
	if !tk.Dismissed(u.b.URL, "") || tk.Dismissed(u.c.URL, "") {
		t.Errorf("dismissed = %q, want issue #2 only", tk.DismissedLinks)
	}
	if u.m.msg != "Removed issue #2" {
		t.Errorf("message = %q, want Removed issue #2", u.m.msg)
	}
}

func TestOverviewUnlinkRemovesTheLinkYouSaw(t *testing.T) {
	u := newUnlinkCase(t)
	u.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("L")})
	u.publish(t)
	u.m.Update(refreshMsg{})
	if u.m.mode != modeUnlink {
		t.Fatal("the refresh ended unlinking")
	}
	// The numbers you're choosing from don't move.
	if view := u.m.View(); !slices.ContainsFunc(strings.Split(view, "\n"), func(l string) bool { return strings.Contains(plain(l), "2 issue #2") }) {
		t.Errorf("view doesn't list issue #2 as 2:\n%s", plain(view))
	}
	u.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	u.check(t)
}

func TestOverviewUnlinkClickRemovesTheLinkYouSaw(t *testing.T) {
	u := newUnlinkCase(t)
	u.m.View()
	y := -1
	for at, l := range u.m.linkAt {
		if l.URL == u.b.URL {
			y = at
		}
	}
	if y < 0 {
		t.Fatalf("issue #2 has no ×: %+v", u.m.linkAt)
	}
	// The hook writes after the overview drew the list, before you click.
	u.publish(t)
	u.m.Update(tea.MouseMsg{X: u.m.width - 1, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	u.check(t)
}

func TestOverviewUnlinkOfALinkGoneMeanwhileSaysSo(t *testing.T) {
	u := newUnlinkCase(t)
	u.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("L")})
	// The board removes issue #2 before you press 2.
	setLinks(t, u.st, u.file, u.c)
	u.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if l := linksOfTab(t, u.st); len(l) != 2 || l[1].URL != u.c.URL {
		t.Errorf("links = %+v, want plan.html and issue #3", l)
	}
	if u.m.msg != "issue #2 was removed already" {
		t.Errorf("message = %q, want issue #2 was removed already", u.m.msg)
	}
	u.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("L")})
	u.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("7")})
	if u.m.msg != "No link 7" {
		t.Errorf("message = %q, want No link 7", u.m.msg)
	}
}

func TestOverviewSaysWhenALinkIsntAdded(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	setLinks(t, st)
	m, err := newModel(Env{Client: fakeHerdr(t), Store: st, CurrentTab: "w1:t1"})
	if err != nil {
		t.Fatal(err)
	}
	add := func(url string) {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
		m.input.SetValue(url)
		m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	}
	for _, bad := range []string{"https://example.com/100%done", "https://example.com/%0A"} {
		add(bad)
		if l := linksOfTab(t, st); len(l) != 0 {
			t.Errorf("%s: links = %+v, want none", bad, l)
		}
		if m.msg != "Not a valid link" {
			t.Errorf("%s: message = %q, want Not a valid link", bad, m.msg)
		}
	}
	add("https://github.com/acme/x/pull/2")
	if m.msg != "Added PR #2 to Acme" {
		t.Errorf("message = %q, want Added PR #2 to Acme", m.msg)
	}
	add("https://github.com/acme/x/pull/2")
	if l := linksOfTab(t, st); len(l) != 1 {
		t.Errorf("links = %+v, want PR #2 once", l)
	}
	if m.msg != "Already linked to Acme" {
		t.Errorf("message = %q, want Already linked to Acme", m.msg)
	}
}

func TestOverviewMoveAndMute(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	setTask(t, st, &task.Task{Stage: task.Active, Slack: dana(task.ResearchLocked)})
	m, err := newModel(Env{Client: fakeHerdr(t, others...), Store: st, CurrentTab: "w1:t1"})
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	var moves, mutes [][2]string
	m.act.move = func(tab, ws string) (string, error) {
		moves = append(moves, [2]string{tab, ws})
		return "w4:t9", nil
	}
	m.act.mute = func(channel, place string) (bool, error) {
		mutes = append(mutes, [2]string{channel, place})
		return len(mutes) == 1, nil
	}
	press := func(key string) tea.Cmd {
		_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		return cmd
	}

	// m mutes the selected task's channel.
	press("m")
	if want := "Muted #billing. Undo: herdr-tasks slack unmute C0123ABCD"; m.msg != want {
		t.Errorf("message = %q, want %q", m.msg, want)
	}
	press("m")
	if m.msg != "#billing is muted already" {
		t.Errorf("message = %q, want #billing is muted already", m.msg)
	}
	if want := [][2]string{{"C0123ABCD", "#billing"}, {"C0123ABCD", "#billing"}}; !slices.Equal(mutes, want) {
		t.Errorf("mutes = %v, want %v", mutes, want)
	}

	// w asks for a workspace number, as L asks for a link's; another key cancels.
	press("w")
	if want := "Move to workspace: 1 billing  2 infra  3 Slack"; m.msg != want || m.mode != modeMove {
		t.Fatalf("mode %v message %q, want the picker and %q", m.mode, m.msg, want)
	}
	press("x")
	if m.mode != modeList || len(moves) != 0 {
		t.Errorf("x: mode %v moves %v, want it cancelled", m.mode, moves)
	}
	press("w")
	cmd := press("3")
	if m.msg != "Moving Acme to Slack…" || cmd == nil {
		t.Fatalf("message = %q, want Moving Acme to Slack… and a command", m.msg)
	}
	m.Update(cmd())
	if want := [][2]string{{"w1:t1", "w4"}}; !slices.Equal(moves, want) {
		t.Errorf("moves = %v, want %v", moves, want)
	}
	if m.msg != "Moved Acme to Slack" {
		t.Errorf("message = %q, want Moved Acme to Slack", m.msg)
	}
	press("w")
	press("7")
	if m.msg != "No workspace 7" || len(moves) != 1 {
		t.Errorf("7: message %q moves %v", m.msg, moves)
	}

	// A plain task's channel can't be muted.
	setTask(t, st, &task.Task{Stage: task.Active})
	m.Update(refreshMsg{})
	press("m")
	if m.msg != "Only a Slack task's channel can be muted." || len(mutes) != 2 {
		t.Errorf("plain task: message %q mutes %v", m.msg, mutes)
	}

	// The help lists both keys.
	press("?")
	if view := plain(m.View()); !strings.Contains(view, "w move to workspace") || !strings.Contains(view, "m mute channel") {
		t.Errorf("help doesn't list w and m:\n%s", view)
	}
}

// b opens the selected task on the board, in the browser, and the overview
// goes. When the board has something to say, as when no browser opened, the
// overview stays and says it.
func TestOverviewOpensTheBoard(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	setTask(t, st, &task.Task{Stage: task.Active})
	var opened []string
	say := ""
	m, err := newModel(Env{Client: fakeHerdr(t, others...), Store: st, CurrentTab: "w1:t1", OpenBoard: func(tab string) string {
		opened = append(opened, tab)
		return say
	}})
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	press := func(key string) tea.Cmd {
		_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		return cmd
	}
	quits := func(cmd tea.Cmd) bool {
		if cmd == nil {
			return false
		}
		_, ok := cmd().(tea.QuitMsg)
		return ok
	}

	open := press("b")
	if m.msg != "Opening the board…" || open == nil {
		t.Fatalf("message = %q, want Opening the board… and a command", m.msg)
	}
	if len(opened) != 0 {
		t.Errorf("the board opened before the command ran: %v", opened)
	}
	_, next := m.Update(open())
	if !quits(next) {
		t.Error("the overview stayed after the board opened")
	}
	if !slices.Equal(opened, []string{"w1:t1"}) {
		t.Errorf("opened %v, want the selected task's tab", opened)
	}

	say = "Open this in your browser: http://127.0.0.1:7391/test/?code=abc#tab=w1%3At1 (the code works for a minute)"
	open = press("b")
	if _, next := m.Update(open()); quits(next) {
		t.Error("the overview went with something to say")
	}
	if m.msg != say {
		t.Errorf("message = %q, want %q", m.msg, say)
	}

	press("?")
	if view := plain(m.View()); !strings.Contains(view, "b open on the board") {
		t.Errorf("help doesn't list b:\n%s", view)
	}
}

// A asks the assistant about the selected task, from the tab the overview
// is in, and goes once the assistant shows; it stays to say what went wrong.
func TestOverviewAsksTheAssistant(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	setTask(t, st, &task.Task{Stage: task.Active})
	m, err := newModel(Env{Client: fakeHerdr(t), Store: st, CurrentTab: "w1:t1"})
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	var asked [][2]string
	var askErr error
	m.act.ask = func(tab, from string) error {
		asked = append(asked, [2]string{tab, from})
		return askErr
	}
	press := func(key string) tea.Cmd {
		_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		return cmd
	}
	quits := func(cmd tea.Cmd) bool {
		if cmd == nil {
			return false
		}
		_, ok := cmd().(tea.QuitMsg)
		return ok
	}

	ask := press("A")
	if m.msg != "Opening the assistant…" || ask == nil || len(asked) != 0 {
		t.Fatalf("message = %q, asked %v; want Opening the assistant… and a command", m.msg, asked)
	}
	// Starting Claude takes a moment: A again meanwhile asks nothing more.
	if again := press("A"); again != nil {
		t.Error("A asked again while the question was on its way")
	}
	if _, next := m.Update(ask()); !quits(next) {
		t.Error("the overview stayed after the assistant showed")
	}
	if want := [][2]string{{"w1:t1", "w1:t1"}}; !slices.Equal(asked, want) {
		t.Errorf("asked %v, want %v", asked, want)
	}

	askErr = errors.New("The assistant is waiting for your answer. Answer it first")
	if _, next := m.Update(press("A")()); quits(next) || m.msg != askErr.Error() {
		t.Errorf("message = %q, quit %v; want the error, and to stay", m.msg, quits(next))
	}

	press("?")
	if view := plain(m.View()); !strings.Contains(view, "A assistant") {
		t.Errorf("help doesn't list A:\n%s", view)
	}
}

// Quitting while a move the overview started runs would cut the move
// short: the overview quits once the move is done, or at a second ctrl+c.
func TestOverviewQuitsOnceItsMoveIsDone(t *testing.T) {
	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	setTask(t, st, &task.Task{Stage: task.Active})
	overview := func() *model {
		m, err := newModel(Env{Client: fakeHerdr(t, others...), Store: st, CurrentTab: "w1:t1"})
		if err != nil {
			t.Fatal(err)
		}
		m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
		m.act.move = func(string, string) (string, error) { return "w1:t1", nil }
		return m
	}
	runes := func(k string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)} }
	ctrlC := tea.KeyMsg{Type: tea.KeyCtrlC}
	quits := func(cmd tea.Cmd) bool {
		if cmd == nil {
			return false
		}
		_, ok := cmd().(tea.QuitMsg)
		return ok
	}
	send := func(m *model, msg tea.Msg) tea.Cmd {
		_, cmd := m.Update(msg)
		return cmd
	}

	m := overview()
	if !quits(send(m, runes("q"))) {
		t.Fatal("q didn't quit with no move running")
	}
	moving := func() (*model, tea.Cmd) {
		m := overview()
		send(m, runes("w"))
		move := send(m, runes("3"))
		if move == nil {
			t.Fatalf("no move: %q", m.msg)
		}
		return m, move
	}

	// A move with no quit asked for doesn't quit.
	m, move := moving()
	if quits(send(m, move())) {
		t.Error("the overview quit after a move nobody asked it to quit after")
	}

	// Each way to quit waits for the move.
	for _, k := range []tea.KeyMsg{runes("q"), {Type: tea.KeyEsc}, runes("t"), ctrlC, {Type: tea.KeyEnter}} {
		m, move := moving()
		if cmd := send(m, k); quits(cmd) || m.msg != "Moving… quit when it's done" {
			t.Errorf("%s while moving: quit %v, message %q; want Moving… quit when it's done", k, quits(cmd), m.msg)
		}
		if !quits(send(m, move())) {
			t.Errorf("%s: the overview didn't quit once the move was done", k)
		}
	}

	// A second ctrl+c quits at once.
	m, _ = moving()
	send(m, ctrlC)
	if !quits(send(m, ctrlC)) {
		t.Error("a second ctrl+c didn't quit")
	}

	// A move that fails doesn't quit: its error needs to be seen. The next
	// q quits.
	m = overview()
	m.act.move = func(string, string) (string, error) {
		return "w4:t9", errors.New("Moved 1 of 2 panes; the rest are still in the old tab: refused")
	}
	send(m, runes("w"))
	move = send(m, runes("3"))
	send(m, runes("q"))
	if quits(send(m, move())) || m.msg != "Moved 1 of 2 panes; the rest are still in the old tab: refused" {
		t.Errorf("a failed move: message %q; want no quit, and the error", m.msg)
	}
	if !quits(send(m, runes("q"))) {
		t.Error("q didn't quit after the failed move")
	}
}

func TestOverviewGroupsLinks(t *testing.T) {
	local := time.Local
	time.Local = time.FixedZone("UTC+3", 3*60*60)
	t.Cleanup(func() { time.Local = local })

	st := &store.Store{Dir: t.TempDir(), Session: "test"}
	page := task.Link{URL: "http://localhost:4100/demo", Label: "Demo", By: "claude"}
	pr := task.Link{URL: prLink}
	slack := task.Link{URL: slackMsg, Label: "#billing · Dana Reed: hi"}
	issue := task.Link{URL: linearIssue}
	setLinks(t, st, page, pr, slack, issue)
	m, err := newModel(Env{Client: fakeHerdr(t), Store: st, CurrentTab: "w1:t1"})
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})

	// The task's row previews the first of them.
	if view := plain(m.View()); !strings.Contains(view, "↗ ABC-12 +3") {
		t.Errorf("the row doesn't preview ↗ ABC-12 +3:\n%s", view)
	}

	// The LINKS list as it shows on the right.
	m.View()
	lines := m.detailLines(detailWidth-2, m.bodyHeight())
	start := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(plain(l), "LINKS") })
	if start < 0 {
		t.Fatalf("no LINKS:\n%s", plain(strings.Join(lines, "\n")))
	}
	var got []string
	for _, l := range lines[start+1:] {
		got = append(got, plain(l))
	}
	want := []string{
		"Linear",
		"1 ABC-12 linear.app/acme/issue/ABC-12 ×",
		"GitHub",
		"2 PR #1 github.com/acme/x/pull/1 ×",
		"Slack",
		"3 Nov 15 01:13 #billing · Dana Reed: hi ×",
		"Pages",
		"4 Demo localhost:4100/demo via claude ×",
	}
	if !slices.Equal(got, want) {
		t.Errorf("LINKS:\n%q\nwant\n%q", got, want)
	}

	// Each × removes the link on its line.
	var ys []int
	for y := range m.linkAt {
		ys = append(ys, y)
	}
	slices.Sort(ys)
	var order []string
	for _, y := range ys {
		order = append(order, m.linkAt[y].URL)
	}
	if want := []string{issue.URL, pr.URL, slack.URL, page.URL}; !slices.Equal(order, want) {
		t.Errorf("× lines remove %q, want %q", order, want)
	}

	// Unlink numbers count the same way: 1 is the first link shown.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("L")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	var left []string
	for _, l := range linksOfTab(t, st) {
		left = append(left, l.URL)
	}
	if want := []string{page.URL, pr.URL, slack.URL}; !slices.Equal(left, want) {
		t.Errorf("links = %q, want %q", left, want)
	}
	if m.msg != "Removed ABC-12" {
		t.Errorf("message = %q, want Removed ABC-12", m.msg)
	}
}
