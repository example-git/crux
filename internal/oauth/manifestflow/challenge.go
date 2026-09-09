package manifestflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
)

func (e *Executor) CallbackRequirement() (oauth.CallbackRequirement, error) {
	r := oauth.CallbackRequirement{Mode: e.flow.Redirect.Mode}
	if r.Mode != "hosted-paste" {
		r.Path = e.flow.Redirect.CallbackPath
		if r.Path == "" {
			r.Path = "/callback"
		}
		if r.Mode == "loopback-fixed" {
			if e.flow.Redirect.Port <= 0 || e.flow.Redirect.Port > 65535 {
				return r, errors.New("OAuth callback port is invalid")
			}
			r.Port = uint16(e.flow.Redirect.Port)
		}
	}
	return r, r.Validate()
}

func (e *Executor) PrepareCode(ctx context.Context, port uint16) (*oauth.CodeChallenge, error) {
	requirement, err := e.CallbackRequirement()
	if err != nil {
		return nil, err
	}
	if err := requirement.ValidatePort(port); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return nil, err
	}
	timeout := time.Duration(e.flow.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	expiresAt := time.Now().Add(timeout)
	captured, err := e.challengeExecutor()
	if err != nil {
		return nil, err
	}
	verifier, challenge, err := createPKCE(captured.flow.PKCE)
	if err != nil {
		return nil, err
	}
	state := ""
	if captured.flow.Redirect.StateRequired {
		state, err = randomString(32)
		if err != nil {
			return nil, err
		}
	}
	redirectURI := captured.flow.Redirect.URI
	if requirement.Mode != "hosted-paste" {
		redirectURI = "http://localhost:" + strconv.Itoa(int(port)) + requirement.Path
	}
	if redirectURI == "" {
		return nil, errors.New("hosted OAuth flow has no redirect URI")
	}
	clientID, err := captured.eval(captured.flow.ClientID, nil)
	if err != nil {
		return nil, err
	}
	captured.flow.ClientID = manifest.Template{Kind: "literal", Value: clientID}
	authorizationURL, err := captured.authorizationURL(redirectURI, challenge, state)
	if err != nil {
		return nil, err
	}
	// These protocol fields must describe the captured parser and exchange.
	// Identical declarations are permitted; a conflicting override fails before
	// publishing a URL instead of advertising an authorization we cannot finish.
	method := ""
	if challenge != "" {
		method = "S256"
	}
	parsed, err := url.Parse(authorizationURL)
	if err != nil {
		return nil, errors.New("OAuth authorization URL is invalid")
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return nil, errors.New("OAuth authorization parameters are invalid")
	}
	for _, field := range []struct{ name, value string }{
		{"client_id", clientID}, {"state", state}, {"code_challenge", challenge},
		{"code_challenge_method", method}, {"response_type", "code"}, {"redirect_uri", redirectURI},
	} {
		if len(query[field.name]) > 1 || query.Get(field.name) != field.value {
			return nil, fmt.Errorf("OAuth authorization parameter %q conflicts with the captured challenge", field.name)
		}
	}
	return oauth.NewCodeChallenge(ctx, authorizationURL, expiresAt, func(ctx context.Context, input string) (*oauth.Token, error) {
		var code, returnedState, providerError string
		if requirement.Mode == "hosted-paste" {
			code, returnedState, providerError, err = parseHostedCallback(input)
		} else {
			var query url.Values
			query, err = url.ParseQuery(strings.TrimPrefix(input, "?"))
			if err == nil && (len(query["code"]) > 1 || len(query["state"]) > 1 || len(query["error"]) > 1) {
				err = errors.New("OAuth callback contains duplicate parameters")
			}
			code, returnedState, providerError = query.Get("code"), query.Get("state"), query.Get("error")
		}
		if err != nil {
			return nil, err
		}
		if providerError != "" {
			return nil, fmt.Errorf("OAuth authorization failed: %s", providerError)
		}
		if code == "" || returnedState != "" && returnedState != state || requirement.Mode != "hosted-paste" && captured.flow.Redirect.StateRequired && returnedState != state {
			return nil, errors.New("OAuth callback validation failed")
		}
		if err := providertransport.ValidateContextOwner(ctx); err != nil {
			return nil, err
		}
		token, err := captured.exchange(ctx, captured.flow.TokenRequest.Code, map[string]string{"oauth.code": code, "oauth.redirect_uri": redirectURI, "oauth.pkce_verifier": verifier, "oauth.state": state}, "")
		if err == nil {
			err = providertransport.ValidateContextOwner(ctx)
		}
		return token, err
	})
}

// Freeze private declarative inputs before exposing interaction data. The HTTP
// client retains the trusted adapter's transport; no configuration is resolved.
func (e *Executor) challengeExecutor() (*Executor, error) {
	type declaration struct {
		Flow      manifest.OAuthFlow
		Endpoints map[string]manifest.Endpoint
		Bindings  Bindings
	}
	data, err := json.Marshal(declaration{e.flow, e.endpoints, e.bindings})
	if err != nil {
		return nil, errors.New("OAuth challenge declaration is not serializable")
	}
	var state declaration
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&state); err != nil {
		return nil, errors.New("OAuth challenge declaration could not be captured")
	}
	copy := *e
	copy.flow, copy.endpoints, copy.bindings = state.Flow, state.Endpoints, state.Bindings
	if e.client != nil {
		client := *e.client
		copy.client = &client
	}
	return &copy, nil
}
