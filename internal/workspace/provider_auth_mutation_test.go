package workspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/internal/app"
	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/server"
	"github.com/stretchr/testify/require"
)

func newWorkspaceAuthenticationStore(t *testing.T) (*config.ConfigStore, providerregistry.RegistrationOwner) {
	t.Helper()
	root := t.TempDir()
	values := map[string]string{"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": "integrated", "CRUX_DISABLE_AUTO_MEMORY": "true"}
	t.Setenv("AI_CLI_DIR", values["AI_CLI_DIR"])
	require.NoError(t, os.MkdirAll(values["CRUX_GLOBAL_DATA"], 0o700))
	first := accounts.Entry{ID: "first", AccessToken: "synthetic-first", RefreshToken: "synthetic-refresh-first", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	second := accounts.Entry{ID: "second", AccessToken: "synthetic-second", RefreshToken: "synthetic-refresh-second", ExpiresAt: first.ExpiresAt}
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, first))
	require.NoError(t, accounts.SaveWithoutActivating(t.Context(), accounts.ProviderCodex, second))
	document := map[string]any{"providers": map[string]any{"codex": map[string]any{"api_key": first.AccessToken, "oauth": first.Token(), "models": []map[string]any{{"id": "main", "name": "Retained main", "context_window": 8192, "default_max_tokens": 1024}}}}, "models": map[string]any{"large": map[string]any{"provider": "codex", "model": "main", "max_tokens": 123}, "small": map[string]any{"provider": "codex", "model": "main", "max_tokens": 45}}}
	data, err := json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(values["CRUX_GLOBAL_DATA"], "crux.json"), data, 0o600))
	store, err := config.LoadIsolated(root, filepath.Join(root, "workspace"), false, env.NewFromMap(values))
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("codex")
	require.True(t, ok)
	return store, owner
}

func TestProviderAuthenticationPublicLocalMutation(t *testing.T) {
	store, owner := newWorkspaceAuthenticationStore(t)
	w := NewAppWorkspace(nil, store)
	t.Cleanup(w.providerAuthCancel)
	state, err := w.ProviderAuthentication(t.Context())
	require.NoError(t, err)
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("a", 32), Target: providerauth.Target{WorkspaceID: state.WorkspaceID, Generation: state.Generation, Owner: providerauth.PublicOwner(owner)}, AccountID: "second"}
	models := store.Config().Models
	outcome, err := w.SwitchProviderAccount(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, outcome.ValidateSwitch(request))
	require.Equal(t, "second", outcome.Change.Current.Status.ActiveAccountID)
	provider, _ := store.Config().Providers.Get(owner.ProviderID)
	require.Equal(t, "synthetic-second", provider.APIKey)
	require.Equal(t, models, store.Config().Models)
	logout := providerauth.LogoutRequest{OperationID: strings.Repeat("b", 32), Target: outcome.Change.Current.Target}
	outcome, err = w.LogoutProvider(t.Context(), logout)
	require.NoError(t, err)
	require.NoError(t, outcome.ValidateLogout(logout))
	require.Empty(t, outcome.Change.Current.Accounts)
	require.Equal(t, models, store.Config().Models)
	w.providerAuthCancel()
	_, err = w.LogoutProvider(t.Context(), logout)
	require.ErrorIs(t, err, context.Canceled)
}

func TestProviderAuthenticationPublicServerMutation(t *testing.T) {
	store, owner := newWorkspaceAuthenticationStore(t)
	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	s := server.NewServer(store, "unix", "")
	host := &backend.Workspace{ID: "auth-workspace", Path: t.TempDir(), App: a, Cfg: store}
	backend.InsertWorkspaceForTest(s.Backend(), host)
	backend.SetWorkspaceShutdownFnForTest(host, func() {})
	remote := httptest.NewServer(s.Handler())
	t.Cleanup(remote.Close)
	sdk, err := client.NewClient(t.TempDir(), "tcp", strings.TrimPrefix(remote.URL, "http://"))
	require.NoError(t, err)
	w := NewClientWorkspace(sdk, proto.Workspace{ID: host.ID, Path: host.Path, DataDir: "retained-data-dir", Config: store.Config().RedactedForTransport(), ProviderSurfaces: config.ProviderSurfaces(store.Config())})
	t.Cleanup(w.subCancel)
	state, err := w.ProviderAuthentication(t.Context())
	require.NoError(t, err)
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("a", 32), Target: providerauth.Target{WorkspaceID: state.WorkspaceID, Generation: state.Generation, Owner: providerauth.PublicOwner(owner)}, AccountID: "second"}
	models := w.Config().Models
	before := w.Config()
	outcome, err := w.SwitchProviderAccount(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, outcome.ValidateSwitch(request))
	require.NotSame(t, before, w.Config())
	require.Equal(t, models, w.Config().Models)
	bound, ok := w.Config().ProviderOwner(owner.ProviderID)
	require.True(t, ok)
	require.Equal(t, owner, bound, "retain exact host owner, including its private account namespace")
	require.Equal(t, "retained-data-dir", w.cached().DataDir)
	provider, _ := w.Config().Providers.Get(owner.ProviderID)
	require.Empty(t, provider.APIKey)
	require.Nil(t, provider.OAuthToken)
	provider, _ = store.Config().Providers.Get(owner.ProviderID)
	require.Equal(t, "synthetic-second", provider.APIKey)
	logout := providerauth.LogoutRequest{OperationID: strings.Repeat("b", 32), Target: outcome.Change.Current.Target}
	outcome, err = w.LogoutProvider(t.Context(), logout)
	require.NoError(t, err)
	require.NoError(t, outcome.ValidateLogout(logout))
	require.Equal(t, models, w.Config().Models)
	current := w.Config()
	outcome, err = w.SwitchProviderAccount(t.Context(), request)
	require.NoError(t, err)
	require.True(t, outcome.Superseded)
	require.Same(t, current, w.Config(), "historical receipt must not restore a prior cached config")
}

func TestProviderAuthenticationServerAdoptionRejectsConcurrentChanges(t *testing.T) {
	for _, mode := range []string{"workspace", "authority", "refresh", "read", "generation", "canceled", "public owner", "missing surface", "partial", "historical"} {
		t.Run(mode, func(t *testing.T) {
			store, owner := newWorkspaceAuthenticationStore(t)
			service := providerauth.New(store, "fixture")
			state, err := service.Status(t.Context())
			require.NoError(t, err)
			before := proto.Workspace{ID: "fixture", Config: store.Config().RedactedForTransport(), ProviderSurfaces: config.ProviderSurfaces(store.Config())}
			request := providerauth.SwitchRequest{OperationID: strings.Repeat("a", 32), Target: providerauth.Target{WorkspaceID: state.WorkspaceID, Generation: state.Generation, Owner: providerauth.PublicOwner(owner)}, AccountID: "second"}
			result, err := service.Switch(t.Context(), request)
			require.NoError(t, err)
			snapshot, ok := result.RuntimeSnapshot()
			require.True(t, ok)
			view, err := proto.NewAuthenticationWorkspaceView(before.ID, snapshot)
			require.NoError(t, err)
			response := proto.ProviderAuthenticationMutationResponse{Outcome: result.Outcome, Workspace: view}
			w := NewClientWorkspace(nil, before)
			t.Cleanup(w.subCancel)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "workspace":
				w.ws.ID = "replacement"
			case "authority":
				w.ws.Authority = &config.RemoteAuthority{Mode: "server", Principal: "replacement"}
			case "refresh":
				w.appliedRefresh = 2
			case "read":
				w.providerAuthAppliedRead = 2
			case "generation":
				w.providerAuthWorkspaceID, w.providerAuthGeneration = before.ID, result.Outcome.Change.Current.Target.Generation
				w.providerAuthGeneration.Sequence++
			case "canceled":
				cancel()
			case "public owner":
				for i := range view.ProviderSurfaces {
					if view.ProviderSurfaces[i].ID == owner.ProviderID {
						copy := *view.ProviderSurfaces[i].Owner
						copy.Construction = providerregistry.ConstructionOpenAICompat
						view.ProviderSurfaces[i].Owner = &copy
					}
				}
			case "missing surface":
				view.ProviderSurfaces = view.ProviderSurfaces[1:]
			case "partial":
				response.Error = proto.NewProviderAuthenticationError(providerauth.ErrReceiptUnverified)
			case "historical":
				response.Outcome.Superseded = true
			}
			old := w.Config()
			err = w.adoptServerAuthentication(ctx, before, 1, 1, response)
			require.Error(t, err)
			require.Same(t, old, w.Config())
		})
	}
}

func TestProviderAuthenticationSurfaceReadClone(t *testing.T) {
	owner := providerregistry.RegistrationOwner{ProviderID: "fixture", AccountNamespace: "retained-private"}
	w := NewClientWorkspace(nil, proto.Workspace{ID: "fixture", ProviderSurfaces: []providerregistry.Surface{{ID: "fixture", Owner: &owner}}})
	t.Cleanup(w.subCancel)
	first := w.ProviderSurfaces()
	first[0].Owner.AccountNamespace = "changed"
	require.Equal(t, "retained-private", w.ProviderSurfaces()[0].Owner.AccountNamespace)
	require.Same(t, &owner, w.cached().ProviderSurfaces[0].Owner, "reading surfaces must not replace cached slice entries")
}

func TestProviderAuthenticationAcknowledgementFencesDelayedRefresh(t *testing.T) {
	store, owner := newWorkspaceAuthenticationStore(t)
	service := providerauth.New(store, "fixture")
	state, err := service.Status(t.Context())
	require.NoError(t, err)
	before := proto.Workspace{ID: "fixture", Config: store.Config().RedactedForTransport(), ProviderSurfaces: config.ProviderSurfaces(store.Config())}
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("a", 32), Target: providerauth.Target{WorkspaceID: state.WorkspaceID, Generation: state.Generation, Owner: providerauth.PublicOwner(owner)}, AccountID: "second"}
	result, err := service.Switch(t.Context(), request)
	require.NoError(t, err)
	snapshot, ok := result.RuntimeSnapshot()
	require.True(t, ok)
	view, err := proto.NewAuthenticationWorkspaceView(before.ID, snapshot)
	require.NoError(t, err)
	response := proto.ProviderAuthenticationMutationResponse{Outcome: result.Outcome, Workspace: view}
	entered, release := make(chan struct{}), make(chan struct{})
	w := providerAuthTestWorkspace(t, func(out http.ResponseWriter, req *http.Request) {
		require.Equal(t, "/v1/workspaces/fixture", req.URL.Path)
		close(entered)
		select {
		case <-release:
		case <-req.Context().Done():
			return
		}
		require.NoError(t, json.NewEncoder(out).Encode(before))
	})
	w.ws = before
	// Mutation starts, then a newer generic refresh captures the old config.
	mutationSequence := w.refreshSequence.Add(1)
	authReadSequence := w.providerAuthReadSequence.Add(1)
	done := make(chan struct{})
	go func() { w.refreshWorkspace(); close(done) }()
	<-entered
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	require.NoError(t, w.adoptServerAuthentication(t.Context(), before, mutationSequence, authReadSequence, response))
	acknowledged := w.Config()
	require.NotSame(t, before.Config, acknowledged)
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("delayed workspace refresh did not finish")
	}
	require.Same(t, acknowledged, w.Config(), "a pre-acknowledgement GET must not replace the coherent authenticated view")
	require.Greater(t, w.appliedRefresh, mutationSequence+1)
	// The same completion fence applies to authentication reads begun before
	// acknowledgement; their later arrival cannot regress the public target.
	err = w.acceptProviderAuthRead(before.ID, authReadSequence, state.Generation)
	require.ErrorContains(t, err, "superseded")
}
