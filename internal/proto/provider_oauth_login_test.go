package proto

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func protoOAuthRef() providerauth.OAuthLoginRef {
	return providerauth.OAuthLoginRef{LoginID: strings.Repeat("a", 32), OperationID: strings.Repeat("b", 32), Target: providerauth.Target{WorkspaceID: "workspace", Owner: providerauth.Owner{ProviderID: "provider", HasOAuth: true, OAuthAdapter: providerregistry.LoginBrowser, OAuthFlowID: "flow"}, Generation: providerauth.Generation{Epoch: strings.Repeat("c", 32), Sequence: 1}}}
}

func TestProviderOAuthStrictCodeRequestAndEscapedLimit(t *testing.T) {
	request := providerauth.OAuthLoginCodeRequest{Login: protoOAuthRef(), SubmissionID: strings.Repeat("d", 32), Input: "synthetic-private-input"}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	valid := string(encoded)
	for _, body := range []string{`null`, `{}`, valid + `{}`, strings.Replace(valid, `"input":`, `"Input":`, 1), strings.Replace(valid, `"input":`, `"input":"synthetic-private","input":`, 1), strings.Replace(valid, `"input":"synthetic-private-input"`, `"input":null`, 1), strings.Replace(valid, `"input":"synthetic-private-input"`, `"input":{"nested":"synthetic-private"}`, 1), strings.Replace(valid, `"login":{`, `"login":{"unknown":true,`, 1), strings.Repeat(" ", MaxProviderOAuthCodeRequestBytes) + valid} {
		got, err := DecodeProviderOAuthLoginCodeRequest([]byte(body))
		require.Error(t, err)
		require.Zero(t, got)
		require.NotContains(t, err.Error(), "synthetic-private")
	}
	request.Input = strings.Repeat("\x01", providerauth.OAuthLoginInputLimit)
	encoded, err = json.Marshal(request)
	require.NoError(t, err)
	require.Greater(t, len(encoded), MaxProviderAuthRequestBytes)
	got, err := DecodeProviderOAuthLoginCodeRequest(encoded)
	require.NoError(t, err)
	require.Equal(t, request, got)
	request.Input += "x"
	encoded, err = json.Marshal(request)
	require.NoError(t, err)
	_, err = DecodeProviderOAuthLoginCodeRequest(encoded)
	require.Error(t, err)
}

func TestProviderOAuthResponseExactIdentityAndActionPhase(t *testing.T) {
	ref := protoOAuthRef()
	state := providerauth.OAuthLoginState{Login: ref, Sequence: 2, Phase: providerauth.OAuthLoginWaitingLoopback, Callback: &providerauth.OAuthLoginCallback{Mode: "loopback-dynamic", Path: "/callback"}}
	original := ProviderOAuthLoginResponse{Login: ref, State: &state}
	data, err := json.Marshal(original)
	require.NoError(t, err)
	got, err := DecodeProviderOAuthLoginBeginResponse(data, ref)
	require.NoError(t, err)
	require.Equal(t, original, got)
	for _, mutate := range []func(*ProviderOAuthLoginResponse){
		func(r *ProviderOAuthLoginResponse) { r.Login.LoginID = strings.Repeat("f", 32) },
		func(r *ProviderOAuthLoginResponse) { r.Login.Target.Owner.ProviderID = "changed" },
		func(r *ProviderOAuthLoginResponse) { r.State.Login.Target.Generation.Sequence++ },
		func(r *ProviderOAuthLoginResponse) { r.SubmissionID = strings.Repeat("e", 32) },
		func(r *ProviderOAuthLoginResponse) { r.State.AuthorizationURL = "https://example.invalid/private" },
		func(r *ProviderOAuthLoginResponse) { r.State = nil },
	} {
		copy := original
		stateCopy := state
		copy.State = &stateCopy
		mutate(&copy)
		body, err := json.Marshal(copy)
		require.NoError(t, err)
		got, err := DecodeProviderOAuthLoginBeginResponse(body, ref)
		require.Error(t, err)
		require.Zero(t, got)
	}
	bind := providerauth.OAuthLoginBindRequest{Login: ref, BindingID: strings.Repeat("e", 32), Port: 32123}
	bound := ProviderOAuthLoginResponse{Login: ref, BindingID: bind.BindingID, Port: bind.Port, State: &state}
	require.Error(t, bound.ValidateBind(bind), "a successful bind cannot still wait for binding")
	state.Phase, state.Callback = providerauth.OAuthLoginPreparing, nil
	require.NoError(t, bound.ValidateBind(bind))
	bound.Port++
	require.Error(t, bound.ValidateBind(bind))
	submit := providerauth.OAuthLoginCodeRequest{Login: ref, SubmissionID: strings.Repeat("f", 32), Input: "private"}
	submitted := ProviderOAuthLoginResponse{Login: ref, SubmissionID: submit.SubmissionID, State: &state}
	require.Error(t, submitted.ValidateCode(submit))
	state.Phase = providerauth.OAuthLoginAuthorizing
	require.NoError(t, submitted.ValidateCode(submit))
	submitted.SubmissionID = bind.BindingID
	require.Error(t, submitted.ValidateCode(submit))
}

func TestProviderOAuthWaitFailureAndCompleteReceipt(t *testing.T) {
	ref := protoOAuthRef()
	state := providerauth.OAuthLoginState{Login: ref, Sequence: 3, Phase: providerauth.OAuthLoginAuthorized}
	response := ProviderOAuthLoginResponse{Login: ref, State: &state}
	require.NoError(t, response.ValidateWait(ref, 2))
	require.Error(t, response.ValidateWait(ref, 3))
	require.Error(t, response.ValidateWait(ref, 4))
	response.Error = NewProviderAuthenticationError(context.DeadlineExceeded)
	require.NoError(t, response.ValidateWait(ref, 3), "a canceled wait preserves its unchanged state")
	state.Phase = providerauth.OAuthLoginCanceled
	response.Error = NewProviderAuthenticationError(providerauth.ErrOAuthLogin)
	require.NoError(t, response.ValidateCancel(ref))
	body, err := json.Marshal(response)
	require.NoError(t, err)
	got, err := DecodeProviderOAuthLoginCancelResponse(body, ref)
	require.NoError(t, err)
	require.ErrorIs(t, got.Error, providerauth.ErrOAuthLogin)
	response.State, response.Error = nil, NewProviderAuthenticationError(providerauth.ErrOAuthLoginUnavailable)
	require.NoError(t, response.ValidateBegin(ref))
	response.Error = nil
	require.Error(t, response.ValidateBegin(ref))
	partial := ProviderAuthenticationMutationResponse{Outcome: providerauth.MutationOutcome{OperationID: ref.OperationID, LoginID: ref.LoginID, Previous: ref.Target, Progress: providerauth.MutationProgress{AccountsSaved: true}}, Error: NewProviderAuthenticationError(providerauth.ErrMutation)}
	body, err = json.Marshal(partial)
	require.NoError(t, err)
	completed, err := DecodeProviderOAuthLoginCompleteResponse(body, ref)
	require.NoError(t, err)
	require.Equal(t, partial, completed)
	ref.LoginID = strings.Repeat("f", 32)
	_, err = DecodeProviderOAuthLoginCompleteResponse(body, ref)
	require.Error(t, err)
	state.AuthorizationURL = "synthetic-private-url"
	response.State = &state
	for _, format := range []string{"%v", "%+v", "%#v"} {
		require.NotContains(t, fmt.Sprintf(format, response), "synthetic-private-url")
	}
}
