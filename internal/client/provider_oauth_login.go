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

// Interaction requests target one retained login. No method creates a new ID,
// retries automatically, follows a redirect, or performs provider token I/O.
func (c *Client) providerOAuthInteraction(ctx context.Context, id string, ref providerauth.OAuthLoginRef, action string, request any, maximum int, decode func([]byte) (proto.ProviderOAuthLoginResponse, error)) (proto.ProviderOAuthLoginResponse, error) {
	if err := ref.Validate(); err != nil {
		return proto.ProviderOAuthLoginResponse{}, err
	}
	if err := ctx.Err(); err != nil {
		return proto.ProviderOAuthLoginResponse{}, err
	}
	if err := validateOAuthWorkspaceID(id, ref); err != nil {
		return proto.ProviderOAuthLoginResponse{}, errors.New("OAuth login target does not match workspace")
	}
	body, err := json.Marshal(request)
	if err != nil || len(body) > maximum {
		return proto.ProviderOAuthLoginResponse{}, errors.New("invalid OAuth login request")
	}
	response, err := c.fixedAPIKeyRouteClient().sendReq(ctx, http.MethodPost, "/workspaces/"+url.PathEscape(id)+"/auth/oauth/"+action, nil, bytes.NewReader(body), http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return proto.ProviderOAuthLoginResponse{}, fmt.Errorf("OAuth login request: %w", err)
	}
	defer response.Body.Close()
	body, err = io.ReadAll(io.LimitReader(response.Body, proto.MaxProviderAuthResponseBytes+1))
	if err != nil {
		return proto.ProviderOAuthLoginResponse{}, fmt.Errorf("read OAuth login response: %w", err)
	}
	result, err := decode(body)
	if err != nil {
		return proto.ProviderOAuthLoginResponse{}, err
	}
	if response.StatusCode == http.StatusOK && result.Error != nil || response.StatusCode != http.StatusOK && result.Error == nil {
		return proto.ProviderOAuthLoginResponse{}, errors.New("OAuth login status disagrees with its outcome")
	}
	if result.Error != nil {
		return result, result.Error
	}
	return result, nil
}

func (c *Client) BeginProviderOAuthLogin(ctx context.Context, id string, request providerauth.OAuthLoginRequest) (proto.ProviderOAuthLoginResponse, error) {
	return c.providerOAuthInteraction(ctx, id, request, "begin", request, proto.MaxProviderAuthRequestBytes, func(body []byte) (proto.ProviderOAuthLoginResponse, error) {
		return proto.DecodeProviderOAuthLoginBeginResponse(body, request)
	})
}
func (c *Client) BindProviderOAuthLogin(ctx context.Context, id string, request providerauth.OAuthLoginBindRequest) (proto.ProviderOAuthLoginResponse, error) {
	if err := request.Validate(); err != nil {
		return proto.ProviderOAuthLoginResponse{}, err
	}
	return c.providerOAuthInteraction(ctx, id, request.Login, "bind", request, proto.MaxProviderAuthRequestBytes, func(body []byte) (proto.ProviderOAuthLoginResponse, error) {
		return proto.DecodeProviderOAuthLoginBindResponse(body, request)
	})
}
func (c *Client) SubmitProviderOAuthLoginCode(ctx context.Context, id string, request providerauth.OAuthLoginCodeRequest) (proto.ProviderOAuthLoginResponse, error) {
	if err := request.Validate(); err != nil {
		return proto.ProviderOAuthLoginResponse{}, err
	}
	return c.providerOAuthInteraction(ctx, id, request.Login, "code", request, proto.MaxProviderOAuthCodeRequestBytes, func(body []byte) (proto.ProviderOAuthLoginResponse, error) {
		return proto.DecodeProviderOAuthLoginCodeResponse(body, request)
	})
}
func (c *Client) WaitProviderOAuthLogin(ctx context.Context, id string, ref providerauth.OAuthLoginRef, after uint64) (proto.ProviderOAuthLoginResponse, error) {
	request := proto.ProviderOAuthLoginWaitRequest{Login: ref, After: after}
	return c.providerOAuthInteraction(ctx, id, ref, "wait", request, proto.MaxProviderAuthRequestBytes, func(body []byte) (proto.ProviderOAuthLoginResponse, error) {
		return proto.DecodeProviderOAuthLoginWaitResponse(body, ref, after)
	})
}
func (c *Client) CancelProviderOAuthLogin(ctx context.Context, id string, ref providerauth.OAuthLoginRef) (proto.ProviderOAuthLoginResponse, error) {
	return c.providerOAuthInteraction(ctx, id, ref, "cancel", ref, proto.MaxProviderAuthRequestBytes, func(body []byte) (proto.ProviderOAuthLoginResponse, error) {
		return proto.DecodeProviderOAuthLoginCancelResponse(body, ref)
	})
}

// Complete returns the exact local progress/acknowledgement response. An absent
// or lost response remains uncertain; retry only this original login reference.
func (c *Client) CompleteProviderOAuthLogin(ctx context.Context, id string, ref providerauth.OAuthLoginRef) (proto.ProviderAuthenticationMutationResponse, error) {
	if err := ref.Validate(); err != nil {
		return proto.ProviderAuthenticationMutationResponse{}, err
	}
	if err := validateOAuthWorkspaceID(id, ref); err != nil {
		return proto.ProviderAuthenticationMutationResponse{}, err
	}
	return c.fixedAPIKeyRouteClient().providerAuthMutation(ctx, id, ref.Target, "oauth/complete", ref, func(body []byte) (proto.ProviderAuthenticationMutationResponse, error) {
		return proto.DecodeProviderOAuthLoginCompleteResponse(body, ref)
	})
}

func validateOAuthWorkspaceID(id string, ref providerauth.OAuthLoginRef) error {
	if id == "" || id == "." || id == ".." || strings.TrimSpace(id) != id || strings.ContainsAny(id, "/\\") || ref.Target.WorkspaceID != id {
		return errors.New("OAuth login target does not match workspace")
	}
	return nil
}
