package proto

import (
	"fmt"

	"github.com/example-git/crux/internal/providerauth"
)

func (r ProviderAuthenticationMutationResponse) ValidateRemove(request providerauth.RemoveRequest) error {
	if err := r.Outcome.ValidateRemove(request); err != nil {
		return err
	}
	return r.validate()
}

func DecodeProviderAuthRemoveRequest(body []byte) (providerauth.RemoveRequest, error) {
	var request providerauth.RemoveRequest
	if err := decodeProviderAuthJSON(body, MaxProviderAuthRequestBytes, &request); err != nil {
		return request, err
	}
	return request, request.Validate()
}

func DecodeProviderAuthRemoveResponse(body []byte, request providerauth.RemoveRequest) (ProviderAuthenticationMutationResponse, error) {
	var response ProviderAuthenticationMutationResponse
	if err := decodeProviderAuthJSON(body, MaxProviderAuthResponseBytes, &response); err != nil {
		return response, err
	}
	if err := response.ValidateRemove(request); err != nil {
		return ProviderAuthenticationMutationResponse{}, fmt.Errorf("invalid authentication remove response: %w", err)
	}
	return response, nil
}
