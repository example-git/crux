package cmd

import (
	"context"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/ui/dialog"
)

// serverMenuClient adapts *client.Client to dialog.ServerMenuClient for the
// server workspace menu shell. It prefers the connection-scoped peer channel
// (push-capable list/browse, and the only transport that can stream
// `git clone` progress for project creation) and falls back to the older
// one-shot HTTP endpoints only when the peer channel itself returns an
// error, so the menu keeps working against a server that predates the peer
// channel's workspace-menu message types.
type serverMenuClient struct {
	*client.Client
}

var _ dialog.ServerMenuClient = serverMenuClient{}

// RefreshWorkspaces implements [dialog.ServerMenuClient].
func (c serverMenuClient) RefreshWorkspaces(ctx context.Context) ([]proto.Workspace, error) {
	workspaces, err := c.Client.RefreshWorkspacesViaPeer(ctx)
	if err == nil {
		return workspaces, nil
	}
	return c.Client.RefreshWorkspaces(ctx)
}

// Browse implements [dialog.ServerMenuClient].
func (c serverMenuClient) Browse(ctx context.Context, path string) (proto.BrowserListing, error) {
	listing, err := c.Client.BrowseViaPeer(ctx, path)
	if err == nil {
		return listing, nil
	}
	return c.Client.Browse(ctx, path)
}

// CloseIdleWorkspace and CreateProject are inherited unchanged from the
// embedded *client.Client: CloseIdleWorkspace has no peer-channel variant,
// and CreateProject (internal/client/peer_menu.go) is peer-channel-only by
// design (Phase 3), since progress streaming requires it.
