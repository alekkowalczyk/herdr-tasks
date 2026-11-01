package slack

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/alekkowalczyk/herdr-tasks/internal/store"
	"github.com/alekkowalczyk/herdr-tasks/internal/task"
)

// maxChannels is how many channels' names are kept.
const maxChannels = 1000

// channel is a channel's ID and its name, "#billing".
type channel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// channelList is slack/channels.json: the channels' names the watcher and
// the Claude links hook saw, the least recently seen first.
type channelList struct {
	Channels []channel `json:"channels"`
}

// channelsFile is slack/channels.json in the state folder dir, next to the
// watcher's state.
func channelsFile(dir string) store.File {
	return store.File{Dir: filepath.Join(dir, "slack"), Name: "channels"}
}

// Channels returns the names of the channels seen, by ID: "C0123ABCD" →
// "#billing". Before any were seen it's empty, and it makes no file.
func Channels(dir string) (map[string]string, error) {
	f := channelsFile(dir)
	if _, err := os.Stat(f.Path()); errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	var l channelList
	if err := f.Load(&l); err != nil {
		return nil, err
	}
	names := make(map[string]string, len(l.Channels))
	for _, c := range l.Channels {
		names[c.ID] = c.Name
	}
	return names, nil
}

// LearnChannels saves names, channel ID → "#name", as the channels seen
// most recently. It keeps the 1,000 seen most recently. Nothing is written
// when nothing changes.
func LearnChannels(dir string, names map[string]string) error {
	var seen []channel
	for id, name := range names {
		if id != "" && name != "" {
			seen = append(seen, channel{id, name})
		}
	}
	if len(seen) == 0 {
		return nil
	}
	slices.SortFunc(seen, func(a, b channel) int { return strings.Compare(a.ID, b.ID) })
	var l channelList
	err := channelsFile(dir).Update(&l, func() error {
		kept := slices.DeleteFunc(slices.Clone(l.Channels), func(c channel) bool {
			_, again := slices.BinarySearchFunc(seen, c.ID, func(s channel, id string) int { return strings.Compare(s.ID, id) })
			return again
		})
		kept = append(kept, seen...)
		if n := len(kept) - maxChannels; n > 0 {
			kept = kept[n:]
		}
		if slices.Equal(kept, l.Channels) {
			return errUnchanged
		}
		l.Channels = kept
		return nil
	})
	if errors.Is(err, errUnchanged) {
		return nil
	}
	return err
}

// ChannelNames are the names of the channels msgs are in, by ID, for
// LearnChannels: channels only, never DMs or group DMs, and only those
// Slack gave a name.
func ChannelNames(msgs []Message) map[string]string {
	names := map[string]string{}
	for _, m := range msgs {
		if dmLike(m) || !channelID.MatchString(m.Channel) {
			continue
		}
		if name := bare(task.CleanLabel(m.Place)); name != "" {
			names[m.Channel] = "#" + name
		}
	}
	return names
}
