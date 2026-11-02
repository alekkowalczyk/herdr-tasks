package view

import (
	"encoding/json"
	"os"
	"testing"
)

// searchCase is a case in testdata/search.json, which the board page's
// search.js passes too.
type searchCase struct {
	Why    string `json:"why"`
	Query  string `json:"query"`
	Fields struct {
		Name    string   `json:"name"`
		Panes   []string `json:"panes"`
		Recaps  []string `json:"recaps"`
		Content []string `json:"content"`
	} `json:"fields"`
	Match string `json:"match"`
	Text  string `json:"text"`
	Found string `json:"found"`
}

func TestSearch(t *testing.T) {
	b, err := os.ReadFile("testdata/search.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []searchCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	names := map[Match]string{NoMatch: "none", InName: "name", InPane: "pane", InRecap: "recap", InContent: "content"}
	for _, c := range cases {
		f := Fields{Name: c.Fields.Name, Panes: c.Fields.Panes, Recaps: c.Fields.Recaps, Content: c.Fields.Content}
		h := Search(Words(c.Query), f)
		if got := names[h.Match]; got != c.Match {
			t.Errorf("%s: %q found in %s, want %s", c.Why, c.Query, got, c.Match)
			continue
		}
		if h.Text != c.Text {
			t.Errorf("%s: %q found in %q, want %q", c.Why, c.Query, h.Text, c.Text)
		}
		if found := h.Text[h.At : h.At+h.Len]; found != c.Found {
			t.Errorf("%s: %q found as %q, want %q", c.Why, c.Query, found, c.Found)
		}
	}
}
