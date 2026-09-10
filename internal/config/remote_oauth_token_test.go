package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/redact"
	"github.com/stretchr/testify/require"
)

func namespaceFreeOAuthStore(t *testing.T) (*ConfigStore, RemoteRuntimeProposal, *oauth.Token, string) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "namespace-free.plugin")
	require.NoError(t, os.CopyFS(source, os.DirFS("../../docs/provider-plugins/examples/responses-oauth.plugin")))
	data, err := os.ReadFile(filepath.Join(source, "manifest.json"))
	require.NoError(t, err)
	var declaration manifest.Manifest
	require.NoError(t, json.Unmarshal(data, &declaration))
	declaration.Provider.AccountNamespace = ""
	data, err = json.Marshal(declaration)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	values := map[string]string{"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(ProviderProfilePluginNative), "CRUX_PROVIDER_PLUGINS": declaration.Provider.ID}
	installTrustedProviderBundle(t, values["CRUX_GLOBAL_DATA"], values["CRUX_CACHE_DIR"], source)
	require.NoDirExists(t, values["AI_CLI_DIR"], "namespace-free installation must not access accounts")
	token := &oauth.Token{AccessToken: "namespace-access-$LITERAL", RefreshToken: "namespace-refresh-secret", ExpiresIn: 3600, ExpiresAt: time.Now().Add(time.Hour).Unix(), Client: &oauth.OAuthClient{ClientID: "namespace-client-id", ClientSecret: "namespace-client-secret", AuthURL: "https://auth.example/authorize", TokenURL: "https://auth.example/token", AuthStyle: 2}}
	data, err = json.Marshal(map[string]any{"providers": map[string]any{declaration.Provider.ID: map[string]any{"plugin": map[string]string{"id": declaration.ID}, "api_key": token.AccessToken, "oauth": token, "configuration": map[string]string{"oauth_client_id": "captured-client"}}}, "models": map[string]any{"large": map[string]string{"provider": declaration.Provider.ID, "model": "example-reasoner"}, "small": map[string]string{"provider": declaration.Provider.ID, "model": "example-small"}}})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(values["CRUX_GLOBAL_CONFIG"], 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(values["CRUX_GLOBAL_DATA"], "crux.json"), data, 0o600))
	store, err := LoadIsolated(root, filepath.Join(root, "workspace"), false, env.NewFromMap(values))
	require.NoError(t, err)
	require.NoDirExists(t, values["AI_CLI_DIR"], "namespace-free loading must not access accounts")
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.NoDirExists(t, values["AI_CLI_DIR"], "namespace-free collection must not access accounts")
	return store, proposal, token, root
}

func TestNamespaceFreeOAuthInstalledCollectionAndDetachedAdmission(t *testing.T) {
	local, proposal, token, root := namespaceFreeOAuthStore(t)
	require.Len(t, proposal.Credentials, 1)
	binding := proposal.Credentials[0]
	require.True(t, binding.Owner.HasOAuth)
	require.Empty(t, binding.Owner.AccountNamespace)
	require.Nil(t, binding.Account)
	require.Empty(t, binding.APIKey)
	require.Equal(t, token, binding.OAuthToken)
	require.NotContains(t, fmt.Sprintf("%+v", binding), token.AccessToken)
	require.NotContains(t, fmt.Sprintf("%#v", binding), token.Client.ClientSecret)
	capture, err := local.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.NoError(t, capture.ValidateAcceptedAuthentication(proposal, proposal.CollectionConfig()))
	// The rollout catalogue independently retains Copilot, whose global status
	// capture takes an account lock. The namespace-free credential creates no
	// database or account identity; installation/load/collection above take no lock.
	require.Equal(t, []string{"copilot"}, local.Config().ProviderAccountNamespaces())
	require.NoFileExists(t, filepath.Join(root, "accounts", "accounts.json"))
	serverRoot := t.TempDir()
	receiver, err := CompileRemoteRuntime(serverRoot, filepath.Join(serverRoot, "workspace"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"HOME": serverRoot, "AI_CLI_DIR": filepath.Join(serverRoot, "forbidden"), "LITERAL": "ambient-substitution"}))
	require.NoError(t, err)
	receiver.RegisterRemoteRuntimeSecrets()
	accepted := receiver.RuntimeSnapshot()
	got, ok := accepted.ClientOAuthToken(binding.Owner)
	require.True(t, ok)
	require.Equal(t, token, got)
	got.Client.ClientSecret = "caller-mutation"
	proposal.Credentials[0].OAuthToken.Client.ClientSecret = "proposal-mutation"
	got, ok = accepted.ClientOAuthToken(binding.Owner)
	require.True(t, ok)
	require.Equal(t, token.Client.ClientSecret, got.Client.ClientSecret)
	provider, _ := receiver.Config().Providers.Get(binding.Owner.ProviderID)
	require.Equal(t, token, provider.OAuthToken)
	require.Equal(t, token.AccessToken, provider.APIKey)
	_, account := accepted.EphemeralAccount(binding.Owner)
	require.False(t, account)
	require.Empty(t, receiver.RemoteAuthority().Accounts[0].AccountID)
	public, err := json.Marshal(receiver.Config().RedactedForTransport())
	require.NoError(t, err)
	for _, secret := range []string{token.AccessToken, token.RefreshToken, token.Client.ClientSecret} {
		require.NotContains(t, string(public), secret)
		require.Equal(t, redact.Replacement, redact.String(secret))
	}
	require.NoDirExists(t, filepath.Join(serverRoot, "forbidden"))
}

func TestNamespaceFreeOAuthBindingValidationAndCompleteTokenIdentity(t *testing.T) {
	_, proposal, token, _ := namespaceFreeOAuthStore(t)
	copyProposal := func() RemoteRuntimeProposal {
		data, err := json.Marshal(proposal)
		require.NoError(t, err)
		var result RemoteRuntimeProposal
		require.NoError(t, json.Unmarshal(data, &result))
		return result
	}
	for _, edit := range []func(*oauth.Token){func(v *oauth.Token) { v.AccessToken += "x" }, func(v *oauth.Token) { v.RefreshToken += "x" }, func(v *oauth.Token) { v.ExpiresIn++ }, func(v *oauth.Token) { v.ExpiresAt++ }, func(v *oauth.Token) { v.Client.ClientID += "x" }, func(v *oauth.Token) { v.Client.ClientSecret += "x" }, func(v *oauth.Token) { v.Client.AuthURL += "x" }, func(v *oauth.Token) { v.Client.TokenURL += "x" }, func(v *oauth.Token) { v.Client.AuthStyle++ }} {
		changed := copyProposal()
		edit(changed.Credentials[0].OAuthToken)
		require.NotEqual(t, OAuthTokenCredentialID(token), OAuthTokenCredentialID(changed.Credentials[0].OAuthToken))
		changed = sealRemoteRuntime(t, changed)
		require.NotEqual(t, proposal.Digest, changed.Digest)
	}
	for _, kind := range []string{"account-and-token", "key-and-token", "unavailable-and-token", "empty-token", "oversized-token", "wrong-owner"} {
		t.Run(kind, func(t *testing.T) {
			invalid := copyProposal()
			binding := &invalid.Credentials[0]
			switch kind {
			case "account-and-token":
				binding.Account = &accounts.Entry{ID: "invented", AccessToken: token.AccessToken}
			case "key-and-token":
				binding.APIKey = token.AccessToken
			case "unavailable-and-token":
				binding.Unavailable = true
			case "empty-token":
				binding.OAuthToken.AccessToken = ""
			case "oversized-token":
				binding.OAuthToken.Client.ClientSecret = strings.Repeat("x", maxRemoteOAuthTokenBytes)
			case "wrong-owner":
				binding.Owner.AccountNamespace = "invented"
			}
			invalid = sealRemoteRuntime(t, invalid)
			root := t.TempDir()
			_, err := CompileRemoteRuntime(root, filepath.Join(root, "workspace"), false, invalid, strings.Repeat("a", 64), env.NewFromMap(nil))
			require.Error(t, err)
		})
	}
}
