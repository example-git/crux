package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

func (c *Client) ProviderAuthentication(ctx context.Context, id string) (providerauth.Snapshot, error) {
	body, err := c.providerAuthRequest(ctx, id, nil)
	if err != nil {
		return providerauth.Snapshot{}, err
	}
	state, err := proto.DecodeProviderAuthSnapshot(body)
	if err != nil {
		return providerauth.Snapshot{}, err
	}
	if state.WorkspaceID != id {
		return providerauth.Snapshot{}, fmt.Errorf("provider authentication response changed workspace")
	}
	return state, ctx.Err()
}

func (c *Client) ProviderAccounts(ctx context.Context, id string, target providerauth.Target) (providerauth.AccountsState, error) {
	if err := target.Validate(); err != nil {
		return providerauth.AccountsState{}, err
	}
	if target.WorkspaceID != id {
		return providerauth.AccountsState{}, fmt.Errorf("provider account target does not match workspace")
	}
	body, err := c.providerAuthRequest(ctx, id, &target)
	if err != nil {
		return providerauth.AccountsState{}, err
	}
	state, err := proto.DecodeProviderAccountsState(body)
	if err != nil {
		return providerauth.AccountsState{}, err
	}
	if state.Target != target {
		return providerauth.AccountsState{}, fmt.Errorf("provider account response changed workspace, owner or generation")
	}
	return state, ctx.Err()
}

func (c *Client) providerAuthRequest(ctx context.Context, id string, target *providerauth.Target) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id == "" || strings.TrimSpace(id) != id || strings.ContainsAny(id, "/\\") {
		return nil, fmt.Errorf("provider authentication workspace is required")
	}
	method, path := http.MethodGet, "/workspaces/"+url.PathEscape(id)+"/auth"
	var body []byte
	if target != nil {
		var err error
		body, err = json.Marshal(target)
		if err != nil {
			return nil, err
		}
		if len(body) > proto.MaxProviderAuthRequestBytes {
			return nil, fmt.Errorf("provider account request exceeds size limit")
		}
		method, path = http.MethodPost, path+"/accounts"
	}
	response, err := c.sendReq(ctx, method, path, nil, bytes.NewReader(body), http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return nil, fmt.Errorf("provider authentication request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		bounded := *response
		bounded.Body = io.NopCloser(io.LimitReader(response.Body, proto.MaxProviderAuthRequestBytes))
		return nil, fmt.Errorf("provider authentication request: %w", checkStatus(&bounded))
	}
	body, err = io.ReadAll(io.LimitReader(response.Body, proto.MaxProviderAuthResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read provider authentication response: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}
