// Package linear asks Linear's GraphQL API for the status of issues, for the
// board's link chips, and keeps the personal API key it needs.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Issue is what the board shows of a Linear issue. ID is its identifier, as
// "ABC-12". An issue that wasn't fetched has the ID alone.
type Issue struct {
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	URL       string `json:"url,omitempty"`
	State     string `json:"state,omitempty"`     // the status name, as "In Progress"
	StateType string `json:"stateType,omitempty"` // triage, backlog, unstarted, started, completed or canceled
	Color     string `json:"color,omitempty"`     // the status color, as "#f2c94c"
	Assignee  string `json:"assignee,omitempty"`  // display name
}

const (
	defaultEndpoint = "https://api.linear.app/graphql"
	batchSize       = 50
	maxResponse     = 8 << 20
)

var defaultHTTP = &http.Client{Timeout: 10 * time.Second}

// Client asks Linear's API with a personal API key.
type Client struct {
	Key      string
	Endpoint string       // default https://api.linear.app/graphql
	HTTP     *http.Client // default: a 10 s timeout
}

// Issues fetches issues by identifier, 50 per request, keyed by the ID asked
// for. Issues Linear can't find are left out. A failed request fails the
// whole call.
func (c *Client) Issues(ctx context.Context, ids []string) (map[string]Issue, error) {
	out := map[string]Issue{}
	seen := map[string]bool{}
	var todo []string
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			todo = append(todo, id)
		}
	}
	if len(todo) == 0 {
		return out, nil
	}
	if c.Key == "" {
		return nil, ErrNoKey
	}
	for len(todo) > 0 {
		n := min(batchSize, len(todo))
		if err := c.batch(ctx, todo[:n], out); err != nil {
			return nil, err
		}
		todo = todo[n:]
	}
	return out, nil
}

// batch fetches ids into out. Linear's issue field never returns null, so an
// issue it can't find makes GraphQL null the whole answer; the others are then
// asked for again without it.
func (c *Client) batch(ctx context.Context, ids []string, out map[string]Issue) error {
	for len(ids) > 0 {
		data, failed, err := c.post(ctx, ids)
		if err != nil {
			return err
		}
		var again []string
		for i, id := range ids {
			if is := data[alias(i)]; is != nil {
				out[id] = is.issue(id)
			} else if len(failed) > 0 && !failed[i] {
				again = append(again, id)
			}
		}
		ids = again
	}
	return nil
}

type gqlIssue struct {
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	State      *struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		Color string `json:"color"`
	} `json:"state"`
	Assignee *struct {
		DisplayName string `json:"displayName"`
	} `json:"assignee"`
}

func (g *gqlIssue) issue(id string) Issue {
	is := Issue{ID: g.Identifier, Title: g.Title, URL: g.URL}
	if is.ID == "" {
		is.ID = id
	}
	if g.State != nil {
		is.State, is.StateType, is.Color = g.State.Name, g.State.Type, g.State.Color
	}
	if g.Assignee != nil {
		is.Assignee = g.Assignee.DisplayName
	}
	return is
}

type response struct {
	Data   map[string]*gqlIssue `json:"data"`
	Errors []struct {
		Message string `json:"message"`
		Path    []any  `json:"path"`
	} `json:"errors"`
}

// failed is the aliases, by index, that have an error, and whether every
// error is on one of them.
func (r *response) failed(n int) (map[int]bool, bool) {
	f := map[int]bool{}
	all := true
	for _, e := range r.Errors {
		hit := false
		for i := 0; i < n && len(e.Path) > 0; i++ {
			if e.Path[0] == alias(i) {
				f[i], hit = true, true
			}
		}
		all = all && hit
	}
	return f, all
}

func (r *response) message() string {
	if len(r.Errors) == 0 {
		return ""
	}
	return r.Errors[0].Message
}

func alias(i int) string { return fmt.Sprintf("i%d", i) }

// query asks for n issues, aliased i0, i1, …, with their IDs as variables.
func query(n int) string {
	var b strings.Builder
	b.WriteString("query Issues(")
	for i := range n {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "$%s: String!", alias(i))
	}
	b.WriteString(") {")
	for i := range n {
		fmt.Fprintf(&b, " %s: issue(id: $%[1]s) { ...F }", alias(i))
	}
	b.WriteString(" } fragment F on Issue { identifier title url state { name type color } assignee { displayName } }")
	return b.String()
}

// post asks for one batch. It returns the issues by alias and the aliases,
// by index, that have an error.
func (c *Client) post(ctx context.Context, ids []string) (map[string]*gqlIssue, map[int]bool, error) {
	vars := map[string]string{}
	for i, id := range ids {
		vars[alias(i)] = id
	}
	body, err := json.Marshal(map[string]any{"query": query(len(ids)), "variables": vars})
	if err != nil {
		return nil, nil, err
	}
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("linear: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Personal API keys go in as they are, without "Bearer".
	req.Header.Set("Authorization", c.Key)
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("linear: %w", err)
	}
	defer resp.Body.Close()
	var r response
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&r)
	failed, issuesOnly := r.failed(len(ids))
	// Errors on single issues may come with a 400. They still leave just
	// those issues out.
	if resp.StatusCode >= 400 && (decodeErr != nil || len(r.Errors) == 0 || !issuesOnly) {
		if m := r.message(); m != "" {
			return nil, nil, fmt.Errorf("linear: %s: %s", resp.Status, m)
		}
		return nil, nil, fmt.Errorf("linear: %s", resp.Status)
	}
	if decodeErr != nil {
		return nil, nil, fmt.Errorf("linear: bad response: %w", decodeErr)
	}
	if len(r.Data) == 0 && len(failed) == 0 && len(r.Errors) > 0 {
		return nil, nil, fmt.Errorf("linear: %s", r.message())
	}
	return r.Data, failed, nil
}
