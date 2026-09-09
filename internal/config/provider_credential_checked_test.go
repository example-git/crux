package config

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/require"
)

func resolvedCredentialStore(t *testing.T, provider ProviderConfig) *ConfigStore {
	t.Helper()
	for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	root := t.TempDir()
	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{provider.ID: provider}), Models: map[SelectedModelType]SelectedModel{
		SelectedModelTypeLarge: {Provider: provider.ID, Model: "main"},
		SelectedModelTypeSmall: {Provider: provider.ID, Model: "small"},
	}}
	cfg.setDefaults(root, filepath.Join(root, "state"))
	store := NewTestStore(cfg)
	store.globalDataPath = filepath.Join(root, "crux.json")
	require.NoError(t, os.WriteFile(store.globalDataPath, mustMarshalConfig(cfg), 0o600))
	return store
}

func resolvedCredentialHeader(t *testing.T, requests <-chan string) string {
	t.Helper()
	select {
	case header := <-requests:
		return header
	case <-time.After(time.Second):
		t.Fatal("expected actual HTTPS credential request")
		return ""
	}
}

func resolvedCredentialFixture(t *testing.T) (*ConfigStore, ProviderConfig, providerregistry.RegistrationOwner) {
	t.Helper()
	provider := ProviderConfig{ID: "checked-fixture", Type: catalog.TypeOpenAICompat, BaseURL: "https://example.invalid/v1", APIKey: "$SOURCE", Owner: &ProviderOwnerReference{Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}, Models: []catalog.Model{{ID: "main"}, {ID: "small"}}}
	store := resolvedCredentialStore(t, provider)
	owner, ok := store.RuntimeSnapshot().ProviderOwner(provider.ID)
	require.True(t, ok)
	provider, err := bindResolvedProviderAPIKey(store.RuntimeSnapshot(), provider, owner, "$SOURCE", "literal-$NOT_AN_EXPRESSION")
	require.NoError(t, err)
	store.Config().Providers.Set(provider.ID, provider)
	return store, provider, owner
}

func TestCheckedProviderCredentialProbeUsesOneResolvedLiteral(t *testing.T) {
	root := t.TempDir()
	marker, counter := filepath.Join(root, "secret-must-not-execute"), filepath.Join(root, "input-evaluations")
	literal := "synthetic-$(printf y > " + marker + ")-$CHECKED_SECRET"
	source := fmt.Sprintf("$(printf x >> '%s'; printf '%%s' '%s')", counter, literal)
	resolver := NewShellVariableResolver(env.NewFromMap(map[string]string{"CHECKED_SECRET": "must-not-substitute"}))
	resolved, err := resolver.ResolveValue(source)
	require.NoError(t, err)
	require.Equal(t, literal, resolved)
	received := make(chan string, 1)
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(host.Close)
	prior := http.DefaultClient
	http.DefaultClient = host.Client()
	t.Cleanup(func() { http.DefaultClient = prior })
	provider := ProviderConfig{ID: "checked-fixture", Type: catalog.TypeOpenAICompat, BaseURL: host.URL, APIKey: source, Owner: &ProviderOwnerReference{Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}}
	store := resolvedCredentialStore(t, provider)
	owner, ok := store.RuntimeSnapshot().ProviderOwner(provider.ID)
	require.True(t, ok)
	provider, err = bindResolvedProviderAPIKey(store.RuntimeSnapshot(), provider, owner, source, resolved)
	require.NoError(t, err)
	require.NoError(t, provider.TestConnection(t.Context(), resolver, func() error { return nil }))
	require.Equal(t, "Bearer "+literal, resolvedCredentialHeader(t, received))
	require.NoFileExists(t, marker)
	evaluations, err := os.ReadFile(counter)
	require.NoError(t, err)
	require.Equal(t, "x", string(evaluations))
}

func TestResolvedProviderCredentialRejectsStaleCopiesWithoutResolving(t *testing.T) {
	store, provider, _ := resolvedCredentialFixture(t)
	for name, mutate := range map[string]func(*ProviderConfig){
		"source":  func(p *ProviderConfig) { p.APIKeyTemplate = "$OTHER" },
		"literal": func(p *ProviderConfig) { p.APIKey = "other" },
		"owner":   func(p *ProviderConfig) { p.Owner.Type = ProviderOwnerCore },
		"id":      func(p *ProviderConfig) { p.ID = "other" },
		"oauth":   func(p *ProviderConfig) { p.OAuthToken = &oauth.Token{AccessToken: p.APIKey} },
	} {
		t.Run(name, func(t *testing.T) {
			copy := cloneProviderConfig(provider)
			mutate(&copy)
			_, err := ResolveProviderAPIKey(copy, func(string) (string, error) { t.Fatal("stale literal was resolved"); return "", nil })
			require.ErrorIs(t, err, errResolvedProviderAPIKeyStale)
			_, err = store.RuntimeSnapshot().ResolveProviderAPIKey(copy)
			require.ErrorIs(t, err, errResolvedProviderAPIKeyStale)
			_, err = providerAPIKeySourceProjection(copy)
			require.ErrorIs(t, err, errResolvedProviderAPIKeyStale)
		})
	}
	oldSnapshot := store.RuntimeSnapshot()
	next := store.Config().cloneForWrite()
	changed := cloneProviderConfig(provider)
	changed.APIKey = "replacement"
	changed.resolvedAPIKey = nil
	next.Providers.Set(provider.ID, changed)
	store.setConfig(next)
	_, err := store.RuntimeSnapshot().ResolveProviderAPIKey(provider)
	require.ErrorIs(t, err, errResolvedProviderAPIKeyStale)
	got, err := oldSnapshot.ResolveProviderAPIKey(provider)
	require.NoError(t, err)
	require.Equal(t, provider.APIKey, got)
	// A current custom-provider replacement must reject even a formerly valid copy.
	next = store.Config().cloneForWrite()
	next.Providers.Del(provider.ID)
	store.setConfig(next)
	_, err = store.RuntimeSnapshot().ResolveProviderAPIKey(provider)
	require.ErrorIs(t, err, errResolvedProviderAPIKeyStale)
}

func TestResolvedProviderCredentialFullOwnerAndRefresh(t *testing.T) {
	registration := ownerTestRegistration("checked-plugin", "checked.plugin")
	registration.Manifest.Capabilities.Credentials = []manifest.Credential{{ID: "key", Kind: "api-key"}}
	provider := forwardedAccountOwnerTestProvider(registration, &oauth.Token{AccessToken: "old"})
	provider.OAuthToken, provider.APIKey = nil, "$SOURCE"
	store, _ := newProviderOwnerTestStore(t, &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{provider.ID: provider})}, registration)
	provider, err := bindResolvedProviderAPIKey(store.RuntimeSnapshot(), provider, registration.Owner(), "$SOURCE", "checked-$LITERAL")
	require.NoError(t, err)
	store.Config().Providers.Set(provider.ID, provider)
	before := store.Config()
	require.NoError(t, store.SetResolvedProviderAPIKey(registration.Owner(), "$SOURCE", "renewed-$LITERAL"))
	current, _ := store.Config().Providers.Get(provider.ID)
	require.NotSame(t, before, store.Config())
	require.NotSame(t, provider.resolvedAPIKey, current.resolvedAPIKey)
	got, err := store.RuntimeSnapshot().ResolveProviderAPIKey(current)
	require.NoError(t, err)
	require.Equal(t, "renewed-$LITERAL", got)
	require.Equal(t, "checked-$LITERAL", provider.resolvedAPIKey.literal)
	// Public references remain identical while a private account namespace changes.
	registration.AccountNamespace = "replacement.namespace"
	registry, err := providerregistry.New(registration)
	require.NoError(t, err)
	snapshot := store.RuntimeSnapshot()
	snapshot.registry = registry
	_, err = snapshot.ResolveProviderAPIKey(current)
	require.ErrorIs(t, err, errResolvedProviderAPIKeyStale)
	applyOAuthTokenToProvider(&current, &oauth.Token{AccessToken: "oauth-$LITERAL"}, registration)
	require.Nil(t, current.resolvedAPIKey)
	got, err = ResolveProviderAPIKey(current, func(string) (string, error) { t.Fatal("OAuth literal was resolved"); return "", nil })
	require.NoError(t, err)
	require.Equal(t, "oauth-$LITERAL", got)
}

func TestResolvedProviderCredentialEqualSourceRetirement(t *testing.T) {
	store, provider, owner := resolvedCredentialFixture(t)
	provider, err := bindResolvedProviderAPIKey(store.RuntimeSnapshot(), provider, owner, "same-bytes", "same-bytes")
	require.NoError(t, err)
	store.Config().Providers.Set(provider.ID, provider)
	before := store.RuntimeSnapshot()
	next := store.Config().cloneForWrite()
	retired := cloneProviderConfig(provider)
	retired.resolvedAPIKey = nil
	next.Providers.Set(provider.ID, retired)
	store.setConfig(next)
	_, err = store.RuntimeSnapshot().ResolveProviderAPIKey(provider)
	require.ErrorIs(t, err, errResolvedProviderAPIKeyStale)
	got, err := before.ResolveProviderAPIKey(provider)
	require.NoError(t, err)
	require.Equal(t, "same-bytes", got)
}

func TestResolvedProviderCredentialLogoutCollectsDenial(t *testing.T) {
	store, provider, owner := resolvedCredentialFixture(t)
	before, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	next, err := authenticationConfigCandidate(before, owner, nil)
	require.NoError(t, err)
	require.NoError(t, before.finalizeRuntimeAuthenticationAccounts(next, owner, nil))
	store.setConfig(next)
	denied, _ := next.Providers.Get(provider.ID)
	require.Nil(t, denied.resolvedAPIKey)
	proposal, err := store.CollectRemoteRuntimeWithUnavailable(t.Context(), 1, map[providerregistry.RegistrationOwner]bool{owner: true})
	require.NoError(t, err)
	require.Len(t, proposal.Credentials, 1)
	require.True(t, proposal.Credentials[0].Unavailable)
	require.Empty(t, proposal.Credentials[0].APIKey)
	require.Nil(t, proposal.Credentials[0].Account)
	got, err := before.runtime.ResolveProviderAPIKey(provider)
	require.NoError(t, err)
	require.Equal(t, provider.APIKey, got)
}

func TestResolvedProviderCredentialSlotAdmission(t *testing.T) {
	for _, construction := range []providerregistry.Construction{providerregistry.ConstructionCodex, providerregistry.ConstructionCopilot, providerregistry.ConstructionGeminiAntigravity, providerregistry.ConstructionOpenAIResponses, providerregistry.ConstructionAnthropicMessages, providerregistry.ConstructionGeminiContent, providerregistry.ConstructionGeminiInteraction, providerregistry.ConstructionGenericJSON} {
		for _, kind := range []string{"api-key", "bearer", "oauth2"} {
			t.Run(string(construction)+"/"+kind, func(t *testing.T) {
				r := ownerTestRegistration("checked", "checked.plugin")
				r.Construction = construction
				r.Operation = &providertransport.Operation{Key: providertransport.Key{Protocol: string(construction), Transport: "http-json"}, Endpoint: manifest.Endpoint{Credential: "inference"}}
				r.Manifest.Capabilities.Credentials = []manifest.Credential{{ID: "inference", Kind: kind}, {ID: "unrelated", Kind: "api-key", ConfigProperty: "other"}}
				wantSupported := kind != "oauth2" && construction != providerregistry.ConstructionCodex && construction != providerregistry.ConstructionCopilot && construction != providerregistry.ConstructionGeminiAntigravity
				require.Equal(t, wantSupported, registrationAPIKeySlotSupported(r))
				r.Operation.Endpoint.Credential = ""
				require.Equal(t, wantSupported, registrationAPIKeySlotSupported(r), "headers and request templates can use declared credential mappings without endpoint metadata")
				r.Manifest.Capabilities.Credentials[0].ConfigProperty = "secret"
				require.False(t, registrationAPIKeySlotSupported(r))
			})
		}
	}
	for _, registration := range providerregistry.Integrated() {
		provider := ProviderConfig{ID: registration.ProviderID, Owner: providerOwnerReferenceForRegistration(registration), APIKey: "old"}
		store := resolvedCredentialStore(t, provider)
		_, err := bindResolvedProviderAPIKey(store.RuntimeSnapshot(), provider, registration.Owner(), "$SOURCE", "literal")
		require.ErrorIs(t, err, errProviderAPIKeySlotUnsupported, "integrated OAuth admission must fail before publishing checked bytes")
	}
}

func TestResolvedProviderCredentialOptionalOAuthBundleUsesAPIKey(t *testing.T) {
	for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	root := t.TempDir()
	source := filepath.Join(root, "mixed.plugin")
	require.NoError(t, os.MkdirAll(filepath.Join(source, "instructions"), 0o700))
	data, err := os.ReadFile("../../docs/provider-plugins/examples/responses-oauth.plugin/manifest.json")
	require.NoError(t, err)
	var declaration manifest.Manifest
	require.NoError(t, json.Unmarshal(data, &declaration))
	declaration.Capabilities.Credentials = append(declaration.Capabilities.Credentials, manifest.Credential{ID: "key", Kind: "api-key", Audience: []string{"api"}})
	for i := range declaration.Capabilities.Endpoints {
		if declaration.Capabilities.Endpoints[i].ID == "api" {
			declaration.Capabilities.Endpoints[i].Credential = "key"
		}
	}
	data, err = json.Marshal(declaration)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	text, err := os.ReadFile("../../docs/provider-plugins/examples/responses-oauth.plugin/instructions/native.txt")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "instructions", "native.txt"), text, 0o600))
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(filepath.Join(root, "data"), filepath.Join(root, "cache")))
	require.NoError(t, err)
	t.Cleanup(manager.Close)
	installed, err := manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, Trust: true})
	require.NoError(t, err)
	status := installed.Plugins[0]
	bundles, err := manager.ExportRegisteredBundles(installed.Revision, map[string]string{status.ID: status.Digest})
	require.NoError(t, err)
	bundle, err := providerplugin.ValidateDetachedBundle(bundles[0])
	require.NoError(t, err)
	catalogue, err := bundle.Catalog()
	require.NoError(t, err)
	registration, err := providerregistry.FromManifest(bundle.Provider().Manifest, bundle.Provider().StaticText)
	require.NoError(t, err)
	provider := ProviderConfig{ID: registration.ProviderID, Type: catalogue.Type, BaseURL: catalogue.APIEndpoint, Models: catalogue.Models,
		Owner: providerOwnerReferenceForRegistration(registration), Plugin: &ProviderPluginReference{ID: bundle.ID(), Version: bundle.Version()}, Configuration: map[string]any{"oauth_client_id": "synthetic-client"}}
	literal := "explicit-$LITERAL-key"
	proposal := sealRemoteRuntime(t, RemoteRuntimeProposal{Version: RemoteRuntimeVersion, Revision: 1, Bundles: bundles,
		Providers:   []RemoteProviderDefinition{{Config: provider, BundleDigest: bundle.Digest()}},
		Models:      map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: {Provider: provider.ID, Model: provider.Models[0].ID}, SelectedModelTypeSmall: {Provider: provider.ID, Model: provider.Models[1].ID}},
		Credentials: []RemoteCredentialBinding{{Owner: registration.Owner(), Generation: 1, APIKey: literal}}})
	receiver, err := CompileRemoteRuntime(root, filepath.Join(root, "workspace"), false, proposal, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err, "optional OAuth cannot replace the declared inference API-key slot")
	apiKey, err := receiver.RuntimeSnapshot().UsesResolvedProviderAPIKey(provider.ID)
	require.NoError(t, err)
	require.True(t, apiKey)
	// Reuse this validated bundle generation as a local captured config, then
	// prove collection does not consult even a same-token active OAuth account.
	local := NewTestStore(receiver.Config().cloneForWrite())
	scan := *local.Config().providerScan
	scan.bundles = map[string]providerplugin.TransportBundle{bundles[0].Digest: bundles[0]}
	local.Config().providerScan = &scan
	provider, _ = local.Config().Providers.Get(provider.ID)
	provider, err = bindResolvedProviderAPIKey(local.RuntimeSnapshot(), provider, registration.Owner(), "$SOURCE", literal)
	require.NoError(t, err)
	local.Config().Providers.Set(provider.ID, provider)
	require.NoError(t, accounts.Save(t.Context(), registration.AccountNamespace, accounts.Entry{ID: "must-not-select", AccessToken: literal, Raw: json.RawMessage(`{"foreign":true}`)}))
	collected, err := local.CollectRemoteRuntime(t.Context(), 2)
	require.NoError(t, err)
	require.Len(t, collected.Credentials, 1)
	require.Equal(t, literal, collected.Credentials[0].APIKey)
	require.Nil(t, collected.Credentials[0].Account)
}

func TestResolvedProviderCredentialCopiesProjectionAndPrivacy(t *testing.T) {
	store, provider, _ := resolvedCredentialFixture(t)
	next := store.Config().cloneForWrite()
	copy, _ := next.Providers.Get(provider.ID)
	copy.BaseURL = "https://changed.invalid/v1"
	copy.ExtraHeaders = map[string]string{"X-Changed": "different"}
	copy.Models = []catalog.Model{{ID: "different"}}
	next.Providers.Set(copy.ID, copy)
	store.setConfig(next)
	got, err := store.RuntimeSnapshot().ResolveProviderAPIKey(copy)
	require.NoError(t, err, "unrelated settings do not invalidate literal bytes")
	require.Equal(t, provider.APIKey, got)
	projected, err := providerAPIKeySourceProjection(copy)
	require.NoError(t, err)
	require.Equal(t, "$SOURCE", projected.APIKey)
	require.Empty(t, projected.APIKeyTemplate)
	require.Nil(t, projected.resolvedAPIKey)
	require.Nil(t, projected.OAuthToken)
	require.Equal(t, copy.BaseURL, projected.BaseURL)
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d"} {
		require.NotContains(t, fmt.Sprintf(format, copy.resolvedAPIKey), "$SOURCE")
		require.NotContains(t, fmt.Sprintf(format, *copy.resolvedAPIKey), copy.APIKey)
	}
	_, err = json.Marshal(copy.resolvedAPIKey)
	require.Error(t, err)
	encoded, err := json.Marshal(copy)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "$SOURCE")
	var decoded ProviderConfig
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Nil(t, decoded.resolvedAPIKey)
	redacted, _ := next.RedactedForTransport().Providers.Get(copy.ID)
	require.Nil(t, redacted.resolvedAPIKey)
	require.Empty(t, redacted.APIKey)
	require.Empty(t, redacted.APIKeyTemplate)
}

func TestResolvedProviderCredentialExplicitReplacementRetiresBinding(t *testing.T) {
	store, provider, owner := resolvedCredentialFixture(t)
	require.NoError(t, store.SetProviderAPIKey(ScopeGlobal, provider.ID, ProviderAPIKeyCredential{Owner: owner, APIKey: "$NEW_SOURCE"}))
	current, _ := store.Config().Providers.Get(provider.ID)
	require.Nil(t, current.resolvedAPIKey)
	require.Empty(t, current.APIKeyTemplate)
	require.Nil(t, current.OAuthToken)
	got, err := ResolveProviderAPIKey(current, func(source string) (string, error) {
		require.Equal(t, "$NEW_SOURCE", source)
		return "fresh-expression", nil
	})
	require.NoError(t, err)
	require.Equal(t, "fresh-expression", got)
	require.Equal(t, "literal-$NOT_AN_EXPRESSION", provider.resolvedAPIKey.literal)
}

func TestResolvedProviderCredentialDiscoveryAndCollectionHTTPS(t *testing.T) {
	store, provider, owner := resolvedCredentialFixture(t)
	marker := filepath.Join(t.TempDir(), "must-not-execute")
	literal := "literal-$(printf x > " + marker + ")-$UNSET"
	provider, err := bindResolvedProviderAPIKey(store.RuntimeSnapshot(), provider, owner, "$SOURCE", literal)
	require.NoError(t, err)
	requests := make(chan string, 1)
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"main"},{"id":"small"}]}`))
	}))
	t.Cleanup(host.Close)
	old := http.DefaultTransport
	http.DefaultTransport = host.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = old })
	provider.BaseURL, provider.Models = host.URL, nil
	store.Config().Providers.Set(provider.ID, provider)
	require.NoError(t, store.Config().configureProvidersWithMigration(t.Context(), store, env.New(), store.resolver, nil, nil))
	require.Equal(t, "Bearer "+literal, resolvedCredentialHeader(t, requests))
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, proposal.Credentials, 1)
	require.Equal(t, literal, proposal.Credentials[0].APIKey)
	require.Nil(t, proposal.Credentials[0].Account)
	require.Len(t, proposal.Providers, 1)
	require.Nil(t, proposal.Providers[0].Config.resolvedAPIKey)
	encoded, err := json.Marshal(proposal)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "$SOURCE")
	require.NoFileExists(t, marker)
	root := t.TempDir()
	receiver, err := CompileRemoteRuntime(root, filepath.Join(root, "state"), false, proposal, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SnapshotEnvironment())
	require.NoError(t, err)
	remoteProvider, ok := receiver.Config().Providers.Get(provider.ID)
	require.True(t, ok)
	got, err := receiver.RuntimeSnapshot().ResolveProviderAPIKey(remoteProvider)
	require.NoError(t, err)
	require.Equal(t, literal, got)
	require.NoFileExists(t, marker)
	current, _ := store.Config().Providers.Get(provider.ID)
	current.APIKeyTemplate = "$OTHER"
	store.Config().Providers.Set(provider.ID, current)
	_, err = store.CollectRemoteRuntime(t.Context(), 2)
	require.ErrorIs(t, err, errResolvedProviderAPIKeyStale)
	require.NoFileExists(t, marker)
}

func TestResolvedProviderCredentialKnownPreparationPreservesSource(t *testing.T) {
	store, provider, _ := resolvedCredentialFixture(t)
	catalogProvider := catalog.Provider{ID: catalog.ProviderID(provider.ID), Name: "Catalog name", Type: provider.Type, APIEndpoint: provider.BaseURL, APIKey: "$CATALOG", Models: provider.Models}
	require.NoError(t, store.Config().configureProvidersWithMigration(t.Context(), store, env.New(), store.resolver, []catalog.Provider{catalogProvider}, nil))
	prepared, ok := store.Config().Providers.Get(provider.ID)
	require.True(t, ok)
	require.Equal(t, "$SOURCE", prepared.APIKeyTemplate)
	got, err := store.RuntimeSnapshot().ResolveProviderAPIKey(prepared)
	require.NoError(t, err)
	require.Equal(t, provider.APIKey, got)
	prepared.APIKeyTemplate = "$CHANGED"
	store.Config().Providers.Set(provider.ID, prepared)
	migrated := false
	err = store.Config().configureProvidersWithMigration(t.Context(), store, env.New(), store.resolver, []catalog.Provider{catalogProvider}, func(map[string]ProviderOwnerReference, map[string]ProviderPluginReference, map[string]ProviderPresetReference) error {
		migrated = true
		return nil
	})
	require.ErrorIs(t, err, errResolvedProviderAPIKeyStale)
	require.False(t, migrated)
}

func TestResolvedProviderCredentialReloadRestoresSourceSemantics(t *testing.T) {
	store, _, _ := authenticationBasisStore(t, func(root string, values map[string]string) {
		values["RELOAD_SOURCE"] = "ordinary-reloaded-value"
		authenticationBasisWriteField(t, filepath.Join(root, "crux.json"), []string{"providers", "fixture", "api_key"}, `"$RELOAD_SOURCE"`)
	})
	provider, ok := store.Config().Providers.Get("fixture")
	require.True(t, ok)
	owner, ok := store.RuntimeSnapshot().ProviderOwner(provider.ID)
	require.True(t, ok)
	provider, err := bindResolvedProviderAPIKey(store.RuntimeSnapshot(), provider, owner, "$RELOAD_SOURCE", "previous-checked-$LITERAL")
	require.NoError(t, err)
	next := store.Config().cloneForWrite()
	next.Providers.Set(provider.ID, provider)
	store.setConfig(next)
	before := store.RuntimeSnapshot()
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	loaded, ok := store.Config().Providers.Get(provider.ID)
	require.True(t, ok)
	require.Nil(t, loaded.resolvedAPIKey)
	require.Equal(t, "$RELOAD_SOURCE", loaded.APIKey)
	got, err := store.RuntimeSnapshot().ResolveProviderAPIKey(loaded)
	require.NoError(t, err)
	require.Equal(t, "ordinary-reloaded-value", got)
	_, err = store.RuntimeSnapshot().ResolveProviderAPIKey(provider)
	require.ErrorIs(t, err, errResolvedProviderAPIKeyStale)
	got, err = before.ResolveProviderAPIKey(provider)
	require.NoError(t, err)
	require.Equal(t, "previous-checked-$LITERAL", got)
}
