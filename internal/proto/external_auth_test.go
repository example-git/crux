package proto

import (
	"encoding/json"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/stretchr/testify/require"
)

func TestExternalAuthenticationWireValidation(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"service":"fixture","Service":"other"}`, `{"service":"fixture","include_token":null}`, `{"service":"fixture","include_token":"true"}`, `{"service":"fixture","extra":true}`, `{"service":"fixture"}{}`} {
		_, err := DecodeExternalAuthenticationRequest([]byte(body))
		require.Error(t, err, body)
	}
	request, err := DecodeExternalAuthenticationRequest([]byte(`{"service":"fixture"}`))
	require.NoError(t, err)
	require.False(t, request.IncludeToken)
	require.False(t, request.RefreshToken)
	response := ExternalAuthenticationResponse{WorkspaceID: "workspace", Service: "fixture", Authentication: config.ExternalAuthentication{Required: true, Method: "oauth", Token: "synthetic-secret"}}
	body, err := json.Marshal(response)
	require.NoError(t, err)
	_, err = DecodeExternalAuthenticationResponse(body, "workspace", request)
	require.Error(t, err)
	request.IncludeToken = true
	got, err := DecodeExternalAuthenticationResponse(body, "workspace", request)
	require.NoError(t, err)
	require.Equal(t, response.Authentication, got)
	_, err = DecodeExternalAuthenticationResponse(body, "other", request)
	require.Error(t, err)
	request.Service = "other"
	_, err = DecodeExternalAuthenticationResponse(body, "workspace", request)
	require.Error(t, err)
	request.Service = "fixture"
	for _, malformed := range []string{`{}`, `{"workspace_id":"workspace","service":"fixture","authentication":{}}`, `{"workspace_id":"workspace","service":"fixture","authentication":{"required":null}}`, string(body) + `{}`} {
		_, err = DecodeExternalAuthenticationResponse([]byte(malformed), "workspace", request)
		require.Error(t, err)
	}
	for _, state := range []config.ExternalAuthentication{{Token: "synthetic-secret"}, {Method: "oauth"}, {Required: true, Token: "synthetic-secret"}} {
		response.Authentication = state
		body, err = json.Marshal(response)
		require.NoError(t, err)
		_, err = DecodeExternalAuthenticationResponse(body, "workspace", request)
		require.Error(t, err)
	}
}
