package client

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func workspaceChannelTestCapabilities(principal string) proto.RemoteRuntimeCapabilities {
	return proto.RemoteRuntimeCapabilities{
		PeerChannel:      proto.PeerChannelProtocol,
		IncrementalState: true,
		Protocol:         proto.RemoteRuntimeProtocol,
		RuntimeVersion:   config.RemoteRuntimeVersion,
		Compiler:         config.RemoteRuntimeCompiler,
		Principal:        principal,
		MaxRequestBytes:  config.MaxRemoteRuntimeBytes,
		MaxBundles:       64,
		MaxProviders:     64,
		WorkspaceSharing: proto.RemoteRuntimeCertificateSharing,
	}
}

func workspaceChannelTestMessage(status proto.WorkspaceChannelStatus) string {
	if status == proto.WorkspaceChannelStatusOK {
		return ""
	}
	return "synthetic command rejection"
}

// newWorkspaceChannelTestServer starts a TLS test server that negotiates
// runtime capabilities over HTTP and then serves a single peer-channel v2
// connection: hello/ready handshake advertising workspaceAuthority (if any),
// a workspace.attach handshake, and finally one client command decoded and
// handed to acknowledge for a caller-supplied response.
func newWorkspaceChannelTestServer(t *testing.T, principal string, workspaceAuthority *config.RemoteAuthority, acknowledge func(proto.PeerDecodedMessage) proto.PeerAcknowledgement) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{
		Subprotocols: []string{proto.PeerChannelProtocol},
		CheckOrigin:  func(*http.Request) bool { return true },
	}
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/runtime-capabilities" {
			require.NoError(t, json.NewEncoder(w).Encode(workspaceChannelTestCapabilities(principal)))
			return
		}
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/peer-channel", r.URL.Path)
		require.Equal(t, "test", r.URL.Query().Get("client_id"))
		require.Equal(t, proto.RemoteRuntimeProtocol, r.Header.Get("Crux-Runtime-Protocol"))
		connection, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer connection.Close()

		_, data, err := connection.ReadMessage()
		require.NoError(t, err)
		hello, err := proto.DecodePeerMessage(data, proto.PeerDirectionClientToServer)
		require.NoError(t, err)
		require.Equal(t, proto.PeerTypeHello, hello.Envelope.Type)
		epoch := hello.Envelope.Epoch

		data, err = proto.EncodePeerMessage(epoch, 1, "server-1", hello.Envelope.MessageID, "", proto.PeerTypeAcknowledgement, proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK})
		require.NoError(t, err)
		require.NoError(t, connection.WriteMessage(websocket.TextMessage, data))
		var ready proto.PeerReady
		if workspaceAuthority != nil {
			ready.Workspaces = []proto.PeerWorkspaceSummary{{WorkspaceID: "workspace", Revision: workspaceAuthority.Revision, Digest: workspaceAuthority.Digest}}
		}
		data, err = proto.EncodePeerMessage(epoch, 2, "server-2", "", "", proto.PeerTypeReady, ready)
		require.NoError(t, err)
		require.NoError(t, connection.WriteMessage(websocket.TextMessage, data))

		_, data, err = connection.ReadMessage()
		require.NoError(t, err)
		attach, err := proto.DecodePeerMessage(data, proto.PeerDirectionClientToServer)
		require.NoError(t, err)
		require.Equal(t, proto.PeerTypeWorkspaceAttach, attach.Envelope.Type)

		data, err = proto.EncodePeerMessage(epoch, 3, "server-3", attach.Envelope.MessageID, attach.Envelope.WorkspaceID, proto.PeerTypeAcknowledgement, proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK})
		require.NoError(t, err)
		require.NoError(t, connection.WriteMessage(websocket.TextMessage, data))

		_, data, err = connection.ReadMessage()
		require.NoError(t, err)
		command, err := proto.DecodePeerMessage(data, proto.PeerDirectionClientToServer)
		require.NoError(t, err)
		acknowledgement := acknowledge(command)
		data, err = proto.EncodePeerMessage(epoch, 4, "server-4", command.Envelope.MessageID, command.Envelope.WorkspaceID, proto.PeerTypeAcknowledgement, acknowledgement)
		require.NoError(t, err)
		require.NoError(t, connection.WriteMessage(websocket.TextMessage, data))
	}))
}

func TestClientRuntimeNegotiationReportsHTTPFailureBeforePrivateRequest(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		for _, operation := range []string{"create", "replace"} {
			t.Run(strconv.Itoa(status)+"/"+operation, func(t *testing.T) {
				var gets, privateRequests atomic.Int32
				message := "capability request rejected by server"
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != "/v1/runtime-capabilities" {
						privateRequests.Add(1)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					gets.Add(1)
					body, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					require.Empty(t, body)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					require.NoError(t, json.NewEncoder(w).Encode(proto.Error{Message: message}))
				}))
				defer server.Close()
				c := &Client{h: server.Client(), network: "tcp", addr: strings.TrimPrefix(server.URL, "https://"), secure: true, clientID: "test"}
				proposal := config.RemoteRuntimeProposal{Version: config.RemoteRuntimeVersion, Revision: 1, Credentials: []config.RemoteCredentialBinding{{APIKey: "synthetic-private-key"}}}
				var err error
				if operation == "create" {
					_, err = c.CreateWorkspace(t.Context(), proto.Workspace{Path: "/workspace", Runtime: &proposal})
				} else {
					_, err = c.ReplaceRemoteRuntime(t.Context(), "workspace", 0, proposal)
				}
				require.ErrorContains(t, err, "GET /v1/runtime-capabilities")
				require.ErrorContains(t, err, "status code "+strconv.Itoa(status))
				require.ErrorContains(t, err, message)
				require.NotContains(t, err.Error(), "upgrade the server")
				switch status {
				case http.StatusUnauthorized, http.StatusForbidden:
					require.ErrorContains(t, err, "authorization")
				case http.StatusNotFound, http.StatusMethodNotAllowed:
					require.ErrorContains(t, err, "endpoint is unavailable")
				}
				if status == http.StatusNotFound {
					require.ErrorIs(t, err, ErrNotFound)
				}
				if status == http.StatusServiceUnavailable {
					require.ErrorIs(t, err, ErrServerShuttingDown)
				}
				require.Equal(t, int32(1), gets.Load())
				require.Zero(t, privateRequests.Load())
			})
		}
	}
}

func TestClientRefreshCompletionAcknowledgementStatus(t *testing.T) {
	for _, status := range []proto.WorkspaceChannelStatus{
		proto.WorkspaceChannelStatusOK,
		proto.WorkspaceChannelStatusConflict,
		proto.WorkspaceChannelStatusNotFound,
		proto.WorkspaceChannelStatusInternal,
	} {
		t.Run(string(status), func(t *testing.T) {
			principal := strings.Repeat("a", 64)
			priorProposal := config.RemoteRuntimeProposal{Version: 1, Revision: 1}
			priorProposal.Digest, _ = config.RemoteRuntimeDigest(priorProposal)
			prior := &config.RemoteAuthority{Mode: "client", Principal: principal, Revision: priorProposal.Revision, Digest: priorProposal.Digest}
			server := newWorkspaceChannelTestServer(t, principal, prior, func(message proto.PeerDecodedMessage) proto.PeerAcknowledgement {
				require.Equal(t, proto.PeerTypeProviderRefreshCompleted, message.Envelope.Type)
				completion := message.Payload.(*proto.PeerProviderRefreshCompletion)
				require.Equal(t, "request", completion.RequestID)
				require.True(t, completion.Failed)
				return proto.PeerAcknowledgement{Status: status, Message: workspaceChannelTestMessage(status)}
			})
			defer server.Close()
			c := &Client{h: server.Client(), network: "tcp", addr: strings.TrimPrefix(server.URL, "https://"), secure: true, clientID: "test"}
			c.retainWorkspaceAttachment("workspace", prior)
			err := c.CompleteClientRefresh(t.Context(), "workspace", config.ClientRefreshCompletion{RequestID: "request", Failed: true})
			if status == proto.WorkspaceChannelStatusOK {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrClientRefreshRejected)
			var commandErr *WorkspaceChannelCommandError
			require.ErrorAs(t, err, &commandErr)
			require.Equal(t, status, commandErr.Status)
		})
	}
}

func TestClientRuntimeNegotiatesBeforePrivatePost(t *testing.T) {
	for _, kind := range []string{"missing", "old-version", "wrong-compiler", "pre-catalog-compiler", "pre-native-identity-compiler", "pre-namespace-free-oauth-compiler", "pre-ordered-presence-compiler", "pre-provider-identity-compiler", "small-limit", "compatible", "different-development-version", "codebase-supported", "codebase-unsupported"} {
		t.Run(kind, func(t *testing.T) {
			var gets, posts atomic.Int32
			secret := "synthetic-private-client-key"
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/runtime-capabilities" {
					gets.Add(1)
					require.Zero(t, r.ContentLength)
					if kind == "missing" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					value := proto.RemoteRuntimeCapabilities{PeerChannel: proto.PeerChannelProtocol, IncrementalState: true, Protocol: proto.RemoteRuntimeProtocol, RuntimeVersion: config.RemoteRuntimeVersion, Compiler: config.RemoteRuntimeCompiler, Principal: strings.Repeat("a", 64), MaxRequestBytes: config.MaxRemoteRuntimeBytes, MaxBundles: 64, MaxProviders: 64, WorkspaceSharing: "exclusive-certificate"}
					value.CodebaseIndex = kind == "codebase-supported"
					if kind == "different-development-version" {
						value.HostVersion = "v0.93.1-0.20260901000000-111111111111"
					}
					if kind == "old-version" {
						value.RuntimeVersion = 0
					}
					if kind == "pre-catalog-compiler" {
						value.Compiler = "crux-declarative-runtime-v17"
					}
					if kind == "pre-native-identity-compiler" {
						value.Compiler = "crux-declarative-runtime-v18"
					}
					if kind == "pre-namespace-free-oauth-compiler" {
						value.Compiler = "crux-declarative-runtime-v19"
					}
					if kind == "pre-ordered-presence-compiler" {
						value.Compiler = "crux-declarative-runtime-v22"
					}
					if kind == "pre-provider-identity-compiler" {
						value.Compiler = "crux-declarative-runtime-v23"
					}
					if kind == "wrong-compiler" {
						value.Compiler = "unsupported"
					}
					if kind == "small-limit" {
						value.MaxRequestBytes = 1
					}
					require.NoError(t, json.NewEncoder(w).Encode(value))
					return
				}
				posts.Add(1)
				require.Equal(t, int32(1), gets.Load())
				require.Equal(t, proto.RemoteRuntimeProtocol, r.Header.Get("Crux-Runtime-Protocol"))
				var request proto.CreateWorkspaceRequest
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				require.Equal(t, "v0.93.1-0.20260909000000-222222222222", request.Version)
				require.Equal(t, secret, request.Runtime.Credentials[0].APIKey)
				require.NoError(t, json.NewEncoder(w).Encode(proto.Workspace{Authority: &config.RemoteAuthority{Mode: "client", Principal: strings.Repeat("a", 64), Revision: 1, Digest: request.Runtime.Digest}}))
			}))
			defer server.Close()
			c := &Client{h: server.Client(), network: "tcp", addr: strings.TrimPrefix(server.URL, "https://"), secure: true, clientID: "test"}
			proposal := &config.RemoteRuntimeProposal{Version: 1, Revision: 1, Credentials: []config.RemoteCredentialBinding{{APIKey: secret}}}
			if strings.HasPrefix(kind, "codebase-") {
				proposal.CodebaseIndex = &config.RemoteCodebaseIndex{Settings: config.ToolCodebaseSearch{Enabled: new(true)}, AccessToken: "synthetic-codebase-token"}
			}
			proposal.Digest, _ = config.RemoteRuntimeDigest(*proposal)
			_, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: "/workspace", Runtime: proposal, Version: "v0.93.1-0.20260909000000-222222222222"})
			if kind == "compatible" || kind == "different-development-version" || kind == "codebase-supported" {
				require.NoError(t, err)
				require.Equal(t, int32(1), posts.Load())
			} else {
				require.Error(t, err)
				require.Zero(t, posts.Load())
			}
			require.Equal(t, int32(1), gets.Load())
		})
	}
}

func TestClientRuntimeAcknowledgementIdentifiesMismatchedFields(t *testing.T) {
	for _, operation := range []string{"create", "replace"} {
		for _, field := range []string{"matching", "missing", "mode", "principal", "revision", "digest"} {
			t.Run(operation+"/"+field, func(t *testing.T) {
				principal := strings.Repeat("a", 64)
				proposal := config.RemoteRuntimeProposal{Version: 1, Revision: 1}
				if operation == "replace" {
					proposal.Revision = 2
				}
				proposal.Digest, _ = config.RemoteRuntimeDigest(proposal)
				ack := &config.RemoteAuthority{Mode: "client", Principal: principal, Revision: proposal.Revision, Digest: proposal.Digest}
				switch field {
				case "missing":
					ack = nil
				case "mode":
					ack.Mode = "server"
				case "principal":
					ack.Principal = "unexpected-private-response"
				case "revision":
					ack.Revision++
				case "digest":
					ack.Digest = "unexpected-private-response"
				}
				var server *httptest.Server
				if operation == "create" {
					server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/v1/runtime-capabilities" {
							require.NoError(t, json.NewEncoder(w).Encode(workspaceChannelTestCapabilities(principal)))
							return
						}
						require.NoError(t, json.NewEncoder(w).Encode(proto.Workspace{ID: "workspace", Authority: ack}))
					}))
				} else {
					priorProposal := config.RemoteRuntimeProposal{Version: 1, Revision: 1}
					priorProposal.Digest, _ = config.RemoteRuntimeDigest(priorProposal)
					server = newWorkspaceChannelTestServer(t, principal, &config.RemoteAuthority{Mode: "client", Principal: principal, Revision: priorProposal.Revision, Digest: priorProposal.Digest}, func(message proto.PeerDecodedMessage) proto.PeerAcknowledgement {
						require.Equal(t, proto.PeerTypeRuntimeReplace, message.Envelope.Type)
						replace := message.Payload.(*proto.PeerRuntimeReplace)
						require.Equal(t, uint64(1), replace.ExpectedRevision)
						require.Equal(t, proposal, replace.Runtime)
						return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK, Authority: ack}
					})
				}
				defer server.Close()
				c := &Client{h: server.Client(), network: "tcp", addr: strings.TrimPrefix(server.URL, "https://"), secure: true, clientID: "test"}
				var err error
				if operation == "create" {
					_, err = c.CreateWorkspace(t.Context(), proto.Workspace{Path: "/workspace", Runtime: &proposal})
				} else {
					priorProposal := config.RemoteRuntimeProposal{Version: 1, Revision: 1}
					priorProposal.Digest, _ = config.RemoteRuntimeDigest(priorProposal)
					c.retainWorkspaceAttachment("workspace", &config.RemoteAuthority{Mode: "client", Principal: principal, Revision: priorProposal.Revision, Digest: priorProposal.Digest})
					_, err = c.ReplaceRemoteRuntime(t.Context(), "workspace", 1, proposal)
				}
				if field == "matching" {
					require.NoError(t, err)
					return
				}
				require.ErrorContains(t, err, "acknowledgement does not match")
				if field != "missing" {
					require.ErrorContains(t, err, "mismatched fields: "+field)
				}
				require.NotContains(t, err.Error(), "unexpected-private-response")
			})
		}
	}
}
