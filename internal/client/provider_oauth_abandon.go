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

func (c *Client) AbandonProviderOAuthLoginResult(ctx context.Context, id string, request providerauth.OAuthLoginAbandonRequest) (proto.ProviderOAuthLoginAbandonResponse, error) {
	if err := request.Validate(); err != nil {
		return proto.ProviderOAuthLoginAbandonResponse{}, err
	}
	if id == "" || strings.TrimSpace(id) != id || strings.ContainsAny(id, "/\\") || request.Target.WorkspaceID != id {
		return proto.ProviderOAuthLoginAbandonResponse{}, errors.New("OAuth abandonment target does not match workspace")
	}
	if err := ctx.Err(); err != nil {
		return proto.ProviderOAuthLoginAbandonResponse{}, err
	}
	body, err := json.Marshal(request)
	if err != nil || len(body) > proto.MaxProviderAuthRequestBytes {
		return proto.ProviderOAuthLoginAbandonResponse{}, errors.New("invalid OAuth abandonment request")
	}
	response, err := c.fixedAPIKeyRouteClient().sendReq(ctx, http.MethodPost, "/workspaces/"+url.PathEscape(id)+"/auth/oauth/abandon", nil, bytes.NewReader(body), http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return proto.ProviderOAuthLoginAbandonResponse{}, err
	}
	defer response.Body.Close()
	body, err = io.ReadAll(io.LimitReader(response.Body, proto.MaxProviderAuthResponseBytes+1))
	if err != nil {
		return proto.ProviderOAuthLoginAbandonResponse{}, err
	}
	result, err := proto.DecodeProviderOAuthLoginAbandonResponse(body, request)
	if err != nil {
		return proto.ProviderOAuthLoginAbandonResponse{}, err
	}
	if response.StatusCode == http.StatusOK && result.Error != nil || response.StatusCode != http.StatusOK && result.Error == nil {
		return proto.ProviderOAuthLoginAbandonResponse{}, errors.New("OAuth abandonment status disagrees with its response")
	}
	if result.Error != nil {
		return result, result.Error
	}
	return result, ctx.Err()
}
