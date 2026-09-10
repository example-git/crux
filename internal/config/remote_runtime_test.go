package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providerregistry/registrytest"
	"github.com/stretchr/testify/require"
)

func TestRemoteRuntimeReplacementIsAtomicAndKeepsCapturedState(t *testing.T) {
	proposal := remoteRuntimeFixture(t, "minimal.plugin")
	root := t.TempDir()
	principal := strings.Repeat("a", 64)
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "workspace"), false, proposal, principal, env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	before := store.RuntimeSnapshot()
	files := remoteBaselineTree(t, root)
	data, err := json.Marshal(proposal)
	require.NoError(t, err)
	var replacement RemoteRuntimeProposal
	require.NoError(t, json.Unmarshal(data, &replacement))
	replacement.Revision = 2
	replacement.Credentials[0].Generation = 2
	replacement.Credentials[0].APIKey = "synthetic-second-key"
	replacement = sealRemoteRuntime(t, replacement)
	store.SetRuntimeGenerationPreparer(func(context.Context, RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
		return RuntimeGenerationCandidate{}, errors.New("synthetic preparation failure")
	})
	_, err = store.ReplaceRemoteRuntime(t.Context(), replacement, principal, 1)
	require.ErrorContains(t, err, "synthetic preparation failure")
	require.Same(t, before.Config(), store.Config())
	require.Equal(t, before.RemoteAuthority(), store.RemoteAuthority())
	store.SetRuntimeGenerationPreparer(nil)
	_, err = store.ReplaceRemoteRuntime(t.Context(), replacement, strings.Repeat("b", 64), 1)
	require.ErrorContains(t, err, "owning client principal")
	_, err = store.ReplaceRemoteRuntime(t.Context(), replacement, principal, 0)
	require.ErrorIs(t, err, ErrRemoteRuntimeRevision)
	ack, err := store.ReplaceRemoteRuntime(t.Context(), replacement, principal, 1)
	require.NoError(t, err)
	require.Equal(t, uint64(2), ack.Revision)
	oldProvider, _ := before.Config().Providers.Get(proposal.Providers[0].Config.ID)
	newProvider, _ := store.Config().Providers.Get(proposal.Providers[0].Config.ID)
	require.Equal(t, "synthetic-client-$EXACT-key", oldProvider.APIKey)
	require.Equal(t, "synthetic-second-key", newProvider.APIKey)
	require.Equal(t, uint64(1), before.RemoteAuthority().Revision)
	require.Equal(t, uint64(2), store.RuntimeSnapshot().RemoteAuthority().Revision)
	_, err = store.ReplaceRemoteRuntime(t.Context(), replacement, principal, 1)
	require.ErrorIs(t, err, ErrRemoteRuntimeRevision)
	require.Equal(t, files, remoteBaselineTree(t, root))
}

func TestRemoteRuntimeCompilerRetainsExactProviderOptionNumbers(t *testing.T) {
	proposal := remoteRuntimeFixture(t, "minimal.plugin")
	model := proposal.Models[SelectedModelTypeLarge]
	model.ProviderOptions = map[string]any{"vendor.limit": json.Number("9007199254740993")}
	proposal.Models[SelectedModelTypeLarge] = model
	proposal.Providers[0].Config.ProviderOptions = map[string]any{"vendor.zero": json.Number("0.00")}
	proposal = sealRemoteRuntime(t, proposal)
	root := t.TempDir()
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "workspace"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	require.Equal(t, json.Number("9007199254740993"), store.Config().Models[SelectedModelTypeLarge].ProviderOptions["vendor.limit"])
	provider, ok := store.Config().Providers.Get(proposal.Providers[0].Config.ID)
	require.True(t, ok)
	require.Equal(t, json.Number("0.00"), provider.ProviderOptions["vendor.zero"])
	require.Equal(t, proposal.Digest, store.RemoteAuthority().Digest)
}

func TestClientRuntimeCannotPersistOrReloadServerAuthority(t *testing.T) {
	proposal := remoteRuntimeFixture(t, "minimal.plugin")
	root := t.TempDir()
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	before := store.RuntimeSnapshot()
	files := remoteBaselineTree(t, root)
	for _, action := range []func() error{
		func() error { return store.ReloadFromDisk(t.Context()) },
		func() error {
			return store.SetProviderAPIKey(ScopeGlobal, proposal.Providers[0].Config.ID, ProviderAPIKeyCredential{Owner: proposal.Credentials[0].Owner, APIKey: "replacement"})
		},
		func() error {
			_, err := store.RefreshOAuthTokenForOwner(t.Context(), ScopeGlobal, proposal.Credentials[0].Owner)
			return err
		},
		func() error {
			return store.writeConfigFields(ScopeGlobal, map[string]any{"providers": map[string]any{}})
		},
	} {
		require.ErrorIs(t, action(), ErrClientRuntimeManaged)
		require.Same(t, before.Config(), store.Config())
		require.Equal(t, before.RemoteAuthority(), store.RemoteAuthority())
		require.Equal(t, files, remoteBaselineTree(t, root))
	}
}

func remoteRuntimeFixture(t *testing.T, name string) RemoteRuntimeProposal {
	t.Helper()
	root := t.TempDir()
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(root, filepath.Join(root, "cache")))
	require.NoError(t, err)
	t.Cleanup(manager.Close)
	source, err := filepath.Abs(filepath.Join("..", "..", "docs", "provider-plugins", "examples", name))
	require.NoError(t, err)
	snapshot, err := manager.Install(t.Context(), providerplugin.InstallRequest{Source: source})
	require.NoError(t, err)
	status := snapshot.Plugins[0]
	snapshot, err = manager.SetTrust(t.Context(), status.ID, providerplugin.TrustRequest{Digest: status.Digest, Trusted: true})
	require.NoError(t, err)
	bundles, err := manager.ExportRegisteredBundles(snapshot.Revision, map[string]string{status.ID: status.Digest})
	require.NoError(t, err)
	bundle, err := providerplugin.ValidateDetachedBundle(bundles[0])
	require.NoError(t, err)
	metadata, err := bundle.Catalog()
	require.NoError(t, err)
	provider := ProviderConfig{ID: bundle.ProviderID(), Name: metadata.Name, Type: metadata.Type, BaseURL: metadata.APIEndpoint, Models: metadata.Models}
	var credentialOwner providerregistry.RegistrationOwner
	if full := bundle.Provider(); full != nil {
		registration, err := providerregistry.FromManifest(full.Manifest, full.StaticText)
		require.NoError(t, err)
		provider.Owner = providerOwnerReferenceForRegistration(registration)
		provider.Plugin = &ProviderPluginReference{ID: bundle.ID(), Version: bundle.Version()}
		credentialOwner = registration.Owner()
	} else {
		provider.Owner = providerPresetOwnerReference()
		provider.Preset = &ProviderPresetReference{ID: bundle.ID(), Version: bundle.Version(), Digest: bundle.Digest()}
		credentialOwner = providerregistry.RegistrationOwner{ProviderID: provider.ID, HasPreset: true, PresetID: bundle.ID(), PresetVersion: bundle.Version(), PresetDigest: bundle.Digest()}
	}
	proposal := RemoteRuntimeProposal{Version: RemoteRuntimeVersion, Revision: 1, Bundles: bundles, Providers: []RemoteProviderDefinition{{Config: provider, BundleDigest: bundle.Digest()}}, Models: map[SelectedModelType]SelectedModel{
		SelectedModelTypeLarge: {Provider: provider.ID, Model: provider.Models[0].ID}, SelectedModelTypeSmall: {Provider: provider.ID, Model: provider.Models[0].ID},
	}, Credentials: []RemoteCredentialBinding{{Owner: credentialOwner, Generation: 1, APIKey: "synthetic-client-$EXACT-key"}}}
	return sealRemoteRuntime(t, proposal)
}

func sealRemoteRuntime(t *testing.T, proposal RemoteRuntimeProposal) RemoteRuntimeProposal {
	bindNativeTestBundles(t, &proposal)
	t.Helper()
	digest, err := RemoteRuntimeDigest(proposal)
	require.NoError(t, err)
	proposal.Digest = digest
	return proposal
}

func TestCompileRemoteRuntimeUsesClientBundleWithoutServerInstallation(t *testing.T) {
	for _, name := range []string{"minimal.plugin", "deepseek-preset.plugin"} {
		t.Run(name, func(t *testing.T) {
			proposal := remoteRuntimeFixture(t, name)
			serverRoot := t.TempDir()
			serverEnv := env.NewFromMap(map[string]string{"HOME": serverRoot, "CRUX_GLOBAL_DATA": filepath.Join(serverRoot, "global"), "CRUX_GLOBAL_CONFIG": filepath.Join(serverRoot, "config"), "CRUX_CACHE_DIR": filepath.Join(serverRoot, "cache"), "EXACT": "SERVER-SUBSTITUTION"})
			before := remoteBaselineTree(t, serverRoot)
			store, err := CompileRemoteRuntime(serverRoot, filepath.Join(serverRoot, "workspace"), false, proposal, strings.Repeat("a", 64), serverEnv)
			require.NoError(t, err)
			require.Equal(t, before, remoteBaselineTree(t, serverRoot), "candidate compilation must not install bundles or initialize server stores")
			runtime := store.RuntimeSnapshot()
			id := proposal.Providers[0].Config.ID
			provider, ok := runtime.Config().Providers.Get(id)
			require.True(t, ok)
			actual, registration, registered, err := runtime.ProviderForConstruction(id, provider)
			require.NoError(t, err)
			require.Equal(t, "synthetic-client-$EXACT-key", actual.APIKey)
			resolved, err := runtime.Resolve(actual.APIKey)
			require.NoError(t, err)
			require.Equal(t, actual.APIKey, resolved, "the server must not reinterpret client-resolved credential bytes")
			if name == "minimal.plugin" {
				require.True(t, registered)
				require.Equal(t, id, registration.ProviderID)
			} else {
				require.False(t, registered)
				require.True(t, provider.Owner.Type == ProviderOwnerPreset)
			}
			authority := store.RemoteAuthority()
			require.Equal(t, proposal.Digest, authority.Digest)
			require.Equal(t, uint64(1), authority.Revision)
			public, err := json.Marshal(authority)
			require.NoError(t, err)
			require.NotContains(t, string(public), "synthetic-client")
			require.NotContains(t, string(public), "manifest")
			proposal.Models[SelectedModelTypeLarge] = SelectedModel{Provider: "changed", Model: "changed"}
			proposal.Providers[0].Config.Models[0].ID = "changed"
			require.NotEqual(t, "changed", runtime.Config().Models[SelectedModelTypeLarge].Provider)
			current, _ := runtime.Config().Providers.Get(id)
			require.NotEqual(t, "changed", current.Models[0].ID)
			_, err = os.Stat(filepath.Join(serverRoot, "global", "plugins"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestCompileRemoteRuntimeRejectsInvalidCandidateWithoutServerWrites(t *testing.T) {
	base := remoteRuntimeFixture(t, "minimal.plugin")
	for _, test := range []struct {
		name    string
		change  func(*RemoteRuntimeProposal)
		message string
	}{
		{"version", func(p *RemoteRuntimeProposal) { p.Version = 0 }, "protocol version"},
		{"revision", func(p *RemoteRuntimeProposal) { p.Revision = 0 }, "revision"},
		{"duplicate-bundle", func(p *RemoteRuntimeProposal) { p.Bundles = append(p.Bundles, p.Bundles[0]) }, "duplicate runtime bundle"},
		{"duplicate-provider", func(p *RemoteRuntimeProposal) { p.Providers = append(p.Providers, p.Providers[0]) }, "duplicate client provider"},
		{"missing-bundle", func(p *RemoteRuntimeProposal) { p.Bundles = nil }, "exact bundle digest"},
		{"endpoint", func(p *RemoteRuntimeProposal) { p.Providers[0].Config.BaseURL = "https://elsewhere.invalid" }, "destination policy"},
		{"empty-endpoint", func(p *RemoteRuntimeProposal) { p.Providers[0].Config.BaseURL = "" }, "explicit HTTP"},
		{"owner-change", func(p *RemoteRuntimeProposal) {
			p.Providers[0].Config.Owner.Construction = providerregistry.ConstructionCopilot
		}, "construction mismatch"},
		{"embedded-token", func(p *RemoteRuntimeProposal) { p.Providers[0].Config.APIKey = "private-invalid-key" }, "separate credential bindings"},
		{"credential-owner", func(p *RemoteRuntimeProposal) { p.Credentials[0].Owner.ManifestVersion = "99.0.0" }, "exact provider owner"},
		{"credential-generation", func(p *RemoteRuntimeProposal) { p.Credentials[0].Generation = 0 }, "credential binding"},
		{"unavailable-with-secret", func(p *RemoteRuntimeProposal) { p.Credentials[0].Unavailable = true }, "cannot contain a secret"},
		{"duplicate-credential", func(p *RemoteRuntimeProposal) { p.Credentials = append(p.Credentials, p.Credentials[0]) }, "credential binding"},
		{"missing-model", func(p *RemoteRuntimeProposal) {
			p.Models[SelectedModelTypeLarge] = SelectedModel{Provider: p.Providers[0].Config.ID, Model: "absent"}
		}, "model is unavailable"},
		{"missing-provider", func(p *RemoteRuntimeProposal) {
			p.Models[SelectedModelTypeLarge] = SelectedModel{Provider: "absent", Model: "absent"}
		}, "provider is missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(base)
			require.NoError(t, err)
			var proposal RemoteRuntimeProposal
			require.NoError(t, json.Unmarshal(data, &proposal))
			test.change(&proposal)
			proposal = sealRemoteRuntime(t, proposal)
			root := t.TempDir()
			before := remoteBaselineTree(t, root)
			_, err = CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"HOME": root}))
			require.ErrorContains(t, err, test.message)
			require.NotContains(t, err.Error(), "private-invalid-key")
			require.Equal(t, before, remoteBaselineTree(t, root))
		})
	}
}

func TestRemoteRuntimeExplicitMissingCredential(t *testing.T) {
	proposal := remoteRuntimeFixture(t, "deepseek-preset.plugin")
	proposal.Credentials[0].APIKey = ""
	proposal = sealRemoteRuntime(t, proposal)
	root := t.TempDir()
	_, err := CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, strings.Repeat("a", 64), SnapshotEnvironment())
	require.ErrorContains(t, err, "requires its client credential")
	proposal.Credentials[0].Unavailable = true
	proposal = sealRemoteRuntime(t, proposal)
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, strings.Repeat("a", 64), SnapshotEnvironment())
	require.NoError(t, err)
	require.ErrorContains(t, store.RuntimeSnapshot().ClientProviderUnavailable(proposal.Providers[0].Config.ID), "has no credential")
}

// bindNativeTestBundles makes the fixture's provider authority explicit before
// sealing its remote digest. Existing plugin/custom proposals are unchanged.
func bindNativeTestBundles(t *testing.T, proposal *RemoteRuntimeProposal) {
	t.Helper()
	for i := range proposal.Providers {
		definition := &proposal.Providers[i]
		p := &definition.Config
		if p.Owner == nil || p.Owner.Type != ProviderOwnerCore || (p.ID != "codex" && p.ID != "gemini-ag") {
			continue
		}
		registration, bundle, err := registrytest.BundleFor(p.ID, p.BaseURL, p.Models)
		require.NoError(t, err)
		p.Plugin = &ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version}
		p.Owner = &ProviderOwnerReference{Type: ProviderOwnerPlugin, Construction: registration.Construction, CompatibilityAdapter: registration.CompatibilityAdapter}
		definition.BundleDigest = bundle.Digest
		proposal.Bundles = append(proposal.Bundles, bundle)
		for j := range proposal.Credentials {
			if proposal.Credentials[j].Owner.ProviderID == p.ID {
				proposal.Credentials[j].Owner = registration.Owner()
			}
		}
	}
}
