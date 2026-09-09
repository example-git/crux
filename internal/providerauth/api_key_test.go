package providerauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

// These stubs isolate service admission and replay. Production key resolution,
// fixed writes and actual HTTPS transport are exercised separately.
type checkedKeyStub struct {
	prepareCalls, saveCalls int
	prepareErr, saveErr     error
	result                  config.AuthenticationMutationResult
	onPrepare               func()
}

func (s *checkedKeyStub) PrepareCheckedAPIKey(context.Context, config.AuthenticationCapture, providerregistry.RegistrationOwner, string, string) (config.CheckedAPIKeyPreparation, error) {
	s.prepareCalls++
	if s.onPrepare != nil {
		s.onPrepare()
	}
	return config.CheckedAPIKeyPreparation{}, s.prepareErr
}

func (s *checkedKeyStub) SaveCheckedAPIKey(context.Context, config.Scope, config.CheckedAPIKeyPreparation) (config.AuthenticationMutationResult, error) {
	s.saveCalls++
	return s.result, s.saveErr
}

func keyCheckRequest(target Target) APIKeyCheckRequest {
	return APIKeyCheckRequest{CheckID: strings.Repeat("a", 32), Target: target, CredentialID: "provider.api_key", Source: "synthetic-private-key-source"}
}

func TestAPIKeyServiceFailedCheckReplayNeverReadsOrRunsAgain(t *testing.T) {
	f := newMutationFixture(t)
	privateFailure := errors.New("synthetic-private-endpoint-query-and-key")
	stub := &checkedKeyStub{prepareErr: privateFailure}
	f.service.apiKeys = stub
	request := keyCheckRequest(f.request.Target)
	outcome, err := f.service.CheckAPIKey(t.Context(), request)
	require.ErrorIs(t, err, ErrAPIKeyCheck)
	require.ErrorIs(t, err, privateFailure)
	require.NotContains(t, err.Error(), "synthetic-private")
	require.NoError(t, outcome.Validate())
	require.Nil(t, outcome.CheckedTarget)
	require.Equal(t, 1, stub.prepareCalls)
	require.Greater(t, f.service.sequence, request.Target.Generation.Sequence)
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		require.NotContains(t, fmt.Sprintf(format, request), request.Source)
		require.NotContains(t, fmt.Sprintf(format, f.service.keyChecks[request.CheckID]), request.Source)
		require.NotContains(t, fmt.Sprintf(format, err), "synthetic-private")
	}
	_, err = json.Marshal(f.service.keyChecks[request.CheckID])
	require.Error(t, err)
	// A fresh capture would now fail. The exact retained receipt remains
	// available without account-file reads or another provider preparation.
	require.NoError(t, os.WriteFile(f.accountPath, []byte("not-json"), 0o600))
	replayed, err := f.service.CheckAPIKey(t.Context(), request)
	require.ErrorIs(t, err, privateFailure)
	require.Equal(t, outcome, replayed)
	require.Equal(t, 1, stub.prepareCalls)
	conflict := request
	conflict.Source = "different-private-input"
	_, err = f.service.CheckAPIKey(t.Context(), conflict)
	require.ErrorIs(t, err, ErrOperationConflict)
	require.Equal(t, 1, stub.prepareCalls)
}

func TestAPIKeyServiceCheckBindsBeforeObservationAndHistoricalTarget(t *testing.T) {
	f := newMutationFixture(t)
	stub := &checkedKeyStub{}
	f.service.apiKeys = stub
	request := keyCheckRequest(f.request.Target)
	checked, err := f.service.CheckAPIKey(t.Context(), request)
	require.NoError(t, err)
	require.NotNil(t, checked.CheckedTarget)
	originalTarget := *checked.CheckedTarget
	checked.CheckedTarget.Owner.ProviderID = "caller-mutated-copy"
	retained, err := f.service.CheckAPIKey(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, originalTarget, *retained.CheckedTarget)
	require.Equal(t, 1, stub.prepareCalls)
	// Same-value account saves are a new observed authority, even though the
	// public account summaries and provider configuration remain unchanged.
	require.NoError(t, accounts.Save(t.Context(), f.owner.AccountNamespace, f.old))
	historical, err := f.service.CheckAPIKey(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, retained, historical)
	_, err = f.service.SaveAPIKey(t.Context(), APIKeySaveRequest{OperationID: strings.Repeat("b", 32), Target: originalTarget, CheckID: request.CheckID})
	require.ErrorIs(t, err, ErrStale)
	require.Zero(t, stub.saveCalls)

	newRequest := keyCheckRequest(mutationTarget(t, f))
	newRequest.CheckID = strings.Repeat("c", 32)
	stub.onPrepare = func() { require.NoError(t, accounts.Save(t.Context(), f.owner.AccountNamespace, f.old)) }
	changed, err := f.service.CheckAPIKey(t.Context(), newRequest)
	require.ErrorIs(t, err, ErrStale)
	require.Nil(t, changed.CheckedTarget, "a post-probe observation cannot replace the initiating capture")
	require.Equal(t, 2, stub.prepareCalls)
}

func TestAPIKeyServiceFailedCheckEvictionDoesNotRepeatPreparation(t *testing.T) {
	f := newMutationFixture(t)
	stub := &checkedKeyStub{prepareErr: errors.New("synthetic probe failure")}
	f.service.apiKeys = stub
	first := keyCheckRequest(f.request.Target)
	for i := range mutationReceiptLimit + 1 {
		request := keyCheckRequest(mutationTarget(t, f))
		request.CheckID = fmt.Sprintf("%032x", i+1)
		if i == 0 {
			first = request
		}
		_, err := f.service.CheckAPIKey(t.Context(), request)
		require.ErrorIs(t, err, ErrAPIKeyCheck)
	}
	require.Len(t, f.service.keyChecks, mutationReceiptLimit)
	require.Equal(t, mutationReceiptLimit+1, stub.prepareCalls)
	_, err := f.service.CheckAPIKey(t.Context(), first)
	require.ErrorIs(t, err, ErrStale)
	require.Equal(t, mutationReceiptLimit+1, stub.prepareCalls)
}

func TestAPIKeyServicePartialSaveRetainsCheckOwnerAndExactReplay(t *testing.T) {
	f := newMutationFixture(t)
	privateFailure := errors.New("synthetic-private-save-failure")
	stub := &checkedKeyStub{result: config.AuthenticationMutationResult{ConfigSaved: true}, saveErr: privateFailure}
	f.service.apiKeys = stub
	check := keyCheckRequest(f.request.Target)
	checked, err := f.service.CheckAPIKey(t.Context(), check)
	require.NoError(t, err)
	request := APIKeySaveRequest{OperationID: strings.Repeat("d", 32), Target: *checked.CheckedTarget, CheckID: check.CheckID}
	result, err := f.service.SaveAPIKey(t.Context(), request)
	require.ErrorIs(t, err, privateFailure)
	require.Equal(t, check.CheckID, result.Outcome.CheckID)
	require.NoError(t, result.Outcome.ValidateAPIKeySave(request))
	require.Equal(t, MutationProgress{ConfigSaved: true}, result.Outcome.Progress)
	require.Nil(t, result.Outcome.Change)
	owner, ok := result.OriginalOwner()
	require.True(t, ok)
	require.Equal(t, f.owner, owner)
	_, ok = result.AuthenticationCapture()
	require.False(t, ok)
	require.Equal(t, 1, stub.saveCalls)
	require.NoError(t, os.WriteFile(f.accountPath, []byte("not-json"), 0o600))
	replayed, err := f.service.SaveAPIKey(t.Context(), request)
	require.ErrorIs(t, err, privateFailure)
	require.Equal(t, result.Outcome, replayed.Outcome)
	require.Equal(t, 1, stub.saveCalls)
	wrongCheck := request
	wrongCheck.CheckID = strings.Repeat("e", 32)
	_, err = f.service.SaveAPIKey(t.Context(), wrongCheck)
	require.ErrorIs(t, err, ErrOperationConflict)
	require.Error(t, result.Outcome.ValidateAPIKeySave(wrongCheck), "partial outcomes must echo the exact checked input identity")
	require.Error(t, result.Outcome.ValidateLogout(LogoutRequest{OperationID: request.OperationID, Target: request.Target}))
	require.Error(t, result.Outcome.ValidateSwitch(SwitchRequest{OperationID: request.OperationID, Target: request.Target, AccountID: f.old.ID}))
}

func TestAPIKeyServiceAdmissionDoesNotResolveInput(t *testing.T) {
	f := newMutationFixture(t)
	stub := &checkedKeyStub{}
	f.service.apiKeys = stub
	request := keyCheckRequest(f.request.Target)
	for _, mutate := range []func(*APIKeyCheckRequest){
		func(r *APIKeyCheckRequest) { r.Source = "" },
		func(r *APIKeyCheckRequest) { r.Source = "key\x00value" },
		func(r *APIKeyCheckRequest) { r.Source = strings.Repeat("x", 64*1024+1) },
		func(r *APIKeyCheckRequest) { r.Target.WorkspaceID = "another-workspace" },
		func(r *APIKeyCheckRequest) { r.Target.Generation.Epoch = strings.Repeat("f", 32) },
		func(r *APIKeyCheckRequest) { r.CredentialID = "" },
	} {
		invalid := request
		mutate(&invalid)
		_, err := f.service.CheckAPIKey(t.Context(), invalid)
		require.Error(t, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.service.CheckAPIKey(ctx, request)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, stub.prepareCalls)
	require.Zero(t, stub.saveCalls)
	require.Empty(t, f.service.keyChecks)
	require.Equal(t, request.Target.Generation.Sequence, f.service.sequence)
}

func TestAPIKeyManifestProbeOutcomeRequiresHTTP200(t *testing.T) {
	f := newMutationFixture(t)
	previous := f.request.Target
	current := previous
	current.Generation.Sequence++
	for _, status := range []int{200, 401, 503} {
		outcome := APIKeyCheckOutcome{CheckID: strings.Repeat("a", 32), Previous: previous, CredentialID: "provider.api_key", Probe: config.ConnectionProbeResult{Kind: config.ConnectionProbeHTTPResponse, Policy: config.ConnectionProbePolicyManifestHTTP200, HTTPStatus: status}, CheckedTarget: &current}
		if status == 200 {
			require.NoError(t, outcome.Validate())
		} else {
			require.Error(t, outcome.Validate())
			outcome.CheckedTarget = nil
			require.NoError(t, outcome.Validate(), "failed HTTP evidence remains transport-safe")
		}
	}
}
