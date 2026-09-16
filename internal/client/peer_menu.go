package client

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/example-git/crux/internal/proto"
	"github.com/google/uuid"
)

// RefreshWorkspacesViaPeer requests the caller's visible workspace list over
// the connection-scoped peer channel instead of a one-shot HTTP request.
// Combined with SubscribeWorkspaceListChanged, this lets a caller refresh
// only when the server actually reports a change instead of polling.
func (c *Client) RefreshWorkspacesViaPeer(ctx context.Context) ([]proto.Workspace, error) {
	peer, err := c.ensurePeerChannel(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open peer channel: %w", err)
	}
	ack, err := peer.command(ctx, "", proto.PeerTypeWorkspaceList, proto.PeerWorkspaceListRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to list workspaces: %w", err)
	}
	var workspaces []proto.Workspace
	if err := json.Unmarshal(ack.Data, &workspaces); err != nil {
		return nil, fmt.Errorf("failed to decode workspace list: %w", err)
	}
	for i := range workspaces {
		if err := bindWorkspaceProviderOwners(&workspaces[i]); err != nil {
			return nil, fmt.Errorf("failed to bind workspace provider owners: %w", err)
		}
	}
	return workspaces, nil
}

// BrowseViaPeer requests a directory listing rooted at one of the server's
// configured workspace roots over the connection-scoped peer channel.
func (c *Client) BrowseViaPeer(ctx context.Context, path string) (proto.BrowserListing, error) {
	peer, err := c.ensurePeerChannel(ctx)
	if err != nil {
		return proto.BrowserListing{}, fmt.Errorf("failed to open peer channel: %w", err)
	}
	ack, err := peer.command(ctx, "", proto.PeerTypeBrowserList, proto.PeerBrowserListRequest{Path: path})
	if err != nil {
		return proto.BrowserListing{}, fmt.Errorf("failed to browse server path: %w", err)
	}
	var listing proto.BrowserListing
	if err := json.Unmarshal(ack.Data, &listing); err != nil {
		return proto.BrowserListing{}, fmt.Errorf("failed to decode browser listing: %w", err)
	}
	return listing, nil
}

// CreateProject creates a new project directory (optionally running git init
// or git clone) rooted at one of the server's configured workspace roots,
// over the connection-scoped peer channel. progress, if non-nil, receives
// best-effort output lines for long-running modes (git clone) as they
// arrive; it may be called concurrently with CreateProject still running
// and must not block for long.
func (c *Client) CreateProject(ctx context.Context, request proto.PeerWorkspaceCreateRequest, progress func(string)) (string, error) {
	peer, err := c.ensurePeerChannel(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to open peer channel: %w", err)
	}
	messageID := uuid.NewString()
	if progress != nil {
		peer.menuMu.Lock()
		peer.progressListeners[messageID] = progress
		peer.menuMu.Unlock()
		defer func() {
			peer.menuMu.Lock()
			delete(peer.progressListeners, messageID)
			peer.menuMu.Unlock()
		}()
	}
	ack, err := peer.commandWithID(ctx, "", messageID, proto.PeerTypeWorkspaceCreate, request)
	if err != nil {
		return "", fmt.Errorf("failed to create project: %w", err)
	}
	var result proto.PeerWorkspaceCreateResult
	if err := json.Unmarshal(ack.Data, &result); err != nil {
		return "", fmt.Errorf("failed to decode project creation result: %w", err)
	}
	return result.Path, nil
}

// SubscribeWorkspaceListChanged registers for a best-effort invalidation
// signal delivered whenever the server creates or removes a workspace
// visible to this connection's authenticated principal. The returned
// channel is closed when the underlying peer channel closes; callers
// should treat closure as "refresh once more, then stop watching" rather
// than an error. The returned cleanup function must be called when the
// caller is done watching.
func (c *Client) SubscribeWorkspaceListChanged(ctx context.Context) (<-chan struct{}, func(), error) {
	peer, err := c.ensurePeerChannel(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open peer channel: %w", err)
	}
	sub := make(chan struct{}, 1)
	peer.menuMu.Lock()
	peer.listChangedSubs[sub] = struct{}{}
	peer.menuMu.Unlock()
	unsubscribe := func() {
		peer.menuMu.Lock()
		if _, ok := peer.listChangedSubs[sub]; ok {
			delete(peer.listChangedSubs, sub)
			close(sub)
		}
		peer.menuMu.Unlock()
	}
	return sub, unsubscribe, nil
}
