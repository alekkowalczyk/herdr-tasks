package view

import "strings"

// Fields is what a search reads of a task, from the best place to find a
// word in to the least. The board's page has the same rules, in search.js.
type Fields struct {
	Name string
	// The names its panes have in herdr, and its agents' titles.
	Panes []string
	// Its agents' recaps.
	Recaps []string
	// Everything else: its reason, note and log, its links, and its agents'
	// last prompts and replies.
	Content []string
}

// Match is where a search found a task, best first.
type Match int

const (
	NoMatch Match = iota
	InName
	InPane
	InRecap
	InContent
)

// Hit is where a search found a task, and the text it found it in, on one
// line. Text[At:At+Len] is the word that decided where: Len is 0 when the
// text's lower case is longer or shorter than it, so the word can't be
// placed.
type Hit struct {
	Match   Match
	Text    string
	At, Len int
}

// Words are what you search for, in lower case.
func Words(query string) []string { return strings.Fields(strings.ToLower(query)) }

// Search finds every one of words in f, or returns NoMatch. Each word is
// found in the best place that has it, and the task where its worst word
// was: "login timeout", with login in the name and timeout in a recap, is
// found in the recap. A word matches any part of a word.
func Search(words []string, f Fields) Hit {
	if len(words) == 0 {
		return Hit{}
	}
	places := [...][]string{InName: {f.Name}, InPane: f.Panes, InRecap: f.Recaps, InContent: f.Content}
	lines := map[string][2]string{} // text → it on one line, and that in lower case
	var hit Hit
	for _, w := range words {
		found := false
		for m := InName; m <= InContent && !found; m++ {
			for _, text := range places[m] {
				l, ok := lines[text]
				if !ok {
					one := strings.Join(strings.Fields(text), " ")
					l = [2]string{one, strings.ToLower(one)}
					lines[text] = l
				}
				i := strings.Index(l[1], w)
				if i < 0 {
					continue
				}
				found = true
				if m > hit.Match {
					hit = Hit{Match: m, Text: l[0]}
					if len(l[0]) == len(l[1]) {
						hit.At, hit.Len = i, len(w)
					}
				}
				break
			}
		}
		if !found {
			return Hit{}
		}
	}
	return hit
}
