package webclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/redact"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/version"
	"github.com/example-git/crux/internal/workspace"
	"github.com/gorilla/websocket"
)

type remoteOwner struct {
	connection string
	clientID   string
}

type remoteWorkspaces struct {
	mu     sync.Mutex
	closed bool
	owners map[remoteOwner]*remoteSlot
	native *server.Server
}

type remoteSlot struct {
	mu         sync.Mutex
	attachment *remoteAttachment
}

type remoteAttachment struct {
	client          *client.Client
	workspace       *workspace.ClientWorkspace
	path            string
	dataDir         string
	mu              sync.Mutex
	closed          bool
	listeners       map[chan []byte]struct{}
	usageFrames     map[string][]byte
	sockets         map[*websocket.Conn]struct{}
	selection       uint64
	selectionIntent client.CurrentSessionSelection
	selectionMu     sync.Mutex
	instructionMu   sync.Mutex
	idleTimer       *time.Timer
	idleGeneration  uint64
	detachGrace     time.Duration
	retire          func(uint64)
}

func (m *remoteWorkspaces) slot(owner remoteOwner) (*remoteSlot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("remote workspace host is closed")
	}
	if m.owners == nil {
		m.owners = make(map[remoteOwner]*remoteSlot)
	}
	slot := m.owners[owner]
	if slot == nil {
		slot = &remoteSlot{}
		m.owners[owner] = slot
	}
	return slot, nil
}

func (m *remoteWorkspaces) Close() error {
	m.mu.Lock()
	m.closed = true
	slots := make([]*remoteSlot, 0, len(m.owners))
	for _, slot := range m.owners {
		slots = append(slots, slot)
	}
	m.owners = nil
	m.mu.Unlock()
	for _, slot := range slots {
		slot.mu.Lock()
		if slot.attachment != nil {
			slot.attachment.close()
			slot.attachment = nil
		}
		slot.mu.Unlock()
	}
	return nil
}

func (m *remoteWorkspaces) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	owner := remoteOwner{connection: r.PathValue("name"), clientID: r.URL.Query().Get("client_id")}
	if owner.connection == "" || owner.clientID == "" {
		http.Error(w, "connection and client identity are required", http.StatusBadRequest)
		return
	}
	slot, err := m.slot(owner)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, route, ok := strings.Cut(r.URL.Path, "/v1/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if route == "workspaces" && r.Method == http.MethodPost {
		m.attach(w, r, owner, slot)
		return
	}
	if route == "browser" || route == "browser/create" || route == "workspaces" || route == "config-view" {
		slot.mu.Lock()
		selected := slot.attachment
		slot.mu.Unlock()
		if selected == nil {
			http.Error(w, "remote workspace attachment not found", http.StatusNotFound)
			return
		}
		var value any
		var err error
		switch {
		case route == "browser" && r.Method == http.MethodGet:
			value, err = selected.client.Browse(r.Context(), r.URL.Query().Get("path"))
		case route == "workspaces" && r.Method == http.MethodGet:
			value, err = selected.client.ListWorkspaces(r.Context())
		case route == "config-view" && r.Method == http.MethodGet:
			var view config.ConfigReadView
			view, err = selected.client.ReadConfigView(r.Context(), "")
			value = proto.ConfigReadResponse{View: view}
		case route == "browser/create" && r.Method == http.MethodPost:
			var request proto.PeerWorkspaceCreateRequest
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request) != nil {
				http.Error(w, "invalid project creation request", http.StatusBadRequest)
				return
			}
			var path string
			path, err = selected.client.CreateProject(r.Context(), request, nil)
			value = proto.PeerWorkspaceCreateResult{Path: path}
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, value)
		return
	}
	parts := strings.SplitN(route, "/", 3)
	if len(parts) < 2 || parts[0] != "workspaces" {
		http.NotFound(w, r)
		return
	}
	slot.mu.Lock()
	selected := slot.attachment
	if selected == nil || selected.workspace.Snapshot().ID != parts[1] {
		slot.mu.Unlock()
		http.Error(w, "remote workspace attachment not found", http.StatusNotFound)
		return
	}
	if len(parts) == 2 && r.Method == http.MethodDelete {
		slot.attachment = nil
		selected.close()
		slot.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	slot.mu.Unlock()
	if len(parts) == 2 {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, selected.workspace.Snapshot())
		return
	}
	suffix := "/" + parts[2]
	if suffix == "/_ui/instructions" && (r.Method == http.MethodGet || r.Method == http.MethodPut) {
		selected.instructions(w, r)
		return
	}
	if suffix == "/_ui/terminal" && r.Method == http.MethodGet {
		selected.terminal(w, r)
		return
	}
	if suffix == "/_ui/files/attachment" {
		suffix = "/files/attachment"
	}
	if suffix == "/_ui/tmux-sessions" {
		suffix = "/tmux-sessions"
	}
	if suffix == "/_ui/pending" {
		suffix = "/pending"
	}
	if suffix == "/_ui/commands" && r.Method == http.MethodGet {
		r.SetPathValue("id", parts[1])
		customCommands(selected).ServeHTTP(w, r)
		return
	}
	if suffix == "/_ui/oauth/loopback" && r.Method == http.MethodPost {
		r.SetPathValue("id", parts[1])
		oauthLoopback(selected).ServeHTTP(w, r)
		return
	}
	if suffix == "/auth" || strings.HasPrefix(suffix, "/auth/") {
		request := r.Clone(r.Context())
		request.URL.Path = suffix
		request.URL.RawPath = ""
		selected.authenticationHandler().ServeHTTP(w, request)
		return
	}
	if suffix == "/agent/default-small-model" && r.Method == http.MethodGet {
		model, err := selected.workspace.GetDefaultSmallModelContext(r.Context(), r.URL.Query().Get("provider_id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, model)
		return
	}
	if suffix == "/codebase-index" {
		var status proto.CodebaseIndexStatus
		var err error
		switch r.Method {
		case http.MethodGet:
			status, err = selected.workspace.CodebaseIndexStatus(r.Context())
		case http.MethodPost:
			var update proto.CodebaseIndexUpdate
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&update) != nil || decoder.Decode(new(any)) != io.EOF {
				http.Error(w, "invalid codebase index request", http.StatusBadRequest)
				return
			}
			status, err = selected.workspace.UpdateCodebaseIndex(r.Context(), update)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, status)
		return
	}
	if suffix == "/config" && r.Method == http.MethodGet {
		writeJSON(w, selected.workspace.Snapshot().Config)
		return
	}
	if suffix == "/channel" && r.Method == http.MethodGet {
		selected.channel(w, r)
		return
	}
	if suffix == "/current-session" && r.Method == http.MethodPost {
		selected.selectSession(w, r)
		return
	}
	if suffix == "/config-view" && r.Method == http.MethodGet {
		value, err := selected.workspace.ReadConfigView(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, proto.ConfigReadResponse{WorkspaceID: parts[1], View: value})
		return
	}
	if suffix == "/config/model-overrides" && r.Method == http.MethodPost {
		var request proto.ModelOverridesRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || request.State.Validate() != nil {
			http.Error(w, "invalid model overrides request", http.StatusBadRequest)
			return
		}
		value, err := selected.workspace.OverrideModels(r.Context(), request.State)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, value)
		return
	}
	if strings.HasPrefix(suffix, "/config/") {
		handler := m.native.WorkspaceSettingsHandler(workspaceSettings{resolve: func(id string) (workspace.Workspace, error) {
			if id != selected.workspace.Snapshot().ID {
				return nil, backend.ErrWorkspaceNotFound
			}
			return selected.workspace, nil
		}})
		request := r.Clone(r.Context())
		request.URL.Path = "/v1/" + route
		request.URL.RawPath = ""
		handler.ServeHTTP(w, request)
		return
	}
	snapshot := selected.workspace.Snapshot()
	if snapshot.Authority == nil {
		http.Error(w, "remote workspace authority is unavailable", http.StatusConflict)
		return
	}
	response, err := selected.client.WorkspaceRequest(r.Context(), parts[1], r.Method, suffix, r.URL.Query(), r.Body, *snapshot.Authority)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	for _, key := range []string{"Content-Type", "Crux-Messages-More", "Crux-Workspace-Authority-Mode", "Crux-Workspace-Authority-Revision", "Crux-Workspace-Authority-Digest"} {
		if value := response.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func (m *remoteWorkspaces) attach(w http.ResponseWriter, r *http.Request, owner remoteOwner, slot *remoteSlot) {
	var request struct {
		Path     string   `json:"path"`
		DataDir  string   `json:"data_dir"`
		Debug    bool     `json:"debug"`
		YOLO     bool     `json:"yolo"`
		Channels []string `json:"channels"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil || request.Path == "" {
		http.Error(w, "invalid remote workspace request", http.StatusBadRequest)
		return
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		http.Error(w, "remote workspace host is closed", http.StatusServiceUnavailable)
		return
	}
	if selected := slot.attachment; selected != nil {
		if selected.path != request.Path || selected.dataDir != request.DataDir {
			http.Error(w, "release the current remote workspace before changing projects", http.StatusConflict)
			return
		}
		selected.mu.Lock()
		selected.scheduleRetirementLocked()
		selected.mu.Unlock()
		writeJSON(w, selected.workspace.Snapshot())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	saved, exists, err := connection.Get(ctx, owner.connection)
	if err != nil || !exists {
		http.Error(w, "selected saved connection is unavailable", http.StatusNotFound)
		return
	}
	remote, err := client.NewAuthenticatedClient(config.GlobalWorkspaceDir(), saved)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	accepted := false
	defer func() {
		if !accepted {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = remote.RetireClient(cleanup)
			remote.CloseIdleConnections()
		}
	}()
	if _, err = remote.NegotiateRemoteRuntime(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	runtime, err := client.CollectRemoteRuntime(ctx, remote, request.Debug, 1, nil, request.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	created, err := remote.CreateWorkspace(ctx, proto.Workspace{Path: request.Path, DataDir: request.DataDir, Debug: request.Debug, YOLO: request.YOLO, Channels: request.Channels, Version: version.Version, AuthorityMode: "client", Runtime: runtime})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	selected := &remoteAttachment{client: remote, workspace: workspace.NewProtocolClientWorkspace(remote, *created), path: request.Path, dataDir: request.DataDir, listeners: make(map[chan []byte]struct{})}
	slot.attachment = selected
	selected.detachGrace = m.native.Backend().DetachGrace()
	selected.retire = func(generation uint64) {
		slot.mu.Lock()
		defer slot.mu.Unlock()
		selected.mu.Lock()
		if slot.attachment != selected || selected.closed || selected.idleGeneration != generation || len(selected.listeners) != 0 {
			selected.mu.Unlock()
			return
		}
		selected.closeLocked()
		selected.mu.Unlock()
		slot.attachment = nil
		selected.shutdown()
	}
	selected.mu.Lock()
	selected.scheduleRetirementLocked()
	selected.mu.Unlock()
	accepted = true
	go selected.workspace.SubscribeProtocolEvents(selected.publish, func(workspace.ConnectionEvent) { selected.disconnect() })
	writeJSON(w, selected.workspace.Snapshot())
}

func (a *remoteAttachment) GetWorkspaceConfig(id string) (*config.Config, error) {
	if a.workspace.Snapshot().ID != id {
		return nil, backend.ErrWorkspaceNotFound
	}
	return a.workspace.Config(), nil
}

func (a *remoteAttachment) close() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closeLocked()
	a.mu.Unlock()
	a.shutdown()
}

func (a *remoteAttachment) closeLocked() {
	a.closed = true
	clear(a.usageFrames)
	if a.idleTimer != nil {
		a.idleTimer.Stop()
	}
	for socket := range a.sockets {
		_ = socket.Close()
		delete(a.sockets, socket)
	}
	for listener := range a.listeners {
		close(listener)
		delete(a.listeners, listener)
	}
}

func (a *remoteAttachment) shutdown() {
	a.workspace.Shutdown()
	a.client.CloseIdleConnections()
}

func (a *remoteAttachment) scheduleRetirementLocked() {
	if a.closed || len(a.listeners) != 0 || a.retire == nil {
		return
	}
	if a.idleTimer != nil {
		a.idleTimer.Stop()
	}
	a.idleGeneration++
	generation := a.idleGeneration
	a.idleTimer = time.AfterFunc(a.detachGrace, func() { a.retire(generation) })
}

func (a *remoteAttachment) disconnect() {
	a.mu.Lock()
	defer a.mu.Unlock()
	clear(a.usageFrames)
	for listener := range a.listeners {
		close(listener)
		delete(a.listeners, listener)
	}
}

func (a *remoteAttachment) publish(event any) {
	payload, err := client.EncodeWorkspaceEvent(event)
	if err != nil {
		return
	}
	body, err := json.Marshal(payload)
	if err == nil {
		body, err = redact.JSON(body)
	}
	if err != nil {
		a.disconnect()
		return
	}
	frame, err := json.Marshal(proto.WorkspaceChannelFrame{Type: proto.WorkspaceChannelEventFrame, Event: body})
	if err != nil {
		a.disconnect()
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	switch value := event.(type) {
	case pubsub.Event[proto.ConfigChanged]:
		clear(a.usageFrames)
	case pubsub.Event[proto.AgentEvent]:
		if value.Payload.Type == proto.AgentEventTypeUsage && value.Payload.ProviderID != "" {
			if a.usageFrames == nil {
				a.usageFrames = make(map[string][]byte)
			}
			a.usageFrames[value.Payload.ProviderID] = frame
		}
	}
	for listener := range a.listeners {
		select {
		case listener <- frame:
		default:
			close(listener)
			delete(a.listeners, listener)
		}
	}
}

func (a *remoteAttachment) channel(w http.ResponseWriter, r *http.Request) {
	listener := make(chan []byte, 128)
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		http.Error(w, "remote workspace is closed", http.StatusGone)
		return
	}
	initial := make([][]byte, 0, len(a.usageFrames))
	for _, frame := range a.usageFrames {
		initial = append(initial, frame)
	}
	a.listeners[listener] = struct{}{}
	a.idleGeneration++
	if a.idleTimer != nil {
		a.idleTimer.Stop()
	}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.listeners, listener)
		a.scheduleRetirementLocked()
		a.mu.Unlock()
	}()
	socket, err := (&websocket.Upgrader{Subprotocols: []string{proto.WorkspaceChannelProtocol}}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer socket.Close()
	socket.SetReadLimit(proto.MaxWorkspaceChannelFrameBytes)
	_ = socket.SetReadDeadline(time.Now().Add(60 * time.Second))
	socket.SetPongHandler(func(string) error { return socket.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			if _, _, err := socket.ReadMessage(); err != nil {
				return
			}
		}
	}()
	for _, frame := range initial {
		_ = socket.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if socket.WriteMessage(websocket.TextMessage, frame) != nil {
			return
		}
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-closed:
			return
		case frame, ok := <-listener:
			if !ok {
				return
			}
			_ = socket.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if socket.WriteMessage(websocket.TextMessage, frame) != nil {
				return
			}
		case <-ticker.C:
			if socket.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)) != nil {
				return
			}
		}
	}
}

func (a *remoteAttachment) terminal(w http.ResponseWriter, r *http.Request) {
	snapshot := a.workspace.Snapshot()
	if snapshot.Authority == nil {
		http.Error(w, "remote workspace authority is unavailable", http.StatusConflict)
		return
	}
	upstream, err := a.client.OpenWorkspaceTerminal(r.Context(), snapshot.ID, r.URL.Query(), *snapshot.Authority)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	downstream, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer downstream.Close()
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	if a.sockets == nil {
		a.sockets = make(map[*websocket.Conn]struct{})
	}
	a.sockets[upstream] = struct{}{}
	a.sockets[downstream] = struct{}{}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.sockets, upstream)
		delete(a.sockets, downstream)
		a.mu.Unlock()
	}()
	upstream.SetReadLimit(1 << 20)
	downstream.SetReadLimit(1 << 20)
	done := make(chan struct{}, 2)
	relay := func(destination, source *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			kind, body, err := source.ReadMessage()
			if err != nil {
				return
			}
			_ = destination.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if destination.WriteMessage(kind, body) != nil {
				return
			}
		}
	}
	go relay(upstream, downstream)
	go relay(downstream, upstream)
	select {
	case <-done:
	case <-r.Context().Done():
	}
	_ = upstream.Close()
	_ = downstream.Close()
}

func (a *remoteAttachment) selectSession(w http.ResponseWriter, r *http.Request) {
	var request proto.CurrentSession
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil || request.SelectionGeneration == nil || *request.SelectionGeneration == 0 {
		http.Error(w, "invalid session selection", http.StatusBadRequest)
		return
	}
	a.selectionMu.Lock()
	defer a.selectionMu.Unlock()
	if *request.SelectionGeneration < a.selection {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := a.workspace.Snapshot().ID
	if *request.SelectionGeneration > a.selection {
		selection, err := a.client.PrepareCurrentSession(id, request.SessionID, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		a.selection = *request.SelectionGeneration
		a.selectionIntent = selection
	} else if request.SessionID != a.selectionIntent.SessionID {
		http.Error(w, "session selection generation was reused for a different session", http.StatusConflict)
		return
	}
	if err := a.client.SendCurrentSessionSelection(r.Context(), id, a.selectionIntent); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
