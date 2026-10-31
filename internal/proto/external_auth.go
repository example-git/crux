package proto

import (
	"errors"
	"fmt"

	"github.com/example-git/crux/internal/config"
)

type ExternalAuthenticationResponse struct {
	WorkspaceID    string                        `json:"workspace_id"`
	Service        string                        `json:"service"`
	Authentication config.ExternalAuthentication `json:"authentication"`
}

func (ExternalAuthenticationResponse) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private external authentication response]"))
}

func DecodeExternalAuthenticationRequest(body []byte) (config.ExternalAuthenticationRequest, error) {
	var request config.ExternalAuthenticationRequest
	if err := decodeProviderAuthJSON(body, MaxProviderAuthRequestBytes, &request); err != nil {
		return request, errors.New("invalid external authentication request")
	}
	return request, request.Validate()
}

func DecodeExternalAuthenticationResponse(body []byte, workspaceID string, request config.ExternalAuthenticationRequest) (config.ExternalAuthentication, error) {
	var response ExternalAuthenticationResponse
	if err := decodeProviderAuthJSON(body, MaxProviderAuthResponseBytes, &response); err != nil {
		return config.ExternalAuthentication{}, errors.New("invalid external authentication response")
	}
	fields, err := providerAuthFields(body, "workspace_id", "service", "authentication")
	if err != nil {
		return config.ExternalAuthentication{}, err
	}
	if _, err := providerAuthFields(fields["authentication"], "required"); err != nil {
		return config.ExternalAuthentication{}, err
	}
	state := response.Authentication
	if response.WorkspaceID != workspaceID || response.Service != request.Service ||
		!state.Required && (state.Method != "" || state.Token != "") ||
		state.Token != "" && (!request.IncludeToken || state.Method == "") {
		return config.ExternalAuthentication{}, errors.New("external authentication response does not match request")
	}
	return state, nil
}
