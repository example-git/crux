package server_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/server"
	"github.com/stretchr/testify/require"
)

func TestLiveRevocationDrainsRealInferenceAndStreamOnSameDaemon(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "CRUX_GLOBAL_DATA", "CRUX_GLOBAL_CONFIG", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		path := filepath.Join(root, name)
		t.Setenv(name, path)
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	entered := make(chan struct{})
	var enterOnce sync.Once
	var active, cancelled, conversationCancelled, retainedCalls atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected fixture route", http.StatusNotFound)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer synthetic-revoked-credential":
			active.Add(1)
			defer active.Add(-1)
			conversation := r.Header.Get("x-request-purpose") == "conversation"
			if conversation {
				enterOnce.Do(func() { close(entered) })
			}
			<-r.Context().Done()
			cancelled.Add(1)
			if conversation {
				conversationCancelled.Add(1)
			}
		case "Bearer synthetic-retained-credential":
			retainedCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"id\":\"retained\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"retained daemon inference verified\"},\"finish_reason\":null}]}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"id\":\"retained\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
		default:
			http.Error(w, "wrong fixture credential", http.StatusUnauthorized)
		}
	}))
	t.Cleanup(provider.Close)
	originalTransport := http.DefaultTransport
	fixtureTransport := provider.Client().Transport.(*http.Transport).Clone()
	http.DefaultTransport = fixtureTransport
	t.Cleanup(func() { fixtureTransport.CloseIdleConnections(); http.DefaultTransport = originalTransport })

	serverCode, err := connection.EnsureServerIdentity(ctx)
	require.NoError(t, err)
	identities := map[string]connection.Identity{}
	for _, name := range []string{"revoked", "retained"} {
		identity, err := connection.NewClientIdentity(name)
		require.NoError(t, err)
		require.NoError(t, connection.AuthorizeClient(ctx, name, identity.Certificate))
		identities[name] = identity
	}
	srv := server.NewServer(nil, "tcp", "127.0.0.1:0")
	require.NoError(t, srv.SetWorkspaceRoots([]string{root}))
	require.NoError(t, srv.EnableNetworkAuth(t.Context()))
	tlsConfig, err := connection.ServerTLSConfig(t.Context())
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(tls.NewListener(listener, tlsConfig)) }()
	t.Cleanup(func() {
		_ = srv.Close()
		for _, listed := range srv.Backend().ListWorkspaces() {
			ws, err := srv.Backend().GetWorkspace(listed.ID)
			if err == nil {
				ws.Shutdown()
			}
		}
		srv.Backend().Shutdown()
		select {
		case err := <-served:
			require.ErrorIs(t, err, http.ErrServerClosed)
		case <-time.After(5 * time.Second):
			t.Error("production listener did not stop")
		}
	})

	clients := map[string]*client.Client{}
	workspaces := map[string]*proto.Workspace{}
	sessions := map[string]string{}
	streams := map[string]<-chan any{}
	for _, name := range []string{"revoked", "retained"} {
		owner := providerregistry.RegistrationOwner{ProviderID: "live-fixture"}
		proposal := config.RemoteRuntimeProposal{
			Version: config.RemoteRuntimeVersion, Revision: 1,
			Providers: []config.RemoteProviderDefinition{{Config: config.ProviderConfig{
				ID: owner.ProviderID, Name: "Live fixture", Type: catalog.TypeOpenAICompat, BaseURL: provider.URL + "/v1",
				Owner:  &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
				Models: []catalog.Model{{ID: "fixture", Name: "Fixture", ContextWindow: 8192, DefaultMaxTokens: 256}},
			}}},
			Models: map[config.SelectedModelType]config.SelectedModel{
				config.SelectedModelTypeLarge: {Provider: owner.ProviderID, Model: "fixture"},
				config.SelectedModelTypeSmall: {Provider: owner.ProviderID, Model: "fixture"},
			},
			Credentials: []config.RemoteCredentialBinding{{Owner: owner, Generation: 1, APIKey: "synthetic-" + name + "-credential"}},
		}
		proposal.Digest, err = config.RemoteRuntimeDigest(proposal)
		require.NoError(t, err)
		c, err := client.NewAuthenticatedClient(filepath.Join(root, "client-"+name), connection.Connection{
			Address: "tcp://" + listener.Addr().String(), ServerCertificate: serverCode, Client: identities[name],
		})
		require.NoError(t, err)
		path := filepath.Join(root, "workspace-"+name)
		require.NoError(t, os.MkdirAll(path, 0o700))
		ws, err := c.CreateWorkspace(ctx, proto.Workspace{Path: path, Runtime: &proposal, AuthorityMode: "client"})
		require.NoError(t, err)
		require.NoError(t, c.InitiateAgentProcessing(ctx, ws.ID, false))
		session, err := c.CreateSession(ctx, ws.ID, "Live revocation acceptance")
		require.NoError(t, err)
		events, err := c.SubscribeEvents(ctx, ws.ID, *ws.Authority)
		require.NoError(t, err)
		clients[name], workspaces[name], sessions[name], streams[name] = c, ws, session.ID, events
	}
	streamEnded := make(chan struct{})
	go func() {
		for range streams["revoked"] {
		}
		close(streamEnded)
	}()
	require.NoError(t, clients["revoked"].SendMessageWithPermissionMode(ctx, workspaces["revoked"].ID, sessions["revoked"], "active-revoked-run", "Return the fixture response.", proto.AgentPermissionDeny))
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("real inference did not reach the disposable provider")
	}
	require.Positive(t, active.Load())
	outcome, err := connection.RevokeClientWithOutcome(ctx, "revoked", "")
	require.NoError(t, err)
	require.True(t, outcome.Saved)
	require.Equal(t, "acknowledged", outcome.Resolution)
	require.Len(t, outcome.Daemons, 1)
	require.True(t, outcome.Daemons[0].Acknowledged)
	require.Eventually(t, func() bool { return active.Load() == 0 && cancelled.Load() > 0 && conversationCancelled.Load() > 0 }, 5*time.Second, 10*time.Millisecond, "foreground provider transport must observe real cancellation")
	select {
	case <-streamEnded:
	case <-time.After(5 * time.Second):
		t.Fatal("revoked event stream remained open")
	}
	_, err = clients["revoked"].GetWorkspace(ctx, workspaces["revoked"].ID)
	require.Error(t, err)
	_, err = srv.Backend().GetWorkspace(workspaces["revoked"].ID)
	require.Error(t, err, "revoked workspace must no longer be discoverable after acknowledgement")

	retained, err := clients["retained"].GetWorkspace(ctx, workspaces["retained"].ID)
	require.NoError(t, err)
	require.Equal(t, workspaces["retained"].Authority, retained.Authority)
	require.NoError(t, clients["retained"].SendMessageWithPermissionMode(ctx, retained.ID, sessions["retained"], "retained-after-revoke", "Return the fixture response.", proto.AgentPermissionDeny))
	for {
		select {
		case event, ok := <-streams["retained"]:
			require.True(t, ok, "retained stream closed after another principal's revocation")
			finished, ok := event.(pubsub.Event[proto.RunComplete])
			if !ok || finished.Payload.RunID != "retained-after-revoke" {
				continue
			}
			require.Empty(t, finished.Payload.Error)
			require.True(t, strings.Contains(finished.Payload.Text, "retained daemon inference verified"))
			require.Positive(t, retainedCalls.Load())
			return
		case <-ctx.Done():
			t.Fatal("retained inference did not complete on the same daemon")
		}
	}
}
