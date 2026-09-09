package gemini

import (
	"context"
	"errors"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providertransport"
)

func CallbackRequirement() oauth.CallbackRequirement {
	return oauth.CallbackRequirement{Mode: "hosted-paste"}
}

func PrepareCode(ctx context.Context, port uint16) (*oauth.CodeChallenge, error) {
	if err := CallbackRequirement().ValidatePort(port); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return nil, err
	}
	clientID, clientSecret, err := oauthClientCredentials(ctx)
	if err != nil {
		return nil, err
	}
	verifier, challenge, err := createPKCE()
	if err != nil {
		return nil, err
	}
	state, err := randomString(32)
	if err != nil {
		return nil, err
	}
	return oauth.NewCodeChallenge(ctx, buildAuthorizeURL(challenge, state, clientID), time.Time{}, func(ctx context.Context, input string) (*oauth.Token, error) {
		code, returnedState, err := parsePastedCode(input)
		if err != nil {
			return nil, err
		}
		if returnedState != "" && returnedState != state {
			return nil, errors.New("OAuth state mismatch — possible CSRF, please try again")
		}
		if err := providertransport.ValidateContextOwner(ctx); err != nil {
			return nil, err
		}
		token, err := exchangeCodeWithClientCredentials(ctx, code, verifier, clientID, clientSecret)
		if err == nil {
			err = providertransport.ValidateContextOwner(ctx)
		}
		return token, err
	})
}
