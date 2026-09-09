package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/stretchr/testify/require"
)

func TestProviderAuthMutationSDKPreservesBoundPartialErrors(t *testing.T) {
	_, accounts := clientProviderAuthFixture()
	target := accounts.Target
	target.Owner.HasOAuth = true
	operation := strings.Repeat("b", 32)
	for _, action := range []string{"switch", "logout"} {
		t.Run(action, func(t *testing.T) {
			outcome := providerauth.MutationOutcome{OperationID: operation, Previous: target, Progress: providerauth.MutationProgress{AccountRefreshed: true, AccountsSaved: true, ConfigSaved: true, RuntimePublished: true}}
			expected := proto.ProviderAuthenticationMutationResponse{Outcome: outcome, Error: proto.NewProviderAuthenticationError(providerauth.ErrMutation)}
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "/v1/workspaces/workspace/auth/"+action, r.URL.Path)
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				if action == "switch" {
					request, err := proto.DecodeProviderAuthSwitchRequest(body)
					require.NoError(t, err)
					require.Equal(t, target, request.Target)
					require.Equal(t, "selected", request.AccountID)
					require.Equal(t, operation, request.OperationID)
				} else {
					request, err := proto.DecodeProviderAuthLogoutRequest(body)
					require.NoError(t, err)
					require.Equal(t, target, request.Target)
					require.Equal(t, operation, request.OperationID)
				}
				w.WriteHeader(http.StatusUnprocessableEntity)
				require.NoError(t, json.NewEncoder(w).Encode(expected))
			}))
			defer srv.Close()
			sdk := captureClient(t, srv)
			var got proto.ProviderAuthenticationMutationResponse
			var err error
			if action == "switch" {
				got, err = sdk.SwitchProviderAccount(t.Context(), "workspace", providerauth.SwitchRequest{OperationID: operation, Target: target, AccountID: "selected"})
			} else {
				got, err = sdk.LogoutProvider(t.Context(), "workspace", providerauth.LogoutRequest{OperationID: operation, Target: target})
			}
			require.ErrorIs(t, err, providerauth.ErrMutation)
			require.Equal(t, expected, got)
			require.Nil(t, got.Workspace)
			require.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestProviderAuthMutationSDKRejectsUnboundAndContradictoryEnvelopes(t *testing.T) {
	_, accounts := clientProviderAuthFixture()
	target := accounts.Target
	target.Owner.HasOAuth = true
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("b", 32), Target: target, AccountID: "selected"}
	result := proto.ProviderAuthenticationMutationResponse{Outcome: providerauth.MutationOutcome{OperationID: request.OperationID, Previous: target, Progress: providerauth.MutationProgress{AccountsSaved: true}}, Error: proto.NewProviderAuthenticationError(providerauth.ErrMutation)}
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	valid := string(encoded)
	for name, body := range map[string]string{
		"wrong operation":      strings.Replace(valid, request.OperationID, strings.Repeat("c", 32), 1),
		"wrong target":         strings.Replace(valid, `"workspace_id":"workspace"`, `"workspace_id":"other"`, 1),
		"null error":           strings.Replace(valid, `"error":{`, `"error":null,"ignored":{`, 1),
		"alias progress":       strings.Replace(valid, `"accounts_saved":true`, `"Accounts_Saved":true`, 1),
		"private message":      strings.Replace(valid, providerauth.ErrMutation.Error(), "private-token", 1),
		"oversized":            strings.Repeat(" ", proto.MaxProviderAuthResponseBytes) + valid,
		"status contradiction": valid,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if name != "status contradiction" {
					w.WriteHeader(http.StatusUnprocessableEntity)
				}
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			got, err := captureClient(t, srv).SwitchProviderAccount(t.Context(), "workspace", request)
			require.Error(t, err)
			require.Zero(t, got)
		})
	}
}

func TestProviderAuthMutationSDKHistoricalReceiptHasNoView(t *testing.T) {
	_, state := clientProviderAuthFixture()
	state.Target.Owner.HasOAuth = true
	state.Status.Owner = state.Target.Owner
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("b", 32), Target: state.Target, AccountID: "account"}
	state.Target.Generation.Sequence++
	state.Status.Credentials = []providerauth.CredentialStatus{{Kind: "api-key", State: "configured"}, {Kind: "oauth", State: "present"}}
	state.Accounts[0].CredentialState = "present"
	response := proto.ProviderAuthenticationMutationResponse{Outcome: providerauth.MutationOutcome{OperationID: request.OperationID, Previous: request.Target, Progress: providerauth.MutationProgress{RuntimePublished: true}, Superseded: true, Change: &providerauth.Change{OperationID: request.OperationID, Previous: request.Target, Current: state}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { require.NoError(t, json.NewEncoder(w).Encode(response)) }))
	defer srv.Close()
	got, err := captureClient(t, srv).SwitchProviderAccount(t.Context(), "workspace", request)
	require.NoError(t, err)
	require.Equal(t, response, got)
	require.Nil(t, got.Workspace)
}

func TestProviderAuthMutationSDKRejectsBeforeHTTP(t *testing.T) {
	_, state := clientProviderAuthFixture()
	state.Target.Owner.HasOAuth = true
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("b", 32), Target: state.Target, AccountID: "selected"}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid request reached HTTP") }))
	defer srv.Close()
	sdk := captureClient(t, srv)
	for _, id := range []string{"", "other", "workspace/other", " workspace"} {
		_, err := sdk.SwitchProviderAccount(t.Context(), id, request)
		require.Error(t, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := sdk.SwitchProviderAccount(ctx, "workspace", request)
	require.ErrorIs(t, err, context.Canceled)
	request.OperationID = "invalid"
	_, err = sdk.SwitchProviderAccount(t.Context(), "workspace", request)
	require.Error(t, err)
}
