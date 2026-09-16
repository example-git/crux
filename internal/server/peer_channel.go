package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/redact"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	peerChannelWriteTimeout = 10 * time.Second
	peerChannelPongTimeout  = 60 * time.Second
	peerChannelPingInterval = 30 * time.Second
	peerJournalTTL          = 15 * time.Minute
	peerJournalLimit        = 4096
)

type peerJournalEntry struct {
	digest  string
	ack     proto.PeerAcknowledgement
	created time.Time
}

type peerChannelInbound struct {
	message proto.PeerDecodedMessage
	err     error
}

type peerChannelOutbound struct {
	workspaceID string
	replyTo     string
	messageType proto.PeerMessageType
	payload     any
	controlType int
	controlData []byte
}

type serverPeerAttachment struct {
	workspace      *backend.Workspace
	cancel         context.CancelFunc
	once           sync.Once
	authMu         sync.Mutex
	authentication map[providerauth.Owner]providerauth.Generation
}

type serverPeerChannel struct {
	controller            *controllerV1
	connection            *websocket.Conn
	clientID              string
	principal             string
	epoch                 string
	ctx                   context.Context
	cancel                context.CancelCauseFunc
	critical              chan peerChannelOutbound
	state                 chan peerChannelOutbound
	workspace             chan peerChannelOutbound
	telemetry             chan peerChannelOutbound
	attachmentsMu         sync.Mutex
	attachments           map[string]*serverPeerAttachment
	lastReceived          atomic.Uint64
	lastSent              atomic.Uint64
	lastHeartbeat         atomic.Int64
	lastHeartbeatSequence atomic.Uint64
}

func (c *controllerV1) handleGetPeerChannel(w http.ResponseWriter, r *http.Request) {
	if !c.requireRuntimeProtocol(w, r) {
		return
	}
	clientID, ok := c.requireClientID(w, r)
	if !ok {
		return
	}
	connection, err := (&websocket.Upgrader{
		ReadBufferSize:  32 << 10,
		WriteBufferSize: 32 << 10,
		Subprotocols:    []string{proto.PeerChannelProtocol},
	}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancelCause(r.Context())
	peer := &serverPeerChannel{
		controller: c, connection: connection, clientID: clientID, principal: requestPrincipal(r),
		ctx: ctx, cancel: cancel, critical: make(chan peerChannelOutbound, 64), state: make(chan peerChannelOutbound, 128),
		workspace: make(chan peerChannelOutbound, 512), telemetry: make(chan peerChannelOutbound, 64), attachments: map[string]*serverPeerAttachment{},
	}
	peer.lastHeartbeat.Store(time.Now().UnixNano())
	connection.SetReadLimit(proto.MaxPeerChannelEnvelopeBytes)
	_ = connection.SetReadDeadline(time.Now().Add(peerChannelPongTimeout))
	connection.SetPingHandler(func(data string) error {
		return peer.enqueue(peerChannelOutbound{controlType: websocket.PongMessage, controlData: []byte(data)})
	})
	connection.SetPongHandler(func(string) error {
		return connection.SetReadDeadline(time.Now().Add(peerChannelPongTimeout))
	})
	defer peer.close()
	reads := make(chan peerChannelInbound, 32)
	go peer.readLoop(reads)
	writerDone := make(chan error, 1)
	go func() { writerDone <- peer.writeLoop() }()
	if err := peer.dispatch(reads, writerDone); err != nil {
		cancel(err)
	}
}

func (peer *serverPeerChannel) dispatch(reads <-chan peerChannelInbound, writerDone <-chan error) error {
	ready := false
	for {
		select {
		case <-peer.ctx.Done():
			return context.Cause(peer.ctx)
		case err := <-writerDone:
			return err
		case received := <-reads:
			if received.err != nil {
				return received.err
			}
			message := received.message
			if !ready {
				if message.Envelope.Type != proto.PeerTypeHello || message.Envelope.WorkspaceID != "" {
					return errors.New("peer channel requires hello as its first message")
				}
				hello := message.Payload.(*proto.PeerHello)
				if hello.ClientID != peer.clientID {
					return errors.New("peer channel hello changed client identity")
				}
				peer.epoch = message.Envelope.Epoch
				summaries, requirements := peer.reconcileHelloWorkspaces(hello.Workspaces)
				ready = true
				if err := peer.enqueue(peerChannelOutbound{replyTo: message.Envelope.MessageID, messageType: proto.PeerTypeAcknowledgement, payload: proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK}}); err != nil {
					return err
				}
				if len(requirements) > 0 {
					required := proto.PeerStateRequired{Reason: "peer workspace state requires explicit reconciliation", Requirements: requirements, FullResync: true}
					if err := peer.enqueue(peerChannelOutbound{messageType: proto.PeerTypeStateRequired, payload: required}); err != nil {
						return err
					}
				}
				if err := peer.enqueue(peerChannelOutbound{messageType: proto.PeerTypeReady, payload: proto.PeerReady{LastReceivedSequence: peer.lastReceived.Load(), Workspaces: summaries}}); err != nil {
					return err
				}
				continue
			}
			if message.Envelope.Type == proto.PeerTypeHeartbeat {
				heartbeat := message.Payload.(*proto.PeerHeartbeat)
				previous := peer.lastHeartbeatSequence.Load()
				if heartbeat.LastReceivedSequence < previous || heartbeat.LastReceivedSequence > peer.lastSent.Load() {
					return errors.New("peer channel heartbeat reported an invalid received sequence")
				}
				peer.lastHeartbeatSequence.Store(heartbeat.LastReceivedSequence)
				peer.lastHeartbeat.Store(time.Now().UnixNano())
				continue
			}
			if message.Spec.Kind != proto.PeerMessageCommand {
				return errors.New("peer channel client sent an unsupported non-command message")
			}
			ack := peer.execute(message)
			if err := peer.enqueue(peerChannelOutbound{workspaceID: message.Envelope.WorkspaceID, replyTo: message.Envelope.MessageID, messageType: proto.PeerTypeAcknowledgement, payload: ack}); err != nil {
				return err
			}
		}
	}
}

func (peer *serverPeerChannel) execute(message proto.PeerDecodedMessage) proto.PeerAcknowledgement {
	peer.controller.peerExecutionMu.Lock()
	defer peer.controller.peerExecutionMu.Unlock()
	digest := sha256.Sum256(append([]byte(string(message.Envelope.Type)+"\x00"+message.Envelope.WorkspaceID+"\x00"), message.Envelope.Payload...))
	requestDigest := hex.EncodeToString(digest[:])
	key := peer.principal + "\x00" + peer.clientID + "\x00" + message.Envelope.WorkspaceID + "\x00" + message.Envelope.MessageID
	if ack, found, conflict := peer.controller.peerJournalLookup(key, requestDigest); found {
		return ack
	} else if conflict {
		return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusConflict, Message: "peer message ID was reused with different content"}
	}
	var ack proto.PeerAcknowledgement
	switch message.Envelope.Type {
	case proto.PeerTypeWorkspaceAttach:
		ack = peer.attach(message.Envelope.WorkspaceID, message.Payload.(*proto.PeerWorkspaceAttach).Attachment)
	case proto.PeerTypeWorkspaceDetach:
		ack = peer.detach(message.Envelope.WorkspaceID)
	case proto.PeerTypeSessionCurrentSet:
		selection := message.Payload.(*proto.CurrentSession)
		ack = peer.setCurrentSession(message.Envelope.WorkspaceID, *selection)
	case proto.PeerTypeModelSelectionSet:
		ack = peer.setModelSelection(message.Envelope.WorkspaceID, *message.Payload.(*proto.PeerModelSelectionSet))
	case proto.PeerTypeRuntimeControlsPatch:
		ack = peer.patchRuntimeControls(message.Envelope.WorkspaceID, *message.Payload.(*proto.PeerRuntimeControlsPatch))
	case proto.PeerTypeRuntimeTransaction:
		ack = peer.applyRuntimeTransaction(message.Envelope.WorkspaceID, *message.Payload.(*proto.PeerRuntimeTransaction))
	case proto.PeerTypeRuntimeReplace:
		ack = peer.replaceRuntime(message.Envelope.WorkspaceID, *message.Payload.(*proto.PeerRuntimeReplace))
	case proto.PeerTypeProviderRefreshCompleted:
		value := message.Payload.(*proto.PeerProviderRefreshCompletion)
		ack = peer.completeRefresh(message.Envelope.WorkspaceID, *value)
	default:
		ack = proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusInvalid, Message: "unsupported peer channel command"}
	}
	peer.controller.peerJournalStore(key, requestDigest, ack)
	return ack
}

func (peer *serverPeerChannel) reconcileHelloWorkspaces(requested []proto.PeerWorkspaceSummary) ([]proto.PeerWorkspaceSummary, []proto.PeerStateRequirement) {
	summaries := make([]proto.PeerWorkspaceSummary, 0, len(requested))
	var requirements []proto.PeerStateRequirement
	for _, candidate := range requested {
		workspace, err := peer.controller.backend.GetWorkspace(candidate.WorkspaceID)
		if err != nil || workspace.Cfg == nil {
			requirements = append(requirements, proto.PeerStateRequirement{Kind: "workspace", WorkspaceID: candidate.WorkspaceID})
			continue
		}
		authority := workspace.Cfg.RemoteAuthority()
		if authority == nil || authority.Mode != "client" || authority.Principal != peer.principal {
			requirements = append(requirements, proto.PeerStateRequirement{Kind: "runtime_full", WorkspaceID: candidate.WorkspaceID})
			continue
		}
		summaries = append(summaries, proto.PeerWorkspaceSummary{WorkspaceID: candidate.WorkspaceID, Revision: authority.Revision, Digest: authority.Digest})
	}
	return summaries, requirements
}

func (peer *serverPeerChannel) attach(workspaceID string, accepted *proto.WorkspaceAttachment) proto.PeerAcknowledgement {
	peer.attachmentsMu.Lock()
	if _, exists := peer.attachments[workspaceID]; exists {
		peer.attachmentsMu.Unlock()
		return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusConflict, Message: "workspace is already attached"}
	}
	peer.attachmentsMu.Unlock()
	ctx, cancel := context.WithCancel(peer.ctx)
	events, err := peer.controller.backend.SubscribeEvents(ctx, workspaceID)
	if err != nil {
		cancel()
		return peerAcknowledgementError(err)
	}
	if err := peer.controller.backend.AttachClientWithAuthority(workspaceID, peer.clientID, peer.principal, accepted); err != nil {
		cancel()
		return peerAcknowledgementError(err)
	}
	workspace, err := peer.controller.backend.GetWorkspace(workspaceID)
	if err != nil {
		cancel()
		peer.controller.backend.DetachClient(workspaceID, peer.clientID)
		return peerAcknowledgementError(err)
	}
	attachment := &serverPeerAttachment{workspace: workspace, cancel: cancel, authentication: map[providerauth.Owner]providerauth.Generation{}}
	peer.attachmentsMu.Lock()
	if peer.ctx.Err() != nil {
		peer.attachmentsMu.Unlock()
		attachment.once.Do(func() {
			cancel()
			peer.controller.backend.DetachClient(workspaceID, peer.clientID)
		})
		return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusUnavailable, Message: "peer channel is closing"}
	}
	peer.attachments[workspaceID] = attachment
	peer.attachmentsMu.Unlock()
	go peer.forwardEvents(ctx, workspaceID, events)
	if workspace.Cfg != nil && workspace.Cfg.RemoteAuthority() != nil {
		for _, request := range workspace.Cfg.PendingClientRefreshes() {
			if err := peer.forwardEvent(workspaceID, pubsub.Event[config.ClientRefreshRequest]{Type: pubsub.CreatedEvent, Payload: request}); err != nil {
				peer.cancel(err)
				break
			}
		}
	}
	if workspace.Cfg == nil {
		return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK}
	}
	return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK, Authority: workspace.Cfg.RemoteAuthority()}
}

func (peer *serverPeerChannel) detach(workspaceID string) proto.PeerAcknowledgement {
	peer.attachmentsMu.Lock()
	attachment := peer.attachments[workspaceID]
	delete(peer.attachments, workspaceID)
	peer.attachmentsMu.Unlock()
	if attachment == nil {
		return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusNotFound, Message: "workspace is not attached"}
	}
	attachment.once.Do(func() {
		attachment.cancel()
		peer.controller.backend.DetachClient(workspaceID, peer.clientID)
	})
	return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK}
}

func (peer *serverPeerChannel) setCurrentSession(workspaceID string, selection proto.CurrentSession) proto.PeerAcknowledgement {
	if peer.attachment(workspaceID) == nil {
		return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusConflict, Message: backend.ErrClientNotAttached.Error()}
	}
	if err := peer.controller.backend.SetCurrentSessionSelection(workspaceID, peer.clientID, selection); err != nil {
		return peerAcknowledgementError(err)
	}
	return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK}
}

func (peer *serverPeerChannel) setModelSelection(workspaceID string, request proto.PeerModelSelectionSet) proto.PeerAcknowledgement {
	attachment := peer.attachment(workspaceID)
	if attachment == nil || attachment.workspace.Cfg == nil {
		return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusUnavailable, Message: "workspace runtime is unavailable"}
	}
	selections := make(map[config.SelectedModelType]config.SelectedModel, len(request.Selections))
	for _, selection := range request.Selections {
		definition, owner, err := attachment.workspace.Cfg.RuntimeSnapshot().RetainedClientProviderDefinition(selection.Model.Provider.Owner.ProviderID)
		if err != nil {
			return peerAcknowledgementError(err)
		}
		digest, err := definition.Digest()
		if err != nil || digest != selection.Model.Provider.DefinitionDigest || definition.BundleDigest != selection.Model.Provider.BundleDigest || providerauth.PublicOwner(owner) != selection.Model.Provider.Owner {
			return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusConflict, Message: "provider reference does not match the accepted runtime"}
		}
		selections[selection.ModelType] = selection.Settings
	}
	base := request.Runtime
	ack, err := attachment.workspace.Cfg.PatchRemoteModelSelections(peer.ctx, peer.principal, base.ExpectedRevision, base.ExpectedDigest, base.ResultRevision, base.ResultDigest, selections)
	if err != nil {
		return peerAcknowledgementError(err)
	}
	for _, selection := range request.Selections {
		changed := proto.PeerModelSelectionChanged{Runtime: base, ModelType: selection.ModelType, Model: selection.Model}
		_ = peer.enqueue(peerChannelOutbound{workspaceID: workspaceID, messageType: proto.PeerTypeModelSelectionChanged, payload: changed})
	}
	_ = peer.enqueue(peerChannelOutbound{workspaceID: workspaceID, messageType: proto.PeerTypeRuntimePatchApplied, payload: proto.PeerRuntimePatchApplied{Revision: ack.Revision, Digest: ack.Digest}})
	attachment.workspace.SendEvent(pubsub.Event[proto.ConfigChanged]{Type: pubsub.UpdatedEvent, Payload: proto.ConfigChanged{WorkspaceID: workspaceID}})
	return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK, Authority: ack}
}

func (peer *serverPeerChannel) patchRuntimeControls(workspaceID string, request proto.PeerRuntimeControlsPatch) proto.PeerAcknowledgement {
	attachment := peer.attachment(workspaceID)
	if attachment == nil || attachment.workspace.Cfg == nil {
		return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusUnavailable, Message: "workspace runtime is unavailable"}
	}
	base := request.Runtime
	ack, err := attachment.workspace.Cfg.PatchRemoteRuntimeControls(peer.ctx, peer.principal, base.ExpectedRevision, base.ExpectedDigest, base.ResultRevision, base.ResultDigest, request.Controls)
	if err != nil {
		return peerAcknowledgementError(err)
	}
	_ = peer.enqueue(peerChannelOutbound{workspaceID: workspaceID, messageType: proto.PeerTypeRuntimePatchApplied, payload: proto.PeerRuntimePatchApplied{Revision: ack.Revision, Digest: ack.Digest}})
	attachment.workspace.SendEvent(pubsub.Event[proto.ConfigChanged]{Type: pubsub.UpdatedEvent, Payload: proto.ConfigChanged{WorkspaceID: workspaceID}})
	return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK, Authority: ack}
}

func (peer *serverPeerChannel) applyRuntimeTransaction(workspaceID string, request proto.PeerRuntimeTransaction) proto.PeerAcknowledgement {
	attachment := peer.attachment(workspaceID)
	if attachment == nil || attachment.workspace.Cfg == nil {
		return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusUnavailable, Message: "workspace runtime is unavailable"}
	}
	attachment.authMu.Lock()
	defer attachment.authMu.Unlock()
	transaction := config.RemoteRuntimeTransaction{}
	removed := map[string]bool{}
	introduced := map[string]proto.ProviderRef{}
	generations := map[providerauth.Owner]providerauth.Generation{}
	var availability []proto.PeerProviderAvailability
	var authentication []struct {
		messageType proto.PeerMessageType
		state       proto.PeerProviderAuthentication
	}
	var selections []proto.PeerModelSelectionOperation
	for _, operation := range request.Operations {
		switch operation.Type {
		case proto.PeerTypeProviderDefinitionRemove:
			provider := operation.DefinitionRemove.Provider
			owner, err := peer.acceptedProviderOwner(attachment, provider)
			if err != nil {
				return peerRuntimeTransactionAcknowledgementError(err)
			}
			transaction.DefinitionRemovals = append(transaction.DefinitionRemovals, owner)
			removed[provider.Owner.ProviderID] = true
		case proto.PeerTypeProviderDefinitionPut:
			update := operation.DefinitionPut
			transaction.DefinitionPuts = append(transaction.DefinitionPuts, config.RemoteProviderDefinitionPut{Definition: update.Definition, Bundle: update.Bundle, ContextInstruction: update.ContextInstruction})
			introduced[update.Provider.Owner.ProviderID] = update.Provider
		case proto.PeerTypeProviderCredentialReplace:
			update := operation.CredentialReplace
			config.RegisterRemoteCredentialSecrets(update.Credential)
			if err := peer.validateTransactionProvider(attachment, update.Provider, removed, introduced); err != nil {
				return peerRuntimeTransactionAcknowledgementError(err)
			}
			transaction.CredentialReplacements = append(transaction.CredentialReplacements, update.Credential)
			if err := collectPeerAuthenticationGeneration(generations, update.Provider.Owner, update.Generation); err != nil {
				return peerRuntimeTransactionAcknowledgementError(err)
			}
		case proto.PeerTypeProviderCredentialInvalidate:
			update := operation.CredentialInvalidate
			owner, err := peer.acceptedProviderOwner(attachment, update.Provider)
			if err != nil || removed[update.Provider.Owner.ProviderID] {
				return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusConflict, Message: "provider credential invalidation does not match the accepted runtime"}
			}
			transaction.CredentialInvalidations = append(transaction.CredentialInvalidations, owner)
			if err := collectPeerAuthenticationGeneration(generations, update.Provider.Owner, update.Generation); err != nil {
				return peerRuntimeTransactionAcknowledgementError(err)
			}
		case proto.PeerTypeProviderAvailability:
			state := *operation.Availability
			if err := peer.validateTransactionProvider(attachment, state.Provider, removed, introduced); err != nil {
				return peerRuntimeTransactionAcknowledgementError(err)
			}
			if err := collectPeerAuthenticationGeneration(generations, state.Provider.Owner, state.Generation); err != nil {
				return peerRuntimeTransactionAcknowledgementError(err)
			}
			transaction.Advance = true
			availability = append(availability, state)
		case proto.PeerTypeProviderAuthChanged, proto.PeerTypeProviderAuthInvalidated:
			state := *operation.Authentication
			if err := peer.validateTransactionProvider(attachment, state.Provider, removed, introduced); err != nil {
				return peerRuntimeTransactionAcknowledgementError(err)
			}
			if err := collectPeerAuthenticationGeneration(generations, state.Provider.Owner, state.Generation); err != nil {
				return peerRuntimeTransactionAcknowledgementError(err)
			}
			transaction.Advance = true
			authentication = append(authentication, struct {
				messageType proto.PeerMessageType
				state       proto.PeerProviderAuthentication
			}{messageType: operation.Type, state: state})
		case proto.PeerTypeModelSelectionSet:
			selection := *operation.ModelSelection
			if err := peer.validateTransactionProvider(attachment, selection.Model.Provider, removed, introduced); err != nil {
				return peerRuntimeTransactionAcknowledgementError(err)
			}
			if transaction.ModelSelections == nil {
				transaction.ModelSelections = map[config.SelectedModelType]config.SelectedModel{}
			}
			transaction.ModelSelections[selection.ModelType] = selection.Settings
			selections = append(selections, selection)
		case proto.PeerTypeRuntimeControlsPatch:
			controls := *operation.Controls
			transaction.Controls = &controls
		}
	}
	for owner, generation := range generations {
		if current, found := attachment.authentication[owner]; found && (generation.Epoch != current.Epoch || generation.Sequence <= current.Sequence) {
			return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusConflict, Message: "provider authentication generation is stale"}
		}
	}
	base := request.Runtime
	ack, err := attachment.workspace.Cfg.PatchRemoteRuntime(peer.ctx, peer.principal, base.ExpectedRevision, base.ExpectedDigest, base.ResultRevision, base.ResultDigest, transaction)
	if err != nil {
		return peerRuntimeTransactionAcknowledgementError(err)
	}
	for owner, generation := range generations {
		attachment.authentication[owner] = generation
	}
	for _, state := range availability {
		_ = peer.enqueue(peerChannelOutbound{workspaceID: workspaceID, messageType: proto.PeerTypeProviderAvailability, payload: state})
	}
	for _, state := range authentication {
		_ = peer.enqueue(peerChannelOutbound{workspaceID: workspaceID, messageType: state.messageType, payload: state.state})
	}
	for _, selection := range selections {
		changed := proto.PeerModelSelectionChanged{Runtime: base, ModelType: selection.ModelType, Model: selection.Model}
		_ = peer.enqueue(peerChannelOutbound{workspaceID: workspaceID, messageType: proto.PeerTypeModelSelectionChanged, payload: changed})
	}
	_ = peer.enqueue(peerChannelOutbound{workspaceID: workspaceID, messageType: proto.PeerTypeRuntimePatchApplied, payload: proto.PeerRuntimePatchApplied{Revision: ack.Revision, Digest: ack.Digest}})
	attachment.workspace.SendEvent(pubsub.Event[proto.ConfigChanged]{Type: pubsub.UpdatedEvent, Payload: proto.ConfigChanged{WorkspaceID: workspaceID}})
	return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK, Authority: ack}
}

func (peer *serverPeerChannel) acceptedProviderOwner(attachment *serverPeerAttachment, provider proto.ProviderRef) (providerregistry.RegistrationOwner, error) {
	definition, owner, err := attachment.workspace.Cfg.RuntimeSnapshot().RetainedClientProviderDefinition(provider.Owner.ProviderID)
	if err != nil {
		return providerregistry.RegistrationOwner{}, err
	}
	digest, err := proto.ProviderDefinitionDigest(definition)
	if err != nil || digest != provider.DefinitionDigest || definition.BundleDigest != provider.BundleDigest || providerauth.PublicOwner(owner) != provider.Owner {
		return providerregistry.RegistrationOwner{}, errors.New("provider reference does not match the accepted runtime")
	}
	return owner, nil
}

func (peer *serverPeerChannel) validateTransactionProvider(attachment *serverPeerAttachment, provider proto.ProviderRef, removed map[string]bool, introduced map[string]proto.ProviderRef) error {
	providerID := provider.Owner.ProviderID
	if introduced[providerID] == provider {
		return nil
	}
	if removed[providerID] {
		return errors.New("provider reference was removed from the runtime transaction")
	}
	_, err := peer.acceptedProviderOwner(attachment, provider)
	return err
}

func collectPeerAuthenticationGeneration(values map[providerauth.Owner]providerauth.Generation, owner providerauth.Owner, generation providerauth.Generation) error {
	if current, found := values[owner]; found && current != generation {
		return errors.New("provider authentication generation changed within one runtime transaction")
	}
	values[owner] = generation
	return nil
}

func (peer *serverPeerChannel) attachment(workspaceID string) *serverPeerAttachment {
	peer.attachmentsMu.Lock()
	defer peer.attachmentsMu.Unlock()
	return peer.attachments[workspaceID]
}

func (peer *serverPeerChannel) replaceRuntime(workspaceID string, request proto.PeerRuntimeReplace) proto.PeerAcknowledgement {
	if !request.ExplicitRecovery {
		return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusInvalid, Message: "complete runtime replacement requires explicit recovery"}
	}
	peer.attachmentsMu.Lock()
	attachment := peer.attachments[workspaceID]
	peer.attachmentsMu.Unlock()
	if attachment == nil || attachment.workspace.Cfg == nil {
		return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusUnavailable, Message: "workspace runtime is unavailable"}
	}
	ack, err := attachment.workspace.Cfg.ReplaceRemoteRuntime(peer.ctx, request.Runtime, peer.principal, request.ExpectedRevision)
	if err != nil {
		return peerAcknowledgementError(err)
	}
	attachment.workspace.SendEvent(pubsub.Event[proto.ConfigChanged]{Type: pubsub.UpdatedEvent, Payload: proto.ConfigChanged{WorkspaceID: workspaceID}})
	return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK, Authority: ack}
}

func (peer *serverPeerChannel) completeRefresh(workspaceID string, request proto.PeerProviderRefreshCompletion) proto.PeerAcknowledgement {
	peer.attachmentsMu.Lock()
	attachment := peer.attachments[workspaceID]
	peer.attachmentsMu.Unlock()
	if attachment == nil || attachment.workspace.Cfg == nil {
		return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusUnavailable, Message: "workspace runtime is unavailable"}
	}
	completion := config.ClientRefreshCompletion{RequestID: request.RequestID, Revision: request.Revision, Digest: request.Digest, CredentialID: request.CredentialID, Failed: request.Failed}
	if err := attachment.workspace.Cfg.CompleteClientRefresh(peer.principal, completion); err != nil {
		return peerAcknowledgementError(err)
	}
	return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK}
}

func (peer *serverPeerChannel) forwardEvents(ctx context.Context, workspaceID string, events <-chan pubsub.Event[tea.Msg]) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, open := <-events:
			if !open {
				return
			}
			if err := peer.forwardEvent(workspaceID, event.Payload); err != nil {
				peer.cancel(err)
				return
			}
		}
	}
}

func (peer *serverPeerChannel) forwardEvent(workspaceID string, event any) error {
	if refresh, ok := event.(pubsub.Event[config.ClientRefreshRequest]); ok {
		request := refresh.Payload
		payload := proto.PeerProviderRefreshRequest{
			Provider:  proto.ProviderRef{Owner: providerauth.PublicOwner(request.Owner), DefinitionDigest: request.DefinitionDigest, BundleDigest: request.BundleDigest},
			RequestID: request.ID, Revision: request.Revision, Digest: request.Digest, Deadline: request.Deadline,
		}
		return peer.enqueue(peerChannelOutbound{workspaceID: workspaceID, messageType: proto.PeerTypeProviderRefreshRequired, payload: payload})
	}
	wrapped := wrapEvent(event)
	if wrapped == nil {
		return errors.New("workspace event has no registered peer message type")
	}
	messageType, err := peerEventMessageType(*wrapped)
	if err != nil {
		return err
	}
	payload, err := proto.DecodePeerPayload(messageType, wrapped.Payload)
	if err != nil {
		return err
	}
	return peer.enqueue(peerChannelOutbound{workspaceID: workspaceID, messageType: messageType, payload: payload})
}

func peerEventMessageType(payload pubsub.Payload) (proto.PeerMessageType, error) {
	switch payload.Type {
	case pubsub.PayloadTypeLSPEvent:
		return proto.PeerTypeEventLSP, nil
	case pubsub.PayloadTypeMCPEvent:
		return proto.PeerTypeEventMCP, nil
	case pubsub.PayloadTypePermissionRequest:
		return proto.PeerTypeEventPermissionRequest, nil
	case pubsub.PayloadTypePermissionNotification:
		return proto.PeerTypeEventPermissionResult, nil
	case pubsub.PayloadTypeQuestionRequest:
		return proto.PeerTypeEventQuestionRequest, nil
	case pubsub.PayloadTypeQuestionNotification:
		return proto.PeerTypeEventQuestionResult, nil
	case pubsub.PayloadTypeMessage:
		return proto.PeerTypeEventMessage, nil
	case pubsub.PayloadTypeSession:
		return proto.PeerTypeEventSession, nil
	case pubsub.PayloadTypeFile:
		return proto.PeerTypeEventFile, nil
	case pubsub.PayloadTypeAgentEvent:
		return proto.PeerTypeEventAgent, nil
	case pubsub.PayloadTypeConfigChanged:
		return proto.PeerTypeEventConfigChanged, nil
	case pubsub.PayloadTypeSkillsEvent:
		return proto.PeerTypeEventSkills, nil
	case pubsub.PayloadTypeTaskNotification:
		return proto.PeerTypeEventTask, nil
	case pubsub.PayloadTypeRunComplete:
		var event proto.PeerResourceEvent[proto.RunComplete]
		if err := json.Unmarshal(payload.Payload, &event); err != nil {
			return "", err
		}
		if event.Payload.Cancelled {
			return proto.PeerTypeRunCancelled, nil
		}
		if event.Payload.Error != "" {
			return proto.PeerTypeRunFailed, nil
		}
		return proto.PeerTypeRunCompleted, nil
	default:
		return "", errors.New("workspace event type is not registered on the peer channel")
	}
}

func (peer *serverPeerChannel) enqueue(message peerChannelOutbound) error {
	if message.controlType != 0 {
		select {
		case peer.critical <- message:
			return nil
		default:
			err := errors.New("peer channel delivery queue is exhausted")
			peer.cancel(err)
			return err
		}
	}
	spec, ok := proto.PeerMessageSpecification(message.messageType)
	if !ok {
		return errors.New("peer message type is not registered")
	}
	queue := peer.workspace
	switch spec.Delivery {
	case proto.PeerDeliveryCritical:
		queue = peer.critical
	case proto.PeerDeliveryState:
		queue = peer.state
	case proto.PeerDeliveryTelemetry:
		select {
		case peer.telemetry <- message:
		default:
		}
		return nil
	}
	select {
	case queue <- message:
		return nil
	default:
		err := errors.New("peer channel delivery queue is exhausted")
		peer.cancel(err)
		return err
	}
}

func (peer *serverPeerChannel) readLoop(results chan<- peerChannelInbound) {
	var epoch string
	var sequence uint64
	for {
		messageType, data, err := peer.connection.ReadMessage()
		if err == nil && messageType != websocket.TextMessage {
			err = errors.New("peer channel accepts text frames only")
		}
		var message proto.PeerDecodedMessage
		if err == nil {
			message, err = proto.DecodePeerMessage(data, proto.PeerDirectionClientToServer)
		}
		if err == nil {
			if epoch == "" {
				epoch = message.Envelope.Epoch
			} else if message.Envelope.Epoch != epoch {
				err = errors.New("peer channel epoch changed")
			}
			if err == nil && message.Envelope.Sequence != sequence+1 {
				err = errors.New("peer channel sequence regressed or skipped")
			}
		}
		if err == nil {
			sequence = message.Envelope.Sequence
			peer.lastReceived.Store(sequence)
		}
		select {
		case results <- peerChannelInbound{message: message, err: err}:
		case <-peer.ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func (peer *serverPeerChannel) writeLoop() error {
	pings := time.NewTicker(peerChannelPingInterval)
	heartbeats := time.NewTicker(peerChannelPingInterval)
	defer pings.Stop()
	defer heartbeats.Stop()
	var sequence uint64
	for {
		message, ok := peer.nextOutbound(pings.C, heartbeats.C)
		if !ok {
			return context.Cause(peer.ctx)
		}
		if message.controlType != 0 {
			if err := peer.connection.WriteControl(message.controlType, message.controlData, time.Now().Add(peerChannelWriteTimeout)); err != nil {
				return err
			}
			continue
		}
		sequence++
		data, err := proto.EncodePeerMessage(peer.epoch, sequence, uuid.NewString(), message.replyTo, message.workspaceID, message.messageType, message.payload)
		if err == nil {
			err = peer.connection.SetWriteDeadline(time.Now().Add(peerChannelWriteTimeout))
		}
		if err == nil {
			peer.lastSent.Store(sequence)
			err = peer.connection.WriteMessage(websocket.TextMessage, data)
		}
		if err != nil {
			return err
		}
	}
}

func (peer *serverPeerChannel) nextOutbound(pings, heartbeats <-chan time.Time) (peerChannelOutbound, bool) {
	for _, queue := range []<-chan peerChannelOutbound{peer.critical, peer.state, peer.workspace} {
		select {
		case message := <-queue:
			return message, true
		default:
		}
	}
	select {
	case <-peer.ctx.Done():
		return peerChannelOutbound{}, false
	case message := <-peer.critical:
		return message, true
	case message := <-peer.state:
		return message, true
	case message := <-peer.workspace:
		return message, true
	case message := <-peer.telemetry:
		return message, true
	case <-pings:
		if time.Since(time.Unix(0, peer.lastHeartbeat.Load())) > peerChannelPongTimeout {
			peer.cancel(errors.New("peer channel application heartbeat stalled"))
			return peerChannelOutbound{}, false
		}
		return peerChannelOutbound{controlType: websocket.PingMessage}, true
	case <-heartbeats:
		return peerChannelOutbound{messageType: proto.PeerTypeHeartbeat, payload: proto.PeerHeartbeat{LastReceivedSequence: peer.lastReceived.Load(), Workspaces: peer.workspaceSummaries()}}, true
	}
}

func (peer *serverPeerChannel) workspaceSummaries() []proto.PeerWorkspaceSummary {
	peer.attachmentsMu.Lock()
	defer peer.attachmentsMu.Unlock()
	result := make([]proto.PeerWorkspaceSummary, 0, len(peer.attachments))
	for id, attachment := range peer.attachments {
		if attachment.workspace.Cfg == nil {
			continue
		}
		authority := attachment.workspace.Cfg.RemoteAuthority()
		if authority == nil || authority.Mode != "client" {
			continue
		}
		result = append(result, proto.PeerWorkspaceSummary{WorkspaceID: id, Revision: authority.Revision, Digest: authority.Digest})
	}
	return result
}

func (peer *serverPeerChannel) close() {
	peer.cancel(errors.New("peer channel closed"))
	_ = peer.connection.Close()
	peer.attachmentsMu.Lock()
	attachments := peer.attachments
	peer.attachments = map[string]*serverPeerAttachment{}
	peer.attachmentsMu.Unlock()
	for id, attachment := range attachments {
		workspaceID := id
		attachment.once.Do(func() {
			attachment.cancel()
			peer.controller.backend.DetachClient(workspaceID, peer.clientID)
		})
	}
}

func (c *controllerV1) peerJournalLookup(key, digest string) (proto.PeerAcknowledgement, bool, bool) {
	c.peerJournalMu.Lock()
	defer c.peerJournalMu.Unlock()
	now := time.Now()
	for entryKey, entry := range c.peerJournal {
		if now.Sub(entry.created) > peerJournalTTL {
			delete(c.peerJournal, entryKey)
		}
	}
	entry, ok := c.peerJournal[key]
	if !ok {
		return proto.PeerAcknowledgement{}, false, false
	}
	if entry.digest != digest {
		return proto.PeerAcknowledgement{}, false, true
	}
	return entry.ack, true, false
}

func (c *controllerV1) peerJournalStore(key, digest string, ack proto.PeerAcknowledgement) {
	c.peerJournalMu.Lock()
	defer c.peerJournalMu.Unlock()
	if c.peerJournal == nil {
		c.peerJournal = map[string]peerJournalEntry{}
	}
	if len(c.peerJournal) >= peerJournalLimit {
		var oldestKey string
		var oldest time.Time
		for entryKey, entry := range c.peerJournal {
			if oldestKey == "" || entry.created.Before(oldest) {
				oldestKey, oldest = entryKey, entry.created
			}
		}
		delete(c.peerJournal, oldestKey)
	}
	c.peerJournal[key] = peerJournalEntry{digest: digest, ack: ack, created: time.Now()}
}

func peerRuntimeTransactionAcknowledgementError(err error) proto.PeerAcknowledgement {
	acknowledgement := peerAcknowledgementError(err)
	if acknowledgement.Status == proto.WorkspaceChannelStatusInvalid {
		acknowledgement.Message = redact.String(err.Error())
		if acknowledgement.Message == "" {
			acknowledgement.Message = "incremental runtime transaction is invalid"
		}
	}
	return acknowledgement
}

func peerAcknowledgementError(err error) proto.PeerAcknowledgement {
	status, message := proto.WorkspaceChannelStatusInvalid, "peer channel command is invalid"
	switch {
	case errors.Is(err, backend.ErrRuntimeConflict):
		status, message = proto.WorkspaceChannelStatusConflict, backend.ErrRuntimeConflict.Error()
	case errors.Is(err, backend.ErrSessionSelectionConflict), errors.Is(err, backend.ErrClientNotAttached):
		status, message = proto.WorkspaceChannelStatusConflict, err.Error()
	case errors.Is(err, backend.ErrSessionSelectionInvalid), errors.Is(err, backend.ErrInvalidClientID):
		status, message = proto.WorkspaceChannelStatusInvalid, err.Error()
	case errors.Is(err, config.ErrRemoteRuntimeRevision):
		status, message = proto.WorkspaceChannelStatusConflict, "workspace runtime revision changed"
	case errors.Is(err, config.ErrRuntimeRevoked), errors.Is(err, backend.ErrWorkspaceAuthority):
		status, message = proto.WorkspaceChannelStatusForbidden, "workspace runtime authority was revoked"
	case errors.Is(err, backend.ErrWorkspaceNotFound):
		status, message = proto.WorkspaceChannelStatusNotFound, "workspace is unavailable"
	case errors.Is(err, backend.ErrWorkspaceClosing), errors.Is(err, backend.ErrServerShuttingDown):
		status, message = proto.WorkspaceChannelStatusUnavailable, "workspace is unavailable"
	}
	return proto.PeerAcknowledgement{Status: status, Message: message}
}
