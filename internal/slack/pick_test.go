package slack

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const me = "U0ME00001"

// pickNow is Sat Oct 31 14:00 UTC; the check's window starts at 13:40.
var (
	pickNow   = time.Unix(1793455200, 0).UTC()
	pickSince = pickNow.Add(-10 * time.Minute)
)

var names = map[string]string{me: "You", "U0DANA001": "Dana Reed", "U0RUI0001": "Rui Costa"}

// ago is the Slack ts of a message posted d before pickNow.
func ago(d time.Duration) string {
	t := pickNow.Add(-d)
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

func atTS(ts string) time.Time {
	at, ok := tsTime(ts)
	if !ok {
		panic("bad ts " + ts)
	}
	return at
}

// msg is a message with the permalink Slack gives it. A D… channel is a DM
// between its author and you.
func msg(channel, place, author, ts, threadTS, text string) Message {
	link := "https://acme.slack.com/archives/" + channel + "/p" + strings.Replace(ts, ".", "", 1)
	if threadTS != "" {
		link += "?thread_ts=" + threadTS + "&cid=" + channel
	}
	m := Message{Channel: channel, Place: place, AuthorID: author, Author: names[author],
		TS: ts, ThreadTS: threadTS, Permalink: link, Text: text}
	if strings.HasPrefix(channel, "D") {
		m.Participants = []string{author, me}
	}
	return m
}

func billing(author, ts, threadTS, text string) Message {
	return msg("C0123ABCD", "#billing", author, ts, threadTS, text)
}

func dm(author, ts, text string) Message { return msg("D0RUI0001", "DM", author, ts, "", text) }

func group(author, ts, text string) Message {
	m := msg("C0GRP0001", "mpdm-dana--rui--me-1", author, ts, "", text)
	m.Participants = []string{"U0DANA001", "U0RUI0001", me}
	return m
}

// pickStart is where pick's check starts: 10 minutes before pickSince.
var pickStart = From(pickSince, time.Time{}, pickNow).Unix()

func mentionSearch(msgs ...Message) Search {
	return Search{Input: SearchInput{Keywords: []string{"<@" + me + ">"}, SortDir: "asc", After: pickStart, Limit: 20}, Messages: msgs}
}

func dmSearch(msgs ...Message) Search {
	return Search{Input: SearchInput{Filters: "is:dm", ChannelTypes: "im", SortDir: "asc", After: pickStart, Limit: 20}, Messages: msgs}
}

func groupSearch(msgs ...Message) Search {
	return Search{Input: SearchInput{Filters: "is:mpim", ChannelTypes: "mpim", SortDir: "asc", After: pickStart, Limit: 20}, Messages: msgs}
}

func ownSearch(msgs ...Message) Search {
	return Search{Input: SearchInput{Filters: "from:<@" + me + ">", SortDir: "desc", After: pickStart, Limit: 20}, Messages: msgs}
}

// startingAt is out with every search starting at start.
func startingAt(out Output, start time.Time) Output {
	out.Searches = slices.Clone(out.Searches)
	for i := range out.Searches {
		out.Searches[i].Input.After = start.Unix()
	}
	return out
}

// slackFrom answers a check that starts at start as Slack would: each
// search's messages from that second on, oldest first, at most 20.
func slackFrom(start time.Time, mentions, dms []Message) Output {
	page := func(s Search, msgs []Message) Search {
		s.Input.After = start.Unix()
		for _, m := range msgs {
			if atTS(m.TS).Unix() >= start.Unix() && len(s.Messages) < 20 {
				s.Messages = append(s.Messages, m)
			}
		}
		return s
	}
	return Output{Searches: []Search{page(mentionSearch(), mentions), page(dmSearch(), dms), page(groupSearch(), nil), page(ownSearch(), nil)}}
}

// output is a check that ran all four searches.
func output(mentions, dms, groups, own []Message) Output {
	return Output{Connector: "connected", Gists: map[string]string{}, Searches: []Search{
		mentionSearch(mentions...), dmSearch(dms...), groupSearch(groups...), ownSearch(own...),
	}}
}

var dmsOn = Settings{DMs: true}

func pick(t *testing.T, out Output, st *State, set Settings) Pick {
	t.Helper()
	if st == nil {
		st = &State{}
	}
	p, err := PickAsks(out, me, st, set, pickSince, pickNow)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func keys(asks []Ask) []string {
	var ks []string
	for _, a := range asks {
		ks = append(ks, a.Key)
	}
	return ks
}

func TestPickTakesMentionsAndDMs(t *testing.T) {
	thread := billing("U0DANA001", "1793454730.123456", "1793450000.000100", "<@U0ME00001> can the invoice fix ship Friday?")
	direct := dm("U0RUI0001", ago(5*time.Minute), "<@U0ME00001|you> quick q: is ABC-12 merged?")
	grp := group("U0DANA001", ago(3*time.Minute), "who owns the billing page?")
	// The DM is found by the mention search and the DM search: one ask.
	p := pick(t, output([]Message{thread, direct}, []Message{direct}, []Message{grp}, nil), nil, dmsOn)

	if !slices.Equal(keys(p.Asks), []string{"C0123ABCD/1793450000.000100", "D0RUI0001", "C0GRP0001"}) {
		t.Fatalf("keys = %v, dropped %v", keys(p.Asks), p.Dropped)
	}
	for i, kind := range []Kind{KindMention, KindDM, KindGroup} {
		if p.Asks[i].Kind != kind {
			t.Errorf("ask %d kind = %q, want %q", i, p.Asks[i].Kind, kind)
		}
	}
	if a := p.Asks[0]; !a.At.Equal(time.Unix(1793454730, 123456000)) || a.TS != thread.TS || a.Author != "Dana Reed" || a.Permalink != thread.Permalink {
		t.Errorf("mention = %+v", a)
	}
	if !slices.Equal(p.Seen, []string{thread.ID(), direct.ID(), grp.ID()}) {
		t.Errorf("seen = %v", p.Seen)
	}
	if !p.Since.Equal(pickNow) || len(p.Dropped) != 0 || p.Gap != "" {
		t.Errorf("since %v dropped %v gap %q", p.Since, p.Dropped, p.Gap)
	}

	// The next check finds them again, in the 10 minutes it looks back.
	st := State{Seen: map[string]time.Time{}}
	for _, id := range p.Seen {
		st.Seen[id] = pickNow
	}
	again := pick(t, output([]Message{thread, direct}, []Message{direct}, []Message{grp}, nil), &st, dmsOn)
	if len(again.Asks) != 0 || len(again.Dropped) != 0 || !slices.Equal(again.Seen, p.Seen) {
		t.Errorf("again: asks %v dropped %v seen %v", keys(again.Asks), again.Dropped, again.Seen)
	}

	// The DM search finds only DMs, and the group search only group DMs you're in.
	notDM := msg("C0123ABCD", "#billing", "U0RUI0001", ago(4*time.Minute), "", "in a channel")
	notIn := group("U0DANA001", ago(2*time.Minute), "not for you")
	notIn.Participants = []string{"U0DANA001", "U0RUI0001"}
	p = pick(t, output(nil, []Message{notDM}, []Message{notIn}, nil), nil, dmsOn)
	if len(p.Asks) != 0 || len(p.Dropped) != 2 {
		t.Errorf("outsiders: asks %v dropped %v", keys(p.Asks), p.Dropped)
	}
}

func TestPickDropsHereGroupsBotsAndYou(t *testing.T) {
	bot := billing("B0BOT0001", ago(9*time.Minute), "", "<@U0ME00001> deploy finished")
	bot.Author, bot.Bot = "Deploys", true
	mentions := []Message{
		billing("U0DANA001", ago(8*time.Minute), "", "<!here> standup"),
		billing("U0DANA001", ago(7*time.Minute), "", "<!subteam^S0TEAM001> review"),
		bot,
		billing(me, ago(6*time.Minute), "", "<@U0ME00001> note to self"),
	}
	p := pick(t, output(mentions, nil, nil, nil), nil, dmsOn)
	if len(p.Asks) != 0 || len(p.Dropped) != 4 {
		t.Fatalf("asks %v dropped %v", keys(p.Asks), p.Dropped)
	}
	for _, line := range p.Dropped {
		if !strings.HasPrefix(line, "C0123ABCD/") || !strings.Contains(line, ": ") {
			t.Errorf("line %q isn't <key>: <reason>", line)
		}
		for _, word := range []string{"standup", "review", "deploy", "note", "here", "subteam"} {
			if strings.Contains(line, word) {
				t.Errorf("line %q has message text %q", line, word)
			}
		}
	}
	if len(p.Seen) != 4 {
		t.Errorf("seen = %v", p.Seen)
	}

	// Only your own ID counts, not a longer one that starts with it, and only
	// people's messages, marked [bot] or not.
	longer := billing("U0DANA001", ago(5*time.Minute), "", "<@U0ME000012> can you look?")
	app := billing("B0APP0001", ago(4*time.Minute), "", "<@U0ME00001> build ready")
	named := billing("U0DANA001", ago(3*time.Minute), "", "<@U0ME00001|you> can you look?")
	p = pick(t, output([]Message{longer, app, named}, nil, nil, nil), nil, dmsOn)
	if !slices.Equal(keys(p.Asks), []string{"C0123ABCD/" + named.TS}) || len(p.Dropped) != 2 {
		t.Fatalf("asks %v dropped %v", keys(p.Asks), p.Dropped)
	}
}

func TestPickSkipsAnsweredThreads(t *testing.T) {
	answered := billing("U0DANA001", ago(8*time.Minute), "1793450000.000100", "<@U0ME00001> did the invoice fix ship?")
	open := billing("U0RUI0001", ago(7*time.Minute), "1793451000.000200", "<@U0ME00001> and the refunds?")
	top := billing("U0DANA001", ago(5*time.Minute), "", "<@U0ME00001> ABC-12 is blocked")
	direct := dm("U0RUI0001", ago(4*time.Minute), "are you around?")
	own := []Message{
		billing(me, ago(1*time.Minute), "", "unrelated top-level note"), // not in open's thread
		dm(me, ago(2*time.Minute), "yes"),
		billing(me, ago(3*time.Minute), top.TS, "looking"),
		billing(me, ago(6*time.Minute), answered.ThreadTS, "shipped"),
		billing(me, ago(9*time.Minute), open.ThreadTS, "before the ask"),
	}
	p := pick(t, output([]Message{answered, open, top}, []Message{direct}, nil, own), nil, dmsOn)
	if !slices.Equal(keys(p.Asks), []string{"C0123ABCD/1793451000.000200"}) {
		t.Fatalf("asks = %v, dropped %v", keys(p.Asks), p.Dropped)
	}
	want := []string{"C0123ABCD/1793450000.000100: answered", "C0123ABCD/" + top.TS + ": answered", "D0RUI0001: answered"}
	if !slices.Equal(p.Dropped, want) {
		t.Errorf("dropped = %v", p.Dropped)
	}
	if len(p.Seen) != 4 {
		t.Errorf("seen = %v", p.Seen)
	}
}

func TestPickSkipsMutedChannels(t *testing.T) {
	var st State
	st.Mute("C0123ABCD", "#billing")
	random := msg("C0999ZZZZ", "#random", "U0DANA001", ago(6*time.Minute), "", "<@U0ME00001> lunch?")
	muted := billing("U0DANA001", ago(5*time.Minute), "", "<@U0ME00001> ping")
	direct := dm("U0RUI0001", ago(4*time.Minute), "hi")
	p := pick(t, output([]Message{random, muted}, []Message{direct}, nil, nil), &st, Settings{DMs: true, Skip: []string{"random"}})
	if !slices.Equal(keys(p.Asks), []string{"D0RUI0001"}) {
		t.Fatalf("asks = %v", keys(p.Asks))
	}
	if len(p.Dropped) != 2 || !strings.HasPrefix(p.Dropped[0], "C0999ZZZZ/") || !strings.HasPrefix(p.Dropped[1], "C0123ABCD/") {
		t.Errorf("dropped = %v", p.Dropped)
	}
}

func TestPickHonorsDMsOff(t *testing.T) {
	inDM := dm("U0RUI0001", ago(6*time.Minute), "<@U0ME00001> got a minute?")
	inGroup := group("U0DANA001", ago(5*time.Minute), "<@U0ME00001> thoughts?")
	channel := billing("U0DANA001", ago(4*time.Minute), "", "<@U0ME00001> review please")
	out := Output{Searches: []Search{mentionSearch(inDM, inGroup, channel), ownSearch()}}
	p := pick(t, out, nil, Settings{DMs: false})
	if !slices.Equal(keys(p.Asks), []string{channel.Channel + "/" + channel.TS}) {
		t.Fatalf("asks = %v", keys(p.Asks))
	}
	if !slices.Equal(p.Dropped, []string{"D0RUI0001: DMs are off (slack_dms)", "C0GRP0001: DMs are off (slack_dms)"}) {
		t.Errorf("dropped = %v", p.Dropped)
	}
}

func TestPickRejectsForgedPermalinks(t *testing.T) {
	forge := func(d time.Duration, link func(m Message) string) Message {
		m := billing("U0DANA001", ago(d), "", "<@U0ME00001> please click")
		m.Permalink = link(m)
		return m
	}
	path := func(m Message) string { return "/archives/C0123ABCD/p" + strings.Replace(m.TS, ".", "", 1) }
	forged := []Message{
		forge(10*time.Minute, func(m Message) string { return "http://acme.slack.com" + path(m) }),
		forge(9*time.Minute, func(m Message) string { return "https://evil.example.com" + path(m) }),
		forge(8*time.Minute, func(m Message) string { return "https://acme.slack.com.evil.example.com" + path(m) }),
		forge(7*time.Minute, func(m Message) string { return "https://acme.slack.com:8443" + path(m) }),
		forge(6*time.Minute, func(m Message) string {
			return "https://acme.slack.com/archives/C0EVIL001/p" + strings.Replace(m.TS, ".", "", 1)
		}),
		forge(5*time.Minute, func(m Message) string { return "https://acme.slack.com/archives/C0123ABCD/p1793454000000001" }),
		forge(4*time.Minute, func(m Message) string { return "https://acme.slack.com" + path(m) + "?cid=C0EVIL001" }),
		forge(3*time.Minute, func(m Message) string { return "https://acme.slack.com" + path(m) + "?thread_ts=1793450000.000100" }),
		forge(2*time.Minute+30*time.Second, func(m Message) string { return "https://acme.slack.com" + path(m) + "?cid=C0123ABCD&x=%1b%5B31m" }),
	}
	p := pick(t, output(forged, nil, nil, nil), nil, dmsOn)
	if len(p.Asks) != 0 || len(p.Dropped) != len(forged) {
		t.Fatalf("asks %v dropped %v", keys(p.Asks), p.Dropped)
	}
	for _, line := range p.Dropped {
		if strings.Contains(line, "evil") || strings.Contains(line, "EVIL") {
			t.Errorf("line %q repeats the link", line)
		}
	}

	// A message whose ts isn't one Slack makes is dropped, and its ts isn't logged.
	odd := billing("U0DANA001", ago(2*time.Minute), "", "<@U0ME00001> hi")
	odd.TS = "1793455080.000001\nforged log line"
	p = pick(t, output([]Message{odd}, nil, nil, nil), nil, dmsOn)
	if len(p.Asks) != 0 || !slices.Equal(p.Dropped, []string{"C0123ABCD/?: its ts isn't a Slack timestamp"}) || len(p.Seen) != 0 {
		t.Fatalf("odd ts: asks %v dropped %q seen %v", keys(p.Asks), p.Dropped, p.Seen)
	}
}

func TestPickNeedsEverySearch(t *testing.T) {
	ok := output(nil, nil, nil, nil)
	without := func(i int) Output {
		out := ok
		out.Searches = slices.Delete(slices.Clone(ok.Searches), i, i+1)
		return out
	}
	cases := map[string]struct {
		out  Output
		want string
	}{
		"no searches at all":    {Output{Connector: "connected"}, "mentions"},
		"no mention search":     {without(0), "mentions"},
		"no DM search":          {without(1), "the search for DMs"},
		"no group DM search":    {without(2), "group DMs"},
		"no own-message search": {without(3), "your own messages"},
	}
	for name, c := range cases {
		if _, err := PickAsks(c.out, me, &State{}, dmsOn, pickSince, pickNow); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to name %q", name, err, c.want)
		}
	}

	failed := output(nil, nil, nil, nil)
	failed.Searches[0].Err = "ratelimited\nretry later"
	_, err := PickAsks(failed, me, &State{}, dmsOn, pickSince, pickNow)
	if err == nil || !strings.Contains(err.Error(), "mentions") || !strings.Contains(err.Error(), "ratelimited retry later") {
		t.Errorf("failed search: err = %v", err)
	}

	// Searches for another user's mentions aren't yours.
	other := output(nil, nil, nil, nil)
	other.Searches[0].Input.Keywords = []string{"<@U0DANA001>"}
	if _, err := PickAsks(other, me, &State{}, dmsOn, pickSince, pickNow); err == nil {
		t.Error("a search for someone else's mentions passed as yours")
	}
}

func TestPickStopsAtTwenty(t *testing.T) {
	var mentions, dms []Message
	for i := range 25 {
		at := time.Duration(25-i) * 30 * time.Second // oldest first, 30 s apart
		if i%5 >= 3 {
			dms = append(dms, dm("U0RUI0001", ago(at), fmt.Sprintf("dm %d", i)))
		} else {
			mentions = append(mentions, billing("U0DANA001", ago(at), "", fmt.Sprintf("<@U0ME00001> ask %d", i)))
		}
	}
	p := pick(t, output(mentions, dms, nil, nil), nil, dmsOn)
	if len(p.Asks) != 20 {
		t.Fatalf("asks = %d", len(p.Asks))
	}
	for i := 1; i < len(p.Asks); i++ {
		if !p.Asks[i-1].At.Before(p.Asks[i].At) {
			t.Fatalf("asks aren't oldest first at %d", i)
		}
	}
	twentieth := atTS(ago(6 * 30 * time.Second))
	if !p.Asks[19].At.Equal(twentieth) || !p.Since.Equal(twentieth) || !p.More {
		t.Errorf("20th at %v, since %v, more %v; want %v, and more", p.Asks[19].At, p.Since, p.More, twentieth)
	}
	if len(p.Seen) != 20 {
		t.Errorf("seen %d: %v", len(p.Seen), p.Seen)
	}
	for _, m := range append(mentions, dms...) {
		if atTS(m.TS).After(twentieth) && slices.Contains(p.Seen, m.ID()) {
			t.Errorf("%s is past the 20th but seen", m.ID())
		}
	}
}

func TestPickStopsAtAFullPage(t *testing.T) {
	var page []Message
	for i := range 5 {
		page = append(page, billing("U0DANA001", ago(time.Duration(9-i)*time.Minute), "", "<@U0ME00001> ping"))
	}
	end := atTS(page[4].TS) // 13:55, after since
	before := dm("U0RUI0001", ago(6*time.Minute+30*time.Second), "before the end")
	after := dm("U0RUI0001", ago(3*time.Minute), "after the end")
	bot := dm("U0RUI0001", ago(2*time.Minute), "after the end, from a bot")
	bot.Bot = true

	full := func(s Search) Output {
		return Output{Searches: []Search{s, dmSearch(before, after, bot), groupSearch(), ownSearch()}}
	}
	// A page is full at its limit too; a check asks for 20, and
	// TestTickSkipsASecondWithMoreThanAPage has such a page.
	byMore := mentionSearch(page...)
	byMore.More = true
	for name, out := range map[string]Output{"more": full(byMore)} {
		p := pick(t, out, nil, dmsOn)
		if len(p.Asks) != 6 || p.Asks[5].ID() == after.ID() {
			t.Errorf("%s: asks = %v", name, keys(p.Asks))
		}
		if !p.Since.Equal(end) || !p.More {
			t.Errorf("%s: since = %v, more %v; want %v, and more", name, p.Since, p.More, end)
		}
		if slices.Contains(p.Seen, after.ID()) || slices.Contains(p.Seen, bot.ID()) || len(p.Dropped) != 0 {
			t.Errorf("%s: seen %v dropped %v", name, p.Seen, p.Dropped)
		}
	}

	// A page under its limit, without more, says everything up to now.
	p := pick(t, full(mentionSearch(page...)), nil, dmsOn)
	if len(p.Asks) != 7 || !p.Since.Equal(pickNow) || len(p.Dropped) != 1 || p.More {
		t.Errorf("not full: asks %v since %v dropped %v more %v", keys(p.Asks), p.Since, p.Dropped, p.More)
	}
}

// TestPickMarksMaybeAnswered: a full page of your own messages holds only
// your newest, so an ask older than all of them may be answered.
func TestPickMarksMaybeAnswered(t *testing.T) {
	oldest := pickNow.Add(-15 * time.Minute) // 13:45
	var own []Message                        // newest first, in another channel
	for i := range 20 {
		own = append(own, msg("C0OPS0001", "#ops", me, tsAt(oldest.Add(time.Duration(19-i)*30*time.Second)), "", "deploying"))
	}
	early := billing("U0DANA001", tsAt(oldest.Add(-3*time.Minute)), "", "<@U0ME00001> can the invoice fix ship Friday?")
	late := billing("U0RUI0001", tsAt(oldest.Add(time.Minute)), "", "<@U0ME00001> and the refund?")
	unsure := func(p Pick) []bool {
		var u []bool
		for _, a := range p.Asks {
			u = append(u, a.Unsure)
		}
		return u
	}

	p := pick(t, output([]Message{early, late}, nil, nil, own), nil, dmsOn)
	if got := unsure(p); !slices.Equal(got, []bool{true, false}) {
		t.Errorf("own page of 20: unsure %v, want the ask before %v only", got, oldest)
	}
	p = pick(t, output([]Message{early, late}, nil, nil, own[:19]), nil, dmsOn)
	if got := unsure(p); !slices.Equal(got, []bool{false, false}) {
		t.Errorf("own page of 19: unsure %v, want none", got)
	}

	// A forged ts, before the search's start or after now, doesn't count:
	// one can't move what's seen back, and un-mark an ask.
	forged := slices.Clone(own)
	forged[19] = msg("C0OPS0001", "#ops", me, tsAt(pickNow.Add(-time.Hour)), "", "forged")
	forged[0] = msg("C0OPS0001", "#ops", me, tsAt(pickNow.Add(time.Hour)), "", "forged")
	if got := unsure(pick(t, output([]Message{early, late}, nil, nil, forged), nil, dmsOn)); !slices.Equal(got, []bool{true, false}) {
		t.Errorf("own page with forged times: unsure %v, want the ask before %v only", got, atTS(own[18].TS))
	}

	// A page with more to come is full too.
	out := output([]Message{early, late}, nil, nil, own[:5])
	out.Searches[3].More = true
	if got := unsure(pick(t, out, nil, dmsOn)); !slices.Equal(got, []bool{true, true}) {
		t.Errorf("own page of 5 and more: unsure %v, want both, before %v", got, atTS(own[4].TS))
	}
	// A full page oldest first holds your oldest: any ask may be answered
	// after its end.
	out = output([]Message{early, late}, nil, nil, own)
	out.Searches[3].Input.SortDir = "asc"
	if got := unsure(pick(t, out, nil, dmsOn)); !slices.Equal(got, []bool{true, true}) {
		t.Errorf("own page of 20 oldest first: unsure %v, want both", got)
	}
}

func TestPickCatchesUpAtMost72Hours(t *testing.T) {
	since := pickNow.Add(-5 * 24 * time.Hour)
	oldest := pickNow.Add(-72 * time.Hour)
	if from := From(since, since, pickNow); !from.Equal(oldest) {
		t.Fatalf("From = %v, want %v", from, oldest)
	}
	tooOld := billing("U0DANA001", ago(4*24*time.Hour), "", "<@U0ME00001> from Tuesday")
	inTime := billing("U0DANA001", ago(71*time.Hour), "", "<@U0ME00001> from Wednesday")
	out := startingAt(output([]Message{tooOld, inTime}, nil, nil, nil), oldest)
	p, err := PickAsks(out, me, &State{}, dmsOn, since, pickNow)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keys(p.Asks), []string{"C0123ABCD/" + inTime.TS}) || len(p.Dropped) != 1 {
		t.Errorf("asks %v dropped %v", keys(p.Asks), p.Dropped)
	}
	if p.Gap != "Mon Oct 26 14:00 to Wed Oct 28 14:00" {
		t.Errorf("gap = %q", p.Gap)
	}
	since = pickNow.Add(-71 * time.Hour)
	out = startingAt(output(nil, nil, nil, nil), From(since, time.Time{}, pickNow))
	if p, err := PickAsks(out, me, &State{}, dmsOn, since, pickNow); err != nil || p.Gap != "" {
		t.Errorf("71 h back: gap = %q, err %v", p.Gap, err)
	}
}

func TestFromLooksBackOnlyAfterACompleteCheck(t *testing.T) {
	oldest := pickNow.Add(-72 * time.Hour)
	longAgo := pickNow.Add(-5 * 24 * time.Hour)
	cases := []struct {
		name             string
		since, lastCheck time.Time
		want             time.Time
	}{
		{"the last check saw up to its start", pickSince, pickSince, pickSince.Add(-10 * time.Minute)},
		{"no check yet", pickSince, time.Time{}, pickSince.Add(-10 * time.Minute)},
		{"the last check stopped at a full page", pickSince, pickSince.Add(5 * time.Minute), pickSince},
		{"72 h floor after a complete check", longAgo, longAgo, oldest},
		{"72 h floor after a full page", longAgo, pickNow.Add(-time.Hour), oldest},
	}
	for _, c := range cases {
		if got := From(c.since, c.lastCheck, pickNow); !got.Equal(c.want) {
			t.Errorf("%s: From = %v, want %v", c.name, got, c.want)
		}
	}
}

// busyDMs is a state whose last check saw everything up to 13:50, and the
// 20 DMs you and Rui sent before that, all seen, between 13:40:30 and
// 13:49:03. They fill the DM search's page in the 10 minutes a check looks
// back.
func busyDMs() (State, []Message) {
	st := State{LastCheck: pickSince, Seen: map[string]time.Time{}}
	var busy []Message
	for i := range 20 {
		author := "U0RUI0001"
		if i%2 == 0 {
			author = me
		}
		m := dm(author, ago(19*time.Minute+30*time.Second-time.Duration(i)*27*time.Second), "chat")
		busy = append(busy, m)
		st.Seen[m.ID()] = pickSince
	}
	return st, busy
}

func TestPickDoesntStallOnABusyConversation(t *testing.T) {
	since := pickSince
	st, busy := busyDMs()
	fresh := billing("U0DANA001", ago(7*time.Minute), "", "<@U0ME00001> can you look?") // 13:53
	slack := func(start time.Time) Output { return slackFrom(start, []Message{fresh}, busy) }

	// Check 1, at 13:55, looks back 10 minutes; the busy DMs fill its page.
	now := pickNow.Add(-5 * time.Minute)
	start := From(since, st.LastCheck, now)
	p, err := PickAsks(slack(start), me, &st, dmsOn, since, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Asks) != 0 || !p.Since.Equal(since) {
		t.Fatalf("check 1: asks %v, since %v; want none, and since to stay at %v", keys(p.Asks), p.Since, since)
	}
	st.LastCheck = now
	for _, id := range p.Seen {
		st.Seen[id] = now
	}

	// Check 2, at 14:00, starts at since exactly, past the busy DMs.
	since = p.Since
	if start := From(since, st.LastCheck, pickNow); !start.Equal(since) {
		t.Fatalf("check 2 starts at %v, want %v", start, since)
	}
	p, err = PickAsks(slack(From(since, st.LastCheck, pickNow)), me, &st, dmsOn, since, pickNow)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keys(p.Asks), []string{"C0123ABCD/" + fresh.TS}) || !p.Since.Equal(pickNow) {
		t.Fatalf("check 2: asks %v since %v", keys(p.Asks), p.Since)
	}
}

func TestPickTakesWhatOtherSearchesFoundBeforeSince(t *testing.T) {
	// The busy DMs' full page ends at 13:49:03. The mention search, which
	// isn't full, shows a mention at 13:49:30 that Slack indexed late.
	st, busy := busyDMs()
	late := billing("U0DANA001", ago(10*time.Minute+30*time.Second), "", "<@U0ME00001> one more thing")
	now := pickNow.Add(-5 * time.Minute)
	p, err := PickAsks(slackFrom(From(pickSince, st.LastCheck, now), []Message{late}, busy), me, &st, dmsOn, pickSince, now)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keys(p.Asks), []string{"C0123ABCD/" + late.TS}) || !slices.Contains(p.Seen, late.ID()) || !p.Since.Equal(pickSince) {
		t.Fatalf("asks %v, seen has it %v, since %v; want it taken and seen, and since %v",
			keys(p.Asks), slices.Contains(p.Seen, late.ID()), p.Since, pickSince)
	}
	if len(p.Dropped) != 0 {
		t.Errorf("dropped = %v; the seen DMs need no lines", p.Dropped)
	}
}

func TestPickTakesAnOverlapBurstOverTwoChecks(t *testing.T) {
	// 25 asks you haven't seen, 15 mentions and 10 DMs, came in between 13:40:30
	// and 13:48:30, inside the overlap before since (13:50), as just after
	// you turn the watcher on.
	since := pickSince
	st := State{LastCheck: since, Seen: map[string]time.Time{}}
	var mentions, dms, all []Message
	for i := range 25 {
		at := ago(19*time.Minute + 30*time.Second - time.Duration(i)*20*time.Second)
		if i%5 >= 3 {
			m := dm("U0RUI0001", at, fmt.Sprintf("dm %d", i))
			dms, all = append(dms, m), append(all, m)
		} else {
			m := billing("U0DANA001", at, "", fmt.Sprintf("<@U0ME00001> ask %d", i))
			mentions, all = append(mentions, m), append(all, m)
		}
	}
	twentieth := atTS(all[19].TS) // 13:46:50, before since

	// Check 1, at 13:55: the first 20, and the watermark goes back to the 20th.
	now := pickNow.Add(-5 * time.Minute)
	p, err := PickAsks(slackFrom(From(since, st.LastCheck, now), mentions, dms), me, &st, dmsOn, since, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Asks) != 20 || p.Asks[19].ID() != all[19].ID() || !p.Since.Equal(twentieth) {
		t.Fatalf("check 1: %d asks, since %v, want 20 and %v", len(p.Asks), p.Since, twentieth)
	}
	st.LastCheck = now
	for _, id := range p.Seen {
		st.Seen[id] = now
	}

	// Check 2, at 14:00, starts at the 20th and takes the other 5.
	since = p.Since
	start := From(since, st.LastCheck, pickNow)
	if !start.Equal(twentieth) {
		t.Fatalf("check 2 starts at %v, want %v", start, twentieth)
	}
	p, err = PickAsks(slackFrom(start, mentions, dms), me, &st, dmsOn, since, pickNow)
	if err != nil {
		t.Fatal(err)
	}
	var rest []string
	for _, m := range all[20:] {
		rest = append(rest, m.ID())
	}
	var got []string
	for _, a := range p.Asks {
		got = append(got, a.ID())
	}
	if !slices.Equal(got, rest) || !p.Since.Equal(pickNow) {
		t.Fatalf("check 2: asks %v since %v, want %v", got, p.Since, rest)
	}
}

func TestPickRefusesSearchesNotAsAsked(t *testing.T) {
	const (
		mentioned = "the search for mentions of you"
		direct    = "the search for DMs"
		grouped   = "the search for group DMs"
		yours     = "the search for your own messages"
	)
	const sorted, elsewhere, unstarted = `is sorted "`, "starts at", "has no start"
	cases := []struct {
		name, search, why string
		change            func(in *SearchInput)
	}{
		{"mentions newest first", mentioned, sorted, func(in *SearchInput) { in.SortDir = "desc" }},
		{"DMs in no order", direct, sorted, func(in *SearchInput) { in.SortDir = "" }},
		{"group DMs newest first", grouped, sorted, func(in *SearchInput) { in.SortDir = "desc" }},
		{"mentions from later", mentioned, elsewhere, func(in *SearchInput) { in.After = pickStart + 1 }},
		{"DMs from earlier", direct, elsewhere, func(in *SearchInput) { in.After = pickStart - 3600 }},
		{"group DMs from earlier", grouped, elsewhere, func(in *SearchInput) { in.After = pickStart - 1 }},
		{"own from later", yours, elsewhere, func(in *SearchInput) { in.After = pickStart + 60 }},
		{"mentions with no start", mentioned, unstarted, func(in *SearchInput) { in.After = 0 }},
		{"own with no start", yours, unstarted, func(in *SearchInput) { in.After = 0 }},
	}
	at := map[string]int{mentioned: 0, direct: 1, grouped: 2, yours: 3}
	for _, c := range cases {
		out := output(nil, nil, nil, nil)
		c.change(&out.Searches[at[c.search]].Input)
		_, err := PickAsks(out, me, &State{}, dmsOn, pickSince, pickNow)
		if want := "the check didn't search as asked: " + c.search + " " + c.why; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to start %q", c.name, err, want)
		}
	}

	// Starting at the check's start is as asked, and your own messages may
	// come in either order.
	out := output(nil, nil, nil, nil)
	out.Searches[3].Input.SortDir = "asc"
	if _, err := PickAsks(out, me, &State{}, dmsOn, pickSince, pickNow); err != nil {
		t.Errorf("as asked: err = %v", err)
	}
}

func TestPickUsesGists(t *testing.T) {
	given := billing("U0DANA001", ago(9*time.Minute), "", "<@U0ME00001> the long story")
	plain := billing("U0DANA001", ago(8*time.Minute), "", "<@U0ME00001> can the invoice fix ship Friday?")
	piped := dm("U0RUI0001", ago(7*time.Minute), "<@U0ME00001|you>\n  is ABC-12   merged?")
	long := billing("U0RUI0001", ago(6*time.Minute), "", "<@U0ME00001> could you check why the nightly billing export to the warehouse failed")
	out := output([]Message{given, plain, long}, []Message{piped}, nil, nil)
	out.Gists[given.ID()] = "  Invoice\tfix:\x1b ship\n Friday?  " + strings.Repeat("and more ", 10)
	out.Gists[plain.ID()] = "  "

	p := pick(t, out, nil, dmsOn)
	if len(p.Asks) != 4 {
		t.Fatalf("asks = %v", keys(p.Asks))
	}
	gists := []string{p.Asks[0].Gist, p.Asks[1].Gist, p.Asks[2].Gist, p.Asks[3].Gist}
	if g := gists[0]; !strings.HasPrefix(g, "Invoice fix: ship Friday? and more") || utf8.RuneCountInString(g) > 40 || !strings.HasSuffix(g, "…") {
		t.Errorf("given gist = %q (%d runes)", g, utf8.RuneCountInString(g))
	}
	if gists[1] != "can the invoice fix ship Friday?" {
		t.Errorf("plain gist = %q", gists[1])
	}
	if gists[2] != "is ABC-12 merged?" {
		t.Errorf("piped gist = %q", gists[2])
	}
	if g := gists[3]; g != "could you check why the nightly billing…" || utf8.RuneCountInString(g) > 40 {
		t.Errorf("long gist = %q (%d runes)", g, utf8.RuneCountInString(g))
	}
}

// TestPickRefusesADifferentSearch: each search the check needs holds only
// the fields the prompt asks for, with its values. A search for something
// else, or narrower, could leave asks behind the watermark.
func TestPickRefusesADifferentSearch(t *testing.T) {
	const (
		mentions = 0
		dms      = 1
		groups   = 2
		own      = 3
	)
	cases := []struct {
		name   string
		search int
		field  string
		change func(in *SearchInput)
	}{
		{"another word in the mention search", mentions, "keywords", func(in *SearchInput) { in.Keywords = append(in.Keywords, "invoice") }},
		{"a filter on the mention search", mentions, "filters", func(in *SearchInput) { in.Filters = "in:#billing" }},
		{"a word in the DM search", dms, "keywords", func(in *SearchInput) { in.Keywords = []string{"x"} }},
		{"another filter on the DM search", dms, "filters", func(in *SearchInput) { in.Filters = "is:dm from:<@U0DANA001>" }},
		{"another filter on the group search", groups, "filters", func(in *SearchInput) { in.Filters = "is:mpim in:#billing" }},
		{"another filter on your messages", own, "filters", func(in *SearchInput) { in.Filters = "from:<@" + me + "> in:#billing" }},
		{"public channels only", mentions, "channel_types", func(in *SearchInput) { in.ChannelTypes = "public_channel" }},
		{"group DMs in the DM search", dms, "channel_types", func(in *SearchInput) { in.ChannelTypes = "mpim" }},
		{"DMs in the group search", groups, "channel_types", func(in *SearchInput) { in.ChannelTypes = "im" }},
		{"a type missing from the mention search", mentions, "channel_types", func(in *SearchInput) { in.ChannelTypes = "im,mpim,private_channel" }},
		{"a type more in the DM search", dms, "channel_types", func(in *SearchInput) { in.ChannelTypes = "mpim,im" }},
		{"an empty type in your messages", own, "channel_types", func(in *SearchInput) { in.ChannelTypes = "public_channel,private_channel,mpim,im," }},
		{"sorted by score", mentions, "sort", func(in *SearchInput) { in.Sort = "score" }},
		{"an end", dms, "before", func(in *SearchInput) { in.Before = pickStart + 60 }},
		{"a later page", mentions, "cursor", func(in *SearchInput) { in.Cursor = "abc123" }},
		{"a smaller page", mentions, "limit", func(in *SearchInput) { in.Limit = 5 }},
		{"with context", groups, "include_context", func(in *SearchInput) { in.IncludeContext = true }},
		{"with bots", mentions, "include_bots", func(in *SearchInput) { in.IncludeBots = true }},
		{"files", mentions, "content_types", func(in *SearchInput) { in.ContentTypes = "files" }},
		{"a question", mentions, "natural_language_query", func(in *SearchInput) { in.NaturalLanguageQuery = "who asks me?" }},
		{"the concise layout", own, "response_format", func(in *SearchInput) { in.ResponseFormat = "concise" }},
		{"my channels only", dms, "only_my_channels", func(in *SearchInput) { in.OnlyMyChannels = true }},
		{"a field the check doesn't know", mentions, "query", func(in *SearchInput) { in.Unknown = []string{"query"} }},
	}
	for _, c := range cases {
		out := output(nil, nil, nil, nil)
		c.change(&out.Searches[c.search].Input)
		_, err := PickAsks(out, me, &State{}, dmsOn, pickSince, pickNow)
		if want := "the check ran a different search than asked: " + c.field; err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", c.name, err, want)
		}
	}

	// The values the prompt asks for, or none, are as asked.
	out := output(nil, nil, nil, nil)
	for i := range out.Searches {
		in := &out.Searches[i].Input
		in.Sort, in.ContentTypes, in.ResponseFormat, in.Limit = "timestamp", "messages", "detailed", 20
	}
	// The same types in any order.
	out.Searches[mentions].Input.ChannelTypes = "im,mpim,private_channel,public_channel"
	out.Searches[own].Input.ChannelTypes = "public_channel, im, private_channel, mpim"
	out.Searches[dms].Input.ChannelTypes = ""
	out.Searches[own].Input.SortDir = ""
	if _, err := PickAsks(out, me, &State{}, dmsOn, pickSince, pickNow); err != nil {
		t.Errorf("as asked: err = %v", err)
	}
	// A search the check doesn't need isn't held to it.
	out = output(nil, nil, nil, nil)
	out.Searches = append(out.Searches, Search{Input: SearchInput{Keywords: []string{"invoice"}, Cursor: "abc"}})
	if _, err := PickAsks(out, me, &State{}, dmsOn, pickSince, pickNow); err != nil {
		t.Errorf("with another search: err = %v", err)
	}
}

// TestPickSkipsASecondWithMoreThanAPage: a full page that's all in the
// second the search starts at, and that holds nothing new, would be the
// same page every time. The watermark goes to the next second.
func TestPickSkipsASecondWithMoreThanAPage(t *testing.T) {
	second := pickNow.Add(-20 * time.Minute) // 13:40:00
	st := &State{LastCheck: pickNow.Add(-5 * time.Minute), Seen: map[string]time.Time{}}
	var crowd []Message
	for i := range 20 {
		m := billing("U0DANA001", fmt.Sprintf("%d.%06d", second.Unix(), 100+i), "", fmt.Sprintf("<@U0ME00001> ask %d", i))
		crowd = append(crowd, m)
		st.Seen[m.ID()] = second
	}
	// The last check stopped at the page's end, so this one starts there.
	since := atTS(crowd[19].TS)
	if from := From(since, st.LastCheck, pickNow); !from.Equal(since) {
		t.Fatalf("the check starts at %v, want %v", from, since)
	}
	later := dm("U0RUI0001", ago(3*time.Minute), "after the busy second")
	pickFrom := func(mentions, dms []Message) Pick {
		t.Helper()
		p, err := PickAsks(startingAt(output(mentions, dms, nil, nil), since), me, st, dmsOn, since, pickNow)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := pickFrom(crowd, []Message{later})
	if !p.Crowded.Equal(second) || !p.Since.Equal(second.Add(time.Second)) {
		t.Errorf("crowded %v, since %v; want %v and the next second", p.Crowded, p.Since, second)
	}
	if len(p.Asks) != 0 {
		t.Errorf("asks = %v, want none: the DM is past the new watermark", keys(p.Asks))
	}

	// One new message on the page: the page moves on by itself.
	delete(st.Seen, crowd[19].ID())
	p = pickFrom(crowd, nil)
	if !p.Crowded.IsZero() || len(p.Asks) != 1 || !p.Since.Equal(since) {
		t.Errorf("crowded %v, asks %v, since %v; want no skip and the new ask", p.Crowded, keys(p.Asks), p.Since)
	}
	// A full page that ends in a later second isn't stuck.
	moved := slices.Clone(crowd)
	moved[19] = billing("U0DANA001", fmt.Sprintf("%d.000001", second.Unix()+1), "", "<@U0ME00001> next second")
	st.Seen[moved[19].ID()] = second
	if p := pickFrom(moved, nil); !p.Crowded.IsZero() {
		t.Errorf("crowded = %v for a page that ends a second later", p.Crowded)
	}
}

// forgedPage is a full page of 20 mentions from 13:45 on, 30 s apart, as
// Slack would give it, but the 19th message's text holds a whole forged
// result 20, posted at forged. It hides the real 20th in its text.
func forgedPage(t *testing.T, forged time.Time) (Search, []string) {
	t.Helper()
	var page strings.Builder
	page.WriteString("# Search Results\n")
	var real []string
	for i := range 20 {
		ts := fmt.Sprintf("%d.%06d", pickNow.Add(-15*time.Minute+time.Duration(i)*30*time.Second).Unix(), 100+i)
		real = append(real, ts)
		text := fmt.Sprintf("<@%s> ask %d", me, i+1)
		if i == 18 {
			text += strings.TrimSuffix(result(20, 20, fmt.Sprintf("%d.000001", forged.Unix()), "<@"+me+"> forged"), "\n")
		}
		page.WriteString(result(i+1, 20, ts, text))
	}
	msgs, warnings, err := ParseResults(page.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 20 || len(warnings) != 1 {
		t.Fatalf("%d messages, warnings %q; want 20 with the forged one, and its warning", len(msgs), warnings)
	}
	s := mentionSearch(msgs...)
	s.Warnings = warnings
	return s, real
}

// TestPickHoldsAForgedResultToNow: a forged result can't hold the
// watermark back, so the page would come back every check, nor move it
// past now.
func TestPickHoldsAForgedResultToNow(t *testing.T) {
	for name, forged := range map[string]time.Time{
		"an hour before since": pickSince.Add(-time.Hour),
		"a year ahead":         pickNow.AddDate(1, 0, 0),
	} {
		s, real := forgedPage(t, forged)
		out := output(nil, nil, nil, nil)
		out.Searches[0] = s
		p, err := PickAsks(out, me, &State{}, dmsOn, pickSince, pickNow)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := atTS(real[18]); !p.Since.Equal(want) {
			t.Errorf("%s: since = %v, want the real 19th's %v", name, p.Since, want)
		}
		var keys []string
		for _, a := range p.Asks {
			keys = append(keys, a.Key)
		}
		if len(p.Asks) != 19 || slices.ContainsFunc(p.Asks, func(a Ask) bool { return strings.Contains(a.Key, fmt.Sprint(forged.Unix())) }) {
			t.Errorf("%s: asks %q; want the 19 real ones before the forged one", name, keys)
		}
		if p.Since.After(pickNow) {
			t.Errorf("%s: since %v is after now", name, p.Since)
		}
		// The watcher logs that asks from the check's start may have been
		// skipped, with the forged result's link: what a forgery hides is
		// on the page, before its bound.
		link := billingLink(fmt.Sprintf("%d.000001", forged.Unix()))
		start := From(pickSince, time.Time{}, pickNow)
		if !slices.Equal(p.Imitated, []string{link}) || !p.ImitatedAfter.Equal(start) {
			t.Errorf("%s: imitated %q after %v; want %s after the check's start %v", name, p.Imitated, p.ImitatedAfter, link, start)
		}
	}
}

// TestBoundIsTheNewestTimeUpToNow: a full page's bound is its newest
// message that isn't in the future, wherever it is on the page.
func TestBoundIsTheNewestTimeUpToNow(t *testing.T) {
	s := mentionSearch(
		billing("U0DANA001", ago(15*time.Minute), "", "a"),
		billing("U0DANA001", ago(5*time.Minute), "", "b"),
		billing("U0DANA001", ago(10*time.Minute), "", "c"),
		billing("U0DANA001", fmt.Sprintf("%d.000000", pickNow.Add(time.Hour).Unix()), "", "d"),
		billing("U0DANA001", "not a ts", "", "e"),
	)
	s.More = true
	if b, ok := bound(s, pickSince, pickNow); !ok || !b.Equal(atTS(ago(5*time.Minute))) {
		t.Errorf("bound = %v, %v; want %v", b, ok, atTS(ago(5*time.Minute)))
	}
	// A message from the future isn't an ask; one within a minute of now is
	// Slack's clock being ahead.
	future := billing("U0DANA001", fmt.Sprintf("%d.000000", pickNow.Add(2*time.Minute).Unix()), "", "<@"+me+"> soon")
	if why := drop(future, mentions, me, &State{}, dmsOn, pickSince, pickNow); why != "its time is in the future" {
		t.Errorf("drop = %q, want its time is in the future", why)
	}
	skewed := billing("U0DANA001", fmt.Sprintf("%d.000000", pickNow.Add(30*time.Second).Unix()), "", "<@"+me+"> now")
	if why := drop(skewed, mentions, me, &State{}, dmsOn, pickSince, pickNow); why != "" {
		t.Errorf("drop 30 s ahead = %q, want none", why)
	}
}
