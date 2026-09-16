package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	stdpath "path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	peerChannelClientWriteTimeout = 10 * time.Second
	peerChannelClientPongTimeout  = 60 * time.Second
	peerChannelClientPingInterval = 30 * time.Second
)

type peerChannelWrite struct {
	workspaceID string
	messageID   string
	replyTo     string
	messageType proto.PeerMessageType
	payload     any
	controlType int
	controlData []byte
	written     chan error
}

type peerChannelPending struct {
	result chan proto.PeerAcknowledgement
}

type peerChannel struct {
	client                *Client
	conn                  *websocket.Conn
	ctx                   context.Context
	cancel                context.CancelCauseFunc
	done                  chan struct{}
	ready                 chan struct{}
	closeOnce             sync.Once
	readyOnce             sync.Once
	epoch                 string
	critical              chan peerChannelWrite
	state                 chan peerChannelWrite
	workspace             chan peerChannelWrite
	telemetry             chan peerChannelWrite
	mu                    sync.Mutex
	err                   error
	pending               map[string]peerChannelPending
	serverWorkspaces      map[string]proto.PeerWorkspaceSummary
	stateRequirements     map[string]proto.PeerStateRequirement
	lastReceived          atomic.Uint64
	lastSent              atomic.Uint64
	lastHeartbeat         atomic.Int64
	lastHeartbeatSequence atomic.Uint64
}

type workspaceChannel struct {
	client        *Client
	peer          *peerChannel
	id            string
	mu            sync.Mutex
	subs          map[string]chan any
	pendingEvents []any
	pending       int
	attachDone    chan struct{}
	attachErr     error
	closed        bool
	closeOnce     sync.Once
}

type WorkspaceChannelCommandError struct {
	Status  proto.WorkspaceChannelStatus
	Message string
}

func (e *WorkspaceChannelCommandError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "peer channel command failed: " + string(e.Status)
}

func (e *WorkspaceChannelCommandError) Unwrap() error {
	if e.Status == proto.WorkspaceChannelStatusNotFound {
		return ErrNotFound
	}
	return nil
}

func (c *Client) getWorkspaceChannel(ctx context.Context, id string, authority ...config.RemoteAuthority) (*workspaceChannel, error) {
	accepted, err := c.workspaceAttachment(id, authority)
	if err != nil {
		return nil, err
	}
	c.channelsMu.Lock()
	if c.retired {
		c.channelsMu.Unlock()
		return nil, errors.New("client has been retired")
	}
	channel := c.channels[id]
	c.channelsMu.Unlock()
	if channel != nil {
		return awaitWorkspaceChannel(ctx, channel)
	}
	peer, err := c.ensurePeerChannel(ctx)
	if err != nil {
		return nil, err
	}
	if accepted != nil && accepted.Mode == "client" {
		summary, err := peer.workspaceSummary(id)
		if err != nil {
			return nil, err
		}
		if summary.Revision != accepted.Revision || summary.Digest != accepted.Digest {
			return nil, errors.New("peer channel receiver has a different accepted runtime than the retained client authority")
		}
	}
	c.channelsMu.Lock()
	if c.retired {
		c.channelsMu.Unlock()
		return nil, errors.New("client has been retired")
	}
	if channel = c.channels[id]; channel != nil {
		c.channelsMu.Unlock()
		return awaitWorkspaceChannel(ctx, channel)
	}
	channel = &workspaceChannel{client: c, peer: peer, id: id, subs: map[string]chan any{}, attachDone: make(chan struct{})}
	if c.channels == nil {
		c.channels = map[string]*workspaceChannel{}
	}
	c.channels[id] = channel
	c.channelsMu.Unlock()

	ack, err := peer.command(ctx, id, proto.PeerTypeWorkspaceAttach, proto.PeerWorkspaceAttach{Attachment: accepted})
	if err == nil && accepted != nil && ack.Authority != nil && (ack.Authority.Mode != accepted.Mode || ack.Authority.Revision != accepted.Revision || ack.Authority.Digest != accepted.Digest) {
		err = errors.New("peer channel acknowledged a different workspace authority")
		peer.close(err)
	}
	channel.mu.Lock()
	channel.attachErr = err
	if err != nil {
		channel.closed = true
	}
	close(channel.attachDone)
	channel.mu.Unlock()
	if err != nil {
		c.channelsMu.Lock()
		if c.channels[id] == channel {
			delete(c.channels, id)
		}
		c.channelsMu.Unlock()
		if peer.terminalError() != nil {
			peer.close(err)
		}
		return nil, err
	}
	return channel, nil
}

func awaitWorkspaceChannel(ctx context.Context, channel *workspaceChannel) (*workspaceChannel, error) {
	select {
	case <-channel.attachDone:
		channel.mu.Lock()
		err := channel.attachErr
		channel.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return channel, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Client) ensurePeerChannel(ctx context.Context) (*peerChannel, error) {
	c.peerOpenMu.Lock()
	defer c.peerOpenMu.Unlock()
	c.channelsMu.Lock()
	if c.retired {
		c.channelsMu.Unlock()
		return nil, errors.New("client has been retired")
	}
	peer := c.peer
	c.channelsMu.Unlock()
	if peer != nil {
		return peer, nil
	}
	peer, err := c.openPeerChannel(ctx)
	if err != nil {
		return nil, err
	}
	c.channelsMu.Lock()
	if c.retired {
		c.channelsMu.Unlock()
		peer.close(errors.New("client has been retired"))
		return nil, errors.New("client has been retired")
	}
	c.peer = peer
	c.channelsMu.Unlock()
	return peer, nil
}

func (c *Client) openPeerChannel(ctx context.Context) (*peerChannel, error) {
	connection, err := c.dialPeerChannel(ctx)
	if err != nil {
		return nil, err
	}
	channelCtx, cancel := context.WithCancelCause(context.Background())
	peer := &peerChannel{
		client: c, conn: connection, ctx: channelCtx, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}), epoch: strings.ReplaceAll(uuid.NewString(), "-", ""),
		critical: make(chan peerChannelWrite, 64), state: make(chan peerChannelWrite, 128), workspace: make(chan peerChannelWrite, 512), telemetry: make(chan peerChannelWrite, 64),
		pending: map[string]peerChannelPending{}, serverWorkspaces: map[string]proto.PeerWorkspaceSummary{}, stateRequirements: map[string]proto.PeerStateRequirement{},
	}
	peer.lastHeartbeat.Store(time.Now().UnixNano())
	connection.SetReadLimit(proto.MaxPeerChannelEnvelopeBytes)
	_ = connection.SetReadDeadline(time.Now().Add(peerChannelClientPongTimeout))
	connection.SetPingHandler(func(data string) error {
		return peer.enqueue(peerChannelWrite{controlType: websocket.PongMessage, controlData: []byte(data)})
	})
	connection.SetPongHandler(func(string) error {
		return connection.SetReadDeadline(time.Now().Add(peerChannelClientPongTimeout))
	})
	go peer.runWriter()
	go peer.runReader()
	if _, err := peer.command(ctx, "", proto.PeerTypeHello, proto.PeerHello{ClientID: c.clientID, Workspaces: c.workspaceSummaries()}); err != nil {
		peer.close(err)
		return nil, err
	}
	select {
	case <-peer.ready:
		return peer, nil
	case <-ctx.Done():
		peer.close(ctx.Err())
		return nil, ctx.Err()
	case <-peer.done:
		return nil, peer.terminalError()
	}
}

func (c *Client) dialPeerChannel(ctx context.Context) (*websocket.Conn, error) {
	if !c.secure && c.network != "unix" && c.network != "npipe" {
		return nil, errors.New("peer channel refuses insecure TCP; use mutually authenticated TLS or a local socket")
	}
	scheme, host := "ws", c.addr
	if c.secure {
		scheme = "wss"
	}
	if c.network == "unix" || c.network == "npipe" {
		host = DummyHost
	}
	query := url.Values{"client_id": []string{c.clientID}}
	endpoint := (&url.URL{Scheme: scheme, Host: host, Path: stdpath.Join("/v1/peer-channel"), RawQuery: query.Encode()}).String()
	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second, NetDialContext: c.dialer, Subprotocols: []string{proto.PeerChannelProtocol}, TLSClientConfig: c.workspaceChannelTLSConfig()}
	connection, response, err := dialer.DialContext(ctx, endpoint, runtimeHeaders())
	if err != nil {
		if response != nil {
			defer response.Body.Close()
			if statusErr := checkStatus(response); statusErr != nil {
				return nil, fmt.Errorf("peer channel upgrade failed: %w", statusErr)
			}
		}
		return nil, fmt.Errorf("peer channel upgrade failed: %w", err)
	}
	if connection.Subprotocol() != proto.PeerChannelProtocol {
		_ = connection.Close()
		return nil, errors.New("peer channel protocol was not acknowledged")
	}
	return connection, nil
}

func (c *Client) workspaceChannelTLSConfig() *tls.Config {
	if c.tlsConfig != nil {
		return c.tlsConfig.Clone()
	}
	if c.h != nil {
		if transport, ok := c.h.Transport.(*http.Transport); ok && transport.TLSClientConfig != nil {
			return transport.TLSClientConfig.Clone()
		}
	}
	return nil
}

func (c *Client) closeWorkspaceChannel(id string) {
	c.channelsMu.Lock()
	channel := c.channels[id]
	c.channelsMu.Unlock()
	if channel != nil {
		channel.close()
	}
}

func (c *Client) closeWorkspaceChannels(err error) {
	c.channelsMu.Lock()
	peer := c.peer
	c.peer = nil
	c.retired = true
	c.channelsMu.Unlock()
	if peer != nil {
		peer.close(err)
	}
}

func (c *Client) sendWorkspaceChannelCommand(ctx context.Context, id string, frame proto.WorkspaceChannelFrame) (proto.WorkspaceChannelAcknowledgement, error) {
	channel, err := c.getWorkspaceChannel(ctx, id)
	if err != nil {
		return proto.WorkspaceChannelAcknowledgement{}, err
	}
	var messageType proto.PeerMessageType
	var payload any
	switch frame.Type {
	case proto.WorkspaceChannelRuntimeReplaceFrame:
		messageType = proto.PeerTypeRuntimeReplace
		payload = proto.PeerRuntimeReplace{ExpectedRevision: frame.RuntimeReplace.ExpectedRevision, Runtime: frame.RuntimeReplace.Runtime}
	case proto.WorkspaceChannelRefreshCompleteFrame:
		messageType = proto.PeerTypeProviderRefreshCompleted
		payload = proto.PeerProviderRefreshCompletion{RequestID: frame.RefreshComplete.RequestID, Revision: frame.RefreshComplete.Revision, Digest: frame.RefreshComplete.Digest, CredentialID: frame.RefreshComplete.CredentialID, Failed: frame.RefreshComplete.Failed}
	default:
		return proto.WorkspaceChannelAcknowledgement{}, errors.New("workspace command has no peer channel mapping")
	}
	channel.mu.Lock()
	if channel.closed {
		channel.mu.Unlock()
		return proto.WorkspaceChannelAcknowledgement{}, errors.New("workspace peer attachment is closed")
	}
	channel.pending++
	channel.mu.Unlock()
	defer func() {
		channel.mu.Lock()
		channel.pending--
		channel.mu.Unlock()
	}()
	ack, err := channel.peer.command(ctx, id, messageType, payload)
	return proto.WorkspaceChannelAcknowledgement(ack), err
}

func (peer *peerChannel) command(ctx context.Context, workspaceID string, messageType proto.PeerMessageType, payload any) (proto.PeerAcknowledgement, error) {
	return peer.commandWithID(ctx, workspaceID, uuid.NewString(), messageType, payload)
}

func (peer *peerChannel) commandWithID(ctx context.Context, workspaceID, messageID string, messageType proto.PeerMessageType, payload any) (proto.PeerAcknowledgement, error) {
	pending := peerChannelPending{result: make(chan proto.PeerAcknowledgement, 1)}
	peer.mu.Lock()
	if peer.err != nil {
		err := peer.err
		peer.mu.Unlock()
		return proto.PeerAcknowledgement{}, err
	}
	peer.pending[messageID] = pending
	peer.mu.Unlock()
	defer func() {
		peer.mu.Lock()
		delete(peer.pending, messageID)
		peer.mu.Unlock()
	}()
	written := make(chan error, 1)
	if err := peer.enqueue(peerChannelWrite{workspaceID: workspaceID, messageID: messageID, messageType: messageType, payload: payload, written: written}); err != nil {
		return proto.PeerAcknowledgement{}, err
	}
	select {
	case err := <-written:
		if err != nil {
			return proto.PeerAcknowledgement{}, err
		}
	case <-ctx.Done():
		return proto.PeerAcknowledgement{}, ctx.Err()
	case <-peer.done:
		return proto.PeerAcknowledgement{}, peer.terminalError()
	}
	select {
	case acknowledgement := <-pending.result:
		if acknowledgement.Status != proto.WorkspaceChannelStatusOK {
			return acknowledgement, &WorkspaceChannelCommandError{Status: acknowledgement.Status, Message: acknowledgement.Message}
		}
		if acknowledgement.Authority != nil && workspaceID != "" {
			peer.updateWorkspaceSummary(workspaceID, acknowledgement.Authority.Revision, acknowledgement.Authority.Digest)
		}
		return acknowledgement, nil
	case <-ctx.Done():
		return proto.PeerAcknowledgement{}, ctx.Err()
	case <-peer.done:
		return proto.PeerAcknowledgement{}, peer.terminalError()
	}
}

func (peer *peerChannel) enqueue(message peerChannelWrite) error {
	if message.controlType != 0 {
		select {
		case peer.critical <- message:
			return nil
		default:
			err := errors.New("peer channel delivery queue is exhausted")
			peer.close(err)
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
		peer.close(err)
		return err
	}
}

func (channel *workspaceChannel) subscribe(ctx context.Context) <-chan any {
	id := uuid.NewString()
	events := make(chan any, 100)
	channel.mu.Lock()
	if channel.closed {
		close(events)
		channel.mu.Unlock()
		return events
	}
	for _, event := range channel.pendingEvents {
		events <- event
	}
	channel.pendingEvents = nil
	channel.subs[id] = events
	channel.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
		case <-channel.peer.done:
		}
		channel.removeSubscriber(id)
	}()
	return events
}

func (channel *workspaceChannel) removeSubscriber(id string) {
	channel.mu.Lock()
	if events, ok := channel.subs[id]; ok {
		delete(channel.subs, id)
		close(events)
	}
	idle := len(channel.subs) == 0 && channel.pending == 0 && !channel.closed
	channel.mu.Unlock()
	if idle {
		channel.close()
	}
}

func (channel *workspaceChannel) publish(event any) error {
	channel.mu.Lock()
	defer channel.mu.Unlock()
	if channel.closed {
		return errors.New("workspace peer attachment is closed")
	}
	if len(channel.subs) == 0 {
		if len(channel.pendingEvents) >= 100 {
			return errors.New("workspace event attachment queue is exhausted")
		}
		channel.pendingEvents = append(channel.pendingEvents, event)
		return nil
	}
	for _, events := range channel.subs {
		select {
		case events <- event:
		default:
			return errors.New("workspace event subscriber queue is exhausted")
		}
	}
	return nil
}

func (channel *workspaceChannel) close() {
	channel.closeOnce.Do(func() {
		channel.mu.Lock()
		channel.closed = true
		for id, events := range channel.subs {
			delete(channel.subs, id)
			close(events)
		}
		channel.mu.Unlock()
		channel.client.channelsMu.Lock()
		if channel.client.channels[channel.id] == channel {
			delete(channel.client.channels, channel.id)
		}
		channel.client.channelsMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), peerChannelClientWriteTimeout)
		defer cancel()
		_, _ = channel.peer.command(ctx, channel.id, proto.PeerTypeWorkspaceDetach, proto.PeerWorkspaceDetach{})
	})
}

func (peer *peerChannel) runWriter() {
	pings := time.NewTicker(peerChannelClientPingInterval)
	heartbeats := time.NewTicker(peerChannelClientPingInterval)
	defer pings.Stop()
	defer heartbeats.Stop()
	var sequence uint64
	for {
		message, ok := peer.nextOutbound(pings.C, heartbeats.C)
		if !ok {
			return
		}
		if message.controlType != 0 {
			err := peer.conn.WriteControl(message.controlType, message.controlData, time.Now().Add(peerChannelClientWriteTimeout))
			if message.written != nil {
				message.written <- err
			}
			if err != nil {
				peer.close(err)
				return
			}
			continue
		}
		sequence++
		messageID := message.messageID
		if messageID == "" {
			messageID = uuid.NewString()
		}
		data, err := proto.EncodePeerMessage(peer.epoch, sequence, messageID, message.replyTo, message.workspaceID, message.messageType, message.payload)
		if err == nil {
			err = peer.conn.SetWriteDeadline(time.Now().Add(peerChannelClientWriteTimeout))
		}
		if err == nil {
			peer.lastSent.Store(sequence)
			err = peer.conn.WriteMessage(websocket.TextMessage, data)
		}
		if message.written != nil {
			message.written <- err
		}
		if err != nil {
			peer.close(err)
			return
		}
	}
}

func (peer *peerChannel) nextOutbound(pings, heartbeats <-chan time.Time) (peerChannelWrite, bool) {
	for _, queue := range []<-chan peerChannelWrite{peer.critical, peer.state, peer.workspace} {
		select {
		case message := <-queue:
			return message, true
		default:
		}
	}
	select {
	case <-peer.ctx.Done():
		return peerChannelWrite{}, false
	case message := <-peer.critical:
		return message, true
	case message := <-peer.state:
		return message, true
	case message := <-peer.workspace:
		return message, true
	case message := <-peer.telemetry:
		return message, true
	case <-pings:
		if time.Since(time.Unix(0, peer.lastHeartbeat.Load())) > peerChannelClientPongTimeout {
			peer.close(errors.New("peer channel application heartbeat stalled"))
			return peerChannelWrite{}, false
		}
		return peerChannelWrite{controlType: websocket.PingMessage}, true
	case <-heartbeats:
		return peerChannelWrite{messageType: proto.PeerTypeHeartbeat, payload: proto.PeerHeartbeat{LastReceivedSequence: peer.lastReceived.Load(), Workspaces: peer.client.workspaceSummaries()}}, true
	}
}

func (peer *peerChannel) runReader() {
	var sequence uint64
	for {
		messageType, data, err := peer.conn.ReadMessage()
		if err == nil && messageType != websocket.TextMessage {
			err = errors.New("peer channel accepts text frames only")
		}
		var message proto.PeerDecodedMessage
		if err == nil {
			message, err = proto.DecodePeerMessage(data, proto.PeerDirectionServerToClient)
		}
		if err == nil && message.Envelope.Epoch != peer.epoch {
			err = errors.New("peer channel server changed connection epoch")
		}
		if err == nil && message.Envelope.Sequence != sequence+1 {
			err = errors.New("peer channel server sequence regressed or skipped")
		}
		if err != nil {
			peer.close(err)
			return
		}
		sequence = message.Envelope.Sequence
		peer.lastReceived.Store(sequence)
		switch message.Envelope.Type {
		case proto.PeerTypeAcknowledgement:
			peer.mu.Lock()
			pending, ok := peer.pending[message.Envelope.ReplyTo]
			peer.mu.Unlock()
			if !ok {
				peer.close(errors.New("peer channel acknowledgement has no pending command"))
				return
			}
			pending.result <- *message.Payload.(*proto.PeerAcknowledgement)
		case proto.PeerTypeHeartbeat:
			heartbeat := message.Payload.(*proto.PeerHeartbeat)
			previous := peer.lastHeartbeatSequence.Load()
			if heartbeat.LastReceivedSequence < previous || heartbeat.LastReceivedSequence > peer.lastSent.Load() {
				peer.close(errors.New("peer channel heartbeat reported an invalid received sequence"))
				return
			}
			peer.lastHeartbeatSequence.Store(heartbeat.LastReceivedSequence)
			peer.lastHeartbeat.Store(time.Now().UnixNano())
			peer.mergeWorkspaceSummaries(heartbeat.Workspaces)
		case proto.PeerTypeReady:
			ready := message.Payload.(*proto.PeerReady)
			peer.replaceWorkspaceSummaries(ready.Workspaces)
			peer.lastHeartbeat.Store(time.Now().UnixNano())
			peer.readyOnce.Do(func() { close(peer.ready) })
		case proto.PeerTypeStateSummary:
			summary := message.Payload.(*proto.PeerStateSummary)
			workspaces := make([]proto.PeerWorkspaceSummary, 0, len(summary.Workspaces))
			for _, workspace := range summary.Workspaces {
				workspaces = append(workspaces, proto.PeerWorkspaceSummary{WorkspaceID: workspace.WorkspaceID, Revision: workspace.Revision, Digest: workspace.Digest})
			}
			peer.replaceWorkspaceSummaries(workspaces)
			peer.lastHeartbeat.Store(time.Now().UnixNano())
		case proto.PeerTypeStateRequired:
			required := message.Payload.(*proto.PeerStateRequired)
			peer.mu.Lock()
			for _, requirement := range required.Requirements {
				if requirement.WorkspaceID != "" {
					peer.stateRequirements[requirement.WorkspaceID] = requirement
				}
			}
			peer.mu.Unlock()
			peer.lastHeartbeat.Store(time.Now().UnixNano())
		default:
			event, err := decodePeerEvent(message)
			if err != nil {
				peer.close(err)
				return
			}
			peer.client.channelsMu.Lock()
			channel := peer.client.channels[message.Envelope.WorkspaceID]
			peer.client.channelsMu.Unlock()
			if channel == nil || channel.peer != peer {
				peer.close(errors.New("peer channel event belongs to an unattached workspace"))
				return
			}
			if err := channel.publish(event); err != nil {
				peer.close(err)
				return
			}
		}
	}
}

func decodePeerEvent(message proto.PeerDecodedMessage) (any, error) {
	switch message.Envelope.Type {
	case proto.PeerTypeEventLSP:
		value := message.Payload.(*proto.PeerResourceEvent[proto.LSPEvent])
		return pubsub.Event[proto.LSPEvent]{Type: pubsub.EventType(value.Type), Payload: value.Payload}, nil
	case proto.PeerTypeEventMCP:
		value := message.Payload.(*proto.PeerResourceEvent[proto.MCPEvent])
		return pubsub.Event[proto.MCPEvent]{Type: pubsub.EventType(value.Type), Payload: value.Payload}, nil
	case proto.PeerTypeEventPermissionRequest:
		value := message.Payload.(*proto.PeerResourceEvent[proto.PermissionRequest])
		return pubsub.Event[proto.PermissionRequest]{Type: pubsub.EventType(value.Type), Payload: value.Payload}, nil
	case proto.PeerTypeEventPermissionResult:
		value := message.Payload.(*proto.PeerResourceEvent[proto.PermissionNotification])
		return pubsub.Event[proto.PermissionNotification]{Type: pubsub.EventType(value.Type), Payload: value.Payload}, nil
	case proto.PeerTypeEventQuestionRequest:
		value := message.Payload.(*proto.PeerResourceEvent[proto.QuestionRequest])
		return pubsub.Event[proto.QuestionRequest]{Type: pubsub.EventType(value.Type), Payload: value.Payload}, nil
	case proto.PeerTypeEventQuestionResult:
		value := message.Payload.(*proto.PeerResourceEvent[proto.QuestionNotification])
		return pubsub.Event[proto.QuestionNotification]{Type: pubsub.EventType(value.Type), Payload: value.Payload}, nil
	case proto.PeerTypeEventMessage:
		value := message.Payload.(*proto.PeerResourceEvent[proto.Message])
		return pubsub.Event[proto.Message]{Type: pubsub.EventType(value.Type), Payload: value.Payload}, nil
	case proto.PeerTypeEventSession:
		value := message.Payload.(*proto.PeerResourceEvent[proto.Session])
		return pubsub.Event[proto.Session]{Type: pubsub.EventType(value.Type), Payload: value.Payload}, nil
	case proto.PeerTypeEventFile:
		value := message.Payload.(*proto.PeerResourceEvent[proto.File])
		return pubsub.Event[proto.File]{Type: pubsub.EventType(value.Type), Payload: value.Payload}, nil
	case proto.PeerTypeEventAgent:
		value := message.Payload.(*proto.PeerResourceEvent[proto.AgentEvent])
		return pubsub.Event[proto.AgentEvent]{Type: pubsub.EventType(value.Type), Payload: value.Payload}, nil
	case proto.PeerTypeEventConfigChanged:
		value := message.Payload.(*proto.PeerResourceEvent[proto.ConfigChanged])
		return pubsub.Event[proto.ConfigChanged]{Type: pubsub.EventType(value.Type), Payload: value.Payload}, nil
	case proto.PeerTypeEventSkills:
		value := message.Payload.(*proto.PeerResourceEvent[proto.SkillsEvent])
		return pubsub.Event[proto.SkillsEvent]{Type: pubsub.EventType(value.Type), Payload: value.Payload}, nil
	case proto.PeerTypeEventTask:
		value := message.Payload.(*proto.PeerResourceEvent[proto.TaskNotification])
		return pubsub.Event[proto.TaskNotification]{Type: pubsub.EventType(value.Type), Payload: value.Payload}, nil
	case proto.PeerTypeRunCompleted, proto.PeerTypeRunFailed, proto.PeerTypeRunCancelled:
		value := message.Payload.(*proto.PeerResourceEvent[proto.RunComplete])
		return pubsub.Event[proto.RunComplete]{Type: pubsub.EventType(value.Type), Payload: value.Payload}, nil
	case proto.PeerTypeProviderAvailability:
		return *message.Payload.(*proto.PeerProviderAvailability), nil
	case proto.PeerTypeProviderAuthChanged, proto.PeerTypeProviderAuthInvalidated:
		return *message.Payload.(*proto.PeerProviderAuthentication), nil
	case proto.PeerTypeProviderRefreshRequired:
		return *message.Payload.(*proto.PeerProviderRefreshRequest), nil
	case proto.PeerTypeModelSelectionChanged:
		return *message.Payload.(*proto.PeerModelSelectionChanged), nil
	case proto.PeerTypeRuntimePatchApplied:
		return *message.Payload.(*proto.PeerRuntimePatchApplied), nil
	default:
		return nil, fmt.Errorf("peer message %q is not a workspace event", message.Envelope.Type)
	}
}

func (peer *peerChannel) close(err error) {
	peer.closeOnce.Do(func() {
		if err == nil {
			err = errors.New("peer channel closed")
		}
		peer.mu.Lock()
		peer.err = err
		peer.mu.Unlock()
		peer.cancel(err)
		_ = peer.conn.Close()
		close(peer.done)
		peer.client.channelsMu.Lock()
		if peer.client.peer == peer {
			peer.client.peer = nil
		}
		channels := make([]*workspaceChannel, 0, len(peer.client.channels))
		for id, channel := range peer.client.channels {
			if channel.peer == peer {
				delete(peer.client.channels, id)
				channels = append(channels, channel)
			}
		}
		peer.client.channelsMu.Unlock()
		for _, channel := range channels {
			channel.closeOnce.Do(func() {
				channel.mu.Lock()
				channel.closed = true
				for id, events := range channel.subs {
					delete(channel.subs, id)
					close(events)
				}
				channel.mu.Unlock()
			})
		}
	})
}

func (peer *peerChannel) terminalError() error {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	return peer.err
}

func (peer *peerChannel) replaceWorkspaceSummaries(summaries []proto.PeerWorkspaceSummary) {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	peer.serverWorkspaces = make(map[string]proto.PeerWorkspaceSummary, len(summaries))
	for _, summary := range summaries {
		peer.serverWorkspaces[summary.WorkspaceID] = summary
		delete(peer.stateRequirements, summary.WorkspaceID)
	}
}

func (peer *peerChannel) mergeWorkspaceSummaries(summaries []proto.PeerWorkspaceSummary) {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	for _, summary := range summaries {
		peer.serverWorkspaces[summary.WorkspaceID] = summary
		delete(peer.stateRequirements, summary.WorkspaceID)
	}
}

func (peer *peerChannel) updateWorkspaceSummary(workspaceID string, revision uint64, digest string) {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	peer.serverWorkspaces[workspaceID] = proto.PeerWorkspaceSummary{WorkspaceID: workspaceID, Revision: revision, Digest: digest}
	delete(peer.stateRequirements, workspaceID)
}

func (peer *peerChannel) workspaceSummary(workspaceID string) (proto.PeerWorkspaceSummary, error) {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if requirement, ok := peer.stateRequirements[workspaceID]; ok {
		if requirement.Kind == "workspace" {
			return proto.PeerWorkspaceSummary{}, ErrNotFound
		}
		return proto.PeerWorkspaceSummary{}, errors.New("peer channel requires explicit workspace state synchronization")
	}
	summary, ok := peer.serverWorkspaces[workspaceID]
	if !ok {
		return proto.PeerWorkspaceSummary{}, ErrNotFound
	}
	return summary, nil
}

func (c *Client) peerWorkspaceAuthority(ctx context.Context, workspaceID, principal string) (*config.RemoteAuthority, error) {
	peer, err := c.ensurePeerChannel(ctx)
	if err != nil {
		return nil, err
	}
	summary, err := peer.workspaceSummary(workspaceID)
	if err != nil {
		return nil, err
	}
	return &config.RemoteAuthority{Mode: "client", Principal: principal, Revision: summary.Revision, Digest: summary.Digest}, nil
}

func (c *Client) PeerWorkspaceAuthority(ctx context.Context, workspaceID, principal string) (*config.RemoteAuthority, error) {
	return c.peerWorkspaceAuthority(ctx, workspaceID, principal)
}

func (c *Client) workspaceSummaries() []proto.PeerWorkspaceSummary {
	c.attachmentMu.RLock()
	defer c.attachmentMu.RUnlock()
	result := make([]proto.PeerWorkspaceSummary, 0, len(c.attachments))
	for id, attachment := range c.attachments {
		if attachment.Mode == "client" {
			result = append(result, proto.PeerWorkspaceSummary{WorkspaceID: id, Revision: attachment.Revision, Digest: attachment.Digest})
		}
	}
	return result
}
