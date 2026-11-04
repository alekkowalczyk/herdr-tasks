package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alekkowalczyk/herdr-tasks/internal/assistant"
	"github.com/alekkowalczyk/herdr-tasks/internal/board"
	"github.com/alekkowalczyk/herdr-tasks/internal/claudelog"
	"github.com/alekkowalczyk/herdr-tasks/internal/herdr"
	"github.com/alekkowalczyk/herdr-tasks/internal/reconcile"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// errAssistantTab is why a command that changes a task doesn't change the
// assistant's tab, which is where it runs when the assistant forgets --tab.
var errAssistantTab = errors.New("this is the assistant's tab, not a task; pass --tab with the task's tab ID")

// notAssistant is fn, but for the assistant's tab, which it leaves alone.
func notAssistant(fn func(*task.Task) error) func(*task.Task) error {
	return func(t *task.Task) error {
		if t.Assistant != nil {
			return errAssistantTab
		}
		return fn(t)
	}
}

// assistantCmd runs `herdr-tasks assistant`: alone, it shows the assistant's
// tab, or from there the tab you came from; ask opens it with the target
// tab's task typed in; hook gives the assistant's Claude the tasks.
func (a *app) assistantCmd() error {
	sub := ""
	if len(a.flags.args) > 0 {
		sub = a.flags.args[0]
	}
	if sub == "hook" {
		dir := a.store.Dir
		quietly(dir, func() error { return a.assistantHook(os.Stdin, os.Stdout) })
		return nil
	}
	cfg, err := assistant.ConfigFor(a.opts, a.store.Dir)
	if err != nil {
		return err
	}
	switch sub {
	case "":
		_, err := assistant.Toggle(a.host(), a.store, a.opts, cfg, a.fromTab(), time.Now())
		return err
	case "ask":
		id, err := a.targetTab()
		if err != nil {
			return err
		}
		return assistant.Show(a.host(), a.store, a.opts, cfg, id, a.fromTab(), time.Now())
	}
	return fmt.Errorf("unknown assistant command %q; use ask, or nothing", sub)
}

// fromTab is the tab you were in: the one herdr's plugin action names, else
// the target tab, or "" when there's none.
func (a *app) fromTab() string {
	var ctx struct {
		TabID string `json:"tab_id"`
	}
	if json.Unmarshal([]byte(os.Getenv("HERDR_PLUGIN_CONTEXT_JSON")), &ctx) == nil && ctx.TabID != "" {
		return ctx.TabID
	}
	id, _ := a.targetTab()
	return id
}

// assistantHook prints the tasks for Claude, which adds what a
// UserPromptSubmit hook prints to your message. It reads what Claude sends
// first, so Claude isn't left writing to a closed pipe, and keeps the
// session Claude says it runs as the one to resume.
func (a *app) assistantHook(in io.Reader, w io.Writer) error {
	b, _ := io.ReadAll(io.LimitReader(in, maxHookInput))
	_, _ = io.Copy(io.Discard, in)
	var ev struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(b, &ev)
	live, err := a.client.Live()
	if err != nil {
		return err
	}
	if err := assistant.KeepSession(a.store, live, ev.SessionID); err != nil {
		return err
	}
	s, err := a.store.Load()
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, assistant.Brief(s, live, time.Now()))
	return err
}

// contextCmd prints every task, or the --tab one, with what the board knows
// of it: its note, log and links, and its agents' status, recap, and last
// prompt and reply. --json prints the board's cards.
func (a *app) contextCmd(w io.Writer) error {
	now := time.Now()
	// What couldn't be synced, as a strip that couldn't be added to a tab,
	// doesn't keep the tasks from being read.
	live, err := reconcile.Run(a.host(), a.store, a.opts, now, nil)
	if live == nil {
		return err
	}
	s, err := a.store.Load()
	if err != nil {
		return err
	}
	panes, err := a.client.Panes()
	if err != nil {
		return err
	}
	logs := claudelog.NewCache(claudelog.ConfigDir())
	summary := func(p herdr.Pane) (claudelog.Summary, bool) {
		sum, ok, err := logs.Get(p.Session.Value)
		return sum, ok && err == nil
	}
	b := board.Build([]board.Snapshot{{Name: a.store.Session, State: s, Live: live, Panes: panes}}, summary, nil, false, version)
	cards := b.Sessions[0].Cards
	if a.flags.tab != "" {
		var one []board.Card
		for _, c := range cards {
			if c.Tab == a.flags.tab {
				one = append(one, c)
			}
		}
		if len(one) == 0 {
			return fmt.Errorf("no task in tab %s: it isn't there, it's a tool, or its space is hidden", a.flags.tab)
		}
		if a.flags.json {
			return printJSON(one[0])
		}
		writeCard(w, one[0], true, now)
		return nil
	}
	if a.flags.json {
		return printJSON(cards)
	}
	writeContext(w, a.store.Session, cards, now)
	return nil
}

// Over every task, context cuts an agent's last prompt and reply, and keeps
// the last lines of a log.
const (
	contextText = 300
	contextLog  = 5
)

// columnNames say where a card stands.
var columnNames = map[string]string{
	"asking": "asking", "yourturn": "your turn", "running": "working",
	"review": "in review", "paused": "paused", "parked": "parked", "done": "done",
}

func writeContext(w io.Writer, session string, cards []board.Card, now time.Time) {
	tasks := fmt.Sprintf("%d tasks", len(cards))
	if len(cards) == 1 {
		tasks = "1 task"
	}
	fmt.Fprintf(w, "%s in herdr session %s, %s. `herdr-tasks context --tab ID` has all of one.\n", tasks, task.CleanLabel(session), now.Format("Mon Jan 2 15:04"))
	for _, c := range cards {
		fmt.Fprintln(w)
		writeCard(w, c, false, now)
	}
}

// writeCard prints a card. Agents and other people wrote much of it, so all
// of it is cleaned of control characters. Unless whole, the log keeps its
// last lines, and what agents said is cut short.
func writeCard(w io.Writer, c board.Card, whole bool, now time.Time) {
	head := []string{columnNames[c.Column]}
	if c.Priority {
		head = append(head, "priority")
	}
	head = append(head, task.Age(now.Sub(c.Since)))
	where := c.Tab
	if l := task.CleanLabel(c.SpaceLabel); l != "" {
		where += ", " + l
	}
	fmt.Fprintf(w, "%s (%s): %s\n", task.CleanLabel(c.Title), where, strings.Join(head, " · "))
	if r := task.CleanLabel(c.Reason); r != "" {
		label := "Until"
		if c.Stage == string(task.Review) {
			label = "Waiting for"
		}
		fmt.Fprintf(w, "  %s: %s\n", label, r)
	}
	if note := strings.TrimSpace(task.CleanText(c.Note)); note != "" {
		fmt.Fprintln(w, "  Note:")
		for _, l := range strings.Split(note, "\n") {
			fmt.Fprintln(w, "    "+l)
		}
	}
	if log := c.Log; len(log) > 0 {
		fmt.Fprintln(w, "  Log:")
		if !whole && len(log) > contextLog {
			fmt.Fprintf(w, "    (%d earlier lines)\n", len(log)-contextLog)
			log = log[len(log)-contextLog:]
		}
		for _, e := range log {
			fmt.Fprintf(w, "    %s %s: %s\n", task.CleanLabel(e.Who), e.At.Local().Format("Jan 2 15:04"), oneLine(e.Text, 0))
		}
	}
	if len(c.Links) > 0 {
		fmt.Fprintln(w, "  Links:")
		for _, l := range c.Links {
			line := task.CleanLabel(l.Label) + "  " + l.URL
			if !task.SafeURL(l.URL) {
				line = task.CleanLabel(l.Label) + "  (unsafe link)"
			}
			fmt.Fprintln(w, "    - "+line)
		}
	}
	if len(c.Agents) == 0 {
		fmt.Fprintln(w, "  No agent runs in its tab.")
	}
	cut := contextText
	if whole {
		cut = 0
	}
	for _, ag := range c.Agents {
		name := task.CleanLabel(ag.Agent)
		if t := task.CleanLabel(ag.Title); t != "" {
			name += " " + fmt.Sprintf("%q", t)
		}
		status := task.CleanLabel(ag.Status)
		if status == "" {
			status = "unknown"
		}
		fmt.Fprintf(w, "  Agent %s in pane %s: %s\n", name, ag.Pane, status)
		for _, f := range []struct{ label, text string }{{"Recap", ag.Recap}, {"Last prompt", ag.LastPrompt}, {"Last reply", ag.LastReply}} {
			if s := oneLine(f.text, cut); s != "" {
				fmt.Fprintf(w, "    %s: %s\n", f.label, s)
			}
		}
	}
}

// oneLine is s cleaned, on one line, and cut to n runes when n > 0.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(task.CleanText(s)), " ")
	if n > 0 && utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n-1]) + "…"
	}
	return s
}
