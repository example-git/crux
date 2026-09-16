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
			decision := peerChannelProxyDecision{}
			if messageType == websocket.TextMessage && intercept != nil {
				var envelope proto.PeerEnvelope
				if json.Unmarshal(data, &envelope) == nil && envelope.Version == proto.PeerChannelVersion {
					decision = intercept(fromClient, envelope)
				}
			}
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
		}
	}
	go pump(frontend, backend, true, &backendWrite, &frontendWrite)
	go pump(backend, frontend, false, &frontendWrite, &backendWrite)
	<-done
}
