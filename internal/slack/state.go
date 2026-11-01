package slack

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

const (
	seenFor    = 96 * time.Hour
	startsFor  = time.Hour
	skippedFor = 7 * 24 * time.Hour
	maxSkipped = 50
)

// Mute is a channel you muted from a card.
type Mute struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// State is what the watcher keeps between checks, in slack/state.json.
type State struct {
	UserID       string               `json:"user_id,omitempty"`
	On           bool                 `json:"on,omitempty"`         // the watcher was on at its last tick
	Since        time.Time            `json:"since,omitempty"`      // messages after this are new; zero until the first check
	LastTry      time.Time            `json:"last_try,omitempty"`   // the last check started
	LastCheck    time.Time            `json:"last_check,omitempty"` // the last check that worked
	LastCost     float64              `json:"last_cost,omitempty"`
	Problem      string               `json:"problem,omitempty"`
	ProblemSince *time.Time           `json:"problem_since,omitempty"`
	Failures     int                  `json:"failures,omitempty"`
	NextTry      *time.Time           `json:"next_try,omitempty"`
	Seen         map[string]time.Time `json:"seen,omitempty"` // message ID (channel/ts) → when it was posted
	Muted        []Mute               `json:"muted,omitempty"`
	Starts       []time.Time          `json:"starts,omitempty"`   // research sessions started by the watcher
	Skipped      []Skipped            `json:"skipped,omitempty"`  // asks that made no task, oldest first
	SkipSeq      int                  `json:"skip_seq,omitempty"` // the number the last skipped ask got
}

// Skipped is an ask that made no task, why, and when it was skipped: you
// may have answered it already (Answered), or the topic check found it
// off-topic, and Why.
type Skipped struct {
	Ask      Ask       `json:"ask"`
	Why      string    `json:"why"`
	Answered bool      `json:"maybe_answered,omitempty"`
	At       time.Time `json:"at"`
	N        int       `json:"n"` // its number, which `slack take` takes
}

// Label is how the skipped list shows the ask: when it was posted, in loc,
// as the strip shows its link, then the link's label, "Oct 28 12:59
// #billing · Dana Reed: can the invoice fix ship Friday?".
func (k Skipped) Label(loc *time.Location) string {
	return task.CleanLabel(task.SlackStamp(k.Ask.Permalink, loc) + " " + askLabel(k.Ask))
}

// Reason is why the ask was skipped, as the skipped list says it: "may be
// answered already", or "off-topic: lunch plans".
func (k Skipped) Reason() string {
	if k.Answered {
		return "may be answered already"
	}
	return "off-topic: " + task.CleanLabel(k.Why)
}

// file is the state's file, slack/state.json in the state folder. Each herdr
// session keeps its tasks in <session>.json there, so the watcher's is in
// the slack folder, where a session named "slack" can't take its place.
func file(dir string) store.File { return store.File{Dir: filepath.Join(dir, "slack"), Name: "state"} }

// Load reads slack/state.json in the state folder dir. A missing file is
// an empty state. Skipped asks saved before they had numbers get them.
func Load(dir string) (State, error) {
	var s State
	err := file(dir).Load(&s)
	s.number()
	return s, err
}

// Update runs fn on the state under an exclusive lock and saves the result.
// Nothing is saved if fn returns an error. Skipped asks saved before they
// had numbers get them before fn runs, and keep them once it's saved.
func Update(dir string, fn func(*State) error) error {
	var s State
	return file(dir).Update(&s, func() error {
		s.number()
		return fn(&s)
	})
}

// number gives the skipped asks that have no number one each, oldest
// first, as Skip would have.
func (s *State) number() {
	if !slices.ContainsFunc(s.Skipped, func(k Skipped) bool { return k.N == 0 }) {
		return
	}
	slices.SortStableFunc(s.Skipped, func(x, y Skipped) int { return x.At.Compare(y.At) })
	for _, k := range s.Skipped {
		s.SkipSeq = max(s.SkipSeq, k.N)
	}
	for i := range s.Skipped {
		if s.Skipped[i].N == 0 {
			s.SkipSeq++
			s.Skipped[i].N = s.SkipSeq
		}
	}
}

// IsMuted reports whether a channel is muted. State mutes match by ID; skip
// (the slack_skip_channels setting) matches by ID, or by name with or without
// the "#", in any case.
func (s *State) IsMuted(id, place string, skip []string) bool {
	if slices.ContainsFunc(s.Muted, func(m Mute) bool { return m.ID == id }) {
		return true
	}
	name := bare(place)
	for _, k := range skip {
		if k == id || (name != "" && strings.EqualFold(bare(k), name)) {
			return true
		}
	}
	return false
}

func bare(name string) string { return strings.TrimPrefix(strings.TrimSpace(name), "#") }

// Mute adds a channel to the muted ones. It returns false when it already is.
func (s *State) Mute(id, name string) bool {
	if slices.ContainsFunc(s.Muted, func(m Mute) bool { return m.ID == id }) {
		return false
	}
	s.Muted = append(s.Muted, Mute{ID: id, Name: name})
	return true
}

// Unmute removes a muted channel, by ID or name. It returns false when none
// matches.
func (s *State) Unmute(which string) bool {
	i := slices.IndexFunc(s.Muted, func(m Mute) bool {
		return m.ID == which || strings.EqualFold(bare(m.Name), bare(which))
	})
	if i < 0 {
		return false
	}
	s.Muted = slices.Delete(s.Muted, i, i+1)
	return true
}

// Prune drops Seen messages older than 96 h, Starts older than an hour, and
// skipped asks older than 7 days, which it returns.
func (s *State) Prune(now time.Time) (dropped []Skipped) {
	for id, at := range s.Seen {
		if now.Sub(at) > seenFor {
			delete(s.Seen, id)
		}
	}
	s.Starts = slices.DeleteFunc(s.Starts, func(t time.Time) bool { return now.Sub(t) > startsFor })
	s.Skipped = slices.DeleteFunc(s.Skipped, func(k Skipped) bool {
		if now.Sub(k.At) > skippedFor {
			dropped = append(dropped, k)
			return true
		}
		return false
	})
	return dropped
}

// Skip keeps an ask that made no task on the skipped list, with why, and
// its text cut to 2,000 runes. It takes the next number, or the number of
// the entry for the same message, which it replaces. The list keeps the
// newest 50, and Skip returns those it dropped.
func (s *State) Skip(a Ask, why Skip, now time.Time) (dropped []Skipped) {
	s.number()
	a.Text = cut(a.Text, maxJudged)
	a.Participants = slices.Clone(a.Participants)
	same := func(k Skipped) bool { return k.Ask.ID() == a.ID() }
	var n int
	if i := slices.IndexFunc(s.Skipped, same); i >= 0 {
		n = s.Skipped[i].N
	} else {
		s.SkipSeq++
		n = s.SkipSeq
	}
	s.Skipped = slices.DeleteFunc(s.Skipped, same)
	s.Skipped = append(s.Skipped, Skipped{Ask: a, Why: why.Why, Answered: why.Answered, At: now, N: n})
	slices.SortStableFunc(s.Skipped, func(x, y Skipped) int { return x.At.Compare(y.At) })
	if over := len(s.Skipped) - maxSkipped; over > 0 {
		dropped = slices.Clone(s.Skipped[:over])
		s.Skipped = slices.Delete(s.Skipped, 0, over)
	}
	return dropped
}

// SkippedList is the skipped asks, newest first.
func (s *State) SkippedList() []Skipped {
	list := slices.Clone(s.Skipped)
	slices.SortStableFunc(list, func(x, y Skipped) int { return x.At.Compare(y.At) })
	slices.Reverse(list)
	return list
}

// Take removes the skipped ask which names and returns it. which is its
// number, N, with or without a "#", never its place in the list; or else
// its permalink, or its message ID.
func (s *State) Take(which string) (Skipped, bool) {
	i := -1
	if n, err := strconv.Atoi(strings.TrimPrefix(which, "#")); err == nil && n >= 1 {
		i = slices.IndexFunc(s.Skipped, func(k Skipped) bool { return k.N == n })
	}
	if i < 0 {
		i = slices.IndexFunc(s.Skipped, func(k Skipped) bool { return k.Ask.Permalink == which })
	}
	if i < 0 {
		i = slices.IndexFunc(s.Skipped, func(k Skipped) bool { return k.Ask.ID() == which })
	}
	if i < 0 {
		return Skipped{}, false
	}
	k := s.Skipped[i]
	s.Skipped = slices.DeleteFunc(s.Skipped, func(o Skipped) bool { return o.Ask.ID() == k.Ask.ID() })
	return k, true
}

// CanStart reports whether fewer than max research sessions started in the
// last hour.
func (s *State) CanStart(max int, now time.Time) bool {
	n := 0
	for _, t := range s.Starts {
		if now.Sub(t) <= startsFor {
			n++
		}
	}
	return n < max
}
