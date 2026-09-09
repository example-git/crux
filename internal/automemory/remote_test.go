package automemory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestClientOwnedMemoryUsesScopedPromptAndToolStorage(t *testing.T) {
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	t.Setenv("CRUX_AUTO_MEMORY_DIR", t.TempDir())
	t.Setenv("CRUX_GLOBAL_DATA", t.TempDir())
	root := t.TempDir()
	proposal := config.RemoteRuntimeProposal{Version: 1, Revision: 1, Providers: []config.RemoteProviderDefinition{{Config: config.ProviderConfig{ID: "fixture", Type: catalog.TypeOpenAICompat, BaseURL: "https://example.invalid/v1", Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}, Models: []catalog.Model{{ID: "model"}}}}}, Models: map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: {Provider: "fixture", Model: "model"}}}
	proposal.Credentials = []config.RemoteCredentialBinding{{Owner: providerregistry.RegistrationOwner{ProviderID: "fixture"}, Generation: 1, APIKey: "synthetic-test-key"}}
	var err error
	proposal.Digest, err = config.RemoteRuntimeDigest(proposal)
	require.NoError(t, err)
	stores := []*config.ConfigStore{}
	for _, name := range []string{"a", "b"} {
		store, err := config.CompileRemoteRuntime(root, filepath.Join(root, name), false, proposal, strings.Repeat(name, 64), env.NewFromMap(nil))
		require.NoError(t, err)
		stores = append(stores, store)
	}
	for _, directory := range []string{os.Getenv("CRUX_AUTO_MEMORY_DIR"), UserDirectory()} {
		require.NoError(t, os.MkdirAll(directory, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(directory, "host-decision.md"), []byte("---\nname: Decision\ndescription: Host decision\ntype: feedback\n---\n\nserver-memory-only-marker"), 0o600))
	}
	for _, scope := range []Scope{ScopeProject, ScopeUser} {
		_, err := NewServiceForStore(stores[0]).Upsert(t.Context(), scope, Entry{File: "decision", Name: "Decision", Description: "Saved by first owner", Type: "feedback", Content: "first-client-only"})
		require.NoError(t, err)
		entries, err := NewServiceForStore(stores[1]).List(t.Context(), scope)
		require.NoError(t, err)
		require.Empty(t, entries)
	}
	first, err := LoadForStore(t.Context(), stores[0])
	require.NoError(t, err)
	second, err := LoadForStore(t.Context(), stores[1])
	require.NoError(t, err)
	require.Contains(t, Prompt(first), "decision.md")
	require.NotContains(t, Prompt(second), "decision.md")
	require.Equal(t, filepath.Join(root, "a", "memory", "project"), first.Directory)
	require.Equal(t, filepath.Join(root, "a", "memory", "user"), first.UserDirectory)
	firstRelevant, err := RelevantForStore(t.Context(), stores[0], "decision", time.Now())
	require.NoError(t, err)
	require.Contains(t, firstRelevant, "first-client-only")
	require.NotContains(t, firstRelevant, "server-memory-only-marker")
	secondRelevant, err := RelevantForStore(t.Context(), stores[1], "decision", time.Now())
	require.NoError(t, err)
	require.Empty(t, secondRelevant)
	localRelevant, err := Relevant(t.Context(), root, "decision", time.Now())
	require.NoError(t, err)
	require.Contains(t, localRelevant, "server-memory-only-marker", "the ordinary local path still loads host memory")
}
