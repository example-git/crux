package providerauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providerregistry/registrytest"
	"github.com/stretchr/testify/require"
)

type mutationFixture struct {
	store             *config.ConfigStore
	service           *Service
	owner             providerregistry.RegistrationOwner
	old, selected     accounts.Entry
	root, accountPath string
	request           SwitchRequest
}

func newMutationFixture(t *testing.T, configure ...func(map[string]any)) mutationFixture {
	t.Helper()
	root := t.TempDir()
	accountDir, configDir, dataDir, project := filepath.Join(root, "accounts"), filepath.Join(root, "config"), filepath.Join(root, "global-data"), filepath.Join(root, "project")
	for _, directory := range []string{configDir, dataDir, project} {
		require.NoError(t, os.MkdirAll(directory, 0o700))
	}
	t.Setenv("AI_CLI_DIR", accountDir)
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	old := accounts.Entry{ID: "old-account", DisplayName: "Old account", AccessToken: "synthetic-private-old", RefreshToken: "synthetic-private-old-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	selected := accounts.Entry{ID: "selected-account", DisplayName: "Selected account", AccessToken: "synthetic-private-selected-$LITERAL", RefreshToken: "synthetic-private-selected-refresh", ExpiresAt: old.ExpiresAt, Raw: json.RawMessage(`{"account_id":"synthetic-private-metadata"}`)}
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, selected))
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, old))
	document := map[string]any{
		"providers": map[string]any{"codex": map[string]any{
			"api_key": old.AccessToken, "oauth": old.Token(),
			"models": []map[string]any{{"id": "auth-fixture", "name": "Auth fixture", "context_window": 8192, "default_max_tokens": 1024}},
		}},
		"models": map[string]any{
			"large": map[string]any{"provider": "codex", "model": "auth-fixture", "max_tokens": 1024},
			"small": map[string]any{"provider": "codex", "model": "auth-fixture", "max_tokens": 512},
		},
		"foreign": map[string]any{"number": json.Number("9007199254740993")},
	}
	registration := registrytest.Provider("codex")
	require.NoError(t, registrytest.Install(t.Context(), dataDir, filepath.Join(root, "cache"), *registration.Manifest))
	provider := document["providers"].(map[string]any)["codex"].(map[string]any)
	provider["plugin"] = &config.ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version}
	provider["owner"] = &config.ProviderOwnerReference{Type: config.ProviderOwnerPlugin, Construction: registration.Construction, CompatibilityAdapter: registration.CompatibilityAdapter}
	for _, apply := range configure {
		apply(document)
	}
	data, err := json.Marshal(document)
	require.NoError(t, err)
	// Credentials live in the actual global write scope. A lower layer retaining
	// them would correctly veto global logout rather than silently reveal them.
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "crux.json"), data, 0o600))
	base := env.NewFromMap(map[string]string{
		"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": accountDir,
		"CRUX_GLOBAL_CONFIG": configDir, "CRUX_GLOBAL_DATA": dataDir,
		"CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(config.ProviderProfilePluginCompat),
	})
	store, err := config.LoadIsolated(project, filepath.Join(root, "workspace-data"), false, base)
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("codex")
	require.True(t, ok)
	service := New(store, "mutation-workspace")
	snapshot, err := service.Status(t.Context())
	require.NoError(t, err)
	request := SwitchRequest{OperationID: strings.Repeat("1", 32), Target: Target{WorkspaceID: snapshot.WorkspaceID, Owner: PublicOwner(owner), Generation: snapshot.Generation}, AccountID: selected.ID}
	return mutationFixture{store: store, service: service, owner: owner, old: old, selected: selected, root: root, accountPath: filepath.Join(accountDir, "accounts.json"), request: request}
}

func TestAuthenticationMutationUnconfiguredLogoutAndDisabledMaintenance(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("disabled=%t", disabled), func(t *testing.T) {
			f := newMutationFixture(t, func(document map[string]any) {
				provider := document["providers"].(map[string]any)["codex"].(map[string]any)
				if disabled {
					provider["disable"] = true
				} else {
					delete(provider, "api_key")
					delete(provider, "oauth")
				}
			})
			models := f.store.Config().Models
			_, configured := f.store.Config().Providers.Get("codex")
			require.Equal(t, disabled, configured)
			target := f.request.Target
			if disabled {
				switched, err := f.service.Switch(t.Context(), f.request)
				require.NoError(t, err)
				require.True(t, switched.Outcome.Change.Current.Status.Disabled)
				require.True(t, switched.Outcome.Change.Current.Status.Configured)
				target = switched.Outcome.Change.Current.Target
			}
			request := LogoutRequest{OperationID: strings.Repeat("3", 32), Target: target}
			cleared, err := f.service.Logout(t.Context(), request)
			require.NoError(t, err)
			require.NoError(t, cleared.Outcome.ValidateLogout(request))
			require.Equal(t, disabled, cleared.Outcome.Change.Current.Status.Configured)
			require.Equal(t, disabled, cleared.Outcome.Change.Current.Status.Disabled)
			require.Equal(t, models, f.store.Config().Models)
			_, configured = f.store.Config().Providers.Get("codex")
			require.Equal(t, disabled, configured, "authentication maintenance preserves provider membership")
		})
	}
}

type mutationStub struct {
	result config.AuthenticationMutationResult
	err    error
	calls  int
}

func (s *mutationStub) SwitchAuthenticationAccount(context.Context, config.Scope, config.AuthenticationCapture, providerregistry.RegistrationOwner, string) (config.AuthenticationMutationResult, error) {
	s.calls++
	return s.result, s.err
}

func (s *mutationStub) LogoutAuthentication(context.Context, config.Scope, config.AuthenticationCapture, providerregistry.RegistrationOwner) (config.AuthenticationMutationResult, error) {
	s.calls++
	return s.result, s.err
}

func mutationTarget(t *testing.T, fixture mutationFixture) Target {
	t.Helper()
	snapshot, err := fixture.service.Status(t.Context())
	require.NoError(t, err)
	return Target{WorkspaceID: snapshot.WorkspaceID, Owner: PublicOwner(fixture.owner), Generation: snapshot.Generation}
}

func TestAuthenticationMutationSwitchLogoutAndExactReplay(t *testing.T) {
	f := newMutationFixture(t)
	models, agents := f.store.Config().Models, f.store.Config().Agents
	prepared, committed := 0, 0
	f.store.SetRuntimeGenerationPreparer(func(_ context.Context, snapshot config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
		prepared++
		return config.RuntimeGenerationCandidate{Abort: func() {}, Commit: func() {
			committed++
			require.Same(t, snapshot.Config(), f.store.Config(), "runtime publication uses the exact prepared Config")
		}}, nil
	})
	switched, err := f.service.Switch(t.Context(), f.request)
	require.NoError(t, err)
	require.NoError(t, switched.Outcome.ValidateSwitch(f.request))
	require.Equal(t, MutationProgress{AccountsSaved: true, ConfigSaved: true, RuntimePublished: true}, switched.Outcome.Progress)
	snapshot, current := switched.RuntimeSnapshot()
	require.True(t, current)
	require.True(t, snapshot.SamePublication(f.store.RuntimeSnapshot()))
	capture, current := switched.AuthenticationCapture()
	require.True(t, current)
	require.True(t, capture.SameObservation(f.service.receipts[f.request.OperationID].after), "capture is the exact transaction receipt")
	observed, err := f.store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.True(t, capture.SameObservation(observed))
	provider, _ := snapshot.Config().Providers.Get("codex")
	require.Equal(t, f.selected.AccessToken, provider.APIKey)
	require.Empty(t, provider.APIKeyTemplate)
	accountState, err := accounts.CaptureStateAt(t.Context(), f.accountPath, []string{f.owner.AccountNamespace})
	require.NoError(t, err)
	require.Equal(t, f.selected.ID, accountState.ActiveID(f.owner.AccountNamespace))
	require.Equal(t, models, f.store.Config().Models)
	require.Equal(t, agents, f.store.Config().Agents)
	require.Equal(t, 1, prepared)
	require.Equal(t, 1, committed)
	public, err := json.Marshal(switched.Outcome)
	require.NoError(t, err)
	for _, secret := range []string{"synthetic-private", "account_namespace", "access_token", "refresh_token", f.root} {
		require.NotContains(t, string(public), secret)
	}
	_, err = json.Marshal(switched)
	require.Error(t, err)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		require.NotContains(t, fmt.Sprintf(verb, switched), "synthetic-private")
	}
	files := authenticationInputsTree(t, f.root)
	// Returned DTOs are detached, including nested model and account data.
	switched.Outcome.Change.Models.Large.Model.MaxTokens = 1
	switched.Outcome.Change.Current.Accounts[0].DisplayName = "caller mutation"
	replayed, err := f.service.Switch(t.Context(), f.request)
	require.NoError(t, err)
	encoded, err := json.Marshal(replayed.Outcome)
	require.NoError(t, err)
	require.JSONEq(t, string(public), string(encoded))
	require.Equal(t, files, authenticationInputsTree(t, f.root), "receipt replay performs no writes")
	require.Equal(t, 1, prepared)
	logout := LogoutRequest{OperationID: strings.Repeat("2", 32), Target: replayed.Outcome.Change.Current.Target}
	cleared, err := f.service.Logout(t.Context(), logout)
	require.NoError(t, err)
	require.NoError(t, cleared.Outcome.ValidateLogout(logout))
	require.Empty(t, cleared.Outcome.Change.Current.Accounts)
	provider, _ = f.store.Config().Providers.Get("codex")
	require.Empty(t, provider.APIKey)
	require.Nil(t, provider.OAuthToken)
	require.ErrorIs(t, f.store.RuntimeSnapshot().AuthenticationRevocation("codex"), config.ErrAuthenticationRevoked)
	require.Equal(t, models, f.store.Config().Models)
	require.Equal(t, agents, f.store.Config().Agents)
	require.Equal(t, 2, prepared)
	require.Equal(t, 2, committed)
	historical, err := f.service.Switch(t.Context(), f.request)
	require.NoError(t, err)
	require.True(t, historical.Outcome.Superseded)
	_, current = historical.RuntimeSnapshot()
	require.False(t, current, "an old successful switch cannot restore its runtime")
	_, current = historical.AuthenticationCapture()
	require.False(t, current, "historical receipts cannot authorize new remote collection")
	require.Equal(t, f.selected.ID, historical.Outcome.Change.Current.Status.ActiveAccountID, "receipt remains truthful history")
	require.Equal(t, 2, prepared)
}

func TestAuthenticationMutationRejectedPreparationHasNoWritesAndConsumesTarget(t *testing.T) {
	f := newMutationFixture(t)
	f.store.SetRuntimeGenerationPreparer(func(context.Context, config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
		return config.RuntimeGenerationCandidate{}, errors.New("synthetic-private-preparer-error")
	})
	files := authenticationInputsTree(t, f.root)
	before := f.store.RuntimeSnapshot()
	failed, err := f.service.Switch(t.Context(), f.request)
	require.ErrorIs(t, err, ErrMutation)
	require.NotContains(t, err.Error(), "synthetic-private")
	require.NoError(t, failed.Outcome.ValidateSwitch(f.request))
	require.Equal(t, MutationProgress{}, failed.Outcome.Progress)
	require.Nil(t, failed.Outcome.Change)
	require.True(t, before.SamePublication(f.store.RuntimeSnapshot()))
	// Lock files may be created during admission; credential files stay exact.
	for path, state := range files {
		if strings.HasSuffix(path, ".json") {
			require.Equal(t, state, authenticationInputsTree(t, f.root)[path])
		}
	}
	fresh := f.request
	fresh.OperationID = strings.Repeat("2", 32)
	_, err = f.service.Switch(t.Context(), fresh)
	require.ErrorIs(t, err, ErrStale)
	require.Greater(t, mutationTarget(t, f).Generation.Sequence, f.request.Target.Generation.Sequence)
}

func TestAuthenticationMutationPartialReceiptsNeverUpgradeOrRepeat(t *testing.T) {
	for _, progress := range []config.AuthenticationMutationResult{
		{AccountRefreshed: true}, {AccountsSaved: true}, {AccountsSaved: true, ConfigSaved: true}, {AccountsSaved: true, ConfigSaved: true, RuntimePublished: true},
	} {
		t.Run(fmt.Sprintf("%t-%t-%t-%t", progress.AccountRefreshed, progress.AccountsSaved, progress.ConfigSaved, progress.RuntimePublished), func(t *testing.T) {
			f := newMutationFixture(t)
			// Inject the transaction's partial protocol result here. Filesystem
			// rename failures are exercised by the config transaction tests.
			stub := &mutationStub{result: progress, err: errors.New("synthetic-private-partial-error")}
			f.service.mutations = stub
			first, err := f.service.Switch(t.Context(), f.request)
			require.ErrorIs(t, err, ErrMutation)
			require.NotContains(t, err.Error(), "synthetic-private")
			require.NoError(t, first.Outcome.ValidateSwitch(f.request))
			require.Equal(t, MutationProgress{progress.AccountRefreshed, progress.AccountsSaved, progress.ConfigSaved, progress.RuntimePublished}, first.Outcome.Progress)
			_, err = f.service.Status(t.Context())
			require.NoError(t, err)
			replayed, err := f.service.Switch(t.Context(), f.request)
			require.ErrorIs(t, err, ErrMutation)
			require.Equal(t, first.Outcome, replayed.Outcome)
			require.Nil(t, replayed.Outcome.Change)
			_, current := replayed.RuntimeSnapshot()
			require.False(t, current)
			require.Equal(t, 1, stub.calls)
		})
	}
}

func TestAuthenticationMutationReceiptWindowRejectsEvictedOriginalTarget(t *testing.T) {
	f := newMutationFixture(t)
	stub := &mutationStub{err: errors.New("preflight rejected")}
	f.service.mutations = stub
	var first SwitchRequest
	for i := 1; i <= mutationReceiptLimit+1; i++ {
		request := f.request
		request.OperationID = fmt.Sprintf("%032x", i)
		request.Target = mutationTarget(t, f)
		if i == 1 {
			first = request
		}
		_, err := f.service.Switch(t.Context(), request)
		require.ErrorIs(t, err, ErrMutation)
	}
	require.Len(t, f.service.receipts, mutationReceiptLimit)
	require.NotContains(t, f.service.receipts, first.OperationID)
	_, err := f.service.Switch(t.Context(), first)
	require.ErrorIs(t, err, ErrStale)
	require.Equal(t, mutationReceiptLimit+1, stub.calls)
	// Only retained IDs have a request-content conflict guarantee. Expiry is
	// not a reason to automatically invent this fresh target on a caller's behalf.
	retained := f.service.receipts[fmt.Sprintf("%032x", mutationReceiptLimit+1)].request
	conflict := SwitchRequest{OperationID: retained.operationID, Target: mutationTarget(t, f), AccountID: f.selected.ID}
	_, err = f.service.Switch(t.Context(), conflict)
	require.ErrorIs(t, err, ErrOperationConflict)
	require.Equal(t, mutationReceiptLimit+1, stub.calls)
}

func TestAuthenticationMutationReceiptRecoveryRequiresExactObservation(t *testing.T) {
	f := newMutationFixture(t)
	first, err := f.service.Switch(t.Context(), f.request)
	require.NoError(t, err)
	held, release := make(chan struct{}), make(chan struct{})
	guard, cancelGuard := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancelGuard()
	done := make(chan error, 1)
	go func() {
		done <- accounts.WithSelectedForOwner(guard, f.owner.AccountNamespace, f.selected, func() error { return nil }, func() error {
			close(held)
			select {
			case <-release:
				return nil
			case <-guard.Done():
				return guard.Err()
			}
		})
	}()
	select {
	case <-held:
	case <-guard.Done():
		t.Fatal("receipt fixture did not acquire the account lease")
	}
	blocked, cancelBlocked := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancelBlocked()
	unverified, err := f.service.Switch(blocked, f.request)
	require.ErrorIs(t, err, ErrReceiptUnverified)
	require.Equal(t, first.Outcome, unverified.Outcome)
	_, current := unverified.RuntimeSnapshot()
	require.False(t, current)
	close(release)
	require.NoError(t, <-done)
	recovered, err := f.service.Switch(t.Context(), f.request)
	require.NoError(t, err)
	_, current = recovered.RuntimeSnapshot()
	require.True(t, current, "a failed read can recover only while the original observation remains exact")
	data, err := os.ReadFile(f.accountPath)
	require.NoError(t, err)
	backup := f.accountPath + ".receipt-replacement"
	require.NoError(t, os.WriteFile(backup, data, 0o600))
	require.NoError(t, os.Rename(backup, f.accountPath))
	superseded, err := f.service.Switch(t.Context(), f.request)
	require.NoError(t, err)
	require.True(t, superseded.Outcome.Superseded, "equal-content replacement is a new observation")
	_, current = superseded.RuntimeSnapshot()
	require.False(t, current)
}

func TestAuthenticationMutationPublishedButUnverifiedIsPermanentPartialReceipt(t *testing.T) {
	f := newMutationFixture(t)
	commits, aborts := 0, 0
	f.store.SetRuntimeGenerationPreparer(func(_ context.Context, prepared config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
		return config.RuntimeGenerationCandidate{Abort: func() { aborts++ }, Commit: func() {
			commits++
			require.Same(t, prepared.Config(), f.store.Config())
			path := filepath.Join(f.root, "global-data", "crux.json")
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			// Deliberately bypass the held protocol locks after publication.
			// The transaction must report the real completed effects, while
			// refusing to acknowledge this changed postimage as its receipt.
			require.NoError(t, os.WriteFile(path, append(data, '\n'), 0o600))
		}}, nil
	})
	partial, err := f.service.Switch(t.Context(), f.request)
	require.ErrorIs(t, err, ErrMutation)
	require.Equal(t, MutationProgress{AccountsSaved: true, ConfigSaved: true, RuntimePublished: true}, partial.Outcome.Progress)
	require.Nil(t, partial.Outcome.Change)
	_, current := partial.RuntimeSnapshot()
	require.False(t, current)
	_, current = partial.AuthenticationCapture()
	require.False(t, current, "published but unverified state has no coherent capture")
	provider, _ := f.store.Config().Providers.Get("codex")
	require.Equal(t, f.selected.AccessToken, provider.APIKey, "the failed final observation does not undo publication")
	_, err = f.service.Status(t.Context())
	require.NoError(t, err)
	replayed, err := f.service.Switch(t.Context(), f.request)
	require.ErrorIs(t, err, ErrMutation)
	require.Equal(t, partial.Outcome, replayed.Outcome, "a later observation cannot upgrade an incomplete original receipt")
	require.Equal(t, 1, commits)
	require.Zero(t, aborts)
}

func TestAuthenticationMutationFailedAttemptSupersedesEarlierPublicGeneration(t *testing.T) {
	f := newMutationFixture(t)
	first, err := f.service.Switch(t.Context(), f.request)
	require.NoError(t, err)
	stub := &mutationStub{err: errors.New("no-effect rejection")}
	f.service.mutations = stub
	failed := f.request
	failed.OperationID = strings.Repeat("2", 32)
	failed.Target = first.Outcome.Change.Current.Target
	_, err = f.service.Switch(t.Context(), failed)
	require.ErrorIs(t, err, ErrMutation)
	older, err := f.service.Switch(t.Context(), f.request)
	require.NoError(t, err)
	require.True(t, older.Outcome.Superseded)
	_, current := older.RuntimeSnapshot()
	require.False(t, current)
	require.Equal(t, 1, stub.calls)
}

func TestAuthenticationMutationGenerationAndOwnerAdmissionPrecedeEffects(t *testing.T) {
	f := newMutationFixture(t)
	stub := &mutationStub{err: errors.New("must not run")}
	f.service.mutations = stub
	for _, mode := range []string{"workspace", "epoch", "sequence", "owner", "account", "operation-id"} {
		t.Run(mode, func(t *testing.T) {
			request := f.request
			switch mode {
			case "workspace":
				request.Target.WorkspaceID = "other-workspace"
			case "epoch":
				request.Target.Generation.Epoch = strings.Repeat("0", 32)
			case "sequence":
				request.Target.Generation.Sequence++
			case "owner":
				request.Target.Owner.OAuthFlowID = "another-flow"
			case "account":
				request.AccountID = "absent-account"
			case "operation-id":
				request.OperationID = strings.Repeat("A", 32)
			}
			_, err := f.service.Switch(t.Context(), request)
			require.Error(t, err)
			require.Zero(t, stub.calls)
		})
	}
	_, err := New(f.store, f.request.Target.WorkspaceID).Switch(t.Context(), f.request)
	require.ErrorIs(t, err, ErrStale)
}

type blockingMutation struct {
	authenticationMutator
	entered, release chan struct{}
	calls            atomic.Int32
	cancelAfter      context.CancelFunc
}

func (m *blockingMutation) SwitchAuthenticationAccount(ctx context.Context, scope config.Scope, before config.AuthenticationCapture, owner providerregistry.RegistrationOwner, account string) (config.AuthenticationMutationResult, error) {
	m.calls.Add(1)
	close(m.entered)
	select {
	case <-m.release:
	case <-ctx.Done():
		return config.AuthenticationMutationResult{}, ctx.Err()
	}
	result, err := m.authenticationMutator.SwitchAuthenticationAccount(ctx, scope, before, owner, account)
	if m.cancelAfter != nil {
		m.cancelAfter()
	}
	return result, err
}

func TestAuthenticationMutationGateCancellationAndCommittedReply(t *testing.T) {
	f := newMutationFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	blocking := &blockingMutation{authenticationMutator: f.store, entered: make(chan struct{}), release: make(chan struct{}), cancelAfter: cancel}
	f.service.mutations = blocking
	type reply struct {
		result MutationResult
		err    error
	}
	done := make(chan reply, 1)
	go func() { result, err := f.service.Switch(ctx, f.request); done <- reply{result, err} }()
	select {
	case <-blocking.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("mutation did not start")
	}
	waiting, cancelWait := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancelWait()
	_, err := f.service.Switch(waiting, f.request)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	close(blocking.release)
	select {
	case completed := <-done:
		require.NoError(t, completed.err, "cancellation after committed publication cannot erase its receipt")
		require.True(t, completed.result.Outcome.Progress.RuntimePublished)
	case <-time.After(5 * time.Second):
		t.Fatal("committed mutation did not return")
	}
	_, err = f.service.Switch(t.Context(), f.request)
	require.NoError(t, err)
	require.Equal(t, int32(1), blocking.calls.Load())
}

func TestAuthenticationMutationAcceptedClientAdmissionAndLocalReceipt(t *testing.T) {
	f := newMutationFixture(t)
	acceptedView := f.store.Config()
	proposal, err := f.store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	snapshot, err := f.service.StatusForAccepted(t.Context(), proposal, acceptedView)
	require.NoError(t, err)
	f.request.Target.Generation = snapshot.Generation
	local, err := f.service.SwitchForAccepted(t.Context(), f.request, proposal, acceptedView)
	require.NoError(t, err)
	require.True(t, local.Outcome.Progress.RuntimePublished)
	_, err = f.service.StatusForAccepted(t.Context(), proposal, acceptedView)
	require.Error(t, err, "saved local credentials are not the receiver's accepted runtime")
	recovered, err := f.service.SwitchForAccepted(t.Context(), f.request, proposal, acceptedView)
	require.NoError(t, err, "the same operation can recover its exact local receipt for remote acknowledgement")
	require.Equal(t, local.Outcome, recovered.Outcome)
	fresh := f.request
	fresh.OperationID = strings.Repeat("2", 32)
	fresh.Target = local.Outcome.Change.Current.Target
	_, err = f.service.SwitchForAccepted(t.Context(), fresh, proposal, acceptedView)
	require.Error(t, err, "a different action still requires accepted-state admission")
}
