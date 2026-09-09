package client

import (
	"context"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

func (c *Client) RemoveProviderAccount(ctx context.Context, id string, request providerauth.RemoveRequest) (proto.ProviderAuthenticationMutationResponse, error) {
	if err := request.Validate(); err != nil {
		return proto.ProviderAuthenticationMutationResponse{}, err
	}
	return c.fixedAPIKeyRouteClient().providerAuthMutation(ctx, id, request.Target, "remove", request, func(body []byte) (proto.ProviderAuthenticationMutationResponse, error) {
		return proto.DecodeProviderAuthRemoveResponse(body, request)
	})
}
