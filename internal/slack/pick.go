package slack

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// Kind is how an ask reached you.
type Kind string

const (
	KindMention Kind = "mention" // an @-mention of you in a channel or thread
	KindDM      Kind = "dm"
	KindGroup   Kind = "group" // a group DM
)

// Ask is a message that asks something of you.
type Ask struct {
	Message
	Kind Kind
	Key  string // the conversation: Channel+"/"+(ThreadTS or TS) for a channel message; Channel for a DM or group DM
	Gist string // cleaned, ≤ 40 runes; the text's first words when the run gave none
	At   time.Time
	// Unsure is an ask you may have answered: the check saw only your newest
	// messages, and the ask is older than all of them.
	Unsure bool
}

// Pick is what a check found, and how far it got.
type Pick struct {
	Asks    []Ask     // oldest first, at most 20
	Since   time.Time // the new watermark
	Seen    []string  // message IDs to mark seen: the asks and everything dropped within the watermark
	Dropped []string  // one line per dropped message not seen before, "<key>: <reason>", for slack.log; never message text
	Gap     string    // "" or the period skipped because it was over 72 h ago
	// Crowded is a second with more messages than a page, whose rest is
	// skipped; zero when there's none.
	Crowded time.Time
	// Imitated are the links of the messages whose text imitates Slack's
	// result layout, and ImitatedAfter is the time from which asks may
	// have been skipped for them: the check's start.
	Imitated      []string
	ImitatedAfter time.Time
	// More is a full page, or the 20-ask cap, that stopped the watermark
	// before now: the next check has more to take.
	More bool
}

const (
	catchUp  = 72 * time.Hour   // a check goes back at most this far
	lateBy   = 10 * time.Minute // Slack's search can show a message this late
	ahead    = time.Minute      // how far Slack's clock may be ahead of this one
	maxAsks  = 20
	maxGist  = 40
	pageSize = 20 // the most one search returns
	gapTime  = "Mon Jan 2 15:04"

	alreadySeen = "already seen" // a drop reason that gets no Dropped line
)

// From is where a check starts: at since, but not more than 72 h ago. When
// the last check that worked saw everything up to its own start (since isn't
// before lastCheck), it starts 10 minutes earlier, for messages Slack's
// search shows late. After a check that stopped at a full page, it starts at
// since exactly, so its page goes past what that check saw.
func From(since, lastCheck, now time.Time) time.Time {
	from := since
	if !since.Before(lastCheck) {
		from = since.Add(-lateBy)
	}
	if oldest := now.Add(-catchUp); from.Before(oldest) {
		return oldest
	}
	return from
}

// role is which of the check's searches a search is.
type role int

const (
	mentions role = iota
	dms
	groups
	own
)

func (r role) String() string {
	return [...]string{"the search for mentions of you", "the search for DMs", "the search for group DMs", "the search for your own messages"}[r]
}

// roleOf tells a search's role from its input.
func roleOf(in SearchInput, me string) (role, bool) {
	if slices.Contains(in.Keywords, "<@"+me+">") {
		return mentions, true
	}
	filters := strings.Fields(in.Filters)
	switch {
	case slices.Contains(filters, "is:dm"):
		return dms, true
	case slices.Contains(filters, "is:mpim"):
		return groups, true
	case slices.Contains(filters, "from:<@"+me+">"):
		return own, true
	}
	return 0, false
}

var (
	slackTS   = regexp.MustCompile(`^(\d{10})\.(\d{6})$`)
	channelID = regexp.MustCompile(`^[A-Z0-9]{2,}$`)
	personID  = regexp.MustCompile(`^[UW][A-Z0-9]{2,}$`)
)

// tsTime is when the message with Slack ts ts was posted.
func tsTime(ts string) (time.Time, bool) {
	m := slackTS.FindStringSubmatch(ts)
	if m == nil {
		return time.Time{}, false
	}
	sec, _ := strconv.ParseInt(m[1], 10, 64)
	usec, _ := strconv.ParseInt(m[2], 10, 64)
	return time.Unix(sec, usec*1000), true
}

// dmLike reports whether m is in a DM or a group DM.
func dmLike(m Message) bool { return strings.HasPrefix(m.Channel, "D") || len(m.Participants) > 0 }

// thread is the ts of the thread m is in, or starts.
func thread(m Message) string {
	if m.ThreadTS != "" {
		return m.ThreadTS
	}
	return m.TS
}

// logKey is m's conversation key, for a Dropped line. Parts Slack wouldn't
// make show as "?", so nothing from the message's text reaches the log.
func logKey(m Message) string {
	ch := m.Channel
	if !channelID.MatchString(ch) {
		ch = "?"
	}
	if dmLike(m) {
		return ch
	}
	ts := thread(m)
	if !slackTS.MatchString(ts) {
		ts = "?"
	}
	return ch + "/" + ts
}

// found is one message from the mention, DM or group searches.
type found struct {
	m   Message
	at  time.Time // zero when its ts isn't a Slack ts
	why string    // why it's dropped; "" for an ask
}

// PickAsks picks the asks from a check's output. Each message is checked
// against what Slack said about it, and against your settings and state. It
// fails when a search the check needs is missing, failed, or didn't search as
// asked, so the watermark doesn't move past what Slack didn't say. The check
// started at From(since, st.LastCheck, now). The watermark moves back only
// when the 20-ask cap stops before since.
func PickAsks(out Output, me string, st *State, set Settings, since, now time.Time) (Pick, error) {
	from := From(since, st.LastCheck, now)
	need := []role{mentions, own}
	if set.DMs {
		need = []role{mentions, dms, groups, own}
	}
	byRole := map[role][]Search{}
	for _, s := range out.Searches {
		if r, ok := roleOf(s.Input, me); ok && slices.Contains(need, r) {
			byRole[r] = append(byRole[r], s)
		}
	}
	for _, r := range need {
		if len(byRole[r]) == 0 {
			return Pick{}, fmt.Errorf("the check didn't run %s", r)
		}
		for _, s := range byRole[r] {
			if s.Err != "" {
				return Pick{}, fmt.Errorf("%s failed: %s", r, strings.Join(strings.Fields(task.CleanText(s.Err)), " "))
			}
			if why := unasked(s.Input, r, from); why != "" {
				return Pick{}, fmt.Errorf("the check didn't search as asked: %s %s", r, why)
			}
			if field := offContract(s.Input, r, me); field != "" {
				return Pick{}, fmt.Errorf("the check ran a different search than asked: %s", field)
			}
		}
	}

	var all []*found
	byID := map[string]*found{}
	horizon, full := now, false
	var crowded time.Time
	var imitated []string
	var imitatedAfter time.Time
	for _, r := range []role{mentions, dms, groups} {
		for _, s := range byRole[r] {
			b, ok := bound(s, since, now)
			if len(s.Warnings) > 0 {
				// A message on the page imitates the layout, and may hide
				// the results after it anywhere on the page, so asks from
				// the check's start on may have been skipped.
				imitatedAfter = from
				for _, w := range s.Warnings {
					imitated = append(imitated, strings.TrimPrefix(w, imitation))
				}
			}
			if ok && stuck(s, b, st) {
				// The same page would come back every time: skip the rest
				// of that second.
				crowded, b = time.Unix(s.Input.After, 0), time.Unix(s.Input.After+1, 0)
			}
			if ok && (!full || b.Before(horizon)) {
				horizon, full = b, true
			}
			for _, m := range s.Messages {
				why := drop(m, r, me, st, set, from, now)
				f := byID[m.ID()]
				switch {
				case f == nil:
					at, _ := tsTime(m.TS)
					f = &found{m: m, at: at, why: why}
					byID[m.ID()] = f
					all = append(all, f)
				case f.why != "" && why == "":
					// Another search found it too, and this one shows it's an ask.
					f.m, f.why = m, ""
				}
			}
		}
	}
	if horizon.Before(since) {
		// The last check saw everything up to since. A full page that ends
		// before it says nothing past its end, but the other searches'
		// messages up to since are known: take them.
		horizon = since
	}

	var mine []Message
	for _, s := range byRole[own] {
		mine = append(mine, s.Messages...)
	}
	unsure := maybeAnswered(byRole[own], now)
	var asks []*found
	for _, f := range all {
		if f.why == "" && answered(f, mine) {
			f.why = "answered"
		}
		if f.why == "" && !f.at.After(horizon) {
			asks = append(asks, f)
		}
	}
	slices.SortStableFunc(asks, func(a, b *found) int { return a.at.Compare(b.at) })
	if len(asks) > maxAsks {
		// Only this moves the watermark back, when the 20th ask is before
		// since, so the next check starts there and takes the rest.
		asks = asks[:maxAsks]
		horizon = asks[maxAsks-1].at
	}

	p := Pick{Since: horizon, More: horizon.Before(now), Imitated: imitated, ImitatedAfter: imitatedAfter}
	if !crowded.IsZero() && horizon.Equal(crowded.Add(time.Second)) {
		// The skip moved the watermark; another page may have stopped it.
		p.Crowded = crowded
	}
	token := regexp.MustCompile(`<@` + regexp.QuoteMeta(me) + `(\|[^>]*)?>`) // your mention, as Slack writes it
	for _, f := range asks {
		a := ask(f, out.Gists, token)
		a.Unsure = unsure(f.at)
		p.Asks = append(p.Asks, a)
		p.Seen = append(p.Seen, f.m.ID())
	}
	for _, f := range all {
		switch {
		case f.why == "":
		case f.at.IsZero():
			p.Dropped = append(p.Dropped, logKey(f.m)+": "+f.why)
		case !f.at.After(horizon):
			if f.why != alreadySeen { // each check looks back over some it saw
				p.Dropped = append(p.Dropped, logKey(f.m)+": "+f.why)
			}
			p.Seen = append(p.Seen, f.m.ID())
		}
	}
	if !since.IsZero() && now.Sub(since) > catchUp {
		p.Gap = since.Format(gapTime) + " to " + now.Add(-catchUp).Format(gapTime)
	}
	return p, nil
}

// unasked says how a search differs from what the check asked for, or "".
// The mention, DM and group searches go oldest first, so a full page's last
// message bounds what's known. Every search starts exactly at from: a later
// start misses messages, and an earlier one, or none, fills the page with
// ones already settled, so the watermark could stop without a sign.
func unasked(in SearchInput, r role, from time.Time) string {
	switch {
	case r != own && in.SortDir != "asc":
		return fmt.Sprintf("is sorted %q, not \"asc\"", in.SortDir)
	case in.After == 0:
		return "has no start (after)"
	case in.After != from.Unix():
		return fmt.Sprintf("starts at %d, not at the check's start %d", in.After, from.Unix())
	}
	return ""
}

// stuck reports whether a full search's page, which goes to b, is all in
// the second the search starts at, and holds nothing new: the next check
// would get the same page.
func stuck(s Search, b time.Time, st *State) bool {
	if b.Unix() != s.Input.After {
		return false
	}
	for _, m := range s.Messages {
		if !hasKey(st.Seen, m.ID()) {
			return false
		}
	}
	return true
}

// allChannels is channel_types for every kind of conversation.
const allChannels = "public_channel,private_channel,mpim,im"

// offContract names a field of a search's input that isn't as the check
// asked for that search, or is "". The query is exactly the search's:
// your mention, or a filter, with nothing added. after and sort_dir are
// unasked's. The rest are the values the prompt gives, or not there; your
// own messages may come in either order.
func offContract(in SearchInput, r role, me string) string {
	var keywords []string
	filters, channels := "", allChannels
	switch r {
	case mentions:
		keywords = []string{"<@" + me + ">"}
	case dms:
		filters, channels = "is:dm", "im"
	case groups:
		filters, channels = "is:mpim", "mpim"
	case own:
		filters = "from:<@" + me + ">"
	}
	switch {
	case len(in.Unknown) > 0:
		return in.Unknown[0]
	case !slices.Equal(in.Keywords, keywords): // [] and none are equal
		return "keywords"
	case in.Filters != filters:
		return "filters"
	case in.ChannelTypes != "" && !sameTypes(in.ChannelTypes, channels):
		return "channel_types"
	case in.ContentTypes != "" && in.ContentTypes != "messages":
		return "content_types"
	case in.Before != 0:
		return "before"
	case in.Sort != "" && in.Sort != "timestamp":
		return "sort"
	case r == own && in.SortDir != "" && in.SortDir != "asc" && in.SortDir != "desc":
		return "sort_dir"
	case in.Limit != 0 && in.Limit != pageSize:
		return "limit"
	case in.IncludeContext:
		return "include_context"
	case in.IncludeBots:
		return "include_bots"
	case in.NaturalLanguageQuery != "":
		return "natural_language_query"
	case in.Cursor != "":
		return "cursor"
	case in.ResponseFormat != "" && in.ResponseFormat != "detailed":
		return "response_format"
	case in.OnlyMyChannels:
		return "only_my_channels"
	}
	return ""
}

// sameTypes reports whether the channel_types a and b name the same kinds
// of conversation, in any order.
func sameTypes(a, b string) bool {
	set := func(s string) []string {
		types := strings.Split(s, ",")
		for i, t := range types {
			types[i] = strings.TrimSpace(t)
		}
		slices.Sort(types)
		return slices.Compact(types)
	}
	return slices.Equal(set(a), set(b))
}

// fullPage reports whether a search came back full: with its limit of
// results, or more to come. Past what it shows, it doesn't say what there is.
func fullPage(s Search) bool {
	limit := s.Input.Limit
	if limit <= 0 || limit > pageSize {
		limit = pageSize
	}
	return len(s.Messages) > 0 && (s.More || len(s.Messages) >= limit)
}

// maybeAnswered reports, for an ask's time, whether you may have answered it
// where the searches for your own messages didn't look. A full page of them,
// newest first, holds only your newest, so an ask older than all of them
// may be answered. Only a time the search could return counts, from its
// start to now, so a forged one can't move what's seen back. A full page
// oldest first holds your oldest, and one without such a time says nothing
// of when, so any ask may be.
func maybeAnswered(mine []Search, now time.Time) func(at time.Time) bool {
	var known time.Time // your messages from this on are all seen
	all := false        // no ask is sure
	for _, s := range mine {
		if !fullPage(s) {
			continue
		}
		if s.Input.SortDir == "asc" {
			all = true
			continue
		}
		start := time.Unix(s.Input.After, 0)
		var oldest time.Time
		for _, m := range s.Messages {
			at, ok := tsTime(m.TS)
			if !ok || at.Before(start) || at.After(now) {
				continue
			}
			if oldest.IsZero() || at.Before(oldest) {
				oldest = at
			}
		}
		if oldest.IsZero() {
			all = true
		}
		if oldest.After(known) {
			known = oldest
		}
	}
	return func(at time.Time) bool { return all || at.Before(known) }
}

// bound is how far a full search's results go: its newest message's time
// that isn't after now. Past it, the search doesn't say what there is. The
// newest, not the last: a message's text can forge a last result, with any
// time, which would hold the watermark back or move it past now. It's false
// when the search isn't full, and it's since when none of its messages has
// a Slack ts up to now.
func bound(s Search, since, now time.Time) (time.Time, bool) {
	if !fullPage(s) {
		return time.Time{}, false
	}
	var newest time.Time
	for _, m := range s.Messages {
		if at, ok := tsTime(m.TS); ok && !at.After(now) && at.After(newest) {
			newest = at
		}
	}
	if newest.IsZero() {
		return since, true
	}
	return newest, true
}

// drop says why a message the search r found isn't an ask, or "" when it is
// one so far.
func drop(m Message, r role, me string, st *State, set Settings, from, now time.Time) string {
	at, ok := tsTime(m.TS)
	switch {
	case !ok:
		return "its ts isn't a Slack timestamp"
	case at.After(now.Add(ahead)):
		return "its time is in the future"
	case hasKey(st.Seen, m.ID()):
		return alreadySeen
	case at.Before(from):
		return "older than the check's window"
	case !channelID.MatchString(m.Channel):
		return "its channel isn't a Slack ID"
	}
	if why := linkProblem(m); why != "" {
		return why
	}
	switch {
	case m.AuthorID == me:
		return "your own message"
	case m.Bot:
		return "from a bot or app"
	case !personID.MatchString(m.AuthorID):
		return "its author isn't a person"
	}
	switch r {
	case mentions:
		if !strings.Contains(m.Text, "<@"+me+">") && !strings.Contains(m.Text, "<@"+me+"|") {
			return "doesn't mention you by name"
		}
	case dms:
		if !strings.HasPrefix(m.Channel, "D") {
			return "the DM search found a message outside a DM"
		}
	case groups:
		if !slices.Contains(m.Participants, me) {
			return "you're not in its group DM"
		}
	}
	switch {
	case dmLike(m) && !set.DMs:
		return "DMs are off (slack_dms)"
	case st.IsMuted(m.Channel, m.Place, set.Skip):
		return "muted channel"
	}
	return ""
}

func hasKey(seen map[string]time.Time, id string) bool {
	_, ok := seen[id]
	return ok
}

// linkProblem says what's wrong with m's permalink, or "" when it's the
// link Slack gives that message.
func linkProblem(m Message) string {
	if !task.SafeURL(m.Permalink) || task.KindOf(m.Permalink) != task.KindSlack {
		return "its permalink isn't a Slack link"
	}
	u, _ := url.Parse(m.Permalink) // KindOf parsed it already.
	if !strings.HasSuffix(strings.ToLower(u.Host), ".slack.com") {
		return "its permalink isn't on slack.com"
	}
	if u.EscapedPath() != "/archives/"+m.Channel+"/p"+strings.Replace(m.TS, ".", "", 1) {
		return "its permalink is for another message"
	}
	q := u.Query()
	for _, cid := range q["cid"] {
		if cid != m.Channel {
			return "its permalink names another channel"
		}
	}
	if q.Get("thread_ts") != m.ThreadTS || (m.ThreadTS != "" && !slackTS.MatchString(m.ThreadTS)) {
		return "its permalink names another thread"
	}
	return ""
}

// answered reports whether you wrote in f's conversation after it: in its
// thread for a channel message, anywhere in it for a DM or group DM.
func answered(f *found, mine []Message) bool {
	for _, o := range mine {
		at, ok := tsTime(o.TS)
		if !ok || !at.After(f.at) || o.Channel != f.m.Channel {
			continue
		}
		if dmLike(f.m) || o.ThreadTS == thread(f.m) {
			return true
		}
	}
	return false
}

// ask makes an Ask of f. Its gist is the run's, or else the text's first
// words without your mention token.
func ask(f *found, gists map[string]string, token *regexp.Regexp) Ask {
	a := Ask{Message: f.m, Kind: KindMention, Key: f.m.Channel + "/" + thread(f.m), At: f.at}
	if dmLike(f.m) {
		a.Kind, a.Key = KindGroup, f.m.Channel
		if strings.HasPrefix(f.m.Channel, "D") {
			a.Kind = KindDM
		}
	}
	a.Gist = cut(task.CleanLabel(gists[f.m.ID()]), maxGist)
	if a.Gist == "" {
		a.Gist = firstWords(task.CleanLabel(token.ReplaceAllString(f.m.Text, " ")), maxGist)
	}
	return a
}

// cut cuts s to max runes, ending in "…" when it's cut.
func cut(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return strings.TrimRight(string(r[:max-1]), " ") + "…"
	}
	return s
}

// firstWords is the whole words of s that fit in max runes, ending in "…"
// when some are left out. A first word longer than that is cut.
func firstWords(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	var words []string
	for _, w := range strings.Fields(s) {
		next := n + utf8.RuneCountInString(w)
		if n > 0 {
			next++
		}
		if next > max-1 {
			break
		}
		words, n = append(words, w), next
	}
	if len(words) == 0 {
		return cut(s, max)
	}
	return strings.Join(words, " ") + "…"
}
