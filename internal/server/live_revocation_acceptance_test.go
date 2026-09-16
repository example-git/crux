package server_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
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
	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/session"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

func TestLiveRevocationDrainsRealInferenceAndStreamOnSameDaemon(t *testing.T) {
	testLiveCredentialWorkLifetime(t, false, false)
}

func TestLiveRevocationDrainsDetachedAgentOnSameDaemon(t *testing.T) {
	testLiveCredentialWorkLifetime(t, true, false)
}

func TestFinalDisconnectGraceDrainsRealInferenceAndTitle(t *testing.T) {
	testLiveCredentialWorkLifetime(t, false, true)
}

func TestFinalDisconnectGraceDrainsDetachedAgent(t *testing.T) {
	testLiveCredentialWorkLifetime(t, true, true)
}

func testLiveCredentialWorkLifetime(t *testing.T, detached, disconnect bool) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "CRUX_GLOBAL_DATA", "CRUX_GLOBAL_CONFIG", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		path := filepath.Join(root, name)
		t.Setenv(name, path)
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	const disconnectGrace = time.Second
	if disconnect {
		t.Setenv("CRUX_SERVER_DETACH_GRACE", fmt.Sprint(int(disconnectGrace/time.Second)))
	}
	entered := make(chan struct{})
	titleEntered := make(chan struct{})
	childSessionHashes := make(chan string, 1)
	var parentSessionHash atomic.Value
	parentSessionHash.Store("")
	const childPrompt = "synthetic detached credential drain marker"
	const toolCallID = "call_detached_revocation"
	var enterOnce, titleOnce sync.Once
	var toolCalls atomic.Int32
	var active, cancelled, conversationCancelled, titleCancelled, retainedCalls atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid fixture body", http.StatusBadRequest)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected fixture route", http.StatusNotFound)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer synthetic-revoked-credential":
			conversation := r.Header.Get("x-request-purpose") == "conversation"
			if detached {
				if !conversation {
					writeLiveRevocationText(w, "Disposable session title")
					return
				}
				if r.Header.Get("x-session-id") == parentSessionHash.Load().(string) {
					var request struct {
						Messages []struct {
							Role       string `json:"role"`
							ToolCallID string `json:"tool_call_id"`
						} `json:"messages"`
						Tools []struct {
							Function struct {
								Name string `json:"name"`
							} `json:"function"`
						} `json:"tools"`
					}
					if json.Unmarshal(body, &request) != nil {
						http.Error(w, "invalid parent request", http.StatusBadRequest)
						return
					}
					for _, message := range request.Messages {
						if message.Role == "tool" && message.ToolCallID == toolCallID {
							writeLiveRevocationText(w, "Parent finished while its child remains active.")
							return
						}
					}
					var advertised bool
					for _, tool := range request.Tools {
						advertised = advertised || tool.Function.Name == "agent"
					}
					if !advertised || !toolCalls.CompareAndSwap(0, 1) {
						http.Error(w, "agent tool must be advertised and called once", http.StatusBadRequest)
						return
					}
					arguments, _ := json.Marshal(map[string]any{"prompt": childPrompt, "run_in_background": true})
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: {\"id\":\"parent\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":\"agent\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", toolCallID, string(arguments))
					_, _ = fmt.Fprint(w, "data: {\"id\":\"parent\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
					return
				}
				if r.Header.Get("x-session-id") == "" || !strings.Contains(string(body), childPrompt) {
					http.Error(w, "unexpected child request", http.StatusBadRequest)
					return
				}
			}
			active.Add(1)
			defer active.Add(-1)
			if conversation {
				enterOnce.Do(func() {
					if detached {
						childSessionHashes <- r.Header.Get("x-session-id")
					}
					close(entered)
				})
			}
			if r.Header.Get("x-request-purpose") == "title" {
				titleOnce.Do(func() { close(titleEntered) })
			}
			<-r.Context().Done()
			cancelled.Add(1)
			if conversation {
				conversationCancelled.Add(1)
			}
			if r.Header.Get("x-request-purpose") == "title" {
				titleCancelled.Add(1)
			}
		case "Bearer synthetic-retained-credential":
			retainedCalls.Add(1)
			writeLiveRevocationText(w, "retained daemon inference verified")
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
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(tls.NewListener(listener, tlsConfig)) }()
	t.Cleanup(func() {
		// This runs only after the acceptance assertions. Force fixture sockets
		// closed so a failed cancellation assertion is reported instead of being
		// hidden behind httptest.Close waiting forever for the leaked request.
		provider.CloseClientConnections()
		shutdown := make(chan struct{})
		go func() {
			_ = srv.Close()
			for _, listed := range srv.Backend().ListWorkspaces() {
				ws, err := srv.Backend().GetWorkspace(listed.ID)
				if err == nil {
					ws.Shutdown()
				}
			}
			srv.Backend().Shutdown()
			close(shutdown)
		}()
		select {
		case <-shutdown:
		case <-time.After(10 * time.Second):
			t.Error("production workspace cleanup did not finish after fixture sockets closed")
		}
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
	streamCancels := map[string]context.CancelFunc{}
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
		streamCtx, cancelStream := context.WithCancel(ctx)
		t.Cleanup(cancelStream)
		events, err := c.SubscribeEvents(streamCtx, ws.ID, *ws.Authority)
		require.NoError(t, err)
		clients[name], workspaces[name], sessions[name], streams[name] = c, ws, session.ID, events
		streamCancels[name] = cancelStream
	}
	parentSessionHash.Store(session.HashID(sessions["revoked"]))
	streamEnded := make(chan struct{})
	parentFinished := make(chan proto.RunComplete, 1)
	go func() {
		for event := range streams["revoked"] {
			if finished, ok := event.(pubsub.Event[proto.RunComplete]); ok && finished.Payload.RunID == "active-revoked-run" {
				select {
				case parentFinished <- finished.Payload:
				default:
				}
			}
		}
		close(streamEnded)
	}()
	require.NoError(t, clients["revoked"].SendMessageWithPermissionMode(ctx, workspaces["revoked"].ID, sessions["revoked"], "active-revoked-run", "Return the fixture response.", proto.AgentPermissionDeny))
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("real inference did not reach the disposable provider")
	}
	if !detached {
		select {
		case <-titleEntered:
		case <-ctx.Done():
			t.Fatal("detached title did not reach the disposable provider")
		}
	}
	require.Positive(t, active.Load())
	var detachedTask managedtask.View
	var taskCoordinator agent.TaskCoordinator
	if detached {
		select {
		case finished := <-parentFinished:
			require.Empty(t, finished.Error)
			require.Contains(t, finished.Text, "Parent finished while its child remains active.")
		case <-ctx.Done():
			t.Fatal("parent did not finish while its background child remained active")
		}
		tasks, err := clients["revoked"].ListTasks(ctx, workspaces["revoked"].ID)
		require.NoError(t, err)
		require.Len(t, tasks, 1)
		require.Equal(t, managedtask.TypeAgent, tasks[0].Type)
		require.Equal(t, managedtask.StatusRunning, tasks[0].State.Status)
		// ListTasks exposes identity/state; TaskOutput carries the full agent
		// ownership and child session, both fetched through the real mTLS API.
		output, err := clients["revoked"].TaskOutput(ctx, workspaces["revoked"].ID, tasks[0].ID, false, 0)
		require.NoError(t, err)
		detachedTask = output.Task
		require.Equal(t, tasks[0].ID, detachedTask.ID)
		require.Equal(t, managedtask.StatusRunning, detachedTask.State.Status)
		require.Equal(t, sessions["revoked"], detachedTask.Ownership.ParentSessionID)
		require.Equal(t, toolCallID, detachedTask.Ownership.OriginToolCallID)
		require.NotEmpty(t, detachedTask.ChildSessionID)
		require.Equal(t, session.HashID(detachedTask.ChildSessionID), <-childSessionHashes)
		require.Equal(t, int32(1), toolCalls.Load())
		require.Equal(t, int32(1), active.Load(), "only the detached child's transport remains active after parent completion")
		local, err := srv.Backend().GetWorkspace(workspaces["revoked"].ID)
		require.NoError(t, err)
		var ok bool
		taskCoordinator, ok = local.CurrentAgentCoordinator().(agent.TaskCoordinator)
		require.True(t, ok)
	}
	if disconnect {
		// A second real channel claim must keep the credential-bearing request
		// alive even after the first stream has been absent longer than grace.
		remainingCtx, cancelRemaining := context.WithCancel(ctx)
		defer cancelRemaining()
		remaining, err := clients["revoked"].SubscribeEvents(remainingCtx, workspaces["revoked"].ID, *workspaces["revoked"].Authority)
		require.NoError(t, err)
		remainingEnded := make(chan struct{})
		go func() {
			for range remaining {
			}
			close(remainingEnded)
		}()
		streamCancels["revoked"]()
		select {
		case <-streamEnded:
		case <-ctx.Done():
			t.Fatal("first client stream did not close")
		}
		require.Never(t, func() bool { return active.Load() == 0 || cancelled.Load() != 0 }, disconnectGrace+100*time.Millisecond, 10*time.Millisecond, "another live stream must preserve foreground and detached work")
		_, err = clients["revoked"].GetWorkspace(ctx, workspaces["revoked"].ID)
		require.NoError(t, err)
		cancelRemaining()
		select {
		case <-remainingEnded:
		case <-ctx.Done():
			t.Fatal("final client stream did not close")
		}
		require.Never(t, func() bool { return active.Load() == 0 || cancelled.Load() != 0 }, disconnectGrace/3, 10*time.Millisecond, "work must survive the configured grace after final detach")
	} else {
		outcome, err := connection.RevokeClientWithOutcome(ctx, "revoked", "")
		require.NoError(t, err)
		require.True(t, outcome.Saved)
		require.Equal(t, "acknowledged", outcome.Resolution)
		require.Len(t, outcome.Daemons, 1)
		require.True(t, outcome.Daemons[0].Acknowledged)
	}
	require.Eventually(t, func() bool { return active.Load() == 0 && cancelled.Load() > 0 && conversationCancelled.Load() > 0 }, 5*time.Second, 10*time.Millisecond, "credential-bearing provider transport must observe real cancellation")
	if !detached {
		require.Positive(t, titleCancelled.Load(), "acknowledgement must include physical cancellation of the detached title")
	}
	if detached {
		// Read the real retained manager after the workspace has been retired;
		// do not call Drain or Stop from the test to manufacture a terminal task.
		if disconnect {
			require.Eventually(t, func() bool {
				tasks := taskCoordinator.ListTasks()
				return len(tasks) == 1 && tasks[0].State.Status.Terminal()
			}, 5*time.Second, 10*time.Millisecond, "the real task manager must finish after provider cancellation")
		}
		tasks := taskCoordinator.ListTasks()
		require.Len(t, tasks, 1)
		require.Equal(t, detachedTask.ID, tasks[0].ID)
		require.True(t, tasks[0].State.Status.Terminal())
		require.NotEqual(t, managedtask.StatusCompleted, tasks[0].State.Status)
		// ListTasks deliberately projects state to status only. Inspect the
		// full retained result for the end timestamp and exact child identity;
		// this read must not stop, drain, or otherwise finish the task itself.
		output, err := taskCoordinator.TaskOutput(ctx, detachedTask.ID, disconnect, 5*time.Second)
		require.NoError(t, err)
		require.Equal(t, managedtask.RetrievalReady, output.RetrievalStatus)
		require.Equal(t, detachedTask.ID, output.Task.ID)
		require.Equal(t, detachedTask.Ownership, output.Task.Ownership)
		require.Equal(t, detachedTask.ChildSessionID, output.Task.ChildSessionID)
		require.Equal(t, tasks[0].State.Status, output.Task.State.Status)
		require.False(t, output.Task.State.EndedAt.IsZero())
	}
	select {
	case <-streamEnded:
	case <-time.After(5 * time.Second):
		t.Fatal("revoked workspace channel remained open")
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

func writeLiveRevocationText(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\n", text)
	_, _ = fmt.Fprint(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
}
