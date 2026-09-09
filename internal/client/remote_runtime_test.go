package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

func TestClientRefreshCompletionAcknowledgementStatus(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusConflict, http.StatusNotFound, http.StatusBadGateway} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "/v1/workspaces/workspace/runtime/refresh-completion", r.URL.Path)
				require.Equal(t, proto.RemoteRuntimeProtocol, r.Header.Get("Crux-Runtime-Protocol"))
				w.WriteHeader(status)
			}))
			defer server.Close()
			c := &Client{h: server.Client(), network: "tcp", addr: strings.TrimPrefix(server.URL, "https://"), secure: true, clientID: "test"}
			err := c.CompleteClientRefresh(t.Context(), "workspace", config.ClientRefreshCompletion{RequestID: "request", Failed: true})
			switch status {
			case http.StatusNoContent:
				require.NoError(t, err)
			case http.StatusConflict, http.StatusNotFound:
				require.ErrorIs(t, err, ErrClientRefreshRejected)
			default:
				require.Error(t, err)
				require.NotErrorIs(t, err, ErrClientRefreshRejected)
			}
		})
	}
}

func TestClientRuntimeNegotiatesBeforePrivatePost(t *testing.T) {
	for _, kind := range []string{"missing", "old-version", "wrong-compiler", "pre-catalog-compiler", "pre-native-identity-compiler", "small-limit", "compatible"} {
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
					value := proto.RemoteRuntimeCapabilities{Protocol: proto.RemoteRuntimeProtocol, RuntimeVersion: config.RemoteRuntimeVersion, Compiler: config.RemoteRuntimeCompiler, Principal: strings.Repeat("a", 64), MaxRequestBytes: config.MaxRemoteRuntimeBytes, MaxBundles: 64, MaxProviders: 64, WorkspaceSharing: "exclusive-certificate"}
					if kind == "old-version" {
						value.RuntimeVersion = 0
					}
					if kind == "pre-catalog-compiler" {
						value.Compiler = "crux-declarative-runtime-v17"
					}
					if kind == "pre-native-identity-compiler" {
						value.Compiler = "crux-declarative-runtime-v18"
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
				require.Equal(t, secret, request.Runtime.Credentials[0].APIKey)
				require.NoError(t, json.NewEncoder(w).Encode(proto.Workspace{Authority: &config.RemoteAuthority{Mode: "client", Principal: strings.Repeat("a", 64), Revision: 1, Digest: request.Runtime.Digest}}))
			}))
			defer server.Close()
			c := &Client{h: server.Client(), network: "tcp", addr: strings.TrimPrefix(server.URL, "https://"), secure: true, clientID: "test"}
			proposal := &config.RemoteRuntimeProposal{Version: 1, Revision: 1, Credentials: []config.RemoteCredentialBinding{{APIKey: secret}}}
			proposal.Digest, _ = config.RemoteRuntimeDigest(*proposal)
			_, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: "/workspace", Runtime: proposal})
			if kind == "compatible" {
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
