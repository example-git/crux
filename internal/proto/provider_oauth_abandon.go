package proto

import (
	"errors"

	"github.com/example-git/crux/internal/providerauth"
)

type ProviderOAuthLoginAbandonRequest = providerauth.OAuthLoginAbandonRequest
type ProviderOAuthLoginAbandonResponse struct {
	Outcome providerauth.OAuthLoginAbandonOutcome `json:"outcome"`
	Error   *ProviderAuthenticationError          `json:"error,omitempty"`
}

func (r ProviderOAuthLoginAbandonResponse) Validate(request providerauth.OAuthLoginAbandonRequest) error {
	if err := r.Outcome.Validate(request); err != nil {
		return err
	}
	if r.Error != nil {
		return r.Error.Validate()
	}
	if !r.Outcome.Abandoned {
		return errors.New("OAuth abandonment has no acknowledged retirement")
	}
	return nil
}
func DecodeProviderOAuthLoginAbandonRequest(body []byte) (providerauth.OAuthLoginAbandonRequest, error) {
	return decodeProviderOAuthRequest[providerauth.OAuthLoginAbandonRequest](body, MaxProviderAuthRequestBytes)
}
func DecodeProviderOAuthLoginAbandonResponse(body []byte, request providerauth.OAuthLoginAbandonRequest) (ProviderOAuthLoginAbandonResponse, error) {
	var response ProviderOAuthLoginAbandonResponse
	if err := decodeProviderAuthJSON(body, MaxProviderAuthResponseBytes, &response); err != nil {
		return ProviderOAuthLoginAbandonResponse{}, errors.New("invalid OAuth abandonment response")
	}
	if err := response.Validate(request); err != nil {
		return ProviderOAuthLoginAbandonResponse{}, err
	}
	return response, nil
}
