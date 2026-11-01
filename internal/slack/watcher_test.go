package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// watchStart is Fri Oct 30 2026 14:00 UTC, when the tests' watcher first
// ticks.
var watchStart = time.Date(2026, 10, 30, 14, 0, 0, 0, time.UTC)

// tsAt is the Slack ts of a message posted at t.
func tsAt(t time.Time) string { return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000) }

// fakeClock is the watcher's clock, moved by the test.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

// fakeSlack is Claude with the Slack connector, as a check sees it. The
// identify run reads your profile. A check's searches find the messages
// posted from its start on, at most 20 each: oldest first, and your own
// newest first. Each run's prompt is kept.
type fakeSlack struct {
	mentions, dms, groups, own []Message
	signin                     bool // Slack wants you to sign in again
	runs                       []string
	// extra is more input for the mention search, which then isn't the
	// search the check asked for.
	extra map[string]any
	// unanswered leaves out the mention search's result.
	unanswered bool
	// verdicts answer the topic checks, in order; past them, an ask is on
	// topic. judged keeps each topic check's prompt, and args each run's
	// arguments, checks' and topic checks'.
	verdicts []verdict
	judged   []string
	args     [][]string
}

// verdict is a topic check's answer, or a run that fails.
type verdict struct {
	relevant bool
	why      string
	fails    bool
}

// judgeCost is what each of the fake's topic checks costs.
const judgeCost = 0.0011

var startsAt = regexp.MustCompile(`uses after (\d+),`)

func (s *fakeSlack) run(_ context.Context, _ string, args []string, stdin string) ([]byte, error) {
	s.args = append(s.args, slices.Clone(args))
	// A topic check runs with no MCP servers, and reads its prompt.
	if slices.Contains(args, "--strict-mcp-config") {
		return s.judge(stdin), nil
	}
	prompt := args[1]
	s.runs = append(s.runs, prompt)
	var out bytes.Buffer
	emit := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		out.Write(append(b, '\n'))
	}
	status := "connected"
	if s.signin {
		status = "needs-auth"
	}
	emit(map[string]any{"type": "system", "subtype": "init", "mcp_servers": []any{map[string]string{"name": "plugin:slack:slack", "status": status}}})
	call := func(id, tool string, input map[string]any, result string) {
		emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": id, "name": "mcp__plugin_slack_slack__" + tool, "input": input}}}})
		if s.unanswered && id == "t1" {
			return
		}
		emit(map[string]any{"type": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": id, "content": result}}}})
	}
	switch {
	case s.signin:
	case prompt == IdentifyPrompt:
		call("p1", profileTool, map[string]any{}, `{"result": "User ID: `+me+`\nUsername: me\n"}`)
	default:
		m := startsAt.FindStringSubmatch(prompt)
		if m == nil {
			return nil, fmt.Errorf("the prompt has no start: %q", prompt)
		}
		after, _ := strconv.ParseInt(m[1], 10, 64)
		type search struct {
			input map[string]any
			msgs  []Message
		}
		searches := []search{{map[string]any{"keywords": []string{"<@" + me + ">"}, "sort_dir": "asc"}, s.mentions}}
		for k, v := range s.extra {
			searches[0].input[k] = v
		}
		if strings.Contains(prompt, "is:dm") {
			searches = append(searches,
				search{map[string]any{"keywords": []string{}, "filters": "is:dm", "channel_types": "im", "sort_dir": "asc"}, s.dms},
				search{map[string]any{"keywords": []string{}, "filters": "is:mpim", "channel_types": "mpim", "sort_dir": "asc"}, s.groups})
		}
		searches = append(searches, search{map[string]any{"keywords": []string{}, "filters": "from:<@" + me + ">", "sort_dir": "desc"}, s.own})
		for i, se := range searches {
			se.input["after"], se.input["sort"], se.input["limit"] = after, "timestamp", 20
			call(fmt.Sprintf("t%d", i+1), searchTool, se.input, searchAnswer(se.msgs, after, se.input["sort_dir"] == "desc"))
		}
	}
	emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "done",
		"total_cost_usd": 0.0079, "structured_output": map[string]any{"gists": []any{}}})
	return out.Bytes(), nil
}

// judge answers a topic check with the next verdict.
func (s *fakeSlack) judge(prompt string) []byte {
	v := verdict{relevant: true, why: "about the billing service"}
	if n := len(s.judged); n < len(s.verdicts) {
		v = s.verdicts[n]
	}
	s.judged = append(s.judged, prompt)
	result := map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "", "total_cost_usd": judgeCost,
		"structured_output": map[string]any{"relevant": v.relevant, "why": v.why}}
	if v.fails {
		result = map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": "Claude AI usage limit reached", "total_cost_usd": 0.0}
	}
	b, err := json.Marshal(result)
	if err != nil {
		panic(err)
	}
	return judgeOutput(string(b))
}

// searchAnswer is Slack's answer to a search from after: a page of the
// messages posted from then on.
func searchAnswer(msgs []Message, after int64, desc bool) string {
	var page []Message
	for _, m := range msgs {
		if atTS(m.TS).Unix() >= after {
			page = append(page, m)
		}
	}
	sort.SliceStable(page, func(i, j int) bool { return atTS(page[i].TS).Before(atTS(page[j].TS)) != desc })
	paging := "No more results."
	if len(page) > pageSize {
		page, paging = page[:pageSize], "For the next page of results use cursor `abc123`"
	}
	var b strings.Builder
	b.WriteString("# Search Results\n")
	for i, m := range page {
		fmt.Fprintf(&b, "\n### Result %d of %d\nChannel: %s (ID: %s)\n", i+1, len(page), m.Place, m.Channel)
		if len(m.Participants) > 0 {
			var who []string
			for _, id := range m.Participants {
				who = append(who, fmt.Sprintf("%s (ID: %s)", names[id], id))
			}
			fmt.Fprintf(&b, "Participants: %s\n", strings.Join(who, ", "))
		}
		from := fmt.Sprintf("%s (ID: %s)", m.Author, m.AuthorID)
		if m.Bot {
			from += "  [bot]"
		}
		fmt.Fprintf(&b, "From: %s\nTime: 2026-10-30 14:00:00 UTC\nMessage_ts: %s\nPermalink: [link](%s)\nText: %s\n\n---\n", from, m.TS, m.Permalink, m.Text)
	}
	answer, _ := json.Marshal(map[string]string{"results": b.String(), "pagination_info": paging})
	return string(answer)
}

// rig is a watcher with fakes all round: the clock, Claude and Slack, and
// one herdr session, "work", with your workspace "acme". Slack is on,
// around the clock, and research sessions start in a folder that exists.
type rig struct {
	*Watcher
	clock   *fakeClock
	slack   *fakeSlack
	herdr   *fakeHerdr
	store   *store.Store
	opts    reconcile.Options
	running []Session
	log     bytes.Buffer
	changed int // how many times Changed was called
}

func newRig(t *testing.T) *rig {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	f, st, dir := slackDesk(t)
	r := &rig{clock: &fakeClock{t: watchStart}, slack: &fakeSlack{}, herdr: f, store: st}
	r.opts = reconcile.Options{
		Markers: true, SortTabs: true, Strip: true,
		SlackWatch: true, SlackSession: "work", SlackWorkspace: "Slack", SlackCwd: t.TempDir(), SlackDMs: true,
		SlackEvery: "5m", SlackHours: "always", SlackMaxNew: 4,
	}
	r.running = []Session{{Name: "work", Herdr: f, Store: st}}
	r.Watcher = &Watcher{
		Dir: dir, Exe: guardExe,
		Options:  func() (reconcile.Options, error) { return r.opts, nil },
		Claude:   r.slack.run,
		Sessions: func() []Session { return r.running },
		Now:      r.clock.now,
		Log:      log.New(&r.log, "", 0),
		Changed:  func() { r.changed++ },
	}
	return r
}

func (r *rig) tick() { r.Tick(context.Background()) }

func (r *rig) state(t *testing.T) State {
	t.Helper()
	st, err := Load(r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// tabs is how many tabs were made for asks.
func (r *rig) tabs() int {
	r.herdr.mu.Lock()
	defer r.herdr.mu.Unlock()
	return len(r.herdr.created)
}

// identified has the rig's first tick identify you, at watchStart.
func (r *rig) identified(t *testing.T) {
	t.Helper()
	r.tick()
	if st := r.state(t); st.UserID != me {
		t.Fatalf("not identified: %+v, status %+v", st, r.Status())
	}
}

func TestTickOffDoesNothing(t *testing.T) {
	r := newRig(t)
	r.opts.SlackWatch = false
	r.tick()
	r.tick()
	if len(r.slack.runs) != 0 {
		t.Errorf("Claude ran: %q", r.slack.runs)
	}
	if got := r.Status(); got != (Status{Text: "Slack: off"}) {
		t.Errorf("status = %+v", got)
	}
	if r.changed != 1 {
		t.Errorf("Changed called %d times, want once", r.changed)
	}
	if calls := r.herdr.methods(); len(calls) != 0 {
		t.Errorf("herdr was asked %v", calls)
	}
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the state folder has %d entries (%s first); nothing should be made while Slack is off", len(entries), entries[0].Name())
	}
	if r.log.Len() != 0 {
		t.Errorf("logged %q", r.log.String())
	}
}

// TestStatusOffersFetchOnceUsed: the board offers Fetch while the watcher
// is on, and while it's off once a check found who you are. Off, the status
// reads that from the state file, and makes none.
func TestStatusOffersFetchOnceUsed(t *testing.T) {
	r := newRig(t)
	r.opts.SlackWatch = false
	r.tick()
	if got := r.Status(); got != (Status{Text: "Slack: off"}) {
		t.Errorf("status with no state = %+v, want off with no fetch", got)
	}
	if entries, err := os.ReadDir(r.Dir); err != nil || len(entries) != 0 {
		t.Errorf("the state folder: %v, %d entries; want nothing made", err, len(entries))
	}

	if err := Update(r.Dir, func(s *State) error { s.UserID = me; return nil }); err != nil {
		t.Fatal(err)
	}
	r.tick()
	if got := r.Status(); got != (Status{Text: "Slack: off", Fetch: true}) {
		t.Errorf("status once identified = %+v, want off with a fetch", got)
	}

	// On, the watcher offers one, whatever it shows.
	r.opts.SlackWatch = true
	r.tick()
	if got := r.Status(); got != (Status{On: true, Fetch: true, Text: "Slack: last check 14:00"}) {
		t.Errorf("status on = %+v", got)
	}
	r.opts.SlackHours = "Mon-Fri 08:00-09:00" // closed at 14:00
	r.tick()
	if got := r.Status(); !got.On || !got.Fetch {
		t.Errorf("status outside the hours = %+v, want a fetch", got)
	}
	r.opts.SlackHours = "always"
	r.running = nil
	r.clock.add(5 * time.Minute)
	r.tick()
	if got := r.Status(); !got.On || !got.Problem || !got.Fetch {
		t.Errorf("status with no session = %+v, want the problem with a fetch", got)
	}
}

func TestTickFirstRunIdentifiesWithoutBacklog(t *testing.T) {
	r := newRig(t)
	r.slack.mentions = []Message{billing("U0DANA001", tsAt(watchStart.Add(-time.Hour)), "", "<@U0ME00001> still on for the invoice fix?")}
	r.tick()
	if !slices.Equal(r.slack.runs, []string{IdentifyPrompt}) {
		t.Fatalf("runs = %q, want only the identify run", r.slack.runs)
	}
	st := r.state(t)
	if st.UserID != me || !st.On || !st.Since.Equal(watchStart) || !st.LastCheck.Equal(watchStart) || !st.LastTry.Equal(watchStart) {
		t.Errorf("state = %+v", st)
	}
	if r.tabs() != 0 {
		t.Errorf("%d tabs made", r.tabs())
	}
	if got := r.Status(); got != (Status{On: true, Fetch: true, Text: "Slack: last check 14:00"}) {
		t.Errorf("status = %+v", got)
	}

	// The first check starts 10 minutes before then: the mention an hour
	// before isn't in it.
	r.clock.add(5 * time.Minute)
	r.tick()
	if len(r.slack.runs) != 2 || !strings.Contains(r.slack.runs[1], fmt.Sprintf("after %d,", watchStart.Add(-10*time.Minute).Unix())) {
		t.Fatalf("runs = %q", r.slack.runs)
	}
	if r.tabs() != 0 {
		t.Errorf("%d tabs made for the backlog", r.tabs())
	}
}

func TestTickDeliversAndMovesTheWatermark(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	dana := billing("U0DANA001", tsAt(watchStart.Add(time.Minute)), "", "<@U0ME00001> can the invoice fix ship Friday?")
	rui := dm("U0RUI0001", tsAt(watchStart.Add(2*time.Minute)), "quick q: is ABC-12 merged?")
	bot := billing("B0BOT0001", tsAt(watchStart.Add(3*time.Minute)), "", "<@U0ME00001> deploy 41 is waiting")
	bot.Author, bot.Bot = "Deploys", true
	r.slack.mentions, r.slack.dms = []Message{dana, bot}, []Message{rui}

	// Not due before slack_every has passed.
	r.clock.add(4 * time.Minute)
	r.tick()
	if len(r.slack.runs) != 1 {
		t.Fatalf("checked before slack_every passed: %q", r.slack.runs)
	}

	r.clock.add(time.Minute)
	checked := r.clock.now()
	r.tick()
	if len(r.slack.runs) != 2 || !strings.Contains(r.slack.runs[1], fmt.Sprintf("after %d,", From(watchStart, watchStart, checked).Unix())) {
		t.Fatalf("runs = %q", r.slack.runs)
	}
	if r.tabs() != 2 {
		t.Fatalf("%d tabs, want one per ask; status %+v, log:\n%s", r.tabs(), r.Status(), r.log.String())
	}
	if calls := r.herdr.callsTo("FocusTab"); len(calls) != 0 {
		t.Errorf("focused %q", calls)
	}
	st := r.state(t)
	if !st.Since.Equal(checked) || !st.LastCheck.Equal(checked) || st.LastCost != 0.0079 || st.Problem != "" || st.Failures != 0 {
		t.Errorf("state = %+v", st)
	}
	for _, m := range []Message{dana, rui, bot} {
		if at, ok := st.Seen[m.ID()]; !ok || !at.Equal(atTS(m.TS)) {
			t.Errorf("seen %s = %v, %v; want when it was posted", m.ID(), at, ok)
		}
	}
	if len(st.Starts) != 2 {
		t.Errorf("starts = %v, want the two research sessions", st.Starts)
	}
	if got := r.Status(); got != (Status{On: true, Fetch: true, Text: "Slack: last check 14:05"}) {
		t.Errorf("status = %+v", got)
	}
	logged := r.log.String()
	if !strings.Contains(logged, "dropped C0123ABCD/"+bot.TS+": from a bot or app") {
		t.Errorf("the bot's message isn't logged as dropped:\n%s", logged)
	}
	for _, text := range []string{"invoice", "ABC-12", "deploy"} {
		if strings.Contains(logged, text) {
			t.Errorf("the log has message text %q:\n%s", text, logged)
		}
	}

	// The next check finds them again, and makes nothing new.
	r.clock.add(5 * time.Minute)
	r.tick()
	if len(r.slack.runs) != 3 || r.tabs() != 2 {
		t.Errorf("runs %d, tabs %d; want 3 runs and still 2 tabs", len(r.slack.runs), r.tabs())
	}
	if st := r.state(t); !st.Since.Equal(r.clock.now()) {
		t.Errorf("since = %v, want %v", st.Since, r.clock.now())
	}
}

func TestTickOutsideHours(t *testing.T) {
	r := newRig(t)
	r.opts.SlackHours = "Mon-Fri 08:00-18:00"
	r.clock.t = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) // a Saturday
	r.tick()
	r.clock.add(time.Minute)
	r.tick()
	if len(r.slack.runs) != 0 {
		t.Errorf("Claude ran: %q", r.slack.runs)
	}
	if got := r.Status(); got != (Status{On: true, Fetch: true, Text: "Slack: outside work hours; next check Mon 08:00"}) {
		t.Errorf("status = %+v", got)
	}
	if n := strings.Count(r.log.String(), "outside work hours"); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, r.log.String())
	}

	r.clock.t = time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	r.tick()
	if !slices.Equal(r.slack.runs, []string{IdentifyPrompt}) {
		t.Errorf("Monday 08:00: runs = %q", r.slack.runs)
	}
}

func TestTickBacksOff(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	since := r.state(t).Since
	r.slack.signin = true
	r.clock.add(5 * time.Minute)
	first := r.clock.now()
	for i, wait := range []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute} {
		failed := r.clock.now()
		r.tick()
		st := r.state(t)
		if len(r.slack.runs) != i+2 {
			t.Fatalf("failure %d: %d runs", i+1, len(r.slack.runs))
		}
		if st.Failures != i+1 || st.NextTry == nil || st.NextTry.Sub(failed) != wait {
			t.Fatalf("failure %d: failures %d, next try %v; want %v after %v", i+1, st.Failures, st.NextTry, wait, failed)
		}
		if st.ProblemSince == nil || !st.ProblemSince.Equal(first) || !st.Since.Equal(since) {
			t.Errorf("failure %d: problem since %v, since %v", i+1, st.ProblemSince, st.Since)
		}
		want := Status{On: true, Problem: true, Fetch: true, Text: "Slack: can't reach Slack since 14:05. Run /mcp in Claude Code to sign in again"}
		if got := r.Status(); got != want {
			t.Errorf("failure %d: status = %+v", i+1, got)
		}
		// Nothing runs before the next try.
		r.clock.t = st.NextTry.Add(-time.Second)
		r.tick()
		if len(r.slack.runs) != i+2 {
			t.Fatalf("ran before the next try")
		}
		r.clock.t = *st.NextTry
	}
	if n := strings.Count(r.log.String(), "can't reach Slack"); n != 4 {
		t.Errorf("logged %d failures, want 4:\n%s", n, r.log.String())
	}

	r.slack.signin = false
	r.tick()
	st := r.state(t)
	if st.Failures != 0 || st.NextTry != nil || st.Problem != "" || st.ProblemSince != nil || !st.LastCheck.Equal(r.clock.now()) {
		t.Errorf("after a check that worked: %+v", st)
	}
	if got := r.Status(); got.Problem || !strings.HasPrefix(got.Text, "Slack: last check ") {
		t.Errorf("status = %+v", got)
	}
}

func TestTickNeverStartsMoreThanTheHourlyLimit(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	for i := range 20 {
		at := watchStart.Add(time.Duration(i+1) * 10 * time.Second)
		r.slack.mentions = append(r.slack.mentions, billing("U0DANA001", tsAt(at), "", fmt.Sprintf("<@U0ME00001> question %d", i+1)))
	}
	r.clock.add(5 * time.Minute)
	r.tick()
	if r.tabs() != 20 {
		t.Fatalf("%d tabs, want 20; status %+v", r.tabs(), r.Status())
	}
	if starts := r.herdr.callsTo("StartAgent"); len(starts) != 4 {
		t.Errorf("%d research sessions started, want 4", len(starts))
	}
	s, err := r.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	held := 0
	for _, tk := range s.Tasks {
		if tk.Slack != nil && tk.Slack.Research == task.ResearchHeld {
			held++
		}
	}
	if held != 16 {
		t.Errorf("%d tasks held, want 16", held)
	}
	if st := r.state(t); len(st.Starts) != 4 {
		t.Errorf("starts = %v", st.Starts)
	}

	// Within the hour, the next ask is held too.
	r.slack.mentions = append(r.slack.mentions, billing("U0RUI0001", tsAt(r.clock.now().Add(time.Minute)), "", "<@U0ME00001> one more"))
	r.clock.add(5 * time.Minute)
	r.tick()
	if r.tabs() != 21 || len(r.herdr.callsTo("StartAgent")) != 4 {
		t.Errorf("tabs %d, starts %d; want 21 and still 4", r.tabs(), len(r.herdr.callsTo("StartAgent")))
	}
}

func TestTickNeedsASession(t *testing.T) {
	r := newRig(t)
	home := Session{Name: "home", Herdr: newFakeHerdr(), Store: &store.Store{Dir: r.Dir, Session: "home"}}
	work := r.running[0]

	r.opts.SlackSession = "work"
	r.running = []Session{home}
	r.tick()
	want := Status{On: true, Problem: true, Fetch: true, Text: "Slack: waiting for herdr session work. Set slack_session in config.toml to use another"}
	if got := r.Status(); got != want {
		t.Errorf("status = %+v, want %+v", got, want)
	}
	if st := r.state(t); st.Problem != "waiting for herdr session work. Set slack_session in config.toml to use another" || st.NextTry != nil || st.Failures != 0 {
		t.Errorf("state = %+v", st)
	}

	r.opts.SlackSession = ""
	r.running = []Session{work, home}
	r.clock.add(30 * time.Second) // no backoff: the next tick looks again
	r.tick()
	want.Text = "Slack: waiting for herdr session default. Set slack_session in config.toml to use another"
	if got := r.Status(); got != want {
		t.Errorf("status = %+v, want %+v", got, want)
	}
	if len(r.slack.runs) != 0 {
		t.Errorf("Claude ran: %q", r.slack.runs)
	}

	r.opts.SlackSession = "work"
	r.clock.add(30 * time.Second)
	r.tick()
	if !slices.Equal(r.slack.runs, []string{IdentifyPrompt}) || r.Status().Problem {
		t.Errorf("with work running: runs %q, status %+v", r.slack.runs, r.Status())
	}
	if st := r.state(t); st.Problem != "" {
		t.Errorf("problem = %q", st.Problem)
	}
}

func TestTickTurningOnAgainStartsFresh(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	r.clock.add(5 * time.Minute)
	r.tick()

	r.opts.SlackWatch = false
	r.tick()
	if st := r.state(t); st.On {
		t.Errorf("still on: %+v", st)
	}
	if got := r.Status(); got != (Status{Text: "Slack: off", Fetch: true}) {
		t.Errorf("status = %+v", got)
	}
	// Over 72 h pass, with asks.
	for i := range 3 {
		at := r.clock.now().Add(time.Duration(i+1) * 6 * time.Hour)
		r.slack.mentions = append(r.slack.mentions, billing("U0DANA001", tsAt(at), "", fmt.Sprintf("<@U0ME00001> question %d", i+1)))
	}
	r.clock.add(73 * time.Hour)

	r.opts.SlackWatch = true
	r.tick()
	if got := r.slack.runs[len(r.slack.runs)-1]; got != IdentifyPrompt {
		t.Fatalf("turning on ran %q, want the identify run", got)
	}
	if st := r.state(t); !st.On || !st.Since.Equal(r.clock.now()) {
		t.Errorf("state = %+v", st)
	}
	r.clock.add(5 * time.Minute)
	r.tick()
	if r.tabs() != 0 {
		t.Errorf("%d tabs made for the day it was off", r.tabs())
	}
}

// TestTurnOnKeepsARecentWatermark: turning the watcher on continues from a
// check or fetch within 72 h, so the asks since then aren't lost.
func TestTurnOnKeepsARecentWatermark(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	r.clock.add(5 * time.Minute)
	r.tick()
	since := r.state(t).Since
	r.opts.SlackWatch = false
	r.tick()

	// An hour off, with an ask, and a problem and a backoff from before.
	r.clock.add(time.Hour)
	ask := billing("U0DANA001", tsAt(since.Add(30*time.Minute)), "", "<@U0ME00001> can the invoice fix ship Friday?")
	r.slack.mentions = []Message{ask}
	later := r.clock.now().Add(time.Hour)
	if err := Update(r.Dir, func(s *State) error {
		s.Failures, s.NextTry, s.Problem, s.ProblemSince = 2, &later, "can't reach Slack", &since
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.opts.SlackWatch = true
	runs := len(r.slack.runs)
	r.tick()
	if len(r.slack.runs) != runs+1 || r.slack.runs[runs] == IdentifyPrompt {
		t.Fatalf("turning on ran %q, want a check and no identify run", r.slack.runs[runs:])
	}
	if from := since.Add(-10 * time.Minute); !strings.Contains(r.slack.runs[runs], fmt.Sprintf("uses after %d,", from.Unix())) {
		t.Errorf("the check didn't start at %v: %q", from, r.slack.runs[runs])
	}
	if r.tabs() != 1 {
		t.Errorf("%d tabs, want one for the ask while off; log:\n%s", r.tabs(), r.log.String())
	}
	if st := r.state(t); !st.On || st.UserID != me || st.Failures != 0 || st.NextTry != nil || st.Problem != "" {
		t.Errorf("state = %+v", st)
	}

	// Off for over 72 h since the last check: turning on starts fresh.
	r.opts.SlackWatch = false
	r.tick()
	r.clock.add(73 * time.Hour)
	r.opts.SlackWatch = true
	r.tick()
	if got := r.slack.runs[len(r.slack.runs)-1]; got != IdentifyPrompt {
		t.Fatalf("turning on ran %q, want the identify run", got)
	}
	if st := r.state(t); !st.Since.Equal(r.clock.now()) {
		t.Errorf("since = %v, want now", st.Since)
	}
}

// TestTurnOnKeepsTheWatermarkOfAStoppedFetch: a fetch that was stopped
// saved nothing, so LastCheck is old, but it tried an hour ago. Turning on
// still continues from the watermark, and the catch-up isn't lost.
func TestTurnOnKeepsTheWatermarkOfAStoppedFetch(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	r.opts.SlackWatch = false
	r.tick()
	r.clock.add(73 * time.Hour)
	now := r.clock.now()
	if err := Update(r.Dir, func(s *State) error {
		s.LastCheck, s.LastTry = now.Add(-73*time.Hour), now.Add(-time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.opts.SlackWatch = true
	runs := len(r.slack.runs)
	r.tick()
	if len(r.slack.runs) != runs+1 || r.slack.runs[runs] == IdentifyPrompt {
		t.Fatalf("turning on ran %q, want a check and no identify run", r.slack.runs[runs:])
	}
	if from := now.Add(-catchUp); !strings.Contains(r.slack.runs[runs], fmt.Sprintf("uses after %d,", from.Unix())) {
		t.Errorf("the check didn't start at %v: %q", from, r.slack.runs[runs])
	}
	if st := r.state(t); !st.On || st.UserID != me || st.Since.IsZero() {
		t.Errorf("state = %+v", st)
	}
}

// TestTickBlockedAtTurnOnShowsAStatus: a tick that waits for a fetch to turn
// the watcher on says so in the status.
func TestTickBlockedAtTurnOnShowsAStatus(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	r.opts.SlackWatch = false
	r.tick()
	r.clock.add(time.Hour)
	unlock, err := LockChecks(context.Background(), r.Dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	r.opts.SlackWatch = true
	r.tick()
	want := Status{On: true, Fetch: true, Text: "Slack: a fetch is running"}
	if got := r.Status(); got != want {
		t.Errorf("status = %+v, want %+v", got, want)
	}
}

// TestTurningOnWaitsForAFetch: the watcher turns on under check.lock, as
// turning on may clear the watermark a fetch checks from. While a fetch
// holds the lock, a tick that would turn it on changes nothing. The next
// tick after the fetch turns it on.
func TestTurningOnWaitsForAFetch(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	r.clock.add(5 * time.Minute)
	r.tick()
	r.opts.SlackWatch = false
	r.tick()
	r.clock.add(73 * time.Hour)

	path := file(r.Dir).Path()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := LockChecks(context.Background(), r.Dir, false)
	if err != nil {
		t.Fatal(err)
	}
	r.opts.SlackWatch = true
	runs := len(r.slack.runs)
	r.log.Reset()
	r.tick()
	r.tick()
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Errorf("the state changed while a fetch ran:\n%s\nwas:\n%s", after, before)
	}
	if len(r.slack.runs) != runs {
		t.Errorf("ran %q while a fetch ran", r.slack.runs[runs:])
	}
	if got := r.log.String(); got != "a fetch is running\n" {
		t.Errorf("log = %q, want the fetch named once", got)
	}

	unlock()
	r.tick()
	if st := r.state(t); !st.On || !st.Since.Equal(r.clock.now()) {
		t.Errorf("state = %+v, want it on, starting fresh", st)
	}
	if got := r.slack.runs[len(r.slack.runs)-1]; got != IdentifyPrompt {
		t.Errorf("turning on ran %q, want the identify run", got)
	}
}

// TestTickSkipsWhileAFetchRuns: a tick that's due while a fetch holds the
// lock runs nothing and changes nothing, and checks once the fetch is done.
func TestTickSkipsWhileAFetchRuns(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	r.slack.mentions = []Message{billing("U0DANA001", tsAt(watchStart.Add(time.Minute)), "", "<@U0ME00001> can the invoice fix ship Friday?")}
	r.clock.add(5 * time.Minute)
	path := file(r.Dir).Path()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := LockChecks(context.Background(), r.Dir, false)
	if err != nil {
		t.Fatal(err)
	}
	r.tick()
	r.tick()
	if len(r.slack.runs) != 1 || r.tabs() != 0 {
		t.Errorf("runs %q, %d tabs; want nothing run while a fetch runs", r.slack.runs[1:], r.tabs())
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Errorf("the state changed:\n%s\nwas:\n%s", after, before)
	}
	if n := strings.Count(r.log.String(), "a fetch is running\n"); n != 1 {
		t.Errorf("log = %q, want the fetch named once", r.log.String())
	}

	unlock()
	r.tick()
	if len(r.slack.runs) != 2 || r.tabs() != 1 {
		t.Errorf("after the fetch: runs %d, %d tabs; want the check", len(r.slack.runs), r.tabs())
	}
}

func TestTickDeliveryFailureRetriesNextTime(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	var asks []Message
	for i, who := range []string{"U0DANA001", "U0RUI0001", "U0DANA001"} {
		at := watchStart.Add(time.Duration(i+1) * time.Minute)
		asks = append(asks, billing(who, tsAt(at), "", fmt.Sprintf("<@U0ME00001> question %d", i+1)))
	}
	r.slack.mentions = asks
	// The first ask's tab comes with its workspace; the second's fails.
	r.herdr.fail = func(call string) error {
		if strings.HasPrefix(call, "CreateTab ") {
			return errors.New("herdr: busy")
		}
		return nil
	}
	r.clock.add(5 * time.Minute)
	failed := r.clock.now()
	r.tick()
	st := r.state(t)
	if r.tabs() != 1 {
		t.Fatalf("%d tabs, want 1", r.tabs())
	}
	if _, ok := st.Seen[asks[0].ID()]; !ok {
		t.Errorf("the first ask isn't seen: %v", st.Seen)
	}
	for _, m := range asks[1:] {
		if _, ok := st.Seen[m.ID()]; ok {
			t.Errorf("%s is seen, but has no task", m.ID())
		}
	}
	if !st.Since.Before(atTS(asks[1].TS)) || st.Since.Before(atTS(asks[0].TS)) {
		t.Errorf("since = %v, want just before the second ask, %v", st.Since, atTS(asks[1].TS))
	}
	want := Status{On: true, Problem: true, Fetch: true, Text: "Slack: couldn't make a task for an ask in #billing: herdr: busy since 14:05"}
	if got := r.Status(); got != want {
		t.Errorf("status = %+v\nwant     %+v", got, want)
	}
	if st.NextTry == nil || !st.NextTry.Equal(failed.Add(5*time.Minute)) {
		t.Errorf("next try = %v", st.NextTry)
	}

	r.herdr.mu.Lock()
	r.herdr.fail = nil
	r.herdr.mu.Unlock()
	r.clock.t = *st.NextTry
	r.tick()
	if r.tabs() != 3 {
		t.Fatalf("%d tabs, want 3; status %+v", r.tabs(), r.Status())
	}
	st = r.state(t)
	for _, m := range asks {
		if _, ok := st.Seen[m.ID()]; !ok {
			t.Errorf("%s isn't seen", m.ID())
		}
	}
	if st.Problem != "" || r.Status().Problem {
		t.Errorf("problem = %q, status %+v", st.Problem, r.Status())
	}
}

func TestTickChecksTheResearchFolder(t *testing.T) {
	r := newRig(t)
	r.opts.SlackCwd = filepath.Join(r.opts.SlackCwd, "gone")
	r.tick()
	want := Status{On: true, Problem: true, Fetch: true, Text: "Slack: slack_cwd " + r.opts.SlackCwd + " isn't a folder"}
	if got := r.Status(); got != want || len(r.slack.runs) != 0 {
		t.Errorf("status = %+v, runs %q", got, r.slack.runs)
	}
}

func TestTickShowsSettingsTrouble(t *testing.T) {
	r := newRig(t)
	r.opts.SlackEvery = "30s"
	r.tick()
	if got := r.Status(); !got.On || !got.Problem || got.Text != `Slack: slack_every must be at least 1m, got "30s"` {
		t.Errorf("status = %+v", got)
	}

	r.Options = func() (reconcile.Options, error) {
		return reconcile.Options{}, errors.New("config.toml: line 3: expected '='")
	}
	r.tick()
	if got := r.Status(); !got.Problem || got.Text != "Slack: reading config: config.toml: line 3: expected '='" {
		t.Errorf("status = %+v", got)
	}
	if len(r.slack.runs) != 0 {
		t.Errorf("Claude ran: %q", r.slack.runs)
	}
}

// A config.toml that doesn't read is in the status. It goes to slack.log
// only when the watcher was on: one that was never on has no log to keep.
func TestTickLogsAConfigErrorOnlyWhenOn(t *testing.T) {
	r := newRig(t)
	options := r.Options
	r.Options = func() (reconcile.Options, error) {
		return reconcile.Options{}, errors.New("config.toml: line 3: expected '='")
	}
	r.tick()
	if got := r.Status(); got.On || !got.Problem || got.Text != "Slack: reading config: config.toml: line 3: expected '='" {
		t.Errorf("status = %+v", got)
	}
	if r.log.Len() != 0 {
		t.Errorf("logged %q while off", r.log.String())
	}

	r.Options = options
	r.tick()
	r.log.Reset()
	r.Options = func() (reconcile.Options, error) {
		return reconcile.Options{}, errors.New("config.toml: line 3: expected '='")
	}
	r.tick()
	if got := r.log.String(); got != "reading config: config.toml: line 3: expected '='\n" {
		t.Errorf("logged %q while on, want the config error", got)
	}
}

func TestOpenLogMovesAFullLogAside(t *testing.T) {
	dir := t.TempDir()
	l := OpenLog(dir)
	if _, err := os.Stat(filepath.Join(dir, "slack.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenLog made the file before a line: %v", err)
	}
	l.Print("first")
	path := filepath.Join(dir, "slack.log")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}

	// A log that a line would take over 1 MiB goes to slack.log.1.
	full := bytes.Repeat([]byte("x"), 1<<20-10)
	if err := os.WriteFile(path, full, 0o600); err != nil {
		t.Fatal(err)
	}
	l = OpenLog(dir)
	l.Print("a line longer than ten bytes")
	old, err := os.ReadFile(path + ".1")
	if err != nil || !bytes.Equal(old, full) {
		t.Fatalf("slack.log.1: %d bytes, %v", len(old), err)
	}
	now, err := os.ReadFile(path)
	if err != nil || !strings.HasSuffix(string(now), "a line longer than ten bytes\n") || len(now) > 100 {
		t.Errorf("slack.log = %q, %v", now, err)
	}
}

func TestRunningSessions(t *testing.T) {
	// t.TempDir paths can pass the unix socket path limit on macOS.
	home, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("HERDR_SOCKET_PATH", "")
	state := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", state)
	for _, name := range []string{"work", "gone"} {
		sock := filepath.Join(home, ".config", "herdr", "sessions", name, "herdr.sock")
		if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		if name == "gone" {
			// A socket a herdr that ended left behind.
			ln.(*net.UnixListener).SetUnlinkOnClose(false)
			ln.Close()
			continue
		}
		t.Cleanup(func() { ln.Close() })
	}

	got := RunningSessions()
	if len(got) != 1 || got[0].Name != "work" || got[0].Store.Session != "work" || got[0].Store.Dir != state || got[0].Herdr == nil {
		t.Fatalf("sessions = %+v", got)
	}
}

func TestWatcherLearnsChannelNames(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	r.slack.mentions = []Message{billing("U0DANA001", tsAt(watchStart.Add(time.Minute)), "", "<@U0ME00001> can the invoice fix ship Friday?")}
	r.slack.dms = []Message{dm("U0RUI0001", tsAt(watchStart.Add(2*time.Minute)), "quick q: is ABC-12 merged?")}
	r.slack.groups = []Message{group("U0DANA001", tsAt(watchStart.Add(3*time.Minute)), "lunch?")}
	r.slack.own = []Message{msg("C0OPS0001", "#ops", me, tsAt(watchStart.Add(4*time.Minute)), "", "deploying now")}

	// A check whose searches found the messages, but didn't search as asked,
	// failed, and learns nothing.
	r.Watcher.Claude = func(ctx context.Context, dir string, args []string, stdin string) ([]byte, error) {
		args = slices.Clone(args)
		args[1] = startsAt.ReplaceAllStringFunc(args[1], func(s string) string {
			after, _ := strconv.ParseInt(startsAt.FindStringSubmatch(s)[1], 10, 64)
			return fmt.Sprintf("uses after %d,", after+1)
		})
		return r.slack.run(ctx, dir, args, stdin)
	}
	r.clock.add(5 * time.Minute)
	r.tick()
	if st := r.state(t); !strings.Contains(st.Problem, "didn't search as asked") {
		t.Fatalf("problem = %q, want a check that didn't search as asked", st.Problem)
	}
	if got := channels(t, r.Dir); len(got) != 0 {
		t.Errorf("a failed check learned %v", got)
	}

	r.Watcher.Claude = r.slack.run
	r.clock.t = *r.state(t).NextTry
	r.tick()
	if st := r.state(t); st.Problem != "" {
		t.Fatalf("the check failed: %s", st.Problem)
	}
	want := map[string]string{"C0123ABCD": "#billing", "C0OPS0001": "#ops"}
	if got := channels(t, r.Dir); !maps.Equal(got, want) {
		t.Errorf("Channels = %v, want %v: channels only, never DMs or group DMs", got, want)
	}
}

// TestTickRefusesAClaudeTooOldToLock: before 2.1.295, Claude Code lets a
// tool call through when the guard's hook fails, so the watcher doesn't run
// a check with it, and says why.
func TestTickRefusesAClaudeTooOldToLock(t *testing.T) {
	r := newRig(t)
	r.Claude = nil // the claude on PATH
	runs := filepath.Join(t.TempDir(), "runs")
	bin := fakeClaude(t, `echo "$@" >> '`+runs+`'; echo '2.1.200 (Claude Code)'`)
	t.Setenv("PATH", filepath.Dir(bin))
	r.tick()
	want := "Claude Code 2.1.295 or later is needed for the read-only lock; this one is 2.1.200"
	st := r.state(t)
	if st.Problem != want || st.UserID != "" {
		t.Errorf("problem %q, user %q; want %q and no check", st.Problem, st.UserID, want)
	}
	if s := r.Status(); !s.Problem || !strings.HasPrefix(s.Text, "Slack: "+want) {
		t.Errorf("status = %+v, want the problem", s)
	}
	if !strings.Contains(r.log.String(), want) {
		t.Errorf("log = %q, want the problem", r.log.String())
	}
	if b, _ := os.ReadFile(runs); string(b) != "--version\n" {
		t.Errorf("claude ran with %q, want only --version", b)
	}
}

// TestTickSkipsASecondWithMoreThanAPage: twenty messages in one second fill
// a page that every later check would get again. The watcher skips the
// rest of that second, says so, and goes on to the asks after it.
func TestTickSkipsASecondWithMoreThanAPage(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	busy := watchStart.Add(time.Minute) // 14:01:00
	for i := range 20 {
		r.slack.mentions = append(r.slack.mentions,
			billing("U0DANA001", fmt.Sprintf("%d.%06d", busy.Unix(), 100+i), "", fmt.Sprintf("<@U0ME00001> ask %d", i)))
	}

	// Check 1 delivers the twenty, up to the page's end.
	r.clock.add(5 * time.Minute)
	r.tick()
	end := atTS(r.slack.mentions[19].TS)
	if st := r.state(t); r.tabs() != 20 || !st.Since.Equal(end) {
		t.Fatalf("check 1: %d tabs, since %v; want 20 and %v; log:\n%s", r.tabs(), st.Since, end, r.log.String())
	}

	// Check 2 starts in that second and gets the same page: it skips on.
	late := dm("U0RUI0001", tsAt(watchStart.Add(7*time.Minute)), "are you around?")
	r.slack.dms = []Message{late}
	r.clock.add(5 * time.Minute)
	r.tick()
	if st := r.state(t); !st.Since.Equal(busy.Add(time.Second)) || st.Problem != "" {
		t.Fatalf("check 2: since %v, problem %q; want %v", st.Since, st.Problem, busy.Add(time.Second))
	}
	if !strings.Contains(r.log.String(), "more than 20 messages at 14:01:00; skipping the rest of that second") {
		t.Errorf("log = %q, want the skipped second", r.log.String())
	}

	// Check 3 starts after it, and delivers the DM.
	r.clock.add(5 * time.Minute)
	r.tick()
	if r.tabs() != 21 {
		t.Errorf("check 3: %d tabs, want 21; log:\n%s", r.tabs(), r.log.String())
	}
	if !strings.Contains(r.slack.runs[len(r.slack.runs)-1], fmt.Sprintf("after %d,", busy.Unix()+1)) {
		t.Errorf("check 3 didn't start at the next second: %q", r.slack.runs[len(r.slack.runs)-1])
	}
}

// TestTickRefusesADifferentSearch: a check whose searches aren't the ones
// asked for, or that has a search without its answer, failed, and the
// watermark stays.
func TestTickRefusesADifferentSearch(t *testing.T) {
	for _, c := range []struct {
		name    string
		rig     func(*fakeSlack)
		problem string
	}{
		{"an end", func(s *fakeSlack) { s.extra = map[string]any{"before": watchStart.Unix()} }, "the check ran a different search than asked: before"},
		{"a filter", func(s *fakeSlack) { s.extra = map[string]any{"filters": "in:#billing"} }, "the check ran a different search than asked: filters"},
		{"no answer", func(s *fakeSlack) { s.unanswered = true }, "the search for mentions of you failed: the search got no answer"},
	} {
		r := newRig(t)
		r.identified(t)
		r.slack.mentions = []Message{billing("U0DANA001", tsAt(watchStart.Add(time.Minute)), "", "<@U0ME00001> can the invoice fix ship Friday?")}
		c.rig(r.slack)
		r.clock.add(5 * time.Minute)
		r.tick()
		st := r.state(t)
		if st.Problem != c.problem || !st.Since.Equal(watchStart) || r.tabs() != 0 {
			t.Errorf("%s: problem %q, since %v, %d tabs; want %q, since %v, no tabs", c.name, st.Problem, st.Since, r.tabs(), c.problem, watchStart)
		}
	}
}

// TestTickLogsTextShapedLikeHeaders: a message with a line like a result's
// header is still an ask, and slack.log names its permalink, not its text.
func TestTickLogsTextShapedLikeHeaders(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	dana := billing("U0DANA001", tsAt(watchStart.Add(time.Minute)), "", "<@U0ME00001> secret plans\n### Result 1 of 1")
	r.slack.mentions = []Message{dana}
	r.clock.add(5 * time.Minute)
	r.tick()
	if r.tabs() != 1 {
		t.Fatalf("%d tabs, want 1; log:\n%s", r.tabs(), r.log.String())
	}
	logged := r.log.String()
	if !strings.Contains(logged, "a message contains text shaped like Slack's result headers: "+dana.Permalink+"\n") {
		t.Errorf("log = %q, want the warning", logged)
	}
	// Once per check: which asks it may have missed.
	missed := regexp.MustCompile(`(?m)^a message on this page imitates Slack's result layout; asks since \d\d:\d\d:\d\d may have been skipped: ` + regexp.QuoteMeta(dana.Permalink) + `$`)
	if n := len(missed.FindAllString(logged, -1)); n != 1 || strings.Count(logged, "imitates") != 1 {
		t.Errorf("log = %q, want the page's line once", logged)
	}
	if strings.Contains(logged, "secret") {
		t.Errorf("the log has the message's text: %q", logged)
	}
}

// TestTickLogsResearchThatDidntStart: slack.log says when an ask's research
// didn't start, rather than that it did.
func TestTickLogsResearchThatDidntStart(t *testing.T) {
	fastRetries(t)
	r := newRig(t)
	r.identified(t)
	r.slack.mentions = []Message{billing("U0DANA001", tsAt(watchStart.Add(time.Minute)), "", "<@U0ME00001> can the invoice fix ship Friday?")}
	busy := errors.New("herdr: pane_busy: the pane isn't at a shell prompt")
	r.herdr.startErrs = []error{busy, busy, busy, busy, busy}
	r.clock.add(5 * time.Minute)
	r.tick()
	logged := r.log.String()
	if !strings.Contains(logged, ": new tab w2:t1, research didn't start: "+busy.Error()+"\n") || strings.Contains(logged, "with research") {
		t.Errorf("log = %q, want research didn't start", logged)
	}
}

// TestWatcherKeepsOffTopicAsks: with slack_topic, each new ask gets its own
// topic check, with the background. One that's off-topic gets no tab: it's
// seen, and kept on the skipped list.
func TestWatcherKeepsOffTopicAsks(t *testing.T) {
	r := newRig(t)
	r.opts.SlackTopic = billingTopic
	if err := os.WriteFile(filepath.Join(r.opts.SlackCwd, "README.md"), []byte("# acme billing\nInvoices and refunds.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.identified(t)
	dana := billing("U0DANA001", tsAt(watchStart.Add(time.Minute)), "", "<@U0ME00001> can the invoice fix ship Friday?")
	rui := dm("U0RUI0001", tsAt(watchStart.Add(2*time.Minute)), "lunch on Friday?")
	r.slack.mentions, r.slack.dms = []Message{dana}, []Message{rui}
	r.slack.verdicts = []verdict{{relevant: true, why: "the invoice fix"}, {relevant: false, why: "lunch plans"}}
	r.clock.add(5 * time.Minute)
	got := r.fetch(t)

	if r.tabs() != 1 || len(r.herdr.callsTo("StartAgent")) != 1 {
		t.Fatalf("%d tabs, %d research sessions; want one each; log:\n%s", r.tabs(), len(r.herdr.callsTo("StartAgent")), r.log.String())
	}
	if len(r.slack.judged) != 2 {
		t.Fatalf("%d topic checks, want one per ask", len(r.slack.judged))
	}
	for i, m := range []Message{dana, rui} {
		p := r.slack.judged[i]
		other := []Message{rui, dana}[i]
		if !strings.Contains(p, "Topic: "+billingTopic) || !strings.Contains(p, "Invoices and refunds.") ||
			!strings.Contains(p, "> "+m.Text) || strings.Contains(p, other.Text) {
			t.Errorf("topic check %d: want the topic, the background and only its own message:\n%s", i+1, p)
		}
	}
	// The prompt is on stdin: no command line has the message, which other
	// users could read.
	for _, args := range r.slack.args {
		line := strings.Join(args, " ")
		for _, m := range []Message{dana, rui} {
			if strings.Contains(line, m.Text) || strings.Contains(line, billingTopic) {
				t.Errorf("a run's arguments have the message or the topic: %q", args)
			}
		}
	}
	st := r.state(t)
	if len(st.Skipped) != 1 {
		t.Fatalf("skipped = %+v, want Rui's DM", st.Skipped)
	}
	if k := st.Skipped[0]; k.Ask.ID() != rui.ID() || k.Why != "lunch plans" || !k.At.Equal(r.clock.now()) || k.Ask.Text != rui.Text || k.Ask.Kind != KindDM {
		t.Errorf("skipped = %+v", k)
	}
	for _, m := range []Message{dana, rui} {
		if at, ok := st.Seen[m.ID()]; !ok || !at.Equal(atTS(m.TS)) {
			t.Errorf("%s isn't seen", m.ID())
		}
	}
	if len(st.Starts) != 1 {
		t.Errorf("starts = %v, want Dana's only", st.Starts)
	}
	if want := 0.0079 + 2*judgeCost; math.Abs(st.LastCost-want) > 1e-9 || math.Abs(got.Cost-want) > 1e-9 {
		t.Errorf("last cost %v, fetched %v; want the check's and the topic checks', %v", st.LastCost, got.Cost, want)
	}
	if got.New != 1 || got.Research != 1 || got.OffTopic != 1 {
		t.Errorf("Fetched = %+v, want 1 new, 1 off-topic", got)
	}
	logged := r.log.String()
	if !strings.Contains(logged, "\nD0RUI0001: off-topic (lunch plans)\n") {
		t.Errorf("log = %q, want the off-topic ask", logged)
	}
	if strings.Contains(logged, "lunch on") || strings.Contains(logged, "invoice fix ship") {
		t.Errorf("the log has message text: %q", logged)
	}
	want := "Fetched from 13:50: 1 new task, 1 with research; 1 off-topic (herdr-tasks slack skipped)."
	if s := got.Summary(r.clock.now()); s != want {
		t.Errorf("summary = %q\nwant      %q", s, want)
	}

	// Next time, neither is checked again.
	r.clock.add(5 * time.Minute)
	r.tick()
	if len(r.slack.judged) != 2 || r.tabs() != 1 {
		t.Errorf("again: %d topic checks, %d tabs", len(r.slack.judged), r.tabs())
	}
}

// TestSkippedListSaysWhatItDrops: an ask that falls off the skipped list,
// past 50 entries or 7 days, is logged: it's seen, so it won't come back.
func TestSkippedListSaysWhatItDrops(t *testing.T) {
	r := newRig(t)
	r.opts.SlackTopic = billingTopic
	r.identified(t)
	var kept []Ask
	if err := Update(r.Dir, func(s *State) error {
		for i := range 50 {
			a := askOf(billing("U0DANA001", fmt.Sprintf("17930%05d.000100", i), "", "<@U0ME00001> lunch?"), "lunch?")
			kept = append(kept, a)
			s.Skip(a, Skip{Why: "lunch plans"}, watchStart.Add(-6*24*time.Hour+time.Duration(i)*time.Minute))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A 51st pushes the oldest off.
	lunch := dm("U0RUI0001", tsAt(watchStart.Add(time.Minute)), "lunch on Friday?")
	r.slack.dms = []Message{lunch}
	r.slack.verdicts = []verdict{{why: "lunch plans"}}
	r.clock.add(5 * time.Minute)
	r.log.Reset()
	r.tick()
	if st := r.state(t); len(st.Skipped) != 50 || st.Skipped[0].Ask.ID() != kept[1].ID() {
		t.Fatalf("%d skipped, from %s; want 50, the oldest gone", len(st.Skipped), st.Skipped[0].Ask.ID())
	}
	want := kept[0].Key + ": dropped from the skipped list (over 50)\n"
	if logged := r.log.String(); strings.Count(logged, "dropped from the skipped list") != 1 || !strings.Contains(logged, want) {
		t.Errorf("log = %q, want %q", logged, want)
	}

	// A day on, the check drops those skipped over 7 days ago, and says so.
	r.clock.add(24*time.Hour + 10*time.Minute)
	r.log.Reset()
	r.tick()
	logged := r.log.String()
	if n := strings.Count(logged, ": dropped from the skipped list (over 7 days old)\n"); n != 14 {
		t.Errorf("logged %d dropped as over 7 days old, want 14:\n%s", n, logged)
	}
	if !strings.Contains(logged, kept[1].Key+": dropped from the skipped list (over 7 days old)\n") {
		t.Errorf("log = %q, want the oldest named", logged)
	}
	if st := r.state(t); len(st.Skipped) != 36 {
		t.Errorf("%d skipped, want 36", len(st.Skipped))
	}
}

// TestJudgeFailureDelivers: a topic check that fails lets the ask through,
// and slack.log says the topic wasn't checked.
func TestJudgeFailureDelivers(t *testing.T) {
	r := newRig(t)
	r.opts.SlackTopic = billingTopic
	r.identified(t)
	dana := billing("U0DANA001", tsAt(watchStart.Add(time.Minute)), "", "<@U0ME00001> lunch on Friday?")
	r.slack.mentions = []Message{dana}
	r.slack.verdicts = []verdict{{fails: true}}
	r.clock.add(5 * time.Minute)
	r.tick()
	if r.tabs() != 1 || len(r.slack.judged) != 1 {
		t.Fatalf("%d tabs after %d topic checks, want the ask's tab; log:\n%s", r.tabs(), len(r.slack.judged), r.log.String())
	}
	want := dana.ID() + ": couldn't check the topic: Claude couldn't run the topic check: Claude AI usage limit reached; delivering it\n"
	if logged := r.log.String(); !strings.Contains(logged, want) {
		t.Errorf("log = %q\nwant %q", logged, want)
	}
	if st := r.state(t); len(st.Skipped) != 0 || st.Problem != "" || st.Failures != 0 {
		t.Errorf("state = %+v, want nothing skipped, and no problem", st)
	}
}

// TestNoTopicNoJudge: with no slack_topic, no ask is checked for it.
func TestNoTopicNoJudge(t *testing.T) {
	r := newRig(t)
	r.identified(t)
	r.slack.mentions = []Message{billing("U0DANA001", tsAt(watchStart.Add(time.Minute)), "", "<@U0ME00001> lunch on Friday?")}
	r.slack.dms = []Message{dm("U0RUI0001", tsAt(watchStart.Add(2*time.Minute)), "lunch?")}
	r.clock.add(5 * time.Minute)
	r.tick()
	if r.tabs() != 2 {
		t.Fatalf("%d tabs, want 2", r.tabs())
	}
	if len(r.slack.judged) != 0 || len(r.slack.args) != 2 {
		t.Errorf("%d topic checks in %d runs, want none", len(r.slack.judged), len(r.slack.args))
	}
	for _, args := range r.slack.args {
		if slices.Contains(args, "--strict-mcp-config") {
			t.Errorf("a run with --strict-mcp-config: %q", args)
		}
	}
	if strings.Contains(r.log.String(), "topic") {
		t.Errorf("log = %q", r.log.String())
	}
}

// TestJudgeStoppedDeliversNothing: a topic check cut short because the
// check itself was stopped doesn't fail open. Its ask gets no tab and isn't
// seen or skipped, the delivery stops there, and the watermark stays, so
// the next check takes the ask again.
func TestJudgeStoppedDeliversNothing(t *testing.T) {
	r := newRig(t)
	r.opts.SlackTopic = billingTopic
	r.identified(t)
	dana := billing("U0DANA001", tsAt(watchStart.Add(time.Minute)), "", "<@U0ME00001> can the invoice fix ship Friday?")
	rui := billing("U0RUI0001", tsAt(watchStart.Add(2*time.Minute)), "", "<@U0ME00001> and the refund?")
	r.slack.mentions = []Message{dana, rui}
	ctx, cancel := context.WithCancel(context.Background())
	run := r.Watcher.Claude
	judged := 0
	r.Watcher.Claude = func(c context.Context, dir string, args []string, stdin string) ([]byte, error) {
		if slices.Contains(args, "--strict-mcp-config") {
			if judged++; judged == 2 {
				// Rui's topic check blocks until the fetch is stopped.
				time.AfterFunc(50*time.Millisecond, cancel)
				<-c.Done()
				return nil, errors.New("signal: killed")
			}
		}
		return run(c, dir, args, stdin)
	}
	r.clock.add(5 * time.Minute)
	if _, err := r.Fetch(ctx); err == nil || err.Error() != "the fetch stopped: context canceled" {
		t.Fatalf("Fetch = %v, want it stopped", err)
	}
	st := r.state(t)
	if r.tabs() != 1 {
		t.Errorf("%d tabs, want Dana's only; log:\n%s", r.tabs(), r.log.String())
	}
	if _, ok := st.Seen[dana.ID()]; !ok {
		t.Errorf("Dana's ask isn't seen")
	}
	if _, ok := st.Seen[rui.ID()]; ok || len(st.Skipped) != 0 {
		t.Errorf("Rui's ask: seen %v, skipped %+v; want neither", ok, st.Skipped)
	}
	if !st.Since.Equal(watchStart) || !st.Since.Before(atTS(rui.TS)) || st.Problem != "" || st.Failures != 0 {
		t.Errorf("since %v, problem %q, failures %d; want the watermark where it was, and no failure", st.Since, st.Problem, st.Failures)
	}
	logged := r.log.String()
	if strings.Contains(logged, "couldn't check the topic") || strings.Contains(logged, rui.ID()+": ") {
		t.Errorf("log = %q, want nothing delivered for Rui's ask", logged)
	}
	if !strings.HasSuffix(logged, "the fetch stopped: context canceled\n") {
		t.Errorf("log = %q, want the stop last", logged)
	}

	// The next check takes it again.
	r.Watcher.Claude = run
	r.clock.add(time.Minute)
	if got := r.fetch(t); got.New != 1 || r.tabs() != 2 {
		t.Errorf("again: Fetched %+v, %d tabs; want Rui's ask", got, r.tabs())
	}
	if _, ok := r.state(t).Seen[rui.ID()]; !ok {
		t.Errorf("Rui's ask isn't seen after the next check")
	}
}

func TestSessionForWaitsForTheNamedSession(t *testing.T) {
	work, side, def := Session{Name: "work"}, Session{Name: "side"}, Session{Name: "default"}
	got, why := sessionFor([]Session{work, def}, "default")
	if got.Name != "default" || why != "" {
		t.Errorf("got %q, %q", got.Name, why)
	}
	if _, why := sessionFor([]Session{work, side}, "default"); why != "waiting for herdr session default. Set slack_session in config.toml to use another" {
		t.Errorf("why = %q", why)
	}
	if _, why := sessionFor(nil, "work"); why != "waiting for herdr session work. Set slack_session in config.toml to use another" {
		t.Errorf("why = %q", why)
	}
	// It says how to pick another session.
	if WaitingFor("work") != "waiting for herdr session work. Set slack_session in config.toml to use another" {
		t.Errorf("WaitingFor = %q", WaitingFor("work"))
	}
}
