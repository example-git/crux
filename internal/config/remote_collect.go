package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerregistry"
)

func cloneTransportBundles(values map[string]providerplugin.TransportBundle) map[string]providerplugin.TransportBundle {
	result := make(map[string]providerplugin.TransportBundle, len(values))
	for digest, bundle := range values {
		bundle.Files = slices.Clone(bundle.Files)
		for i := range bundle.Files {
			bundle.Files[i].Data = slices.Clone(bundle.Files[i].Data)
		}
		result[digest] = bundle
	}
	return result
}

// CollectRemoteRuntime captures one accepted configuration generation. Accounts
// are read only for selected owners and must match that generation's token.
// It never reopens a bundle path or substitutes another provider/account.
func (s *ConfigStore) CollectRemoteRuntime(ctx context.Context, revision uint64) (RemoteRuntimeProposal, error) {
	return s.CollectRemoteRuntimeWithUnavailable(ctx, revision, nil)
}

// CollectRemoteRuntimeWithUnavailable records intentional client logout without
// keeping the receiver's old secret or choosing a different provider.
func (s *ConfigStore) CollectRemoteRuntimeWithUnavailable(ctx context.Context, revision uint64, removed map[providerregistry.RegistrationOwner]bool) (RemoteRuntimeProposal, error) {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	snapshot := s.runtimeSnapshotLocked(s.config, s.resolver, s.providerRegistry, s.effectiveEnvironment)
	if snapshot.IsClientOwned() {
		return RemoteRuntimeProposal{}, errors.New("collect runtime on its owning client")
	}
	cfg := snapshot.Config()
	proposal := RemoteRuntimeProposal{Version: RemoteRuntimeVersion, Revision: revision, Models: maps.Clone(cfg.Models), Images: cloneImageConfiguration(cfg.Images), Controls: remoteControlsFromOptions(cfg.Options)}
	var err error
	proposal.ProviderContextInstructions, err = collectProviderContextInstructions(ctx, snapshot, proposal.Models)
	if err != nil {
		return proposal, err
	}
	selected := make(map[string]bool)
	for _, model := range proposal.Models {
		selected[model.Provider] = true
	}
	wantedBundles := make(map[string]bool)
	if proposal.Images != nil {
		for _, owner := range proposal.Images.Preferred {
			wantedBundles[owner.Digest] = true
		}
		for _, image := range proposal.Images.Providers {
			wantedBundles[image.Owner.Digest] = true
			for _, owner := range image.Credentials {
				selected[owner.ProviderID] = true
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(selected)) {
		provider, ok := cfg.Providers.Get(id)
		if !ok {
			return proposal, fmt.Errorf("selected client provider %q is unavailable", id)
		}
		definition, owner, err := snapshot.ClientProviderDefinition(id)
		if err != nil {
			return proposal, err
		}
		if definition.BundleDigest != "" {
			wantedBundles[definition.BundleDigest] = true
		}
		credential := RemoteCredentialBinding{Owner: owner, Generation: revision, Unavailable: removed[owner] || provider.Disable}
		key, err := snapshot.Resolve(provider.APIKey)
		if err != nil {
			return proposal, errors.New("selected client API credential cannot be resolved")
		}
		credential.APIKey = key
		if !credential.Unavailable && (provider.OAuthToken != nil || owner.HasOAuth) {
			entry, err := accounts.Active(ctx, owner.AccountNamespace)
			if err != nil {
				return proposal, errors.New("selected client account cannot be read")
			}
			selectedAccess := key
			if provider.OAuthToken != nil {
				selectedAccess = provider.OAuthToken.AccessToken
			}
			if entry == nil || entry.ID == "" || entry.AccessToken != selectedAccess || provider.OAuthToken != nil && entry.RefreshToken != provider.OAuthToken.RefreshToken {
				return proposal, errors.New("selected client account changed; reload client configuration before reconnecting")
			}
			credential.Account, credential.APIKey = entry, ""
		}
		if credential.Unavailable {
			credential.APIKey = ""
			credential.Account = nil
		}
		proposal.Providers = append(proposal.Providers, definition)
		proposal.Credentials = append(proposal.Credentials, credential)
	}
	for _, digest := range slices.Sorted(maps.Keys(wantedBundles)) {
		if cfg.providerScan == nil {
			return proposal, errors.New("client bundle scan is unavailable")
		}
		bundle, ok := cfg.providerScan.bundles[digest]
		if !ok {
			return proposal, errors.New("selected bundle was not captured in the accepted client generation; reload client configuration")
		}
		detached, err := providerplugin.ValidateDetachedBundle(bundle)
		if err != nil {
			return proposal, err
		}
		if image := detached.Image(); image != nil {
			for _, credential := range image.Credentials {
				if credential.Source != "environment" {
					continue
				}
				value := snapshot.Getenv(credential.Environment)
				if value == "" {
					return proposal, errors.New("selected client image environment credential is missing")
				}
				if proposal.CredentialEnvironment == nil {
					proposal.CredentialEnvironment = map[string]string{}
				}
				proposal.CredentialEnvironment[credential.Environment] = value
			}
		}
		proposal.Bundles = append(proposal.Bundles, bundle)
	}
	// The result owns its bytes, maps and account metadata independently of the
	// captured local generation, including caller-side edits for later revisions.
	data, err := json.Marshal(proposal)
	if err != nil {
		return proposal, errors.New("client runtime cannot be encoded")
	}
	var result RemoteRuntimeProposal
	if err := json.Unmarshal(data, &result); err != nil {
		return result, errors.New("client runtime cannot be copied")
	}
	result.Digest, err = RemoteRuntimeDigest(result)
	return result, err
}
