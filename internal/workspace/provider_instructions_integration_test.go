package workspace_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestProviderInstructionsThroughTLS(t *testing.T) {
	xdgIsolate(t)
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv("CRUX_PROVIDER_PROFILE", "plugin-compat")
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	serverConfig := filepath.Join(os.Getenv("CRUX_GLOBAL_CONFIG"), "crux.json")
	serverConfigBytes := []byte(`{"providers":{"example-responses":{"api_key":"synthetic-wrong-host-key","tooling_instructions":"crux"}}}`)
	require.NoError(t, os.WriteFile(serverConfig, serverConfigBytes, 0o600))
	serverCode, err := connection.EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	identity, err := connection.NewClientIdentity("instructions-client")
	require.NoError(t, err)
	require.NoError(t, connection.AuthorizeClient(t.Context(), "instructions-client", identity.Certificate))
	tlsConfig, err := connection.ServerTLSConfig(t.Context())
	require.NoError(t, err)
	s := server.NewServer(nil, "tcp", "127.0.0.1:0")
	require.NoError(t, s.EnableNetworkAuth(t.Context()))
	var rejectPublication, loseAcknowledgement atomic.Bool
	var publications atomic.Int32
	proxyTLS, err := connection.ClientTLSConfig(connection.Connection{ServerCertificate: serverCode, Client: identity})
	require.NoError(t, err)
	var lostCommands sync.Map
	remote := startPeerChannelProxyServer(t, s.Handler(), tlsConfig, func(*http.Request) *tls.Config { return proxyTLS }, func(fromClient bool, envelope proto.PeerEnvelope) workspaceChannelProxyDecision {
		if fromClient && envelope.Type == proto.PeerTypeRuntimeTransaction {
			publications.Add(1)
			if rejectPublication.Swap(false) {
				// Corrupt the expected digest so the real server
				// genuinely rejects the transaction; the peer-channel leg
				// has no synthesized-reply path, only real rejection.
				var transaction proto.PeerRuntimeTransaction
				if json.Unmarshal(envelope.Payload, &transaction) != nil {
					t.Error("cannot decode intercepted peer runtime transaction")
					return workspaceChannelProxyDecision{drop: true, close: true}
				}
				originalDigest := transaction.Runtime.ExpectedDigest
				transaction.Runtime.ExpectedDigest = strings.Repeat("f", 64)
				if transaction.Runtime.ExpectedDigest == originalDigest {
					transaction.Runtime.ExpectedDigest = strings.Repeat("e", 64)
				}
				payload, encodeErr := json.Marshal(transaction)
				if encodeErr != nil {
					t.Errorf("cannot encode intercepted peer runtime transaction: %v", encodeErr)
					return workspaceChannelProxyDecision{drop: true, close: true}
				}
				replacement := envelope
				replacement.Payload = payload
				return workspaceChannelProxyDecision{replacement: &replacement}
			}
			if loseAcknowledgement.Swap(false) {
				lostCommands.Store(envelope.MessageID, struct{}{})
			}
		}
		if !fromClient && envelope.Type == proto.PeerTypeAcknowledgement {
			if _, ok := lostCommands.LoadAndDelete(envelope.ReplyTo); ok {
				return workspaceChannelProxyDecision{drop: true, close: true}
			}
		}
		return workspaceChannelProxyDecision{}
	})
	t.Cleanup(func() { _ = s.Close() })

	type observedRequest struct{ body, instructions string }
	var requestMu sync.Mutex
	var requests []observedRequest
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/responses", r.URL.Path)
		assert.Equal(t, "Bearer synthetic-instructions-access", r.Header.Get("Authorization"))
		data, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}
		var instructions []string
		gjson.GetBytes(data, "input").ForEach(func(_, item gjson.Result) bool {
			role := item.Get("role").String()
			if role == "system" || role == "developer" {
				content := item.Get("content")
				if content.Type == gjson.String {
					instructions = append(instructions, content.String())
				} else {
					content.ForEach(func(_, part gjson.Result) bool {
						instructions = append(instructions, part.Get("text").String())
						return true
					})
				}
			}
			return true
		})
		requestMu.Lock()
		requests = append(requests, observedRequest{string(data), strings.Join(instructions, "\n")})
		id := fmt.Sprintf("instruction_fixture_%d", len(requests))
		requestMu.Unlock()
		writeRefreshFixtureSSE(w, id)
	}))
	t.Cleanup(provider.Close)
	target, err := url.Parse(provider.URL)
	require.NoError(t, err)
	previousTransport := http.DefaultTransport
	http.DefaultTransport = refreshFixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != target.Scheme || r.URL.Host != target.Host {
			return nil, fmt.Errorf("unexpected instruction fixture destination %s", r.URL.Host)
		}
		return provider.Client().Transport.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	clientHome, configDir, dataDir, cacheDir := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", clientHome)
	t.Setenv("USERPROFILE", clientHome)
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv("CRUX_GLOBAL_CONFIG", configDir)
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	t.Setenv("CRUX_CACHE_DIR", cacheDir)
	const nativeText = "Exact client native tooling instruction marker."
	installInstructionsFixture(t, provider.URL, dataDir, cacheDir, nativeText)
	require.NoError(t, accounts.Save(t.Context(), "example.responses", accounts.Entry{ID: "selected", AccessToken: "synthetic-instructions-access", RefreshToken: "synthetic-instructions-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}))
	clientConfig := filepath.Join(configDir, "crux.json")
	require.NoError(t, os.WriteFile(clientConfig, []byte(`{"providers":{"example-responses":{"api_key":"synthetic-instructions-access","plugin":{"id":"example.responses-oauth","version":"1.2.0"},"configuration":{"oauth_client_id":"synthetic-client"}}},"models":{"large":{"provider":"example-responses","model":"example-reasoner"},"small":{"provider":"example-responses","model":"example-small"}}}`), 0o600))
	instructionDir := filepath.Join(clientHome, ".ai-cli", "instructions")
	require.NoError(t, os.MkdirAll(instructionDir, 0o700))
	instructionFile := filepath.Join(instructionDir, "example-responses.txt")
	const initialContext = "Exact client provider context first marker."
	const editedContext = "Exact client provider context edited marker."
	require.NoError(t, os.WriteFile(instructionFile, []byte(initialContext), 0o600))
	local, err := config.Load(t.TempDir(), t.TempDir(), false)
	require.NoError(t, err)
	owner, ok := local.Config().ProviderOwner("example-responses")
	require.True(t, ok)
	proposal, err := local.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, initialContext, proposal.ProviderContextInstructions[owner.ProviderID])
	c, err := client.NewAuthenticatedClient(t.TempDir(), connection.Connection{Address: "tcp://" + strings.TrimPrefix(remote.URL, "https://"), ServerCertificate: serverCode, Client: identity})
	require.NoError(t, err)
	c.SetLocalRuntimeStore(local)
	created, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: t.TempDir(), AuthorityMode: "client", Runtime: &proposal})
	require.NoError(t, err)
	w := workspace.NewClientWorkspace(c, *created)
	t.Cleanup(w.Shutdown)
	require.NoError(t, w.InitCoderAgentNonInteractive(t.Context()))
	receiver, err := s.Backend().GetWorkspace(created.ID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	events, err := c.SubscribeEvents(ctx, created.ID)
	require.NoError(t, err)
	run := func(phase, tooling, providerContext string) {
		t.Helper()
		require.NoError(t, w.UpdateAgentModel(ctx, w.Config().AgentModelState()))
		session, err := c.CreateSession(ctx, created.ID, "Instructions "+phase)
		require.NoError(t, err)
		marker := "provider-instructions-" + phase
		require.NoError(t, c.SendMessageWithPermissionMode(ctx, created.ID, session.ID, marker, marker+": Return the fixture response.", proto.AgentPermissionDeny))
		events = awaitRefreshFixtureRun(t, ctx, c, created.ID, events, w, marker)
		requestMu.Lock()
		defer requestMu.Unlock()
		var matching []observedRequest
		for _, request := range requests {
			if strings.Contains(request.body, marker) && gjson.Get(request.body, "model").String() == "example-reasoner" {
				matching = append(matching, request)
			}
		}
		require.Len(t, matching, 1, "one foreground request must execute for each instruction phase")
		actual := matching[0].instructions
		if tooling == "native" {
			require.Contains(t, actual, nativeText)
		} else {
			require.NotContains(t, actual, nativeText)
		}
		if tooling == "crux" {
			require.Contains(t, actual, "<critical_rules>")
		} else {
			require.NotContains(t, actual, "<critical_rules>")
		}
		if providerContext != "" {
			require.Contains(t, actual, providerContext)
		}
		for _, excluded := range []string{initialContext, editedContext} {
			if excluded != providerContext {
				require.NotContains(t, actual, excluded)
			}
		}
	}
	run("native-default", "native", initialContext)
	require.NoError(t, w.SetProviderToolingInstructions(config.ScopeGlobal, owner, "crux"))
	require.Equal(t, uint64(2), receiver.Cfg.RemoteAuthority().Revision)
	run("crux-selected", "crux", initialContext)
	require.NoError(t, w.RemoveProviderToolingInstructions(config.ScopeGlobal, owner))
	persisted, err := os.ReadFile(clientConfig)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(persisted, "providers.example-responses.tooling_instructions").Exists())
	run("native-inherited", "native", initialContext)

	require.NoError(t, os.WriteFile(instructionFile, []byte(editedContext), 0o600))
	require.NoError(t, w.ReloadProviderContextInstructions(ctx, owner))
	run("edited-context", "native", editedContext)
	require.NoError(t, w.SetConfigField(config.ScopeGlobal, "options.instruction_mode", "project"))
	run("project-mode", "", editedContext)
	require.NoError(t, w.RemoveConfigField(config.ScopeGlobal, "options.instruction_mode"))
	require.NoError(t, os.Remove(instructionFile))
	require.NoError(t, w.ReloadProviderContextInstructions(ctx, owner))
	run("deleted-context", "native", "")

	// Invalid explicit values and owners cannot write locally or publish.
	accepted := receiver.Cfg.RemoteAuthority()
	beforeCalls := publications.Load()
	persisted, err = os.ReadFile(clientConfig)
	require.NoError(t, err)
	require.Error(t, w.SetProviderToolingInstructions(config.ScopeGlobal, owner, "invalid"))
	changedOwner := owner
	changedOwner.ManifestVersion = "different"
	require.Error(t, w.SetProviderToolingInstructions(config.ScopeGlobal, changedOwner, "crux"))
	afterInvalid, err := os.ReadFile(clientConfig)
	require.NoError(t, err)
	require.Equal(t, persisted, afterInvalid)
	require.Equal(t, accepted, receiver.Cfg.RemoteAuthority())
	require.Equal(t, beforeCalls, publications.Load())

	// Rejection leaves a saved local edit pending and preserves the accepted prompt.
	rejectPublication.Store(true)
	require.ErrorContains(t, w.SetProviderToolingInstructions(config.ScopeGlobal, owner, "crux"), "client state saved")
	require.Equal(t, accepted, receiver.Cfg.RemoteAuthority())
	localProvider, _ := local.Config().Providers.Get(owner.ProviderID)
	require.Equal(t, "crux", localProvider.ToolingInstructions)
	acceptedProvider, _ := w.Config().Providers.Get(owner.ProviderID)
	require.Empty(t, acceptedProvider.ToolingInstructions)
	run("rejected-publication", "native", "")

	// A subsequent publication reconciles retained local state; an applied command
	// whose acknowledgement is lost succeeds only after GET confirms its exact digest.
	loseAcknowledgement.Store(true)
	require.NoError(t, w.SetProviderToolingInstructions(config.ScopeGlobal, owner, "crux"))
	run("lost-ack-reconciled", "crux", "")
	beforeContext := receiver.Cfg.RemoteAuthority()
	require.NoError(t, os.WriteFile(instructionFile, []byte(editedContext), 0o600))
	rejectPublication.Store(true)
	require.ErrorContains(t, w.ReloadProviderContextInstructions(ctx, owner), "client state saved")
	require.Equal(t, beforeContext, receiver.Cfg.RemoteAuthority())
	run("rejected-context", "crux", "")
	require.NoError(t, w.ReloadProviderContextInstructions(ctx, owner))
	run("context-retried", "crux", editedContext)
	discovery, err := c.GetWorkspace(ctx, created.ID)
	require.NoError(t, err)
	public, err := json.Marshal(discovery)
	require.NoError(t, err)
	require.NotContains(t, string(public), editedContext)
	require.NotContains(t, string(public), "synthetic-instructions")
	actualServerConfig, err := os.ReadFile(serverConfig)
	require.NoError(t, err)
	require.Equal(t, serverConfigBytes, actualServerConfig)
}

func installInstructionsFixture(t *testing.T, endpoint, dataDir, cacheDir, text string) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "instructions.plugin")
	require.NoError(t, os.MkdirAll(source, 0o700))
	require.NoError(t, os.CopyFS(source, os.DirFS(filepath.Join("..", "..", "docs", "provider-plugins", "examples", "responses-oauth.plugin"))))
	data, err := os.ReadFile(filepath.Join(source, "manifest.json"))
	require.NoError(t, err)
	value, err := manifest.DecodeStrict(data)
	require.NoError(t, err)
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	for i := range value.Capabilities.Endpoints {
		item := &value.Capabilities.Endpoints[i]
		item.BaseURL = endpoint
		if item.ID != "api" {
			item.BaseURL += "/" + item.ID
		}
		item.AllowedHosts, item.AllowedSchemes = []string{target.Hostname()}, []string{target.Scheme}
	}
	value.Capabilities.Instructions.SelectionDefault = "native"
	data, err = json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "instructions", "native.txt"), []byte(text), 0o600))
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(dataDir, cacheDir))
	require.NoError(t, err)
	defer manager.Close()
	_, err = manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, Trust: true, ExpectedRevision: manager.Snapshot().Revision})
	require.NoError(t, err)
}
