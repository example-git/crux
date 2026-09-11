package workspace_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/workspace"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRemoteCodebaseIndexBuildSearchAndSettingsThroughTLS(t *testing.T) {
	xdgIsolate(t)
	clientAuth, serverAuth := t.TempDir(), t.TempDir()
	t.Setenv("AI_CLI_DIR", clientAuth)
	require.NoError(t, os.WriteFile(filepath.Join(clientAuth, "codebase-index-auth.json"), []byte(`{"accessToken":"synthetic-client-index-token","authMode":"vscode"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(serverAuth, "codebase-index-auth.json"), []byte(`{"accessToken":"synthetic-server-must-not-be-used","authMode":"vscode"}`), 0o600))
	require.NoError(t, os.WriteFile(config.GlobalConfigData(), []byte(`{"providers":{"fixture":{"type":"openai-compat","api_key":"synthetic-model-token","base_url":"https://fixture.invalid/v1","models":[{"id":"fixture","name":"Fixture"}]}},"models":{"large":{"provider":"fixture","model":"fixture"},"small":{"provider":"fixture","model":"fixture"}},"tools":{"codebase_search":{"database_path":"/client-only/source.db","store_directory":"/client-only/store"}}}`), 0o600))
	store, err := config.LoadRemoteClient(false)
	require.NoError(t, err)
	serverCode, err := connection.EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	identity, err := connection.NewClientIdentity("codebase-client")
	require.NoError(t, err)
	require.NoError(t, connection.AuthorizeClient(t.Context(), "codebase-client", identity.Certificate))
	s := server.NewServer(nil, "tcp", "127.0.0.1:0")
	require.NoError(t, s.EnableNetworkAuth(t.Context()))
	hs := httptest.NewUnstartedServer(s.Handler())
	hs.TLS, err = connection.ServerTLSConfig(t.Context())
	require.NoError(t, err)
	hs.StartTLS()
	t.Cleanup(func() { hs.Close(); _ = s.Close() })
	c, err := client.NewAuthenticatedClient(t.TempDir(), connection.Connection{Address: "tcp://" + strings.TrimPrefix(hs.URL, "https://"), ServerCertificate: serverCode, Client: identity})
	require.NoError(t, err)
	c.SetLocalRuntimeStore(store)
	remotePath, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(remotePath, "main.go"), []byte("package fixture\n\n// RemoteIndexMarker locates the server project.\nfunc RemoteIndexMarker() {}\n"), 0o600))
	store.BindRemoteCodebaseIndexScope(c.AuthenticationJournalIdentity(), remotePath)
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Nil(t, proposal.CodebaseIndex)
	created, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: remotePath, Runtime: &proposal})
	require.NoError(t, err)
	w := workspace.NewClientWorkspace(c, *created)
	t.Cleanup(w.Shutdown)
	t.Setenv("AI_CLI_DIR", serverAuth)

	var chunks, queries atomic.Int32
	embedding := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer synthetic-client-index-token", r.Header.Get("Authorization"))
		rw.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/embeddings/models":
			_, _ = rw.Write([]byte(`{"models":[{"id":"fixture-embedding","active":true}]}`))
		case "/chunks":
			var input struct{ Content, Path string }
			require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
			require.Equal(t, "main.go", input.Path)
			chunks.Add(1)
			require.NoError(t, json.NewEncoder(rw).Encode(map[string]any{"embedding_model": "fixture-embedding", "chunks": []any{map[string]any{"hash": "fixture-hash", "text": input.Content, "line_range": map[string]int{"start": 1, "end": 4}, "embedding": map[string]any{"model": "fixture-embedding", "embedding": []float32{1, 0}}}}}))
		case "/embeddings":
			queries.Add(1)
			_, _ = rw.Write([]byte(`{"embedding_model":"fixture-embedding","embeddings":[{"embedding":[1,0]}]}`))
		default:
			t.Errorf("unexpected embedding route %s", r.URL.Path)
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(embedding.Close)
	embeddingURL, err := url.Parse(embedding.URL)
	require.NoError(t, err)
	base := http.DefaultTransport
	transport := refreshFixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.github.com" {
			return nil, fmt.Errorf("unexpected embedding destination %s", r.URL.Host)
		}
		clone := r.Clone(r.Context())
		clone.URL.Scheme, clone.URL.Host = embeddingURL.Scheme, embeddingURL.Host
		return base.RoundTrip(clone)
	})
	oldClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: transport}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultClient = oldClient; http.DefaultTransport = base })

	status, err := w.UpdateCodebaseIndex(t.Context(), proto.CodebaseIndexUpdate{Enabled: true, Reindex: true})
	require.NoError(t, err)
	require.True(t, status.Enabled)
	require.Equal(t, remotePath, status.ProjectRoot)
	require.Empty(t, status.ConfiguredDatabasePath)
	require.NotContains(t, status.StoreDirectory, "client-only")
	require.Eventually(t, func() bool {
		status, err = w.CodebaseIndexStatus(t.Context())
		return err == nil && status.State == "ready"
	}, 15*time.Second, 25*time.Millisecond, "index did not become ready: %+v", status)
	require.Greater(t, chunks.Load(), int32(0))
	require.True(t, status.Serving)
	require.Equal(t, "signed-in", status.CredentialStatus)
	host, err := s.Backend().GetWorkspace(created.ID)
	require.NoError(t, err)
	snapshot := host.Cfg.RuntimeSnapshot()
	tool := tools.NewCodebaseSearchTool(remotePath, snapshot.Config().Tools.CodebaseSearch, http.DefaultClient, nil, snapshot.CodebaseIndexToken)
	result, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "search", Name: tools.CodebaseSearchToolName, Input: `{"query":"RemoteIndexMarker","count":1}`})
	require.NoError(t, err)
	require.False(t, result.IsError, result.Content)
	require.Contains(t, result.Content, "RemoteIndexMarker")
	require.Greater(t, queries.Load(), int32(0))
	public, err := c.GetWorkspace(t.Context(), created.ID)
	require.NoError(t, err)
	encoded, err := json.Marshal(public)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "synthetic-client-index-token")
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	recollected, err := store.CollectRemoteRuntime(t.Context(), public.Authority.Revision+1)
	require.NoError(t, err)
	require.True(t, recollected.CodebaseIndex.Settings.IsEnabled())
	require.Empty(t, recollected.CodebaseIndex.Settings.IncludePaths)

	// A plain endpoint update cannot replace accepted client paths or settings.
	_, err = c.UpdateCodebaseIndex(t.Context(), created.ID, proto.CodebaseIndexUpdate{Enabled: true, StoreDirectory: "unaccepted-store"})
	require.ErrorContains(t, err, "settings must be accepted")
	_, err = c.UpdateCodebaseIndex(t.Context(), created.ID, proto.CodebaseIndexUpdate{Enabled: true, DatabasePath: "unaccepted.db"})
	require.ErrorContains(t, err, "settings must be accepted")
	_, err = s.Backend().UpdateCodebaseIndex(created.ID, proto.CodebaseIndexUpdate{Enabled: true}, &proto.WorkspaceAttachment{Mode: "client", Revision: 1, Digest: proposal.Digest})
	require.ErrorIs(t, err, config.ErrRemoteRuntimeRevision)

	// Missing client credentials cannot fall back to the signed-in server.
	require.NoError(t, os.Remove(filepath.Join(clientAuth, "codebase-index-auth.json")))
	status, err = w.UpdateCodebaseIndex(t.Context(), proto.CodebaseIndexUpdate{Enabled: true, IncludePaths: []string{"main.go"}})
	require.NoError(t, err)
	require.Equal(t, "missing", status.CredentialStatus)
	token, err := host.Cfg.RuntimeSnapshot().CodebaseIndexToken(context.Background())
	require.NoError(t, err)
	require.Empty(t, token)
	beforeQueries := queries.Load()
	missingSnapshot := host.Cfg.RuntimeSnapshot()
	missingTool := tools.NewCodebaseSearchTool(remotePath, missingSnapshot.Config().Tools.CodebaseSearch, http.DefaultClient, nil, missingSnapshot.CodebaseIndexToken)
	result, err = missingTool.Run(t.Context(), fantasy.ToolCall{ID: "missing-search", Name: tools.CodebaseSearchToolName, Input: `{"query":"RemoteIndexMarker"}`})
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Equal(t, beforeQueries, queries.Load(), "must not query with server credentials")
	status, err = w.UpdateCodebaseIndex(t.Context(), proto.CodebaseIndexUpdate{Enabled: false, IncludePaths: []string{"main.go"}})
	require.NoError(t, err)
	require.Equal(t, "disabled", status.State)
	w.Shutdown()
}

func TestLocalCodebaseIndexPreservesConfiguredPaths(t *testing.T) {
	xdgIsolate(t)
	t.Setenv("AI_CLI_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigData(), []byte(`{"providers":{"fixture":{"type":"openai-compat","api_key":"synthetic-model-token","base_url":"https://fixture.invalid/v1","models":[{"id":"fixture","name":"Fixture"}]}},"models":{"large":{"provider":"fixture","model":"fixture"},"small":{"provider":"fixture","model":"fixture"}}}`), 0o600))
	s := server.NewServer(nil, "unix", filepath.Join(t.TempDir(), "server.sock"))
	t.Cleanup(func() { _ = s.Close() })
	clientID := uuid.NewString()
	host, _, err := s.Backend().CreateWorkspace(proto.Workspace{Path: t.TempDir(), ClientID: clientID})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Backend().DeleteWorkspace(host.ID, clientID)) })
	local := workspace.NewAppWorkspace(host.App, host.Cfg)
	update := proto.CodebaseIndexUpdate{Enabled: false, DatabasePath: filepath.Join(host.Path, "chosen.db"), StoreDirectory: filepath.Join(host.Path, "chosen-store"), IncludePaths: []string{"src"}}
	status, err := local.UpdateCodebaseIndex(t.Context(), update)
	require.NoError(t, err)
	require.Equal(t, "disabled", status.State)
	status, err = local.CodebaseIndexStatus(t.Context())
	require.NoError(t, err)
	require.Equal(t, update.DatabasePath, status.ConfiguredDatabasePath)
	require.Equal(t, update.StoreDirectory, status.ConfiguredStoreDirectory)
	require.Equal(t, update.IncludePaths, status.IncludePaths)
	require.Equal(t, update.DatabasePath, host.Cfg.Config().Tools.CodebaseSearch.DatabasePath)
}
