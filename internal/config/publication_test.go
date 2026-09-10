package config

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestPublicationFenceStableReadsAndStoreIdentity(t *testing.T) {
	cfg := &Config{Options: &Options{}}
	store := &ConfigStore{config: cfg}
	first := store.RuntimeSnapshot()
	require.NotZero(t, first.publicationSequence)
	require.True(t, first.SamePublication(store.RuntimeSnapshot()))
	require.NoError(t, store.WithRuntimeSnapshot(func(snapshot RuntimeSnapshot) error {
		require.True(t, first.SamePublication(snapshot))
		return nil
	}))
	other := &ConfigStore{config: cfg}
	require.False(t, first.SamePublication(other.RuntimeSnapshot()), "same config pointer in another store is a different incarnation")
	require.False(t, (RuntimeSnapshot{}).SamePublication(RuntimeSnapshot{}))
	require.False(t, first.SamePublication(RuntimeSnapshot{config: cfg}))
	require.False(t, (&ConfigStore{}).RuntimeSnapshot().SamePublication((&ConfigStore{}).RuntimeSnapshot()))
	encoded, err := json.Marshal(first) //nolint:staticcheck // The empty JSON result is the assertion: no snapshot identity is exported.
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(encoded), "private publication identity never enters JSON")

	store.writeMu.RLock()
	store.configMu.Lock()
	candidate := store.runtimeSnapshotLocked(&Config{}, store.resolver, store.providerRegistry, store.effectiveEnvironment)
	store.configMu.Unlock()
	store.writeMu.RUnlock()
	require.False(t, first.SamePublication(candidate))
	require.False(t, candidate.SamePublication(candidate), "unpublished prospective config has no accepted identity")
}

func TestPublicationFenceSameValueAndABA(t *testing.T) {
	cfg := &Config{Options: &Options{}}
	store := NewTestStore(cfg)
	initial := store.RuntimeSnapshot()
	store.setConfig(cfg)
	sameValue := store.RuntimeSnapshot()
	require.Same(t, initial.Config(), sameValue.Config())
	require.False(t, initial.SamePublication(sameValue))
	require.Equal(t, initial.publicationSequence+1, sameValue.publicationSequence)
	store.setConfig(&Config{Options: &Options{Debug: true}})
	intermediate := store.RuntimeSnapshot()
	store.setConfig(cfg)
	returned := store.RuntimeSnapshot()
	require.Same(t, cfg, returned.Config())
	require.False(t, initial.SamePublication(returned))
	require.False(t, sameValue.SamePublication(returned))
	require.False(t, intermediate.SamePublication(returned))
}

func TestPublicationFenceConcurrentInitialCaptureAndPublication(t *testing.T) {
	store := &ConfigStore{config: &Config{Options: &Options{}}}
	const readers = 16
	var group sync.WaitGroup
	captured := make(chan RuntimeSnapshot, readers)
	for range readers {
		group.Go(func() { captured <- store.RuntimeSnapshot() })
	}
	group.Wait()
	close(captured)
	initial := store.RuntimeSnapshot()
	for snapshot := range captured {
		require.True(t, initial.SamePublication(snapshot))
	}
	for range readers {
		group.Go(func() {
			for range 32 {
				_ = store.RuntimeSnapshot()
			}
		})
	}
	for range 32 {
		store.writeMu.Lock()
		store.setConfig(&Config{Options: &Options{}})
		store.writeMu.Unlock()
	}
	group.Wait()
	require.False(t, initial.SamePublication(store.RuntimeSnapshot()))
}

func TestPublicationFenceEphemeralPublicationPaths(t *testing.T) {
	registration := ownerTestRegistration("owner-test", "plugin.one")
	store := newForwardedAccountOwnerTestStore(t, registration)
	store.globalDataPath = filepath.Join(t.TempDir(), "crux.json")
	owner := registration.Owner()
	forwarded := map[string]ForwardedAccount{owner.AccountNamespace: {Owner: owner, Entry: accounts.Entry{ID: "one", AccessToken: "first", RefreshToken: "refresh"}}}
	before := store.RuntimeSnapshot()
	require.NoError(t, store.ApplyEphemeralProviderState(nil, forwarded))
	after := store.RuntimeSnapshot()
	require.False(t, before.SamePublication(after))
	require.NoError(t, store.ApplyEphemeralProviderState(nil, forwarded))
	repeated := store.RuntimeSnapshot()
	require.False(t, after.SamePublication(repeated), "identical forwarded state is still a publication")
	require.NoError(t, store.applyEphemeralToken(&oauth.Token{AccessToken: "second", RefreshToken: "rotated"}, owner))
	refreshed := store.RuntimeSnapshot()
	require.False(t, repeated.SamePublication(refreshed))
	entry, ok := refreshed.EphemeralAccount(owner)
	require.True(t, ok)
	require.Equal(t, "second", entry.AccessToken)
	require.NoError(t, store.RemoveProviderCredentials(ScopeGlobal, owner))
	removed := store.RuntimeSnapshot()
	require.False(t, refreshed.SamePublication(removed))
	_, ok = removed.EphemeralAccount(owner)
	require.False(t, ok)
}

func TestPublicationFenceReloadAndCollector(t *testing.T) {
	store, _ := runtimeControlTestStore(t, "generic-json", []manifest.RuntimeControl{})
	before := store.RuntimeSnapshot()
	require.NotZero(t, before.publicationSequence, "LoadIsolated initializes its publication")
	_, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.True(t, before.SamePublication(store.RuntimeSnapshot()), "collection is not a publication")
	var candidate RuntimeSnapshot
	store.SetRuntimeGenerationPreparer(func(_ context.Context, snapshot RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
		candidate = snapshot
		return RuntimeGenerationCandidate{}, errors.New("reject preparation")
	})
	err = store.ReloadFromDisk(t.Context())
	require.ErrorContains(t, err, "reject preparation")
	require.False(t, candidate.SamePublication(before))
	require.False(t, candidate.SamePublication(candidate))
	require.True(t, before.SamePublication(store.RuntimeSnapshot()))
	store.SetRuntimeGenerationPreparer(nil)
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	after := store.RuntimeSnapshot()
	require.False(t, before.SamePublication(after), "same-value accepted reload advances publication")
	require.True(t, after.SamePublication(store.RuntimeSnapshot()))
}

func TestPublicationFenceClientRuntimeReplacement(t *testing.T) {
	proposal := remoteRuntimeFixture(t, "minimal.plugin")
	root := t.TempDir()
	principal := strings.Repeat("a", 64)
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "workspace"), false, proposal, principal, env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	before := store.RuntimeSnapshot()
	require.NotZero(t, before.publicationSequence)
	proposal.Revision++
	for index := range proposal.Credentials {
		proposal.Credentials[index].Generation = proposal.Revision
	}
	proposal = sealRemoteRuntime(t, proposal)
	store.SetRuntimeGenerationPreparer(func(_ context.Context, candidate RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
		require.False(t, before.SamePublication(candidate), "compiled candidate belongs to a different store")
		return RuntimeGenerationCandidate{}, errors.New("reject replacement")
	})
	_, err = store.ReplaceRemoteRuntime(t.Context(), proposal, principal, 1)
	require.ErrorContains(t, err, "reject replacement")
	require.True(t, before.SamePublication(store.RuntimeSnapshot()))
	store.SetRuntimeGenerationPreparer(nil)
	_, err = store.ReplaceRemoteRuntime(t.Context(), proposal, principal, 1)
	require.NoError(t, err)
	after := store.RuntimeSnapshot()
	require.False(t, before.SamePublication(after))
	require.Equal(t, before.publicationSequence+1, after.publicationSequence)
	require.True(t, after.SamePublication(store.RuntimeSnapshot()))
}

func TestPublicationFenceOverflowDoesNotWrapOrPublish(t *testing.T) {
	store := NewTestStore(&Config{Options: &Options{}})
	store.configMu.Lock()
	store.publicationSequence = ^uint64(0)
	store.configMu.Unlock()
	before := store.RuntimeSnapshot()
	require.PanicsWithValue(t, "configuration publication sequence exhausted", func() {
		store.setConfig(&Config{Options: &Options{Debug: true}})
	})
	after := store.RuntimeSnapshot()
	require.True(t, before.SamePublication(after))
	require.Same(t, before.Config(), after.Config())
	require.Equal(t, ^uint64(0), after.publicationSequence)
}
