package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/claudehook"
	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// hookDeadline is how long claude-hook works before it gives up, say when
// herdr doesn't answer or a sync holds the state lock.
var hookDeadline = 5 * time.Second

const (
	// maxHookInput caps what claude-hook reads. Only a Write of a huge file
	// sends more, and that call is skipped.
	maxHookInput = 8 << 20
	// hookLogMax is the size at which claude-hook.log starts over.
	hookLogMax = 256 << 10
	// maxClaudeLinks is how many links a task has before Claude adds no new
	// ones. Updating or replacing a link still works.
	maxClaudeLinks = 60
)

// errNoOrg is why an issue known only by its ID can't be linked yet: its URL
// needs the Linear workspace, which comes from issue links seen before.
var errNoOrg = errors.New("no Linear URL seen yet")

// runClaudeHook runs `herdr-tasks claude-hook`, which Claude Code runs on
// your prompts, its replies, and some of its tool calls. Claude sees what a
// hook prints and how it exits, so this prints nothing and always succeeds.
// Problems go to claude-hook.log in the state directory.
func runClaudeHook() error {
	quietly(store.Dir(), func() error { return claudeHook(os.Stdin, os.Getenv, linkFound) })
	return nil
}

// logBudget is the part of hookDeadline kept for writing the log, at most a
// fifth of it.
const logBudget = 250 * time.Millisecond

// quietly runs fn, and returns within hookDeadline whatever happens. An
// error, a panic, or giving up goes to claude-hook.log in dir. The log sits
// on the same filesystem as the state, and writing it is given up on at the
// deadline too.
func quietly(dir string, fn func() error) {
	end := time.Now().Add(hookDeadline)
	work := hookDeadline - min(logBudget, hookDeadline/5)
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic: %v", r)
			}
		}()
		done <- fn()
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(work):
		err = fmt.Errorf("gave up after %s", work)
	}
	if err == nil {
		return
	}
	logged := make(chan struct{})
	go func() {
		logHookError(dir, err)
		close(logged)
	}()
	select {
	case <-logged:
	case <-time.After(time.Until(end)):
	}
}

// logHookError adds a line about problem to claude-hook.log in dir, starting
// the file over once it's past hookLogMax.
func logHookError(dir string, problem error) {
	path := filepath.Join(dir, "claude-hook.log")
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, _ := os.Stat(path); fi != nil && fi.Size() > hookLogMax {
		flags |= os.O_TRUNC
	}
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %v\n", time.Now().Format(time.RFC3339), problem)
}

// found is what a hook call found: the Linear issue a tool call worked on,
// and the links a prompt, reply or tool call named.
type found struct {
	issue *claudehook.Issue
	links []claudehook.Link
}

// claudeHook reads a Claude Code hook's input from in and links what it found
// to the task Claude runs in. It does nothing outside herdr, while
// claude_links is off, or when the call names nothing.
func claudeHook(in io.Reader, env func(string) string, link func(paneID string, f found) error) error {
	pane := env("HERDR_PANE_ID")
	if pane == "" {
		return nil
	}
	opts, err := reconcile.LoadOptions()
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	if !opts.ClaudeLinks {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(in, maxHookInput+1))
	if err != nil {
		return err
	}
	if len(b) > maxHookInput {
		return nil
	}
	var p claudehook.Payload
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("reading the hook input (%d bytes): %w", len(b), err)
	}
	f := found{links: claudehook.Links(p)}
	if issue, ok := claudehook.Find(p); ok {
		f.issue = &issue
	}
	if f.issue == nil && len(f.links) == 0 {
		return nil
	}
	return link(pane, f)
}

// linkFound links f to the task of the tab paneID is in. It asks herdr only
// for the tab: the pane's tab may have changed since Claude started, and
// guessing another tab would put the links on the wrong task.
func linkFound(paneID string, f found) error {
	c := herdr.New()
	tab, err := c.PaneTab(paneID)
	if err != nil {
		return err
	}
	if tab == "" {
		return fmt.Errorf("no tab for pane %s", paneID)
	}
	return addFound(store.New(herdr.SessionKey(c.SocketPath)), tab, f)
}

// addFound links f to the task of tab, as added by claude. It edits the state
// file only: a link changes nothing herdr shows, and a sync could move panes
// under Claude. Most calls find their links there or removed already, so it
// reads first and takes the write lock only when something changes.
func addFound(st *store.Store, tab string, f found) error {
	s, err := st.Load()
	if err != nil {
		return err
	}
	// herdr-tasks remove leaves tasks alone until setup runs again.
	if s.Removed {
		return nil
	}
	// Keep the workspace for issues known only by their ID, in any session.
	rememberOrg(st.Dir, foundOrg(s, f))
	if t, ok := s.Tasks[tab]; ok && !changes(t, f) {
		return nil
	}
	afterLoad()
	full := false
	err = st.Update(func(s *store.State) error {
		// remove may have run since the Load above.
		if s.Removed {
			return errNothingToSave
		}
		t, ok := s.Tasks[tab]
		if !ok {
			return fmt.Errorf("no task for tab %s", tab)
		}
		links := f.links
		if is := f.issue; is != nil && !hasIssue(t, is.ID) {
			url := is.URL
			if url == "" {
				org := orgFor(s)
				if org == "" {
					org = readOrg(st.Dir)
				}
				if org == "" {
					return fmt.Errorf("%w, so the workspace for %s is unknown", errNoOrg, is.ID)
				}
				url = task.LinearURL(org, is.ID)
			}
			links = append([]claudehook.Link{{URL: url}}, links...)
		}
		before := len(t.Links)
		if !merge(t, links, time.Now()) {
			return errNothingToSave
		}
		full = before < maxClaudeLinks && len(t.Links) >= maxClaudeLinks
		return nil
	})
	if errors.Is(err, errNothingToSave) {
		return nil
	}
	if err == nil && full {
		logHookError(st.Dir, fmt.Errorf("task %s has %d links; Claude adds no more", tab, maxClaudeLinks))
	}
	return err
}

// changes reports whether addFound would change t.
func changes(t *task.Task, f found) bool {
	if f.issue != nil && !hasIssue(t, f.issue.ID) && len(t.Links) < maxClaudeLinks {
		return true
	}
	c := *t
	// merge edits all three lists, and the dry run mustn't write into t's.
	c.Links = slices.Clone(t.Links)
	c.DismissedLinks = slices.Clone(t.DismissedLinks)
	c.DismissedIssues = slices.Clone(t.DismissedIssues)
	return merge(&c, f.links, time.Time{})
}

// merge adds links to t as found by claude, and reports whether t changed.
//   - An artifact published from a file ties the two together: when you
//     removed either, both stay off. Hooks can arrive in any order, and this
//     is where the tie is known.
//   - A link t has already gets the file and title it lacks. Links you added
//     stay as you made them.
//   - An artifact published from a file takes the place of claude's link to
//     that file, and the file isn't added again.
//   - New links stop at maxClaudeLinks; an artifact that takes a file's place
//     isn't new.
func merge(t *task.Task, links []claudehook.Link, now time.Time) bool {
	changed := false
	var add []claudehook.Link
	for _, l := range links {
		if l.Source != "" && task.KindOf(l.URL) == task.KindArtifact && t.Dismissed(l.URL, l.Source) {
			changed = keepOff(t, l.URL, task.FileURL(l.Source)) || changed
			continue
		}
		key := task.LinkKey(l.URL)
		i := slices.IndexFunc(t.Links, func(o task.Link) bool { return task.LinkKey(o.URL) == key })
		if i < 0 {
			add = append(add, l)
			continue
		}
		o := &t.Links[i]
		if o.By == "" {
			continue
		}
		if o.Source == "" && l.Source != "" {
			o.Source, changed = l.Source, true
		}
		if label := task.CleanLabel(l.Label); o.Label == "" && label != "" {
			o.Label, changed = label, true
		}
	}

	// The files artifacts were published from: those on t, and those about
	// to be added.
	sources := map[string]bool{}
	for _, o := range t.Links {
		if o.Source != "" && task.KindOf(o.URL) == task.KindArtifact {
			sources[filepath.Clean(o.Source)] = true
		}
	}
	for _, l := range add {
		if l.Source != "" && task.KindOf(l.URL) == task.KindArtifact && task.SafeURL(l.URL) && !t.Dismissed(l.URL, l.Source) {
			sources[filepath.Clean(l.Source)] = true
		}
	}
	replaced := map[string]bool{}
	t.Links = slices.DeleteFunc(t.Links, func(o task.Link) bool {
		p, ok := task.FilePath(o.URL)
		if ok && o.By != "" && sources[p] {
			replaced[p] = true
			return true
		}
		return false
	})
	if len(replaced) > 0 {
		changed = true
	}

	for _, l := range add {
		if p, ok := task.FilePath(l.URL); ok && sources[p] {
			continue
		}
		if len(t.Links) >= maxClaudeLinks && (l.Source == "" || !replaced[filepath.Clean(l.Source)]) {
			continue
		}
		if _, ok := t.Add(task.Link{URL: l.URL, Label: task.CleanLabel(l.Label), Source: l.Source, By: "claude", Added: now}); ok {
			changed = true
		}
	}
	return changed
}

// keepOff dismisses urls and drops claude's links to them, and reports
// whether t changed. A URL you linked yourself stays, and isn't dismissed:
// adding it was your call.
func keepOff(t *task.Task, urls ...string) bool {
	changed := false
	for _, u := range urls {
		key := task.LinkKey(u)
		if slices.ContainsFunc(t.Links, func(o task.Link) bool { return o.By == "" && task.LinkKey(o.URL) == key }) {
			continue
		}
		n := len(t.Links)
		t.Links = slices.DeleteFunc(t.Links, func(o task.Link) bool { return task.LinkKey(o.URL) == key })
		if t.Dismiss(u) || len(t.Links) != n {
			changed = true
		}
	}
	return changed
}

// foundOrg is the Linear workspace of what f found: from an issue URL in it,
// else the one in the state's issue links. It's "" when f has no issue.
func foundOrg(s *store.State, f found) string {
	org := ""
	if f.issue != nil {
		org, _ = task.LinearOrg(f.issue.URL)
	}
	for _, l := range f.links {
		if o, ok := task.LinearOrg(l.URL); ok && org == "" {
			org = o
		}
	}
	if org == "" && f.issue != nil {
		org = orgFor(s)
	}
	return org
}

// errNothingToSave ends an Update that leaves the state as it was.
var errNothingToSave = errors.New("nothing to save")

// afterLoad runs between addFound's read and its write. Tests use it to
// change the state in between.
var afterLoad = func() {}

// orgFile holds the Linear workspace of the issue links the hook last saw,
// for every herdr session: each session has its own state file, but the
// workspace is yours.
const orgFile = "linear-org"

// rememberOrg saves org as the workspace for issues known only by their ID.
// It's best effort: without it, those issues wait for a link with a URL.
func rememberOrg(dir, org string) {
	if !validOrg(org) || readOrg(dir) == org {
		return
	}
	f, err := os.CreateTemp(dir, "."+orgFile+"-*")
	if err != nil {
		return
	}
	_, err = f.WriteString(org + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, orgFile))
	}
	if err != nil {
		os.Remove(f.Name())
	}
}

// readOrg is the workspace rememberOrg saved, or "".
func readOrg(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, orgFile))
	if err != nil {
		return ""
	}
	if org := strings.TrimSpace(string(b)); validOrg(org) {
		return org
	}
	return ""
}

// validOrg reports whether org is a workspace a Linear issue URL can name.
func validOrg(org string) bool {
	got, ok := task.LinearOrg(task.LinearURL(org, "X-1"))
	return org != "" && ok && got == org
}

// hasIssue reports whether t links the issue id already, or you removed it.
func hasIssue(t *task.Task, id string) bool {
	if slices.Contains(t.DismissedIssues, id) {
		return true
	}
	for _, l := range t.Links {
		if lid, ok := task.LinearID(l.URL); ok && lid == id {
			return true
		}
	}
	return false
}

// orgFor is the Linear workspace in the state's issue links, for building an
// issue's URL from its ID: the most common one, the first alphabetically on a
// tie, or "" when there are no issue links.
func orgFor(s *store.State) string {
	count := map[string]int{}
	for _, t := range s.Tasks {
		for _, l := range t.Links {
			if org, ok := task.LinearOrg(l.URL); ok {
				count[org]++
			}
		}
	}
	best := ""
	for org, n := range count {
		if n > count[best] || n == count[best] && org < best {
			best = org
		}
	}
	return best
}
