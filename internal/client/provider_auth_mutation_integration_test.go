package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/app"
	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry/registrytest"
	"github.com/example-git/crux/internal/server"
	"github.com/stretchr/testify/require"
)

type authMutationRouteHarness struct {
	workspace *backend.Workspace
	httpSrv   *httptest.Server
}

func authMutationServerFixture(t *testing.T) (*authMutationRouteHarness, *Client, providerauth.SwitchRequest, string, string) {
	t.Helper()
	root := t.TempDir()
	accountDir := filepath.Join(root, "accounts")
	global := filepath.Join(root, "global")
	project := filepath.Join(root, "project")
	globalConfig := filepath.Join(root, "config")
	for _, path := range []string{global, project, globalConfig} {
		require.NoError(t, os.MkdirAll(path, 0700))
	}
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("AI_CLI_DIR", accountDir)
	old := accounts.Entry{ID: "old", AccessToken: "private-old-token", RefreshToken: "private-old-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	selected := accounts.Entry{ID: "selected", DisplayName: "Selected", AccessToken: "private-selected-token", RefreshToken: "private-selected-refresh", ExpiresAt: old.ExpiresAt, Raw: json.RawMessage(`{"account_id":"private-account-metadata"}`)}
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, selected))
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, old))
	document := map[string]any{"providers": map[string]any{"codex": map[string]any{"api_key": old.AccessToken, "oauth": old.Token(), "models": []map[string]any{{"id": "user-model", "name": "User model", "context_window": 8192, "default_max_tokens": 1024}}}}, "models": map[string]any{"large": map[string]any{"provider": "codex", "model": "user-model", "max_tokens": 1024, "provider_options": map[string]any{"false": false, "zero": 0, "empty": ""}}, "small": map[string]any{"provider": "codex", "model": "user-model", "max_tokens": 512}}}
	registration := registrytest.Provider("codex")
	require.NoError(t, registrytest.Install(t.Context(), global, filepath.Join(root, "cache"), *registration.Manifest))
	provider := document["providers"].(map[string]any)["codex"].(map[string]any)
	provider["plugin"] = &config.ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version}
	provider["owner"] = &config.ProviderOwnerReference{Type: config.ProviderOwnerPlugin, Construction: registration.Construction, CompatibilityAdapter: registration.CompatibilityAdapter}
	bytes, err := json.Marshal(document)
	require.NoError(t, err)
	configPath := filepath.Join(global, "crux.json")
	require.NoError(t, os.WriteFile(configPath, bytes, 0600))
	store, err := config.LoadIsolated(project, filepath.Join(root, "workspace-data"), false, env.NewFromMap(map[string]string{"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": accountDir, "CRUX_GLOBAL_CONFIG": globalConfig, "CRUX_GLOBAL_DATA": global, "CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(config.ProviderProfilePluginCompat)}))
	require.NoError(t, err)
	appCtx, cancel := context.WithCancel(t.Context())
	a := app.NewForTest(appCtx)
	t.Cleanup(func() { cancel(); a.ShutdownForTest() })
	host := server.NewServer(store, "unix", "")
	ws := &backend.Workspace{ID: "mutation-workspace", Path: project, App: a}
	ws.Cfg = store
	backend.InsertWorkspaceForTest(host.Backend(), ws)
	backend.SetWorkspaceShutdownFnForTest(ws, func() {})
	httpSrv := httptest.NewServer(host.Handler())
	t.Cleanup(httpSrv.Close)
	harness := &authMutationRouteHarness{workspace: ws, httpSrv: httpSrv}
	sdk, err := NewClient(root, "tcp", strings.TrimPrefix(harness.httpSrv.URL, "http://"))
	require.NoError(t, err)
	status, err := sdk.ProviderAuthentication(t.Context(), harness.workspace.ID)
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("codex")
	require.True(t, ok)
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("a", 32), Target: providerauth.Target{WorkspaceID: harness.workspace.ID, Owner: providerauth.PublicOwner(owner), Generation: status.Generation}, AccountID: selected.ID}
	return harness, sdk, request, configPath, filepath.Join(accountDir, "accounts.json")
}

func TestProviderAuthMutationRegisteredRoutesRealStoreAndSDK(t *testing.T) {
	h, sdk, request, configPath, accountPath := authMutationServerFixture(t)
	commits := 0
	h.workspace.Cfg.SetRuntimeGenerationPreparer(func(_ context.Context, snapshot config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
		return config.RuntimeGenerationCandidate{Abort: func() {}, Commit: func() { commits++; require.Same(t, snapshot.Config(), h.workspace.Cfg.Config()) }}, nil
	})
	switched, err := sdk.SwitchProviderAccount(t.Context(), h.workspace.ID, request)
	require.NoError(t, err)
	require.NotNil(t, switched.Workspace)
	require.Equal(t, "selected", switched.Outcome.Change.Current.Status.ActiveAccountID)
	require.Equal(t, json.Number("0"), switched.Workspace.Config.Models[config.SelectedModelTypeLarge].ProviderOptions["zero"])
	var codexSurface *proto.AuthenticationProviderSurface
	for i := range switched.Workspace.ProviderSurfaces {
		if switched.Workspace.ProviderSurfaces[i].ID == "codex" {
			codexSurface = &switched.Workspace.ProviderSurfaces[i]
		}
	}
	require.NotNil(t, codexSurface)
	require.Equal(t, "user-model", codexSurface.Models[0].ID)
	require.NotEmpty(t, codexSurface.RuntimeControls)
	require.NotNil(t, codexSurface.RuntimeControls[0].Binding)
	encoded, err := json.Marshal(switched)
	require.NoError(t, err)
	for _, secret := range []string{"private-old", "private-selected", "private-account", "account_namespace", "access_token", "refresh_token"} {
		require.NotContains(t, string(encoded), secret)
	}
	provider, _ := h.workspace.Cfg.Config().Providers.Get("codex")
	require.Equal(t, "private-selected-token", provider.APIKey)
	configBytes, err := os.ReadFile(configPath)
	require.NoError(t, err)
	accountBytes, err := os.ReadFile(accountPath)
	require.NoError(t, err)
	retry, err := sdk.SwitchProviderAccount(t.Context(), h.workspace.ID, request)
	require.NoError(t, err)
	require.Equal(t, switched, retry)
	require.Equal(t, 1, commits)
	current, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, configBytes, current)
	current, err = os.ReadFile(accountPath)
	require.NoError(t, err)
	require.Equal(t, accountBytes, current)
	conflict := request
	conflict.AccountID = "old"
	failed, err := sdk.SwitchProviderAccount(t.Context(), h.workspace.ID, conflict)
	require.ErrorIs(t, err, providerauth.ErrOperationConflict)
	require.Nil(t, failed.Workspace)
	require.Equal(t, request.OperationID, failed.Outcome.OperationID)
	logout := providerauth.LogoutRequest{OperationID: strings.Repeat("b", 32), Target: switched.Outcome.Change.Current.Target}
	cleared, err := sdk.LogoutProvider(t.Context(), h.workspace.ID, logout)
	require.NoError(t, err)
	require.NotNil(t, cleared.Workspace)
	require.Empty(t, cleared.Outcome.Change.Current.Accounts)
	require.Equal(t, 2, commits)
	provider, _ = h.workspace.Cfg.Config().Providers.Get("codex")
	require.Empty(t, provider.APIKey)
	require.Nil(t, provider.OAuthToken)
	old, err := sdk.SwitchProviderAccount(t.Context(), h.workspace.ID, request)
	require.NoError(t, err)
	require.True(t, old.Outcome.Superseded)
	require.Nil(t, old.Workspace)
	require.Equal(t, 2, commits)
}

func TestProviderAuthMutationRegisteredRouteKeepsPublishedPartial(t *testing.T) {
	h, sdk, request, configPath, _ := authMutationServerFixture(t)
	commits := 0
	h.workspace.Cfg.SetRuntimeGenerationPreparer(func(context.Context, config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
		return config.RuntimeGenerationCandidate{Abort: func() {}, Commit: func() {
			commits++
			data, err := os.ReadFile(configPath)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(configPath, append(data, '\n'), 0600))
		}}, nil
	})
	partial, err := sdk.SwitchProviderAccount(t.Context(), h.workspace.ID, request)
	require.ErrorIs(t, err, providerauth.ErrMutation)
	require.True(t, partial.Outcome.Progress.AccountsSaved)
	require.True(t, partial.Outcome.Progress.ConfigSaved)
	require.True(t, partial.Outcome.Progress.RuntimePublished)
	require.Nil(t, partial.Workspace)
	require.Nil(t, partial.Outcome.Change)
	replay, err := sdk.SwitchProviderAccount(t.Context(), h.workspace.ID, request)
	require.ErrorIs(t, err, providerauth.ErrMutation)
	require.Equal(t, partial, replay)
	require.Equal(t, 1, commits)
}

func TestProviderAuthMutationCanceledPreparationLeavesFilesUnchanged(t *testing.T) {
	h, sdk, request, configPath, accountPath := authMutationServerFixture(t)
	configBefore, err := os.ReadFile(configPath)
	require.NoError(t, err)
	accountsBefore, err := os.ReadFile(accountPath)
	require.NoError(t, err)
	entered, finished := make(chan struct{}), make(chan struct{})
	h.workspace.Cfg.SetRuntimeGenerationPreparer(func(ctx context.Context, _ config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
		close(entered)
		<-ctx.Done()
		close(finished)
		return config.RuntimeGenerationCandidate{}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := sdk.SwitchProviderAccount(ctx, h.workspace.ID, request); result <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not enter real runtime preparation")
	}
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("canceled SDK request did not finish")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("handler cancellation did not reach runtime preparation")
	}
	// The same operation's retained failure waits for handler completion and
	// cannot execute preparation again under the now-canceled initiating action.
	replay, err := sdk.SwitchProviderAccount(t.Context(), h.workspace.ID, request)
	require.ErrorIs(t, err, providerauth.ErrMutation)
	require.Zero(t, replay.Outcome.Progress)
	require.Nil(t, replay.Workspace)
	configAfter, err := os.ReadFile(configPath)
	require.NoError(t, err)
	accountsAfter, err := os.ReadFile(accountPath)
	require.NoError(t, err)
	require.Equal(t, configBefore, configAfter)
	require.Equal(t, accountsBefore, accountsAfter)
}

type loseAuthenticationReply struct {
	next http.RoundTripper
	lost atomic.Bool
}

func (l *loseAuthenticationReply) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := l.next.RoundTrip(r)
	if err == nil && strings.HasSuffix(r.URL.Path, "/auth/switch") && l.lost.CompareAndSwap(false, true) {
		// Read through the final response before dropping it: the server's fixed
		// transaction and coherent reply really completed, but the SDK sees neither.
		_, readErr := io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		return nil, errors.New("synthetic lost authentication reply")
	}
	return response, err
}

func TestProviderAuthMutationLostReplyRecoversExactReceipt(t *testing.T) {
	h, sdk, request, configPath, accountPath := authMutationServerFixture(t)
	var commits atomic.Int32
	h.workspace.Cfg.SetRuntimeGenerationPreparer(func(context.Context, config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
		return config.RuntimeGenerationCandidate{Abort: func() {}, Commit: func() { commits.Add(1) }}, nil
	})
	sdk.h.Transport = &loseAuthenticationReply{next: sdk.h.Transport}
	missing, err := sdk.SwitchProviderAccount(t.Context(), h.workspace.ID, request)
	require.ErrorContains(t, err, "synthetic lost authentication reply")
	require.Zero(t, missing)
	require.EqualValues(t, 1, commits.Load())
	configBefore, err := os.ReadFile(configPath)
	require.NoError(t, err)
	accountsBefore, err := os.ReadFile(accountPath)
	require.NoError(t, err)
	recovered, err := sdk.SwitchProviderAccount(t.Context(), h.workspace.ID, request)
	require.NoError(t, err)
	require.NotNil(t, recovered.Workspace)
	require.Equal(t, "selected", recovered.Outcome.Change.Current.Status.ActiveAccountID)
	require.EqualValues(t, 1, commits.Load())
	configAfter, err := os.ReadFile(configPath)
	require.NoError(t, err)
	accountsAfter, err := os.ReadFile(accountPath)
	require.NoError(t, err)
	require.Equal(t, configBefore, configAfter)
	require.Equal(t, accountsBefore, accountsAfter)
}
