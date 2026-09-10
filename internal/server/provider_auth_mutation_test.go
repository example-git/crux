package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/stretchr/testify/require"
)

func TestProviderAuthMutationRoutesRejectMalformedBeforeBackend(t *testing.T) {
	target := `{"workspace_id":"workspace","owner":{"provider_id":"provider","has_oauth":true},"generation":{"epoch":"` + strings.Repeat("a", 32) + `","sequence":1}}`
	for _, action := range []string{"switch", "logout"} {
		valid := `{"operation_id":"` + strings.Repeat("b", 32) + `","target":` + target
		if action == "switch" {
			valid += `,"account_id":"selected"`
		}
		valid += `}`
		for _, body := range []string{`null`, `{}`, valid + `{}`, strings.Replace(valid, `"target":{`, `"target":{"unknown":1,`, 1), strings.Replace(valid, `"operation_id"`, `"Operation_ID"`, 1), strings.Replace(valid, `"workspace_id":"workspace"`, `"workspace_id":"other"`, 1), strings.Repeat(" ", proto.MaxProviderAuthRequestBytes) + valid} {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body))
			r.SetPathValue("id", "workspace")
			w := httptest.NewRecorder()
			controller := &controllerV1{}
			if action == "switch" {
				controller.handlePostWorkspaceProviderSwitch(w, r)
			} else {
				controller.handlePostWorkspaceProviderLogout(w, r)
			}
			require.Equal(t, http.StatusBadRequest, w.Code)
		}
	}
}

func TestProviderAuthMutationResponseLimitBeforeWrite(t *testing.T) {
	response := proto.ProviderAuthenticationMutationResponse{Outcome: providerauth.MutationOutcome{OperationID: strings.Repeat("x", proto.MaxProviderAuthResponseBytes)}}
	recorder := httptest.NewRecorder()
	writeProviderAuthMutationResponse(recorder, response)
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	body, err := io.ReadAll(recorder.Result().Body)
	require.NoError(t, err)
	require.Less(t, len(body), 1000)
}

func TestProviderAuthMutationOversizedViewRetainsProgress(t *testing.T) {
	target := providerauth.Target{WorkspaceID: "workspace", Owner: providerauth.Owner{ProviderID: "provider"}, Generation: providerauth.Generation{Epoch: strings.Repeat("a", 32), Sequence: 1}}
	request := providerauth.LogoutRequest{OperationID: strings.Repeat("b", 32), Target: target}
	response := proto.ProviderAuthenticationMutationResponse{Outcome: providerauth.MutationOutcome{OperationID: request.OperationID, Previous: target, Progress: providerauth.MutationProgress{AccountsSaved: true, ConfigSaved: true, RuntimePublished: true}}, Workspace: &proto.AuthenticationWorkspaceView{ID: strings.Repeat("x", proto.MaxProviderAuthResponseBytes)}}
	recorder := httptest.NewRecorder()
	writeProviderAuthMutationResponse(recorder, response)
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	decoded, err := proto.DecodeProviderAuthLogoutResponse(recorder.Body.Bytes(), request)
	require.NoError(t, err)
	require.Equal(t, response.Outcome, decoded.Outcome)
	require.Nil(t, decoded.Workspace)
	require.ErrorIs(t, decoded.Error, providerauth.ErrReceiptUnverified)
}
