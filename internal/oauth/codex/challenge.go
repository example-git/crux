package codex

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providertransport"
)

func CallbackRequirement() oauth.CallbackRequirement {
	return oauth.CallbackRequirement{Mode: "loopback-fixed", Port: redirectPort, Path: redirectPath}
}

// PrepareCode creates owner-side PKCE/state without opening a listener/browser.
func (client Client) PrepareCode(ctx context.Context, port uint16) (*oauth.CodeChallenge, error) {
	expiresAt := time.Now().Add(authorizeTimeout)
	if err := CallbackRequirement().ValidatePort(port); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return nil, err
	}
	clientID, err := oauthClientID(ctx)
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
	redirectURI := fmt.Sprintf("http://localhost:%d%s", port, redirectPath)
	return oauth.NewCodeChallenge(ctx, client.buildAuthorizeURL(redirectURI, challenge, state, clientID), expiresAt, func(ctx context.Context, input string) (*oauth.Token, error) {
		query, err := url.ParseQuery(strings.TrimPrefix(input, "?"))
		if err != nil || len(query["state"]) != 1 || len(query["code"]) != 1 || query.Get("state") != state || query.Get("code") == "" {
			return nil, errors.New("codex OAuth callback validation failed")
		}
		if err := providertransport.ValidateContextOwner(ctx); err != nil {
			return nil, err
		}
		token, err := client.exchangeCodeWithClientID(ctx, query.Get("code"), verifier, redirectURI, clientID)
		if err == nil {
			err = providertransport.ValidateContextOwner(ctx)
		}
		return token, err
	})
}
