package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
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
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Exercise the production headless command, authenticated runtime publication,
// native Responses adapter, and owning-client OAuth refresh in one run. The
// continuation cases initially omit the provider named by the flags; the
// prepared case admits the flag selection before workspace creation.
// All accounts, plugin files, TLS identities, and HTTP endpoints are disposable.
func TestHeadlessClientModelOverrideAndRefreshThroughTLS(t *testing.T) {
	for _, mode := range []string{"override-and-refresh", "rejected-ack", "prepared-implicit"} {
		t.Run(mode, func(t *testing.T) { testHeadlessClientModelOverrideThroughTLS(t, mode) })
	}
}

func testHeadlessClientModelOverrideThroughTLS(t *testing.T, mode string) {
	for _, key := range []string{"HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	t.Setenv("CRUX_PROVIDER_PROFILE", "plugin-native")
	t.Setenv("CRUX_PROVIDER_PLUGINS", "example-responses")
	t.Setenv("CRUX_PROVIDER_PLUGIN_COMPAT", "")
	t.Setenv("HERDR_PANE_ID", "")

	type inferenceRequest struct{ model, purpose, credential string }
	var mu sync.Mutex
	var requests []inferenceRequest
	var proposals []config.RemoteRuntimeProposal
	var runtimeState config.RemoteRuntimeProposal
	var exchanges, refreshCompletions, providerRequests, agentInitializations atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerRequests.Add(1)
		if r.URL.Path == "/token" {
			assert.NoError(t, r.ParseForm())
			assert.Equal(t, "synthetic-headless-old-refresh", r.Form.Get("refresh_token"))
			exchanges.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"synthetic-headless-new-access","refresh_token":"synthetic-headless-new-refresh","expires_in":3600}`)
			return
		}
		if !assert.Equal(t, "/v1/responses", r.URL.Path) {
			http.Error(w, "unexpected fixture path", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			http.Error(w, "fixture read failed", http.StatusBadRequest)
			return
		}
		request := inferenceRequest{gjson.GetBytes(body, "model").String(), r.Header.Get("x-request-purpose"), r.Header.Get("Authorization")}
		mu.Lock()
		requests = append(requests, request)
		requestID := fmt.Sprintf("headless_%d", len(requests))
		mu.Unlock()
		if request.credential != "Bearer synthetic-headless-new-access" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"synthetic token expired","type":"authentication_error"}}`)
			return
		}
		writeHeadlessAuthoritySSE(w, requestID)
	}))
	t.Cleanup(provider.Close)
	previousTransport := http.DefaultTransport
	transport := provider.Client().Transport.(*http.Transport).Clone()
	providerHost := strings.TrimPrefix(provider.URL, "https://")
	http.DefaultTransport = headlessAuthorityTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != providerHost {
			return nil, fmt.Errorf("unexpected isolated headless provider host %s", r.URL.Host)
		}
		return transport.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport; transport.CloseIdleConnections() })

	serverConfig, serverData := os.Getenv("CRUX_GLOBAL_CONFIG"), os.Getenv("CRUX_GLOBAL_DATA")
	serverFile := filepath.Join(serverConfig, "crux.json")
	serverConfiguration := fmt.Sprintf(`{"providers":{"example-responses":{"id":"example-responses","type":"openai-compat","api_key":"synthetic-headless-server-secret","base_url":%q,"models":[{"id":"server-main","name":"Server main"},{"id":"server-small","name":"Server small"}]}},"models":{"large":{"provider":"example-responses","model":"server-main"},"small":{"provider":"example-responses","model":"server-small"}}}`, provider.URL+"/v1")
	require.NoError(t, os.WriteFile(serverFile, []byte(serverConfiguration), 0o600))
	serverCode, err := connection.EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	identity, err := connection.NewClientIdentity("headless-client")
	require.NoError(t, err)
	require.NoError(t, connection.AuthorizeClient(t.Context(), "headless-client", identity.Certificate))
	tlsConfig, err := connection.ServerTLSConfig(t.Context())
	require.NoError(t, err)
	proxyTLS, err := connection.ClientTLSConfig(connection.Connection{ServerCertificate: serverCode, Client: identity})
	require.NoError(t, err)
	s := server.NewServer(nil, "tcp", "127.0.0.1:0")
	require.NoError(t, s.EnableNetworkAuth(t.Context()))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/agent/init") {
			agentInitializations.Add(1)
		}
		s.Handler().ServeHTTP(w, r)
	})
	var lastServerSequence uint64
	remote := startCommandChannelProxy(t, handler, tlsConfig, proxyTLS, func(fromClient bool, envelope proto.PeerEnvelope) commandChannelProxyDecision {
		if !fromClient {
			mu.Lock()
			if envelope.Sequence > lastServerSequence {
				lastServerSequence = envelope.Sequence
			}
			mu.Unlock()
			return commandChannelProxyDecision{}
		}
		switch envelope.Type {
		case proto.PeerTypeRuntimeTransaction:
			var transaction proto.PeerRuntimeTransaction
			if json.Unmarshal(envelope.Payload, &transaction) == nil {
				mu.Lock()
				next := applyPeerRuntimeOperations(runtimeState, transaction)
				proposals = append(proposals, next)
				if mode != "rejected-ack" {
					runtimeState = next
				}
				sequence := lastServerSequence + 1
				mu.Unlock()
				if mode == "rejected-ack" {
					payload, err := json.Marshal(proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusInvalid, Message: "synthetic runtime rejection"})
					if err == nil {
						reply := &proto.PeerEnvelope{
							Version:     proto.PeerChannelVersion,
							Epoch:       envelope.Epoch,
							Sequence:    sequence,
							MessageID:   uuid.NewString(),
							ReplyTo:     envelope.MessageID,
							Kind:        proto.PeerMessageAcknowledgement,
							Type:        proto.PeerTypeAcknowledgement,
							WorkspaceID: envelope.WorkspaceID,
							Payload:     payload,
						}
						return commandChannelProxyDecision{drop: true, close: true, reply: reply}
					}
				}
			}
		case proto.PeerTypeProviderRefreshCompleted:
			refreshCompletions.Add(1)
		}
		return commandChannelProxyDecision{}
	})
	t.Cleanup(func() { _ = s.Close() })

	clientConfig, clientData, clientCache := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CRUX_GLOBAL_CONFIG", clientConfig)
	t.Setenv("CRUX_GLOBAL_DATA", clientData)
	t.Setenv("CRUX_CACHE_DIR", clientCache)
	trustedBundleDigest := installHeadlessAuthorityPlugin(t, provider.URL, clientData, clientCache)
	require.NoError(t, accounts.Save(t.Context(), "example.responses", accounts.Entry{
		ID: "selected", AccessToken: "synthetic-headless-old-access", RefreshToken: "synthetic-headless-old-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}))
	clientConfiguration := fmt.Sprintf(`{"providers":{"initial":{"id":"initial","type":"openai-compat","api_key":"synthetic-headless-initial","base_url":%q,"models":[{"id":"initial-main","name":"Initial main"},{"id":"initial-small","name":"Initial small"}]},"example-responses":{"api_key":"synthetic-headless-old-access","plugin":{"id":"example.responses-oauth","version":"1.2.0"},"configuration":{"oauth_client_id":"synthetic-headless-client"}}},"models":{"large":{"provider":"initial","model":"initial-main"},"small":{"provider":"initial","model":"initial-small"}}}`, provider.URL+"/v1")
	clientFile := filepath.Join(clientConfig, "crux.json")
	require.NoError(t, os.WriteFile(clientFile, []byte(clientConfiguration), 0o600))
	clientProject, clientWorkspaceData := t.TempDir(), t.TempDir()
	store, err := config.Load(clientProject, clientWorkspaceData, false)
	require.NoError(t, err)
	require.True(t, store.Config().IsModelAvailable("example-responses", "example-reasoner"), "the client fixture must expose the installed flag-selected plugin before admission")
	modelsPrepared := mode == "prepared-implicit"
	smallFlag := "example-responses/example-small"
	if modelsPrepared {
		smallFlag = ""
		command := &cobra.Command{Use: "run"}
		command.SetContext(t.Context())
		command.Flags().String("model", "", "")
		command.Flags().String("small-model", "", "")
		require.NoError(t, command.Flags().Set("model", "example-responses/example-reasoner"))
		prepare := prepareRunModelOverrides(command)
		require.NotNil(t, prepare)
		require.NoError(t, prepare(store))
	}
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, proposal.Providers, 1)
	if modelsPrepared {
		require.Equal(t, "example-responses", proposal.Providers[0].Config.ID)
		require.Equal(t, trustedBundleDigest, proposal.Providers[0].BundleDigest)
		require.Len(t, proposal.Bundles, 1)
		require.Equal(t, trustedBundleDigest, proposal.Bundles[0].Digest)
		require.Len(t, proposal.Credentials, 1)
		require.Equal(t, "example.responses", proposal.Credentials[0].Owner.AccountNamespace)
		require.NotNil(t, proposal.Credentials[0].Account)
		require.Equal(t, "selected", proposal.Credentials[0].Account.ID)
		require.Equal(t, "synthetic-headless-old-access", proposal.Credentials[0].Account.AccessToken)
		require.Equal(t, "example-reasoner", proposal.Models[config.SelectedModelTypeLarge].Model)
		require.Equal(t, "example-small", proposal.Models[config.SelectedModelTypeSmall].Model)
	} else {
		require.Equal(t, "initial", proposal.Providers[0].Config.ID)
		require.Empty(t, proposal.Bundles, "the flag-selected plugin must be absent from the initial receiver runtime")
	}
	mu.Lock()
	runtimeState = proposal
	mu.Unlock()
	api, err := client.NewAuthenticatedClient(t.TempDir(), connection.Connection{
		Address: "tcp://" + strings.TrimPrefix(remote.URL, "https://"), ServerCertificate: serverCode, Client: identity,
	})
	require.NoError(t, err)
	api.SetLocalRuntimeStore(store)
	created, err := api.CreateWorkspace(t.Context(), proto.Workspace{Path: t.TempDir(), Runtime: &proposal, AuthorityMode: "client"})
	require.NoError(t, err)
	retained := workspace.NewClientWorkspace(api, *created)
	t.Cleanup(retained.Shutdown)
	require.True(t, retained.Config().IsModelAvailable("example-responses", "example-reasoner"), "the owning controller must retain unselected local provider models")
	type configPreimage struct {
		exists bool
		data   []byte
	}
	preimages := make(map[string]configPreimage)
	for _, path := range []string{clientFile, serverFile, filepath.Join(clientData, "crux.json"), filepath.Join(serverData, "crux.json"), filepath.Join(clientProject, "crux.json"), filepath.Join(clientWorkspaceData, "crux.json"), filepath.Join(created.Path, "crux.json"), filepath.Join(created.DataDir, "crux.json")} {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			preimages[path] = configPreimage{}
			continue
		}
		require.NoError(t, err)
		preimages[path] = configPreimage{exists: true, data: data}
	}

	stdout, err := os.CreateTemp(t.TempDir(), "stdout-*")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	func() {
		original := os.Stdout
		os.Stdout = stdout
		defer func() { os.Stdout = original }()
		err = runNonInteractiveWithWorkspace(ctx, api, created, retained, "Return the synthetic headless fixture response.", "example-responses/example-reasoner", smallFlag, true, "", false, proto.AgentPermissionDeny, modelsPrepared)
	}()
	require.NoError(t, stdout.Close())
	if mode == "rejected-ack" {
		require.ErrorContains(t, err, "failed to override models")
		require.ErrorContains(t, err, "acknowledgement is pending")
		output, err := os.ReadFile(stdout.Name())
		require.NoError(t, err)
		require.Empty(t, output)
		require.Zero(t, agentInitializations.Load(), "rejected overrides must stop before agent initialization")
		require.Zero(t, providerRequests.Load(), "rejected overrides must not execute any provider request")
		require.Zero(t, refreshCompletions.Load())
		public, err := api.GetWorkspace(t.Context(), created.ID)
		require.NoError(t, err)
		require.Equal(t, uint64(1), public.Authority.Revision)
		require.Equal(t, proposal.Digest, public.Authority.Digest)
		for _, cfg := range []*config.Config{public.Config, retained.Config()} {
			require.Equal(t, "initial-main", cfg.Models[config.SelectedModelTypeLarge].Model)
			require.Equal(t, "initial-small", cfg.Models[config.SelectedModelTypeSmall].Model)
		}
		mu.Lock()
		captured := append([]config.RemoteRuntimeProposal(nil), proposals...)
		mu.Unlock()
		require.Len(t, captured, 1)
		require.Equal(t, uint64(2), captured[0].Revision)
		require.Equal(t, "example-reasoner", captured[0].Models[config.SelectedModelTypeLarge].Model)
		for path, before := range preimages {
			after, err := os.ReadFile(path)
			if !before.exists {
				require.ErrorIs(t, err, os.ErrNotExist, "rejected choices must not create %s", path)
				continue
			}
			require.NoError(t, err)
			require.Equal(t, before.data, after, "rejected temporary choices must not mutate %s", path)
		}
		return
	}
	require.NoError(t, err)
	output, err := os.ReadFile(stdout.Name())
	require.NoError(t, err)
	require.Equal(t, "verified headless client authority\n", string(output))
	require.EqualValues(t, 1, agentInitializations.Load())
	require.EqualValues(t, 1, exchanges.Load(), "main and auxiliary requests must share the owning client's refresh")
	require.EqualValues(t, 1, refreshCompletions.Load())
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, request := range requests {
			if request.purpose == "title" && request.model == "example-small" && request.credential == "Bearer synthetic-headless-new-access" {
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond, "the selected auxiliary model must execute with the refreshed client credential")

	public, err := api.GetWorkspace(t.Context(), created.ID)
	require.NoError(t, err)
	wantUpdates := 2
	if modelsPrepared {
		wantUpdates = 1
	}
	require.Equal(t, uint64(wantUpdates+1), public.Authority.Revision, "prepared choices must survive initialization and refresh without another model override")
	require.Equal(t, "example-reasoner", public.Config.Models[config.SelectedModelTypeLarge].Model)
	require.Equal(t, "example-small", public.Config.Models[config.SelectedModelTypeSmall].Model)
	require.Equal(t, public.Config.AgentModelState(), retained.Config().AgentModelState())
	require.Equal(t, "example-reasoner", store.Config().Models[config.SelectedModelTypeLarge].Model)
	require.Equal(t, "example-small", store.Config().Models[config.SelectedModelTypeSmall].Model)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, proposals, wantUpdates)
	for i, update := range proposals {
		require.Equal(t, uint64(i+2), update.Revision)
		require.Equal(t, "example-reasoner", update.Models[config.SelectedModelTypeLarge].Model)
		require.Equal(t, "example-small", update.Models[config.SelectedModelTypeSmall].Model)
		require.Len(t, update.Providers, 1)
		require.Equal(t, "example-responses", update.Providers[0].Config.ID)
		require.Equal(t, trustedBundleDigest, update.Providers[0].BundleDigest)
		require.Len(t, update.Bundles, 1)
		require.Equal(t, trustedBundleDigest, update.Bundles[0].Digest)
		require.Len(t, update.Credentials, 1)
		require.Equal(t, "example.responses", update.Credentials[0].Owner.AccountNamespace)
		require.NotNil(t, update.Credentials[0].Account)
		require.Equal(t, "selected", update.Credentials[0].Account.ID)
		require.Contains(t, []string{"synthetic-headless-old-access", "synthetic-headless-new-access"}, update.Credentials[0].Account.AccessToken)
	}
	if modelsPrepared {
		require.Equal(t, "synthetic-headless-new-access", proposals[0].Credentials[0].Account.AccessToken)
	} else {
		require.Equal(t, "synthetic-headless-old-access", proposals[0].Credentials[0].Account.AccessToken)
		require.Equal(t, "synthetic-headless-new-access", proposals[len(proposals)-1].Credentials[0].Account.AccessToken)
	}
	var observedMain bool
	for _, request := range requests {
		require.Contains(t, []string{"example-reasoner", "example-small"}, request.model)
		require.Contains(t, []string{"Bearer synthetic-headless-old-access", "Bearer synthetic-headless-new-access"}, request.credential)
		observedMain = observedMain || request.model == "example-reasoner" && request.credential == "Bearer synthetic-headless-new-access"
	}
	require.True(t, observedMain, "successful output must come from the flag-selected main model")

	clientDisk, err := os.ReadFile(clientFile)
	require.NoError(t, err)
	require.JSONEq(t, gjson.Get(clientConfiguration, "models").Raw, gjson.GetBytes(clientDisk, "models").Raw)
	clientDataDisk, err := os.ReadFile(filepath.Join(clientData, "crux.json"))
	require.NoError(t, err, "refresh must persist the rotated credential locally")
	require.False(t, gjson.GetBytes(clientDataDisk, "models").Exists(), "temporary headless model overrides must not be persisted during refresh")
	require.Equal(t, "synthetic-headless-new-access", gjson.GetBytes(clientDataDisk, "providers.example-responses.oauth.access_token").String())
	serverDisk, err := os.ReadFile(serverFile)
	require.NoError(t, err)
	require.Equal(t, serverConfiguration, string(serverDisk))
	_, err = os.Stat(filepath.Join(serverData, "crux.json"))
	require.ErrorIs(t, err, os.ErrNotExist, "client runtime operations must not create a receiver global override file")
	_, err = os.Stat(filepath.Join(created.Path, "crux.json"))
	require.ErrorIs(t, err, os.ErrNotExist, "headless model overrides must not persist receiver workspace configuration")
	for _, path := range []string{filepath.Join(clientProject, "crux.json"), filepath.Join(clientWorkspaceData, "crux.json"), filepath.Join(created.DataDir, "crux.json")} {
		_, err := os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist, "temporary model choices must not persist either workspace's configuration")
	}
}

// applyPeerRuntimeOperations reconstructs the resulting client-authoritative
// runtime proposal a receiver would accept from an incremental
// runtime.transaction.apply command, so tests can assert against the full
// proposal shape without the wire format needing to carry it directly.
func applyPeerRuntimeOperations(base config.RemoteRuntimeProposal, transaction proto.PeerRuntimeTransaction) config.RemoteRuntimeProposal {
	next := base
	next.Providers = append([]config.RemoteProviderDefinition(nil), base.Providers...)
	next.Bundles = append([]providerplugin.TransportBundle(nil), base.Bundles...)
	next.Credentials = append([]config.RemoteCredentialBinding(nil), base.Credentials...)
	next.Models = maps.Clone(base.Models)
	if next.Models == nil {
		next.Models = map[config.SelectedModelType]config.SelectedModel{}
	}
	next.Revision = transaction.Runtime.ResultRevision
	next.Digest = transaction.Runtime.ResultDigest
	for _, op := range transaction.Operations {
		switch op.Type {
		case proto.PeerTypeProviderDefinitionPut:
			put := op.DefinitionPut
			if put == nil {
				continue
			}
			replaced := false
			for i, definition := range next.Providers {
				if definition.Config.ID == put.Definition.Config.ID {
					next.Providers[i] = put.Definition
					replaced = true
					break
				}
			}
			if !replaced {
				next.Providers = append(next.Providers, put.Definition)
			}
			if put.Bundle != nil {
				hasBundle := false
				for _, bundle := range next.Bundles {
					if bundle.Digest == put.Bundle.Digest {
						hasBundle = true
						break
					}
				}
				if !hasBundle {
					next.Bundles = append(next.Bundles, *put.Bundle)
				}
			}
		case proto.PeerTypeProviderDefinitionRemove:
			if op.DefinitionRemove == nil {
				continue
			}
			providerID := op.DefinitionRemove.Provider.Owner.ProviderID
			filteredProviders := next.Providers[:0]
			for _, definition := range next.Providers {
				if definition.Config.ID != providerID {
					filteredProviders = append(filteredProviders, definition)
				}
			}
			next.Providers = filteredProviders
			filteredCredentials := next.Credentials[:0]
			for _, credential := range next.Credentials {
				if credential.Owner.ProviderID != providerID {
					filteredCredentials = append(filteredCredentials, credential)
				}
			}
			next.Credentials = filteredCredentials
		case proto.PeerTypeProviderCredentialReplace:
			if op.CredentialReplace == nil {
				continue
			}
			replaced := false
			for i, credential := range next.Credentials {
				if credential.Owner.ProviderID == op.CredentialReplace.Credential.Owner.ProviderID {
					next.Credentials[i] = op.CredentialReplace.Credential
					replaced = true
					break
				}
			}
			if !replaced {
				next.Credentials = append(next.Credentials, op.CredentialReplace.Credential)
			}
		case proto.PeerTypeProviderCredentialInvalidate:
			if op.CredentialInvalidate == nil {
				continue
			}
			providerID := op.CredentialInvalidate.Provider.Owner.ProviderID
			for i, credential := range next.Credentials {
				if credential.Owner.ProviderID == providerID {
					next.Credentials[i].Unavailable = true
				}
			}
		case proto.PeerTypeModelSelectionSet:
			if op.ModelSelection == nil {
				continue
			}
			next.Models[op.ModelSelection.ModelType] = op.ModelSelection.Settings
		case proto.PeerTypeRuntimeControlsPatch:
			if op.Controls != nil {
				next.Controls = *op.Controls
			}
		}
	}
	return next
}

type headlessAuthorityTransport func(*http.Request) (*http.Response, error)

func (f headlessAuthorityTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func installHeadlessAuthorityPlugin(t *testing.T, endpoint, dataDir, cacheDir string) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "headless.plugin")
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
	value.Capabilities.Operations[0].Retry.Authentication = "refresh-once"
	data, err = json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(dataDir, cacheDir))
	require.NoError(t, err)
	defer manager.Close()
	snapshot, err := manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, Trust: true, ExpectedRevision: manager.Snapshot().Revision})
	require.NoError(t, err)
	require.Len(t, snapshot.Plugins, 1)
	require.Equal(t, "example-responses", snapshot.Plugins[0].ProviderID)
	require.Equal(t, providerplugin.TrustTrusted, snapshot.Plugins[0].Trust)
	require.Equal(t, providerplugin.StateRegistered, snapshot.Plugins[0].State)
	require.NotEmpty(t, snapshot.Plugins[0].Digest)
	return snapshot.Plugins[0].Digest
}

func writeHeadlessAuthoritySSE(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":%q,\"status\":\"in_progress\",\"output\":[]}}\n\n", id)
	_, _ = fmt.Fprint(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"message\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\",\"content\":[]}}\n\n")
	_, _ = fmt.Fprint(w, "event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"item_id\":\"message\",\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n")
	_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"item_id\":\"message\",\"delta\":\"verified headless client authority\"}\n\n")
	_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"message\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"verified headless client authority\",\"annotations\":[]}]}}\n\n")
	_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", id)
}
