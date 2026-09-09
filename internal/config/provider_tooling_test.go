package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func providerToolingTestStore(t *testing.T) (*ConfigStore, string) {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	workingDir := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	require.NoError(t, os.MkdirAll(workingDir, 0o700))
	t.Setenv("AI_CLI_DIR", filepath.Join(root, "accounts"))
	base := env.NewFromMap(map[string]string{
		"HOME": root, "CRUX_GLOBAL_CONFIG": configDir,
		"CRUX_GLOBAL_DATA":      filepath.Join(root, "global-data"),
		"CRUX_CACHE_DIR":        filepath.Join(root, "cache"),
		"CRUX_PROVIDER_PROFILE": string(ProviderProfileIntegrated),
	})
	cfg := &Config{
		Options: &Options{InstructionMode: "project"},
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"codex": {ID: "codex", APIKey: "synthetic-tooling-key", Models: []catalog.Model{{ID: "fixture"}}},
		}),
		Models: map[SelectedModelType]SelectedModel{
			SelectedModelTypeLarge: {Provider: "codex", Model: "fixture"},
			SelectedModelTypeSmall: {Provider: "codex", Model: "fixture"},
		},
	}
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "crux.json"), mustMarshalConfig(cfg), 0o600))
	store, err := LoadIsolated(workingDir, filepath.Join(root, "workspace-data"), false, base)
	require.NoError(t, err)
	require.True(t, store.Config().IsProviderAvailable("codex"))
	return store, root
}

func TestProviderToolingSetRemoveScopesAndSnapshots(t *testing.T) {
	store, _ := providerToolingTestStore(t)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("codex")
	require.True(t, ok)
	before := store.RuntimeSnapshot()
	require.NoError(t, store.SetProviderToolingInstructions(ScopeGlobal, owner, ToolingInstructionsNative))
	provider, _ := store.Config().Providers.Get("codex")
	require.Equal(t, ToolingInstructionsNative, provider.ToolingInstructions)
	require.Equal(t, ToolingInstructionsNative, gjson.GetBytes(readOwnerMutationTestFile(t, store.globalDataPath), "providers.codex.tooling_instructions").String())
	oldProvider, _ := before.Config().Providers.Get("codex")
	require.Empty(t, oldProvider.ToolingInstructions)
	require.NotSame(t, before.Config(), store.Config())
	registration, registered := store.RuntimeSnapshot().ProviderRegistration("codex")
	require.True(t, registered)
	require.NotEmpty(t, strings.TrimSpace(registration.Instructions.Profiles[registration.Instructions.Default]))
	require.NoError(t, store.Config().ValidateProviderToolingInstructions())

	require.NoError(t, store.SetProviderToolingInstructions(ScopeWorkspace, owner, ToolingInstructionsCrux))
	provider, _ = store.Config().Providers.Get("codex")
	require.Equal(t, ToolingInstructionsCrux, provider.ToolingInstructions)
	require.NoError(t, store.RemoveProviderToolingInstructions(ScopeWorkspace, owner))
	require.False(t, gjson.GetBytes(readOwnerMutationTestFile(t, store.workspacePath), "providers.codex.tooling_instructions").Exists())
	provider, _ = store.Config().Providers.Get("codex")
	require.Equal(t, ToolingInstructionsNative, provider.ToolingInstructions, "removal must reveal the lower-scope profile")
	require.NoError(t, store.RemoveProviderToolingInstructions(ScopeGlobal, owner))
	require.False(t, gjson.GetBytes(readOwnerMutationTestFile(t, store.globalDataPath), "providers.codex.tooling_instructions").Exists())
	provider, _ = store.Config().Providers.Get("codex")
	require.Empty(t, provider.ToolingInstructions, "removal must restore the absent field, not write an empty or null override")
	require.NoError(t, store.Config().ValidateProviderToolingInstructions())
}

func TestProviderToolingSetRejectsShadowedScope(t *testing.T) {
	store, root := providerToolingTestStore(t)
	owner, _ := store.RuntimeSnapshot().ProviderOwner("codex")
	require.NoError(t, store.SetProviderToolingInstructions(ScopeWorkspace, owner, ToolingInstructionsCrux))
	before := store.Config()
	filesBefore := remoteBaselineTree(t, root)
	err := store.SetProviderToolingInstructions(ScopeGlobal, owner, ToolingInstructionsNative)
	require.ErrorContains(t, err, "shadowed")
	require.Same(t, before, store.Config())
	require.Equal(t, filesBefore, remoteBaselineTree(t, root))
}

func TestProviderToolingRemoveRejectsInvalidInheritedProfile(t *testing.T) {
	store, root := providerToolingTestStore(t)
	owner, _ := store.RuntimeSnapshot().ProviderOwner("codex")
	require.NoError(t, store.SetProviderToolingInstructions(ScopeGlobal, owner, ToolingInstructionsNative))
	basePath := filepath.Join(root, "config", "crux.json")
	invalid := "official"
	data, err := providerToolingConfigChange(readOwnerMutationTestFile(t, basePath), "codex", &invalid)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(basePath, data, 0o600))
	before := store.Config()
	filesBefore := remoteBaselineTree(t, root)
	require.ErrorContains(t, store.RemoveProviderToolingInstructions(ScopeGlobal, owner), "unsupported tooling")
	require.Same(t, before, store.Config())
	require.Equal(t, filesBefore, remoteBaselineTree(t, root))
}

func TestProviderToolingRejectsInvalidExplicitProfileBeforeIO(t *testing.T) {
	for _, profile := range []string{"", "official", "Native", " native "} {
		t.Run(profile, func(t *testing.T) {
			store, root := providerToolingTestStore(t)
			owner, _ := store.RuntimeSnapshot().ProviderOwner("codex")
			before := store.Config()
			filesBefore := remoteBaselineTree(t, root)
			require.Error(t, store.SetProviderToolingInstructions(ScopeGlobal, owner, profile))
			require.Same(t, before, store.Config())
			require.Equal(t, filesBefore, remoteBaselineTree(t, root))
		})
	}
}

func TestProviderToolingValidationRequiresExactUsableCapability(t *testing.T) {
	for _, test := range []struct {
		name    string
		profile string
		change  func(*providerregistry.Registration, *ProviderConfig)
		valid   bool
	}{
		{name: "explicit native", profile: "native", valid: true},
		{name: "explicit crux", profile: "crux", valid: true},
		{name: "implicit crux", valid: true},
		{name: "implicit native", change: func(r *providerregistry.Registration, _ *ProviderConfig) { r.Instructions.SelectionDefault = "native" }, valid: true},
		{name: "unknown explicit despite project mode", profile: "official"},
		{name: "missing capability", profile: "native", change: func(r *providerregistry.Registration, _ *ProviderConfig) { r.Instructions = nil }},
		{name: "missing default profile", profile: "native", change: func(r *providerregistry.Registration, _ *ProviderConfig) { r.Instructions.Default = "missing" }},
		{name: "blank text", profile: "native", change: func(r *providerregistry.Registration, _ *ProviderConfig) {
			r.Instructions.Profiles["bundled"] = " \n\t"
		}},
		{name: "implicit native with blank text", change: func(r *providerregistry.Registration, _ *ProviderConfig) {
			r.Instructions.SelectionDefault = "native"
			r.Instructions.Profiles["bundled"] = ""
		}},
		{name: "different plugin generation", profile: "native", change: func(_ *providerregistry.Registration, p *ProviderConfig) { p.Plugin.Version = "2.0.0" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			registration := ownerTestRegistration("owner-test", "tooling.fixture")
			registration.Instructions = &providerregistry.InstructionCapability{Default: "bundled", Profiles: map[string]string{"bundled": "Synthetic native instructions."}}
			provider := ownerMutationTestProvider(registration)
			provider.ToolingInstructions = test.profile
			if test.change != nil {
				test.change(&registration, &provider)
			}
			cfg := &Config{Options: &Options{InstructionMode: "project"}, Providers: csync.NewMapFrom(map[string]ProviderConfig{"owner-test": provider})}
			store := NewTestStoreWithRegistrations(cfg, registration)
			err := store.Config().ValidateProviderToolingInstructions()
			if test.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestProviderToolingRejectsNativeWithoutCapabilityBeforeIO(t *testing.T) {
	registration := ownerTestRegistration("owner-test", "tooling.no-native")
	store, path := newOwnerMutationTestStore(t, registration)
	before := store.Config()
	diskBefore := readOwnerMutationTestFile(t, path)
	require.ErrorContains(t, store.SetProviderToolingInstructions(ScopeGlobal, registration.Owner(), ToolingInstructionsNative), "does not provide native")
	require.Same(t, before, store.Config())
	require.Equal(t, diskBefore, readOwnerMutationTestFile(t, path))
}

func TestProviderToolingRejectsOwnerChangeAndCancellationAtLock(t *testing.T) {
	for _, action := range []string{"set-owner", "remove-owner", "set-cancel", "remove-cancel"} {
		t.Run(action, func(t *testing.T) {
			registration := ownerTestRegistration("owner-test", "tooling.old")
			store, path := newOwnerMutationTestStore(t, registration)
			diskBefore := readOwnerMutationTestFile(t, path)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			started := make(chan struct{})
			done := make(chan error, 1)
			store.writeMu.Lock()
			go func() {
				close(started)
				if strings.HasPrefix(action, "set") {
					done <- store.SetProviderToolingInstructionsContext(ctx, ScopeGlobal, registration.Owner(), ToolingInstructionsCrux)
				} else {
					done <- store.RemoveProviderToolingInstructionsContext(ctx, ScopeGlobal, registration.Owner())
				}
			}()
			<-started
			before := store.Config()
			if strings.HasSuffix(action, "cancel") {
				cancel()
			} else {
				before = replaceOwnerMutationTestGenerationLocked(t, store, ownerTestRegistration("owner-test", "tooling.new"))
			}
			store.writeMu.Unlock()
			err := <-done
			if strings.HasSuffix(action, "cancel") {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorContains(t, err, "active owner")
			}
			require.Same(t, before, store.Config())
			require.Equal(t, diskBefore, readOwnerMutationTestFile(t, path))
		})
	}
}

func TestProviderToolingAndGenericMutationsRejectDetachedBeforeIO(t *testing.T) {
	proposal := remoteRuntimeFixture(t, "minimal.plugin")
	root := t.TempDir()
	t.Chdir(root)
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	owner := proposal.Credentials[0].Owner
	before := store.Config()
	filesBefore := remoteBaselineTree(t, root)
	for _, scope := range []Scope{ScopeGlobal, ScopeWorkspace, Scope(99)} {
		for _, action := range []func() error{
			func() error { return store.SetProviderToolingInstructions(scope, owner, ToolingInstructionsCrux) },
			func() error { return store.RemoveProviderToolingInstructions(scope, owner) },
			func() error { return store.SetConfigField(scope, "mcp.test", map[string]any{"type": "http"}) },
			func() error { return store.RemoveConfigField(scope, "providers.test.tooling_instructions") },
		} {
			require.ErrorIs(t, action(), ErrClientRuntimeManaged)
			require.Same(t, before, store.Config())
			require.Equal(t, filesBefore, remoteBaselineTree(t, root))
		}
	}
}

func TestProviderToolingConfigPathsRejectMissingAndUnknownBeforeIO(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	store := &ConfigStore{config: &Config{}, workingDir: root}
	for _, scope := range []Scope{ScopeGlobal, ScopeWorkspace, Scope(-1), Scope(2)} {
		filesBefore := remoteBaselineTree(t, root)
		require.Error(t, store.SetConfigField(scope, "test", true))
		require.Error(t, store.RemoveConfigField(scope, "test"))
		require.Equal(t, filesBefore, remoteBaselineTree(t, root))
	}
	store.globalDataPath = filepath.Join(root, "global.json")
	store.workspacePath = filepath.Join(root, "workspace.json")
	filesBefore := remoteBaselineTree(t, root)
	for _, scope := range []Scope{Scope(-1), Scope(2)} {
		require.Error(t, store.SetConfigField(scope, "test", true))
		require.Error(t, store.RemoveConfigField(scope, "test"))
	}
	require.Equal(t, filesBefore, remoteBaselineTree(t, root))
}
