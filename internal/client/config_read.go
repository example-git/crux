package client

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
)

func (c *Client) ReadConfigView(ctx context.Context, workspaceID string) (config.ConfigReadView, error) {
	if strings.TrimSpace(workspaceID) != workspaceID || strings.ContainsAny(workspaceID, "/\\") {
		return config.ConfigReadView{}, fmt.Errorf("invalid configuration workspace")
	}
	if store := c.LocalRuntimeStore(); store != nil {
		scope := config.ScopeGlobal
		if workspaceID != "" {
			scope = config.ScopeWorkspace
		}
		return store.ReadConfigView(ctx, scope)
	}
	path := "/config-view"
	if workspaceID != "" {
		path = "/workspaces/" + url.PathEscape(workspaceID) + "/config-view"
	}
	response, err := c.get(ctx, path, nil, nil)
	if err != nil {
		return config.ConfigReadView{}, err
	}
	defer response.Body.Close()
	if err := checkStatus(response); err != nil {
		return config.ConfigReadView{}, err
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, proto.MaxConfigReadResponseBytes+1))
	if err != nil {
		return config.ConfigReadView{}, err
	}
	if err := ctx.Err(); err != nil {
		return config.ConfigReadView{}, err
	}
	return proto.DecodeConfigReadResponse(body, workspaceID)
}
