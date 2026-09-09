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

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/stretchr/testify/require"
)

func sdkAPIKeyCheckFixture() (providerauth.APIKeyCheckRequest, proto.ProviderAPIKeyCheckResponse) {
	_, state := clientProviderAuthFixture()
	request := providerauth.APIKeyCheckRequest{CheckID: strings.Repeat("b", 32), Target: state.Target, CredentialID: "provider.api_key", Source: "synthetic-secret-$LITERAL"}
	current := request.Target
	current.Generation.Sequence++
	response := proto.ProviderAPIKeyCheckResponse{Outcome: providerauth.APIKeyCheckOutcome{CheckID: request.CheckID, Previous: request.Target, CredentialID: request.CredentialID, CheckedTarget: &current, Probe: config.ConnectionProbeResult{Kind: config.ConnectionProbeHTTPResponse, Policy: config.ConnectionProbePolicyHTTP200, HTTPStatus: 200, EnteredKeyInAuthorization: true}}}
	return request, response
}

func TestProviderAPIKeySDKExactRoutesAndPartialSave(t *testing.T) {
	request, checked := sdkAPIKeyCheckFixture()
	save := providerauth.APIKeySaveRequest{OperationID: strings.Repeat("c", 32), CheckID: request.CheckID, Target: *checked.Outcome.CheckedTarget}
	partial := proto.ProviderAuthenticationMutationResponse{Outcome: providerauth.MutationOutcome{OperationID: save.OperationID, CheckID: save.CheckID, Previous: save.Target, Progress: providerauth.MutationProgress{ConfigSaved: true, RuntimePublished: true}}, Error: proto.NewProviderAuthenticationError(providerauth.ErrMutation)}
	var checks, saves atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		switch r.URL.Path {
		case "/v1/workspaces/workspace/auth/api-key/check":
			checks.Add(1)
			got, err := proto.DecodeProviderAPIKeyCheckRequest(body)
			require.NoError(t, err)
			require.Equal(t, request, got)
			require.NoError(t, json.NewEncoder(w).Encode(checked))
		case "/v1/workspaces/workspace/auth/api-key/save":
			saves.Add(1)
			got, err := proto.DecodeProviderAPIKeySaveRequest(body)
			require.NoError(t, err)
			require.Equal(t, save, got)
			require.NotContains(t, string(body), request.Source)
			w.WriteHeader(http.StatusUnprocessableEntity)
			require.NoError(t, json.NewEncoder(w).Encode(partial))
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	sdk := captureClient(t, srv)
	got, err := sdk.CheckProviderAPIKey(t.Context(), "workspace", request)
	require.NoError(t, err)
	require.Equal(t, checked, got)
	failed, err := sdk.SaveCheckedProviderAPIKey(t.Context(), "workspace", save)
	require.ErrorIs(t, err, providerauth.ErrMutation)
	require.Equal(t, partial, failed)
	require.EqualValues(t, 1, checks.Load())
	require.EqualValues(t, 1, saves.Load())
}

func TestProviderAPIKeySDKRejectsChangedReceipts(t *testing.T) {
	request, checked := sdkAPIKeyCheckFixture()
	save := providerauth.APIKeySaveRequest{OperationID: strings.Repeat("c", 32), CheckID: request.CheckID, Target: *checked.Outcome.CheckedTarget}
	partial := proto.ProviderAuthenticationMutationResponse{Outcome: providerauth.MutationOutcome{OperationID: save.OperationID, CheckID: save.CheckID, Previous: save.Target, Progress: providerauth.MutationProgress{ConfigSaved: true}}, Error: proto.NewProviderAuthenticationError(providerauth.ErrMutation)}
	for _, action := range []string{"check", "save"} {
		var original any = checked
		status := http.StatusOK
		if action == "save" {
			original = partial
			status = http.StatusUnprocessableEntity
		}
		data, err := json.Marshal(original)
		require.NoError(t, err)
		valid := string(data)
		for name, body := range map[string]string{
			"wrong CheckID":        strings.Replace(valid, request.CheckID, strings.Repeat("d", 32), 1),
			"wrong workspace":      strings.Replace(valid, `"workspace_id":"workspace"`, `"workspace_id":"other"`, 1),
			"check alias":          strings.Replace(valid, `"check_id":`, `"Check_ID":`, 1),
			"trailing":             valid + `{}`,
			"oversized":            strings.Repeat(" ", proto.MaxProviderAuthResponseBytes) + valid,
			"status contradiction": valid,
		} {
			t.Run(action+"/"+name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					code := status
					if name == "status contradiction" {
						if code == http.StatusOK {
							code = http.StatusBadRequest
						} else {
							code = http.StatusOK
						}
					}
					w.WriteHeader(code)
					_, _ = io.WriteString(w, body)
				}))
				defer srv.Close()
				sdk := captureClient(t, srv)
				if action == "check" {
					got, err := sdk.CheckProviderAPIKey(t.Context(), "workspace", request)
					require.Error(t, err)
					require.Zero(t, got)
				} else {
					got, err := sdk.SaveCheckedProviderAPIKey(t.Context(), "workspace", save)
					require.Error(t, err)
					require.Zero(t, got)
				}
			})
		}
	}
}

func TestProviderAPIKeySDKRejectsBeforeHTTP(t *testing.T) {
	request, _ := sdkAPIKeyCheckFixture()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid request reached HTTP") }))
	defer srv.Close()
	sdk := captureClient(t, srv)
	for _, id := range []string{"", "other", "workspace/other", " workspace"} {
		_, err := sdk.CheckProviderAPIKey(t.Context(), id, request)
		require.Error(t, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := sdk.CheckProviderAPIKey(ctx, "workspace", request)
	require.ErrorIs(t, err, context.Canceled)
	request.Source = strings.Repeat("x", (64<<10)+1)
	_, err = sdk.CheckProviderAPIKey(t.Context(), "workspace", request)
	require.Error(t, err)
}

func TestProviderAPIKeySDKNeverReplaysRedirectedInput(t *testing.T) {
	request, checked := sdkAPIKeyCheckFixture()
	save := providerauth.APIKeySaveRequest{OperationID: strings.Repeat("c", 32), CheckID: request.CheckID, Target: *checked.Outcome.CheckedTarget}
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer destination.Close()
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, destination.URL+r.URL.Path, status)
		}))
		sdk := captureClient(t, origin)
		got, err := sdk.CheckProviderAPIKey(t.Context(), "workspace", request)
		require.Error(t, err)
		require.Zero(t, got)
		result, err := sdk.SaveCheckedProviderAPIKey(t.Context(), "workspace", save)
		require.Error(t, err)
		require.Zero(t, result)
		require.Nil(t, sdk.h.CheckRedirect, "per-call protection must not mutate the shared client")
		origin.Close()
	}
	require.Zero(t, redirected.Load(), "redirected source or save intent reached another endpoint")
}
