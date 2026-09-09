package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestAuthenticationAcceptanceRejectsSameVersionBundleReplacement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	store, _, _, installed := setupReloadPluginStore(t)
	accepted, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.NotNil(t, accepted.collectionSource, "exercise the production-collected private provenance")
	require.Len(t, accepted.Providers, 1)
	view := store.Config()
	before, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.NoError(t, before.ValidateAcceptedAuthentication(accepted, view))
	definition, owner, err := before.runtime.clientProviderDefinitionRaw("example-echo")
	require.NoError(t, err)
	require.Equal(t, installed.Digest, definition.BundleDigest)
	beforeProviders, err := json.Marshal(view.Providers)
	require.NoError(t, err)

	// Model a newly captured bundle with the same manifest ID/version and
	// registration owner. Clone the scan so the accepted source remains immutable.
	// No live bundle path participates in this accepted-state comparison.
	next := view.cloneForWrite()
	scan := cloneProviderScan(*view.providerScan)
	provider, ok := next.Providers.Get(owner.ProviderID)
	require.True(t, ok)
	require.NotNil(t, provider.Plugin)
	status := scan.pluginStatuses[provider.Plugin.ID]
	replacementDigest := strings.Repeat("b", 64)
	if replacementDigest == status.Digest {
		replacementDigest = strings.Repeat("c", 64)
	}
	status.Digest = replacementDigest
	scan.pluginStatuses[provider.Plugin.ID] = status
	next.providerScan = &scan
	store.writeMu.Lock()
	store.setConfig(next)
	store.writeMu.Unlock()

	after, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	changed, changedOwner, err := after.runtime.clientProviderDefinitionRaw(owner.ProviderID)
	require.NoError(t, err)
	require.Equal(t, owner, changedOwner, "owner equality alone must not bless a changed executable bundle")
	require.Equal(t, definition.Config.Plugin, changed.Config.Plugin, "plugin ID and version intentionally stay identical")
	afterProviders, err := json.Marshal(store.Config().Providers)
	require.NoError(t, err)
	require.Equal(t, beforeProviders, afterProviders, "public/provider configuration does not carry this change")
	require.Equal(t, replacementDigest, changed.BundleDigest)
	changed.BundleDigest = definition.BundleDigest
	require.Equal(t, definition, changed, "the bundle digest is the only definition difference")
	require.Equal(t, installed.Digest, accepted.collectionSource.runtime.config.providerScan.pluginStatuses[provider.Plugin.ID].Digest)
	require.ErrorContains(t, after.ValidateAcceptedAuthentication(accepted, view), "not acknowledged")
	require.NoError(t, before.ValidateAcceptedAuthentication(accepted, view), "the original immutable capture still matches its acknowledged proposal")
}

func TestAuthenticationAcceptanceCollectedOAuthMetadata(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AI_CLI_DIR", root)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	registration := providerregistry.Registration{ProviderID: "codex", AccountNamespace: "collected-auth-metadata", Construction: providerregistry.ConstructionCodex, OAuth: &providerregistry.OAuthCapability{}}
	entry := accounts.Entry{ID: "selected", AccessToken: "synthetic-collected-access", RefreshToken: "synthetic-collected-refresh", Raw: json.RawMessage(`{"number":1,"nested":{"value":"same"}}`)}
	require.NoError(t, accounts.Save(t.Context(), registration.AccountNamespace, entry))
	provider := ProviderConfig{ID: registration.ProviderID, APIKey: entry.AccessToken, OAuthToken: entry.Token(), Owner: providerOwnerReferenceForRegistration(registration), Models: []catalog.Model{{ID: "fixture", Name: "Fixture"}}}
	store := NewTestStoreWithRegistrations(&Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{provider.ID: provider}),
		Models:    map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: {Provider: provider.ID, Model: "fixture"}, SelectedModelTypeSmall: {Provider: provider.ID, Model: "fixture"}},
	}, registration)
	accepted, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.NotNil(t, accepted.collectionSource)
	require.Len(t, accepted.Credentials, 1)
	require.NotNil(t, accepted.Credentials[0].Account)
	view := accepted.CollectionConfig()
	before, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	storedRaw := before.accounts.Entries(registration.AccountNamespace)[0].Raw
	require.NotEqual(t, string(storedRaw), string(accepted.Credentials[0].Account.Raw), "persistence indentation and collected JSON compaction must actually differ")
	require.NoError(t, before.ValidateAcceptedAuthentication(accepted, view), "collected OAuth metadata must tolerate harmless formatting differences")

	entry.Raw = json.RawMessage(`{"number":1.0,"nested":{"value":"same"}}`)
	require.NoError(t, accounts.Save(t.Context(), registration.AccountNamespace, entry))
	after, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.ErrorContains(t, after.ValidateAcceptedAuthentication(accepted, view), "not acknowledged", "numeric spelling affects declared image expressions even with valid collection provenance")
}

func TestAuthenticationAcceptancePreservesAccountMetadataSemantics(t *testing.T) {
	for _, test := range []struct {
		name     string
		accepted json.RawMessage
		current  json.RawMessage
		wantOK   bool
	}{
		{name: "whitespace", accepted: json.RawMessage(`{"number":1,"nested":{"value":"same"}}`), current: json.RawMessage("{\n  \"number\": 1,\n  \"nested\": {\"value\": \"same\"}\n}"), wantOK: true},
		{name: "object order", accepted: json.RawMessage(`{"number":1,"other":false}`), current: json.RawMessage(`{"other":false,"number":1}`), wantOK: true},
		{name: "decimal spelling", accepted: json.RawMessage(`{"number":1}`), current: json.RawMessage(`{"number":1.0}`)},
		{name: "exponent spelling", accepted: json.RawMessage(`{"nested":[1]}`), current: json.RawMessage(`{"nested":[1e0]}`)},
		{name: "absent unchanged", wantOK: true},
		{name: "null unchanged", accepted: json.RawMessage(`null`), current: json.RawMessage(`null`), wantOK: true},
		{name: "absent to null", current: json.RawMessage(`null`)},
		{name: "null to absent", accepted: json.RawMessage(`null`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, owner, entry, _ := authenticationCaptureTestStore(t)
			acceptedEntry := entry
			acceptedEntry.Raw = test.accepted
			entry.Raw = test.current
			require.NoError(t, accounts.Save(t.Context(), owner.AccountNamespace, entry))
			capture, err := store.CaptureAuthentication(t.Context())
			require.NoError(t, err)
			// This trusted host-only fixture isolates account metadata from the
			// production collector's separate definition/provenance checks.
			accepted := RemoteRuntimeProposal{Revision: 1, Digest: "synthetic-accepted-runtime", Models: store.Config().Models,
				Credentials: []RemoteCredentialBinding{{Owner: owner, Generation: 1, Account: &acceptedEntry}}}
			err = capture.ValidateAcceptedAuthentication(accepted, store.Config())
			if test.wantOK {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "not acknowledged")
			}
		})
	}
}

func TestAuthenticationCaptureFormattingNeverExposesPrivateEnvironment(t *testing.T) {
	const variable = "CRUX_AUTH_CAPTURE_PRIVATE_FORMAT_TEST"
	const secret = "synthetic-private-captured-environment-value"
	t.Setenv(variable, secret)
	store, owner, entry, root := authenticationCaptureTestStore(t)
	capture, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.Equal(t, secret, capture.runtime.Getenv(variable), "the fixture must actually retain the private environment value")
	for _, value := range []any{capture, &capture} {
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d", "%f"} {
			formatted := fmt.Sprintf(format, value)
			for _, private := range []string{variable, secret, entry.AccessToken, entry.RefreshToken, owner.AccountNamespace, root} {
				require.NotContains(t, formatted, private, "private capture leaked via %s", format)
			}
			require.Contains(t, formatted, "private authentication capture")
		}
	}
	_, err = json.Marshal(capture)
	require.ErrorContains(t, err, "authentication captures are private")
}
