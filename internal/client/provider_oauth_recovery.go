package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

func (c *Client) RecoverProviderOAuthLogin(ctx context.Context, id string, request providerauth.OAuthLoginRecoveryRequest) (proto.ProviderOAuthLoginResponse, error) {
	if err := request.Validate(); err != nil {
		return proto.ProviderOAuthLoginResponse{}, err
	}
	return c.providerOAuthInteraction(ctx, id, request.Login, "recover", request, proto.MaxProviderAuthRequestBytes, func(body []byte) (proto.ProviderOAuthLoginResponse, error) {
		return proto.DecodeProviderOAuthLoginRecoverResponse(body, request)
	})
}
func (c *Client) ListProviderOAuthLoginResults(ctx context.Context, id string, target providerauth.Target) (proto.ProviderOAuthLoginRecoveryListResponse, error) {
	if err := target.Validate(); err != nil {
		return proto.ProviderOAuthLoginRecoveryListResponse{}, err
	}
	if id == "" || strings.TrimSpace(id) != id || strings.ContainsAny(id, "/\\") || target.WorkspaceID != id {
		return proto.ProviderOAuthLoginRecoveryListResponse{}, errors.New("OAuth recovery target does not match workspace")
	}
	if err := ctx.Err(); err != nil {
		return proto.ProviderOAuthLoginRecoveryListResponse{}, err
	}
	body, err := json.Marshal(target)
	if err != nil || len(body) > proto.MaxProviderAuthRequestBytes {
		return proto.ProviderOAuthLoginRecoveryListResponse{}, errors.New("invalid OAuth recovery listing request")
	}
	response, err := c.fixedAPIKeyRouteClient().sendReq(ctx, http.MethodPost, "/workspaces/"+url.PathEscape(id)+"/auth/oauth/results", nil, bytes.NewReader(body), http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return proto.ProviderOAuthLoginRecoveryListResponse{}, err
	}
	defer response.Body.Close()
	body, err = io.ReadAll(io.LimitReader(response.Body, proto.MaxProviderAuthResponseBytes+1))
	if err != nil {
		return proto.ProviderOAuthLoginRecoveryListResponse{}, err
	}
	result, err := proto.DecodeProviderOAuthLoginRecoveryListResponse(body, target)
	if err != nil {
		return proto.ProviderOAuthLoginRecoveryListResponse{}, err
	}
	if response.StatusCode == http.StatusOK && result.Error != nil || response.StatusCode != http.StatusOK && result.Error == nil {
		return proto.ProviderOAuthLoginRecoveryListResponse{}, errors.New("OAuth recovery listing status disagrees with its response")
	}
	if result.Error != nil {
		return result, result.Error
	}
	return result, ctx.Err()
}
