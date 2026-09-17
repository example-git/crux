package config

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestAdmitSecondaryClientAuthorityMergesDisjointProvider(t *testing.T) {
	primaryProposal := remoteRuntimeFixture(t, "minimal.plugin")
	secondaryProposal := remoteRuntimeFixture(t, "deepseek-preset.plugin")
	require.NotEqual(t, primaryProposal.Providers[0].Config.ID, secondaryProposal.Providers[0].Config.ID)

	root := t.TempDir()
	primaryPrincipal := strings.Repeat("a", 64)
	secondaryPrincipal := strings.Repeat("b", 64)
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "workspace"), false, primaryProposal, primaryPrincipal, env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	require.Equal(t, uint64(1), store.RemoteAuthority().Revision)

	authority, err := store.AdmitSecondaryClientAuthority(t.Context(), secondaryPrincipal, secondaryProposal)
	require.NoError(t, err)
	require.Equal(t, uint64(2), authority.Revision)
	require.Equal(t, primaryPrincipal, authority.Principal, "the primary owner's identity is unaffected by admitting a secondary owner")

	snapshot := store.RuntimeSnapshot()
	_, ok := snapshot.Config().Providers.Get(primaryProposal.Providers[0].Config.ID)
	require.True(t, ok, "primary provider must remain configured")
	_, ok = snapshot.Config().Providers.Get(secondaryProposal.Providers[0].Config.ID)
	require.True(t, ok, "secondary provider must be merged in")

	require.Equal(t, primaryPrincipal, snapshot.ProviderOwnerPrincipal(primaryProposal.Providers[0].Config.ID))
	require.Equal(t, secondaryPrincipal, snapshot.ProviderOwnerPrincipal(secondaryProposal.Providers[0].Config.ID))
	require.Equal(t, primaryPrincipal, snapshot.ProviderOwnerPrincipal("unknown-provider-defaults-to-primary"))
}

func TestAdmitSecondaryClientAuthorityRejectsCollidingProvider(t *testing.T) {
	primaryProposal := remoteRuntimeFixture(t, "minimal.plugin")
	collidingProposal := remoteRuntimeFixture(t, "minimal.plugin")

	root := t.TempDir()
	primaryPrincipal := strings.Repeat("a", 64)
	secondaryPrincipal := strings.Repeat("b", 64)
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "workspace"), false, primaryProposal, primaryPrincipal, env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)

	_, err = store.AdmitSecondaryClientAuthority(t.Context(), secondaryPrincipal, collidingProposal)
	require.ErrorContains(t, err, "collides")
	require.Equal(t, uint64(1), store.RemoteAuthority().Revision, "a rejected merge must not mutate the accepted authority")
}

func TestAdmitSecondaryClientAuthorityRejectsInvalidPrincipalAndSelfAdmission(t *testing.T) {
	primaryProposal := remoteRuntimeFixture(t, "minimal.plugin")
	secondaryProposal := remoteRuntimeFixture(t, "deepseek-preset.plugin")

	root := t.TempDir()
	primaryPrincipal := strings.Repeat("a", 64)
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "workspace"), false, primaryProposal, primaryPrincipal, env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)

	_, err = store.AdmitSecondaryClientAuthority(t.Context(), "not-hex-and-wrong-length", secondaryProposal)
	require.ErrorContains(t, err, "principal")

	_, err = store.AdmitSecondaryClientAuthority(t.Context(), primaryPrincipal, secondaryProposal)
	require.ErrorContains(t, err, "primary owner")

	secondaryPrincipal := strings.Repeat("b", 64)
	_, err = store.AdmitSecondaryClientAuthority(t.Context(), secondaryPrincipal, secondaryProposal)
	require.NoError(t, err)
	_, err = store.AdmitSecondaryClientAuthority(t.Context(), secondaryPrincipal, secondaryProposal)
	require.ErrorContains(t, err, "already contributed")
}

func TestAdmitSecondaryClientAuthorityRejectsLocalRuntime(t *testing.T) {
	primaryProposal := remoteRuntimeFixture(t, "minimal.plugin")
	secondaryProposal := remoteRuntimeFixture(t, "deepseek-preset.plugin")
	root := t.TempDir()
	store, err := CompileLocalRuntime(root, filepath.Join(root, "workspace"), false, primaryProposal, env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	_, err = store.AdmitSecondaryClientAuthority(t.Context(), strings.Repeat("b", 64), secondaryProposal)
	require.ErrorContains(t, err, "remote client-authority")
}

func TestReplaceRemoteRuntimePreservesSecondaryOwner(t *testing.T) {
	primaryProposal := remoteRuntimeFixture(t, "minimal.plugin")
	secondaryProposal := remoteRuntimeFixture(t, "deepseek-preset.plugin")

	root := t.TempDir()
	primaryPrincipal := strings.Repeat("a", 64)
	secondaryPrincipal := strings.Repeat("b", 64)
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "workspace"), false, primaryProposal, primaryPrincipal, env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	_, err = store.AdmitSecondaryClientAuthority(t.Context(), secondaryPrincipal, secondaryProposal)
	require.NoError(t, err)
	require.Equal(t, uint64(2), store.RemoteAuthority().Revision)

	replacement := primaryProposal
	replacement.Revision = 3
	replacement.Credentials = append([]RemoteCredentialBinding{}, primaryProposal.Credentials...)
	replacement.Credentials[0].Generation = 2
	replacement.Credentials[0].APIKey = "synthetic-replaced-key"
	replacement = sealRemoteRuntime(t, replacement)

	authority, err := store.ReplaceRemoteRuntime(t.Context(), replacement, primaryPrincipal, 2)
	require.NoError(t, err)
	require.Equal(t, uint64(3), authority.Revision)

	snapshot := store.RuntimeSnapshot()
	_, ok := snapshot.Config().Providers.Get(secondaryProposal.Providers[0].Config.ID)
	require.True(t, ok, "a primary replace must not drop an already-admitted secondary owner's provider")
	require.Equal(t, secondaryPrincipal, snapshot.ProviderOwnerPrincipal(secondaryProposal.Providers[0].Config.ID))
	require.Equal(t, primaryPrincipal, snapshot.ProviderOwnerPrincipal(primaryProposal.Providers[0].Config.ID))

	newProvider, _ := snapshot.Config().Providers.Get(primaryProposal.Providers[0].Config.ID)
	require.Equal(t, "synthetic-replaced-key", newProvider.APIKey)
}

func TestPatchRemoteRuntimeRejectsCrossOwnerProviderRemoval(t *testing.T) {
	primaryProposal := remoteRuntimeFixture(t, "minimal.plugin")
	secondaryProposal := remoteRuntimeFixture(t, "deepseek-preset.plugin")

	root := t.TempDir()
	primaryPrincipal := strings.Repeat("a", 64)
	secondaryPrincipal := strings.Repeat("b", 64)
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "workspace"), false, primaryProposal, primaryPrincipal, env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	_, err = store.AdmitSecondaryClientAuthority(t.Context(), secondaryPrincipal, secondaryProposal)
	require.NoError(t, err)

	secondaryOwner := secondaryProposal.Credentials[0].Owner
	_, err = store.PatchRemoteRuntime(t.Context(), primaryPrincipal, 2, store.RemoteAuthority().Digest, 3, "irrelevant-result-digest", RemoteRuntimeTransaction{
		DefinitionRemovals: []providerregistry.RegistrationOwner{secondaryOwner},
	})
	require.ErrorContains(t, err, "owned by a different attached client")

	snapshot := store.RuntimeSnapshot()
	_, ok := snapshot.Config().Providers.Get(secondaryProposal.Providers[0].Config.ID)
	require.True(t, ok, "the rejected removal must not have taken effect")
}

// resultDigestFor computes the digest a caller must supply as resultDigest
// for a PatchOwnedModelSelections call, matching each of its two internal
// code paths exactly:
//   - filterOwner == "" (an admitted secondary owner's own selection): the
//     full merged proposal with Models overwritten by selections.
//   - filterOwner == the primary principal (delegates to
//     PatchRemoteModelSelections/remotePatchProposal): the same, but with
//     every other admitted owner's providers/credentials/bundles stripped
//     back out first, since remotePatchProposal builds its base proposal as
//     the primary owner's own contribution only.
func resultDigestFor(t *testing.T, store *ConfigStore, filterOwner string, resultRevision uint64, selections map[SelectedModelType]SelectedModel) string {
	t.Helper()
	snapshot := store.RuntimeSnapshot()
	data, err := json.Marshal(snapshot.clientRuntime.proposal)
	require.NoError(t, err)
	var merged RemoteRuntimeProposal
	require.NoError(t, json.Unmarshal(data, &merged))
	if filterOwner != "" {
		owner := snapshot.clientRuntime.providerOwner
		merged.Providers = slices.DeleteFunc(merged.Providers, func(definition RemoteProviderDefinition) bool {
			return owner[definition.Config.ID] != "" && owner[definition.Config.ID] != filterOwner
		})
		merged.Credentials = slices.DeleteFunc(merged.Credentials, func(binding RemoteCredentialBinding) bool {
			return owner[binding.Owner.ProviderID] != "" && owner[binding.Owner.ProviderID] != filterOwner
		})
		require.NoError(t, pruneRemoteRuntimeBundles(&merged))
	}
	if merged.Models == nil {
		merged.Models = map[SelectedModelType]SelectedModel{}
	}
	for modelType, selected := range selections {
		merged.Models[modelType] = cloneSelectedModel(selected)
	}
	for index := range merged.Credentials {
		merged.Credentials[index].Generation = resultRevision
	}
	merged.Revision = resultRevision
	merged.Digest = ""
	digest, err := RemoteRuntimeDigest(merged)
	require.NoError(t, err)
	return digest
}

func TestPatchOwnedModelSelectionsAllowsSecondaryOwnerToSelectOwnModel(t *testing.T) {
	primaryProposal := remoteRuntimeFixture(t, "minimal.plugin")
	secondaryProposal := remoteRuntimeFixture(t, "deepseek-preset.plugin")

	root := t.TempDir()
	primaryPrincipal := strings.Repeat("a", 64)
	secondaryPrincipal := strings.Repeat("b", 64)
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "workspace"), false, primaryProposal, primaryPrincipal, env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	_, err = store.AdmitSecondaryClientAuthority(t.Context(), secondaryPrincipal, secondaryProposal)
	require.NoError(t, err)
	require.Equal(t, uint64(2), store.RemoteAuthority().Revision)

	ownModel := secondaryProposal.Models[SelectedModelTypeLarge]
	selections := map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: ownModel}
	resultDigest := resultDigestFor(t, store, "", 3, selections)

	authority, err := store.PatchOwnedModelSelections(t.Context(), secondaryPrincipal, 2, store.RemoteAuthority().Digest, 3, resultDigest, selections)
	require.NoError(t, err)
	require.Equal(t, uint64(3), authority.Revision)
	require.Equal(t, primaryPrincipal, authority.Principal, "the workspace's primary identity is unaffected by a secondary's model selection")

	snapshot := store.RuntimeSnapshot()
	require.Equal(t, ownModel, snapshot.Config().Models[SelectedModelTypeLarge])
	require.Equal(t, secondaryPrincipal, snapshot.ProviderOwnerPrincipal(secondaryProposal.Providers[0].Config.ID), "ownership is unaffected by selecting the owned model")
}

func TestPatchOwnedModelSelectionsRejectsCrossOwnerSelection(t *testing.T) {
	primaryProposal := remoteRuntimeFixture(t, "minimal.plugin")
	secondaryProposal := remoteRuntimeFixture(t, "deepseek-preset.plugin")

	root := t.TempDir()
	primaryPrincipal := strings.Repeat("a", 64)
	secondaryPrincipal := strings.Repeat("b", 64)
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "workspace"), false, primaryProposal, primaryPrincipal, env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	_, err = store.AdmitSecondaryClientAuthority(t.Context(), secondaryPrincipal, secondaryProposal)
	require.NoError(t, err)

	primaryModel := primaryProposal.Models[SelectedModelTypeLarge]
	selections := map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: primaryModel}
	resultDigest := resultDigestFor(t, store, "", 3, selections)

	_, err = store.PatchOwnedModelSelections(t.Context(), secondaryPrincipal, 2, store.RemoteAuthority().Digest, 3, resultDigest, selections)
	require.ErrorContains(t, err, "owned by a different attached client")
	require.Equal(t, uint64(2), store.RemoteAuthority().Revision, "a rejected cross-owner selection must not mutate the accepted authority")

	snapshot := store.RuntimeSnapshot()
	require.Equal(t, primaryModel, snapshot.Config().Models[SelectedModelTypeLarge], "the primary's own model selection must be unchanged")
}

func TestPatchOwnedModelSelectionsPrimaryDelegatesToExistingPath(t *testing.T) {
	primaryProposal := remoteRuntimeFixture(t, "minimal.plugin")
	secondaryProposal := remoteRuntimeFixture(t, "deepseek-preset.plugin")

	root := t.TempDir()
	primaryPrincipal := strings.Repeat("a", 64)
	secondaryPrincipal := strings.Repeat("b", 64)
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "workspace"), false, primaryProposal, primaryPrincipal, env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	_, err = store.AdmitSecondaryClientAuthority(t.Context(), secondaryPrincipal, secondaryProposal)
	require.NoError(t, err)

	ownModel := primaryProposal.Models[SelectedModelTypeSmall]
	selections := map[SelectedModelType]SelectedModel{SelectedModelTypeSmall: ownModel}
	resultDigest := resultDigestFor(t, store, primaryPrincipal, 3, selections)

	authority, err := store.PatchOwnedModelSelections(t.Context(), primaryPrincipal, 2, store.RemoteAuthority().Digest, 3, resultDigest, selections)
	require.NoError(t, err)
	require.Equal(t, uint64(3), authority.Revision)

	snapshot := store.RuntimeSnapshot()
	require.Equal(t, ownModel, snapshot.Config().Models[SelectedModelTypeSmall])
	_, ok := snapshot.Config().Providers.Get(secondaryProposal.Providers[0].Config.ID)
	require.True(t, ok, "the primary's own model-selection patch must not drop the secondary owner's provider")
}
