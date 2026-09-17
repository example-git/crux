package workspace_test

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type workspaceChannelProxyDecision struct {
	drop        bool
	close       bool
	reply       *proto.WorkspaceChannelFrame
	replacement *proto.PeerEnvelope
}

type (
	workspaceChannelProxyInterceptor func(bool, proto.WorkspaceChannelFrame) workspaceChannelProxyDecision
	peerChannelProxyInterceptor      func(bool, proto.PeerEnvelope) workspaceChannelProxyDecision
)

type workspaceChannelProxyServer struct {
	*httptest.Server
	mu          sync.Mutex
	connections map[*websocket.Conn]struct{}
}

func (s *workspaceChannelProxyServer) add(connection *websocket.Conn) {
	s.mu.Lock()
	s.connections[connection] = struct{}{}
	s.mu.Unlock()
}

func (s *workspaceChannelProxyServer) remove(connection *websocket.Conn) {
	s.mu.Lock()
	delete(s.connections, connection)
	s.mu.Unlock()
}

func (s *workspaceChannelProxyServer) closeChannelConnections() {
	s.mu.Lock()
	connections := make([]*websocket.Conn, 0, len(s.connections))
	for connection := range s.connections {
		connections = append(connections, connection)
	}
	s.mu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
}

func startWorkspaceChannelProxyServer(t *testing.T, handler http.Handler, serverTLS *tls.Config, clientTLS func(*http.Request) *tls.Config, intercept workspaceChannelProxyInterceptor) *workspaceChannelProxyServer {
	t.Helper()
	backend := httptest.NewUnstartedServer(handler)
	backend.TLS = serverTLS
	backend.StartTLS()
	remote := &workspaceChannelProxyServer{connections: make(map[*websocket.Conn]struct{})}
	remote.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/channel") {
			proxyWorkspaceChannel(t, w, r, backend.URL, clientTLS(r), intercept, nil, remote)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	remote.TLS = serverTLS
	remote.StartTLS()
	t.Cleanup(func() {
		remote.Close()
		backend.Close()
	})
	return remote
}

func startPeerChannelProxyServer(t *testing.T, handler http.Handler, serverTLS *tls.Config, clientTLS func(*http.Request) *tls.Config, intercept peerChannelProxyInterceptor) *workspaceChannelProxyServer {
	t.Helper()
	backend := httptest.NewUnstartedServer(handler)
	backend.TLS = serverTLS
	backend.StartTLS()
	remote := &workspaceChannelProxyServer{connections: make(map[*websocket.Conn]struct{})}
	remote.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/peer-channel") {
			proxyWorkspaceChannel(t, w, r, backend.URL, clientTLS(r), nil, intercept, remote)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	remote.TLS = serverTLS
	remote.StartTLS()
	t.Cleanup(func() {
		remote.Close()
		backend.Close()
	})
	return remote
}

func proxyWorkspaceChannel(t *testing.T, w http.ResponseWriter, r *http.Request, target string, tlsConfig *tls.Config, intercept workspaceChannelProxyInterceptor, peerIntercept peerChannelProxyInterceptor, tracked *workspaceChannelProxyServer) {
	t.Helper()
	headers := r.Header.Clone()
	for _, name := range []string{"Connection", "Upgrade", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol"} {
		headers.Del(name)
	}
	endpoint := "wss" + strings.TrimPrefix(target, "https") + r.URL.RequestURI()
	protocol := proto.WorkspaceChannelProtocol
	if peerIntercept != nil {
		protocol = proto.PeerChannelProtocol
	}
	backend, response, err := (&websocket.Dialer{TLSClientConfig: tlsConfig.Clone(), Subprotocols: []string{protocol}}).DialContext(r.Context(), endpoint, headers)
	if err != nil {
		if response != nil {
			defer response.Body.Close()
			for name, values := range response.Header {
				w.Header()[name] = append([]string(nil), values...)
			}
			w.WriteHeader(response.StatusCode)
			_, _ = io.Copy(w, io.LimitReader(response.Body, 64<<10))
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer backend.Close()
	responseHeaders := http.Header{}
	if attachment, parseErr := proto.ParseWorkspaceAttachment(response.Header); parseErr == nil && attachment != nil {
		attachment.SetHeaders(responseHeaders)
	}
	frontend, err := (&websocket.Upgrader{Subprotocols: []string{protocol}}).Upgrade(w, r, responseHeaders)
	require.NoError(t, err)
	tracked.add(frontend)
	defer tracked.remove(frontend)
	defer frontend.Close()

	var frontendWrite sync.Mutex
	var backendWrite sync.Mutex
	// toBackendSeq/toClientSeq are the gapless, per-connection wire sequence
	// counters each endpoint actually observes for peer-channel (v2)
	// traffic. The real v2 server (internal/server/peer_channel.go's
	// readLoop) rejects any message whose Sequence does not immediately
	// follow the last one it saw, so every message actually delivered on a
	// given leg (forwarded untouched, replaced, or synthesized here) must be
	// renumbered against that leg's own counter, or a dropped/injected
	// message on one leg desyncs the other leg's expected sequence and the
	// connection is killed before the fault-injection scenario takes
	// effect. This only applies when peerIntercept is in use; the legacy
	// workspace-channel (v1) path below is unaffected.
	var toBackendSeq, toClientSeq uint64
	done := make(chan struct{}, 2)
	renumber := func(counter *uint64, envelope proto.PeerEnvelope) ([]byte, error) {
		*counter++
		envelope.Sequence = *counter
		return json.Marshal(envelope)
	}
	pump := func(source, destination *websocket.Conn, fromClient bool, destinationWrite, sourceWrite *sync.Mutex, destinationSeq, sourceSeq *uint64) {
		defer func() { done <- struct{}{} }()
		for {
			messageType, data, readErr := source.ReadMessage()
			if readErr != nil {
				return
			}
			decision := workspaceChannelProxyDecision{}
			var peerEnvelope proto.PeerEnvelope
			havePeerEnvelope := false
			if messageType == websocket.TextMessage {
				if peerIntercept != nil {
					if json.Unmarshal(data, &peerEnvelope) == nil && peerEnvelope.Version == proto.PeerChannelVersion {
						havePeerEnvelope = true
						decision = peerIntercept(fromClient, peerEnvelope)
					}
				} else if intercept != nil {
					var frame proto.WorkspaceChannelFrame
					if json.Unmarshal(data, &frame) == nil {
						decision = intercept(fromClient, frame)
					}
				}
			}
			if peerIntercept == nil {
				if decision.replacement != nil {
					replacement, marshalErr := json.Marshal(decision.replacement)
					if marshalErr != nil {
						return
					}
					data = replacement
				}
				if decision.reply != nil {
					reply, marshalErr := json.Marshal(decision.reply)
					if marshalErr != nil {
						return
					}
					sourceWrite.Lock()
					writeErr := source.WriteMessage(websocket.TextMessage, reply)
					sourceWrite.Unlock()
					if writeErr != nil {
						return
					}
				}
				if decision.close {
					return
				}
				if decision.drop {
					continue
				}
				destinationWrite.Lock()
				writeErr := destination.WriteMessage(messageType, data)
				destinationWrite.Unlock()
				if writeErr != nil {
					return
				}
				continue
			}
			// No peer-channel intercept sets decision.reply today (it is
			// typed for the legacy v1 WorkspaceChannelFrame reply below,
			// not proto.PeerEnvelope); only replacement/drop/close are
			// meaningful on this leg.
			if decision.close {
				return
			}
			if decision.drop {
				continue
			}
			outgoing := data
			switch {
			case decision.replacement != nil:
				replacement, marshalErr := renumber(destinationSeq, *decision.replacement)
				if marshalErr != nil {
					return
				}
				outgoing = replacement
			case havePeerEnvelope:
				rewritten, marshalErr := renumber(destinationSeq, peerEnvelope)
				if marshalErr != nil {
					return
				}
				outgoing = rewritten
			}
			destinationWrite.Lock()
			writeErr := destination.WriteMessage(messageType, outgoing)
			destinationWrite.Unlock()
			if writeErr != nil {
				return
			}
		}
	}
	go pump(frontend, backend, true, &backendWrite, &frontendWrite, &toBackendSeq, &toClientSeq)
	go pump(backend, frontend, false, &frontendWrite, &backendWrite, &toClientSeq, &toBackendSeq)
	<-done
}
