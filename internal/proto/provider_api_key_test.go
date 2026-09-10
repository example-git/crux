package proto

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/stretchr/testify/require"
)

func keyCheckRequestFixture() providerauth.APIKeyCheckRequest {
	return providerauth.APIKeyCheckRequest{
		CheckID: strings.Repeat("a", 32), CredentialID: "provider.api_key", Source: "synthetic-secret",
		Target: providerauth.Target{WorkspaceID: "workspace", Owner: providerauth.Owner{ProviderID: "provider"}, Generation: providerauth.Generation{Epoch: strings.Repeat("b", 32), Sequence: 1}},
	}
}

func TestProviderAPIKeyCheckStrictSecretRequest(t *testing.T) {
	request := keyCheckRequestFixture()
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	valid := string(encoded)
	for _, body := range []string{`null`, `{}`, valid + `{}`, strings.Replace(valid, `"source":`, `"source":"synthetic-private","source":`, 1), strings.Replace(valid, `"source":`, `"Source":`, 1), strings.Replace(valid, `"source":"synthetic-secret"`, `"source":{"private":"synthetic-secret"}`, 1), strings.Replace(valid, `"target":{`, `"target":{"account_namespace":"private",`, 1), strings.Repeat(" ", MaxProviderAPIKeyCheckRequestBytes) + valid} {
		decoded, err := DecodeProviderAPIKeyCheckRequest([]byte(body))
		require.Error(t, err)
		require.Zero(t, decoded)
		require.NotContains(t, err.Error(), "synthetic-secret")
		require.NotContains(t, err.Error(), "synthetic-private")
	}
	request.Source = "key" + strings.Repeat("\x01", (64<<10)-3)
	encoded, err = json.Marshal(request)
	require.NoError(t, err)
	require.Greater(t, len(encoded), MaxProviderAuthRequestBytes)
	decoded, err := DecodeProviderAPIKeyCheckRequest(encoded)
	require.NoError(t, err)
	require.Equal(t, request, decoded)
}

func TestProviderAPIKeyResponsesBindCheckAndPreserveEvidence(t *testing.T) {
	request := keyCheckRequestFixture()
	outcome := providerauth.APIKeyCheckOutcome{
		CheckID: request.CheckID, Previous: request.Target, CredentialID: request.CredentialID,
		Probe: config.ConnectionProbeResult{Kind: config.ConnectionProbeHTTPResponse, Policy: config.ConnectionProbePolicyHTTP200, HTTPStatus: 401, EnteredKeyInAuthorization: true},
	}
	response := ProviderAPIKeyCheckResponse{Outcome: outcome, Error: NewProviderAuthenticationError(providerauth.ErrAPIKeyCheck)}
	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	decoded, err := DecodeProviderAPIKeyCheckResponse(encoded, request)
	require.NoError(t, err)
	require.Equal(t, response, decoded)
	require.ErrorIs(t, decoded.Error, providerauth.ErrAPIKeyCheck)
	for _, mutate := range []func(*ProviderAPIKeyCheckResponse){
		func(r *ProviderAPIKeyCheckResponse) { r.Outcome.CheckID = strings.Repeat("c", 32) },
		func(r *ProviderAPIKeyCheckResponse) { r.Outcome.CredentialID = "another-slot" },
		func(r *ProviderAPIKeyCheckResponse) { r.Outcome.Previous.Owner.ProviderID = "another-owner" },
		func(r *ProviderAPIKeyCheckResponse) { r.Error = nil },
		func(r *ProviderAPIKeyCheckResponse) {
			target := request.Target
			target.Generation.Sequence++
			r.Outcome.CheckedTarget = &target
		},
	} {
		copy := response
		mutate(&copy)
		body, err := json.Marshal(copy)
		require.NoError(t, err)
		decoded, err := DecodeProviderAPIKeyCheckResponse(body, request)
		require.Error(t, err)
		require.Zero(t, decoded)
	}
	save := providerauth.APIKeySaveRequest{OperationID: strings.Repeat("d", 32), Target: request.Target, CheckID: request.CheckID}
	partial := ProviderAuthenticationMutationResponse{Outcome: providerauth.MutationOutcome{OperationID: save.OperationID, CheckID: save.CheckID, Previous: save.Target, Progress: providerauth.MutationProgress{ConfigSaved: true, RuntimePublished: true}}, Error: NewProviderAuthenticationError(providerauth.ErrMutation)}
	body, err := json.Marshal(partial)
	require.NoError(t, err)
	got, err := DecodeProviderAPIKeySaveResponse(body, save)
	require.NoError(t, err)
	require.Equal(t, partial, got)
	save.CheckID = strings.Repeat("e", 32)
	got, err = DecodeProviderAPIKeySaveResponse(body, save)
	require.Error(t, err)
	require.Zero(t, got)
}
