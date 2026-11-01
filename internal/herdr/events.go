package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Subscription asks for one event type, such as "tab.created". Some types,
// such as "pane.agent_status_changed", need a PaneID.
type Subscription struct {
	Type   string `json:"type"`
	PaneID string `json:"pane_id,omitempty"`
}

// Event is one event herdr streamed, such as "tab_created". Data is the
// event as herdr sent it.
type Event struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// Subscribe streams events to fn until ctx ends or herdr closes the
// connection, and returns why. herdr replays old events when a subscription
// starts, so treat events as a cue to read again, not as the current state.
func (c *Client) Subscribe(ctx context.Context, subs []Subscription, fn func(Event)) error {
	dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	conn, err := new(net.Dialer).DialContext(dialCtx, "unix", c.SocketPath)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("can't reach herdr at %s (is it running?): %w", c.SocketPath, err)
	}
	defer conn.Close()

	// Reads wait as long as herdr is quiet; ending ctx closes the connection
	// to stop them.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	fail := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("herdr events: %w", err)
	}

	// Only the request and its answer get a deadline.
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if subs == nil {
		subs = []Subscription{}
	}
	req := map[string]any{"id": "herdr-tasks", "method": "events.subscribe", "params": map[string]any{"subscriptions": subs}}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fail(err)
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return fail(err)
	}
	var ack struct {
		Result *struct {
			Type string `json:"type"`
		} `json:"result"`
		Error *Error `json:"error"`
	}
	if err := json.Unmarshal(line, &ack); err != nil {
		return fmt.Errorf("herdr events: bad response: %w", err)
	}
	if ack.Error != nil {
		return ack.Error
	}
	if ack.Result == nil || ack.Result.Type != "subscription_started" {
		return fmt.Errorf("herdr events: unexpected answer %s", line)
	}
	_ = conn.SetDeadline(time.Time{})

	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return fail(err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ev Event
		if json.Unmarshal(line, &ev) == nil && ev.Event != "" {
			fn(ev)
		}
	}
}
