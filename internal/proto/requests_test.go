package proto_test

import (
	"encoding/json"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestConfigProviderKeyRequestStringRoundTrip(t *testing.T) {
	t.Parallel()

	apiKey, err := json.Marshal("sk-test-123")
	require.NoError(t, err)

	owner := providerregistry.RegistrationOwner{ProviderID: "openai"}
	src := proto.ConfigProviderKeyRequest{
		Scope:      config.ScopeGlobal,
		ProviderID: "openai",
		Kind:       proto.APIKeyKindString,
		APIKey:     apiKey,
		Owner:      &owner,
	}
	b, err := json.Marshal(src)
	require.NoError(t, err)

	var got proto.ConfigProviderKeyRequest
	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, proto.APIKeyKindString, got.Kind)

	decoded, err := got.DecodeAPIKey()
	require.NoError(t, err)
	credential, ok := decoded.(config.ProviderAPIKeyCredential)
	require.True(t, ok, "expected owner-bound API key, got %T", decoded)
	require.Equal(t, owner, credential.Owner)
	require.Equal(t, "sk-test-123", credential.APIKey)
}

func TestConfigProviderKeyRequestOAuthRoundTrip(t *testing.T) {
	t.Parallel()

	tok := &oauth.Token{
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresIn:    60,
		ExpiresAt:    1234567890,
	}
	apiKey, err := json.Marshal(tok)
	require.NoError(t, err)

	owner := providerregistry.Registration{ProviderID: "codex", Construction: providerregistry.ConstructionCodex, OAuth: &providerregistry.OAuthCapability{}}.Owner()
	src := proto.ConfigProviderKeyRequest{
		Scope:      config.ScopeGlobal,
		ProviderID: "codex",
		Kind:       proto.APIKeyKindOAuth,
		APIKey:     apiKey,
		Owner:      &owner,
	}
	b, err := json.Marshal(src)
	require.NoError(t, err)

	var got proto.ConfigProviderKeyRequest
	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, proto.APIKeyKindOAuth, got.Kind)

	decoded, err := got.DecodeAPIKey()
	require.NoError(t, err)
	credential, ok := decoded.(config.ProviderOAuthCredential)
	require.True(t, ok, "expected config.ProviderOAuthCredential, got %T", decoded)
	require.Equal(t, owner, credential.Owner)
	require.Equal(t, tok, credential.Token)
}

func TestConfigProviderKeyRequestOAuthRequiresMatchingOwner(t *testing.T) {
	t.Parallel()
	token, err := json.Marshal(&oauth.Token{AccessToken: "access"})
	require.NoError(t, err)

	ownerless := proto.ConfigProviderKeyRequest{
		ProviderID: "codex",
		Kind:       proto.APIKeyKindOAuth,
		APIKey:     token,
	}
	_, err = ownerless.DecodeAPIKey()
	require.ErrorContains(t, err, "initiating owner is required")

	owner := providerregistry.Registration{ProviderID: "other", OAuth: &providerregistry.OAuthCapability{}}.Owner()
	mismatched := proto.ConfigProviderKeyRequest{
		ProviderID: "codex",
		Kind:       proto.APIKeyKindOAuth,
		APIKey:     token,
		Owner:      &owner,
	}
	_, err = mismatched.DecodeAPIKey()
	require.ErrorContains(t, err, "does not match request provider")
}

func TestConfigProviderKeyRequestRemovalRoundTrip(t *testing.T) {
	t.Parallel()
	owner := providerregistry.Registration{ProviderID: "codex", Construction: providerregistry.ConstructionCodex, OAuth: &providerregistry.OAuthCapability{}}.Owner()
	request := proto.ConfigProviderKeyRequest{
		Scope:      config.ScopeGlobal,
		ProviderID: "codex",
		Kind:       proto.APIKeyKindRemove,
		Owner:      &owner,
	}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	var decoded proto.ConfigProviderKeyRequest
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, proto.APIKeyKindRemove, decoded.Kind)
	require.Equal(t, owner, *decoded.Owner)
}

func TestConfigProviderKeyRequestUnknownKind(t *testing.T) {
	t.Parallel()

	req := proto.ConfigProviderKeyRequest{
		Kind:   proto.APIKeyKind("bogus"),
		APIKey: json.RawMessage(`"x"`),
	}
	_, err := req.DecodeAPIKey()
	require.Error(t, err)
	require.Contains(t, err.Error(), "bogus")
}

func TestConfigProviderKeyRequestMalformedPayload(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		kind proto.APIKeyKind
		raw  string
	}{
		{"string kind with object payload", proto.APIKeyKindString, `{"foo":"bar"}`},
		{"oauth kind with string payload", proto.APIKeyKindOAuth, `"not-a-token"`},
		{"oauth kind with invalid json", proto.APIKeyKindOAuth, `{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := proto.ConfigProviderKeyRequest{
				Kind:   tc.kind,
				APIKey: json.RawMessage(tc.raw),
			}
			_, err := req.DecodeAPIKey()
			require.Error(t, err)
		})
	}
}
