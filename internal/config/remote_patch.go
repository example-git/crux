package config

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
)

type RemoteProviderDefinitionPut struct {
	Definition         RemoteProviderDefinition
	Bundle             *providerplugin.TransportBundle
	ContextInstruction *string
}

type RemoteProviderContextInstructionSet struct {
	Provider           providerregistry.RegistrationOwner
	ContextInstruction *string
}

type RemoteRuntimeTransaction struct {
	Advance                 bool
	DefinitionRemovals      []providerregistry.RegistrationOwner
	DefinitionPuts          []RemoteProviderDefinitionPut
	ContextInstructionSets  []RemoteProviderContextInstructionSet
	CredentialReplacements  []RemoteCredentialBinding
	CredentialInvalidations []providerregistry.RegistrationOwner
	ModelSelections         map[SelectedModelType]SelectedModel
	Controls                *RemoteRuntimeControls
}

func (s *ConfigStore) PatchRemoteRuntime(ctx context.Context, principal string, baseRevision uint64, baseDigest string, resultRevision uint64, resultDigest string, transaction RemoteRuntimeTransaction) (*RemoteAuthority, error) {
	proposal, err := s.remotePatchProposal(principal, baseRevision, baseDigest, resultRevision)
	if err != nil {
		return nil, err
	}
	ownerOf := s.RuntimeSnapshot().ProviderOwnerPrincipal
	changed := transaction.Advance
	removed := map[string]bool{}
	for _, owner := range transaction.DefinitionRemovals {
		if ownerOf(owner.ProviderID) != principal {
			return nil, errors.New("provider is owned by a different attached client and cannot be modified here")
		}
		if removed[owner.ProviderID] || !removeRemoteProvider(&proposal, owner) {
			return nil, errors.New("invalid provider definition removal")
		}
		removed[owner.ProviderID] = true
		delete(proposal.ProviderContextInstructions, owner.ProviderID)
		changed = true
	}
	put := map[string]bool{}
	for _, update := range transaction.DefinitionPuts {
		providerID := update.Definition.Config.ID
		if providerID == "" || put[providerID] || remoteProviderIndex(proposal.Providers, providerID) >= 0 {
			return nil, errors.New("invalid provider definition update")
		}
		if update.Bundle != nil {
			if update.Bundle.Digest == "" || update.Bundle.Digest != update.Definition.BundleDigest || remoteBundleIndex(proposal.Bundles, update.Bundle.Digest) >= 0 {
				return nil, errors.New("invalid provider bundle update")
			}
			proposal.Bundles = append(proposal.Bundles, *update.Bundle)
		} else if update.Definition.BundleDigest != "" && remoteBundleIndex(proposal.Bundles, update.Definition.BundleDigest) < 0 {
			return nil, errors.New("provider definition requires an unavailable bundle")
		}
		proposal.Providers = append(proposal.Providers, update.Definition)
		if update.ContextInstruction != nil {
			if proposal.ProviderContextInstructions == nil {
				proposal.ProviderContextInstructions = map[string]string{}
			}
			proposal.ProviderContextInstructions[providerID] = *update.ContextInstruction
		}
		put[providerID] = true
		changed = true
	}
	instructionSet := map[string]bool{}
	for _, set := range transaction.ContextInstructionSets {
		providerID := set.Provider.ProviderID
		if providerID == "" || removed[providerID] || put[providerID] || instructionSet[providerID] || remoteProviderIndex(proposal.Providers, providerID) < 0 {
			return nil, errors.New("invalid provider context instruction update")
		}
		if ownerOf(providerID) != principal {
			return nil, errors.New("provider is owned by a different attached client and cannot be modified here")
		}
		if set.ContextInstruction != nil {
			if proposal.ProviderContextInstructions == nil {
				proposal.ProviderContextInstructions = map[string]string{}
			}
			proposal.ProviderContextInstructions[providerID] = *set.ContextInstruction
		} else {
			delete(proposal.ProviderContextInstructions, providerID)
		}
		instructionSet[providerID] = true
		changed = true
	}
	credentials := map[string]bool{}
	for _, replacement := range transaction.CredentialReplacements {
		providerID := replacement.Owner.ProviderID
		if providerID == "" || credentials[providerID] || replacement.Generation != resultRevision || remoteProviderIndex(proposal.Providers, providerID) < 0 {
			return nil, errors.New("invalid provider credential replacement")
		}
		if ownerOf(providerID) != principal {
			return nil, errors.New("provider is owned by a different attached client and cannot be modified here")
		}
		replaceRemoteCredential(&proposal, replacement)
		credentials[providerID] = true
		changed = true
	}
	for _, owner := range transaction.CredentialInvalidations {
		providerID := owner.ProviderID
		if providerID == "" || credentials[providerID] || remoteProviderIndex(proposal.Providers, providerID) < 0 {
			return nil, errors.New("invalid provider credential invalidation")
		}
		if ownerOf(providerID) != principal {
			return nil, errors.New("provider is owned by a different attached client and cannot be modified here")
		}
		index := remoteCredentialIndex(proposal.Credentials, providerID)
		if index >= 0 && proposal.Credentials[index].Owner != owner {
			return nil, errors.New("provider credential invalidation owner changed")
		}
		replaceRemoteCredential(&proposal, RemoteCredentialBinding{Owner: owner, Generation: resultRevision, Unavailable: true})
		credentials[providerID] = true
		changed = true
	}
	if len(transaction.ModelSelections) > 0 {
		if len(transaction.ModelSelections) > 2 {
			return nil, errors.New("invalid selected model transaction")
		}
		if proposal.Models == nil {
			proposal.Models = map[SelectedModelType]SelectedModel{}
		}
		for modelType, selected := range transaction.ModelSelections {
			if modelType != SelectedModelTypeLarge && modelType != SelectedModelTypeSmall {
				return nil, errors.New("invalid selected model type")
			}
			proposal.Models[modelType] = cloneSelectedModel(selected)
		}
		changed = true
	}
	if transaction.Controls != nil {
		if _, err := transaction.Controls.options(); err != nil {
			return nil, err
		}
		proposal.Controls = *transaction.Controls
		changed = true
	}
	if !changed {
		return nil, errors.New("incremental runtime transaction has no mutation")
	}
	if err := pruneRemoteRuntimeBundles(&proposal); err != nil {
		return nil, err
	}
	slices.SortFunc(proposal.Providers, func(left, right RemoteProviderDefinition) int {
		return compareText(left.Config.ID, right.Config.ID)
	})
	slices.SortFunc(proposal.Credentials, func(left, right RemoteCredentialBinding) int {
		return compareText(left.Owner.ProviderID, right.Owner.ProviderID)
	})
	slices.SortFunc(proposal.Bundles, func(left, right providerplugin.TransportBundle) int {
		return compareText(left.Digest, right.Digest)
	})
	return s.commitRemotePatch(ctx, principal, baseRevision, resultDigest, proposal)
}

func (s *ConfigStore) PatchRemoteModelSelections(ctx context.Context, principal string, baseRevision uint64, baseDigest string, resultRevision uint64, resultDigest string, selections map[SelectedModelType]SelectedModel) (*RemoteAuthority, error) {
	proposal, err := s.remotePatchProposal(principal, baseRevision, baseDigest, resultRevision)
	if err != nil {
		return nil, err
	}
	if len(selections) == 0 || len(selections) > 2 {
		return nil, errors.New("invalid selected model transaction")
	}
	if proposal.Models == nil {
		proposal.Models = map[SelectedModelType]SelectedModel{}
	}
	for modelType, selected := range selections {
		if modelType != SelectedModelTypeLarge && modelType != SelectedModelTypeSmall {
			return nil, errors.New("invalid selected model type")
		}
		proposal.Models[modelType] = cloneSelectedModel(selected)
	}
	return s.commitRemotePatch(ctx, principal, baseRevision, resultDigest, proposal)
}

func (s *ConfigStore) PatchRemoteRuntimeControls(ctx context.Context, principal string, baseRevision uint64, baseDigest string, resultRevision uint64, resultDigest string, controls RemoteRuntimeControls) (*RemoteAuthority, error) {
	proposal, err := s.remotePatchProposal(principal, baseRevision, baseDigest, resultRevision)
	if err != nil {
		return nil, err
	}
	if _, err := controls.options(); err != nil {
		return nil, err
	}
	proposal.Controls = controls
	return s.commitRemotePatch(ctx, principal, baseRevision, resultDigest, proposal)
}

func (s *ConfigStore) remotePatchProposal(principal string, baseRevision uint64, baseDigest string, resultRevision uint64) (RemoteRuntimeProposal, error) {
	if err := s.RuntimeRevocation(); err != nil {
		return RemoteRuntimeProposal{}, err
	}
	snapshot := s.RuntimeSnapshot()
	authority := snapshot.RemoteAuthority()
	if authority == nil || authority.Principal != principal {
		return RemoteRuntimeProposal{}, errors.New("runtime patch requires its owning client principal")
	}
	if authority.Revision != baseRevision || authority.Digest != baseDigest || baseRevision == ^uint64(0) || resultRevision != baseRevision+1 {
		return RemoteRuntimeProposal{}, ErrRemoteRuntimeRevision
	}
	data, err := json.Marshal(snapshot.clientRuntime.proposal)
	if err != nil {
		return RemoteRuntimeProposal{}, errors.New("accepted runtime cannot be copied")
	}
	var proposal RemoteRuntimeProposal
	if err := json.Unmarshal(data, &proposal); err != nil {
		return RemoteRuntimeProposal{}, errors.New("accepted runtime cannot be copied")
	}
	proposal.Revision = resultRevision
	proposal.Digest = ""
	for index := range proposal.Credentials {
		proposal.Credentials[index].Generation = resultRevision
	}
	// ReplaceRemoteRuntime (which every patch ultimately commits through via
	// commitRemotePatch) treats its incoming proposal as the primary owner's
	// own contribution and re-merges any already-admitted secondary owners on
	// top of it. snapshot.clientRuntime.proposal is already the merged view,
	// so any secondary-owned provider must be stripped back out here first;
	// otherwise it would be merged in a second time and rejected as a
	// collision with itself.
	if owner := snapshot.clientRuntime.providerOwner; len(snapshot.clientRuntime.secondaryOwners) > 0 {
		proposal.Providers = slices.DeleteFunc(proposal.Providers, func(definition RemoteProviderDefinition) bool {
			return owner[definition.Config.ID] != "" && owner[definition.Config.ID] != principal
		})
		proposal.Credentials = slices.DeleteFunc(proposal.Credentials, func(binding RemoteCredentialBinding) bool {
			return owner[binding.Owner.ProviderID] != "" && owner[binding.Owner.ProviderID] != principal
		})
		if err := pruneRemoteRuntimeBundles(&proposal); err != nil {
			return RemoteRuntimeProposal{}, err
		}
	}
	return proposal, nil
}

func (s *ConfigStore) commitRemotePatch(ctx context.Context, principal string, baseRevision uint64, resultDigest string, proposal RemoteRuntimeProposal) (*RemoteAuthority, error) {
	digest, err := RemoteRuntimeDigest(proposal)
	if err != nil {
		return nil, err
	}
	if resultDigest == "" || digest != resultDigest {
		return nil, errors.New("runtime patch result digest mismatch")
	}
	proposal.Digest = digest
	return s.ReplaceRemoteRuntime(ctx, proposal, principal, baseRevision)
}

func removeRemoteProvider(proposal *RemoteRuntimeProposal, owner providerregistry.RegistrationOwner) bool {
	providerIndex := remoteProviderIndex(proposal.Providers, owner.ProviderID)
	credentialIndex := remoteCredentialIndex(proposal.Credentials, owner.ProviderID)
	if providerIndex < 0 || credentialIndex < 0 || proposal.Credentials[credentialIndex].Owner != owner {
		return false
	}
	proposal.Providers = slices.Delete(proposal.Providers, providerIndex, providerIndex+1)
	proposal.Credentials = slices.Delete(proposal.Credentials, credentialIndex, credentialIndex+1)
	return true
}

func replaceRemoteCredential(proposal *RemoteRuntimeProposal, replacement RemoteCredentialBinding) {
	index := remoteCredentialIndex(proposal.Credentials, replacement.Owner.ProviderID)
	if index < 0 {
		proposal.Credentials = append(proposal.Credentials, replacement)
		return
	}
	proposal.Credentials[index] = replacement
}

func pruneRemoteRuntimeBundles(proposal *RemoteRuntimeProposal) error {
	used := map[string]bool{}
	for _, definition := range proposal.Providers {
		if definition.BundleDigest != "" {
			used[definition.BundleDigest] = true
		}
	}
	kept := proposal.Bundles[:0]
	for _, transport := range proposal.Bundles {
		bundle, err := providerplugin.ValidateDetachedBundle(transport)
		if err != nil {
			return err
		}
		if used[transport.Digest] || bundle.Type() == manifest.PluginTypeImageProvider {
			kept = append(kept, transport)
		}
	}
	proposal.Bundles = kept
	return nil
}

func remoteProviderIndex(definitions []RemoteProviderDefinition, providerID string) int {
	return slices.IndexFunc(definitions, func(definition RemoteProviderDefinition) bool {
		return definition.Config.ID == providerID
	})
}

func remoteCredentialIndex(bindings []RemoteCredentialBinding, providerID string) int {
	return slices.IndexFunc(bindings, func(binding RemoteCredentialBinding) bool {
		return binding.Owner.ProviderID == providerID
	})
}

func remoteBundleIndex(bundles []providerplugin.TransportBundle, digest string) int {
	return slices.IndexFunc(bundles, func(bundle providerplugin.TransportBundle) bool {
		return bundle.Digest == digest
	})
}

func compareText(left, right string) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}
