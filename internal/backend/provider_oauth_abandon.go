package backend

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

func (b *Backend) AbandonProviderOAuthLoginResult(ctx context.Context, id string, request providerauth.OAuthLoginAbandonRequest) (proto.ProviderOAuthLoginAbandonResponse, error) {
	response := proto.ProviderOAuthLoginAbandonResponse{Outcome: providerauth.OAuthLoginAbandonOutcome{Request: request}}
	fail := func(err error) (proto.ProviderOAuthLoginAbandonResponse, error) {
		response.Error = proto.NewProviderAuthenticationError(err)
		return response, err
	}
	if err := request.Validate(); err != nil {
		return fail(err)
	}
	if request.Target.WorkspaceID != id {
		return fail(errors.New("OAuth abandonment target does not match workspace"))
	}
	ws, ctx, done, err := b.beginProviderAuthRead(ctx, id)
	if err != nil {
		return fail(err)
	}
	defer done()
	outcome, err := ws.providerAuth.AbandonOAuthLoginResult(ctx, request)
	response.Outcome = outcome
	if err != nil {
		return fail(err)
	}
	if err := response.Validate(request); err != nil {
		return fail(err)
	}
	return response, nil
}
