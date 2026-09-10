package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestClientProviderContextUsesCapturedTextWithoutHostFallback(t *testing.T) {
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	directory := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(directory, "fixture.txt"), []byte("execution-host context"), 0o600))
	builder, err := NewPrompt("coder", "generated prompt", withProviderInstructionsDir(directory), WithWorkingDir(t.TempDir()))
	require.NoError(t, err)
	owner := providerregistry.RegistrationOwner{ProviderID: "fixture"}
	proposal := config.RemoteRuntimeProposal{
		Version: config.RemoteRuntimeVersion, Revision: 1,
		Providers:                   []config.RemoteProviderDefinition{{Config: config.ProviderConfig{ID: "fixture", Type: catalog.TypeOpenAICompat, BaseURL: "https://fixture.invalid/v1", Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}, Models: []catalog.Model{{ID: "model"}}}}},
		Models:                      map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: {Provider: "fixture", Model: "model"}},
		Credentials:                 []config.RemoteCredentialBinding{{Owner: owner, Generation: 1, APIKey: "synthetic-context-key"}},
		ProviderContextInstructions: map[string]string{"fixture": "client captured context"},
	}
	proposal.Digest, err = config.RemoteRuntimeDigest(proposal)
	require.NoError(t, err)
	principal := strings.Repeat("a", 64)
	remote, err := config.CompileRemoteRuntime(t.TempDir(), t.TempDir(), false, proposal, principal, env.NewFromMap(map[string]string{}))
	require.NoError(t, err)
	captured := remote.RuntimeSnapshot()
	first, err := builder.BuildInstructionsWithSnapshot(t.Context(), "fixture", "model", remote, captured)
	require.NoError(t, err)
	require.Contains(t, first.String(), "client captured context")
	require.NotContains(t, first.String(), "execution-host context")
	proposal.Revision = 2
	proposal.ProviderContextInstructions = nil
	proposal.Digest, err = config.RemoteRuntimeDigest(proposal)
	require.NoError(t, err)
	_, err = remote.ReplaceRemoteRuntime(t.Context(), proposal, principal, 1)
	require.NoError(t, err)
	missing, err := builder.BuildInstructions(t.Context(), "fixture", "model", remote)
	require.NoError(t, err)
	require.NotContains(t, missing.String(), "context")
	old, err := builder.BuildInstructionsWithSnapshot(t.Context(), "fixture", "model", remote, captured)
	require.NoError(t, err)
	require.Contains(t, old.String(), "client captured context")
	require.NotContains(t, old.String(), "execution-host context")
	local := config.NewTestStore(&config.Config{Options: &config.Options{}})
	host, err := builder.BuildInstructions(t.Context(), "fixture", "model", local)
	require.NoError(t, err)
	require.Contains(t, host.String(), "execution-host context", "server-owned prompts retain their host file behavior")
	auxiliary, err := NewPrompt("task", "auxiliary prompt", withProviderInstructionsDir(directory))
	require.NoError(t, err)
	aux, err := auxiliary.BuildInstructionsWithSnapshot(t.Context(), "fixture", "model", remote, captured)
	require.NoError(t, err)
	require.NotContains(t, aux.String(), "context", "provider context remains limited to coder prompts")
}
