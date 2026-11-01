package slack

import (
	"fmt"
	"maps"
	"os"
	"testing"
)

func channels(t *testing.T, dir string) map[string]string {
	t.Helper()
	names, err := Channels(dir)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func learn(t *testing.T, dir string, names map[string]string) {
	t.Helper()
	if err := LearnChannels(dir, names); err != nil {
		t.Fatal(err)
	}
}

func TestChannelsWithNoneSeenMakesNothing(t *testing.T) {
	dir := t.TempDir()
	if got := channels(t, dir); len(got) != 0 {
		t.Errorf("Channels = %v, want none", got)
	}
	learn(t, dir, nil)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the state folder has %d entries (%s first), want none", len(entries), entries[0].Name())
	}
}

func TestLearnChannelsKeepsTheLatestName(t *testing.T) {
	dir := t.TempDir()
	learn(t, dir, map[string]string{"C0123ABCD": "#billing", "C0OPS0001": "#ops"})
	learn(t, dir, map[string]string{"C0123ABCD": "#billing-team"})
	if got, want := channels(t, dir), map[string]string{"C0123ABCD": "#billing-team", "C0OPS0001": "#ops"}; !maps.Equal(got, want) {
		t.Errorf("Channels = %v, want %v", got, want)
	}
}

func TestLearnChannelsCaps(t *testing.T) {
	dir := t.TempDir()
	learn(t, dir, map[string]string{"C0OLD0001": "#old", "C0KEEP001": "#keep"})
	// Seen again, so it's no longer among the least recently seen.
	learn(t, dir, map[string]string{"C0KEEP001": "#keep"})
	more := map[string]string{}
	for i := range 999 {
		more[fmt.Sprintf("C1%07d", i)] = fmt.Sprintf("#team-%d", i)
	}
	learn(t, dir, more)

	got := channels(t, dir)
	if len(got) != 1000 {
		t.Errorf("Channels has %d, want 1,000", len(got))
	}
	if _, ok := got["C0OLD0001"]; ok {
		t.Error("the least recently seen channel is kept")
	}
	if got["C0KEEP001"] != "#keep" || got["C10000998"] != "#team-998" {
		t.Errorf("C0KEEP001 = %q, C10000998 = %q", got["C0KEEP001"], got["C10000998"])
	}
}
