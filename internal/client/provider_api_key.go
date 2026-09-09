package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

// CheckProviderAPIKey sends the source only to its selected workspace owner.
// A transport failure is uncertain; only the exact CheckID/request may be retried.
// The SDK never resolves input, probes a provider or retries automatically.
func (c *Client) CheckProviderAPIKey(ctx context.Context, id string, request providerauth.APIKeyCheckRequest) (proto.ProviderAPIKeyCheckResponse, error) {
	if err := request.Validate(); err != nil {
		return proto.ProviderAPIKeyCheckResponse{}, err
	}
	if err := ctx.Err(); err != nil {
		return proto.ProviderAPIKeyCheckResponse{}, err
	}
	if id == "" || strings.TrimSpace(id) != id || strings.ContainsAny(id, "/\\") || request.Target.WorkspaceID != id {
		return proto.ProviderAPIKeyCheckResponse{}, errors.New("API key check target does not match workspace")
	}
	body, err := json.Marshal(request)
	if err != nil || len(body) > proto.MaxProviderAPIKeyCheckRequestBytes {
		return proto.ProviderAPIKeyCheckResponse{}, errors.New("invalid API key check request")
	}
	response, err := c.fixedAPIKeyRouteClient().sendReq(ctx, http.MethodPost, "/workspaces/"+url.PathEscape(id)+"/auth/api-key/check", nil, bytes.NewReader(body), http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return proto.ProviderAPIKeyCheckResponse{}, fmt.Errorf("API key check request: %w", err)
	}
	defer response.Body.Close()
	body, err = io.ReadAll(io.LimitReader(response.Body, proto.MaxProviderAuthResponseBytes+1))
	if err != nil {
		return proto.ProviderAPIKeyCheckResponse{}, fmt.Errorf("read API key check response: %w", err)
	}
	result, err := proto.DecodeProviderAPIKeyCheckResponse(body, request)
	if err != nil {
		return proto.ProviderAPIKeyCheckResponse{}, err
	}
	if response.StatusCode == http.StatusOK && result.Error != nil || response.StatusCode != http.StatusOK && result.Error == nil {
		return proto.ProviderAPIKeyCheckResponse{}, errors.New("API key check status disagrees with its outcome")
	}
	if result.Error != nil {
		return result, result.Error
	}
	return result, nil
}

// SaveCheckedProviderAPIKey has no source input. The owning service commits its
// exact retained check preparation and returns request-bound partial progress.
func (c *Client) SaveCheckedProviderAPIKey(ctx context.Context, id string, request providerauth.APIKeySaveRequest) (proto.ProviderAuthenticationMutationResponse, error) {
	if err := request.Validate(); err != nil {
		return proto.ProviderAuthenticationMutationResponse{}, err
	}
	return c.fixedAPIKeyRouteClient().providerAuthMutation(ctx, id, request.Target, "api-key/save", request, func(body []byte) (proto.ProviderAuthenticationMutationResponse, error) {
		return proto.DecodeProviderAPIKeySaveResponse(body, request)
	})
}

// A redirect is not an authenticated check/save receipt. In particular, 307/308
// must never replay secret source to a new endpoint. Copy rather than mutate the
// shared HTTP client, preserving its existing transport and TLS authority.
func (c *Client) fixedAPIKeyRouteClient() *Client {
	copy := *c
	httpClient := *c.h
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	copy.h = &httpClient
	return &copy
}
