package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/example-git/crux/internal/config"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerplugin"
)

// NegotiateRemoteRuntime is authenticated and contains no private state. It is
// deliberately called before constructing a secret-bearing request body.
func (c *Client) NegotiateRemoteRuntime(ctx context.Context) (*proto.RemoteRuntimeCapabilities, error) {
	if !c.secure && !c.localRuntimeTransport() {
		return nil, errors.New("client runtime requires a local transport or saved authenticated TLS connection")
	}
	rsp, err := c.get(ctx, "/runtime-capabilities", nil, nil)
	if err != nil {
		return nil, err
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		switch rsp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, fmt.Errorf("remote runtime authorization failed (GET /v1/runtime-capabilities); verify the saved connection is still authorized: %w", err)
		case http.StatusNotFound, http.StatusMethodNotAllowed:
			return nil, fmt.Errorf("remote runtime capability endpoint is unavailable (GET /v1/runtime-capabilities): %w", err)
		default:
			return nil, fmt.Errorf("remote runtime capability negotiation failed (GET /v1/runtime-capabilities): %w", err)
		}
	}
	var value proto.RemoteRuntimeCapabilities
	if err := json.NewDecoder(io.LimitReader(rsp.Body, 64<<10)).Decode(&value); err != nil {
		return nil, errors.New("invalid remote runtime capabilities")
	}
	sharingValid := value.WorkspaceSharing == proto.RemoteRuntimeLocalSharing && value.Principal == ""
	if c.secure {
		sharingValid = value.WorkspaceSharing == proto.RemoteRuntimeCertificateSharing && len(value.Principal) == 64
	}
	if value.Protocol != proto.RemoteRuntimeProtocol || value.PeerChannel != proto.PeerChannelProtocol || !value.IncrementalState || value.RuntimeVersion != config.RemoteRuntimeVersion || value.Compiler != config.RemoteRuntimeCompiler || !sharingValid || value.MaxRequestBytes <= 0 || value.MaxBundles <= 0 || value.MaxProviders <= 0 || value.DisconnectGraceMillis < 0 {
		return nil, errors.New("remote runtime capabilities are incompatible with peer-channel incremental synchronization; no private state was sent")
	}
	return &value, nil
}

func validateRemoteRuntimeCapabilities(value *proto.RemoteRuntimeCapabilities, proposal *config.RemoteRuntimeProposal) error {
	if proposal == nil {
		return errors.New("client runtime proposal is required")
	}
	if proposal.CodebaseIndex != nil && !value.CodebaseIndex {
		return errors.New("remote server does not advertise client-owned codebase indexing support")
	}
	if len(proposal.Bundles) > value.MaxBundles || len(proposal.Providers) > value.MaxProviders {
		return errors.New("client runtime exceeds remote receiver limits")
	}
	for _, received := range proposal.Bundles {
		bundle, err := providerplugin.ValidateDetachedBundle(received)
		if err != nil {
			return err
		}
		if err := bundle.ValidateHostVersion(value.HostVersion); err != nil {
			return err
		}
	}
	return nil
}

func runtimeHeaders() http.Header {
	return http.Header{"Content-Type": {"application/json"}, "Crux-Runtime-Protocol": {proto.RemoteRuntimeProtocol}, cruxlog.EphemeralStateHeader: {"1"}}
}

func (c *Client) PatchRemoteRuntime(ctx context.Context, id string, previous, next config.RemoteRuntimeProposal, authentication ...proto.PeerProviderAuthentication) (*config.RemoteAuthority, error) {
	if next.Revision != previous.Revision+1 || previous.Digest == "" || next.Digest == "" {
		return nil, errors.New("incremental runtime transaction has an invalid authority revision")
	}
	if digest, err := config.RemoteRuntimeDigest(next); err != nil || digest != next.Digest {
		return nil, errors.New("incremental runtime transaction has an invalid result digest")
	}
	base := proto.PeerRuntimeBase{ExpectedRevision: previous.Revision, ExpectedDigest: previous.Digest, ResultRevision: next.Revision, ResultDigest: next.Digest}
	changedModels := make([]config.SelectedModelType, 0, 2)
	for _, modelType := range []config.SelectedModelType{config.SelectedModelTypeLarge, config.SelectedModelTypeSmall} {
		if !reflect.DeepEqual(previous.Models[modelType], next.Models[modelType]) {
			changedModels = append(changedModels, modelType)
		}
	}
	controlsChanged := !reflect.DeepEqual(previous.Controls, next.Controls)
	beforeShape, afterShape := runtimePatchShape(previous), runtimePatchShape(next)
	providerStateChanged := !remoteRuntimeWireEqual(beforeShape, afterShape)
	var messageType proto.PeerMessageType
	var payload any
	if !providerStateChanged && len(changedModels) > 0 && !controlsChanged {
		selections, err := peerModelSelections(next, changedModels)
		if err != nil {
			return nil, err
		}
		messageType = proto.PeerTypeModelSelectionSet
		payload = proto.PeerModelSelectionSet{Runtime: base, Selections: selections}
	} else if !providerStateChanged && len(changedModels) == 0 && controlsChanged {
		messageType = proto.PeerTypeRuntimeControlsPatch
		payload = proto.PeerRuntimeControlsPatch{Runtime: base, Controls: next.Controls}
	} else {
		transaction, _, err := buildPeerRuntimeTransaction(base, previous, next, changedModels, controlsChanged, authentication)
		if err != nil {
			return nil, err
		}
		messageType = proto.PeerTypeRuntimeTransaction
		payload = transaction
	}
	capabilities, err := c.NegotiateRemoteRuntime(ctx)
	if err != nil {
		return nil, err
	}
	messageID := uuid.NewString()
	var commandErr error
	for attempt := 0; attempt < 3; attempt++ {
		current, stateErr := c.peerWorkspaceAuthority(ctx, id, capabilities.Principal)
		if stateErr != nil {
			return nil, stateErr
		}
		if remoteAuthorityMismatch(current, capabilities.Principal, &next) == "" {
			c.retainWorkspaceAttachment(id, current)
			return current, nil
		}
		if !authorityMatchesAttachment(current, capabilities.Principal, proto.WorkspaceAttachment{Mode: "client", Revision: previous.Revision, Digest: previous.Digest}) {
			return nil, fmt.Errorf("%w: receiver does not retain the expected incremental runtime base", errRemoteAuthorityMismatch)
		}
		channel, sendErr := c.getWorkspaceChannel(ctx, id)
		if sendErr == nil {
			var acknowledgement proto.PeerAcknowledgement
			acknowledgement, sendErr = channel.peer.commandWithID(ctx, id, messageID, messageType, payload)
			if sendErr == nil {
				ack := acknowledgement.Authority
				if mismatch := remoteAuthorityMismatch(ack, capabilities.Principal, &next); mismatch != "" {
					return nil, fmt.Errorf("%w: %s", errRemoteAuthorityMismatch, mismatch)
				}
				c.retainWorkspaceAttachment(id, ack)
				return ack, nil
			}
		}
		commandErr = sendErr
		if workspaceChannelCommandRejected(sendErr) || ctx.Err() != nil {
			return nil, sendErr
		}
		if channel != nil {
			channel.peer.close(errors.New("peer channel reconnect required to reconcile an unacknowledged runtime transaction"))
		}
		if attempt < 2 {
			delay := 50 * time.Millisecond << attempt
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, commandErr
}

func remoteRuntimeWireEqual(left, right config.RemoteRuntimeProposal) bool {
	leftData, leftErr := json.Marshal(left)
	rightData, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftData) == string(rightData)
}

func runtimePatchShape(proposal config.RemoteRuntimeProposal) config.RemoteRuntimeProposal {
	data, err := json.Marshal(proposal)
	if err != nil {
		return config.RemoteRuntimeProposal{}
	}
	var shape config.RemoteRuntimeProposal
	if err := json.Unmarshal(data, &shape); err != nil {
		return config.RemoteRuntimeProposal{}
	}
	shape.Revision = 0
	shape.Digest = ""
	shape.Models = nil
	shape.Controls = config.RemoteRuntimeControls{}
	for index := range shape.Credentials {
		shape.Credentials[index].Generation = 0
	}
	return shape
}

func peerModelSelections(proposal config.RemoteRuntimeProposal, modelTypes []config.SelectedModelType) ([]proto.PeerModelSelectionOperation, error) {
	selections := make([]proto.PeerModelSelectionOperation, 0, len(modelTypes))
	for _, modelType := range modelTypes {
		selected := proposal.Models[modelType]
		provider, err := proto.ProviderRefForRuntime(proposal, selected.Provider)
		if err != nil {
			return nil, err
		}
		selections = append(selections, proto.PeerModelSelectionOperation{ModelType: modelType, Model: proto.ModelRef{Provider: provider, ModelID: selected.Model}, Settings: selected})
	}
	return selections, nil
}

func buildPeerRuntimeTransaction(base proto.PeerRuntimeBase, previous, next config.RemoteRuntimeProposal, changedModels []config.SelectedModelType, controlsChanged bool, authentication []proto.PeerProviderAuthentication) (proto.PeerRuntimeTransaction, bool, error) {
	beforeUnsupported, afterUnsupported := previous, next
	beforeUnsupported.Revision, beforeUnsupported.Digest, beforeUnsupported.Bundles, beforeUnsupported.Providers, beforeUnsupported.Credentials, beforeUnsupported.Models, beforeUnsupported.Controls, beforeUnsupported.ProviderContextInstructions = 0, "", nil, nil, nil, nil, config.RemoteRuntimeControls{}, nil
	afterUnsupported.Revision, afterUnsupported.Digest, afterUnsupported.Bundles, afterUnsupported.Providers, afterUnsupported.Credentials, afterUnsupported.Models, afterUnsupported.Controls, afterUnsupported.ProviderContextInstructions = 0, "", nil, nil, nil, nil, config.RemoteRuntimeControls{}, nil
	if !remoteRuntimeWireEqual(beforeUnsupported, afterUnsupported) {
		var fields []string
		for _, field := range []struct {
			name    string
			changed bool
		}{
			{name: "codebase index", changed: !reflect.DeepEqual(beforeUnsupported.CodebaseIndex, afterUnsupported.CodebaseIndex)},
			{name: "image configuration", changed: !reflect.DeepEqual(beforeUnsupported.Images, afterUnsupported.Images)},
			{name: "image browser credentials", changed: !reflect.DeepEqual(beforeUnsupported.ImageBrowserCredentials, afterUnsupported.ImageBrowserCredentials)},
			{name: "image client identities", changed: !reflect.DeepEqual(beforeUnsupported.ImageClientIdentities, afterUnsupported.ImageClientIdentities)},
			{name: "credential environment", changed: !reflect.DeepEqual(beforeUnsupported.CredentialEnvironment, afterUnsupported.CredentialEnvironment)},
		} {
			if field.changed {
				fields = append(fields, field.name)
			}
		}
		return proto.PeerRuntimeTransaction{}, false, fmt.Errorf("incremental runtime transaction contains unsupported state changes: %s", strings.Join(fields, ", "))
	}
	authenticationByProvider := map[string]proto.PeerProviderAuthentication{}
	for _, state := range authentication {
		providerID := state.Provider.Owner.ProviderID
		if _, exists := authenticationByProvider[providerID]; providerID == "" || exists {
			return proto.PeerRuntimeTransaction{}, false, errors.New("incremental runtime transaction has duplicate authentication state")
		}
		authenticationByProvider[providerID] = state
	}
	previousDefinitions := map[string]config.RemoteProviderDefinition{}
	for _, definition := range previous.Providers {
		previousDefinitions[definition.Config.ID] = definition
	}
	nextDefinitions := map[string]config.RemoteProviderDefinition{}
	for _, definition := range next.Providers {
		nextDefinitions[definition.Config.ID] = definition
	}
	previousBundles := map[string]bool{}
	for _, bundle := range previous.Bundles {
		previousBundles[bundle.Digest] = true
	}
	nextBundles := map[string]providerplugin.TransportBundle{}
	for _, bundle := range next.Bundles {
		nextBundles[bundle.Digest] = bundle
	}
	transaction := proto.PeerRuntimeTransaction{Runtime: base}
	providerIDs := make([]string, 0, len(previousDefinitions)+len(nextDefinitions))
	seenProvider := map[string]bool{}
	for providerID := range previousDefinitions {
		providerIDs = append(providerIDs, providerID)
		seenProvider[providerID] = true
	}
	for providerID := range nextDefinitions {
		if !seenProvider[providerID] {
			providerIDs = append(providerIDs, providerID)
		}
	}
	slices.Sort(providerIDs)
	providerPuts := map[string]bool{}
	expectedContextInstructions := maps.Clone(previous.ProviderContextInstructions)
	selectedProviders := map[string]bool{}
	for _, selected := range next.Models {
		selectedProviders[selected.Provider] = true
	}
	for _, providerID := range providerIDs {
		before, beforeFound := previousDefinitions[providerID]
		after, afterFound := nextDefinitions[providerID]
		if beforeFound && (!afterFound || !reflect.DeepEqual(before, after)) {
			provider, err := proto.ProviderRefForRuntime(previous, providerID)
			if err != nil {
				return proto.PeerRuntimeTransaction{}, false, err
			}
			removal := proto.PeerProviderDefinitionRemove{Provider: provider}
			transaction.Operations = append(transaction.Operations, proto.PeerRuntimeOperation{Type: proto.PeerTypeProviderDefinitionRemove, DefinitionRemove: &removal})
			delete(expectedContextInstructions, providerID)
		}
	}
	for _, providerID := range providerIDs {
		before, beforeFound := previousDefinitions[providerID]
		after, afterFound := nextDefinitions[providerID]
		if !afterFound || beforeFound && reflect.DeepEqual(before, after) {
			continue
		}
		provider, err := proto.ProviderRefForRuntime(next, providerID)
		if err != nil {
			return proto.PeerRuntimeTransaction{}, false, err
		}
		update := proto.PeerProviderDefinitionPut{Provider: provider, Definition: after}
		instruction, hasInstruction := next.ProviderContextInstructions[providerID]
		if selectedProviders[providerID] && !hasInstruction {
			return proto.PeerRuntimeTransaction{}, false, fmt.Errorf("selected provider %q has no context instruction", providerID)
		}
		if hasInstruction {
			update.ContextInstruction = &instruction
			expectedContextInstructions[providerID] = instruction
		}
		if after.BundleDigest != "" && !previousBundles[after.BundleDigest] {
			bundle, found := nextBundles[after.BundleDigest]
			if !found {
				return proto.PeerRuntimeTransaction{}, false, errors.New("incremental provider definition is missing its bundle")
			}
			update.Bundle = &bundle
		}
		transaction.Operations = append(transaction.Operations, proto.PeerRuntimeOperation{Type: proto.PeerTypeProviderDefinitionPut, DefinitionPut: &update})
		providerPuts[providerID] = true
	}
	if !reflect.DeepEqual(expectedContextInstructions, next.ProviderContextInstructions) {
		return proto.PeerRuntimeTransaction{}, false, errors.New("incremental runtime transaction contains unsupported provider context instruction changes")
	}
	previousCredentials := map[string]config.RemoteCredentialBinding{}
	for _, credential := range previous.Credentials {
		previousCredentials[credential.Owner.ProviderID] = credential
	}
	nextCredentials := map[string]config.RemoteCredentialBinding{}
	for _, credential := range next.Credentials {
		nextCredentials[credential.Owner.ProviderID] = credential
	}
	credentialIDs := make([]string, 0, len(nextCredentials))
	for providerID := range nextCredentials {
		credentialIDs = append(credentialIDs, providerID)
	}
	slices.Sort(credentialIDs)
	secretBearing := false
	for _, providerID := range credentialIDs {
		after := nextCredentials[providerID]
		before, beforeFound := previousCredentials[providerID]
		before.Generation, after.Generation = 0, 0
		if beforeFound && reflect.DeepEqual(before, after) && !providerPuts[providerID] {
			continue
		}
		state, found := authenticationByProvider[providerID]
		if !found {
			return proto.PeerRuntimeTransaction{}, false, fmt.Errorf("provider %q credential update has no authentication generation", providerID)
		}
		provider, err := proto.ProviderRefForRuntime(next, providerID)
		if err != nil {
			return proto.PeerRuntimeTransaction{}, false, err
		}
		if state.Provider != provider {
			return proto.PeerRuntimeTransaction{}, false, errors.New("provider authentication state does not match the runtime definition")
		}
		credential := nextCredentials[providerID]
		if credential.Unavailable && beforeFound && !providerPuts[providerID] && before.Owner == credential.Owner {
			invalidation := proto.PeerProviderCredentialInvalidate{Provider: provider, Generation: state.Generation}
			transaction.Operations = append(transaction.Operations, proto.PeerRuntimeOperation{Type: proto.PeerTypeProviderCredentialInvalidate, CredentialInvalidate: &invalidation})
		} else {
			config.RegisterRemoteCredentialSecrets(credential)
			replacement := proto.PeerProviderCredentialReplace{Provider: provider, Generation: state.Generation, Credential: credential}
			transaction.Operations = append(transaction.Operations, proto.PeerRuntimeOperation{Type: proto.PeerTypeProviderCredentialReplace, CredentialReplace: &replacement})
			secretBearing = !credential.Unavailable
		}
	}
	authenticationIDs := make([]string, 0, len(authenticationByProvider))
	for providerID := range authenticationByProvider {
		authenticationIDs = append(authenticationIDs, providerID)
	}
	slices.Sort(authenticationIDs)
	for _, providerID := range authenticationIDs {
		state := authenticationByProvider[providerID]
		messageType := proto.PeerTypeProviderAuthChanged
		if !state.Available {
			messageType = proto.PeerTypeProviderAuthInvalidated
		}
		payload := state
		transaction.Operations = append(transaction.Operations, proto.PeerRuntimeOperation{Type: messageType, Authentication: &payload})
	}
	selections, err := peerModelSelections(next, changedModels)
	if err != nil {
		return proto.PeerRuntimeTransaction{}, false, err
	}
	for index := range selections {
		selection := selections[index]
		transaction.Operations = append(transaction.Operations, proto.PeerRuntimeOperation{Type: proto.PeerTypeModelSelectionSet, ModelSelection: &selection})
	}
	if controlsChanged {
		controls := next.Controls
		transaction.Operations = append(transaction.Operations, proto.PeerRuntimeOperation{Type: proto.PeerTypeRuntimeControlsPatch, Controls: &controls})
	}
	if len(transaction.Operations) == 0 {
		return proto.PeerRuntimeTransaction{}, false, errors.New("incremental runtime transaction has no supported operation")
	}
	return transaction, secretBearing, nil
}

func (c *Client) ReplaceRemoteRuntime(ctx context.Context, id string, expected uint64, proposal config.RemoteRuntimeProposal) (*config.RemoteAuthority, error) {
	capabilities, err := c.NegotiateRemoteRuntime(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateRemoteRuntimeCapabilities(capabilities, &proposal); err != nil {
		return nil, err
	}
	payload := proto.PeerRuntimeReplace{ExpectedRevision: expected, Runtime: proposal, ExplicitRecovery: true}
	data, err := json.Marshal(payload)
	if err != nil || len(data) > capabilities.MaxRequestBytes {
		return nil, errors.New("client runtime exceeds remote request byte limit")
	}
	prior, err := c.workspaceAttachment(id, nil)
	if err != nil {
		return nil, err
	}
	if prior == nil || prior.Mode != "client" || prior.Revision != expected {
		return nil, errors.New("explicit runtime recovery requires the retained client authority")
	}
	messageID := uuid.NewString()
	var commandErr error
	for attempt := 0; attempt < 3; attempt++ {
		current, stateErr := c.peerWorkspaceAuthority(ctx, id, capabilities.Principal)
		if stateErr != nil {
			return nil, stateErr
		}
		if remoteAuthorityMismatch(current, capabilities.Principal, &proposal) == "" {
			c.retainWorkspaceAttachment(id, current)
			return current, nil
		}
		if !authorityMatchesAttachment(current, capabilities.Principal, *prior) {
			return nil, fmt.Errorf("%w: receiver does not retain the explicit recovery base", errRemoteAuthorityMismatch)
		}
		channel, sendErr := c.getWorkspaceChannel(ctx, id)
		if sendErr == nil {
			var acknowledgement proto.PeerAcknowledgement
			acknowledgement, sendErr = channel.peer.commandWithID(ctx, id, messageID, proto.PeerTypeRuntimeReplace, payload)
			if sendErr == nil {
				ack := acknowledgement.Authority
				if mismatch := remoteAuthorityMismatch(ack, capabilities.Principal, &proposal); mismatch != "" {
					return nil, fmt.Errorf("%w: %s", errRemoteAuthorityMismatch, mismatch)
				}
				c.retainWorkspaceAttachment(id, ack)
				return ack, nil
			}
		}
		commandErr = sendErr
		if workspaceChannelCommandRejected(sendErr) || ctx.Err() != nil {
			return nil, sendErr
		}
		if channel != nil {
			channel.peer.close(errors.New("peer channel reconnect required to reconcile an unacknowledged explicit runtime recovery"))
		}
		if attempt < 2 {
			delay := 50 * time.Millisecond << attempt
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, commandErr
}

// Report the failed contract without echoing received or private runtime data.
func remoteAuthorityMismatch(ack *config.RemoteAuthority, principal string, proposal *config.RemoteRuntimeProposal) string {
	if ack == nil {
		return "authority missing"
	}
	var fields []string
	if ack.Mode != "client" {
		fields = append(fields, "mode")
	}
	if ack.Principal != principal {
		fields = append(fields, "principal")
	}
	if ack.Revision != proposal.Revision {
		fields = append(fields, "revision")
	}
	if ack.Digest != proposal.Digest {
		fields = append(fields, "digest")
	}
	if len(fields) == 0 {
		return ""
	}
	return "mismatched fields: " + strings.Join(fields, ", ")
}

var errRemoteAuthorityMismatch = errors.New("remote runtime acknowledgement does not match the submitted authority")

func workspaceChannelCommandRejected(err error) bool {
	var rejection *WorkspaceChannelCommandError
	return errors.As(err, &rejection)
}

func authorityMatchesAttachment(authority *config.RemoteAuthority, principal string, attachment proto.WorkspaceAttachment) bool {
	return authority != nil && authority.Mode == attachment.Mode && authority.Principal == principal && authority.Revision == attachment.Revision && authority.Digest == attachment.Digest
}

func (c *Client) CompleteClientRefresh(ctx context.Context, id string, result config.ClientRefreshCompletion) error {
	frame := proto.WorkspaceChannelFrame{Type: proto.WorkspaceChannelRefreshCompleteFrame, CommandID: uuid.NewString(), RefreshComplete: &result}
	_, err := c.sendWorkspaceChannelCommand(ctx, id, frame)
	if err == nil {
		return nil
	}
	if workspaceChannelCommandRejected(err) {
		return errors.Join(ErrClientRefreshRejected, err)
	}
	if ctx.Err() != nil {
		return err
	}
	c.closeWorkspaceChannel(id)
	_, retryErr := c.sendWorkspaceChannelCommand(ctx, id, frame)
	if retryErr != nil && workspaceChannelCommandRejected(retryErr) {
		return errors.Join(ErrClientRefreshRejected, retryErr)
	}
	return retryErr
}

var ErrClientRefreshRejected = errors.New("client refresh completion was rejected")
