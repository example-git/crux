package server_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/server"
	"github.com/stretchr/testify/require"
)

// This is deliberately a preset, not a full provider with OAuth. Presets carry
// an API-key selection, model catalog, default headers and branding assets;
// saved-account selection is covered by the separate OAuth inference fixture.
func TestRemotePresetPublicInferenceUsesClientSelectionThroughMTLS(t *testing.T) {
	root := remoteOwnershipAcceptanceRoot(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	type observedRequest struct {
		Authorization, Header, Model, Body string
	}
	observed := make(chan observedRequest, 8)
	var fallbackCalls atomic.Int32
	provider := remoteOwnershipAcceptanceProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "cannot read synthetic request", http.StatusBadRequest)
			return
		}
		if r.URL.Path != "/client/v1/chat/completions" {
			fallbackCalls.Add(1)
		}
		var request struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &request) != nil {
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		if r.Header.Get("x-request-purpose") == "conversation" {
			select {
			case observed <- observedRequest{r.Header.Get("Authorization"), r.Header.Get("X-Preset-Selection"), request.Model, string(body)}:
			default:
			}
			writeLiveRevocationText(w, "client preset public inference completed")
			return
		}
		writeLiveRevocationText(w, "Disposable preset title")
	}))
	serverConfigPath := filepath.Join(os.Getenv("CRUX_GLOBAL_CONFIG"), "crux.json")
	serverConfig, err := json.Marshal(map[string]any{
		"providers": map[string]any{"acceptance-preset": map[string]any{
			"type": "openai-compat", "api_key": "synthetic-server-conflict", "base_url": provider.URL + "/server/v1",
			"models": []map[string]any{{"id": "server-conflict", "name": "Server conflict", "context_window": 8192, "default_max_tokens": 128}},
		}},
		"models": map[string]any{"large": map[string]string{"provider": "acceptance-preset", "model": "server-conflict"}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(serverConfigPath, serverConfig, 0o600))
	// A matching host topic would be included by the legacy path-only
	// relevant-memory loader. The remote prompt must never contain it.
	hostMemory := filepath.Join(os.Getenv("CRUX_GLOBAL_DATA"), "memory")
	require.NoError(t, os.MkdirAll(hostMemory, 0o700))
	const hostMemoryMarker = "synthetic-host-only-memory-marker"
	require.NoError(t, os.WriteFile(filepath.Join(hostMemory, "preset.md"), []byte("---\nname: Selected client preset\ndescription: exact selected client preset\ntype: feedback\n---\n\n"+hostMemoryMarker), 0o600))
	h := newRemoteOwnershipAcceptanceServer(t, root)
	protectedBefore := remoteOwnershipAcceptanceProtectedState(t)

	clientRoot := filepath.Join(root, "preset-client")
	clientEnvironment := remoteOwnershipAcceptanceEnvironment(t, clientRoot)
	clientEnvironment["CRUX_PROVIDER_PROFILE"] = string(config.ProviderProfilePluginNative)
	clientEnvironment["PRESET_ACCEPTANCE_KEY"] = "synthetic-client-preset-key"
	// The server sees a conflicting value. The resolved client credential must
	// cross the authenticated runtime boundary without server re-expansion.
	t.Setenv("PRESET_ACCEPTANCE_KEY", "synthetic-server-environment-key")
	source := filepath.Join(clientRoot, "source.plugin")
	require.NoError(t, os.MkdirAll(source, 0o700))
	manifest := map[string]any{
		"plugin_type": "provider-preset", "manifest_version": 1,
		"id": "acceptance.client-preset", "version": "1.0.0", "name": "Client-only acceptance preset", "description": "Synthetic remote inference acceptance",
		"publisher":     map[string]string{"id": "acceptance.fixture", "name": "Disposable fixture"},
		"compatibility": map[string]any{"host_api": map[string]int{"min": 1, "max": 1}},
		"preset": map[string]any{
			"id": "acceptance-preset", "name": "Client preset", "type": "openai-compat", "api_key": "$PRESET_ACCEPTANCE_KEY", "api_endpoint": provider.URL + "/client/v1",
			"default_large_model_id": "client-large", "default_small_model_id": "client-small",
			"default_headers": map[string]string{"X-Preset-Selection": "client-asset-header"},
			"models": []map[string]any{
				{"id": "client-small", "name": "Client small", "context_window": 8192, "default_max_tokens": 128},
				{"id": "client-large", "name": "Client large", "context_window": 16384, "default_max_tokens": 256},
			},
		},
	}
	manifestBytes, err := json.Marshal(manifest)
	require.NoError(t, err)
	brandBytes := []byte(`{"label":"Client asset branding","short_name":"CLIENT","color":"#123456"}`)
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), manifestBytes, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "branding.json"), brandBytes, 0o600))
	manager, err := providerplugin.NewManager(ctx, providerplugin.DefaultPaths(clientEnvironment["CRUX_GLOBAL_DATA"], clientEnvironment["CRUX_CACHE_DIR"]))
	require.NoError(t, err)
	defer manager.Close()
	installed, err := manager.Install(ctx, providerplugin.InstallRequest{Source: source, Trust: true})
	require.NoError(t, err)
	require.Len(t, installed.Plugins, 1)
	digest := installed.Plugins[0].Digest
	clientConfig, err := json.Marshal(map[string]any{
		"providers": map[string]any{"acceptance-preset": map[string]any{
			"api_key": "$PRESET_ACCEPTANCE_KEY", "discover_models": false, "system_prompt_prefix": "client preset configured prompt marker",
			"owner":  map[string]string{"type": "preset", "construction": "openai-compat"},
			"preset": map[string]string{"id": "acceptance.client-preset", "version": "1.0.0", "digest": digest},
		}},
		"models": map[string]any{
			"large": map[string]string{"provider": "acceptance-preset", "model": "client-large"},
			"small": map[string]string{"provider": "acceptance-preset", "model": "client-small"},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(clientEnvironment["CRUX_GLOBAL_CONFIG"], "crux.json"), clientConfig, 0o600))
	store, err := config.LoadIsolated(clientRoot, filepath.Join(clientRoot, "workspace-data"), false, env.NewFromMap(clientEnvironment))
	require.NoError(t, err)
	proposal, err := store.CollectRemoteRuntime(ctx, 1)
	require.NoError(t, err)
	require.Len(t, proposal.Bundles, 1)
	require.Equal(t, digest, proposal.Bundles[0].Digest)
	assets := map[string][]byte{}
	for _, file := range proposal.Bundles[0].Files {
		assets[file.Path] = file.Data
	}
	require.Equal(t, manifestBytes, assets["manifest.json"])
	require.Equal(t, brandBytes, assets["branding.json"])
	var foundCredential bool
	for _, credential := range proposal.Credentials {
		if credential.Owner.ProviderID == "acceptance-preset" {
			foundCredential = true
			require.True(t, credential.Owner.HasPreset)
			require.Equal(t, digest, credential.Owner.PresetDigest)
			require.Equal(t, "synthetic-client-preset-key", credential.APIKey)
			require.Nil(t, credential.Account, "a preset does not declare a saved-account namespace")
		}
	}
	require.True(t, foundCredential)
	c := h.client(t, "A")
	c.SetLocalRuntimeStore(store)
	workdir := filepath.Join(root, "preset-workspace")
	require.NoError(t, os.MkdirAll(workdir, 0o700))
	created, err := c.CreateWorkspace(ctx, proto.Workspace{Path: workdir, Runtime: &proposal, AuthorityMode: "client"})
	require.NoError(t, err)
	require.NotNil(t, created.Authority)
	require.Equal(t, proposal.Digest, created.Authority.Digest)
	require.Equal(t, "client", created.Authority.Mode)
	require.NotNil(t, created.Config)
	selected := created.Config.Models[config.SelectedModelTypeLarge]
	require.Equal(t, "acceptance-preset", selected.Provider)
	require.Equal(t, "client-large", selected.Model)
	var foundBrand bool
	for _, surface := range created.ProviderSurfaces {
		if surface.ID == "acceptance-preset" {
			foundBrand = true
			require.NotNil(t, surface.Brand)
			require.Equal(t, "Client asset branding", surface.Brand.Label)
		}
	}
	require.True(t, foundBrand, "the client bundle's branding asset reaches the public provider surface")
	require.NoError(t, c.InitiateAgentProcessing(ctx, created.ID, false))
	session, err := c.CreateSession(ctx, created.ID, "Preset public inference")
	require.NoError(t, err)
	events, err := c.SubscribeEvents(ctx, created.ID, *created.Authority)
	require.NoError(t, err)
	require.NoError(t, c.SendMessageWithPermissionMode(ctx, created.ID, session.ID, "preset-public-run", "Use the exact selected client preset", proto.AgentPermissionDeny))
	complete := remoteOwnershipAcceptanceRun(t, ctx, events, "preset-public-run")
	require.Empty(t, complete.Error)
	require.Contains(t, complete.Text, "client preset public inference completed")
	select {
	case request := <-observed:
		require.Equal(t, "Bearer synthetic-client-preset-key", request.Authorization)
		require.Equal(t, "client-asset-header", request.Header)
		require.Equal(t, "client-large", request.Model)
		require.Contains(t, request.Body, "client preset configured prompt marker")
		require.NotContains(t, request.Body, hostMemoryMarker, "remote relevant memory must not consult the server's memory topics")
	case <-ctx.Done():
		t.Fatal("public completion had no witnessed conversation inference request")
	}
	require.Zero(t, fallbackCalls.Load(), "the same-ID server endpoint must never receive inference")
	serverAfter, err := os.ReadFile(serverConfigPath)
	require.NoError(t, err)
	require.Equal(t, serverConfig, serverAfter)
	require.NoError(t, c.RetireClient(ctx))
	require.Equal(t, protectedBefore, remoteOwnershipAcceptanceProtectedState(t), "client preset admission, inference and retirement must not install or rewrite any protected server store")
}

func remoteOwnershipAcceptanceEnvironment(t *testing.T, root string) map[string]string {
	t.Helper()
	values := map[string]string{"PATH": os.Getenv("PATH"), "SHELL": "/bin/sh"}
	for _, name := range []string{"HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "CRUX_GLOBAL_DATA", "CRUX_GLOBAL_CONFIG", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		values[name] = filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(values[name], 0o700))
	}
	return values
}

func remoteOwnershipAcceptanceRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for key, value := range remoteOwnershipAcceptanceEnvironment(t, root) {
		t.Setenv(key, value)
	}
	t.Setenv("CRUX_PROVIDER_PROFILE", string(config.ProviderProfilePluginNative))
	t.Setenv("CRUX_PROVIDER_PLUGINS", "")
	t.Setenv("CRUX_PROVIDER_PLUGIN_COMPAT", "")
	return root
}

func remoteOwnershipAcceptanceProvider(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	provider := httptest.NewTLSServer(handler)
	t.Cleanup(provider.Close)
	original := http.DefaultTransport
	transport := provider.Client().Transport.(*http.Transport).Clone()
	http.DefaultTransport = transport
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		http.DefaultTransport = original
	})
	return provider
}

type remoteOwnershipAcceptanceServer struct {
	root, address, certificate string
	identities                 map[string]connection.Identity
}

func newRemoteOwnershipAcceptanceServer(t *testing.T, root string) *remoteOwnershipAcceptanceServer {
	t.Helper()
	certificate, err := connection.EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	h := &remoteOwnershipAcceptanceServer{root: root, certificate: certificate, identities: map[string]connection.Identity{}}
	for _, name := range []string{"A", "B"} {
		identity, err := connection.NewClientIdentity(name)
		require.NoError(t, err)
		require.NoError(t, connection.AuthorizeClient(t.Context(), name, identity.Certificate))
		h.identities[name] = identity
	}
	srv := server.NewServer(nil, "tcp", "127.0.0.1:0")
	require.NoError(t, srv.SetWorkspaceRoots([]string{root}))
	require.NoError(t, srv.EnableNetworkAuth(t.Context()))
	tlsConfig, err := connection.ServerTLSConfig(t.Context())
	require.NoError(t, err)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	h.address = "tcp://" + listener.Addr().String()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(tls.NewListener(listener, tlsConfig)) }()
	t.Cleanup(func() {
		_ = srv.Close()
		for _, listed := range srv.Backend().ListWorkspaces() {
			if ws, err := srv.Backend().GetWorkspace(listed.ID); err == nil {
				ws.Shutdown()
			}
		}
		srv.Backend().Shutdown()
		select {
		case err := <-served:
			require.ErrorIs(t, err, http.ErrServerClosed)
		case <-time.After(5 * time.Second):
			t.Error("acceptance listener did not stop")
		}
	})
	return h
}

func (h *remoteOwnershipAcceptanceServer) client(t *testing.T, name string) *client.Client {
	t.Helper()
	c, err := client.NewAuthenticatedClient(filepath.Join(h.root, "sdk-"+name), connection.Connection{Address: h.address, ServerCertificate: h.certificate, Client: h.identities[name]})
	require.NoError(t, err)
	return c
}

func remoteOwnershipAcceptanceRun(t *testing.T, ctx context.Context, events <-chan any, runID string) proto.RunComplete {
	t.Helper()
	for {
		select {
		case event, ok := <-events:
			require.True(t, ok, "authenticated event stream closed before the exact run completed")
			if complete, ok := event.(pubsub.Event[proto.RunComplete]); ok && complete.Payload.RunID == runID {
				return complete.Payload
			}
		case <-ctx.Done():
			t.Fatal("the exact public inference run did not finish")
		}
	}
}

func remoteOwnershipAcceptanceShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

type remoteOwnershipAcceptanceFile struct {
	Mode fs.FileMode
	Hash string
}

// The disposable roots include the full authentication/control, provider trust,
// account, configuration and cache trees. Include directories and lock/backup
// files as well as contents so creation, deletion and mode changes are visible.
// Workspace databases and task output intentionally live outside these roots.
func remoteOwnershipAcceptanceProtectedState(t *testing.T) map[string]remoteOwnershipAcceptanceFile {
	t.Helper()
	state := map[string]remoteOwnershipAcceptanceFile{}
	for _, name := range []string{"HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "CRUX_GLOBAL_DATA", "CRUX_GLOBAL_CONFIG", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		root := os.Getenv(name)
		require.NotEmpty(t, root)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			var contents []byte
			switch {
			case info.Mode().IsRegular():
				contents, err = os.ReadFile(path)
			case info.Mode()&os.ModeSymlink != 0:
				var target string
				target, err = os.Readlink(path)
				contents = []byte(target)
			}
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(contents)
			state[name+"/"+relative] = remoteOwnershipAcceptanceFile{Mode: info.Mode(), Hash: hex.EncodeToString(digest[:])}
			return nil
		})
		require.NoError(t, err)
	}
	return state
}
