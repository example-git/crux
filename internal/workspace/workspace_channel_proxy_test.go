package workspace

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
	drop  bool
	close bool
	reply *proto.WorkspaceChannelFrame
}

type workspaceChannelProxyInterceptor func(bool, proto.WorkspaceChannelFrame) workspaceChannelProxyDecision

type peerChannelProxyDecision struct {
	drop        bool
	close       bool
	reply       *proto.PeerEnvelope
	replacement *proto.PeerEnvelope
}

type peerChannelProxyInterceptor func(bool, proto.PeerEnvelope) peerChannelProxyDecision

func startWorkspaceChannelProxyServer(t *testing.T, handler http.Handler, serverTLS *tls.Config, clientTLS func(*http.Request) *tls.Config, intercept workspaceChannelProxyInterceptor) *httptest.Server {
	t.Helper()
	backend := httptest.NewUnstartedServer(handler)
	backend.TLS = serverTLS
	backend.StartTLS()
	remote := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/channel") {
			proxyWorkspaceChannel(t, w, r, backend.URL, clientTLS(r), intercept)
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

func proxyWorkspaceChannel(t *testing.T, w http.ResponseWriter, r *http.Request, target string, tlsConfig *tls.Config, intercept workspaceChannelProxyInterceptor) {
	t.Helper()
	headers := r.Header.Clone()
	for _, name := range []string{"Connection", "Upgrade", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol"} {
		headers.Del(name)
	}
	endpoint := "wss" + strings.TrimPrefix(target, "https") + r.URL.RequestURI()
	backend, response, err := (&websocket.Dialer{TLSClientConfig: tlsConfig.Clone(), Subprotocols: []string{proto.WorkspaceChannelProtocol}}).DialContext(r.Context(), endpoint, headers)
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
	frontend, err := (&websocket.Upgrader{Subprotocols: []string{proto.WorkspaceChannelProtocol}}).Upgrade(w, r, responseHeaders)
	require.NoError(t, err)
	defer frontend.Close()

	var frontendWrite sync.Mutex
	var backendWrite sync.Mutex
	done := make(chan struct{}, 2)
	pump := func(source, destination *websocket.Conn, fromClient bool, destinationWrite, sourceWrite *sync.Mutex) {
		defer func() { done <- struct{}{} }()
		for {
			messageType, data, readErr := source.ReadMessage()
			if readErr != nil {
				return
			}
			decision := workspaceChannelProxyDecision{}
			if messageType == websocket.TextMessage && intercept != nil {
				var frame proto.WorkspaceChannelFrame
				if json.Unmarshal(data, &frame) == nil {
					decision = intercept(fromClient, frame)
				}
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
		}
	}
	go pump(frontend, backend, true, &backendWrite, &frontendWrite)
	go pump(backend, frontend, false, &frontendWrite, &backendWrite)
	<-done
}

func proxyPeerChannel(t *testing.T, w http.ResponseWriter, r *http.Request, target string, tlsConfig *tls.Config, intercept peerChannelProxyInterceptor) {
	proxyPeerChannelConnected(t, w, r, target, tlsConfig, intercept, nil)
}

// proxyPeerChannelConnected behaves like proxyPeerChannel but additionally
// invokes connected (if non-nil) with the client-facing connection once the
// proxied WebSocket is established. Some fault-injection scenarios (e.g. "the
// receiver became unreachable" while no wire traffic is in flight) have no
// envelope for the interceptor to act on, since PeerWorkspaceAuthority is a
// local cache read that never touches the network while a connection stays
// open. Capturing the raw connection lets a test force a disconnect directly
// so the client's next network-touching call is forced to redial and can
// observe the fault-injected dial failure.
func proxyPeerChannelConnected(t *testing.T, w http.ResponseWriter, r *http.Request, target string, tlsConfig *tls.Config, intercept peerChannelProxyInterceptor, connected func(*websocket.Conn)) {
	t.Helper()
	headers := r.Header.Clone()
	for _, name := range []string{"Connection", "Upgrade", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol"} {
		headers.Del(name)
	}
	endpoint := "wss" + strings.TrimPrefix(target, "https") + r.URL.RequestURI()
	backend, response, err := (&websocket.Dialer{TLSClientConfig: tlsConfig.Clone(), Subprotocols: []string{proto.PeerChannelProtocol}}).DialContext(r.Context(), endpoint, headers)
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
	frontend, err := (&websocket.Upgrader{Subprotocols: []string{proto.PeerChannelProtocol}}).Upgrade(w, r, nil)
	require.NoError(t, err)
	defer frontend.Close()
	if connected != nil {
		connected(frontend)
	}

	var frontendWrite sync.Mutex
	var backendWrite sync.Mutex
	// toBackendSeq/toClientSeq are the gapless, per-connection wire sequence
	// counters each endpoint actually observes. The real v2 server
	// (internal/server/peer_channel.go's readLoop) rejects any message whose
	// Sequence does not immediately follow the last one it saw, so every
	// message actually delivered on a given leg (forwarded untouched,
	// replaced, or synthesized here) must be renumbered against that leg's
	// own counter. Without this, a dropped or injected message on one leg
	// desyncs the other leg's expected sequence and the connection is
	// killed with "peer channel sequence regressed or skipped" before the
	// fault-injection scenario the test is exercising ever takes effect.
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
			var envelope proto.PeerEnvelope
			haveEnvelope := messageType == websocket.TextMessage && json.Unmarshal(data, &envelope) == nil && envelope.Version == proto.PeerChannelVersion
			if readErr != nil {
				return
			}
			decision := peerChannelProxyDecision{}
			if haveEnvelope && intercept != nil {
				decision = intercept(fromClient, envelope)
			}
			if decision.reply != nil {
				reply, marshalErr := renumber(sourceSeq, *decision.reply)
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
			outgoing := data
			switch {
			case decision.replacement != nil:
				replacement, marshalErr := renumber(destinationSeq, *decision.replacement)
				if marshalErr != nil {
					return
				}
				outgoing = replacement
			case haveEnvelope:
				rewritten, marshalErr := renumber(destinationSeq, envelope)
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
