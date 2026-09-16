package config

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/example-git/crux/internal/providerplugin"
)

// mergeClientRuntimeProposals builds the union of a primary proposal and
// every currently accepted secondary owner's own contribution, in
// principal-sorted order for determinism. Providers/credentials/bundles are
// concatenated; every other aggregate field (Models, Controls, Images,
// CodebaseIndex, ...) is taken from the primary proposal only, since only
// the workspace's primary owner controls those workspace-wide settings. A
// provider ID that appears in more than one contribution is rejected rather
// than merged or overwritten, preserving each owner's exclusive contribution.
func mergeClientRuntimeProposals(primary RemoteRuntimeProposal, secondaries map[string]*clientRuntimeOwner) (RemoteRuntimeProposal, error) {
	merged := primary
	if len(secondaries) == 0 {
		return merged, nil
	}
	providers := append([]RemoteProviderDefinition{}, primary.Providers...)
	credentials := append([]RemoteCredentialBinding{}, primary.Credentials...)
	bundles := append([]providerplugin.TransportBundle{}, primary.Bundles...)
	claimed := map[string]bool{}
	for _, definition := range primary.Providers {
		claimed[definition.Config.ID] = true
	}
	for _, principal := range sortedOwnerPrincipals(secondaries) {
		owner := secondaries[principal]
		for _, definition := range owner.proposal.Providers {
			if claimed[definition.Config.ID] {
				return RemoteRuntimeProposal{}, fmt.Errorf("secondary client provider %q collides with the primary owner or another secondary owner", definition.Config.ID)
			}
			claimed[definition.Config.ID] = true
		}
		providers = append(providers, owner.proposal.Providers...)
		credentials = append(credentials, owner.proposal.Credentials...)
		bundles = append(bundles, owner.proposal.Bundles...)
	}
	merged.Providers = providers
	merged.Credentials = credentials
	merged.Bundles = bundles
	return merged, nil
}

// computeProviderOwners rebuilds the providerID -> owning-principal index
// from scratch. Used after a primary ReplaceRemoteRuntime, whose new
// proposal may have added or removed primary-owned providers, while every
// secondary owner's own contribution stays exactly as previously accepted.
func computeProviderOwners(primaryPrincipal string, primaryProviders []RemoteProviderDefinition, secondaries map[string]*clientRuntimeOwner) map[string]string {
	owners := make(map[string]string, len(primaryProviders))
	for _, definition := range primaryProviders {
		owners[definition.Config.ID] = primaryPrincipal
	}
	for principal, owner := range secondaries {
		for _, definition := range owner.proposal.Providers {
			owners[definition.Config.ID] = principal
		}
	}
	return owners
}

func sortedOwnerPrincipals(owners map[string]*clientRuntimeOwner) []string {
	keys := make([]string, 0, len(owners))
	for principal := range owners {
		keys = append(keys, principal)
	}
	slices.Sort(keys)
	return keys
}

// ProviderOwnerPrincipal reports which principal contributed the given
// provider ID to a client-authority workspace. It always returns a definite
// answer: a provider absent from the secondary-owner index belongs to the
// workspace's primary owner (RemoteAuthority.Principal). Callers use this to
// lock usage of an asymmetrically contributed provider/model to the
// connection that contributed it; a second attached principal's connection
// may see the provider (so its name is visible workspace-wide) but must not
// be able to select or drive it.
func (s RuntimeSnapshot) ProviderOwnerPrincipal(providerID string) string {
	if s.clientRuntime == nil {
		return ""
	}
	if owner, ok := s.clientRuntime.providerOwner[providerID]; ok {
		return owner
	}
	return s.clientRuntime.authority.Principal
}

// AdmitSecondaryClientAuthority merges an additional, distinct principal's
// own disjoint provider/model manifest into an already-accepted
// client-authority workspace, without disturbing the workspace's primary
// owner or its accepted providers. The additional principal's provider IDs
// must be entirely disjoint from the primary owner's and from every other
// already-admitted secondary owner's; a colliding ID is rejected rather than
// merged or overwritten.
//
// Usage of a merged-in provider remains locked to its contributing
// principal (see ProviderOwnerPrincipal); session messages and responses
// produced through it are still relayed to every attached connection via
// the existing workspace event broadcast, which this call does not alter.
//
// Not yet supported (explicitly out of scope for this landing): a secondary
// owner independently calling ReplaceRemoteRuntime/PatchRemoteRuntime* to
// update or withdraw its own contribution after admission, and correctly
// routing an OAuth refresh request for a secondary-owned provider to that
// secondary's own connection rather than the primary owner's. Secondary
// owners should prefer credentials that do not require server-initiated
// refresh until a follow-up change extends refresh routing.
func (s *ConfigStore) AdmitSecondaryClientAuthority(ctx context.Context, principal string, proposal RemoteRuntimeProposal) (*RemoteAuthority, error) {
	if err := s.RuntimeRevocation(); err != nil {
		return nil, err
	}
	if len(principal) != 64 {
		return nil, errors.New("secondary client authority requires a verified principal")
	}
	if _, err := hex.DecodeString(principal); err != nil {
		return nil, errors.New("invalid secondary client principal")
	}

	s.writeMu.RLock()
	if s.clientRuntime == nil || s.clientRuntime.local {
		s.writeMu.RUnlock()
		return nil, errors.New("secondary client authority requires an existing remote client-authority workspace")
	}
	if s.clientRuntime.authority.Principal == principal {
		s.writeMu.RUnlock()
		return nil, errors.New("the workspace's primary owner cannot also be admitted as a secondary owner")
	}
	if _, exists := s.clientRuntime.secondaryOwners[principal]; exists {
		s.writeMu.RUnlock()
		return nil, errors.New("this principal has already contributed a runtime to this workspace")
	}
	workingDir, dataDir, debug := s.workingDir, s.config.Options.DataDirectory, s.config.Options.Debug
	baseEnvironment := cloneEnvironment(s.baseEnvironment)
	primaryPrincipal := s.clientRuntime.authority.Principal
	baseRevision, baseDigest := s.clientRuntime.authority.Revision, s.clientRuntime.authority.Digest
	existingSecondaries := s.clientRuntime.secondaryOwners
	s.writeMu.RUnlock()

	// Validate the contributed manifest compiles as a legitimate,
	// self-consistent client runtime on its own before merging any of it in.
	if _, err := compileClientRuntime(workingDir, dataDir, debug, proposal, principal, baseEnvironment, false); err != nil {
		return nil, fmt.Errorf("secondary client runtime is invalid: %w", err)
	}

	mergedSecondaries := make(map[string]*clientRuntimeOwner, len(existingSecondaries)+1)
	maps.Copy(mergedSecondaries, existingSecondaries)
	mergedSecondaries[principal] = &clientRuntimeOwner{principal: principal, proposal: proposal}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.RuntimeRevocation(); err != nil {
		return nil, err
	}
	if s.clientRuntime == nil || s.clientRuntime.authority.Revision != baseRevision || s.clientRuntime.authority.Digest != baseDigest {
		return nil, ErrRemoteRuntimeRevision
	}
	if _, exists := s.clientRuntime.secondaryOwners[principal]; exists {
		return nil, errors.New("this principal has already contributed a runtime to this workspace")
	}

	merged, err := mergeClientRuntimeProposals(s.clientRuntime.proposal, mergedSecondaries)
	if err != nil {
		return nil, err
	}
	merged.Revision = baseRevision + 1
	merged.Digest = ""
	digest, err := RemoteRuntimeDigest(merged)
	if err != nil {
		return nil, err
	}
	merged.Digest = digest

	candidate, err := compileClientRuntime(workingDir, dataDir, debug, merged, primaryPrincipal, baseEnvironment, false)
	if err != nil {
		return nil, fmt.Errorf("merged client runtime is invalid: %w", err)
	}
	candidate.runtimeParent = s

	next := s.Config().cloneForWrite()
	next.Providers = candidate.config.Providers
	next.Models = candidate.config.Models
	next.Images = candidate.config.Images
	next.Tools.CodebaseSearch = cloneCodebaseSettings(candidate.config.Tools.CodebaseSearch)
	next.bindProviderScan(*candidate.config.providerScan)
	next.captureExplicitModels()
	next.SetupAgents()
	candidate.setConfig(next)

	s.configMu.Lock()
	previous := s.runtimeSnapshotLocked(s.config, s.resolver, s.providerRegistry, s.effectiveEnvironment)
	s.configMu.Unlock()
	candidate.clientRuntime.withdrawnAt = clientProviderWithdrawals(previous, candidate.RuntimeSnapshot())

	runtimeCandidate, err := s.prepareRuntimeGeneration(ctx, candidate.RuntimeSnapshot())
	if err != nil {
		return nil, fmt.Errorf("prepare secondary client runtime merge: %w", err)
	}
	committed := false
	defer func() {
		if !committed && runtimeCandidate.Abort != nil {
			runtimeCandidate.Abort()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	registerConfigSecrets(next)
	for _, account := range candidate.ephemeralAccounts {
		registerAccountSecrets(account.Entry)
	}

	newProviderOwner := make(map[string]string, len(s.clientRuntime.providerOwner)+len(proposal.Providers))
	maps.Copy(newProviderOwner, s.clientRuntime.providerOwner)
	for _, definition := range proposal.Providers {
		newProviderOwner[definition.Config.ID] = principal
	}
	candidate.clientRuntime.secondaryOwners = mergedSecondaries
	candidate.clientRuntime.providerOwner = newProviderOwner

	s.configMu.Lock()
	if err := s.RuntimeRevocation(); err != nil {
		s.configMu.Unlock()
		return nil, err
	}
	s.publishConfigLocked(next)
	s.providerRegistry = candidate.providerRegistry
	s.knownProviders = candidate.knownProviders
	s.ephemeralAccounts = candidate.ephemeralAccounts
	s.clientRuntime = candidate.clientRuntime
	s.effectiveEnvironment = candidate.effectiveEnvironment
	s.resolver = candidate.resolver
	s.configMu.Unlock()
	if runtimeCandidate.Commit != nil {
		runtimeCandidate.Commit()
	}
	committed = true
	authority := candidate.clientRuntime.authority
	return &authority, nil
}

// PatchOwnedModelSelections selects one or two (Large/Small) model slots on
// behalf of principal, honoring the asymmetric multi-owner usage lock: a
// contributor may select only a provider it exclusively owns (see
// ProviderOwnerPrincipal). When principal is the workspace's primary owner
// this delegates to PatchRemoteModelSelections unchanged. When principal is
// a distinct, already-admitted secondary owner (see
// AdmitSecondaryClientAuthority), every entry in selections must reference a
// provider owned by that exact principal; the call then republishes the
// shared, workspace-wide Large/Small selection without otherwise disturbing
// any owner's accepted provider/credential contributions. This is the one
// deliberate exception to secondary owners otherwise being unable to call
// ReplaceRemoteRuntime/PatchRemoteRuntime*: a contributor must be able to put
// its own model into active use, even though it still cannot add, remove, or
// modify any provider definition (that remains primary-only).
func (s *ConfigStore) PatchOwnedModelSelections(ctx context.Context, principal string, baseRevision uint64, baseDigest string, resultRevision uint64, resultDigest string, selections map[SelectedModelType]SelectedModel) (*RemoteAuthority, error) {
	if len(selections) == 0 || len(selections) > 2 {
		return nil, errors.New("invalid selected model transaction")
	}
	if err := s.RuntimeRevocation(); err != nil {
		return nil, err
	}
	snapshot := s.RuntimeSnapshot()
	authority := snapshot.RemoteAuthority()
	if authority == nil {
		return nil, errors.New("accepted client runtime is unavailable")
	}
	if authority.Principal == principal {
		return s.PatchRemoteModelSelections(ctx, principal, baseRevision, baseDigest, resultRevision, resultDigest, selections)
	}
	for modelType, selected := range selections {
		if modelType != SelectedModelTypeLarge && modelType != SelectedModelTypeSmall {
			return nil, errors.New("invalid selected model type")
		}
		if snapshot.ProviderOwnerPrincipal(selected.Provider) != principal {
			return nil, errors.New("provider is owned by a different attached client and cannot be selected here")
		}
	}

	s.writeMu.RLock()
	if s.clientRuntime == nil {
		s.writeMu.RUnlock()
		return nil, errors.New("secondary model selection requires an existing client-authority workspace")
	}
	if _, ok := s.clientRuntime.secondaryOwners[principal]; !ok {
		s.writeMu.RUnlock()
		return nil, errors.New("this principal has not contributed a runtime to this workspace")
	}
	primaryPrincipal := s.clientRuntime.authority.Principal
	workingDir, dataDir, debug := s.workingDir, s.config.Options.DataDirectory, s.config.Options.Debug
	baseEnvironment := cloneEnvironment(s.baseEnvironment)
	secondaryOwners := s.clientRuntime.secondaryOwners
	providerOwner := s.clientRuntime.providerOwner
	s.writeMu.RUnlock()

	if authority.Revision != baseRevision || authority.Digest != baseDigest || baseRevision == ^uint64(0) || resultRevision != baseRevision+1 {
		return nil, ErrRemoteRuntimeRevision
	}

	data, err := json.Marshal(snapshot.clientRuntime.proposal)
	if err != nil {
		return nil, errors.New("accepted runtime cannot be copied")
	}
	var merged RemoteRuntimeProposal
	if err := json.Unmarshal(data, &merged); err != nil {
		return nil, errors.New("accepted runtime cannot be copied")
	}
	if merged.Models == nil {
		merged.Models = map[SelectedModelType]SelectedModel{}
	}
	for modelType, selected := range selections {
		merged.Models[modelType] = cloneSelectedModel(selected)
	}
	// Mirror remotePatchProposal's credential-generation bump exactly so this
	// path and the primary-owner path compute an identical result digest for
	// the same logical model-selection change, regardless of which admitted
	// owner submits it.
	for index := range merged.Credentials {
		merged.Credentials[index].Generation = resultRevision
	}
	merged.Revision = resultRevision
	merged.Digest = ""
	digest, err := RemoteRuntimeDigest(merged)
	if err != nil {
		return nil, err
	}
	if resultDigest == "" || digest != resultDigest {
		return nil, errors.New("runtime patch result digest mismatch")
	}
	merged.Digest = digest

	candidate, err := compileClientRuntime(workingDir, dataDir, debug, merged, primaryPrincipal, baseEnvironment, false)
	if err != nil {
		return nil, fmt.Errorf("secondary model selection is invalid: %w", err)
	}
	candidate.clientRuntime.secondaryOwners = secondaryOwners
	candidate.clientRuntime.providerOwner = providerOwner
	candidate.runtimeParent = s

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.RuntimeRevocation(); err != nil {
		return nil, err
	}
	if s.clientRuntime == nil || s.clientRuntime.authority.Revision != baseRevision || s.clientRuntime.authority.Digest != baseDigest {
		return nil, ErrRemoteRuntimeRevision
	}
	if _, ok := s.clientRuntime.secondaryOwners[principal]; !ok {
		return nil, errors.New("this principal has not contributed a runtime to this workspace")
	}

	next := s.Config().cloneForWrite()
	next.Providers = candidate.config.Providers
	next.Models = candidate.config.Models
	next.Images = candidate.config.Images
	next.Tools.CodebaseSearch = cloneCodebaseSettings(candidate.config.Tools.CodebaseSearch)
	next.bindProviderScan(*candidate.config.providerScan)
	next.captureExplicitModels()
	next.SetupAgents()
	candidate.setConfig(next)

	s.configMu.Lock()
	previous := s.runtimeSnapshotLocked(s.config, s.resolver, s.providerRegistry, s.effectiveEnvironment)
	s.configMu.Unlock()
	candidate.clientRuntime.withdrawnAt = clientProviderWithdrawals(previous, candidate.RuntimeSnapshot())

	runtimeCandidate, err := s.prepareRuntimeGeneration(ctx, candidate.RuntimeSnapshot())
	if err != nil {
		return nil, fmt.Errorf("prepare secondary model selection: %w", err)
	}
	committed := false
	defer func() {
		if !committed && runtimeCandidate.Abort != nil {
			runtimeCandidate.Abort()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	registerConfigSecrets(next)
	for _, account := range candidate.ephemeralAccounts {
		registerAccountSecrets(account.Entry)
	}

	s.configMu.Lock()
	if err := s.RuntimeRevocation(); err != nil {
		s.configMu.Unlock()
		return nil, err
	}
	s.publishConfigLocked(next)
	s.providerRegistry = candidate.providerRegistry
	s.knownProviders = candidate.knownProviders
	s.ephemeralAccounts = candidate.ephemeralAccounts
	s.clientRuntime = candidate.clientRuntime
	s.effectiveEnvironment = candidate.effectiveEnvironment
	s.resolver = candidate.resolver
	s.configMu.Unlock()
	if runtimeCandidate.Commit != nil {
		runtimeCandidate.Commit()
	}
	committed = true
	resultAuthority := candidate.clientRuntime.authority
	return &resultAuthority, nil
}

// PreviewModelSelectionResultDigest computes the exact ResultDigest that a
// subsequent PatchOwnedModelSelections call would require for the given
// principal, resultRevision, and selections, without publishing anything.
// This lets a caller (for example a peer-channel request builder, or a
// test) assemble a valid PeerModelSelectionSet request up front. It mirrors
// both code paths exactly: for the workspace's primary owner, it replicates
// remotePatchProposal's owner-filtering (stripping any secondary-owned
// provider back out of the merged view before recomputing the digest); for
// an already-admitted secondary owner, it replicates
// PatchOwnedModelSelections's unfiltered merge. Both branches apply the same
// credential-generation bump so the two owners' selections are digest
// compatible with the exact production commit path regardless of who
// submits them.
func (s *ConfigStore) PreviewModelSelectionResultDigest(principal string, resultRevision uint64, selections map[SelectedModelType]SelectedModel) (string, error) {
	if len(selections) == 0 || len(selections) > 2 {
		return "", errors.New("invalid selected model transaction")
	}
	for modelType := range selections {
		if modelType != SelectedModelTypeLarge && modelType != SelectedModelTypeSmall {
			return "", errors.New("invalid selected model type")
		}
	}
	snapshot := s.RuntimeSnapshot()
	authority := snapshot.RemoteAuthority()
	if authority == nil {
		return "", errors.New("accepted client runtime is unavailable")
	}
	isPrimary := authority.Principal == principal
	if !isPrimary {
		for _, selected := range selections {
			if snapshot.ProviderOwnerPrincipal(selected.Provider) != principal {
				return "", errors.New("provider is owned by a different attached client and cannot be selected here")
			}
		}
	}

	data, err := json.Marshal(snapshot.clientRuntime.proposal)
	if err != nil {
		return "", errors.New("accepted runtime cannot be copied")
	}
	var merged RemoteRuntimeProposal
	if err := json.Unmarshal(data, &merged); err != nil {
		return "", errors.New("accepted runtime cannot be copied")
	}
	if isPrimary {
		if owner := snapshot.clientRuntime.providerOwner; len(snapshot.clientRuntime.secondaryOwners) > 0 {
			merged.Providers = slices.DeleteFunc(merged.Providers, func(definition RemoteProviderDefinition) bool {
				return owner[definition.Config.ID] != "" && owner[definition.Config.ID] != principal
			})
			merged.Credentials = slices.DeleteFunc(merged.Credentials, func(binding RemoteCredentialBinding) bool {
				return owner[binding.Owner.ProviderID] != "" && owner[binding.Owner.ProviderID] != principal
			})
			if err := pruneRemoteRuntimeBundles(&merged); err != nil {
				return "", err
			}
		}
	}
	if merged.Models == nil {
		merged.Models = map[SelectedModelType]SelectedModel{}
	}
	for modelType, selected := range selections {
		merged.Models[modelType] = cloneSelectedModel(selected)
	}
	for index := range merged.Credentials {
		merged.Credentials[index].Generation = resultRevision
	}
	merged.Revision = resultRevision
	merged.Digest = ""
	return RemoteRuntimeDigest(merged)
}
