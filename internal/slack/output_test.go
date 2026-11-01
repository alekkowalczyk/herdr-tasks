package slack

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseOutputReadsSearchesAndGists(t *testing.T) {
	out, err := ParseOutput(fixture(t, "check.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Connector != "connected" || out.Cost != 0.0079 || out.Failed != "" {
		t.Fatalf("connector %q cost %v failed %q", out.Connector, out.Cost, out.Failed)
	}
	if len(out.Searches) != 4 {
		t.Fatalf("searches = %d", len(out.Searches))
	}
	in := out.Searches[0].Input
	if !slices.Equal(in.Keywords, []string{"<@U0ME00001>"}) || in.After != 1793200000 || in.Limit != 20 || in.SortDir != "asc" {
		t.Fatalf("mention input = %+v", in)
	}
	if f := out.Searches[1].Input; f.Filters != "is:dm" || f.ChannelTypes != "im" {
		t.Fatalf("dm input = %+v", f)
	}
	if out.Searches[2].Input.Filters != "is:mpim" || out.Searches[3].Input.Filters != "from:<@U0ME00001>" {
		t.Fatalf("filters = %q, %q", out.Searches[2].Input.Filters, out.Searches[3].Input.Filters)
	}
	mention := out.Searches[0]
	if !mention.More || mention.Err != "" || len(mention.Messages) != 2 {
		t.Fatalf("mention search = %+v", mention)
	}
	want := Message{Channel: "C0123ABCD", Place: "#billing", AuthorID: "U0DANA001", Author: "Dana Reed",
		TS: "1793454730.123456", ThreadTS: "1793450000.000100",
		Permalink: "https://acme.slack.com/archives/C0123ABCD/p1793454730123456?thread_ts=1793450000.000100&cid=C0123ABCD",
		Text:      "<@U0ME00001> can the invoice fix ship Friday?"}
	got := mention.Messages[0]
	if got.Channel != want.Channel || got.Place != want.Place || got.AuthorID != want.AuthorID || got.Author != want.Author ||
		got.TS != want.TS || got.ThreadTS != want.ThreadTS || got.Permalink != want.Permalink || got.Text != want.Text {
		t.Fatalf("message = %+v", got)
	}
	if got.ID() != "C0123ABCD/1793454730.123456" {
		t.Fatalf("ID = %q", got.ID())
	}
	if out.Gists[got.ID()] != "Invoice fix: ship Friday?" || len(out.Gists) != 1 {
		t.Fatalf("gists = %v", out.Gists)
	}
	if second := mention.Messages[1]; !second.Bot || second.ThreadTS != "" || second.AuthorID != "B0BOT0001" || second.Author != "Deploys" {
		t.Fatalf("bot message = %+v", second)
	}
	if out.Searches[1].More || out.Searches[1].Messages[0].Channel != "D0RUI0001" {
		t.Fatalf("dm search = %+v", out.Searches[1])
	}
}

// TestParseOutputReadsAResultWithOtherStructuredOutput: a structured answer
// that isn't the gists' shape still ends the run, with its cost; it only
// gives no gists.
func TestParseOutputReadsAResultWithOtherStructuredOutput(t *testing.T) {
	const init = `{"type":"system","subtype":"init","mcp_servers":[{"name":"plugin:slack:slack","status":"connected"}]}` + "\n"
	for _, answer := range []string{`"not gists"`, `{"gists":"none"}`, `{"gists":[{"channel":1,"ts":"1793454730.123456","gist":"x"}]}`, `[1,2]`} {
		out, err := ParseOutput([]byte(init + `{"type":"result","subtype":"success","is_error":false,"result":"done","total_cost_usd":0.0079,"structured_output":` + answer + "}\n"))
		if err != nil {
			t.Fatal(err)
		}
		if !out.Done || out.Cost != 0.0079 || out.Failed != "" || len(out.Gists) != 0 || string(out.Structured) != answer {
			t.Errorf("%s: done %v, cost %v, failed %q, gists %v, structured %s", answer, out.Done, out.Cost, out.Failed, out.Gists, out.Structured)
		}
	}
	out, err := ParseOutput([]byte(init + `{"type":"result","subtype":"success","is_error":false,"structured_output":null}` + "\n"))
	if err != nil || !out.Done || out.Structured != nil {
		t.Errorf("a null answer: done %v, structured %s, %v", out.Done, out.Structured, err)
	}
}

func TestParseOutputReadsTheProfile(t *testing.T) {
	out, err := ParseOutput(fixture(t, "profile.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if out.UserID != "U0ME00001" || len(out.Searches) != 0 {
		t.Fatalf("out = %+v", out)
	}
}

func TestParseOutputSaysWhenSlackNeedsSignIn(t *testing.T) {
	out, err := ParseOutput(fixture(t, "signin.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Connector != "needs-auth" {
		t.Fatalf("connector = %q", out.Connector)
	}
}

func TestParseOutputWithoutSlackHasNoConnector(t *testing.T) {
	out, err := ParseOutput([]byte("warning: something\n" + `{"type":"system","subtype":"init","mcp_servers":[{"name":"other","status":"connected"}]}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Connector != "" {
		t.Fatalf("connector = %q", out.Connector)
	}
}

func TestParseOutputFailedRuns(t *testing.T) {
	out, err := ParseOutput([]byte(`{"type":"result","subtype":"error_max_budget_usd","is_error":false,"total_cost_usd":0.05}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Failed, "budget") || out.Cost != 0.05 {
		t.Fatalf("failed = %q cost %v", out.Failed, out.Cost)
	}
	out, err = ParseOutput([]byte(`{"type":"result","subtype":"success","is_error":true,"result":"Credit balance is too low\nplease top up"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Failed != "Credit balance is too low" {
		t.Fatalf("failed = %q", out.Failed)
	}
}

const dmResults = "# Search Results\n\n### Result 1 of 2\nChannel: DM (ID: D0RUI0001)\nParticipants: Rui Costa (ID: U0RUI0001), You (ID: U0ME00001)\n" +
	"From: Rui Costa <rui@acme.test> (ID: U0RUI0001)\nTime: 2026-10-31 14:32:10 CET\nMessage_ts: 1793455000.000300\n" +
	"Permalink: [link](https://acme.slack.com/archives/D0RUI0001/p1793455000000300)\nText: Line one\nline two\n\nline four\n\n---\n\n" +
	"### Result 2 of 2\nChannel: #deploys (ID: C0DEPLOY1)\nFrom: Deploys (ID: B0BOT0001)  [bot]\nTime: 2026-10-31 14:40:00 CET\nMessage_ts: 1793455600.000400\n" +
	"Permalink: [link](https://acme.slack.com/archives/C0DEPLOY1/p1793455600000400)\nText: deploy ok\n\n---\n"

func TestParseResultsDMsBotsAndMultilineText(t *testing.T) {
	msgs, _, err := ParseResults(dmResults)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d", len(msgs))
	}
	dm := msgs[0]
	if !slices.Equal(dm.Participants, []string{"U0RUI0001", "U0ME00001"}) || dm.Place != "DM" || dm.Bot {
		t.Fatalf("dm = %+v", dm)
	}
	if dm.Text != "Line one\nline two\n\nline four" {
		t.Fatalf("text = %q", dm.Text)
	}
	if !msgs[1].Bot || msgs[1].Author != "Deploys" || msgs[1].Text != "deploy ok" || len(msgs[1].Participants) != 0 {
		t.Fatalf("bot = %+v", msgs[1])
	}
}

func TestParseResultsRefusesUnknownLayout(t *testing.T) {
	const refuse = "can't read Slack's search results"
	bad := map[string]string{
		"unknown fields":      "### Result 1 of 1\nSomething: else",
		"no permalink":        "### Result 1 of 1\nChannel: #billing (ID: C0123ABCD)\nFrom: Dana Reed (ID: U0DANA001)\nMessage_ts: 1793454730.123456\nText: hi\n\n---\n",
		"no channel":          "### Result 1 of 1\nFrom: Dana Reed (ID: U0DANA001)\nMessage_ts: 1793454730.123456\nPermalink: [link](https://acme.slack.com/archives/C0123ABCD/p1)\nText: hi\n",
		"labels but no block": "# Search Results\n\nMessage_ts: 1793454730.123456\nFrom: Dana Reed (ID: U0DANA001)\n",
		"permalink only":      "Results:\nPermalink: [link](https://acme.slack.com/archives/C0123ABCD/p1)\n",
	}
	for name, text := range bad {
		msgs, _, err := ParseResults(text)
		if err == nil || !strings.Contains(err.Error(), refuse) || len(msgs) != 0 {
			t.Errorf("%s: msgs %v err %v", name, msgs, err)
		}
	}
	for _, text := range []string{"", "# Search Results\n\nNo results found.\n", "# Search Results\n\n"} {
		msgs, _, err := ParseResults(text)
		if err != nil || len(msgs) != 0 {
			t.Errorf("%q: msgs %v err %v", text, msgs, err)
		}
	}
}

func TestParseOutputToolErrors(t *testing.T) {
	line := func(s string) string { return s + "\n" }
	use := func(id string) string {
		return line(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"mcp__plugin_slack_slack__slack_search_public_and_private","input":{"keywords":["x"]}}]}}`)
	}
	result := func(id, rest string) string {
		return line(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `",` + rest + `}]}}`)
	}
	in := use("a") + result("a", `"is_error":true,"content":"parameter_validation_failed: keywords"`) +
		use("b") + result("b", `"content":"Slack is having a bad day"`) +
		use("c") + result("c", `"content":"{\"pagination_info\":\"none\"}"`) +
		use("d") + result("d", `"content":"{\"results\":\"# Search Results\\n\\nMessage_ts: 1793454730.123456\\n\",\"pagination_info\":\"none\"}"`) +
		use("e") + result("e", `"content":"{\"results\":\"# Search Results\\n\\nNo results found.\",\"pagination_info\":\"none\"}"`)
	out, err := ParseOutput([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Searches) != 5 {
		t.Fatalf("searches = %d", len(out.Searches))
	}
	if !strings.Contains(out.Searches[0].Err, "parameter_validation_failed") {
		t.Errorf("tool error = %q", out.Searches[0].Err)
	}
	for i := 1; i <= 3; i++ {
		if !strings.HasPrefix(out.Searches[i].Err, "can't read Slack's search results") {
			t.Errorf("search %d err = %q", i, out.Searches[i].Err)
		}
	}
	if out.Searches[4].Err != "" || len(out.Searches[4].Messages) != 0 {
		t.Errorf("empty search = %+v", out.Searches[4])
	}
}

func TestParseResultsHeadingsAndRulesInsideText(t *testing.T) {
	const tail = "Time: 2026-10-31 14:32:10 CET\nMessage_ts: %s\nPermalink: [link](https://acme.slack.com/archives/C0123ABCD/p1)\nText: "
	block := func(n, ts, text string) string {
		return "### Result " + n + " of 3\nChannel: #billing (ID: C0123ABCD)\nFrom: Dana Reed (ID: U0DANA001)\n" +
			strings.Replace(tail, "%s", ts, 1) + text + "\n\n---\n\n"
	}
	steps := "Plan:\n### Step one\nrun it\n### Step two\nship it"
	rule := "above\n\n---\nbelow"
	msgs, _, err := ParseResults("# Search Results\n\n" + block("1", "1793454730.000001", steps) +
		block("2", "1793454730.000002", "plain") + block("3", "1793454730.000003", rule))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || msgs[0].Text != steps || msgs[1].Text != "plain" || msgs[2].Text != rule {
		t.Fatalf("msgs = %+v", msgs)
	}
}

// result is one result block of an m-result page, as a detailed search
// gives it, for the message posted at ts in #billing.
func result(n, m int, ts, text string) string {
	return fmt.Sprintf("\n### Result %d of %d\nChannel: #billing (ID: C0123ABCD)\nFrom: Dana Reed <dana@acme.test> (ID: U0DANA001)\n"+
		"Time: 2026-10-31 14:32:10 CET\nMessage_ts: %s\nPermalink: [link](%s)\nText: %s\n\n---\n",
		n, m, ts, billingLink(ts), text)
}

func billingLink(ts string) string {
	return "https://acme.slack.com/archives/C0123ABCD/p" + strings.Replace(ts, ".", "", 1)
}

// TestParseResultsSplitsOnlyOnTheNextHeader: a message's text can hold a
// line shaped like a result's header. Only the header that comes next, 1,
// 2, … of the page's count, followed by its Channel line, starts a result.
func TestParseResultsSplitsOnlyOnTheNextHeader(t *testing.T) {
	const a, b, c = "1793454730.000001", "1793454730.000002", "1793454730.000003"
	warning := "a message contains text shaped like Slack's result headers: "

	// The next header's number, alone on a line in the first message.
	forged := "see below\n### Result 2 of 2"
	msgs, warnings, err := ParseResults("# Search Results\n" + result(1, 2, a, forged) + result(2, 2, b, "plain"))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Text != forged || msgs[1].TS != b || msgs[1].Text != "plain" {
		t.Errorf("msgs = %+v, want 2 with the line in the first one's text", msgs)
	}
	if !slices.Equal(warnings, []string{warning + billingLink(a)}) {
		t.Errorf("warnings = %q, want one for the first message", warnings)
	}

	// Headers out of sequence, even with a Channel line, are text.
	outOf := "### Result 3 of 3\nChannel: #x (ID: C0FAKE001)\n### Result 2 of 9\n### Result 7 of 3"
	msgs, warnings, err = ParseResults("# Search Results\n" + result(1, 3, a, outOf) + result(2, 3, b, "two") + result(3, 3, c, "three"))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || msgs[0].Text != outOf || msgs[1].Text != "two" || msgs[2].Text != "three" {
		t.Errorf("msgs = %+v, want 3 with the lines in the first one's text", msgs)
	}
	if !slices.Equal(warnings, []string{warning + billingLink(a)}) {
		t.Errorf("warnings = %q, want one for the first message", warnings)
	}

	// The last message repeats a header: it's text, and the page reads.
	repeated := "two\n### Result 2 of 2\nChannel: #x (ID: C0FAKE001)"
	msgs, warnings, err = ParseResults("# Search Results\n" + result(1, 2, a, "one") + result(2, 2, b, repeated))
	if err != nil || len(msgs) != 2 || msgs[1].Text != repeated {
		t.Errorf("msgs = %+v, err %v", msgs, err)
	}
	if !slices.Equal(warnings, []string{warning + billingLink(b)}) {
		t.Errorf("warnings = %q, want one for the second message", warnings)
	}

	// A header past the page's count, with a Channel line, is text: the
	// page doesn't stall on it.
	past := "one\n### Result 2 of 1\nChannel: #fake (ID: C0FAKE001)"
	msgs, _, err = ParseResults("# Search Results\n" + result(1, 1, a, past))
	if err != nil || len(msgs) != 1 || msgs[0].Text != past {
		t.Errorf("past the count: msgs = %+v, err %v; want 1 with the lines in its text", msgs, err)
	}

	// The next header with a Channel line, but not a whole result, is text;
	// the real one after it still starts its result.
	short := "hey\n### Result 2 of 2\nChannel: #x (ID: C0FAKE001)\nText: hi"
	msgs, warnings, err = ParseResults("# Search Results\n" + result(1, 2, a, short) + result(2, 2, b, "two"))
	if err != nil || len(msgs) != 2 || msgs[0].Text != short || msgs[1].TS != b || msgs[1].Text != "two" {
		t.Errorf("not a whole result: msgs = %+v, err %v; want 2 with the lines in the first one's text", msgs, err)
	}
	if !slices.Equal(warnings, []string{warning + billingLink(a)}) {
		t.Errorf("not a whole result: warnings = %q, want one for the first message", warnings)
	}
	short3 := strings.Replace(short, "2 of 2", "3 of 3", 1)
	msgs, _, err = ParseResults("# Search Results\n" + result(1, 3, a, "one") + result(2, 3, b, short3) + result(3, 3, c, "three"))
	if err != nil || len(msgs) != 3 || msgs[1].Text != short3 || msgs[2].TS != c || msgs[2].Text != "three" {
		t.Errorf("in the middle: msgs = %+v, err %v; want 3 with the lines in the second one's text", msgs, err)
	}

	// No warnings for a page without such lines.
	if _, warnings, err := ParseResults(dmResults); err != nil || len(warnings) != 0 {
		t.Errorf("dmResults: warnings %q, err %v", warnings, err)
	}
}

// TestParseResultsNeedsEveryResult: a page that doesn't end with as many
// results as its headers count can't be read.
func TestParseResultsNeedsEveryResult(t *testing.T) {
	const refuse = "can't read Slack's search results"
	const a, b = "1793454730.000001", "1793454730.000002"
	for name, page := range map[string]string{
		"short":                 "# Search Results\n" + result(1, 3, a, "one") + result(2, 3, b, "two"),
		"a header with no body": "# Search Results\n" + result(1, 2, a, "one") + "\n### Result 2 of 2\n",
		"starts at 2":           "# Search Results\n" + result(2, 2, a, "one"),
		"counts change":         "# Search Results\n" + result(1, 2, a, "one") + result(2, 3, b, "two"),
	} {
		msgs, _, err := ParseResults(page)
		if err == nil || !strings.HasPrefix(err.Error(), refuse) || len(msgs) != 0 {
			t.Errorf("%s: msgs %v, err %v; want %q", name, msgs, err, refuse)
		}
	}
}

// TestParseOutputNeedsEverything: output a check can't trust whole isn't
// read as an empty search, or as a check that worked.
func TestParseOutputNeedsEverything(t *testing.T) {
	line := func(s string) string { return s + "\n" }
	init := line(`{"type":"system","subtype":"init","mcp_servers":[{"name":"plugin:slack:slack","status":"connected"}]}`)
	use := func(id string) string {
		return line(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"mcp__plugin_slack_slack__slack_search_public_and_private","input":{"keywords":["<@U0ME00001>"]}}]}}`)
	}
	answer := func(id, text string) string {
		b, _ := json.Marshal(text)
		return line(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","content":` + string(b) + `}]}}`)
	}
	done := line(`{"type":"result","subtype":"success","is_error":false,"result":"ok","total_cost_usd":0.001}`)

	out, err := ParseOutput([]byte(init + use("a") + answer("a", `{"results":null,"pagination_info":""}`) +
		use("b") + answer("b", `{"results":42}`) + use("c") + done))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Searches) != 3 {
		t.Fatalf("searches = %+v", out.Searches)
	}
	for i, want := range []string{"can't read Slack's search results", "can't read Slack's search results", "the search got no answer"} {
		if s := out.Searches[i]; !strings.HasPrefix(s.Err, want) || len(s.Messages) != 0 {
			t.Errorf("search %d = %+v, want the error %q", i, s, want)
		}
	}
	if !out.Init || !out.Done || out.Failed != "" {
		t.Errorf("init %v, done %v, failed %q; want init, done, not failed", out.Init, out.Done, out.Failed)
	}

	// No result event, or one before the run went on.
	for name, in := range map[string]string{
		"no result":           init + use("a") + answer("a", `{"results":"","pagination_info":""}`),
		"more after a result": init + done + use("a") + answer("a", `{"results":"","pagination_info":""}`),
	} {
		out, err := ParseOutput([]byte(in))
		if err != nil || out.Done {
			t.Errorf("%s: done %v, err %v; want not done", name, out.Done, err)
		}
	}
	// A result that doesn't say it worked.
	out, _ = ParseOutput([]byte(init + line(`{"type":"result","is_error":false,"result":"ok"}`)))
	if !out.Done || out.Failed == "" {
		t.Errorf("a result with no subtype: done %v, failed %q; want a failure", out.Done, out.Failed)
	}
	// Without an init event.
	out, _ = ParseOutput([]byte(done))
	if out.Init {
		t.Error("init = true with no init event")
	}
}

// TestParseOutputKeepsWhatASearchAskedFor: every field of a search's input
// is kept, and one the check doesn't know, or of the wrong type, is noted.
func TestParseOutputKeepsWhatASearchAskedFor(t *testing.T) {
	use := func(input string) Output {
		out, err := ParseOutput([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a","name":"mcp__plugin_slack_slack__slack_search_public_and_private","input":` + input + `}]}}` + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	in := use(`{"keywords":["<@U0ME00001>"],"filters":"","channel_types":"im","content_types":"messages","after":1793200000,"before":1793300000,` +
		`"sort":"timestamp","sort_dir":"asc","limit":20,"include_context":true,"include_bots":true,"natural_language_query":"asks",` +
		`"cursor":"abc","response_format":"detailed","only_my_channels":true}`).Searches[0].Input
	want := SearchInput{Keywords: []string{"<@U0ME00001>"}, ChannelTypes: "im", ContentTypes: "messages", After: 1793200000, Before: 1793300000,
		Sort: "timestamp", SortDir: "asc", Limit: 20, IncludeContext: true, IncludeBots: true, NaturalLanguageQuery: "asks",
		Cursor: "abc", ResponseFormat: "detailed", OnlyMyChannels: true}
	if !reflect.DeepEqual(in, want) {
		t.Errorf("input = %+v\nwant    %+v", in, want)
	}
	if got := use(`{"keywords":[],"query":"x","limit":"20"}`).Searches[0].Input.Unknown; !slices.Equal(got, []string{"limit", "query"}) {
		t.Errorf("unknown = %q, want limit and query", got)
	}
}
