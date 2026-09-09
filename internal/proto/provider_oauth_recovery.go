package proto

import (
	"errors"

	"github.com/example-git/crux/internal/providerauth"
)

type ProviderOAuthLoginRecoveryRequest = providerauth.OAuthLoginRecoveryRequest

// Listing is an authenticated owner-filtered read; it contains no interaction,
// account namespace, callback, token, source or private fingerprint.
type ProviderOAuthLoginRecoveryListResponse struct {
	List  providerauth.OAuthLoginRecoveryList `json:"list"`
	Error *ProviderAuthenticationError        `json:"error,omitempty"`
}

func (r ProviderOAuthLoginRecoveryListResponse) Validate(target providerauth.Target) error {
	if err := target.Validate(); err != nil {
		return err
	}
	if r.List.Target != target {
		return errors.New("OAuth recovery listing changed its target")
	}
	if err := r.List.Validate(); err != nil {
		return err
	}
	if r.Error != nil {
		return r.Error.Validate()
	}
	return nil
}
func DecodeProviderOAuthLoginRecoveryListRequest(body []byte) (providerauth.Target, error) {
	return decodeProviderOAuthRequest[providerauth.Target](body, MaxProviderAuthRequestBytes)
}
func DecodeProviderOAuthLoginRecoveryListResponse(body []byte, target providerauth.Target) (ProviderOAuthLoginRecoveryListResponse, error) {
	var response ProviderOAuthLoginRecoveryListResponse
	if err := decodeProviderAuthJSON(body, MaxProviderAuthResponseBytes, &response); err != nil {
		return ProviderOAuthLoginRecoveryListResponse{}, errors.New("invalid OAuth recovery listing")
	}
	if err := response.Validate(target); err != nil {
		return ProviderOAuthLoginRecoveryListResponse{}, err
	}
	return response, nil
}
func (r ProviderOAuthLoginResponse) ValidateRecover(request providerauth.OAuthLoginRecoveryRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if r.RecoveryOperationID != request.OriginalOperationID || r.State != nil && !r.State.MatchesOAuthLoginRecovery(request.OriginalOperationID) {
		return errors.New("OAuth recovery response changed the original operation")
	}
	return r.validate(request.Login, "", 0, "")
}
func DecodeProviderOAuthLoginRecoverRequest(body []byte) (providerauth.OAuthLoginRecoveryRequest, error) {
	return decodeProviderOAuthRequest[providerauth.OAuthLoginRecoveryRequest](body, MaxProviderAuthRequestBytes)
}
func DecodeProviderOAuthLoginRecoverResponse(body []byte, request providerauth.OAuthLoginRecoveryRequest) (ProviderOAuthLoginResponse, error) {
	return decodeProviderOAuthResponse(body, func(r ProviderOAuthLoginResponse) error { return r.ValidateRecover(request) })
}
