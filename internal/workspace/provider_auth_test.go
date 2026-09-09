package workspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func workspaceProviderAuthFixture() (providerauth.Snapshot, providerauth.AccountsState) {
	status := providerauth.Status{Owner: providerauth.Owner{ProviderID: "fixture"}, Configured: true,
		Credentials: []providerauth.CredentialStatus{{Kind: "api-key", State: "configured"}, {Kind: "oauth", State: "absent"}}, AccountState: "none"}
	snapshot := providerauth.Snapshot{WorkspaceID: "fixture", Generation: providerauth.Generation{Epoch: strings.Repeat("a", 32), Sequence: 2}, Providers: []providerauth.Status{status}}
	return snapshot, providerauth.AccountsState{Target: providerauth.Target{WorkspaceID: snapshot.WorkspaceID, Owner: status.Owner, Generation: snapshot.Generation}, Status: status, Accounts: []providerauth.AccountSummary{}}
}

func providerAuthTestWorkspace(t *testing.T, handler http.HandlerFunc) *ClientWorkspace {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	address, err := url.Parse(srv.URL)
	require.NoError(t, err)
	sdk, err := client.NewClient(t.TempDir(), "tcp", address.Host)
	require.NoError(t, err)
	w := NewClientWorkspace(sdk, proto.Workspace{ID: "fixture", Config: &config.Config{Options: &config.Options{}}})
	t.Cleanup(w.subCancel)
	return w
}

func TestProviderAuthWorkspaceReadsNeverRefreshConfiguration(t *testing.T) {
	t.Parallel()
	snapshot, accounts := workspaceProviderAuthFixture()
	var requests atomic.Int32
	w := providerAuthTestWorkspace(t, func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		switch request.URL.Path {
		case "/v1/workspaces/fixture/auth":
			require.Equal(t, http.MethodGet, request.Method)
			require.NoError(t, json.NewEncoder(response).Encode(snapshot))
		case "/v1/workspaces/fixture/auth/accounts":
			require.Equal(t, http.MethodPost, request.Method)
			require.NoError(t, json.NewEncoder(response).Encode(accounts))
		default:
			t.Errorf("auth read reached unrelated route %s %s", request.Method, request.URL.Path)
			response.WriteHeader(http.StatusNotFound)
		}
	})
	before := w.Config()
	got, err := w.ProviderAuthentication(t.Context())
	require.NoError(t, err)
	require.Equal(t, snapshot, got)
	listed, err := w.ProviderAccounts(t.Context(), accounts.Target)
	require.NoError(t, err)
	require.Equal(t, accounts, listed)
	require.Same(t, before, w.Config())
	require.EqualValues(t, 2, requests.Load())
}

func TestProviderAuthWorkspaceRejectsRegressedAndSupersededReads(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"generation regressed", "read superseded", "workspace replaced"} {
		t.Run(mode, func(t *testing.T) {
			first, _ := workspaceProviderAuthFixture()
			second := first
			second.Generation.Sequence++
			started, release := make(chan struct{}), make(chan struct{})
			var requests atomic.Int32
			w := providerAuthTestWorkspace(t, func(response http.ResponseWriter, request *http.Request) {
				count := requests.Add(1)
				state := first
				if count == 1 && mode != "generation regressed" {
					close(started)
					select {
					case <-release:
					case <-request.Context().Done():
						return
					}
				}
				if count == 1 && mode == "generation regressed" || count == 2 && mode == "read superseded" {
					state = second
				}
				_ = json.NewEncoder(response).Encode(state)
			})
			if mode == "generation regressed" {
				_, err := w.ProviderAuthentication(t.Context())
				require.NoError(t, err)
				_, err = w.ProviderAuthentication(t.Context())
				require.ErrorContains(t, err, "generation moved backwards")
				return
			}
			result := make(chan error, 1)
			go func() { _, err := w.ProviderAuthentication(t.Context()); result <- err }()
			<-started
			if mode == "read superseded" {
				_, err := w.ProviderAuthentication(t.Context())
				require.NoError(t, err)
			} else {
				w.mu.Lock()
				w.ws.ID = "recovered"
				w.mu.Unlock()
			}
			close(release)
			err := <-result
			if mode == "read superseded" {
				require.ErrorContains(t, err, "superseded")
			} else {
				require.ErrorContains(t, err, "workspace changed")
			}
		})
	}
}

func TestProviderAuthWorkspaceRejectsShutdownAndMissingAuthority(t *testing.T) {
	_, accounts := workspaceProviderAuthFixture()
	w := NewClientWorkspace(nil, proto.Workspace{ID: "fixture"})
	w.subCancel()
	_, err := w.ProviderAuthentication(t.Context())
	require.ErrorIs(t, err, context.Canceled)
	_, err = w.ProviderAccounts(t.Context(), accounts.Target)
	require.ErrorIs(t, err, context.Canceled)
	w = NewClientWorkspace(nil, proto.Workspace{ID: "fixture", Authority: &config.RemoteAuthority{Mode: "client"}})
	defer w.subCancel()
	_, err = w.ProviderAuthentication(t.Context())
	require.ErrorContains(t, err, "owning client")
	_, err = w.ProviderAccounts(t.Context(), accounts.Target)
	require.ErrorContains(t, err, "owning client")
	localCtx, cancel := context.WithCancel(t.Context())
	cancel()
	local := &AppWorkspace{providerAuthCtx: localCtx}
	_, err = local.ProviderAuthentication(t.Context())
	require.ErrorIs(t, err, context.Canceled)
	_, err = local.ProviderAccounts(t.Context(), accounts.Target)
	require.ErrorIs(t, err, context.Canceled)
}

func TestProviderAuthLocalWorkspacesHaveIndependentStableEpochs(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	store := config.NewTestStoreWithRegistrations(&config.Config{})
	first, second := NewAppWorkspace(nil, store), NewAppWorkspace(nil, store)
	defer first.providerAuthCancel()
	defer second.providerAuthCancel()
	initial, err := first.ProviderAuthentication(t.Context())
	require.NoError(t, err)
	again, err := first.ProviderAuthentication(t.Context())
	require.NoError(t, err)
	require.Equal(t, initial, again)
	other, err := second.ProviderAuthentication(t.Context())
	require.NoError(t, err)
	require.NotEqual(t, initial.WorkspaceID, other.WorkspaceID)
	require.NotEqual(t, initial.Generation.Epoch, other.Generation.Epoch)
	first.providerAuthCancel()
	_, err = first.ProviderAuthentication(t.Context())
	require.ErrorIs(t, err, context.Canceled)
}

func TestProviderAuthOwningReadsCancelWhileAuthorityIsLocked(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"status", "accounts"} {
		for _, mode := range []string{"request", "shutdown"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				_, accounts := workspaceProviderAuthFixture()
				w := NewClientWorkspace(nil, proto.Workspace{ID: "fixture", Authority: &config.RemoteAuthority{Mode: "client"}})
				defer w.subCancel()
				w.authority = &clientAuthority{}
				w.authority.mu.Lock()
				defer w.authority.mu.Unlock()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				result := make(chan error, 1)
				go func() {
					var err error
					if operation == "status" {
						_, err = w.ProviderAuthentication(ctx)
					} else {
						_, err = w.ProviderAccounts(ctx, accounts.Target)
					}
					result <- err
				}()
				select {
				case err := <-result:
					t.Fatalf("read bypassed held authority lock: %v", err)
				case <-time.After(20 * time.Millisecond):
				}
				if mode == "request" {
					cancel()
				} else {
					w.subCancel()
				}
				select {
				case err := <-result:
					require.ErrorIs(t, err, context.Canceled)
				case <-time.After(5 * time.Second):
					t.Fatal("canceled read waited for authority lock to be released")
				}
				require.Nil(t, w.authority.providerAuth, "canceled waiter must not initialize or read the service")
			})
		}
	}
}

func TestProviderAuthOwningWorkspaceUsesAcceptedLocalServiceAndRecoveryIdentity(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	registration := providerregistry.Registration{ProviderID: "fixture"}
	provider := config.ProviderConfig{ID: "fixture", APIKey: "synthetic-local-key"}
	newStore := func() *config.ConfigStore {
		return config.NewTestStoreWithRegistrations(&config.Config{Options: &config.Options{}, Providers: csync.NewMapFrom(map[string]config.ProviderConfig{"fixture": provider}), Models: map[config.SelectedModelType]config.SelectedModel{}}, registration)
	}
	local, accepted := newStore(), newStore()
	owner, ok := local.RuntimeSnapshot().ProviderOwner("fixture")
	require.True(t, ok)
	w := NewClientWorkspace(nil, proto.Workspace{ID: "fixture", Authority: &config.RemoteAuthority{Mode: "client", Revision: 1, Digest: "accepted"}})
	defer w.subCancel()
	w.authority = &clientAuthority{store: local, accepted: config.RemoteRuntimeProposal{Revision: 1, Digest: "accepted", Models: accepted.Config().Models, Credentials: []config.RemoteCredentialBinding{{Owner: owner, APIKey: provider.APIKey}}}}
	w.authority.view.Store(accepted.Config())
	first, err := w.ProviderAuthentication(t.Context())
	require.NoError(t, err)
	again, err := w.ProviderAuthentication(t.Context())
	require.NoError(t, err)
	require.Equal(t, first, again)
	require.Len(t, first.Providers, 1)
	target := providerauth.Target{WorkspaceID: first.WorkspaceID, Owner: first.Providers[0].Owner, Generation: first.Generation}
	listed, err := w.ProviderAccounts(t.Context(), target)
	require.NoError(t, err)
	require.Empty(t, listed.Accounts)
	// A recovered workspace receives a new service epoch; old account targets
	// may not address the replacement even when provider identity is unchanged.
	w.mu.Lock()
	w.ws.ID = "recovered"
	w.mu.Unlock()
	recovered, err := w.ProviderAuthentication(t.Context())
	require.NoError(t, err)
	require.Equal(t, "recovered", recovered.WorkspaceID)
	require.NotEqual(t, first.Generation.Epoch, recovered.Generation.Epoch)
	_, err = w.ProviderAccounts(t.Context(), target)
	require.ErrorContains(t, err, "current workspace")
	// No pending flag or SDK is present. A local credential edit must still fail
	// visibly, without publishing or returning it as the acknowledged runtime.
	provider.APIKey = "saved-but-unacknowledged"
	require.NoError(t, local.ApplyEphemeralProviderState(map[string]config.ProviderConfig{"fixture": provider}, nil))
	_, err = w.ProviderAuthentication(t.Context())
	require.ErrorContains(t, err, "not acknowledged")
	target.WorkspaceID, target.Generation = recovered.WorkspaceID, recovered.Generation
	_, err = w.ProviderAccounts(t.Context(), target)
	require.ErrorContains(t, err, "not acknowledged")
	require.Nil(t, w.authority.pending)
	require.Same(t, accepted.Config(), w.authority.configView())
}

func TestProviderAuthOwningReadRejectsChangedCachedAuthorityBeforeIO(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AI_CLI_DIR", root)
	for _, mode := range []string{"revision", "digest", "principal"} {
		t.Run(mode, func(t *testing.T) {
			cached := &config.RemoteAuthority{Mode: "client", Revision: 1, Digest: "accepted", Principal: "owner"}
			switch mode {
			case "revision":
				cached.Revision++
			case "digest":
				cached.Digest = "different"
			case "principal":
				cached.Principal = "different"
			}
			w := NewClientWorkspace(nil, proto.Workspace{ID: "fixture", Authority: cached})
			defer w.subCancel()
			w.authority = &clientAuthority{store: config.NewTestStoreWithRegistrations(&config.Config{}), principal: "owner", accepted: config.RemoteRuntimeProposal{Revision: 1, Digest: "accepted"}}
			_, err := w.ProviderAuthentication(t.Context())
			require.ErrorContains(t, err, "no longer matches")
			_, accounts := workspaceProviderAuthFixture()
			_, err = w.ProviderAccounts(t.Context(), accounts.Target)
			require.ErrorContains(t, err, "no longer matches")
			require.Nil(t, w.authority.providerAuth)
			require.Nil(t, w.authority.pending)
			_, err = os.Stat(filepath.Join(root, "accounts.json.lock"))
			require.True(t, os.IsNotExist(err))
		})
	}
}
