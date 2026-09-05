package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

// captureClient returns a Client that talks to the given test server,
// plus a channel receiving the parsed request body for each call.
func captureClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	c, err := NewClient(t.TempDir(), "tcp", u.Host)
	require.NoError(t, err)
	return c
}

func TestSetProviderAPIKeyStringSendsKind(t *testing.T) {
	t.Parallel()

	var got proto.ConfigProviderKeyRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &got))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	owner := providerregistry.RegistrationOwner{ProviderID: "openai"}
	require.NoError(t, c.SetProviderAPIKey(context.Background(), "ws1", config.ScopeGlobal, "openai", config.ProviderAPIKeyCredential{Owner: owner, APIKey: "sk-xyz"}))

	require.Equal(t, proto.APIKeyKindString, got.Kind)
	require.Equal(t, "openai", got.ProviderID)
	require.Equal(t, config.ScopeGlobal, got.Scope)
	decoded, err := got.DecodeAPIKey()
	require.NoError(t, err)
	require.Equal(t, config.ProviderAPIKeyCredential{Owner: owner, APIKey: "sk-xyz"}, decoded)
}

func TestSetProviderAPIKeyOAuthSendsKind(t *testing.T) {
	t.Parallel()

	var got proto.ConfigProviderKeyRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &got))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tok := &oauth.Token{AccessToken: "a", RefreshToken: "r", ExpiresIn: 60, ExpiresAt: 1234567890}
	owner := providerregistry.Registration{ProviderID: "codex", Construction: providerregistry.ConstructionCodex, OAuth: &providerregistry.OAuthCapability{}}.Owner()
	credential := config.ProviderOAuthCredential{Owner: owner, Token: tok}
	c := captureClient(t, srv)
	require.NoError(t, c.SetProviderAPIKey(context.Background(), "ws1", config.ScopeGlobal, "codex", credential))

	require.Equal(t, proto.APIKeyKindOAuth, got.Kind)
	require.Equal(t, owner, *got.Owner)
	decoded, err := got.DecodeAPIKey()
	require.NoError(t, err)
	require.Equal(t, credential, decoded.(config.ProviderOAuthCredential))
}

func TestRemoveProviderCredentialsSendsExactOwner(t *testing.T) {
	t.Parallel()

	var got proto.ConfigProviderKeyRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/workspaces/ws1/config/provider-key", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	owner := providerregistry.Registration{ProviderID: "codex", Construction: providerregistry.ConstructionCodex, OAuth: &providerregistry.OAuthCapability{}}.Owner()
	require.NoError(t, captureClient(t, srv).RemoveProviderCredentials(t.Context(), "ws1", config.ScopeGlobal, owner))
	require.Equal(t, proto.APIKeyKindRemove, got.Kind)
	require.Equal(t, "codex", got.ProviderID)
	require.Equal(t, owner, *got.Owner)
}

func TestRefreshOAuthTokenSendsCompleteOwner(t *testing.T) {
	t.Parallel()

	var got proto.ConfigRefreshOAuthRequest
	var topLevel map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/workspaces/ws1/config/refresh-oauth", r.URL.Path)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &got))
		require.NoError(t, json.Unmarshal(body, &topLevel))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	owner := providerregistry.RegistrationOwner{
		ProviderID:           "same-id",
		AccountNamespace:     "same-id.accounts",
		Construction:         providerregistry.ConstructionOpenAIResponses,
		CompatibilityAdapter: providerregistry.ConstructionCodex,
		HasOAuth:             true,
		OAuthAdapter:         providerregistry.LoginBrowser,
		OAuthFlowID:          "same-id-flow",
		HasManifest:          true,
		ManifestID:           "plugin.same-id",
		ManifestVersion:      "1.2.3",
	}
	require.NoError(t, captureClient(t, srv).RefreshOAuthToken(t.Context(), "ws1", config.ScopeWorkspace, owner))
	require.Equal(t, config.ScopeWorkspace, got.Scope)
	require.Equal(t, owner, got.Owner)
	require.Contains(t, topLevel, "owner")
	require.NotContains(t, topLevel, "provider_id")
}

func TestRefreshOAuthTokenRejectsMissingOwnerLocally(t *testing.T) {
	t.Parallel()

	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := captureClient(t, srv).RefreshOAuthToken(t.Context(), "ws1", config.ScopeGlobal, providerregistry.RegistrationOwner{})
	require.ErrorContains(t, err, "initiating owner is required")
	select {
	case <-called:
		t.Fatal("server should not have been reached")
	default:
	}
}

func TestUpdatePreferredModelSendsCompleteOwner(t *testing.T) {
	t.Parallel()

	var got proto.ConfigModelRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/workspaces/ws1/config/model", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		state := config.AgentModelState{
			Large: &config.OwnedSelectedModel{Model: got.Model, Owner: got.Owner},
		}
		require.NoError(t, json.NewEncoder(w).Encode(state))
	}))
	defer srv.Close()

	owner := providerregistry.RegistrationOwner{
		ProviderID:           "same-id",
		AccountNamespace:     "same-id.accounts",
		Construction:         providerregistry.ConstructionOpenAIResponses,
		CompatibilityAdapter: providerregistry.ConstructionCodex,
		HasOAuth:             true,
		OAuthAdapter:         providerregistry.LoginBrowser,
		OAuthFlowID:          "same-id-flow",
		HasManifest:          true,
		ManifestID:           "plugin.same-id",
		ManifestVersion:      "1.2.3",
	}
	model := config.SelectedModel{Provider: owner.ProviderID, Model: "model-1"}
	state, err := captureClient(t, srv).UpdatePreferredModel(t.Context(), "ws1", config.ScopeWorkspace, config.SelectedModelTypeLarge, model, owner)
	require.NoError(t, err)
	require.Equal(t, &config.OwnedSelectedModel{Model: model, Owner: owner}, state.Large)
	require.Equal(t, config.ScopeWorkspace, got.Scope)
	require.Equal(t, config.SelectedModelTypeLarge, got.ModelType)
	require.Equal(t, model, got.Model)
	require.Equal(t, owner, got.Owner)
}

func TestUpdatePreferredModelRejectsInvalidOwnerLocally(t *testing.T) {
	t.Parallel()

	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	client := captureClient(t, srv)
	model := config.SelectedModel{Provider: "same-id", Model: "model-1"}

	_, err := client.UpdatePreferredModel(t.Context(), "ws1", config.ScopeGlobal, config.SelectedModelTypeLarge, model, providerregistry.RegistrationOwner{})
	require.ErrorContains(t, err, "initiating owner is required")
	_, err = client.UpdatePreferredModel(t.Context(), "ws1", config.ScopeGlobal, config.SelectedModelTypeLarge, model, providerregistry.RegistrationOwner{ProviderID: "replacement"})
	require.ErrorContains(t, err, "does not match model provider")
	select {
	case <-called:
		t.Fatal("server should not have been reached")
	default:
	}
}

func TestSetProviderDisabledSendsCompleteOwner(t *testing.T) {
	t.Parallel()

	var got proto.ConfigSetRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/workspaces/ws1/config/set", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	owner := providerregistry.Registration{ProviderID: "codex", Construction: providerregistry.ConstructionCodex, OAuth: &providerregistry.OAuthCapability{}}.Owner()
	require.NoError(t, captureClient(t, srv).SetProviderDisabled(t.Context(), "ws1", config.ScopeGlobal, owner, true))
	require.Equal(t, config.ScopeGlobal, got.Scope)
	require.Equal(t, "providers.codex.disable", got.Key)
	require.Equal(t, true, got.Value)
	require.Equal(t, owner, *got.Owner)
}

func TestSetProviderAPIKeyUnsupportedTypeFailsLocally(t *testing.T) {
	t.Parallel()

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	err := c.SetProviderAPIKey(context.Background(), "ws1", config.ScopeGlobal, "x", 42)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported api key type")
	require.False(t, called, "server should not have been reached")
}

func TestSetProviderAPIKeyNilOAuthFailsLocally(t *testing.T) {
	t.Parallel()

	c := captureClient(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	var tok *oauth.Token
	err := c.SetProviderAPIKey(context.Background(), "ws1", config.ScopeGlobal, "x", tok)
	require.Error(t, err)
}

func TestListMCPPrompts(t *testing.T) {
	t.Parallel()

	want := []proto.MCPPrompt{
		{
			ID:          "server:review",
			Title:       "Review changes",
			Description: "Review the current changes.",
			PromptID:    "review",
			ClientID:    "server",
			Arguments: []proto.MCPPromptArgument{
				{ID: "focus", Title: "Focus", Description: "Area to review", Required: true},
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/workspaces/ws1/mcp/prompts", r.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode(want))
	}))
	defer srv.Close()

	got, err := captureClient(t, srv).ListMCPPrompts(t.Context(), "ws1")
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestListMCPPromptsNonOKStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	_, err := c.ListMCPPrompts(t.Context(), "ws1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "status code 500")
}

func TestListMCPPromptsMalformedBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	_, err := c.ListMCPPrompts(t.Context(), "ws1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to decode MCP prompts")
}
