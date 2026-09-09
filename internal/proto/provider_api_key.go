package proto

import (
	"errors"

	"github.com/example-git/crux/internal/providerauth"
)

// A check admits at most 64 KiB of source text. JSON escaping plus the bounded
// owner/target can enlarge that input; other authentication requests stay 16 KiB.
const MaxProviderAPIKeyCheckRequestBytes = 512 << 10

// ProviderAPIKeyCheckResponse contains observation evidence, never the entered
// source, resolved credential, endpoint or private preparation. A checked target
// may be historical; Save must still revalidate its exact retained receipt.
type ProviderAPIKeyCheckResponse struct {
	Outcome providerauth.APIKeyCheckOutcome `json:"outcome"`
	Error   *ProviderAuthenticationError    `json:"error,omitempty"`
}

func (r ProviderAPIKeyCheckResponse) Validate(request providerauth.APIKeyCheckRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := r.Outcome.Validate(); err != nil {
		return err
	}
	if r.Outcome.CheckID != request.CheckID || r.Outcome.Previous != request.Target || r.Outcome.CredentialID != request.CredentialID {
		return errors.New("API key check response does not match the requested check")
	}
	if r.Error != nil {
		if err := r.Error.Validate(); err != nil {
			return err
		}
		if r.Outcome.CheckedTarget != nil {
			return errors.New("failed API key check cannot authorize a save target")
		}
		return nil
	}
	if r.Outcome.CheckedTarget == nil {
		return errors.New("API key check response has no checked target")
	}
	return nil
}

func (r ProviderAuthenticationMutationResponse) ValidateAPIKeySave(request providerauth.APIKeySaveRequest) error {
	if err := r.Outcome.ValidateAPIKeySave(request); err != nil {
		return err
	}
	return r.validate()
}

func DecodeProviderAPIKeyCheckRequest(body []byte) (providerauth.APIKeyCheckRequest, error) {
	var request providerauth.APIKeyCheckRequest
	if err := decodeProviderAuthJSON(body, MaxProviderAPIKeyCheckRequestBytes, &request); err != nil {
		// Decoder diagnostics must not disclose a malformed secret-bearing value.
		return providerauth.APIKeyCheckRequest{}, errors.New("invalid API key check request")
	}
	if err := request.Validate(); err != nil {
		return providerauth.APIKeyCheckRequest{}, err
	}
	return request, nil
}

func DecodeProviderAPIKeySaveRequest(body []byte) (providerauth.APIKeySaveRequest, error) {
	var request providerauth.APIKeySaveRequest
	if err := decodeProviderAuthJSON(body, MaxProviderAuthRequestBytes, &request); err != nil {
		return request, err
	}
	return request, request.Validate()
}

func DecodeProviderAPIKeyCheckResponse(body []byte, request providerauth.APIKeyCheckRequest) (ProviderAPIKeyCheckResponse, error) {
	var response ProviderAPIKeyCheckResponse
	if err := decodeProviderAuthJSON(body, MaxProviderAuthResponseBytes, &response); err != nil {
		return ProviderAPIKeyCheckResponse{}, errors.New("invalid API key check response")
	}
	if err := response.Validate(request); err != nil {
		return ProviderAPIKeyCheckResponse{}, err
	}
	return response, nil
}

func DecodeProviderAPIKeySaveResponse(body []byte, request providerauth.APIKeySaveRequest) (ProviderAuthenticationMutationResponse, error) {
	var response ProviderAuthenticationMutationResponse
	if err := decodeProviderAuthJSON(body, MaxProviderAuthResponseBytes, &response); err != nil {
		return ProviderAuthenticationMutationResponse{}, errors.New("invalid checked API key save response")
	}
	if err := response.ValidateAPIKeySave(request); err != nil {
		return ProviderAuthenticationMutationResponse{}, err
	}
	return response, nil
}
