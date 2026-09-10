package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/redact"
	"github.com/stretchr/testify/require"
)

func TestProviderContextCollectionUsesCapturedClientDirectory(t *testing.T) {
	clientHome := t.TempDir()
	t.Setenv("HOME", clientHome)
	t.Setenv("USERPROFILE", clientHome)
	directory := filepath.Join(clientHome, ".ai-cli", "instructions")
	require.NoError(t, os.MkdirAll(directory, 0o700))
	path := filepath.Join(directory, "fixture.txt")
	require.NoError(t, os.WriteFile(path, []byte("exact client context\n"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(directory, "unselected.txt"), 0o700))
	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		"fixture": {ID: "fixture", Owner: &ProviderOwnerReference{Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}, Type: catalog.TypeOpenAICompat, APIKey: "synthetic-context-key", BaseURL: "https://fixture.invalid/v1", Models: []catalog.Model{{ID: "model"}}},
	}), Models: map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: {Provider: "fixture", Model: "model"}}, Options: &Options{}}
	store := NewTestStore(cfg)
	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome)
	t.Setenv("USERPROFILE", hostHome)
	hostDirectory := filepath.Join(hostHome, ".ai-cli", "instructions")
	require.NoError(t, os.MkdirAll(hostDirectory, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(hostDirectory, "fixture.txt"), []byte("wrong later host context"), 0o600))
	first, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"fixture": "exact client context\n"}, first.ProviderContextInstructions)
	require.NoError(t, os.WriteFile(path, []byte("edited client context"), 0o600))
	second, err := store.CollectRemoteRuntime(t.Context(), 2)
	require.NoError(t, err)
	require.Equal(t, "edited client context", second.ProviderContextInstructions["fixture"])
	require.Equal(t, "exact client context\n", first.ProviderContextInstructions["fixture"])
	require.NotEqual(t, first.Digest, second.Digest)
	require.NoError(t, os.Remove(path))
	missing, err := store.CollectRemoteRuntime(t.Context(), 3)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"fixture": ""}, missing.ProviderContextInstructions)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = store.CollectRemoteRuntime(ctx, 4)
	require.ErrorIs(t, err, context.Canceled)
}

func TestProviderContextReadsAreBoundedAndExplicit(t *testing.T) {
	for _, mode := range []string{"oversized", "directory", "invalid-utf8"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.txt")
			switch mode {
			case "oversized":
				require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", MaxProviderContextInstructionBytes+1)), 0o600))
			case "directory":
				require.NoError(t, os.Mkdir(path, 0o700))
			case "invalid-utf8":
				require.NoError(t, os.WriteFile(path, []byte{0xff}, 0o600))
			}
			_, err := readProviderContextInstructions(path)
			require.Error(t, err)
			require.NotContains(t, err.Error(), path)
		})
	}
}

func TestRemoteProviderContextPreservesCapturesAndRedaction(t *testing.T) {
	proposal := remoteRuntimeFixture(t, "minimal.plugin")
	id := proposal.Models[SelectedModelTypeLarge].Provider
	proposal.ProviderContextInstructions = map[string]string{id: "private-context-first-unique-marker"}
	proposal = sealRemoteRuntime(t, proposal)
	root := t.TempDir()
	principal := strings.Repeat("a", 64)
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, principal, env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	before := store.RuntimeSnapshot()
	proposal.ProviderContextInstructions[id] = "private-context-second-unique-marker"
	text, handled, err := before.ClientProviderContextInstructions(id)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, "private-context-first-unique-marker", text)
	store.RegisterRemoteRuntimeSecrets()
	require.Equal(t, redact.Replacement, redact.String(text))
	public, err := json.Marshal(store.Config())
	require.NoError(t, err)
	require.NotContains(t, string(public), "private-context")
	public, err = json.Marshal(store.RemoteAuthority())
	require.NoError(t, err)
	require.NotContains(t, string(public), "private-context")
	proposal.Revision = 2
	proposal = sealRemoteRuntime(t, proposal)
	_, err = store.ReplaceRemoteRuntime(t.Context(), proposal, principal, 1)
	require.NoError(t, err)
	text, handled, err = store.RuntimeSnapshot().ClientProviderContextInstructions(id)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, "private-context-second-unique-marker", text)
	text, _, err = before.ClientProviderContextInstructions(id)
	require.NoError(t, err)
	require.Equal(t, "private-context-first-unique-marker", text)
	for _, mode := range []string{"unselected", "oversized", "invalid-utf8"} {
		t.Run(mode, func(t *testing.T) {
			candidate := proposal
			candidate.Revision = 3
			candidate.ProviderContextInstructions = map[string]string{id: "unchanged"}
			switch mode {
			case "unselected":
				candidate.ProviderContextInstructions["../other"] = "unselected-private-context"
			case "oversized":
				candidate.ProviderContextInstructions[id] = strings.Repeat("x", MaxProviderContextInstructionBytes+1)
			case "invalid-utf8":
				candidate.ProviderContextInstructions[id] = string([]byte{0xff})
			}
			candidate = sealRemoteRuntime(t, candidate)
			accepted := store.RemoteAuthority()
			files := remoteBaselineTree(t, root)
			_, err := store.ReplaceRemoteRuntime(t.Context(), candidate, principal, 2)
			require.Error(t, err)
			require.Equal(t, accepted, store.RemoteAuthority())
			require.Equal(t, files, remoteBaselineTree(t, root))
		})
	}
}
