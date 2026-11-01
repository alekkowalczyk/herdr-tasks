package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const testKey = "lin_api_test123"

type gqlRequest struct {
	Query     string            `json:"query"`
	Variables map[string]string `json:"variables"`
}

// fakeLinear answers issue queries like Linear's GraphQL API. Issues in
// missing aren't found: their alias gets an error, and with nullAll the whole
// answer is null, as GraphQL does when a non-null field fails.
type fakeLinear struct {
	t       *testing.T
	missing map[string]bool
	nullAll bool
	status  int // for answers with an error; 200 when 0
	mu      sync.Mutex
	reqs    []gqlRequest
}

func (f *fakeLinear) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		f.t.Errorf("method = %s, want POST", r.Method)
	}
	if got := r.Header.Get("Authorization"); got != testKey {
		f.t.Errorf("Authorization = %q, want the bare key", got)
	}
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		f.t.Errorf("Content-Type = %q", got)
	}
	var req gqlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("decoding the request: %v", err)
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()

	data := map[string]any{}
	var errs []map[string]any
	for alias, id := range req.Variables {
		if !strings.Contains(req.Query, alias+": issue(id: $"+alias+")") {
			f.t.Errorf("query has no %s alias for %s", alias, id)
		}
		if f.missing[id] {
			data[alias] = nil
			errs = append(errs, map[string]any{"message": "Entity not found: Issue", "path": []any{alias}})
			continue
		}
		data[alias] = map[string]any{
			"identifier": id,
			"title":      "Title of " + id,
			"url":        "https://linear.app/acme/issue/" + id + "/slug",
			"state":      map[string]any{"name": "In Progress", "type": "started", "color": "#f2c94c"},
			"assignee":   map[string]any{"displayName": "ann"},
		}
	}
	resp := map[string]any{"data": data}
	if len(errs) > 0 {
		resp["errors"] = errs
		if f.nullAll {
			resp["data"] = nil
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
		}
	}
	json.NewEncoder(w).Encode(resp)
}

func (f *fakeLinear) requests() []gqlRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gqlRequest(nil), f.reqs...)
}

func serve(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{Key: testKey, Endpoint: srv.URL}
}

func ids(n int) []string {
	var out []string
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("ABC-%d", i))
	}
	return out
}

func TestIssuesBatchesAndParses(t *testing.T) {
	f := &fakeLinear{t: t, missing: map[string]bool{"ABC-7": true}}
	c := serve(t, f)

	got, err := c.Issues(context.Background(), append(ids(120), "ABC-1", ""))
	if err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 3 {
		t.Fatalf("made %d requests, want 3", len(reqs))
	}
	asked := 0
	for _, r := range reqs {
		if len(r.Variables) > 50 {
			t.Errorf("a request asked for %d issues, want at most 50", len(r.Variables))
		}
		asked += len(r.Variables)
		for _, field := range []string{"identifier", "title", "url", "state { name type color }", "assignee { displayName }"} {
			if !strings.Contains(r.Query, field) {
				t.Errorf("query doesn't ask for %s: %s", field, r.Query)
			}
		}
	}
	if asked != 120 {
		t.Errorf("asked for %d issues, want 120 (each once)", asked)
	}
	if len(got) != 119 {
		t.Errorf("got %d issues, want 119", len(got))
	}
	if _, ok := got["ABC-7"]; ok {
		t.Error("the missing issue is in the result")
	}
	want := Issue{
		ID:        "ABC-12",
		Title:     "Title of ABC-12",
		URL:       "https://linear.app/acme/issue/ABC-12/slug",
		State:     "In Progress",
		StateType: "started",
		Color:     "#f2c94c",
		Assignee:  "ann",
	}
	if got["ABC-12"] != want {
		t.Errorf("ABC-12 = %+v, want %+v", got["ABC-12"], want)
	}
}

func TestIssuesAsksAgainWhenAMissingIssueNullsTheAnswer(t *testing.T) {
	for _, status := range []int{0, http.StatusBadRequest} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := &fakeLinear{t: t, missing: map[string]bool{"ABC-2": true}, nullAll: true, status: status}
			c := serve(t, f)
			got, err := c.Issues(context.Background(), ids(3))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || got["ABC-1"].Title == "" || got["ABC-3"].Title == "" {
				t.Errorf("got %+v, want ABC-1 and ABC-3", got)
			}
			if n := len(f.requests()); n != 2 {
				t.Errorf("made %d requests, want 2", n)
			}
		})
	}
}

func TestIssuesHTTPError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"errors":[{"message":"Authentication required, not authenticated"}]}`, "Authentication required"},
		{"rate limited", http.StatusBadRequest, `{"errors":[{"message":"Rate limit exceeded","extensions":{"code":"RATELIMITED"}}]}`, "Rate limit exceeded"},
		{"server error", http.StatusInternalServerError, "oops", "500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			got, err := c.Issues(context.Background(), ids(2))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one with %q", err, tc.want)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("the error shows the key: %v", err)
			}
			if got != nil {
				t.Errorf("got %v with the error", got)
			}
		})
	}
}

func TestIssuesGraphQLErrorWithoutData(t *testing.T) {
	c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":null,"errors":[{"message":"Query too complex"}]}`)
	}))
	if _, err := c.Issues(context.Background(), ids(2)); err == nil || !strings.Contains(err.Error(), "Query too complex") {
		t.Fatalf("err = %v", err)
	}
}

func TestIssuesTransportError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	c := &Client{Key: testKey, Endpoint: srv.URL}
	if _, err := c.Issues(context.Background(), ids(1)); err == nil {
		t.Fatal("no error from a closed server")
	}
}

func TestIssuesWithoutIDsOrKey(t *testing.T) {
	var called atomic.Bool
	c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Store(true) }))
	got, err := c.Issues(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Errorf("no IDs: got %v, %v", got, err)
	}
	c.Key = ""
	if _, err := c.Issues(context.Background(), ids(1)); !errors.Is(err, ErrNoKey) {
		t.Errorf("no key: err = %v, want ErrNoKey", err)
	}
	if called.Load() {
		t.Error("asked Linear with nothing to ask or no key")
	}
}

func TestIssueJSONFieldNames(t *testing.T) {
	b, err := json.Marshal(Issue{ID: "ABC-1", Title: "t", URL: "u", State: "s", StateType: "started", Color: "#fff", Assignee: "a"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"ABC-1","title":"t","url":"u","state":"s","stateType":"started","color":"#fff","assignee":"a"}`
	if string(b) != want {
		t.Errorf("got %s\nwant %s", b, want)
	}
	if b, _ := json.Marshal(Issue{ID: "ABC-1"}); string(b) != `{"id":"ABC-1"}` {
		t.Errorf("an issue that wasn't fetched is %s, want the ID alone", b)
	}
}
