package slack

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

const (
	harnessDir = "/home/u/acme-harness"
	guardExe   = "/home/u/bin/herdr-tasks"
	danaThread = "C0123ABCD/1793450000.000100"
)

var (
	deliverNow = time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	slackSet   = Settings{Workspace: "Slack", Cwd: harnessDir, DMs: true, MaxNew: 4}
	slackOpt   = reconcile.Options{Markers: true, SortTabs: true, Strip: true}
)

// askOf is the ask PickAsks makes of m, with gist, posted at m's ts in UTC.
func askOf(m Message, gist string) Ask {
	at := atTS(m.TS).UTC()
	return ask(&found{m: m, at: at}, map[string]string{m.ID(): gist}, regexp.MustCompile(`<@`+me+`>`))
}

// danaAsk is Dana asking in a #billing thread, Sat Oct 31 13:52 UTC.
func danaAsk() Ask {
	return askOf(billing("U0DANA001", "1793454730.123456", "1793450000.000100",
		"<@U0ME00001> can the invoice fix ship Friday?"), "ship the invoice fix Friday?")
}

// slackDesk is herdr with your own workspace "acme", where you're typing in
// tab w1:t1, and no Slack workspace yet, and a state folder.
func slackDesk(t *testing.T) (*fakeHerdr, *store.Store, string) {
	t.Helper()
	f := newFakeHerdr()
	f.addWorkspace("w1", "acme")
	f.addTab("w1", "w1:t1", "billing fix", "/home/u/acme")
	dir := t.TempDir()
	return f, &store.Store{Dir: dir, Session: "work"}, dir
}

func deliver(t *testing.T, f *fakeHerdr, st *store.Store, dir string, a Ask, research bool) Delivered {
	t.Helper()
	d, err := Deliver(f, st, slackOpt, slackSet, a, guardExe, dir, research, nil, deliverNow)
	if err != nil {
		t.Fatalf("Deliver(%s): %v", a.Key, err)
	}
	return d
}

func loadTask(t *testing.T, st *store.Store, tabID string) *task.Task {
	t.Helper()
	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	tk, ok := s.Tasks[tabID]
	if !ok {
		t.Fatalf("no task for tab %s", tabID)
	}
	return tk
}

func changeTask(t *testing.T, st *store.Store, tabID string, fn func(*task.Task)) {
	t.Helper()
	if err := st.Update(func(s *store.State) error { fn(s.Tasks[tabID]); return nil }); err != nil {
		t.Fatal(err)
	}
}

func lastLog(tk *task.Task) task.Entry {
	if len(tk.Log) == 0 {
		return task.Entry{}
	}
	return tk.Log[len(tk.Log)-1]
}

func TestDeliverMakesTheTabAndTask(t *testing.T) {
	f, st, dir := slackDesk(t)
	a := danaAsk()
	d := deliver(t, f, st, dir, a, false)

	if got := f.callsTo("CreateWorkspace"); !slices.Equal(got, []string{"CreateWorkspace Slack " + harnessDir}) {
		t.Errorf("CreateWorkspace calls = %q", got)
	}
	if got := f.callsTo("CreateTab"); len(got) != 0 {
		t.Errorf("CreateTab calls = %q, want none: a new workspace comes with its tab", got)
	}
	if len(f.created) != 1 || d.Tab != f.created[0].TabID || d.FollowUp || d.Research {
		t.Fatalf("Delivered = %+v, created %+v", d, f.created)
	}
	tk := loadTask(t, st, d.Tab)
	if tk.Title != "Dana Reed: ship the invoice fix Friday?" || tk.Stage != task.Active || tk.Workspace != "Slack" {
		t.Errorf("task = %q %s in %q", tk.Title, tk.Stage, tk.Workspace)
	}
	if got := f.label(d.Tab); got != "● Dana Reed: ship the invoice fix Friday?" {
		t.Errorf("tab label = %q", got)
	}
	wantNote := "Dana Reed in #billing, Sat Oct 31 13:52:\n> <@U0ME00001> can the invoice fix ship Friday?"
	if tk.Note != wantNote || NoteFor(a) != wantNote {
		t.Errorf("note = %q\nwant   %q", tk.Note, wantNote)
	}
	if len(tk.Links) != 1 || tk.Links[0].URL != a.Permalink || tk.Links[0].By != "slack" {
		t.Errorf("links = %+v", tk.Links)
	}
	want := task.SlackAsk{
		Key: danaThread, Channel: "C0123ABCD", Place: "#billing", Author: "Dana Reed",
		TS: "1793454730.123456", ThreadTS: "1793450000.000100", At: a.At, Permalink: a.Permalink,
		Research: task.ResearchHeld,
	}
	if tk.Slack == nil || !tk.Slack.At.Equal(want.At) {
		t.Fatalf("Slack = %+v", tk.Slack)
	}
	got := *tk.Slack
	got.At = want.At
	if got != want {
		t.Errorf("Slack = %+v\nwant    %+v", got, want)
	}
	if tk.StripPane == "" || f.tabOf(tk.StripPane) != d.Tab {
		t.Errorf("strip pane = %q, want one in %s", tk.StripPane, d.Tab)
	}
	if p, _ := f.pane(f.created[0].PaneID); p.Cwd != harnessDir {
		t.Errorf("the tab's pane starts in %q, want %q", p.Cwd, harnessDir)
	}
}

func TestDeliverNamesTheNewWorkspacesTab(t *testing.T) {
	f, st, dir := slackDesk(t)
	plain := reconcile.Options{SortTabs: true, Strip: true} // no stage markers
	d, err := Deliver(f, st, plain, slackSet, danaAsk(), guardExe, dir, false, nil, deliverNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.label(d.Tab); got != "Dana Reed: ship the invoice fix Friday?" {
		t.Errorf("tab label = %q", got)
	}
	if _, err := reconcile.Run(f, st, plain, deliverNow, nil); err != nil {
		t.Fatal(err)
	}
	if got := loadTask(t, st, d.Tab).Title; got != "Dana Reed: ship the invoice fix Friday?" {
		t.Errorf("after another run the title is %q", got)
	}
}

func TestDeliverDoesntFailOnAStrip(t *testing.T) {
	f, st, dir := slackDesk(t)
	f.openErr = errors.New("herdr: split_failed: the pane is too small")
	d, err := Deliver(f, st, slackOpt, slackSet, danaAsk(), guardExe, dir, true, nil, deliverNow)
	if err != nil {
		t.Fatalf("Deliver = %v, want no error: a run adds a missing strip later", err)
	}
	if tk := loadTask(t, st, d.Tab); tk.Slack == nil || tk.Slack.Research != task.ResearchLocked {
		t.Errorf("task = %+v, want the ask with research locked", tk.Slack)
	}
	next := askOf(billing("U0RUI0001", "1793455000.000300", "1793450000.000100", "<@U0ME00001> and the refund?"), "the refund")
	if d2, err := Deliver(f, st, slackOpt, slackSet, next, guardExe, dir, true, nil, deliverNow); err != nil || d2.Tab != d.Tab {
		t.Errorf("follow-up = %+v, %v; want it in %s", d2, err, d.Tab)
	}
	if len(f.created) != 1 {
		t.Errorf("made %d tabs, want 1", len(f.created))
	}
}

func TestDeliverClosesItsTabWhenItFails(t *testing.T) {
	for _, c := range []struct {
		name                string
		slackWS, closeFails bool
	}{
		{"in a new workspace", false, false},
		{"in the Slack workspace", true, false},
		{"when closing fails too", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, st, dir := slackDesk(t)
			if c.slackWS {
				f.addWorkspace("w2", "Slack")
				f.addTab("w2", "w2:t1", "notes", harnessDir)
			}
			busy := errors.New("herdr: server_busy: try again")
			f.fail = func(call string) error {
				switch {
				case call == "Live" && len(f.created) > 0:
					return busy // the run that would fill the new tab's task
				case strings.HasPrefix(call, "CloseTab") && c.closeFails:
					return errors.New("herdr: tab_not_found")
				}
				return nil
			}
			_, err := Deliver(f, st, slackOpt, slackSet, danaAsk(), guardExe, dir, true, nil, deliverNow)
			if !errors.Is(err, busy) {
				t.Fatalf("Deliver = %v, want %v", err, busy)
			}
			if len(f.created) != 1 {
				t.Fatalf("made %+v, want one tab", f.created)
			}
			tab := f.created[0].TabID
			if got := f.callsTo("CloseTab"); !slices.Equal(got, []string{"CloseTab " + tab}) {
				t.Errorf("CloseTab calls = %q, want the new tab %s closed", got, tab)
			}
			if open := slices.ContainsFunc(f.tabs, func(tb herdr.Tab) bool { return tb.ID == tab }); open != c.closeFails {
				t.Errorf("the new tab is open: %v", open)
			}
			if len(f.starts) != 0 {
				t.Errorf("StartAgent called: %+v", f.starts)
			}
			if s, _ := st.Load(); s.Tasks[tab] != nil {
				t.Errorf("a task for the closed tab: %+v", s.Tasks[tab])
			}
		})
	}
}

func TestDeliverUsesTheWorkspace(t *testing.T) {
	f, st, dir := slackDesk(t)
	f.addWorkspace("w2", "Slack")
	f.addTab("w2", "w2:t1", "Rui Costa: refund for ABC-12", harnessDir)
	f.addWorkspace("w3", "Slack")
	f.addTab("w3", "w3:t1", "notes", harnessDir)

	d := deliver(t, f, st, dir, danaAsk(), false)
	if got := f.callsTo("CreateWorkspace"); len(got) != 0 {
		t.Errorf("CreateWorkspace calls = %q, want none", got)
	}
	want := []string{"CreateTab w2 Dana Reed: ship the invoice fix Friday? " + harnessDir}
	if got := f.callsTo("CreateTab"); !slices.Equal(got, want) {
		t.Errorf("CreateTab calls = %q\nwant %q", got, want)
	}
	if !strings.HasPrefix(d.Tab, "w2:") || loadTask(t, st, d.Tab).Workspace != "Slack" {
		t.Errorf("Delivered to %s", d.Tab)
	}
}

func TestDeliverNeverFocuses(t *testing.T) {
	f, st, dir := slackDesk(t)
	d1 := deliver(t, f, st, dir, danaAsk(), true)
	deliver(t, f, st, dir, askOf(billing("U0RUI0001", "1793455000.000300", "1793450000.000100",
		"<@U0ME00001> and the refund?"), "the refund"), true)
	d3 := deliver(t, f, st, dir, askOf(dm("U0RUI0001", "1793455100.000100", "got a minute?"), "got a minute?"), true)

	// Only reading herdr, naming and ordering tabs, sidebar counts, making
	// tabs and strips, and starting Claude.
	allowed := []string{"Live", "Panes", "RenameTab", "MoveTab", "ReportWorkspaceTokens",
		"CreateWorkspace", "CreateTab", "Open", "Layout", "StartAgent"}
	for _, m := range f.methods() {
		if !slices.Contains(allowed, m) {
			t.Errorf("Deliver called %s: %q", m, f.callsTo(m))
		}
	}
	// Claude starts in each new tab's own root pane, and nowhere else.
	var roots []string
	for _, c := range f.created {
		roots = append(roots, c.PaneID)
	}
	var started []string
	for _, s := range f.starts {
		started = append(started, s.Pane)
	}
	if !slices.Equal(started, roots) || len(roots) != 2 {
		t.Errorf("StartAgent in %q, want the new tabs' root panes %q", started, roots)
	}
	if f.tabOf(roots[0]) != d1.Tab || f.tabOf(roots[1]) != d3.Tab {
		t.Errorf("root panes %q aren't in the new tabs %s, %s", roots, d1.Tab, d3.Tab)
	}
}

func TestFollowUpJoinsTheThread(t *testing.T) {
	f, st, dir := slackDesk(t)
	first := deliver(t, f, st, dir, danaAsk(), false)
	changeTask(t, st, first.Tab, func(tk *task.Task) { tk.SetStage(task.Review, "waiting on QA", deliverNow) })
	note := loadTask(t, st, first.Tab).Note

	next := askOf(billing("U0RUI0001", "1793455000.000300", "1793450000.000100",
		"<@U0ME00001> and the refund\n\tnote too?"), "the refund note")
	later := deliverNow.Add(time.Minute)
	d, err := Deliver(f, st, slackOpt, slackSet, next, guardExe, dir, true, nil, later)
	if err != nil {
		t.Fatal(err)
	}
	if d != (Delivered{Tab: first.Tab, FollowUp: true}) {
		t.Errorf("Delivered = %+v, want a follow-up in %s and no research", d, first.Tab)
	}
	if n := len(f.created); n != 1 {
		t.Errorf("made %d tabs, want 1", n)
	}
	if len(f.starts) != 0 {
		t.Errorf("a follow-up started research: %+v", f.starts)
	}
	tk := loadTask(t, st, first.Tab)
	if tk.Stage != task.Active || tk.Reason != "" || !tk.Since.Equal(later) {
		t.Errorf("stage = %s %q since %v, want active since %v", tk.Stage, tk.Reason, tk.Since, later)
	}
	if len(tk.Links) != 2 || tk.Links[1].URL != next.Permalink || tk.Links[1].By != "slack" {
		t.Errorf("links = %+v", tk.Links)
	}
	want := task.Entry{Who: "slack", At: next.At, Text: "Rui Costa: <@U0ME00001> and the refund note too?"}
	if got := lastLog(tk); got.Who != want.Who || !got.At.Equal(want.At) || got.Text != want.Text {
		t.Errorf("log = %+v\nwant  %+v", got, want)
	}
	if tk.Note != note {
		t.Errorf("a follow-up changed the note to %q", tk.Note)
	}

	// The log line keeps the first 200 runes of a long message.
	long := askOf(billing("U0DANA001", "1793455060.000400", "1793450000.000100", strings.Repeat("ø", 300)), "more")
	deliver(t, f, st, dir, long, false)
	text, _ := strings.CutPrefix(lastLog(loadTask(t, st, first.Tab)).Text, "Dana Reed: ")
	if n := utf8.RuneCountInString(text); n != 200 || !strings.HasSuffix(text, "…") {
		t.Errorf("log text has %d runes: %q", n, text)
	}
}

func TestFollowUpFindsAMovedTask(t *testing.T) {
	f, st, dir := slackDesk(t)
	f.addWorkspace("w3", "billing")
	f.addTab("w3", "w3:t9", "Dana Reed: ship the invoice fix Friday?", "/home/u/acme")
	if _, err := reconcile.Run(f, st, slackOpt, deliverNow, nil); err != nil {
		t.Fatal(err)
	}
	changeTask(t, st, "w3:t9", func(tk *task.Task) {
		tk.SetStage(task.Parked, "", deliverNow)
		tk.Slack = &task.SlackAsk{Key: danaThread, Channel: "C0123ABCD", Place: "#billing", Author: "Dana Reed",
			TS: "1793450000.000100", Research: task.ResearchOpen}
	})

	d := deliver(t, f, st, dir, danaAsk(), true)
	if d != (Delivered{Tab: "w3:t9", FollowUp: true}) {
		t.Errorf("Delivered = %+v, want a follow-up in w3:t9", d)
	}
	if len(f.created) != 0 || len(f.callsTo("CreateWorkspace")) != 0 {
		t.Errorf("made %+v for a follow-up", f.created)
	}
	if tk := loadTask(t, st, "w3:t9"); tk.Stage != task.Active || len(tk.Links) != 1 {
		t.Errorf("moved task = %s with links %+v", tk.Stage, tk.Links)
	}
}

func TestDMsJoinUntilDone(t *testing.T) {
	f, st, dir := slackDesk(t)
	hey := askOf(dm("U0RUI0001", "1793454000.000100", "hey"), "hey")
	first := deliver(t, f, st, dir, hey, false)
	if tk := loadTask(t, st, first.Tab); tk.Slack.Key != "D0RUI0001" || tk.Slack.Place != "a DM" ||
		!strings.HasPrefix(tk.Note, "Rui Costa in a DM, Sat Oct 31 13:40:\n> hey") {
		t.Errorf("DM task = %+v, note %q", tk.Slack, tk.Note)
	}

	q := askOf(dm("U0RUI0001", "1793454060.000100", "quick q: can you look at ABC-12?"), "look at ABC-12")
	if d := deliver(t, f, st, dir, q, false); d.Tab != first.Tab || !d.FollowUp {
		t.Errorf("the next DM = %+v, want it in %s", d, first.Tab)
	}

	changeTask(t, st, first.Tab, func(tk *task.Task) { tk.SetStage(task.Done, "", deliverNow) })
	again := askOf(dm("U0RUI0001", "1793455000.000100", "one more thing"), "one more thing")
	d := deliver(t, f, st, dir, again, false)
	if d.FollowUp || d.Tab == first.Tab {
		t.Errorf("a DM after Done = %+v, want a new task", d)
	}
	if tk := loadTask(t, st, first.Tab); tk.Stage != task.Done || len(tk.Links) != 2 {
		t.Errorf("the done DM task = %s with %d links", tk.Stage, len(tk.Links))
	}
}

func TestDeliverHeldResearch(t *testing.T) {
	f, st, dir := slackDesk(t)
	d := deliver(t, f, st, dir, danaAsk(), false)
	if d.Research {
		t.Error("Delivered.Research = true, want false")
	}
	tk := loadTask(t, st, d.Tab)
	if tk.Slack.Research != task.ResearchHeld || tk.Slack.SessionID != "" {
		t.Errorf("Research = %q, session %q", tk.Slack.Research, tk.Slack.SessionID)
	}
	want := "Research not started: 4 research sessions already started this hour (slack_max_new). " +
		"Press s in the strip or Start research on the board to start it."
	if got := lastLog(tk); got.Text != want || got.Who != "slack" || !got.At.Equal(deliverNow) {
		t.Errorf("log = %+v\nwant %q", got, want)
	}
	if len(f.callsTo("StartAgent")) != 0 {
		t.Errorf("StartAgent called: %q", f.callsTo("StartAgent"))
	}

	for max, want := range map[int]string{
		1: "Research not started: 1 research session already started this hour (slack_max_new).",
		0: "Research not started: slack_max_new is 0.",
	} {
		if got := heldLine(max); !strings.HasPrefix(got, want+" Press s in the strip") {
			t.Errorf("heldLine(%d) = %q, want it to start %q", max, got, want)
		}
	}
}

// TestDeliverSkips: an ask that would make a new task asks skip first, and
// one it skips makes nothing. A follow-up never asks.
func TestDeliverSkips(t *testing.T) {
	f, st, dir := slackDesk(t)
	var asked []string
	lunch := func(a Ask) (Skip, bool) { asked = append(asked, a.Key); return Skip{Why: "lunch"}, true }
	d, err := Deliver(f, st, slackOpt, slackSet, danaAsk(), guardExe, dir, true, lunch, deliverNow)
	if err != nil || d.Tab != "" || d.Research || d.Skipped == nil || *d.Skipped != (Skip{Why: "lunch"}) {
		t.Fatalf("Deliver = %+v, %v; want it off-topic", d, err)
	}
	for _, m := range []string{"CreateWorkspace", "CreateTab", "StartAgent", "Open"} {
		if calls := f.callsTo(m); len(calls) != 0 {
			t.Errorf("an off-topic ask called %q", calls)
		}
	}
	if len(f.created) != 0 || len(asked) != 1 {
		t.Errorf("made %+v, asked %q; want nothing made, and one ask", f.created, asked)
	}
	s, _ := st.Load()
	for id, tk := range s.Tasks {
		if tk.Slack != nil {
			t.Errorf("task %s has the ask: %+v", id, tk.Slack)
		}
	}

	// A follow-up joins its task without asking.
	first := deliver(t, f, st, dir, danaAsk(), false)
	next := askOf(billing("U0RUI0001", "1793455000.000300", "1793450000.000100", "<@U0ME00001> lunch after?"), "lunch after?")
	d, err = Deliver(f, st, slackOpt, slackSet, next, guardExe, dir, false, lunch, deliverNow)
	if err != nil || d != (Delivered{Tab: first.Tab, FollowUp: true}) || len(asked) != 1 {
		t.Errorf("follow-up = %+v, %v, asked %q; want it in %s, without asking", d, err, asked, first.Tab)
	}

	// An ask that's on topic gets its tab, and no why.
	keep := func(Ask) (Skip, bool) { return Skip{Why: "about the refund"}, false }
	d, err = Deliver(f, st, slackOpt, slackSet, askOf(dm("U0RUI0001", "1793455100.000100", "the refund?"), "the refund?"), guardExe, dir, false, keep, deliverNow)
	if err != nil || d.Tab == "" || d.FollowUp || d.Skipped != nil {
		t.Errorf("on topic: Deliver = %+v, %v; want a new tab", d, err)
	}
	// An off-topic ask always says why.
	d, err = Deliver(f, st, slackOpt, slackSet, askOf(group("U0DANA001", "1793455200.000100", "lunch?"), "lunch?"), guardExe, dir, false,
		func(Ask) (Skip, bool) { return Skip{}, true }, deliverNow)
	if err != nil || d.Tab != "" || d.Skipped == nil || *d.Skipped != (Skip{Why: noWhy}) {
		t.Errorf("no why: Deliver = %+v, %v; want it off-topic, with a why", d, err)
	}
	// One that may be answered already has no why.
	d, err = Deliver(f, st, slackOpt, slackSet, askOf(group("U0DANA001", "1793455300.000100", "lunch?"), "lunch?"), guardExe, dir, false,
		func(Ask) (Skip, bool) { return Skip{Answered: true}, true }, deliverNow)
	if err != nil || d.Tab != "" || d.Skipped == nil || *d.Skipped != (Skip{Answered: true}) {
		t.Errorf("maybe answered: Deliver = %+v, %v; want it skipped as that", d, err)
	}
}

func TestNoteForPlacesAndCuts(t *testing.T) {
	g := askOf(group("U0DANA001", "1793454000.000100", "first line\n\nthird\tline"), "first line")
	if want := "Dana Reed in a group DM, Sat Oct 31 13:40:\n> first line\n>\n> third line"; NoteFor(g) != want {
		t.Errorf("group note = %q\nwant         %q", NoteFor(g), want)
	}
	long := askOf(billing("U0DANA001", "1793454000.000100", "", strings.Repeat("a", 600)), "a")
	quote, _ := strings.CutPrefix(NoteFor(long), "Dana Reed in #billing, Sat Oct 31 13:40:\n> ")
	if n := utf8.RuneCountInString(quote); n != 500 || !strings.HasSuffix(quote, "…") {
		t.Errorf("quote has %d runes, ends %q", n, quote[len(quote)-5:])
	}
}

func TestTabLabel(t *testing.T) {
	if got := TabLabel(danaAsk()); got != "Dana Reed: ship the invoice fix Friday?" {
		t.Errorf("TabLabel = %q", got)
	}
	a := danaAsk()
	a.Author = "Dana\x1b[31m Reed-Whitaker-Montgomery of the Acme billing team"
	got := TabLabel(a)
	if n := utf8.RuneCountInString(got); n > 60 || !strings.HasPrefix(got, "Dana[31m Reed-Whitaker") || !strings.HasSuffix(got, "…") {
		t.Errorf("TabLabel = %q (%d runes), want it cleaned and cut to 60", got, n)
	}
}

func TestLinkLabel(t *testing.T) {
	cases := []struct{ place, author, text, want string }{
		{"#billing", "Dana Reed", "can you   check the invoice fix before Friday's release, it's urgent",
			"#billing · Dana Reed: can you check the invoice fix before…"},
		{"a DM", "Rui Costa", "hi", "DM · Rui Costa: hi"},
		{"a group DM", "Dana Reed", "lunch?", "group DM · Dana Reed: lunch?"},
		{"", "you", "", "you"},
		{"", "", "", ""},
		{"#billing", "", "", "#billing"},
		{"", "Dana Reed", "hi", "Dana Reed: hi"},
		{"", "you", "hi", "you: hi"},
		{"#billing", "Dana Reed", "", "#billing · Dana Reed"},
		{"#billing", "", "hi", "#billing · hi"},
		// Slack's mention tokens aren't words: yours starts every ask.
		{"#billing", "Dana Reed", "<@U0ME00001> can the invoice fix ship Friday?", "#billing · Dana Reed: can the invoice fix ship Friday?"},
		{"#billing", "Dana Reed", "<@U0ME00001>", "#billing · Dana Reed"},
		{"#billing", "Dana Reed", "ask <@U0RUI0001|rui> too", "#billing · Dana Reed: ask @rui too"},
		// Text from Slack is cleaned.
		{"#bill\x1b[31ming", "Dana\n\tReed", "hi\x07 there\x9b", "#bill[31ming · Dana Reed: hi there"},
	}
	for _, c := range cases {
		if got := LinkLabel(c.place, c.author, c.text); got != c.want {
			t.Errorf("LinkLabel(%q, %q, %q) = %q, want %q", c.place, c.author, c.text, got, c.want)
		}
	}
	long := LinkLabel("#"+strings.Repeat("b", 70), strings.Repeat("D", 70), "hi")
	if n := utf8.RuneCountInString(long); n != 80 || !strings.HasSuffix(long, "…") {
		t.Errorf("a long label has %d runes: %q", n, long)
	}
}

func TestDeliverLabelsTheAskLink(t *testing.T) {
	f, st, dir := slackDesk(t)
	first := deliver(t, f, st, dir, danaAsk(), false)
	next := askOf(billing("U0RUI0001", "1793455000.000300", "1793450000.000100",
		"<@U0ME00001> and the refund\n\tnote too?"), "the refund note")
	deliver(t, f, st, dir, next, false)
	tk := loadTask(t, st, first.Tab)
	want := []string{"#billing · Dana Reed: can the invoice fix ship Friday?", "#billing · Rui Costa: and the refund note too?"}
	if len(tk.Links) != 2 || tk.Links[0].Label != want[0] || tk.Links[1].Label != want[1] {
		t.Errorf("links = %+v, want labels %q", tk.Links, want)
	}

	d := deliver(t, f, st, dir, askOf(dm("U0RUI0001", "1793454000.000100", "quick q: is ABC-12 merged?"), "is ABC-12 merged?"), false)
	if l := loadTask(t, st, d.Tab).Links; len(l) != 1 || l[0].Label != "DM · Rui Costa: quick q: is ABC-12 merged?" {
		t.Errorf("DM links = %+v", l)
	}
	d = deliver(t, f, st, dir, askOf(group("U0DANA001", "1793454000.000100", "lunch?"), "lunch?"), false)
	if l := loadTask(t, st, d.Tab).Links; len(l) != 1 || l[0].Label != "group DM · Dana Reed: lunch?" {
		t.Errorf("group DM links = %+v", l)
	}
}

func TestMessageLabel(t *testing.T) {
	cases := []struct {
		m           Message
		label, only string
	}{
		{billing("U0DANA001", "1793454730.123456", "", "<@U0ME00001> can the invoice fix ship Friday?"),
			"#billing · Dana Reed: can the invoice fix ship Friday?", "#billing"},
		{dm("U0RUI0001", "1793454800.000200", "quick q"), "DM · Rui Costa: quick q", "DM"},
		{group("U0DANA001", "1793454000.000100", "lunch?"), "group DM · Dana Reed: lunch?", "group DM"},
		{msg("C0OPS0001", "", "U0DANA001", "1793454000.000100", "", "hi"), "#C0OPS0001 · Dana Reed: hi", "#C0OPS0001"},
	}
	for _, c := range cases {
		if got := MessageLabel(c.m); got != c.label {
			t.Errorf("MessageLabel(%s) = %q, want %q", c.m.ID(), got, c.label)
		}
		if got := PlaceLabel(c.m); got != c.only {
			t.Errorf("PlaceLabel(%s) = %q, want %q", c.m.ID(), got, c.only)
		}
	}
}

// TestDeliverReportsResearchThatDidntStart: the ask reaches its task
// whatever happens to its research, and Delivered says what did.
func TestDeliverReportsResearchThatDidntStart(t *testing.T) {
	fastRetries(t)
	busy := errors.New("herdr: pane_busy: the pane isn't at a shell prompt")

	// The start fails.
	f, st, dir := slackDesk(t)
	f.startErrs = []error{busy, busy, busy, busy, busy}
	d, err := Deliver(f, st, slackOpt, slackSet, danaAsk(), guardExe, dir, true, nil, deliverNow)
	if err != nil || !d.Research || d.ResearchErr == nil || d.ResearchErr.Error() != "Research didn't start: "+busy.Error() {
		t.Errorf("Deliver = %+v, %v; want research tried, and why it didn't start", d, err)
	}

	// The claim fails: herdr goes away after the tab is made.
	probe, pst, pdir := slackDesk(t)
	deliver(t, probe, pst, pdir, danaAsk(), false)
	before := len(probe.callsTo("Live"))
	f, st, dir = slackDesk(t)
	lives := 0
	gone := errors.New("can't reach herdr")
	f.fail = func(call string) error {
		if call == "Live" {
			if lives++; lives > before {
				return gone
			}
		}
		return nil
	}
	d, err = Deliver(f, st, slackOpt, slackSet, danaAsk(), guardExe, dir, true, nil, deliverNow)
	if err != nil || d.ResearchErr == nil || !errors.Is(d.ResearchErr, gone) || len(f.starts) != 0 {
		t.Errorf("a failed claim: Deliver = %+v, %v, %d starts; want the claim's error", d, err, len(f.starts))
	}

	// The start fails, and so does recording it.
	f, st, dir = slackDesk(t)
	f.startErrs = []error{busy, busy, busy, busy, busy}
	started := false
	f.onStart = func(agentStart) { started = true }
	f.fail = func(call string) error {
		if started && call == "Live" {
			return gone
		}
		return nil
	}
	d, err = Deliver(f, st, slackOpt, slackSet, danaAsk(), guardExe, dir, true, nil, deliverNow)
	if err != nil || d.ResearchErr == nil || !strings.Contains(d.ResearchErr.Error(), busy.Error()) || !errors.Is(d.ResearchErr, gone) {
		t.Errorf("a failed record: Deliver = %+v, %v; want both errors", d, err)
	}
}

// TestDeliverFindsTheWorkspaceInAnyCase: slack_workspace "Slack" is the
// workspace you named "slack".
func TestDeliverFindsTheWorkspaceInAnyCase(t *testing.T) {
	f, st, dir := slackDesk(t)
	f.addWorkspace("w2", "slack")
	f.addTab("w2", "w2:t1", "notes", harnessDir)
	d := deliver(t, f, st, dir, danaAsk(), false)
	if got := f.callsTo("CreateWorkspace"); len(got) != 0 || !strings.HasPrefix(d.Tab, "w2:") {
		t.Errorf("CreateWorkspace calls %q, tab %s; want the tab in w2", got, d.Tab)
	}
}
