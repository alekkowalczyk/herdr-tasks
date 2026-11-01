package slack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

func TestStateMutes(t *testing.T) {
	var s State
	if !s.Mute("C0123ABCD", "#billing") {
		t.Fatal("first Mute should be true")
	}
	if s.Mute("C0123ABCD", "#billing") {
		t.Fatal("second Mute should be false")
	}
	if !s.IsMuted("C0123ABCD", "#billing", nil) {
		t.Fatal("muted by ID")
	}
	for _, skip := range [][]string{{"random"}, {"#Random"}, {"C0999ZZZZ"}} {
		if !s.IsMuted("C0999ZZZZ", "#random", skip) {
			t.Errorf("skip %v should mute", skip)
		}
	}
	if s.IsMuted("C0999ZZZZ", "#random", []string{"general"}) {
		t.Error("unrelated skip should not mute")
	}
	if !s.Unmute("#billing") {
		t.Fatal("Unmute by name")
	}
	if s.IsMuted("C0123ABCD", "#billing", nil) {
		t.Fatal("still muted")
	}
	s.Mute("C0123ABCD", "#billing")
	if !s.Unmute("C0123ABCD") || s.Unmute("C0123ABCD") {
		t.Fatal("Unmute by ID once")
	}
}

func TestStateSavesMutesInLowerCase(t *testing.T) {
	dir := t.TempDir()
	if err := Update(dir, func(s *State) error { s.Mute("C0123ABCD", "#billing"); return nil }); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file(dir).Path())
	if err != nil {
		t.Fatal(err)
	}
	if want := `"muted":[{"id":"C0123ABCD","name":"#billing"}]`; !bytes.Contains(bytes.Join(bytes.Fields(b), nil), []byte(want)) {
		t.Errorf("state.json = %s, want %s", b, want)
	}
}

func TestStatePruneAndCanStart(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	s := State{
		Starts: []time.Time{
			now.Add(-61 * time.Minute), now.Add(-50 * time.Minute),
			now.Add(-30 * time.Minute), now.Add(-10 * time.Minute), now.Add(-1 * time.Minute),
		},
		Seen: map[string]time.Time{
			"C0123ABCD/1": now.Add(-97 * time.Hour),
			"C0123ABCD/2": now.Add(-95 * time.Hour),
		},
	}
	if s.CanStart(4, now) {
		t.Error("4 starts in the last hour: CanStart(4) should be false")
	}
	if !s.CanStart(5, now) {
		t.Error("CanStart(5) should be true")
	}
	if s.CanStart(0, now) {
		t.Error("CanStart(0) should be false")
	}
	s.Prune(now)
	if len(s.Starts) != 4 {
		t.Errorf("starts = %d", len(s.Starts))
	}
	if _, ok := s.Seen["C0123ABCD/1"]; ok || len(s.Seen) != 1 {
		t.Errorf("seen = %v", s.Seen)
	}
}

func TestStateUpdateAndLoad(t *testing.T) {
	dir := t.TempDir()
	st, err := Load(dir)
	if err != nil || st.On {
		t.Fatalf("empty: %+v, %v", st, err)
	}
	if err := Update(dir, func(s *State) error { s.On = true; s.Mute("D0RUI0001", "a DM"); return nil }); err != nil {
		t.Fatal(err)
	}
	st, err = Load(dir)
	if err != nil || !st.On || !st.IsMuted("D0RUI0001", "", nil) {
		t.Fatalf("loaded: %+v, %v", st, err)
	}
}

// A herdr session named "slack" keeps its tasks in slack.json in the state
// folder. The watcher's state is in the slack folder, with a lock of its own.
func TestStateKeepsClearOfASessionCalledSlack(t *testing.T) {
	dir := t.TempDir()
	sess := &store.Store{Dir: dir, Session: "slack"}
	err := sess.Update(func(s *store.State) error {
		s.Tasks["w1:t1"] = &task.Task{TabID: "w1:t1", Title: "ABC-12 checkout", Stage: task.Active}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := os.ReadFile(sess.Path())
	if err != nil {
		t.Fatal(err)
	}

	// The watcher saves while the session's lock is held: they don't share one.
	saved := make(chan error, 1)
	go func() {
		saved <- sess.Update(func(*store.State) error {
			return Update(dir, func(s *State) error { s.On, s.UserID = true, me; return nil })
		})
	}()
	select {
	case err := <-saved:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("saving the watcher's state waits on the session's lock")
	}

	if got, err := os.ReadFile(sess.Path()); err != nil || !bytes.Equal(got, tasks) {
		t.Errorf("the session's slack.json changed: %s, %v", got, err)
	}
	if s, err := sess.Load(); err != nil || s.Tasks["w1:t1"] == nil || s.Tasks["w1:t1"].Title != "ABC-12 checkout" {
		t.Errorf("the session's tasks: %+v, %v", s, err)
	}
	if st, err := Load(dir); err != nil || !st.On || st.UserID != me {
		t.Errorf("the watcher's state: %+v, %v", st, err)
	}
	for _, name := range []string{"state.json", "state.lock"} {
		if _, err := os.Stat(filepath.Join(dir, "slack", name)); err != nil {
			t.Errorf("slack/%s: %v", name, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "slack")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the slack folder: %v, %v; want mode 0700", fi.Mode().Perm(), err)
	}
}

// TestSkippedListIsKeptSmall: the skipped list keeps the newest 50, one
// entry a message, for 7 days, and each ask's text cut to 2,000 runes.
func TestSkippedListIsKeptSmall(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	lunch := func(i int) Ask {
		return askOf(billing("U0DANA001", fmt.Sprintf("17934%05d.000100", i), "", "<@U0ME00001> lunch?"), "lunch?")
	}
	var s State
	for i := range 51 {
		s.Skip(lunch(i), Skip{Why: "lunch plans"}, now.Add(time.Duration(i)*time.Minute))
	}
	list := s.SkippedList()
	if len(s.Skipped) != 50 || len(list) != 50 {
		t.Fatalf("%d skipped, want the newest 50", len(s.Skipped))
	}
	if list[0].Ask.ID() != lunch(50).ID() || list[49].Ask.ID() != lunch(1).ID() || !list[0].At.Equal(now.Add(50*time.Minute)) {
		t.Errorf("list from %s to %s, want the newest first, and the oldest gone", list[0].Ask.ID(), list[49].Ask.ID())
	}

	// The same message again replaces its entry.
	s.Skip(lunch(10), Skip{Why: "still lunch"}, now.Add(2*time.Hour))
	list = s.SkippedList()
	n := 0
	for _, k := range list {
		if k.Ask.ID() == lunch(10).ID() {
			n++
		}
	}
	if len(list) != 50 || n != 1 || list[0].Why != "still lunch" || list[0].Ask.ID() != lunch(10).ID() {
		t.Errorf("the same message twice: %d entries, %d for it, newest %q", len(list), n, list[0].Why)
	}

	// The text is cut, and the ask is a copy.
	long := askOf(dm("U0RUI0001", "1793455100.000100", strings.Repeat("ø", 3000)), "lunch")
	long.Participants = []string{"U0RUI0001", me}
	s.Skip(long, Skip{Why: "chat"}, now.Add(3*time.Hour))
	long.Participants[0] = "U0DANA001"
	got := s.SkippedList()[0]
	if n := utf8.RuneCountInString(got.Ask.Text); n != 2000 || !strings.HasSuffix(got.Ask.Text, "…") {
		t.Errorf("text has %d runes, want 2,000", n)
	}
	if got.Ask.Participants[0] != "U0RUI0001" || got.Ask.Gist != "lunch" || got.Ask.Permalink != long.Permalink {
		t.Errorf("kept %+v", got.Ask)
	}

	// Prune drops what's over 7 days old.
	fresh := lunch(99)
	s.Skip(fresh, Skip{Why: "lunch plans"}, now.Add(2*24*time.Hour))
	s.Prune(now.Add(9 * 24 * time.Hour))
	if len(s.Skipped) != 1 || s.Skipped[0].Ask.ID() != fresh.ID() {
		t.Errorf("after Prune: %d skipped, want only the one from 7 days ago", len(s.Skipped))
	}

	// Take by number, permalink or message ID, and only once.
	s.Skip(lunch(1), Skip{Why: "lunch plans"}, now.Add(3*24*time.Hour))
	s.Skip(lunch(2), Skip{Why: "lunch plans"}, now.Add(4*24*time.Hour))
	newest := s.SkippedList()[0].N
	if k, ok := s.Take(strconv.Itoa(newest)); !ok || k.Ask.ID() != lunch(2).ID() {
		t.Errorf("Take(%d) = %s, %v; want the newest", newest, k.Ask.ID(), ok)
	}
	if k, ok := s.Take(lunch(1).Permalink); !ok || k.Ask.ID() != lunch(1).ID() {
		t.Errorf("Take(permalink) = %s, %v", k.Ask.ID(), ok)
	}
	if k, ok := s.Take(fresh.ID()); !ok || k.Ask.ID() != fresh.ID() {
		t.Errorf("Take(ID) = %s, %v", k.Ask.ID(), ok)
	}
	for _, which := range []string{"1", "0", fresh.ID(), ""} {
		if k, ok := s.Take(which); ok {
			t.Errorf("Take(%q) = %s from an empty list", which, k.Ask.ID())
		}
	}
}

// TestSkippedSurvivesTheStateFile: the skipped list is saved, with each
// ask whole.
func TestSkippedSurvivesTheStateFile(t *testing.T) {
	dir := t.TempDir()
	a := askOf(group("U0DANA001", "1793454000.000100", "lunch?"), "lunch?")
	a.Unsure = true
	at := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	if err := Update(dir, func(s *State) error { s.Skip(a, Skip{Why: "lunch plans"}, at); return nil }); err != nil {
		t.Fatal(err)
	}
	st, err := Load(dir)
	if err != nil || len(st.Skipped) != 1 {
		t.Fatalf("loaded %+v, %v", st.Skipped, err)
	}
	got := st.Skipped[0]
	if got.Why != "lunch plans" || !got.At.Equal(at) || got.Ask.Key != a.Key || got.Ask.Kind != KindGroup || !got.Ask.At.Equal(a.At) ||
		!got.Ask.Unsure || got.Ask.Author != "Dana Reed" || len(got.Ask.Participants) != 3 {
		t.Errorf("loaded %+v\nwant %+v", got, a)
	}
	if b, _ := os.ReadFile(file(dir).Path()); !bytes.Contains(b, []byte(`"skipped"`)) || !bytes.Contains(b, []byte(`"why"`)) ||
		bytes.Contains(b, []byte(`"maybe_answered"`)) {
		t.Errorf("state.json = %s", b)
	}
	if got.Reason() != "off-topic: lunch plans" {
		t.Errorf("Reason = %q", got.Reason())
	}

	// One you may have answered already says so, and has no why.
	b := askOf(dm("U0RUI0001", "1793454100.000100", "the refund?"), "the refund?")
	if err := Update(dir, func(s *State) error { s.Skip(b, Skip{Answered: true}, at); return nil }); err != nil {
		t.Fatal(err)
	}
	if st, err = Load(dir); err != nil || len(st.Skipped) != 2 {
		t.Fatalf("loaded %+v, %v", st.Skipped, err)
	}
	if got := st.Skipped[1]; got.Ask.ID() != b.ID() || !got.Answered || got.Why != "" || got.Reason() != "may be answered already" {
		t.Errorf("loaded %+v, reason %q", got, got.Reason())
	}
	if b, _ := os.ReadFile(file(dir).Path()); !bytes.Contains(b, []byte(`"maybe_answered": true`)) {
		t.Errorf("state.json = %s", b)
	}
}

// TestSkippedNumbersStay: each skipped ask keeps the number it got, through
// a newer skip, a take and a reload. Take goes by that number, with or
// without a "#", never by the place in the list.
func TestSkippedNumbersStay(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	lunch := func(i int) Ask {
		return askOf(billing("U0DANA001", fmt.Sprintf("17934%05d.000100", i), "", "<@U0ME00001> lunch?"), "lunch?")
	}
	update := func(fn func(*State)) {
		t.Helper()
		if err := Update(dir, func(s *State) error { fn(s); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	numbers := func() []int {
		t.Helper()
		st, err := Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		var ns []int
		for _, k := range st.SkippedList() {
			ns = append(ns, k.N)
		}
		return ns
	}
	update(func(s *State) {
		s.Skip(lunch(1), Skip{Why: "lunch plans"}, at)
		s.Skip(lunch(2), Skip{Answered: true}, at.Add(time.Minute))
	})
	if got := numbers(); fmt.Sprint(got) != "[2 1]" {
		t.Fatalf("numbers, newest first = %v, want [2 1]", got)
	}

	// A newer skip, and a take, leave the numbers as they are.
	update(func(s *State) { s.Skip(lunch(3), Skip{Why: "lunch plans"}, at.Add(2*time.Minute)) })
	update(func(s *State) {
		if k, ok := s.Take("1"); !ok || k.Ask.ID() != lunch(1).ID() || k.N != 1 {
			t.Errorf("Take(1) = %+v, %v; want the first skipped", k, ok)
		}
	})
	if got := numbers(); fmt.Sprint(got) != "[3 2]" {
		t.Errorf("numbers after a skip and a take = %v, want [3 2]", got)
	}
	update(func(s *State) {
		if k, ok := s.Take("1"); ok {
			t.Errorf("Take(1) again = %+v, want none", k)
		}
		if k, ok := s.Take("#3"); !ok || k.Ask.ID() != lunch(3).ID() {
			t.Errorf("Take(#3) = %+v, %v", k, ok)
		}
	})

	// The same message again keeps its number, and a new one gets the next,
	// never one that was used.
	update(func(s *State) {
		s.Skip(lunch(2), Skip{Why: "lunch plans"}, at.Add(time.Hour))
		s.Skip(lunch(4), Skip{Why: "lunch plans"}, at.Add(2*time.Hour))
	})
	if got := numbers(); fmt.Sprint(got) != "[4 2]" {
		t.Errorf("numbers = %v, want [4 2]", got)
	}
	if b, _ := os.ReadFile(file(dir).Path()); !bytes.Contains(b, []byte(`"skip_seq": 4`)) {
		t.Errorf("state.json = %s", b)
	}
}

// TestOldSkippedEntriesGetNumbers: entries saved before they had numbers
// get them when read, oldest first, as Skip would have given them, and keep
// them once the state is saved.
func TestOldSkippedEntriesGetNumbers(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	var asks []Ask
	var entries []string
	for i, text := range []string{"lunch?", "coffee?", "book club?"} {
		a := askOf(dm("U0RUI0001", fmt.Sprintf("17934%05d.000100", i), text), text)
		b, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		asks = append(asks, a)
		// Saved newest first, to show the numbers go by when, not by place.
		entries = append([]string{fmt.Sprintf(`{"ask": %s, "why": "chat", "at": %q}`, b, at.Add(time.Duration(i)*time.Minute).Format(time.RFC3339))}, entries...)
	}
	if err := os.MkdirAll(file(dir).Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"user_id": "U0ME00001", "skipped": [` + strings.Join(entries, ", ") + `]}`
	if err := os.WriteFile(file(dir).Path(), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range asks {
		k, ok := st.Take(strconv.Itoa(i + 1))
		if !ok || k.Ask.ID() != a.ID() {
			t.Errorf("Take(%d) = %+v, %v; want %s", i+1, k, ok, a.Text)
		}
	}
	if st.SkipSeq != 3 {
		t.Errorf("SkipSeq = %d, want 3", st.SkipSeq)
	}

	// The next update saves them, and a new skip gets the next number.
	if err := Update(dir, func(s *State) error {
		s.Skip(askOf(dm("U0RUI0001", "1793499999.000100", "tea?"), "tea?"), Skip{Why: "chat"}, at.Add(time.Hour))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file(dir).Path())
	var saved State
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	var ns []int
	for _, k := range saved.Skipped {
		ns = append(ns, k.N)
	}
	if fmt.Sprint(ns) != "[1 2 3 4]" || saved.SkipSeq != 4 {
		t.Errorf("saved numbers %v, skip_seq %d; want [1 2 3 4] and 4", ns, saved.SkipSeq)
	}
}
