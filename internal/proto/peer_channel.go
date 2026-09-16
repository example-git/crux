package proto

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerplugin"
)

const (
	PeerChannelProtocol         = "crux-peer-channel-v2"
	PeerChannelVersion          = 2
	MaxPeerChannelEnvelopeBytes = config.MaxRemoteRuntimeBytes + (1 << 20)
	MaxPeerChannelPayloadBytes  = config.MaxRemoteRuntimeBytes
	MaxPeerChannelIDBytes       = 128
	MaxPeerChannelMessageBytes  = 4096
)

type PeerMessageKind string

const (
	PeerMessageCommand         PeerMessageKind = "command"
	PeerMessageEvent           PeerMessageKind = "event"
	PeerMessageAcknowledgement PeerMessageKind = "acknowledgement"
	PeerMessageError           PeerMessageKind = "error"
)

type PeerMessageDirection string

const (
	PeerDirectionClientToServer PeerMessageDirection = "client_to_server"
	PeerDirectionServerToClient PeerMessageDirection = "server_to_client"
	PeerDirectionBidirectional  PeerMessageDirection = "bidirectional"
)

type PeerMessageScope string

const (
	PeerScopeConnection PeerMessageScope = "connection"
	PeerScopeWorkspace  PeerMessageScope = "workspace"
	PeerScopeEither     PeerMessageScope = "either"
)

type PeerDeliveryClass string

const (
	PeerDeliveryCritical  PeerDeliveryClass = "critical"
	PeerDeliveryState     PeerDeliveryClass = "state"
	PeerDeliveryWorkspace PeerDeliveryClass = "workspace"
	PeerDeliveryTelemetry PeerDeliveryClass = "telemetry"
)

type PeerMessageType string

const (
	PeerTypeHello                         PeerMessageType = "peer.hello"
	PeerTypeReady                         PeerMessageType = "peer.ready"
	PeerTypeHeartbeat                     PeerMessageType = "peer.heartbeat"
	PeerTypeGoodbye                       PeerMessageType = "peer.goodbye"
	PeerTypeStateSummary                  PeerMessageType = "state.summary"
	PeerTypeStateRequired                 PeerMessageType = "state.required"
	PeerTypeWorkspaceAttach               PeerMessageType = "workspace.attach"
	PeerTypeWorkspaceDetach               PeerMessageType = "workspace.detach"
	PeerTypeSessionCurrentSet             PeerMessageType = "session.current.set"
	PeerTypeProviderDefinitionPut         PeerMessageType = "provider.definition.put"
	PeerTypeProviderDefinitionRemove      PeerMessageType = "provider.definition.remove"
	PeerTypeProviderContextInstructionSet PeerMessageType = "provider.context_instruction.set"
	PeerTypeProviderAvailability          PeerMessageType = "provider.availability.changed"
	PeerTypeProviderAuthChanged           PeerMessageType = "provider.auth.changed"
	PeerTypeProviderAuthInvalidated       PeerMessageType = "provider.auth.invalidated"
	PeerTypeProviderRefreshRequired       PeerMessageType = "provider.auth.refresh_required"
	PeerTypeProviderRefreshCompleted      PeerMessageType = "provider.auth.refresh_completed"
	PeerTypeProviderCredentialReplace     PeerMessageType = "provider.credential.replace"
	PeerTypeProviderCredentialInvalidate  PeerMessageType = "provider.credential.invalidate"
	PeerTypeModelSelectionSet             PeerMessageType = "model.selection.set"
	PeerTypeModelSelectionChanged         PeerMessageType = "model.selection.changed"
	PeerTypeRuntimeControlsPatch          PeerMessageType = "runtime.controls.patch"
	PeerTypeRuntimePatchApplied           PeerMessageType = "runtime.patch.applied"
	PeerTypeRuntimeTransaction            PeerMessageType = "runtime.transaction.apply"
	PeerTypeRuntimeReplace                PeerMessageType = "runtime.replace"
	PeerTypeAcknowledgement               PeerMessageType = "peer.acknowledgement"
	PeerTypeError                         PeerMessageType = "peer.error"
	PeerTypeEventLSP                      PeerMessageType = "event.lsp"
	PeerTypeEventMCP                      PeerMessageType = "event.mcp"
	PeerTypeEventPermissionRequest        PeerMessageType = "event.permission.request"
	PeerTypeEventPermissionResult         PeerMessageType = "event.permission.result"
	PeerTypeEventQuestionRequest          PeerMessageType = "event.question.request"
	PeerTypeEventQuestionResult           PeerMessageType = "event.question.result"
	PeerTypeEventMessage                  PeerMessageType = "event.message"
	PeerTypeEventSession                  PeerMessageType = "event.session"
	PeerTypeEventFile                     PeerMessageType = "event.file"
	PeerTypeEventAgent                    PeerMessageType = "event.agent"
	PeerTypeEventConfigChanged            PeerMessageType = "event.config.changed"
	PeerTypeEventSkills                   PeerMessageType = "event.skills"
	PeerTypeEventTask                     PeerMessageType = "event.task"
	PeerTypeRunCompleted                  PeerMessageType = "run.completed"
	PeerTypeRunFailed                     PeerMessageType = "run.failed"
	PeerTypeRunCancelled                  PeerMessageType = "run.cancelled"
)

type PeerEnvelope struct {
	Version     int             `json:"version"`
	Epoch       string          `json:"epoch"`
	Sequence    uint64          `json:"sequence"`
	MessageID   string          `json:"message_id"`
	ReplyTo     string          `json:"reply_to,omitempty"`
	Kind        PeerMessageKind `json:"kind"`
	Type        PeerMessageType `json:"type"`
	WorkspaceID string          `json:"workspace_id,omitempty"`
	Payload     json.RawMessage `json:"payload"`
}

type PeerMessageSpec struct {
	Direction      PeerMessageDirection
	Scope          PeerMessageScope
	Kind           PeerMessageKind
	Acknowledged   bool
	Delivery       PeerDeliveryClass
	MaxPayloadSize int
	newPayload     func() any
	validate       func(any) error
}

type PeerDecodedMessage struct {
	Envelope PeerEnvelope
	Payload  any
	Spec     PeerMessageSpec
}

type ProviderRef struct {
	Owner            providerauth.Owner `json:"owner"`
	DefinitionDigest string             `json:"definition_digest"`
	BundleDigest     string             `json:"bundle_digest,omitempty"`
}

func (r ProviderRef) Validate() error {
	if err := r.Owner.Validate(); err != nil {
		return err
	}
	if !validPeerDigest(r.DefinitionDigest, true) || !validPeerDigest(r.BundleDigest, false) {
		return errors.New("invalid provider reference digest")
	}
	return nil
}

func ProviderDefinitionDigest(definition config.RemoteProviderDefinition) (string, error) {
	data, err := json.Marshal(definition)
	if err != nil {
		return "", errors.New("provider definition cannot be encoded")
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func ProviderRefForRuntime(proposal config.RemoteRuntimeProposal, providerID string) (ProviderRef, error) {
	var definition *config.RemoteProviderDefinition
	for i := range proposal.Providers {
		if proposal.Providers[i].Config.ID == providerID {
			definition = &proposal.Providers[i]
			break
		}
	}
	if definition == nil {
		return ProviderRef{}, errors.New("provider definition is absent from the runtime")
	}
	var owner *providerauth.Owner
	for _, binding := range proposal.Credentials {
		if binding.Owner.ProviderID != providerID {
			continue
		}
		public := providerauth.PublicOwner(binding.Owner)
		if owner != nil && *owner != public {
			return ProviderRef{}, errors.New("provider runtime has ambiguous public ownership")
		}
		owner = &public
	}
	if owner == nil {
		return ProviderRef{}, errors.New("provider runtime has no owner-bound credential")
	}
	digest, err := ProviderDefinitionDigest(*definition)
	if err != nil {
		return ProviderRef{}, err
	}
	result := ProviderRef{Owner: *owner, DefinitionDigest: digest, BundleDigest: definition.BundleDigest}
	return result, result.Validate()
}

type ModelRef struct {
	Provider       ProviderRef `json:"provider"`
	ModelID        string      `json:"model_id"`
	SettingsDigest string      `json:"settings_digest,omitempty"`
}

func (r ModelRef) Validate() error {
	if err := r.Provider.Validate(); err != nil {
		return err
	}
	if !validPeerText(r.ModelID, 1024, true) || !validPeerDigest(r.SettingsDigest, false) {
		return errors.New("invalid model reference")
	}
	return nil
}

type PeerWorkspaceSummary struct {
	WorkspaceID string `json:"workspace_id"`
	Revision    uint64 `json:"revision"`
	Digest      string `json:"digest"`
}

func (s PeerWorkspaceSummary) Validate() error {
	if !validPeerText(s.WorkspaceID, 512, true) || s.Revision == 0 || !validPeerDigest(s.Digest, true) {
		return errors.New("invalid peer workspace summary")
	}
	return nil
}

type PeerHello struct {
	ClientID             string                 `json:"client_id"`
	LastReceivedSequence uint64                 `json:"last_received_sequence"`
	Workspaces           []PeerWorkspaceSummary `json:"workspaces"`
}

type PeerReady struct {
	LastReceivedSequence uint64                 `json:"last_received_sequence"`
	Workspaces           []PeerWorkspaceSummary `json:"workspaces"`
}

type PeerHeartbeat struct {
	LastReceivedSequence uint64                 `json:"last_received_sequence"`
	Workspaces           []PeerWorkspaceSummary `json:"workspaces"`
}

type PeerGoodbye struct {
	Reason string `json:"reason,omitempty"`
}

type PeerStateSummary struct {
	LastAcknowledgedSequence uint64                    `json:"last_acknowledged_sequence"`
	Workspaces               []PeerWorkspaceState      `json:"workspaces"`
	Providers                []ProviderRef             `json:"providers"`
	Models                   []PeerSelectedModel       `json:"models"`
	Authentication           []PeerAuthenticationState `json:"authentication"`
}

type PeerWorkspaceState struct {
	WorkspaceID string `json:"workspace_id"`
	Revision    uint64 `json:"revision"`
	Digest      string `json:"digest"`
}

type PeerSelectedModel struct {
	WorkspaceID string                   `json:"workspace_id"`
	ModelType   config.SelectedModelType `json:"model_type"`
	Model       ModelRef                 `json:"model"`
}

type PeerAuthenticationState struct {
	Provider   ProviderRef             `json:"provider"`
	Generation providerauth.Generation `json:"generation"`
	Available  bool                    `json:"available"`
}

type PeerStateRequirement struct {
	Kind        string                   `json:"kind"`
	WorkspaceID string                   `json:"workspace_id,omitempty"`
	Provider    *ProviderRef             `json:"provider,omitempty"`
	Digest      string                   `json:"digest,omitempty"`
	Generation  *providerauth.Generation `json:"generation,omitempty"`
}

type PeerStateRequired struct {
	Reason       string                 `json:"reason"`
	Requirements []PeerStateRequirement `json:"requirements"`
	FullResync   bool                   `json:"full_resync,omitempty"`
}

type PeerWorkspaceAttach struct {
	Attachment *WorkspaceAttachment `json:"attachment,omitempty"`
}

type PeerWorkspaceDetach struct{}

type PeerProviderDefinitionPut struct {
	Provider           ProviderRef                     `json:"provider"`
	Definition         config.RemoteProviderDefinition `json:"definition"`
	Bundle             *providerplugin.TransportBundle `json:"bundle,omitempty"`
	ContextInstruction *string                         `json:"context_instruction,omitempty"`
}

type PeerProviderDefinitionRemove struct {
	Provider ProviderRef `json:"provider"`
}

// PeerProviderContextInstructionSet updates only the per-provider context
// instruction text without touching the provider's definition or credential.
// A nil ContextInstruction clears the instruction. This exists because the
// provider definition-put wire operation is add-only (see PatchRemoteRuntime)
// and re-issuing an unchanged credential through a remove+put pair would
// require a fresh provider-authentication generation that an instruction-only
// change does not have, so instruction-only changes must use this dedicated
// operation instead of a definition remove+put pair.
type PeerProviderContextInstructionSet struct {
	Provider           ProviderRef `json:"provider"`
	ContextInstruction *string     `json:"context_instruction,omitempty"`
}

type PeerProviderAvailability struct {
	Provider   ProviderRef             `json:"provider"`
	Generation providerauth.Generation `json:"generation"`
	Available  bool                    `json:"available"`
	Reason     string                  `json:"reason,omitempty"`
}

type PeerProviderAuthentication struct {
	Provider     ProviderRef             `json:"provider"`
	Generation   providerauth.Generation `json:"generation"`
	Available    bool                    `json:"available"`
	AccountState string                  `json:"account_state,omitempty"`
}

type PeerProviderRefreshRequest struct {
	Provider  ProviderRef `json:"provider"`
	RequestID string      `json:"request_id"`
	Revision  uint64      `json:"revision"`
	Digest    string      `json:"digest"`
	Deadline  int64       `json:"deadline"`
}

type PeerProviderRefreshCompletion struct {
	RequestID    string `json:"request_id"`
	Revision     uint64 `json:"revision,omitempty"`
	Digest       string `json:"digest,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
	Failed       bool   `json:"failed,omitempty"`
	// Reason is a non-secret, human-readable explanation of why the owning
	// client could not complete the refresh (for example, the OAuth exchange
	// error or a definition mismatch). It must never carry tokens or other
	// credential material; only set alongside Failed.
	Reason string `json:"reason,omitempty"`
}

type PeerProviderCredentialReplace struct {
	Provider   ProviderRef                    `json:"provider"`
	Generation providerauth.Generation        `json:"generation"`
	Credential config.RemoteCredentialBinding `json:"credential"`
}

type PeerProviderCredentialInvalidate struct {
	Provider   ProviderRef             `json:"provider"`
	Generation providerauth.Generation `json:"generation"`
}

type PeerRuntimeBase struct {
	ExpectedRevision uint64 `json:"expected_revision"`
	ExpectedDigest   string `json:"expected_digest"`
	ResultRevision   uint64 `json:"result_revision"`
	ResultDigest     string `json:"result_digest"`
}

type PeerRuntimeOperation struct {
	Type                  PeerMessageType                    `json:"type"`
	DefinitionPut         *PeerProviderDefinitionPut         `json:"definition_put,omitempty"`
	DefinitionRemove      *PeerProviderDefinitionRemove      `json:"definition_remove,omitempty"`
	ContextInstructionSet *PeerProviderContextInstructionSet `json:"context_instruction_set,omitempty"`
	CredentialReplace     *PeerProviderCredentialReplace     `json:"credential_replace,omitempty"`
	CredentialInvalidate  *PeerProviderCredentialInvalidate  `json:"credential_invalidate,omitempty"`
	Availability          *PeerProviderAvailability          `json:"availability,omitempty"`
	Authentication        *PeerProviderAuthentication        `json:"authentication,omitempty"`
	ModelSelection        *PeerModelSelectionOperation       `json:"model_selection,omitempty"`
	Controls              *config.RemoteRuntimeControls      `json:"controls,omitempty"`
}

type PeerRuntimeTransaction struct {
	Runtime    PeerRuntimeBase        `json:"runtime"`
	Operations []PeerRuntimeOperation `json:"operations"`
}

type PeerModelSelectionOperation struct {
	ModelType config.SelectedModelType `json:"model_type"`
	Model     ModelRef                 `json:"model"`
	Settings  config.SelectedModel     `json:"settings"`
}

type PeerModelSelectionSet struct {
	Runtime    PeerRuntimeBase               `json:"runtime"`
	Selections []PeerModelSelectionOperation `json:"selections"`
}

type PeerModelSelectionChanged struct {
	Runtime   PeerRuntimeBase          `json:"runtime"`
	ModelType config.SelectedModelType `json:"model_type"`
	Model     ModelRef                 `json:"model"`
}

type PeerRuntimeControlsPatch struct {
	Runtime  PeerRuntimeBase              `json:"runtime"`
	Controls config.RemoteRuntimeControls `json:"controls"`
}

type PeerRuntimePatchApplied struct {
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
}

type PeerRuntimeReplace struct {
	ExpectedRevision uint64                       `json:"expected_revision"`
	Runtime          config.RemoteRuntimeProposal `json:"runtime"`
	ExplicitRecovery bool                         `json:"explicit_recovery,omitempty"`
}

type PeerAcknowledgement struct {
	Status    WorkspaceChannelStatus  `json:"status"`
	Authority *config.RemoteAuthority `json:"authority,omitempty"`
	Message   string                  `json:"message,omitempty"`
}

type PeerErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type PeerResourceEvent[T any] struct {
	Type    string `json:"type"`
	Payload T      `json:"payload"`
}

var peerMessageRegistry = map[PeerMessageType]PeerMessageSpec{
	PeerTypeHello:                         peerSpec(PeerDirectionClientToServer, PeerScopeConnection, PeerMessageCommand, true, PeerDeliveryCritical, 64<<10, func() any { return new(PeerHello) }, validateHello),
	PeerTypeReady:                         peerSpec(PeerDirectionServerToClient, PeerScopeConnection, PeerMessageEvent, false, PeerDeliveryCritical, 64<<10, func() any { return new(PeerReady) }, validateReady),
	PeerTypeHeartbeat:                     peerSpec(PeerDirectionBidirectional, PeerScopeConnection, PeerMessageEvent, false, PeerDeliveryCritical, 64<<10, func() any { return new(PeerHeartbeat) }, validateHeartbeat),
	PeerTypeGoodbye:                       peerSpec(PeerDirectionBidirectional, PeerScopeConnection, PeerMessageEvent, false, PeerDeliveryCritical, 8<<10, func() any { return new(PeerGoodbye) }, validateGoodbye),
	PeerTypeStateSummary:                  peerSpec(PeerDirectionBidirectional, PeerScopeConnection, PeerMessageEvent, false, PeerDeliveryState, 1<<20, func() any { return new(PeerStateSummary) }, validateStateSummary),
	PeerTypeStateRequired:                 peerSpec(PeerDirectionBidirectional, PeerScopeEither, PeerMessageEvent, false, PeerDeliveryState, 1<<20, func() any { return new(PeerStateRequired) }, validateStateRequired),
	PeerTypeWorkspaceAttach:               peerSpec(PeerDirectionClientToServer, PeerScopeWorkspace, PeerMessageCommand, true, PeerDeliveryCritical, 64<<10, func() any { return new(PeerWorkspaceAttach) }, validateWorkspaceAttach),
	PeerTypeWorkspaceDetach:               peerSpec(PeerDirectionClientToServer, PeerScopeWorkspace, PeerMessageCommand, true, PeerDeliveryCritical, 1<<10, func() any { return new(PeerWorkspaceDetach) }, nil),
	PeerTypeSessionCurrentSet:             peerSpec(PeerDirectionClientToServer, PeerScopeWorkspace, PeerMessageCommand, true, PeerDeliveryCritical, 64<<10, func() any { return new(CurrentSession) }, validatePeerCurrentSession),
	PeerTypeProviderDefinitionPut:         peerSpec(PeerDirectionClientToServer, PeerScopeWorkspace, PeerMessageCommand, true, PeerDeliveryState, MaxPeerChannelPayloadBytes, func() any { return new(PeerProviderDefinitionPut) }, validateProviderDefinitionPut),
	PeerTypeProviderDefinitionRemove:      peerSpec(PeerDirectionClientToServer, PeerScopeWorkspace, PeerMessageCommand, true, PeerDeliveryState, 64<<10, func() any { return new(PeerProviderDefinitionRemove) }, validateProviderDefinitionRemove),
	PeerTypeProviderContextInstructionSet: peerSpec(PeerDirectionClientToServer, PeerScopeWorkspace, PeerMessageCommand, true, PeerDeliveryState, MaxPeerChannelPayloadBytes, func() any { return new(PeerProviderContextInstructionSet) }, validateProviderContextInstructionSet),
	PeerTypeProviderAvailability:          peerSpec(PeerDirectionBidirectional, PeerScopeWorkspace, PeerMessageEvent, false, PeerDeliveryState, 64<<10, func() any { return new(PeerProviderAvailability) }, validateProviderAvailability),
	PeerTypeProviderAuthChanged:           peerSpec(PeerDirectionBidirectional, PeerScopeWorkspace, PeerMessageEvent, false, PeerDeliveryState, 64<<10, func() any { return new(PeerProviderAuthentication) }, validateProviderAuthentication),
	PeerTypeProviderAuthInvalidated:       peerSpec(PeerDirectionBidirectional, PeerScopeWorkspace, PeerMessageEvent, false, PeerDeliveryCritical, 64<<10, func() any { return new(PeerProviderAuthentication) }, validateProviderAuthentication),
	PeerTypeProviderRefreshRequired:       peerSpec(PeerDirectionServerToClient, PeerScopeWorkspace, PeerMessageEvent, false, PeerDeliveryCritical, 64<<10, func() any { return new(PeerProviderRefreshRequest) }, validateProviderRefreshRequest),
	PeerTypeProviderRefreshCompleted:      peerSpec(PeerDirectionClientToServer, PeerScopeWorkspace, PeerMessageCommand, true, PeerDeliveryCritical, 64<<10, func() any { return new(PeerProviderRefreshCompletion) }, validateProviderRefreshCompletion),
	PeerTypeProviderCredentialReplace:     peerSpec(PeerDirectionClientToServer, PeerScopeWorkspace, PeerMessageCommand, true, PeerDeliveryCritical, 1<<20, func() any { return new(PeerProviderCredentialReplace) }, validateProviderCredentialReplace),
	PeerTypeProviderCredentialInvalidate:  peerSpec(PeerDirectionClientToServer, PeerScopeWorkspace, PeerMessageCommand, true, PeerDeliveryCritical, 64<<10, func() any { return new(PeerProviderCredentialInvalidate) }, validateProviderCredentialInvalidate),
	PeerTypeModelSelectionSet:             peerSpec(PeerDirectionClientToServer, PeerScopeWorkspace, PeerMessageCommand, true, PeerDeliveryState, 256<<10, func() any { return new(PeerModelSelectionSet) }, validateModelSelectionSet),
	PeerTypeModelSelectionChanged:         peerSpec(PeerDirectionServerToClient, PeerScopeWorkspace, PeerMessageEvent, false, PeerDeliveryState, 128<<10, func() any { return new(PeerModelSelectionChanged) }, validateModelSelectionChanged),
	PeerTypeRuntimeControlsPatch:          peerSpec(PeerDirectionClientToServer, PeerScopeWorkspace, PeerMessageCommand, true, PeerDeliveryState, 256<<10, func() any { return new(PeerRuntimeControlsPatch) }, validateRuntimeControlsPatch),
	PeerTypeRuntimePatchApplied:           peerSpec(PeerDirectionServerToClient, PeerScopeWorkspace, PeerMessageEvent, false, PeerDeliveryState, 64<<10, func() any { return new(PeerRuntimePatchApplied) }, validateRuntimePatchApplied),
	PeerTypeRuntimeTransaction:            peerSpec(PeerDirectionClientToServer, PeerScopeWorkspace, PeerMessageCommand, true, PeerDeliveryState, MaxPeerChannelPayloadBytes, func() any { return new(PeerRuntimeTransaction) }, validateRuntimeTransaction),
	PeerTypeRuntimeReplace:                peerSpec(PeerDirectionClientToServer, PeerScopeWorkspace, PeerMessageCommand, true, PeerDeliveryState, MaxPeerChannelPayloadBytes, func() any { return new(PeerRuntimeReplace) }, validateRuntimeReplace),
	PeerTypeAcknowledgement:               peerSpec(PeerDirectionBidirectional, PeerScopeEither, PeerMessageAcknowledgement, false, PeerDeliveryCritical, 64<<10, func() any { return new(PeerAcknowledgement) }, validateAcknowledgement),
	PeerTypeError:                         peerSpec(PeerDirectionBidirectional, PeerScopeEither, PeerMessageError, false, PeerDeliveryCritical, 64<<10, func() any { return new(PeerErrorPayload) }, validateErrorPayload),
	PeerTypeEventLSP:                      peerEventSpec(PeerDeliveryWorkspace, func() any { return new(PeerResourceEvent[LSPEvent]) }),
	PeerTypeEventMCP:                      peerEventSpec(PeerDeliveryWorkspace, func() any { return new(PeerResourceEvent[MCPEvent]) }),
	PeerTypeEventPermissionRequest:        peerEventSpec(PeerDeliveryCritical, func() any { return new(PeerResourceEvent[PermissionRequest]) }),
	PeerTypeEventPermissionResult:         peerEventSpec(PeerDeliveryCritical, func() any { return new(PeerResourceEvent[PermissionNotification]) }),
	PeerTypeEventQuestionRequest:          peerEventSpec(PeerDeliveryCritical, func() any { return new(PeerResourceEvent[QuestionRequest]) }),
	PeerTypeEventQuestionResult:           peerEventSpec(PeerDeliveryCritical, func() any { return new(PeerResourceEvent[QuestionNotification]) }),
	PeerTypeEventMessage:                  peerEventSpec(PeerDeliveryWorkspace, func() any { return new(PeerResourceEvent[Message]) }),
	PeerTypeEventSession:                  peerEventSpec(PeerDeliveryWorkspace, func() any { return new(PeerResourceEvent[Session]) }),
	PeerTypeEventFile:                     peerEventSpec(PeerDeliveryWorkspace, func() any { return new(PeerResourceEvent[File]) }),
	PeerTypeEventAgent:                    peerEventSpec(PeerDeliveryWorkspace, func() any { return new(PeerResourceEvent[AgentEvent]) }),
	PeerTypeEventConfigChanged:            peerEventSpec(PeerDeliveryState, func() any { return new(PeerResourceEvent[ConfigChanged]) }),
	PeerTypeEventSkills:                   peerEventSpec(PeerDeliveryWorkspace, func() any { return new(PeerResourceEvent[SkillsEvent]) }),
	PeerTypeEventTask:                     peerEventSpec(PeerDeliveryWorkspace, func() any { return new(PeerResourceEvent[TaskNotification]) }),
	PeerTypeRunCompleted:                  peerRunEventSpec(PeerTypeRunCompleted),
	PeerTypeRunFailed:                     peerRunEventSpec(PeerTypeRunFailed),
	PeerTypeRunCancelled:                  peerRunEventSpec(PeerTypeRunCancelled),
}

func peerSpec(direction PeerMessageDirection, scope PeerMessageScope, kind PeerMessageKind, acknowledged bool, delivery PeerDeliveryClass, maxPayloadSize int, constructor func() any, validator func(any) error) PeerMessageSpec {
	return PeerMessageSpec{Direction: direction, Scope: scope, Kind: kind, Acknowledged: acknowledged, Delivery: delivery, MaxPayloadSize: maxPayloadSize, newPayload: constructor, validate: validator}
}

func peerEventSpec(delivery PeerDeliveryClass, constructor func() any) PeerMessageSpec {
	return peerSpec(PeerDirectionServerToClient, PeerScopeWorkspace, PeerMessageEvent, false, delivery, 16<<20, constructor, validatePeerResourceEvent)
}

func peerRunEventSpec(messageType PeerMessageType) PeerMessageSpec {
	return peerSpec(PeerDirectionServerToClient, PeerScopeWorkspace, PeerMessageEvent, false, PeerDeliveryCritical, 16<<20, func() any { return new(PeerResourceEvent[RunComplete]) }, func(payload any) error {
		return validatePeerRunEvent(messageType, payload)
	})
}

func PeerMessageSpecification(messageType PeerMessageType) (PeerMessageSpec, bool) {
	spec, ok := peerMessageRegistry[messageType]
	return spec, ok
}

func RegisteredPeerMessageTypes() []PeerMessageType {
	result := make([]PeerMessageType, 0, len(peerMessageRegistry))
	for messageType := range peerMessageRegistry {
		result = append(result, messageType)
	}
	slices.Sort(result)
	return result
}

func EncodePeerMessage(epoch string, sequence uint64, messageID, replyTo, workspaceID string, messageType PeerMessageType, payload any) ([]byte, error) {
	spec, ok := peerMessageRegistry[messageType]
	if !ok {
		return nil, fmt.Errorf("unknown peer message type %q", messageType)
	}
	if spec.validate != nil {
		if err := spec.validate(payload); err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) == 0 || len(raw) > spec.MaxPayloadSize {
		return nil, errors.New("peer message payload exceeds byte limit")
	}
	envelope := PeerEnvelope{Version: PeerChannelVersion, Epoch: epoch, Sequence: sequence, MessageID: messageID, ReplyTo: replyTo, Kind: spec.Kind, Type: messageType, WorkspaceID: workspaceID, Payload: raw}
	if err := validatePeerEnvelope(envelope, spec); err != nil {
		return nil, err
	}
	data, err := json.Marshal(envelope)
	if err != nil || len(data) > MaxPeerChannelEnvelopeBytes {
		return nil, errors.New("peer channel envelope exceeds byte limit")
	}
	return data, nil
}

func DecodePeerPayload(messageType PeerMessageType, data []byte) (any, error) {
	spec, ok := peerMessageRegistry[messageType]
	if !ok {
		return nil, fmt.Errorf("unknown peer message type %q", messageType)
	}
	if len(data) == 0 || len(data) > spec.MaxPayloadSize {
		return nil, errors.New("peer message payload exceeds byte limit")
	}
	if err := validateWorkspaceChannelJSON(data); err != nil {
		return nil, fmt.Errorf("invalid peer message payload: %w", err)
	}
	payload := spec.newPayload()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(payload); err != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("invalid payload for peer message %q", messageType)
	}
	if spec.validate != nil {
		if err := spec.validate(payload); err != nil {
			return nil, fmt.Errorf("invalid payload for peer message %q: %w", messageType, err)
		}
	}
	return payload, nil
}

func DecodePeerMessage(data []byte, direction PeerMessageDirection) (PeerDecodedMessage, error) {
	var result PeerDecodedMessage
	if len(data) == 0 || len(data) > MaxPeerChannelEnvelopeBytes {
		return result, errors.New("peer channel envelope exceeds byte limit")
	}
	if err := validateWorkspaceChannelJSON(data); err != nil {
		return result, fmt.Errorf("invalid peer channel envelope: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result.Envelope); err != nil {
		return result, errors.New("invalid peer channel envelope")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return result, errors.New("peer channel envelope must contain exactly one JSON value")
	}
	spec, ok := peerMessageRegistry[result.Envelope.Type]
	if !ok {
		return result, fmt.Errorf("unknown peer message type %q", result.Envelope.Type)
	}
	if spec.Direction != PeerDirectionBidirectional && spec.Direction != direction {
		return result, fmt.Errorf("peer message %q is not allowed in this direction", result.Envelope.Type)
	}
	if err := validatePeerEnvelope(result.Envelope, spec); err != nil {
		return result, err
	}
	payload, err := DecodePeerPayload(result.Envelope.Type, result.Envelope.Payload)
	if err != nil {
		return result, err
	}
	result.Payload, result.Spec = payload, spec
	return result, nil
}

func validatePeerEnvelope(envelope PeerEnvelope, spec PeerMessageSpec) error {
	if envelope.Version != PeerChannelVersion {
		return errors.New("unsupported peer channel protocol version")
	}
	if !validPeerEpoch(envelope.Epoch) || envelope.Sequence == 0 || !validPeerID(envelope.MessageID) || !validOptionalPeerID(envelope.ReplyTo) {
		return errors.New("invalid peer channel message identity")
	}
	if envelope.Kind != spec.Kind || len(envelope.Payload) == 0 || len(envelope.Payload) > spec.MaxPayloadSize {
		return errors.New("peer channel envelope does not match its registered type")
	}
	workspacePresent := envelope.WorkspaceID != ""
	if workspacePresent && !validPeerText(envelope.WorkspaceID, 512, true) {
		return errors.New("invalid peer channel workspace")
	}
	if spec.Scope == PeerScopeWorkspace && !workspacePresent || spec.Scope == PeerScopeConnection && workspacePresent {
		return errors.New("peer channel message has invalid scope")
	}
	if envelope.Kind == PeerMessageAcknowledgement || envelope.Kind == PeerMessageError {
		if envelope.ReplyTo == "" {
			return errors.New("peer channel response requires reply_to")
		}
	} else if envelope.ReplyTo != "" {
		return errors.New("peer channel request cannot set reply_to")
	}
	return nil
}

func validateHello(payload any) error {
	value, ok := peerPayload[PeerHello](payload)
	if !ok || !validPeerText(value.ClientID, 128, true) {
		return errors.New("invalid peer hello")
	}
	return validateWorkspaceSummaries(value.Workspaces)
}

func validateReady(payload any) error {
	value, ok := peerPayload[PeerReady](payload)
	if !ok {
		return errors.New("invalid peer ready")
	}
	return validateWorkspaceSummaries(value.Workspaces)
}

func validateHeartbeat(payload any) error {
	value, ok := peerPayload[PeerHeartbeat](payload)
	if !ok {
		return errors.New("invalid peer heartbeat")
	}
	return validateWorkspaceSummaries(value.Workspaces)
}

func validateGoodbye(payload any) error {
	value, ok := peerPayload[PeerGoodbye](payload)
	if !ok || !validPeerText(value.Reason, MaxPeerChannelMessageBytes, false) {
		return errors.New("invalid peer goodbye")
	}
	return nil
}

func validateWorkspaceSummaries(values []PeerWorkspaceSummary) error {
	if len(values) > 4096 {
		return errors.New("too many peer workspace summaries")
	}
	seen := map[string]bool{}
	for _, value := range values {
		if err := value.Validate(); err != nil || seen[value.WorkspaceID] {
			return errors.New("invalid peer workspace summaries")
		}
		seen[value.WorkspaceID] = true
	}
	return nil
}

func validateStateSummary(payload any) error {
	value, ok := peerPayload[PeerStateSummary](payload)
	if !ok || len(value.Workspaces) > 4096 || len(value.Providers) > config.MaxRemoteRuntimeProviders || len(value.Models) > 8192 || len(value.Authentication) > config.MaxRemoteRuntimeProviders {
		return errors.New("invalid peer state summary")
	}
	for _, workspace := range value.Workspaces {
		if !validPeerText(workspace.WorkspaceID, 512, true) || workspace.Revision == 0 || !validPeerDigest(workspace.Digest, true) {
			return errors.New("invalid peer state summary workspace")
		}
	}
	for _, provider := range value.Providers {
		if err := provider.Validate(); err != nil {
			return err
		}
	}
	for _, model := range value.Models {
		if !validPeerText(model.WorkspaceID, 512, true) || model.ModelType != config.SelectedModelTypeLarge && model.ModelType != config.SelectedModelTypeSmall {
			return errors.New("invalid peer state summary model")
		}
		if err := model.Model.Validate(); err != nil {
			return err
		}
	}
	for _, authentication := range value.Authentication {
		if err := authentication.Provider.Validate(); err != nil {
			return err
		}
		if err := authentication.Generation.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateStateRequired(payload any) error {
	value, ok := peerPayload[PeerStateRequired](payload)
	if !ok || !validPeerText(value.Reason, MaxPeerChannelMessageBytes, true) || len(value.Requirements) > config.MaxRemoteRuntimeProviders {
		return errors.New("invalid peer state requirement")
	}
	for _, requirement := range value.Requirements {
		if requirement.Kind != "workspace" && requirement.Kind != "provider_definition" && requirement.Kind != "provider_bundle" && requirement.Kind != "provider_credential" && requirement.Kind != "runtime_full" {
			return errors.New("invalid peer state requirement kind")
		}
		if !validPeerText(requirement.WorkspaceID, 512, requirement.Kind == "workspace" || requirement.Kind == "runtime_full") {
			return errors.New("invalid peer state requirement workspace")
		}
		if requirement.Provider != nil {
			if err := requirement.Provider.Validate(); err != nil {
				return err
			}
		}
		if !validPeerDigest(requirement.Digest, false) {
			return errors.New("invalid peer state requirement digest")
		}
		if requirement.Generation != nil {
			if err := requirement.Generation.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateWorkspaceAttach(payload any) error {
	value, ok := peerPayload[PeerWorkspaceAttach](payload)
	if !ok {
		return errors.New("invalid peer workspace attachment")
	}
	if value.Attachment == nil {
		return nil
	}
	return value.Attachment.Validate()
}

func validatePeerCurrentSession(payload any) error {
	value, ok := peerPayload[CurrentSession](payload)
	if !ok || value.SelectionGeneration == nil || *value.SelectionGeneration == 0 || !validPeerText(value.SessionID, 512, false) {
		return errors.New("invalid current session selection")
	}
	return nil
}

func validateProviderDefinitionPut(payload any) error {
	value, ok := peerPayload[PeerProviderDefinitionPut](payload)
	if !ok {
		return errors.New("invalid provider definition update")
	}
	if err := value.Provider.Validate(); err != nil {
		return err
	}
	digest, err := ProviderDefinitionDigest(value.Definition)
	if err != nil || digest != value.Provider.DefinitionDigest || value.Definition.Config.ID != value.Provider.Owner.ProviderID || value.Definition.BundleDigest != value.Provider.BundleDigest {
		return errors.New("provider definition does not match its reference")
	}
	if value.Provider.BundleDigest == "" && value.Bundle != nil || value.Provider.BundleDigest != "" && value.Bundle != nil && value.Bundle.Digest != value.Provider.BundleDigest {
		return errors.New("provider bundle does not match its reference")
	}
	if value.ContextInstruction != nil && (len(*value.ContextInstruction) > config.MaxProviderContextInstructionBytes || !utf8.ValidString(*value.ContextInstruction)) {
		return errors.New("provider context instruction is invalid")
	}
	return nil
}

func validateProviderDefinitionRemove(payload any) error {
	value, ok := peerPayload[PeerProviderDefinitionRemove](payload)
	if !ok {
		return errors.New("invalid provider definition removal")
	}
	return value.Provider.Validate()
}

func validateProviderContextInstructionSet(payload any) error {
	value, ok := peerPayload[PeerProviderContextInstructionSet](payload)
	if !ok {
		return errors.New("invalid provider context instruction update")
	}
	if err := value.Provider.Validate(); err != nil {
		return err
	}
	if value.ContextInstruction != nil && (len(*value.ContextInstruction) > config.MaxProviderContextInstructionBytes || !utf8.ValidString(*value.ContextInstruction)) {
		return errors.New("provider context instruction is invalid")
	}
	return nil
}

func validateProviderAvailability(payload any) error {
	value, ok := peerPayload[PeerProviderAvailability](payload)
	if !ok || !validPeerText(value.Reason, MaxPeerChannelMessageBytes, false) {
		return errors.New("invalid provider availability")
	}
	if err := value.Provider.Validate(); err != nil {
		return err
	}
	return value.Generation.Validate()
}

func validateProviderAuthentication(payload any) error {
	value, ok := peerPayload[PeerProviderAuthentication](payload)
	if !ok || !validPeerText(value.AccountState, 128, false) {
		return errors.New("invalid provider authentication event")
	}
	if err := value.Provider.Validate(); err != nil {
		return err
	}
	return value.Generation.Validate()
}

func validateProviderRefreshRequest(payload any) error {
	value, ok := peerPayload[PeerProviderRefreshRequest](payload)
	if !ok || !validPeerID(value.RequestID) || value.Revision == 0 || !validPeerDigest(value.Digest, true) || value.Deadline <= 0 {
		return errors.New("invalid provider refresh request")
	}
	return value.Provider.Validate()
}

func validateProviderRefreshCompletion(payload any) error {
	value, ok := peerPayload[PeerProviderRefreshCompletion](payload)
	if !ok || !validPeerID(value.RequestID) || !validPeerText(value.Reason, MaxPeerChannelMessageBytes, false) {
		return errors.New("invalid provider refresh completion")
	}
	if value.Failed {
		if value.Revision != 0 || value.Digest != "" || value.CredentialID != "" {
			return errors.New("failed provider refresh contains a credential receipt")
		}
		return nil
	}
	if value.Reason != "" {
		return errors.New("successful provider refresh cannot carry a failure reason")
	}
	if value.Revision == 0 || !validPeerDigest(value.Digest, true) || !validPeerText(value.CredentialID, 1024, true) {
		return errors.New("invalid provider refresh completion receipt")
	}
	return nil
}

func validateProviderCredentialReplace(payload any) error {
	value, ok := peerPayload[PeerProviderCredentialReplace](payload)
	if !ok {
		return errors.New("invalid provider credential replacement")
	}
	if err := value.Provider.Validate(); err != nil {
		return err
	}
	if err := value.Generation.Validate(); err != nil {
		return err
	}
	if providerauth.PublicOwner(value.Credential.Owner) != value.Provider.Owner {
		return errors.New("provider credential does not match its reference")
	}
	return nil
}

func validateProviderCredentialInvalidate(payload any) error {
	value, ok := peerPayload[PeerProviderCredentialInvalidate](payload)
	if !ok {
		return errors.New("invalid provider credential invalidation")
	}
	if err := value.Provider.Validate(); err != nil {
		return err
	}
	return value.Generation.Validate()
}

func validateRuntimeBase(value PeerRuntimeBase) error {
	if value.ExpectedRevision == 0 || value.ExpectedRevision == ^uint64(0) || value.ResultRevision != value.ExpectedRevision+1 || !validPeerDigest(value.ExpectedDigest, true) || !validPeerDigest(value.ResultDigest, true) {
		return errors.New("invalid incremental runtime base")
	}
	return nil
}

func validateModelSelectionSet(payload any) error {
	value, ok := peerPayload[PeerModelSelectionSet](payload)
	if !ok || len(value.Selections) == 0 || len(value.Selections) > 2 {
		return errors.New("invalid model selection")
	}
	if err := validateRuntimeBase(value.Runtime); err != nil {
		return err
	}
	seen := map[config.SelectedModelType]bool{}
	for _, selection := range value.Selections {
		if selection.ModelType != config.SelectedModelTypeLarge && selection.ModelType != config.SelectedModelTypeSmall || seen[selection.ModelType] || selection.Settings.Provider != selection.Model.Provider.Owner.ProviderID || selection.Settings.Model != selection.Model.ModelID {
			return errors.New("invalid model selection")
		}
		if err := selection.Model.Validate(); err != nil {
			return err
		}
		seen[selection.ModelType] = true
	}
	return nil
}

func validateModelSelectionChanged(payload any) error {
	value, ok := peerPayload[PeerModelSelectionChanged](payload)
	if !ok || value.ModelType != config.SelectedModelTypeLarge && value.ModelType != config.SelectedModelTypeSmall {
		return errors.New("invalid model selection change")
	}
	if err := validateRuntimeBase(value.Runtime); err != nil {
		return err
	}
	return value.Model.Validate()
}

func validateRuntimeControlsPatch(payload any) error {
	value, ok := peerPayload[PeerRuntimeControlsPatch](payload)
	if !ok {
		return errors.New("invalid runtime controls patch")
	}
	return validateRuntimeBase(value.Runtime)
}

func validateRuntimePatchApplied(payload any) error {
	value, ok := peerPayload[PeerRuntimePatchApplied](payload)
	if !ok || value.Revision == 0 || !validPeerDigest(value.Digest, true) {
		return errors.New("invalid runtime patch acknowledgement")
	}
	return nil
}

func validateRuntimeTransaction(payload any) error {
	value, ok := peerPayload[PeerRuntimeTransaction](payload)
	if !ok || len(value.Operations) == 0 || len(value.Operations) > 32 {
		return errors.New("invalid incremental runtime transaction")
	}
	if err := validateRuntimeBase(value.Runtime); err != nil {
		return err
	}
	lastPhase := -1
	seen := map[string]bool{}
	seenModels := map[config.SelectedModelType]bool{}
	for _, operation := range value.Operations {
		count := 0
		for _, present := range []bool{operation.DefinitionPut != nil, operation.DefinitionRemove != nil, operation.ContextInstructionSet != nil, operation.CredentialReplace != nil, operation.CredentialInvalidate != nil, operation.Availability != nil, operation.Authentication != nil, operation.ModelSelection != nil, operation.Controls != nil} {
			if present {
				count++
			}
		}
		if count != 1 {
			return errors.New("incremental runtime operation must contain exactly one typed payload")
		}
		phase, key := -1, ""
		switch operation.Type {
		case PeerTypeProviderDefinitionRemove:
			phase = 0
			if operation.DefinitionRemove == nil || validateProviderDefinitionRemove(operation.DefinitionRemove) != nil {
				return errors.New("invalid provider definition removal operation")
			}
			key = string(operation.Type) + "\x00" + operation.DefinitionRemove.Provider.Owner.ProviderID
		case PeerTypeProviderDefinitionPut:
			phase = 1
			if operation.DefinitionPut == nil || validateProviderDefinitionPut(operation.DefinitionPut) != nil {
				return errors.New("invalid provider definition update operation")
			}
			key = string(operation.Type) + "\x00" + operation.DefinitionPut.Provider.Owner.ProviderID
		case PeerTypeProviderContextInstructionSet:
			phase = 1
			if operation.ContextInstructionSet == nil || validateProviderContextInstructionSet(operation.ContextInstructionSet) != nil {
				return errors.New("invalid provider context instruction operation")
			}
			key = string(operation.Type) + "\x00" + operation.ContextInstructionSet.Provider.Owner.ProviderID
		case PeerTypeProviderCredentialReplace:
			phase = 2
			if operation.CredentialReplace == nil {
				return errors.New("invalid provider credential replacement operation")
			}
			if err := validateProviderCredentialReplace(operation.CredentialReplace); err != nil {
				return fmt.Errorf("invalid provider credential replacement operation: %w", err)
			}
			if operation.CredentialReplace.Credential.Generation != value.Runtime.ResultRevision {
				return fmt.Errorf("provider credential replacement has runtime generation %d, expected %d", operation.CredentialReplace.Credential.Generation, value.Runtime.ResultRevision)
			}
			key = "credential\x00" + operation.CredentialReplace.Provider.Owner.ProviderID
		case PeerTypeProviderCredentialInvalidate:
			phase = 2
			if operation.CredentialInvalidate == nil || validateProviderCredentialInvalidate(operation.CredentialInvalidate) != nil {
				return errors.New("invalid provider credential invalidation operation")
			}
			key = "credential\x00" + operation.CredentialInvalidate.Provider.Owner.ProviderID
		case PeerTypeProviderAvailability:
			phase = 3
			if operation.Availability == nil || validateProviderAvailability(operation.Availability) != nil {
				return errors.New("invalid provider availability operation")
			}
			key = string(operation.Type) + "\x00" + operation.Availability.Provider.Owner.ProviderID
		case PeerTypeProviderAuthChanged, PeerTypeProviderAuthInvalidated:
			phase = 3
			if operation.Authentication == nil || validateProviderAuthentication(operation.Authentication) != nil || operation.Type == PeerTypeProviderAuthInvalidated && operation.Authentication.Available {
				return errors.New("invalid provider authentication operation")
			}
			key = "authentication\x00" + operation.Authentication.Provider.Owner.ProviderID
		case PeerTypeModelSelectionSet:
			phase = 4
			selection := operation.ModelSelection
			if selection == nil || selection.ModelType != config.SelectedModelTypeLarge && selection.ModelType != config.SelectedModelTypeSmall || seenModels[selection.ModelType] || selection.Settings.Provider != selection.Model.Provider.Owner.ProviderID || selection.Settings.Model != selection.Model.ModelID || selection.Model.Validate() != nil {
				return errors.New("invalid model selection operation")
			}
			seenModels[selection.ModelType] = true
			key = string(operation.Type) + "\x00" + string(selection.ModelType)
		case PeerTypeRuntimeControlsPatch:
			phase = 5
			if operation.Controls == nil {
				return errors.New("invalid runtime controls operation")
			}
			key = string(operation.Type)
		default:
			return errors.New("unknown incremental runtime operation type")
		}
		if phase < lastPhase || seen[key] {
			return errors.New("incremental runtime operations are duplicated or out of order")
		}
		seen[key] = true
		lastPhase = phase
	}
	return nil
}

func validateRuntimeReplace(payload any) error {
	value, ok := peerPayload[PeerRuntimeReplace](payload)
	if !ok || value.ExpectedRevision == ^uint64(0) || value.Runtime.Revision != value.ExpectedRevision+1 {
		return errors.New("invalid complete runtime replacement")
	}
	digest, err := config.RemoteRuntimeDigest(value.Runtime)
	if err != nil || digest != value.Runtime.Digest {
		return errors.New("complete runtime replacement digest is invalid")
	}
	return nil
}

func validateAcknowledgement(payload any) error {
	value, ok := peerPayload[PeerAcknowledgement](payload)
	if !ok || !validPeerText(value.Message, MaxPeerChannelMessageBytes, false) {
		return errors.New("invalid peer acknowledgement")
	}
	switch value.Status {
	case WorkspaceChannelStatusOK:
		if value.Message != "" {
			return errors.New("successful peer acknowledgement contains an error")
		}
	case WorkspaceChannelStatusInvalid, WorkspaceChannelStatusConflict, WorkspaceChannelStatusForbidden, WorkspaceChannelStatusNotFound, WorkspaceChannelStatusUnavailable, WorkspaceChannelStatusInternal:
		if value.Authority != nil {
			return errors.New("failed peer acknowledgement contains authority")
		}
	default:
		return errors.New("invalid peer acknowledgement status")
	}
	return nil
}

func validateErrorPayload(payload any) error {
	value, ok := peerPayload[PeerErrorPayload](payload)
	if !ok || !validPeerText(value.Code, 128, true) || !validPeerText(value.Message, MaxPeerChannelMessageBytes, true) {
		return errors.New("invalid peer error")
	}
	return nil
}

func validatePeerResourceEvent(payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return errors.New("invalid peer resource event")
	}
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.Type != "created" && event.Type != "updated" && event.Type != "deleted" {
		return errors.New("invalid peer resource event type")
	}
	return nil
}

func validatePeerRunEvent(messageType PeerMessageType, payload any) error {
	value, ok := peerPayload[PeerResourceEvent[RunComplete]](payload)
	if !ok {
		return errors.New("invalid peer run event")
	}
	if err := validatePeerResourceEvent(payload); err != nil {
		return err
	}
	if !validPeerID(value.Payload.RunID) || !validPeerText(value.Payload.SessionID, 512, true) {
		return errors.New("invalid peer run identity")
	}
	switch messageType {
	case PeerTypeRunCompleted:
		if value.Payload.Error != "" || value.Payload.Cancelled {
			return errors.New("completed peer run contains a failure")
		}
	case PeerTypeRunFailed:
		if value.Payload.Error == "" || value.Payload.Cancelled {
			return errors.New("failed peer run has invalid status")
		}
	case PeerTypeRunCancelled:
		if !value.Payload.Cancelled {
			return errors.New("cancelled peer run has invalid status")
		}
	}
	return nil
}

func peerPayload[T any](payload any) (T, bool) {
	var zero T
	switch value := payload.(type) {
	case T:
		return value, true
	case *T:
		if value != nil {
			return *value, true
		}
	}
	return zero, false
}

func validPeerEpoch(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'f') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

func validPeerID(value string) bool {
	return validPeerText(value, MaxPeerChannelIDBytes, true) && !strings.ContainsAny(value, "\x00\r\n")
}

func validOptionalPeerID(value string) bool {
	return value == "" || validPeerID(value)
}

func validPeerText(value string, max int, required bool) bool {
	return len(value) <= max && utf8.ValidString(value) && (!required || strings.TrimSpace(value) != "")
}

func validPeerDigest(value string, required bool) bool {
	if value == "" {
		return !required
	}
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
