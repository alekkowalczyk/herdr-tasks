package slack

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// Output is what a check's stream-json output says happened.
type Output struct {
	Connector string            // the Slack MCP server's status from the init event: "connected", "needs-auth", … ; "" when there's none
	UserID    string            // from a slack_read_user_profile result, if the run read it
	Searches  []Search          // every slack_search_public_and_private call, in order
	Gists     map[string]string // message ID → gist, from structured_output.gists
	Cost      float64
	Failed    string // why the run failed: a result with is_error, or a subtype other than "success"; "" when it worked
	Init      bool   // the run started: it has an init event
	Done      bool   // the run ended: a result event comes after everything else

	// Structured is the result's structured_output as it is, or nil when
	// there's none.
	Structured json.RawMessage
}

// Search is one slack_search_public_and_private call and what came back.
type Search struct {
	Input    SearchInput
	Messages []Message
	More     bool   // pagination_info offers another page
	Err      string // the tool's error text, or why its answer can't be read
	// Warnings are lines for slack.log about the results, which name no
	// message's text.
	Warnings []string
}

// SearchInput is a search call's input: every field the connector takes, so
// the picker can tell what the search covered, and that it's the search the
// check asked for.
type SearchInput struct {
	Keywords             []string `json:"keywords"`
	Filters              string   `json:"filters"`
	ChannelTypes         string   `json:"channel_types"`
	ContentTypes         string   `json:"content_types"`
	After                int64    `json:"after"`
	Before               int64    `json:"before"`
	Sort                 string   `json:"sort"`
	SortDir              string   `json:"sort_dir"`
	Limit                int      `json:"limit"`
	IncludeContext       bool     `json:"include_context"`
	IncludeBots          bool     `json:"include_bots"`
	NaturalLanguageQuery string   `json:"natural_language_query"`
	Cursor               string   `json:"cursor"`
	ResponseFormat       string   `json:"response_format"`
	OnlyMyChannels       bool     `json:"only_my_channels"`
	// Unknown are the fields given that aren't any of these, or that have
	// another type, sorted.
	Unknown []string `json:"-"`
}

// searchFields are the fields of a search's input, as SearchInput has them.
var searchFields = []string{"keywords", "filters", "channel_types", "content_types", "after", "before", "sort", "sort_dir",
	"limit", "include_context", "include_bots", "natural_language_query", "cursor", "response_format", "only_my_channels"}

// readInput reads a search call's input.
func readInput(raw json.RawMessage) SearchInput {
	var in SearchInput
	var bad *json.UnmarshalTypeError
	if err := json.Unmarshal(raw, &in); errors.As(err, &bad) {
		in.Unknown = append(in.Unknown, bad.Field)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for name := range fields {
		if !slices.Contains(searchFields, name) {
			in.Unknown = append(in.Unknown, name)
		}
	}
	slices.Sort(in.Unknown)
	in.Unknown = slices.Compact(in.Unknown)
	return in
}

// Message is one search result.
type Message struct {
	Channel, Place, AuthorID, Author, TS, ThreadTS, Permalink, Text string
	Participants                                                    []string // user IDs, for DMs and group DMs
	Bot                                                             bool     // a bracketed marker followed the author's ID
}

// ID names one message. It's not Ask.Key, which names the conversation.
func (m Message) ID() string { return m.Channel + "/" + m.TS }

const cantRead = "can't read Slack's search results"

const (
	searchTool  = "slack_search_public_and_private"
	profileTool = "slack_read_user_profile"
)

type event struct {
	Type       string `json:"type"`
	Subtype    string `json:"subtype"`
	McpServers []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	IsError          bool            `json:"is_error"`
	Result           json.RawMessage `json:"result"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	StructuredOutput json.RawMessage `json:"structured_output"`
}

// gists is a check's structured answer.
type gists struct {
	Gists []struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
		Gist    string `json:"gist"`
	} `json:"gists"`
}

type block struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// ParseOutput reads the stream-json lines of a check. Lines that aren't JSON
// are skipped. A search without its result has an Err.
func ParseOutput(out []byte) (Output, error) {
	res := Output{Gists: map[string]string{}}
	searchAt := map[string]int{} // tool_use ID → index in res.Searches
	answered := map[int]bool{}   // indexes in res.Searches with a result
	profile := map[string]bool{} // tool_use IDs of profile reads
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev event
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "system":
			if ev.Subtype != "init" {
				continue
			}
			res.Init = true
			for _, s := range ev.McpServers {
				if strings.Contains(s.Name, "slack") {
					res.Connector = s.Status
					break
				}
			}
		case "assistant", "user":
			res.Done = false
			var blocks []block
			if json.Unmarshal(ev.Message.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				switch b.Type {
				case "tool_use":
					server, tool := splitTool(b.Name)
					if !strings.Contains(server, "slack") {
						continue
					}
					switch tool {
					case searchTool:
						searchAt[b.ID] = len(res.Searches)
						res.Searches = append(res.Searches, Search{Input: readInput(b.Input)})
					case profileTool:
						profile[b.ID] = true
					}
				case "tool_result":
					text := contentText(b.Content)
					if i, ok := searchAt[b.ToolUseID]; ok {
						answered[i] = true
						readSearch(&res.Searches[i], text, b.IsError)
					} else if profile[b.ToolUseID] && !b.IsError {
						if m := profileID.FindStringSubmatch(unwrapResult(text)); m != nil {
							res.UserID = m[1]
						}
					}
				}
			}
		case "result":
			res.Done = true
			res.Cost = ev.TotalCostUSD
			res.Failed = failure(ev)
			res.Structured = nil
			if s := bytes.TrimSpace(ev.StructuredOutput); len(s) > 0 && !bytes.Equal(s, []byte("null")) {
				res.Structured = s
			}
			var answer gists
			if json.Unmarshal(res.Structured, &answer) == nil {
				for _, g := range answer.Gists {
					res.Gists[Message{Channel: g.Channel, TS: g.TS}.ID()] = g.Gist
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	for i := range res.Searches {
		if !answered[i] {
			res.Searches[i].Err = "the search got no answer"
		}
	}
	return res, nil
}

var profileID = regexp.MustCompile(`User ID: (\S+)`)

// unwrapResult returns the "result" string of {"result":"…"}, or text itself when it isn't that.
func unwrapResult(text string) string {
	var w struct {
		Result string `json:"result"`
	}
	if json.Unmarshal([]byte(text), &w) == nil && w.Result != "" {
		return w.Result
	}
	return text
}

// splitTool splits mcp__<server>__<tool>. Other names have no server.
func splitTool(name string) (server, tool string) {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return "", name
	}
	server, tool, _ = strings.Cut(rest, "__")
	return server, tool
}

// contentText reads a tool_result's content: a string, or a list of text blocks.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if p.Type == "text" || p.Type == "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func failure(ev event) string {
	var result string
	_ = json.Unmarshal(ev.Result, &result)
	first, _, _ := strings.Cut(strings.TrimSpace(result), "\n")
	switch {
	case ev.IsError && first != "":
		return first
	case ev.IsError:
		return "the run failed (" + ev.Subtype + ")"
	case ev.Subtype == "error_max_budget_usd":
		return "the check went over its budget (error_max_budget_usd)"
	case ev.Subtype == "":
		return "the run's result doesn't say it worked"
	case ev.Subtype != "success":
		return "the run stopped: " + ev.Subtype
	}
	return ""
}

func readSearch(s *Search, text string, isError bool) {
	if isError {
		s.Err = strings.TrimSpace(text)
		if s.Err == "" {
			s.Err = "the search failed"
		}
		return
	}
	var body map[string]json.RawMessage
	var results string
	_ = json.Unmarshal([]byte(text), &body)
	// results must be text: null would read as no results.
	raw := bytes.TrimSpace(body["results"])
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &results) != nil {
		s.Err = cantRead + ": the answer isn't a JSON object with results"
		return
	}
	var paging string
	_ = json.Unmarshal(body["pagination_info"], &paging)
	s.More = strings.Contains(paging, "cursor")
	msgs, warnings, err := ParseResults(results)
	if err != nil {
		s.Err = err.Error()
		return
	}
	s.Messages, s.Warnings = msgs, warnings
}

var (
	labelled = regexp.MustCompile(`^(.*?) \(ID: ([A-Za-z0-9]+)\)\s*(\[[^\]]*\])?$`)
	idOnly   = regexp.MustCompile(`\(ID: ([A-Za-z0-9]+)\)`)
	linkURL  = regexp.MustCompile(`\(([^()\s]+)\)`)
	emailTag = regexp.MustCompile(`\s*<[^>]*>$`)
)

// imitation starts the warning for a message whose text holds a line shaped
// like a result's header, before its permalink.
const imitation = "a message contains text shaped like Slack's result headers: "

// headerLine is a line shaped like a result block's header. Message text
// can hold one too.
var headerLine = regexp.MustCompile(`(?m)^### Result (\d+) of (\d+)[ \t]*$`)

// header is a header-shaped line: where it is, and its numbers.
type header struct {
	start, end int // the line, without its newline
	n, of      int
}

// ParseResults reads the markdown of a detailed search. No blocks is no
// messages; a layout it doesn't know is an error. A page of m results, as
// its first header says, is split only on the header it expects next,
// "### Result 1 of m", then 2, … up to m, and only one followed by its
// Channel line whose result, up to the next header-shaped line, reads as
// one. Any other header-shaped line is text of the message it's in, and
// when its number is one of the page's, a warning for slack.log names that
// message's permalink. The page must end with all m results.
func ParseResults(results string) ([]Message, []string, error) {
	var lines []header
	for _, at := range headerLine.FindAllStringSubmatchIndex(results, -1) {
		n, _ := strconv.Atoi(results[at[2]:at[3]])
		of, _ := strconv.Atoi(results[at[4]:at[5]])
		lines = append(lines, header{at[0], at[1], n, of})
	}
	if len(lines) == 0 {
		for _, label := range []string{"Message_ts:", "Permalink:", "From:"} {
			if strings.Contains(results, label) {
				return nil, nil, errors.New(cantRead + ": found " + label + " outside a result block")
			}
		}
		return nil, nil, nil
	}
	// The headers that start blocks, and the others, which are text. A
	// header that doesn't start a whole result is text too, so a message
	// can't stall the page with a header and a Channel line.
	var heads, text []header
	var blockErr error // why the first header that could have started one didn't
	m := 0
	for i, h := range lines {
		if m == 0 && h.n == 1 && h.of > 0 && opensBlock(results, h) {
			m = h.of
		}
		if m > 0 && h.n == len(heads)+1 && h.n <= m && h.of == m && opensBlock(results, h) {
			end := len(results)
			if i+1 < len(lines) {
				end = lines[i+1].start
			}
			_, err := parseBlock(results[h.end:end])
			if err == nil {
				heads = append(heads, h)
				continue
			}
			if blockErr == nil {
				blockErr = err
			}
		}
		text = append(text, h)
	}
	if m == 0 || len(heads) != m {
		if blockErr != nil {
			return nil, nil, blockErr
		}
		return nil, nil, fmt.Errorf("%s: the page has %d of its %d results", cantRead, len(heads), m)
	}
	var msgs []Message
	var warnings []string
	for i, h := range heads {
		end := len(results)
		if i+1 < len(heads) {
			end = heads[i+1].start
		}
		msg, err := parseBlock(results[h.end:end])
		if err != nil {
			return nil, nil, err
		}
		msgs = append(msgs, msg)
		if slices.ContainsFunc(text, func(t header) bool { return t.start > h.start && t.start < end && t.n >= 1 && t.n <= m }) {
			link := msg.Permalink
			if !task.SafeURL(link) {
				link = "?"
			}
			warnings = append(warnings, imitation+link)
		}
	}
	return msgs, warnings, nil
}

// opensBlock reports whether the header h is followed by a Channel line,
// as every result's header is.
func opensBlock(results string, h header) bool {
	return strings.HasPrefix(results[h.end:], "\nChannel: ")
}

func parseBlock(part string) (Message, error) {
	head, text, hasText := strings.Cut(part, "\nText: ")
	if hasText {
		// The block ends at its last "\n\n---"; the message may hold its own.
		if i := strings.LastIndex(text, "\n\n---"); i >= 0 {
			text = text[:i]
		}
	}
	var m Message
	var have = map[string]bool{}
	for _, line := range strings.Split(head, "\n") {
		label, val, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch label {
		case "Channel":
			if l := labelled.FindStringSubmatch(val); l != nil {
				m.Place, m.Channel = l[1], l[2]
			}
		case "From":
			if l := labelled.FindStringSubmatch(val); l != nil {
				m.Author, m.AuthorID, m.Bot = emailTag.ReplaceAllString(l[1], ""), l[2], l[3] != ""
			}
		case "Message_ts":
			m.TS = val
		case "Permalink":
			if l := linkURL.FindStringSubmatch(val); l != nil {
				m.Permalink = l[1]
				if u, err := url.Parse(l[1]); err == nil {
					m.ThreadTS = u.Query().Get("thread_ts")
				}
			}
		case "Participants":
			for _, id := range idOnly.FindAllStringSubmatch(val, -1) {
				m.Participants = append(m.Participants, id[1])
			}
		}
		have[label] = true
	}
	m.Text = strings.TrimSpace(text)
	for _, label := range []string{"Channel", "From", "Message_ts", "Permalink"} {
		if !have[label] {
			return Message{}, fmt.Errorf("%s: a result has no %s", cantRead, label)
		}
	}
	if m.Channel == "" || m.AuthorID == "" || m.TS == "" || m.Permalink == "" {
		return Message{}, errors.New(cantRead + ": a result's fields don't have the expected form")
	}
	return m, nil
}
