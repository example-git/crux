package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/stretchr/testify/require"
)

func TestProviderAPIKeyRoutesRejectMalformedBeforeBackend(t *testing.T) {
	target := providerauth.Target{WorkspaceID: "workspace", Owner: providerauth.Owner{ProviderID: "provider"}, Generation: providerauth.Generation{Epoch: strings.Repeat("a", 32), Sequence: 1}}
	for _, action := range []string{"check", "save"} {
		var request any = providerauth.APIKeyCheckRequest{CheckID: strings.Repeat("b", 32), Target: target, CredentialID: "provider.api_key", Source: "synthetic-private"}
		maximum := proto.MaxProviderAPIKeyCheckRequestBytes
		if action == "save" {
			request = providerauth.APIKeySaveRequest{CheckID: strings.Repeat("b", 32), OperationID: strings.Repeat("c", 32), Target: target}
			maximum = proto.MaxProviderAuthRequestBytes
		}
		encoded, err := json.Marshal(request)
		require.NoError(t, err)
		valid := string(encoded)
		for _, body := range []string{`null`, `{}`, valid + `{}`, strings.Replace(valid, `"check_id":`, `"Check_ID":`, 1), strings.Replace(valid, `"target":{`, `"target":{"unknown":"synthetic-private",`, 1), strings.Replace(valid, `"workspace_id":"workspace"`, `"workspace_id":"other"`, 1), strings.Repeat(" ", maximum) + valid} {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body))
			r.SetPathValue("id", "workspace")
			w := httptest.NewRecorder()
			c := &controllerV1{}
			if action == "check" {
				c.handlePostWorkspaceAPIKeyCheck(w, r)
			} else {
				c.handlePostWorkspaceAPIKeySave(w, r)
			}
			require.Equal(t, http.StatusBadRequest, w.Code)
			require.NotContains(t, w.Body.String(), "synthetic-private")
		}
	}
}

func TestProviderAPIKeyOversizedSaveRetainsCheckID(t *testing.T) {
	target := providerauth.Target{WorkspaceID: "workspace", Owner: providerauth.Owner{ProviderID: "provider"}, Generation: providerauth.Generation{Epoch: strings.Repeat("a", 32), Sequence: 1}}
	request := providerauth.APIKeySaveRequest{OperationID: strings.Repeat("b", 32), CheckID: strings.Repeat("c", 32), Target: target}
	response := proto.ProviderAuthenticationMutationResponse{Outcome: providerauth.MutationOutcome{OperationID: request.OperationID, CheckID: request.CheckID, Previous: target, Progress: providerauth.MutationProgress{ConfigSaved: true, RuntimePublished: true}}, Workspace: &proto.AuthenticationWorkspaceView{ID: strings.Repeat("x", proto.MaxProviderAuthResponseBytes)}}
	w := httptest.NewRecorder()
	writeProviderAuthMutationResponse(w, response)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	got, err := proto.DecodeProviderAPIKeySaveResponse(w.Body.Bytes(), request)
	require.NoError(t, err)
	require.Equal(t, response.Outcome, got.Outcome)
	require.Nil(t, got.Workspace)
	require.ErrorIs(t, got.Error, providerauth.ErrReceiptUnverified)
}
