package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/stretchr/testify/require"
)

func TestAccountRemovalClientTLSActiveInactiveAndLastInference(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "inactive", true: "active"}[active], func(t *testing.T) {
			f := newClientAuthenticationFixture(t, false)
			t.Cleanup(func() { f.w.Shutdown(); require.NoError(t, f.s.Close()) })
			require.NoError(t, f.w.InitCoderAgentNonInteractive(t.Context()))
			receiver, err := f.s.Backend().GetWorkspace(f.w.workspaceID())
			require.NoError(t, err)
			coordinator := receiver.CurrentAgentCoordinator()
			call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("verify removal")}}
			_, err = coordinator.Model().Model.Generate(t.Context(), call)
			require.NoError(t, err)
			id := f.second.ID
			expected := f.first.AccessToken
			if active {
				id = f.first.ID
				expected = f.second.AccessToken
			}
			request := providerauth.RemoveRequest{OperationID: strings.Repeat("a", 32), Target: f.target(t), AccountID: id}
			oldAuthority := f.w.authority.accepted
			infos, bodies := clientAuthenticationFiles(t, f.path)
			result, err := f.w.RemoveProviderAccount(t.Context(), request)
			require.NoError(t, err)
			require.NoError(t, result.ValidateRemove(request))
			require.False(t, result.Superseded)
			require.Equal(t, active, result.Progress.RuntimePublished)
			require.True(t, result.Progress.AccountsSaved)
			wantPuts := int32(0)
			if active {
				wantPuts = 1
			} else {
				requireClientAuthenticationFilesUnchanged(t, []string{f.path}, infos, bodies)
				require.Equal(t, oldAuthority.Revision, f.w.authority.accepted.Revision)
				require.Equal(t, oldAuthority.Digest, f.w.authority.accepted.Digest)
			}
			require.Equal(t, wantPuts, f.puts.Load())
			require.Zero(t, f.removes.Load(), "owning-client accounts stay on the owning client")
			_, err = coordinator.Model().Model.Generate(t.Context(), call)
			require.NoError(t, err)
			require.Equal(t, []string{"Bearer " + f.first.AccessToken, "Bearer " + expected}, f.observed())
			infos, bodies = clientAuthenticationFiles(t, f.path, f.accountsPath)
			// Discard the in-memory receipt so the public retry must restore the
			// original removal and its exact outcome from the durable journal.
			f.w.authority.mu.Lock()
			f.w.authority.authenticationReceipts = nil
			f.w.authority.authenticationReceiptIDs = nil
			f.w.authority.mu.Unlock()
			again, err := f.w.RemoveProviderAccount(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, result, again)
			require.Equal(t, wantPuts, f.puts.Load())
			requireClientAuthenticationFilesUnchanged(t, []string{f.path, f.accountsPath}, infos, bodies)
			last := providerauth.RemoveRequest{OperationID: strings.Repeat("b", 32), Target: f.target(t), AccountID: result.Change.Current.Status.ActiveAccountID}
			loggedOut, err := f.w.RemoveProviderAccount(t.Context(), last)
			require.NoError(t, err)
			require.NoError(t, loggedOut.ValidateRemove(last))
			require.True(t, f.w.authority.removed[f.owner])
			require.Equal(t, wantPuts+1, f.puts.Load())
			_, err = coordinator.Model().Model.Generate(t.Context(), call)
			require.Error(t, err)
			require.Len(t, f.observed(), 2)
			require.NoFileExists(t, f.marker)
		})
	}
}

func TestAccountRemovalClientTLSLostAcknowledgementAndRecovery(t *testing.T) {
	for _, mode := range []string{"inactive-retry", "inactive-recovery", "active-lost", "active-rejected-recovery"} {
		t.Run(mode, func(t *testing.T) {
			f := newClientAuthenticationFixture(t, false)
			t.Cleanup(func() { f.w.Shutdown(); require.NoError(t, f.s.Close()) })
			active := strings.HasPrefix(mode, "active")
			id := f.second.ID
			if active {
				id = f.first.ID
			}
			request := providerauth.RemoveRequest{OperationID: strings.Repeat("c", 32), Target: f.target(t), AccountID: id}
			if active {
				if mode == "active-lost" {
					f.putMode.Store(2)
					hook := func() { f.getMode.Store(1) }
					f.afterPut.Store(&hook)
				} else {
					f.putMode.Store(1)
				}
			} else {
				f.getMode.Store(1)
			}
			result, err := f.w.RemoveProviderAccount(t.Context(), request)
			require.Error(t, err)
			require.True(t, result.Progress.AccountsSaved)
			require.NoError(t, result.ValidateRemove(request))
			require.Equal(t, active, result.Progress.RuntimePublished)
			before, err := os.ReadFile(f.accountsPath)
			require.NoError(t, err)
			f.getMode.Store(0)
			f.putMode.Store(0)
			f.afterGet.Store(nil)
			f.afterPut.Store(nil)
			if strings.HasSuffix(mode, "recovery") {
				recovery := ProviderAuthenticationRecoveryRequest{RecoveryID: strings.Repeat("d", 32), OperationID: request.OperationID, Target: request.Target, RecoverySequence: 1}
				result, err = f.w.RecoverProviderAuthentication(t.Context(), recovery)
			} else {
				result, err = f.w.RemoveProviderAccount(t.Context(), request)
			}
			require.NoError(t, err)
			require.False(t, result.Superseded)
			require.NoError(t, result.ValidateRemove(request))
			after, err := os.ReadFile(f.accountsPath)
			require.NoError(t, err)
			require.Equal(t, before, after)
			expected := int32(0)
			switch mode {
			case "active-lost":
				expected = 1
			case "active-rejected-recovery":
				expected = 2
			}
			require.Equal(t, expected, f.puts.Load())
		})
	}
}

func TestAccountRemovalServerTLSRegisteredRouteAndLostReply(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "inactive", true: "active"}[active], func(t *testing.T) { testAccountRemovalServerTLS(t, active) })
	}
}

func testAccountRemovalServerTLS(t *testing.T, active bool) {
	f := newClientAuthenticationFixture(t, false)
	t.Cleanup(func() { f.w.Shutdown(); require.NoError(t, f.s.Close()) })
	clientInfos, clientBodies := clientAuthenticationFiles(t, f.accountsPath)
	t.Setenv("AI_CLI_DIR", filepath.Join(f.root, "server-auth"))
	require.NoError(t, accounts.Save(t.Context(), f.owner.AccountNamespace, f.first))
	require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, f.second))
	t.Setenv("AI_CLI_DIR", filepath.Dir(f.accountsPath))
	serverAccountsPath := filepath.Join(f.root, "server-auth", "accounts.json")
	created, err := f.w.client.CreateWorkspace(t.Context(), proto.Workspace{Path: f.root, DataDir: t.TempDir(), AuthorityMode: "server"})
	require.NoError(t, err)
	w := NewClientWorkspace(f.w.client, *created)
	t.Cleanup(w.Shutdown)
	require.NoError(t, w.InitCoderAgentNonInteractive(t.Context()))
	receiver, err := f.s.Backend().GetWorkspace(w.workspaceID())
	require.NoError(t, err)
	coordinator := receiver.CurrentAgentCoordinator()
	call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("verify server removal")}}
	_, err = coordinator.Model().Model.Generate(t.Context(), call)
	require.NoError(t, err)
	state, err := w.ProviderAuthentication(t.Context())
	require.NoError(t, err)
	owner := providerauth.PublicOwner(f.owner)
	request := providerauth.RemoveRequest{OperationID: strings.Repeat("e", 32), Target: providerauth.Target{WorkspaceID: state.WorkspaceID, Generation: state.Generation, Owner: owner}, AccountID: f.second.ID}
	expectedToken := f.first.AccessToken
	if active {
		request.AccountID = f.first.ID
		expectedToken = f.second.AccessToken
	}
	f.removeMode.Store(1)
	_, err = w.RemoveProviderAccount(t.Context(), request)
	require.Error(t, err)
	after, err := os.ReadFile(serverAccountsPath)
	require.NoError(t, err)
	f.removeMode.Store(0)
	result, err := w.RemoveProviderAccount(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, result.ValidateRemove(request))
	require.True(t, result.Progress.AccountsSaved)
	require.Equal(t, active, result.Progress.RuntimePublished)
	require.EqualValues(t, 2, f.removes.Load())
	require.Zero(t, f.puts.Load())
	replay, err := os.ReadFile(serverAccountsPath)
	require.NoError(t, err)
	require.Equal(t, after, replay)
	_, err = coordinator.Model().Model.Generate(t.Context(), call)
	require.NoError(t, err)
	require.Equal(t, []string{"Bearer " + f.first.AccessToken, "Bearer " + expectedToken}, f.observed())
	request.OperationID = strings.Repeat("f", 32)
	request.Target = result.Change.Current.Target
	request.AccountID = result.Change.Current.Status.ActiveAccountID
	result, err = w.RemoveProviderAccount(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, result.ValidateRemove(request))
	require.True(t, result.Progress.RuntimePublished)
	require.Empty(t, result.Change.Current.Accounts)
	requireClientAuthenticationFilesUnchanged(t, []string{f.accountsPath}, clientInfos, clientBodies)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = w.RemoveProviderAccount(ctx, request)
	require.ErrorIs(t, err, context.Canceled)
}

func TestAccountRemovalTLSRejectsForeignPrincipal(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	t.Cleanup(func() { f.w.Shutdown(); require.NoError(t, f.s.Close()) })
	request := providerauth.RemoveRequest{OperationID: strings.Repeat("a", 32), Target: f.target(t), AccountID: f.second.ID}
	identity, err := connection.NewClientIdentity("foreign-removal-client")
	require.NoError(t, err)
	require.NoError(t, connection.AuthorizeClient(t.Context(), "foreign-removal-client", identity.Certificate))
	conn := f.connection
	conn.Client = identity
	foreign, err := client.NewAuthenticatedClient(t.TempDir(), conn)
	require.NoError(t, err)
	infos, bodies := clientAuthenticationFiles(t, f.path, f.accountsPath)
	_, err = foreign.RemoveProviderAccount(t.Context(), request.Target.WorkspaceID, request)
	require.Error(t, err)
	requireClientAuthenticationFilesUnchanged(t, []string{f.path, f.accountsPath}, infos, bodies)
	require.Zero(t, f.puts.Load())
}

func TestAccountRemovalClientTLSPartialSavedStateReview(t *testing.T) {
	for _, recreated := range []bool{false, true} {
		t.Run(map[bool]string{false: "review successor", true: "reject recreated account"}[recreated], func(t *testing.T) {
			f := newClientAuthenticationFixture(t, false)
			t.Cleanup(func() { f.w.Shutdown(); require.NoError(t, f.s.Close()) })
			require.NoError(t, f.w.InitCoderAgentNonInteractive(t.Context()))
			request := providerauth.RemoveRequest{OperationID: strings.Repeat("b", 32), Target: f.target(t), AccountID: f.first.ID}
			f.store.SetRuntimeGenerationPreparer(func(context.Context, config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
				return config.RuntimeGenerationCandidate{Abort: func() {}, Commit: func() {
					data, err := os.ReadFile(f.accountsPath)
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(f.accountsPath, append(data, ' '), 0o600))
				}}, nil
			})
			original, err := f.w.RemoveProviderAccount(t.Context(), request)
			require.Error(t, err)
			require.True(t, original.Progress.AccountsSaved && original.Progress.ConfigSaved && original.Progress.RuntimePublished)
			require.Nil(t, original.Change)
			f.store.SetRuntimeGenerationPreparer(nil)
			if recreated {
				require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, f.first))
			}
			infos, bodies := clientAuthenticationFiles(t, f.path, f.accountsPath)
			review := ProviderAuthenticationReviewRequest(authenticationReviewAction(request.OperationID, request.Target, 1))
			summary, err := f.w.ReviewProviderAuthentication(t.Context(), review)
			if recreated {
				require.ErrorContains(t, err, "removed account is present again")
				require.Zero(t, f.puts.Load())
				requireClientAuthenticationFilesUnchanged(t, []string{f.path, f.accountsPath}, infos, bodies)
				return
			}
			require.NoError(t, err)
			require.Equal(t, f.second.ID, summary.OriginalAccountID)
			require.False(t, summary.OriginalLogout)
			apply := ProviderAuthenticationApplyRequest(authenticationReviewApply(clientAuthenticationReviewRequest(review), summary))
			reconciled, err := f.w.ApplyProviderAuthenticationReview(t.Context(), apply)
			require.NoError(t, err)
			require.True(t, reconciled.RemoteAcknowledged && reconciled.Adopted)
			require.EqualValues(t, 1, f.puts.Load())
			requireClientAuthenticationFilesUnchanged(t, []string{f.path, f.accountsPath}, infos, bodies)
			receiver, err := f.s.Backend().GetWorkspace(f.w.workspaceID())
			require.NoError(t, err)
			_, err = receiver.CurrentAgentCoordinator().Model().Model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("verify reviewed successor")}})
			require.NoError(t, err)
			require.Equal(t, []string{"Bearer " + f.second.AccessToken}, f.observed())
			replay, err := f.w.RemoveProviderAccount(t.Context(), request)
			require.ErrorContains(t, err, "separate reviewed action")
			require.Equal(t, original, replay)
		})
	}
}

func TestAccountRemovalStoredIntentSelectsOneAction(t *testing.T) {
	base := clientAuthenticationStoredRequest{
		OperationID: strings.Repeat("a", 32),
		Target: providerauth.Target{
			WorkspaceID: "removal-workspace",
			Owner:       providerauth.Owner{ProviderID: "removal-provider", HasOAuth: true},
			Generation:  providerauth.Generation{Epoch: strings.Repeat("b", 32), Sequence: 1},
		},
		AccountID: "removed-account", RemovedAccountID: "removed-account",
	}
	for _, test := range []struct {
		name   string
		change func(*clientAuthenticationStoredRequest)
		valid  bool
	}{
		{"original-equal-ids", func(*clientAuthenticationStoredRequest) {}, true},
		{"removal-only", func(r *clientAuthenticationStoredRequest) { r.AccountID = "" }, true},
		{"switch-only", func(r *clientAuthenticationStoredRequest) { r.RemovedAccountID = "" }, true},
		{"conflicting-account", func(r *clientAuthenticationStoredRequest) { r.AccountID = "other-account" }, false},
		{"removal-with-logout", func(r *clientAuthenticationStoredRequest) { r.Logout = true }, false},
		{"removal-with-check", func(r *clientAuthenticationStoredRequest) { r.CheckID = strings.Repeat("c", 32) }, false},
		{"removal-with-login", func(r *clientAuthenticationStoredRequest) { r.LoginID = strings.Repeat("d", 32) }, false},
		{"no-action", func(r *clientAuthenticationStoredRequest) { r.AccountID, r.RemovedAccountID = "", "" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := base
			test.change(&request)
			original := request
			err := request.validate()
			if test.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, original, request, "validation must preserve exact recorded retry identity")
		})
	}
}
