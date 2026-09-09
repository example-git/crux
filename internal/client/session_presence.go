package client

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/proto"
)

// CurrentSessionSelection is an immutable selection, shared across wrappers of
// this SDK client UUID. Reconnection replays it without creating a new intent.
type CurrentSessionSelection struct {
	SessionID  string
	Generation uint64
}

// PrepareCurrentSession orders accepted local intent before any network IO.
// admit must perform only a short, local freshness check/update. It runs inside
// the same lock as generation allocation, so an older command cannot pass its
// check, pause, and then mint a generation after a newer selection.
func (c *Client) PrepareCurrentSession(workspaceID, sessionID string, admit func() error) (CurrentSessionSelection, error) {
	c.presenceMu.Lock()
	defer c.presenceMu.Unlock()
	if workspaceID == "" || c.presenceGeneration == ^uint64(0) {
		return CurrentSessionSelection{}, errors.New("current-session selection identity or generation is invalid")
	}
	if admit != nil {
		if err := admit(); err != nil {
			return CurrentSessionSelection{}, err
		}
	}
	c.presenceGeneration++
	selection := CurrentSessionSelection{SessionID: sessionID, Generation: c.presenceGeneration}
	if c.presenceSelections == nil {
		c.presenceSelections = make(map[string]CurrentSessionSelection)
	}
	c.presenceSelections[workspaceID] = selection
	return selection, nil
}

// CurrentSessionSelection returns the latest exact intent. During legitimate
// recreation, previousID may seed the new receiver only if it has no selection
// yet; a newer selection in that receiver always wins, including an empty one.
func (c *Client) CurrentSessionSelection(workspaceID, previousID string) (CurrentSessionSelection, bool) {
	c.presenceMu.Lock()
	defer c.presenceMu.Unlock()
	selection, ok := c.presenceSelections[workspaceID]
	if !ok && previousID != "" {
		selection, ok = c.presenceSelections[previousID]
		if ok {
			c.presenceSelections[workspaceID] = selection
		}
	}
	return selection, ok
}

func (c *Client) SendCurrentSessionSelection(ctx context.Context, workspaceID string, selection CurrentSessionSelection) error {
	if selection.Generation == 0 {
		return errors.New("current-session selection generation must be positive")
	}
	return c.sendCurrentSession(ctx, workspaceID, proto.CurrentSession{SessionID: selection.SessionID, SelectionGeneration: &selection.Generation})
}
