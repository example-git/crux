package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/redact"
	"github.com/gorilla/websocket"
)

const (
	workspaceChannelWriteTimeout = 10 * time.Second
	workspaceChannelPongTimeout  = 60 * time.Second
	workspaceChannelPingInterval = 30 * time.Second
)

type workspaceChannelRead struct {
	frame proto.WorkspaceChannelFrame
	err   error
}

func (c *controllerV1) handleGetWorkspaceChannel(w http.ResponseWriter, r *http.Request) {
	if !c.requireRuntimeProtocol(w, r) {
		return
	}
	id := r.PathValue("id")
	clientID, ok := c.requireClientID(w, r)
	if !ok {
		return
	}
	accepted, err := proto.ParseWorkspaceAttachment(r.Header)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	events, err := c.backend.SubscribeEvents(r.Context(), id)
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	if err := c.backend.AttachClientWithAuthority(id, clientID, requestPrincipal(r), accepted); err != nil {
		c.handleError(w, r, err)
		return
	}
	defer c.backend.DetachClient(id, clientID)
	ws, err := c.backend.GetWorkspace(id)
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	responseHeader := http.Header{}
	if accepted != nil {
		accepted.SetHeaders(responseHeader)
	}
	connection, err := (&websocket.Upgrader{
		ReadBufferSize:  32 << 10,
		WriteBufferSize: 32 << 10,
		Subprotocols:    []string{proto.WorkspaceChannelProtocol},
	}).Upgrade(w, r, responseHeader)
	if err != nil {
		return
	}
	defer connection.Close()
	connection.SetReadLimit(proto.MaxWorkspaceChannelFrameBytes)
	_ = connection.SetReadDeadline(time.Now().Add(workspaceChannelPongTimeout))
	connection.SetPongHandler(func(string) error {
		return connection.SetReadDeadline(time.Now().Add(workspaceChannelPongTimeout))
	})

	principal := requestPrincipal(r)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	reads := make(chan workspaceChannelRead, 1)
	go c.readWorkspaceChannel(ctx, connection, reads)
	pending := make(chan proto.WorkspaceChannelFrame, 32)
	if ws.Cfg != nil && ws.Cfg.RemoteAuthority() != nil {
		for _, request := range ws.Cfg.PendingClientRefreshes() {
			if !queueWorkspaceChannelEvent(pending, wrapEvent(pubsub.Event[config.ClientRefreshRequest]{Type: pubsub.CreatedEvent, Payload: request})) {
				return
			}
		}
	}
	pings := time.NewTicker(workspaceChannelPingInterval)
	defer pings.Stop()
	seen := map[string]struct{}{}
	for {
		select {
		case <-ctx.Done():
			return
		case result := <-reads:
			if result.err != nil {
				if result.frame.CommandID != "" {
					_ = writeWorkspaceChannelFrame(connection, workspaceChannelFailure(result.frame.CommandID, proto.WorkspaceChannelStatusInvalid, "invalid workspace channel command"))
				} else {
					_ = connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "invalid workspace channel frame"), time.Now().Add(workspaceChannelWriteTimeout))
				}
				return
			}
			frame := result.frame
			if frame.Type != proto.WorkspaceChannelRuntimeReplaceFrame && frame.Type != proto.WorkspaceChannelRefreshCompleteFrame {
				if frame.CommandID == "" {
					_ = connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "unsupported workspace channel frame"), time.Now().Add(workspaceChannelWriteTimeout))
					return
				}
				if !writeWorkspaceChannelFrame(connection, workspaceChannelFailure(frame.CommandID, proto.WorkspaceChannelStatusInvalid, "unsupported workspace channel command")) {
					return
				}
				continue
			}
			if _, duplicate := seen[frame.CommandID]; duplicate {
				if !writeWorkspaceChannelFrame(connection, workspaceChannelFailure(frame.CommandID, proto.WorkspaceChannelStatusConflict, "duplicate workspace channel command")) {
					return
				}
				continue
			}
			seen[frame.CommandID] = struct{}{}
			ack := c.executeWorkspaceChannelCommand(r.Context(), ws, principal, frame)
			if !writeWorkspaceChannelFrame(connection, ack) {
				return
			}
		case frame := <-pending:
			if !writeWorkspaceChannelFrame(connection, frame) {
				return
			}
		case event, open := <-events:
			if !open {
				return
			}
			wrapped := wrapEvent(event.Payload)
			if wrapped == nil {
				continue
			}
			data, err := json.Marshal(wrapped)
			if err != nil {
				continue
			}
			data, err = redact.JSON(data)
			if err != nil || len(data) > proto.MaxWorkspaceChannelEventBytes {
				continue
			}
			if !writeWorkspaceChannelFrame(connection, proto.WorkspaceChannelFrame{Type: proto.WorkspaceChannelEventFrame, Event: data}) {
				return
			}
		case <-pings.C:
			if err := connection.WriteControl(websocket.PingMessage, nil, time.Now().Add(workspaceChannelWriteTimeout)); err != nil {
				return
			}
		}
	}
}

func (c *controllerV1) readWorkspaceChannel(ctx context.Context, connection *websocket.Conn, results chan<- workspaceChannelRead) {
	for {
		messageType, data, err := connection.ReadMessage()
		if err == nil && messageType != websocket.TextMessage {
			err = errors.New("workspace channel accepts text frames only")
		}
		var frame proto.WorkspaceChannelFrame
		if err == nil {
			frame, err = proto.DecodeWorkspaceChannelFrame(data)
		}
		if err == nil && frame.Type == proto.WorkspaceChannelRuntimeReplaceFrame {
			err = validateRuntimeProviderFields(data)
		}
		select {
		case results <- workspaceChannelRead{frame: frame, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func (c *controllerV1) executeWorkspaceChannelCommand(ctx context.Context, ws *backend.Workspace, principal string, frame proto.WorkspaceChannelFrame) proto.WorkspaceChannelFrame {
	if ws.Cfg == nil {
		return workspaceChannelFailure(frame.CommandID, proto.WorkspaceChannelStatusUnavailable, "workspace runtime is unavailable")
	}
	switch frame.Type {
	case proto.WorkspaceChannelRuntimeReplaceFrame:
		request := frame.RuntimeReplace
		ack, err := ws.Cfg.ReplaceRemoteRuntime(ctx, request.Runtime, principal, request.ExpectedRevision)
		if err != nil {
			return workspaceChannelError(frame.CommandID, err)
		}
		ws.SendEvent(pubsub.Event[proto.ConfigChanged]{Type: pubsub.UpdatedEvent, Payload: proto.ConfigChanged{WorkspaceID: ws.ID}})
		return proto.WorkspaceChannelFrame{Type: proto.WorkspaceChannelAcknowledgementFrame, CommandID: frame.CommandID, Acknowledgement: &proto.WorkspaceChannelAcknowledgement{Status: proto.WorkspaceChannelStatusOK, Authority: ack}}
	case proto.WorkspaceChannelRefreshCompleteFrame:
		authority := ws.Cfg.RemoteAuthority()
		if authority == nil {
			return workspaceChannelFailure(frame.CommandID, proto.WorkspaceChannelStatusForbidden, "workspace is not client-authoritative")
		}
		if authority.Principal != principal {
			return workspaceChannelFailure(frame.CommandID, proto.WorkspaceChannelStatusForbidden, "workspace runtime principal changed")
		}
		if err := ws.Cfg.CompleteClientRefresh(principal, *frame.RefreshComplete); err != nil {
			return workspaceChannelError(frame.CommandID, err)
		}
		return proto.WorkspaceChannelFrame{Type: proto.WorkspaceChannelAcknowledgementFrame, CommandID: frame.CommandID, Acknowledgement: &proto.WorkspaceChannelAcknowledgement{Status: proto.WorkspaceChannelStatusOK}}
	default:
		return workspaceChannelFailure(frame.CommandID, proto.WorkspaceChannelStatusInvalid, "unsupported workspace channel command")
	}
}

func workspaceChannelError(commandID string, err error) proto.WorkspaceChannelFrame {
	status, message := proto.WorkspaceChannelStatusInvalid, "workspace channel command is invalid"
	switch {
	case errors.Is(err, config.ErrRemoteRuntimeRevision):
		status, message = proto.WorkspaceChannelStatusConflict, "workspace runtime revision changed"
	case errors.Is(err, config.ErrRuntimeRevoked):
		status, message = proto.WorkspaceChannelStatusForbidden, "workspace runtime authority was revoked"
	case errors.Is(err, backend.ErrWorkspaceNotFound):
		status, message = proto.WorkspaceChannelStatusNotFound, "workspace is unavailable"
	case errors.Is(err, backend.ErrWorkspaceClosing), errors.Is(err, backend.ErrServerShuttingDown):
		status, message = proto.WorkspaceChannelStatusUnavailable, "workspace is unavailable"
	}
	return workspaceChannelFailure(commandID, status, message)
}

func workspaceChannelFailure(commandID string, status proto.WorkspaceChannelStatus, message string) proto.WorkspaceChannelFrame {
	return proto.WorkspaceChannelFrame{Type: proto.WorkspaceChannelAcknowledgementFrame, CommandID: commandID, Acknowledgement: &proto.WorkspaceChannelAcknowledgement{Status: status, Message: message}}
}

func writeWorkspaceChannelFrame(connection *websocket.Conn, frame proto.WorkspaceChannelFrame) bool {
	data, err := json.Marshal(frame)
	if err != nil || len(data) > proto.MaxWorkspaceChannelFrameBytes {
		return false
	}
	if err := connection.SetWriteDeadline(time.Now().Add(workspaceChannelWriteTimeout)); err != nil {
		return false
	}
	return connection.WriteMessage(websocket.TextMessage, data) == nil
}

func queueWorkspaceChannelEvent(queue chan<- proto.WorkspaceChannelFrame, payload *pubsub.Payload) bool {
	if payload == nil {
		return true
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	data, err = redact.JSON(data)
	if err != nil || len(data) > proto.MaxWorkspaceChannelEventBytes {
		return false
	}
	select {
	case queue <- proto.WorkspaceChannelFrame{Type: proto.WorkspaceChannelEventFrame, Event: data}:
		return true
	default:
		return false
	}
}
