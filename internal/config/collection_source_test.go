package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

type collectionSourceTestResolver struct {
	calls int
}

func (r *collectionSourceTestResolver) ResolveValue(value string) (string, error) {
	if value == "$(synthetic-collection-expression)" {
		r.calls++
		return "synthetic-resolved-collection-key", nil
	}
	return value, nil
}

func collectionSourceTestProposal(t *testing.T) (*ConfigStore, RemoteRuntimeProposal, *collectionSourceTestResolver) {
	t.Helper()
	store, root := runtimeControlTestStore(t, "generic-json", []manifest.RuntimeControl{})
	next := store.Config().cloneForWrite()
	provider, _ := next.Providers.Get("example-echo")
	provider.APIKey = "$(synthetic-collection-expression)"
	next.Providers.Set(provider.ID, provider)
	resolver := &collectionSourceTestResolver{}
	store.writeMu.Lock()
	store.resolver = resolver
	store.effectiveEnvironment = env.NewFromMap(map[string]string{"HOME": root, "PRIVATE_COLLECTION_ENV": "synthetic-private-collection-environment"})
	store.setConfig(next)
	store.writeMu.Unlock()
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Positive(t, resolver.calls)
	return store, proposal, resolver
}

func cloneCollectionSourceTestProposal(t *testing.T, proposal RemoteRuntimeProposal) RemoteRuntimeProposal {
	t.Helper()
	data, err := json.Marshal(proposal)
	require.NoError(t, err)
	var result RemoteRuntimeProposal
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&result))
	return result
}

func TestCollectionSourceRetainsExactAdmittedConfigWithoutWireChanges(t *testing.T) {
	store, proposal, resolver := collectionSourceTestProposal(t)
	calls := resolver.calls
	require.NotNil(t, proposal.collectionSource)
	require.Same(t, store.Config(), proposal.CollectionConfig())
	require.True(t, proposal.collectionSource.runtime.SamePublication(store.RuntimeSnapshot()))
	provider, _ := proposal.CollectionConfig().Providers.Get("example-echo")
	require.Equal(t, "$(synthetic-collection-expression)", provider.APIKey)
	require.Equal(t, "synthetic-resolved-collection-key", proposal.Credentials[0].APIKey)
	require.Equal(t, "synthetic-private-collection-environment", proposal.collectionSource.runtime.Getenv("PRIVATE_COLLECTION_ENV"))
	encoded, err := json.Marshal(proposal)
	require.NoError(t, err)
	without := proposal
	without.collectionSource = nil
	wireOnly, err := json.Marshal(without)
	require.NoError(t, err)
	require.Equal(t, wireOnly, encoded)
	require.NotContains(t, string(encoded), "collectionSource")
	require.NotContains(t, string(encoded), "collection_source")
	require.NotContains(t, string(encoded), "PRIVATE_COLLECTION_ENV")
	require.NotContains(t, string(encoded), "synthetic-private-collection-environment")
	digest, err := RemoteRuntimeDigest(without)
	require.NoError(t, err)
	require.Equal(t, proposal.Digest, digest)
	_, err = json.Marshal(proposal.collectionSource)
	require.ErrorContains(t, err, "runtime collection sources are private")
	for _, printed := range []string{fmt.Sprint(proposal.collectionSource), fmt.Sprintf("%#v", proposal.collectionSource), fmt.Sprintf("%+v", proposal.collectionSource), fmt.Sprintf("%q", proposal.collectionSource), fmt.Sprintf("%x", proposal.collectionSource)} {
		require.Equal(t, "[private runtime collection source]", printed)
	}
	decoded := cloneCollectionSourceTestProposal(t, proposal)
	require.Nil(t, decoded.CollectionConfig(), "JSON cannot create local collection provenance")
	require.NoError(t, decoded.RetainCollectionSource(proposal))
	require.Same(t, proposal.CollectionConfig(), decoded.CollectionConfig())
	require.True(t, proposal.collectionSource.runtime.SamePublication(decoded.collectionSource.runtime))
	require.Equal(t, calls, resolver.calls, "retaining or reading collection provenance must not resolve an expression again")

	old := proposal.CollectionConfig()
	store.setConfig(old.cloneForWrite())
	require.Same(t, old, decoded.CollectionConfig(), "later publication cannot replace the admitted collection source")
	require.NotSame(t, store.Config(), decoded.CollectionConfig())
}

func TestCollectionSourceRejectsChangedBindingsAndContents(t *testing.T) {
	_, source, resolver := collectionSourceTestProposal(t)
	calls := resolver.calls
	for _, name := range []string{"destination digest", "destination content", "source digest", "source proof digest", "source missing config"} {
		t.Run(name, func(t *testing.T) {
			candidate := cloneCollectionSourceTestProposal(t, source)
			origin := source
			switch name {
			case "destination digest":
				candidate.Digest = "changed"
			case "destination content":
				candidate.Credentials[0].APIKey = "substituted"
			case "source digest":
				origin.Digest = "changed"
			case "source proof digest":
				proof := *source.collectionSource
				proof.digest = "changed"
				origin.collectionSource = &proof
			case "source missing config":
				proof := *source.collectionSource
				proof.runtime = RuntimeSnapshot{}
				origin.collectionSource = &proof
			}
			require.Error(t, candidate.RetainCollectionSource(origin))
			require.Nil(t, candidate.CollectionConfig())
		})
	}
	changed := source
	changed.Digest = "changed"
	require.Nil(t, changed.CollectionConfig())
	require.Equal(t, calls, resolver.calls)
}

func TestCollectionSourceManualProposalCompatibility(t *testing.T) {
	manual := RemoteRuntimeProposal{Revision: 1, Digest: "manually-constructed"}
	require.Nil(t, manual.CollectionConfig())
	require.NoError(t, manual.RetainCollectionSource(RemoteRuntimeProposal{}))
	_, collected, _ := collectionSourceTestProposal(t)
	require.NotNil(t, collected.CollectionConfig())
	require.NoError(t, collected.RetainCollectionSource(manual))
	require.Nil(t, collected.CollectionConfig(), "source absence must not retain an unrelated previous proof")
	var missing *RemoteRuntimeProposal
	require.Error(t, missing.RetainCollectionSource(manual))
}
