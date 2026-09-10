package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/stretchr/testify/require"
)

func clientAuthenticationRecoveryAction(operation string, target providerauth.Target, sequence uint64) clientAuthenticationRecoveryRequest {
	return clientAuthenticationRecoveryRequest{RecoveryID: fmt.Sprintf("%032x", sequence+10000), OperationID: operation, Target: target, RecoverySequence: sequence}
}

func TestClientAuthenticationRecoveryRejectedLogoutThroughTLS(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	require.True(t, f.w.CanRecoverProviderAuthentication())
	require.NoError(t, f.w.InitCoderAgentNonInteractive(t.Context()))
	receiver, err := f.s.Backend().GetWorkspace(f.w.workspaceID())
	require.NoError(t, err)
	coordinator := receiver.CurrentAgentCoordinator()
	retained := coordinator.Model()
	call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("explicit logout recovery")}}
	_, err = retained.Model.Generate(t.Context(), call)
	require.NoError(t, err)
	request := providerauth.LogoutRequest{OperationID: strings.Repeat("d", 32), Target: f.target(t)}
	f.putMode.Store(1)
	original, err := f.w.LogoutProvider(t.Context(), request)
	require.Error(t, err)
	paths := []string{f.path, f.accountsPath}
	infos, bodies := clientAuthenticationFiles(t, paths...)
	models := f.store.RuntimeSnapshot().AgentModelState()
	networkBefore := f.requests.Load()
	_, err = f.w.recreateClientWorkspace(t.Context())
	require.ErrorContains(t, err, "recover the saved authentication operation")
	require.Equal(t, networkBefore, f.requests.Load(), "automatic recreation cannot negotiate or recollect the unacknowledged logout")
	f.putMode.Store(0)
	recovery := ProviderAuthenticationRecoveryRequest(clientAuthenticationRecoveryAction(request.OperationID, request.Target, 1))
	recovered, err := f.w.RecoverProviderAuthentication(t.Context(), recovery)
	require.NoError(t, err)
	require.Equal(t, original, recovered)
	require.True(t, f.w.authority.removed[f.owner])
	require.EqualValues(t, 2, f.puts.Load())
	require.Equal(t, models, receiver.Cfg.RuntimeSnapshot().AgentModelState())
	for _, model := range []agent.Model{retained, coordinator.Model()} {
		_, err = model.Model.Generate(t.Context(), call)
		require.Error(t, err)
	}
	require.Equal(t, []string{"Bearer " + f.first.AccessToken}, f.observed())
	again, err := f.w.RecoverProviderAuthentication(t.Context(), recovery)
	require.NoError(t, err)
	require.Equal(t, recovered, again)
	_, err = f.w.LogoutProvider(t.Context(), request)
	require.NoError(t, err, "ordinary original retry may return the proven recovery acknowledgement")
	require.EqualValues(t, 2, f.puts.Load())
	requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
	require.False(t, f.w.authority.unacknowledgedClientAuthentication(f.w.workspaceID()))
}

func TestClientAuthenticationRecoveryGuardsGenericPublication(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	request := providerauth.LogoutRequest{OperationID: strings.Repeat("c", 32), Target: f.target(t)}
	f.putMode.Store(1)
	_, err := f.w.logoutClientAuthentication(t.Context(), request)
	require.Error(t, err)
	a := f.w.authority
	before := f.store.RuntimeSnapshot()
	state := before.AgentModelState()
	model := state.Large.Model
	model.MaxTokens++
	paths := []string{f.path, f.accountsPath}
	infos, bodies := clientAuthenticationFiles(t, paths...)
	_, err = f.w.UpdatePreferredModel(config.ScopeGlobal, config.SelectedModelTypeLarge, model, f.owner)
	require.ErrorContains(t, err, "recover the saved authentication operation")
	_, err = f.w.OverrideModels(t.Context(), config.AgentModelState{Large: &config.OwnedSelectedModel{Model: model, Owner: f.owner}})
	require.ErrorContains(t, err, "recover the saved authentication operation")
	require.ErrorContains(t, f.w.SetProviderToolingInstructions(config.ScopeGlobal, f.owner, "crux"), "recover the saved authentication operation")
	_, err = f.w.ImportCopilot(t.Context(), f.owner)
	require.ErrorContains(t, err, "recover the saved authentication operation")
	require.ErrorContains(t, f.w.SetCompactMode(config.ScopeGlobal, true), "recover the saved authentication operation")
	_, err = f.w.fulfillClientRefresh(t.Context(), config.ClientRefreshRequest{
		ID: "pending-logout-refresh", Principal: a.principal, Revision: a.accepted.Revision, Digest: a.accepted.Digest,
		Owner: f.owner, AccountID: f.first.ID, CredentialID: accounts.CredentialID(f.first),
	})
	require.ErrorContains(t, err, "recover the saved authentication operation")
	a.mu.Lock()
	err = f.w.publishClientAuthorityLocked(t.Context(), a)
	a.mu.Unlock()
	require.ErrorContains(t, err, "recover the saved authentication operation")
	require.True(t, before.SamePublication(f.store.RuntimeSnapshot()), "guarded setters cannot publish even an in-memory model change")
	require.EqualValues(t, 1, f.puts.Load())
	requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
	f.putMode.Store(0)
	_, err = f.w.recoverClientAuthentication(t.Context(), clientAuthenticationRecoveryAction(request.OperationID, request.Target, 1))
	require.NoError(t, err)
	changed, err := f.w.UpdatePreferredModel(config.ScopeGlobal, config.SelectedModelTypeLarge, model, f.owner)
	require.NoError(t, err, "an acknowledged logout must not permanently veto unrelated model settings")
	require.Equal(t, model, changed.Large.Model)
	require.EqualValues(t, 3, f.puts.Load())
	require.True(t, a.removed[f.owner], "later generic publication preserves acknowledged logout intent")
	require.NoError(t, f.w.SetCompactMode(config.ScopeGlobal, true), "presentation settings are allowed after exact adoption")
}

func TestClientAuthenticationRecoveryRequiresLocalAdoptionAfterExactPut(t *testing.T) {
	for _, mode := range []string{"cancel", "cache"} {
		t.Run(mode, func(t *testing.T) {
			f := newClientAuthenticationFixture(t, false)
			request := providerauth.LogoutRequest{OperationID: strings.Repeat("7", 32), Target: f.target(t)}
			a := f.w.authority
			acceptedView := a.configView()
			cachedBefore := f.w.cached()
			if mode == "cancel" {
				f.putMode.Store(1)
				_, err := f.w.logoutClientAuthentication(t.Context(), request)
				require.Error(t, err)
				receipt := a.authenticationReceipts[request.OperationID]
				f.putMode.Store(0)
				ack, err := f.w.client.ReplaceRemoteRuntime(t.Context(), f.w.workspaceID(), receipt.base.Revision, *receipt.proposal)
				require.NoError(t, err)
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				a.mu.Lock()
				_, err = f.w.completeClientAuthenticationPutLocked(ctx, a, receipt, ack)
				a.mu.Unlock()
				require.ErrorIs(t, err, context.Canceled)
			} else {
				change := func() {
					f.w.mu.Lock()
					defer f.w.mu.Unlock()
					copy := *f.w.ws.Authority
					copy.Principal = "changed-before-adoption"
					f.w.ws.Authority = &copy
				}
				f.afterPut.Store(&change)
				_, err := f.w.logoutClientAuthentication(t.Context(), request)
				require.Error(t, err)
				f.afterPut.Store(nil)
				f.w.mu.Lock()
				f.w.ws = cachedBefore
				f.w.mu.Unlock()
			}
			receipt := a.authenticationReceipts[request.OperationID]
			require.True(t, receipt.acknowledged, "the exact remote acknowledgement remains truthful")
			require.False(t, receipt.adopted)
			require.Same(t, acceptedView, a.configView())
			require.False(t, a.removed[f.owner])
			paths := []string{f.path, f.accountsPath}
			infos, bodies := clientAuthenticationFiles(t, paths...)
			puts := f.puts.Load()
			_, err := f.w.recreateClientWorkspace(t.Context())
			require.ErrorContains(t, err, "recover the saved authentication operation")
			require.ErrorContains(t, f.w.SetCompactMode(config.ScopeGlobal, true), "recover the saved authentication operation")
			a.mu.Lock()
			err = f.w.publishClientAuthorityLocked(t.Context(), a)
			a.mu.Unlock()
			require.ErrorContains(t, err, "recover the saved authentication operation")
			// Until a GET can install the proven proposal, generic setters
			// cannot turn remote proof alone into a fresh publication.
			f.getMode.Store(1)
			model := f.store.RuntimeSnapshot().AgentModelState().Large.Model
			model.MaxTokens++
			_, err = f.w.UpdatePreferredModel(config.ScopeGlobal, config.SelectedModelTypeLarge, model, f.owner)
			require.Error(t, err)
			require.Equal(t, puts, f.puts.Load())
			requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
			f.getMode.Store(0)
			if mode == "cancel" {
				_, err = f.w.recoverClientAuthentication(t.Context(), clientAuthenticationRecoveryAction(request.OperationID, request.Target, 1))
			} else {
				_, err = f.w.logoutClientAuthentication(t.Context(), request)
			}
			require.NoError(t, err)
			require.True(t, receipt.adopted)
			require.True(t, a.removed[f.owner])
			require.Equal(t, puts, f.puts.Load(), "recovery adopts the exact GET without another PUT")
			require.NoError(t, f.w.SetCompactMode(config.ScopeGlobal, true))
		})
	}
}

func TestClientAuthenticationRecoveryCollectsCanceledCaptureOnce(t *testing.T) {
	f := newClientAuthenticationFixture(t, true)
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("e", 32), Target: f.target(t), AccountID: f.second.ID}
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "block-key"), nil, 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.w.switchClientAuthentication(ctx, request); done <- err }()
	entered := filepath.Join(f.root, "key-entered")
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
waiting:
	for {
		select {
		case <-ticker.C:
			if _, err := os.Stat(entered); err == nil {
				break waiting
			}
		case err := <-done:
			t.Fatalf("original collection did not enter: %v", err)
		case <-ctx.Done():
			t.Fatal("original collection did not enter")
		}
	}
	cancel()
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "key-release"), nil, 0o600))
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("original collection did not cancel")
	}
	require.Nil(t, f.w.authority.authenticationReceipts[request.OperationID].proposal)
	paths := []string{f.path, f.accountsPath}
	infos, bodies := clientAuthenticationFiles(t, paths...)
	f.putMode.Store(2)
	f.getMode.Store(1)
	recovery := clientAuthenticationRecoveryAction(request.OperationID, request.Target, 1)
	// A failed initial GET consumes only this action, before any recollection.
	_, err := f.w.recoverClientAuthentication(t.Context(), recovery)
	require.Error(t, err)
	require.Zero(t, f.puts.Load())
	f.getMode.Store(0)
	recovery = clientAuthenticationRecoveryAction(request.OperationID, request.Target, 2)
	// Lose both the PUT response and its fallback GET after the explicit
	// collection. The server hook runs only for the committed recovery PUT.
	change := func() { f.getMode.Store(1) }
	f.afterPut.Store(&change)
	f.putMode.Store(2)
	outcome, err := f.w.recoverClientAuthentication(t.Context(), recovery)
	require.Error(t, err)
	require.True(t, outcome.Progress.RuntimePublished)
	require.EqualValues(t, 1, f.puts.Load())
	keyReads, err := os.ReadFile(entered)
	require.NoError(t, err)
	require.Equal(t, "xx", string(keyReads), "one original canceled evaluation plus one explicit recollection")
	f.getMode.Store(0)
	f.afterPut.Store(nil)
	_, err = f.w.recoverClientAuthentication(t.Context(), recovery)
	require.NoError(t, err)
	_, err = f.w.switchClientAuthentication(t.Context(), request)
	require.NoError(t, err)
	keyReads, err = os.ReadFile(entered)
	require.NoError(t, err)
	require.Equal(t, "xx", string(keyReads))
	require.EqualValues(t, 1, f.puts.Load())
	requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
	require.NoFileExists(t, f.marker)
}

func TestClientAuthenticationRecoveryRejectsDriftAndUnrelatedAuthority(t *testing.T) {
	for _, mode := range []string{"account", "config", "principal", "revision"} {
		t.Run(mode, func(t *testing.T) {
			f := newClientAuthenticationFixture(t, false)
			request := providerauth.SwitchRequest{OperationID: strings.Repeat("f", 32), Target: f.target(t), AccountID: f.second.ID}
			f.putMode.Store(1)
			_, err := f.w.switchClientAuthentication(t.Context(), request)
			require.Error(t, err)
			switch mode {
			case "account":
				require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, f.first))
			case "config":
				data, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.path, append(data, '\n'), 0o600))
			case "principal", "revision":
				change := func() {
					f.w.mu.Lock()
					defer f.w.mu.Unlock()
					copy := *f.w.ws.Authority
					if mode == "principal" {
						copy.Principal = "foreign"
					} else {
						copy.Revision += 10
						copy.Digest = "foreign"
					}
					f.w.ws.Authority = &copy
				}
				f.afterGet.Store(&change)
			}
			paths := []string{f.path, f.accountsPath}
			infos, bodies := clientAuthenticationFiles(t, paths...)
			f.putMode.Store(0)
			recovery := clientAuthenticationRecoveryAction(request.OperationID, request.Target, 1)
			_, err = f.w.recoverClientAuthentication(t.Context(), recovery)
			require.Error(t, err)
			require.EqualValues(t, 1, f.puts.Load())
			requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
		})
	}
}

func TestClientAuthenticationRecoveryRestoresEvictedOriginalFromJournal(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("f", 32), Target: f.target(t), AccountID: f.second.ID}
	f.putMode.Store(1)
	_, err := f.w.switchClientAuthentication(t.Context(), request)
	require.Error(t, err)
	original := f.w.authority.authenticationReceipts[request.OperationID]
	require.NotNil(t, original)
	require.NotNil(t, original.proposal)
	delete(f.w.authority.authenticationReceipts, request.OperationID)
	paths := []string{f.path, f.accountsPath}
	infos, bodies := clientAuthenticationFiles(t, paths...)
	f.putMode.Store(0)
	outcome, err := f.w.recoverClientAuthentication(t.Context(), clientAuthenticationRecoveryAction(request.OperationID, request.Target, 1))
	require.NoError(t, err)
	require.NotNil(t, outcome.Change)
	restored := f.w.authority.authenticationReceipts[request.OperationID]
	require.NotNil(t, restored)
	require.NotSame(t, original, restored)
	require.Equal(t, original.request, restored.request)
	require.Equal(t, original.owner, restored.owner)
	require.Equal(t, original.proposal.Digest, restored.proposal.Digest)
	require.True(t, restored.acknowledged)
	require.True(t, restored.adopted)
	require.EqualValues(t, 2, f.puts.Load())
	_, err = f.w.switchClientAuthentication(t.Context(), request)
	require.NoError(t, err)
	require.EqualValues(t, 2, f.puts.Load(), "replay must not send another publication")
	requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
}

func TestClientAuthenticationRecoveryRejectsMissingDurableOriginal(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	target := f.target(t)
	operationID := strings.Repeat("f", 32)
	journal, err := f.store.CaptureAuthenticationJournal(t.Context())
	require.NoError(t, err)
	entries, err := journal.Entries(t.Context(), config.AuthenticationJournalClient, target.WorkspaceID)
	require.NoError(t, err)
	require.Empty(t, entries, "the exact workspace has no durable original to restore")
	require.NotContains(t, f.w.authority.authenticationReceipts, operationID)
	paths := []string{f.path, f.accountsPath}
	infos, bodies := clientAuthenticationFiles(t, paths...)
	requests := f.requests.Load()
	outcome, err := f.w.recoverClientAuthentication(t.Context(), clientAuthenticationRecoveryAction(operationID, target, 1))
	require.ErrorIs(t, err, providerauth.ErrStale)
	require.False(t, clientAuthenticationChanged(outcome.Progress))
	require.Nil(t, outcome.Change)
	require.Equal(t, requests, f.requests.Load(), "unavailable original evidence cannot authorize any receiver request")
	require.Zero(t, f.puts.Load())
	requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
}

func TestClientAuthenticationRecoverySequenceSurvivesEviction(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	f.attach(t)
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("a", 32), Target: f.target(t), AccountID: f.second.ID}
	f.putMode.Store(1)
	_, err := f.w.switchClientAuthentication(t.Context(), request)
	require.Error(t, err)
	f.getMode.Store(1)
	first := clientAuthenticationRecoveryAction(request.OperationID, request.Target, 1)
	for sequence := uint64(1); sequence <= clientAuthenticationReceiptLimit+1; sequence++ {
		_, err = f.w.recoverClientAuthentication(t.Context(), clientAuthenticationRecoveryAction(request.OperationID, request.Target, sequence))
		require.Error(t, err)
	}
	require.Len(t, f.w.authority.authenticationRecoveries, clientAuthenticationReceiptLimit)
	require.NotContains(t, f.w.authority.authenticationRecoveries, first.RecoveryID)
	before := f.requests.Load()
	_, err = f.w.recoverClientAuthentication(t.Context(), first)
	require.ErrorIs(t, err, providerauth.ErrStale)
	require.Equal(t, before, f.requests.Load())
	current := clientAuthenticationRecoveryAction(request.OperationID, request.Target, clientAuthenticationReceiptLimit+1)
	conflict := current
	conflict.RecoverySequence++
	_, err = f.w.recoverClientAuthentication(t.Context(), conflict)
	require.ErrorIs(t, err, providerauth.ErrOperationConflict)
	f.getMode.Store(0)
	f.putMode.Store(0)
	_, err = f.w.recoverClientAuthentication(t.Context(), clientAuthenticationRecoveryAction(request.OperationID, request.Target, clientAuthenticationReceiptLimit+2))
	require.NoError(t, err, "a new explicit action must not be vetoed by bounded retention")
	require.EqualValues(t, 2, f.puts.Load())
}

func TestClientAuthenticationRecoveryCancellationWhileAuthorityLocked(t *testing.T) {
	for _, mode := range []string{"request", "lifetime"} {
		t.Run(mode, func(t *testing.T) {
			f := newClientAuthenticationFixture(t, false)
			request := providerauth.LogoutRequest{OperationID: strings.Repeat("b", 32), Target: f.target(t)}
			f.putMode.Store(1)
			_, err := f.w.logoutClientAuthentication(t.Context(), request)
			require.Error(t, err)
			paths := []string{f.path, f.accountsPath}
			infos, bodies := clientAuthenticationFiles(t, paths...)
			f.w.authority.mu.Lock()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := f.w.recoverClientAuthentication(ctx, clientAuthenticationRecoveryAction(request.OperationID, request.Target, 1))
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatalf("recovery bypassed authority mutex: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			if mode == "request" {
				cancel()
			} else {
				f.w.subCancel()
			}
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(time.Second):
				t.Fatal("recovery waited for canceled mutex")
			}
			f.w.authority.mu.Unlock()
			require.Zero(t, f.w.authority.authenticationReceipts[request.OperationID].recoverySequence)
			require.EqualValues(t, 1, f.puts.Load())
			requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
		})
	}
}
