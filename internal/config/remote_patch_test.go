package config

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/stretchr/testify/require"
)

func TestRemoteModelPatchAdvancesRetainedCredentialGeneration(t *testing.T) {
	proposal := remoteRuntimeFixture(t, "minimal.plugin")
	root := t.TempDir()
	principal := strings.Repeat("a", 64)
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "workspace"), false, proposal, principal, env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)

	data, err := json.Marshal(proposal)
	require.NoError(t, err)
	var next RemoteRuntimeProposal
	require.NoError(t, json.Unmarshal(data, &next))
	next.Revision = 2
	next.Credentials[0].Generation = next.Revision
	selected := next.Models[SelectedModelTypeLarge]
	selected.ProviderOptions = map[string]any{"temperature": json.Number("0.25")}
	next.Models[SelectedModelTypeLarge] = selected
	next = sealRemoteRuntime(t, next)

	authority, err := store.PatchRemoteModelSelections(t.Context(), principal, proposal.Revision, proposal.Digest, next.Revision, next.Digest, map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: selected})
	require.NoError(t, err)
	require.Equal(t, next.Digest, authority.Digest)
	provider, found := store.Config().Providers.Get(next.Providers[0].Config.ID)
	require.True(t, found)
	require.Equal(t, "synthetic-client-$EXACT-key", provider.APIKey)
}

func TestRemoteRuntimeTransactionCommitsAtomically(t *testing.T) {
	proposal := remoteRuntimeFixture(t, "minimal.plugin")
	root := t.TempDir()
	principal := strings.Repeat("a", 64)
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "workspace"), false, proposal, principal, env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)

	data, err := json.Marshal(proposal)
	require.NoError(t, err)
	var next RemoteRuntimeProposal
	require.NoError(t, json.Unmarshal(data, &next))
	next.Revision = 2
	next.Credentials[0].Generation = next.Revision
	next.Credentials[0].APIKey = "targeted-second-key"
	selected := next.Models[SelectedModelTypeLarge]
	selected.ProviderOptions = map[string]any{"temperature": json.Number("0.25")}
	next.Models[SelectedModelTypeLarge] = selected
	next = sealRemoteRuntime(t, next)

	transaction := RemoteRuntimeTransaction{
		CredentialReplacements: []RemoteCredentialBinding{next.Credentials[0]},
		ModelSelections:        map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: selected},
	}
	authority, err := store.PatchRemoteRuntime(t.Context(), principal, proposal.Revision, proposal.Digest, next.Revision, next.Digest, transaction)
	require.NoError(t, err)
	require.Equal(t, next.Digest, authority.Digest)
	provider, found := store.Config().Providers.Get(next.Providers[0].Config.ID)
	require.True(t, found)
	require.Equal(t, "targeted-second-key", provider.APIKey)
	require.Equal(t, json.Number("0.25"), store.Config().Models[SelectedModelTypeLarge].ProviderOptions["temperature"])

	failed := next
	failed.Revision = 3
	failed.Credentials[0].Generation = failed.Revision
	failed.Credentials[0].APIKey = "must-not-commit"
	failed = sealRemoteRuntime(t, failed)
	before := store.RuntimeSnapshot()
	store.SetRuntimeGenerationPreparer(func(context.Context, RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
		return RuntimeGenerationCandidate{}, errors.New("synthetic transaction preparation failure")
	})
	_, err = store.PatchRemoteRuntime(t.Context(), principal, next.Revision, next.Digest, failed.Revision, failed.Digest, RemoteRuntimeTransaction{CredentialReplacements: []RemoteCredentialBinding{failed.Credentials[0]}})
	require.ErrorContains(t, err, "synthetic transaction preparation failure")
	require.Same(t, before.Config(), store.Config())
	require.Equal(t, before.RemoteAuthority(), store.RemoteAuthority())
}
