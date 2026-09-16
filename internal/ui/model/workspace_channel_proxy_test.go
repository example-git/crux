package model

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// modelWorkspaceChannelProxyDecision mirrors internal/cmd's
// commandChannelProxyDecision: the real client (internal/client's
// dialPeerChannel) exclusively dials the v2 peer channel at
// "/v1/peer-channel" using PeerEnvelope framing, not the retired v1
// "/channel" WorkspaceChannelFrame shape this fixture used to intercept. A
// fixture still speaking v1 here left every fault-injection hook (PUT
// reject, drop-committed-ack, etc.) permanently un-triggered because real
// traffic never reached it, so tests that asserted an induced failure
// silently passed for the wrong reason (or, after the client stopped
// dialing v1 at all, failed because the induced failure never fired). Do
// not revert this back to the v1 shape; see internal/cmd's
// proxyPeerCommandChannel for the canonical v2 interceptor this mirrors.
//
// The Sequence field on Reply/Replacement is ignored: the proxy itself
// owns wire sequence numbering (see proxyModelWorkspaceChannel) so callers
// never need to reason about the gapless per-connection sequence
// invariant the real v2 server enforces.
type modelWorkspaceChannelProxyDecision struct {
	drop        bool
	close       bool
	reply       *proto.PeerEnvelope
	replacement *proto.PeerEnvelope
}

type modelWorkspaceChannelProxyInterceptor func(bool, proto.PeerEnvelope) modelWorkspaceChannelProxyDecision

// isPeerRuntimeCommand reports whether a client-to-server peer-channel
// message mutates the workspace's remote runtime authority (provider
// definitions, credentials, model selection, or controls). PatchRemoteRuntime
// (internal/client/remote_runtime.go) picks exactly one of these per call
// depending on what changed; RuntimeReplace additionally covers explicit
// manual recovery. Together they are the v2 equivalent of the single v1
// WorkspaceChannelRuntimeReplaceFrame this fixture used to match generically.
func isPeerRuntimeCommand(messageType proto.PeerMessageType) bool {
	switch messageType {
	case proto.PeerTypeRuntimeTransaction, proto.PeerTypeRuntimeReplace, proto.PeerTypeModelSelectionSet, proto.PeerTypeRuntimeControlsPatch:
		return true
	default:
		return false
	}
}

func proxyModelWorkspaceChannel(t *testing.T, w http.ResponseWriter, r *http.Request, target string, tlsConfig *tls.Config, intercept modelWorkspaceChannelProxyInterceptor) {
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
	// toBackendSeq/toClientSeq are the gapless, per-connection wire
	// sequence counters each endpoint actually observes. The real v2
	// server rejects any connection whose received sequence is not
	// exactly one more than the last (internal/server/peer_channel.go's
	// readLoop: "peer channel sequence regressed or skipped"), and the
	// real client enforces the same invariant on what it receives. A
	// fixture that drops a client->server command (for example, to
	// simulate a clean rejection without ever reaching the real backend)
	// or injects a synthetic reply must not let the original, now-stale
	// Sequence values reach either endpoint verbatim: the next message
	// that legitimately passes through would then carry a sequence with
	// a gap (dropped case) or a collision (injected case), and the
	// connection would be torn down with an opaque "abnormal closure"
	// that has nothing to do with the fault actually being injected.
	// Every message that is genuinely written toward an endpoint --
	// whether forwarded, replaced, or synthesized -- is renumbered here
	// so both legs of the proxy stay internally consistent regardless of
	// what the interceptor drops or fabricates.
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
			decision := modelWorkspaceChannelProxyDecision{}
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
