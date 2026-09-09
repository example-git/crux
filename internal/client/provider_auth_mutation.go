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

// SwitchProviderAccount returns exact safe progress even for a typed remote
// failure. Transport failures without a valid response remain uncertain. Retry
// only the same explicit operation ID/target; this method never retries itself.
func (c *Client) SwitchProviderAccount(ctx context.Context, id string, request providerauth.SwitchRequest) (proto.ProviderAuthenticationMutationResponse, error) {
	if err := request.Validate(); err != nil {
		return proto.ProviderAuthenticationMutationResponse{}, err
	}
	return c.providerAuthMutation(ctx, id, request.Target, "switch", request, func(body []byte) (proto.ProviderAuthenticationMutationResponse, error) {
		return proto.DecodeProviderAuthSwitchResponse(body, request)
	})
}

func (c *Client) LogoutProvider(ctx context.Context, id string, request providerauth.LogoutRequest) (proto.ProviderAuthenticationMutationResponse, error) {
	if err := request.Validate(); err != nil {
		return proto.ProviderAuthenticationMutationResponse{}, err
	}
	return c.providerAuthMutation(ctx, id, request.Target, "logout", request, func(body []byte) (proto.ProviderAuthenticationMutationResponse, error) {
		return proto.DecodeProviderAuthLogoutResponse(body, request)
	})
}

func (c *Client) providerAuthMutation(ctx context.Context, id string, target providerauth.Target, action string, request any, decode func([]byte) (proto.ProviderAuthenticationMutationResponse, error)) (proto.ProviderAuthenticationMutationResponse, error) {
	if err := ctx.Err(); err != nil {
		return proto.ProviderAuthenticationMutationResponse{}, err
	}
	if id == "" || strings.TrimSpace(id) != id || strings.ContainsAny(id, "/\\") || target.WorkspaceID != id {
		return proto.ProviderAuthenticationMutationResponse{}, fmt.Errorf("provider authentication target does not match workspace")
	}
	body, err := json.Marshal(request)
	if err != nil || len(body) > proto.MaxProviderAuthRequestBytes {
		return proto.ProviderAuthenticationMutationResponse{}, fmt.Errorf("invalid provider authentication mutation request")
	}
	response, err := c.sendReq(ctx, http.MethodPost, "/workspaces/"+url.PathEscape(id)+"/auth/"+action, nil, bytes.NewReader(body), http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return proto.ProviderAuthenticationMutationResponse{}, fmt.Errorf("provider authentication mutation request: %w", err)
	}
	defer response.Body.Close()
	body, err = io.ReadAll(io.LimitReader(response.Body, proto.MaxProviderAuthResponseBytes+1))
	if err != nil {
		return proto.ProviderAuthenticationMutationResponse{}, fmt.Errorf("read provider authentication mutation response: %w", err)
	}
	result, err := decode(body)
	if err != nil {
		return proto.ProviderAuthenticationMutationResponse{}, fmt.Errorf("provider authentication mutation response (HTTP %d): %w", response.StatusCode, err)
	}
	if response.StatusCode == http.StatusOK && result.Error != nil || response.StatusCode != http.StatusOK && result.Error == nil {
		return proto.ProviderAuthenticationMutationResponse{}, fmt.Errorf("provider authentication mutation status disagrees with its outcome")
	}
	if result.Error != nil {
		return result, result.Error
	}
	// A verified complete reply remains truthful if cancellation follows the
	// durable transaction. A canceled/incomplete read never reaches this point.
	return result, nil
}
