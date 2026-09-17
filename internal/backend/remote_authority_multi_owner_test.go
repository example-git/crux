package backend

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// synthClientProviderProposal builds a minimal, self-consistent client
// runtime proposal for a single synthetic openai-compat provider, suitable
// for exercising client-authority workspace admission without a plugin
// bundle.
func synthClientProviderProposal(t *testing.T, providerID string) config.RemoteRuntimeProposal {
	t.Helper()
	owner := providerregistry.RegistrationOwner{ProviderID: providerID}
	proposal := config.RemoteRuntimeProposal{
		Version: config.RemoteRuntimeVersion, Revision: 1,
		Providers: []config.RemoteProviderDefinition{{Config: config.ProviderConfig{
			ID: providerID, Name: providerID, Type: catalog.TypeOpenAICompat, BaseURL: "http://127.0.0.1:1/v1",
			Owner:  &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
			Models: []catalog.Model{{ID: "model", Name: "Model", ContextWindow: 8192, DefaultMaxTokens: 128}},
		}}},
		Models:      map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: {Provider: providerID, Model: "model"}, config.SelectedModelTypeSmall: {Provider: providerID, Model: "model"}},
		Credentials: []config.RemoteCredentialBinding{{Owner: owner, Generation: 1, APIKey: "synthetic-" + providerID}},
		// The primary owner must explicitly opt in before any distinct
		// principal may attach to this workspace as a secondary owner (see
		// backend.checkWorkspaceReuse). Harmless on a fixture reused as a
		// secondary's own proposal, since that flag is never itself
		// consulted for the gate.
		AllowSecondaryOwners: true,
	}
	digest, err := config.RemoteRuntimeDigest(proposal)
	require.NoError(t, err)
	proposal.Digest = digest
	return proposal
}

func clientAuthorityArgs(t *testing.T, path, principal, providerID string) proto.Workspace {
	t.Helper()
	proposal := synthClientProviderProposal(t, providerID)
	args := protoWS(path, t.TempDir(), uuid.NewString())
	args.AuthorityMode = "client"
	args.AuthenticatedPrincipal = principal
	args.Runtime = &proposal
	return args
}

func TestCreateWorkspaceAdmitsDisjointSecondaryPrincipalAsOwner(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	b := New(ctx, nil, func() {})
	t.Cleanup(func() { drainBackend(t, b) })

	path := t.TempDir()
	principalA := strings.Repeat("a", 64)
	principalB := strings.Repeat("b", 64)

	first, _, err := b.CreateWorkspace(clientAuthorityArgs(t, path, principalA, "fixture-a"))
	require.NoError(t, err)
	t.Cleanup(first.Shutdown)
	require.Equal(t, principalA, first.Cfg.RuntimeSnapshot().ProviderOwnerPrincipal("fixture-a"))

	second, _, err := b.CreateWorkspace(clientAuthorityArgs(t, path, principalB, "fixture-b"))
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID, "a disjoint secondary owner reuses the existing workspace")
	require.Equal(t, principalA, second.Cfg.RuntimeSnapshot().ProviderOwnerPrincipal("fixture-a"), "the primary owner's provider is unaffected")
	require.Equal(t, principalB, second.Cfg.RuntimeSnapshot().ProviderOwnerPrincipal("fixture-b"), "the secondary owner's provider is admitted and locked to it")

	_, ok := second.Cfg.RuntimeSnapshot().Config().Providers.Get("fixture-a")
	require.True(t, ok)
	_, ok = second.Cfg.RuntimeSnapshot().Config().Providers.Get("fixture-b")
	require.True(t, ok)

	// Drain synchronously before returning: releasing a workspace hold can
	// still write into path's tree, and t.Cleanup alone cannot guarantee
	// this runs before the TempDir removals registered later in this test
	// (Cleanup funcs run LIFO, and clientAuthorityArgs allocates further
	// TempDirs after the drainBackend cleanup above was registered).
	first.Shutdown()
	drainBackend(t, b)
}

func TestCreateWorkspaceRejectsCollidingSecondaryPrincipalProvider(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	b := New(ctx, nil, func() {})
	t.Cleanup(func() { drainBackend(t, b) })

	path := t.TempDir()
	principalA := strings.Repeat("a", 64)
	principalB := strings.Repeat("b", 64)

	first, _, err := b.CreateWorkspace(clientAuthorityArgs(t, path, principalA, "fixture-a"))
	require.NoError(t, err)
	t.Cleanup(first.Shutdown)

	_, _, err = b.CreateWorkspace(clientAuthorityArgs(t, path, principalB, "fixture-a"))
	require.ErrorIs(t, err, ErrRuntimeConflict)

	// See the drain comment in TestCreateWorkspaceAdmitsDisjointSecondaryPrincipalAsOwner.
	first.Shutdown()
	drainBackend(t, b)
}

func TestCreateWorkspaceSamePrincipalStillRequiresExactRuntimeMatch(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	b := New(ctx, nil, func() {})
	t.Cleanup(func() { drainBackend(t, b) })

	path := t.TempDir()
	principalA := strings.Repeat("a", 64)

	first, _, err := b.CreateWorkspace(clientAuthorityArgs(t, path, principalA, "fixture-a"))
	require.NoError(t, err)
	t.Cleanup(first.Shutdown)

	_, _, err = b.CreateWorkspace(clientAuthorityArgs(t, path, principalA, "fixture-b"))
	require.ErrorIs(t, err, ErrRuntimeConflict, "the same principal reattaching with a different runtime must still require an explicit revision update, not a silent merge")

	// See the drain comment in TestCreateWorkspaceAdmitsDisjointSecondaryPrincipalAsOwner.
	first.Shutdown()
	drainBackend(t, b)
}
