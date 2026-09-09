package proto

import (
	"errors"
	"fmt"

	"github.com/example-git/crux/internal/providerauth"
)

// A valid 16 KiB code input may expand sixfold when JSON escaped. The decoded
// limit remains providerauth.OAuthLoginInputLimit; no input is truncated.
const MaxProviderOAuthCodeRequestBytes = 128 << 10

type ProviderOAuthLoginWaitRequest struct {
	Login providerauth.OAuthLoginRef `json:"login"`
	After uint64                     `json:"after"`
}

func (r ProviderOAuthLoginWaitRequest) Validate() error { return r.Login.Validate() }

// State is absent only when no valid retained session could be returned. The
// echoed action identity never includes submitted input or a credential hash.
// A complete interaction state is not a runtime acknowledgement; Complete uses
// ProviderAuthenticationMutationResponse and its exact transaction receipt.
type ProviderOAuthLoginResponse struct {
	Login               providerauth.OAuthLoginRef    `json:"login"`
	RecoveryOperationID string                        `json:"recovery_operation_id,omitempty"`
	BindingID           string                        `json:"binding_id,omitempty"`
	Port                uint16                        `json:"port,omitempty"`
	SubmissionID        string                        `json:"submission_id,omitempty"`
	State               *providerauth.OAuthLoginState `json:"state,omitempty"`
	Error               *ProviderAuthenticationError  `json:"error,omitempty"`
}

func (ProviderOAuthLoginResponse) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private OAuth login response]"))
}

func (r ProviderOAuthLoginResponse) validate(ref providerauth.OAuthLoginRef, bindingID string, port uint16, submissionID string) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if r.Login != ref || r.BindingID != bindingID || r.Port != port || r.SubmissionID != submissionID {
		return errors.New("OAuth login response does not match the requested action")
	}
	if r.Error != nil {
		if err := r.Error.Validate(); err != nil {
			return err
		}
	}
	if r.State == nil {
		if r.Error == nil {
			return errors.New("OAuth login response has no state")
		}
		return nil
	}
	if err := r.State.Validate(); err != nil {
		return err
	}
	if r.RecoveryOperationID != "" && !r.State.MatchesOAuthLoginRecovery(r.RecoveryOperationID) {
		return errors.New("OAuth response changed the recorded recovery")
	}
	if r.State.Login != ref {
		return errors.New("OAuth login state changed the requested login")
	}
	if r.Error == nil {
		switch r.State.Phase {
		case providerauth.OAuthLoginCanceled, providerauth.OAuthLoginExpired, providerauth.OAuthLoginFailed:
			return errors.New("failed OAuth login state has no error")
		}
	}
	return nil
}

func (r ProviderOAuthLoginResponse) ValidateBegin(request providerauth.OAuthLoginRequest) error {
	if r.RecoveryOperationID != "" || r.State != nil && r.State.Recovery != nil {
		return errors.New("OAuth begin response substituted a recorded recovery")
	}
	return r.validate(request, "", 0, "")
}
func (r ProviderOAuthLoginResponse) ValidateBind(request providerauth.OAuthLoginBindRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := r.validate(request.Login, request.BindingID, request.Port, ""); err != nil {
		return err
	}
	if r.Error == nil {
		switch r.State.Phase {
		case providerauth.OAuthLoginPreparing, providerauth.OAuthLoginWaitingBrowser, providerauth.OAuthLoginAuthorizing, providerauth.OAuthLoginAuthorized, providerauth.OAuthLoginCommitting, providerauth.OAuthLoginComplete:
		default:
			return errors.New("OAuth bind response has not admitted the binding")
		}
	}
	return nil
}
func (r ProviderOAuthLoginResponse) ValidateCode(request providerauth.OAuthLoginCodeRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := r.validate(request.Login, "", 0, request.SubmissionID); err != nil {
		return err
	}
	if r.Error == nil {
		switch r.State.Phase {
		case providerauth.OAuthLoginAuthorizing, providerauth.OAuthLoginAuthorized, providerauth.OAuthLoginCommitting, providerauth.OAuthLoginComplete:
		default:
			return errors.New("OAuth code response has not admitted the input")
		}
	}
	return nil
}
func (r ProviderOAuthLoginResponse) ValidateWait(ref providerauth.OAuthLoginRef, after uint64) error {
	if err := r.validate(ref, "", 0, ""); err != nil {
		return err
	}
	if r.State != nil && (r.State.Sequence < after || r.Error == nil && r.State.Sequence == after && r.State.Phase != providerauth.OAuthLoginComplete) {
		return errors.New("OAuth login wait response did not satisfy its cursor")
	}
	return nil
}
func (r ProviderOAuthLoginResponse) ValidateCancel(ref providerauth.OAuthLoginRef) error {
	if err := r.validate(ref, "", 0, ""); err != nil {
		return err
	}
	if r.Error == nil && r.State.Phase != providerauth.OAuthLoginCommitting && r.State.Phase != providerauth.OAuthLoginComplete {
		return errors.New("OAuth cancellation response has not ended the interaction")
	}
	return nil
}
func (r ProviderAuthenticationMutationResponse) ValidateOAuthLogin(ref providerauth.OAuthLoginRef) error {
	if err := r.Outcome.ValidateOAuthLogin(ref); err != nil {
		return err
	}
	return r.validate()
}

func decodeProviderOAuthRequest[T interface{ Validate() error }](body []byte, maximum int) (T, error) {
	var value T
	var zero T
	if err := decodeProviderAuthJSON(body, maximum, &value); err != nil {
		return zero, errors.New("invalid OAuth login request")
	}
	if err := value.Validate(); err != nil {
		return zero, err
	}
	return value, nil
}

func DecodeProviderOAuthLoginBeginRequest(body []byte) (providerauth.OAuthLoginRequest, error) {
	return decodeProviderOAuthRequest[providerauth.OAuthLoginRequest](body, MaxProviderAuthRequestBytes)
}
func DecodeProviderOAuthLoginBindRequest(body []byte) (providerauth.OAuthLoginBindRequest, error) {
	return decodeProviderOAuthRequest[providerauth.OAuthLoginBindRequest](body, MaxProviderAuthRequestBytes)
}
func DecodeProviderOAuthLoginCodeRequest(body []byte) (providerauth.OAuthLoginCodeRequest, error) {
	return decodeProviderOAuthRequest[providerauth.OAuthLoginCodeRequest](body, MaxProviderOAuthCodeRequestBytes)
}
func DecodeProviderOAuthLoginWaitRequest(body []byte) (ProviderOAuthLoginWaitRequest, error) {
	return decodeProviderOAuthRequest[ProviderOAuthLoginWaitRequest](body, MaxProviderAuthRequestBytes)
}
func DecodeProviderOAuthLoginCancelRequest(body []byte) (providerauth.OAuthLoginRef, error) {
	return decodeProviderOAuthRequest[providerauth.OAuthLoginRef](body, MaxProviderAuthRequestBytes)
}
func DecodeProviderOAuthLoginCompleteRequest(body []byte) (providerauth.OAuthLoginRef, error) {
	return decodeProviderOAuthRequest[providerauth.OAuthLoginRef](body, MaxProviderAuthRequestBytes)
}

func decodeProviderOAuthResponse(body []byte, validate func(ProviderOAuthLoginResponse) error) (ProviderOAuthLoginResponse, error) {
	var response ProviderOAuthLoginResponse
	if err := decodeProviderAuthJSON(body, MaxProviderAuthResponseBytes, &response); err != nil {
		return ProviderOAuthLoginResponse{}, errors.New("invalid OAuth login response")
	}
	if err := validate(response); err != nil {
		return ProviderOAuthLoginResponse{}, err
	}
	return response, nil
}

func DecodeProviderOAuthLoginBeginResponse(body []byte, request providerauth.OAuthLoginRequest) (ProviderOAuthLoginResponse, error) {
	return decodeProviderOAuthResponse(body, func(r ProviderOAuthLoginResponse) error { return r.ValidateBegin(request) })
}
func DecodeProviderOAuthLoginBindResponse(body []byte, request providerauth.OAuthLoginBindRequest) (ProviderOAuthLoginResponse, error) {
	return decodeProviderOAuthResponse(body, func(r ProviderOAuthLoginResponse) error { return r.ValidateBind(request) })
}
func DecodeProviderOAuthLoginCodeResponse(body []byte, request providerauth.OAuthLoginCodeRequest) (ProviderOAuthLoginResponse, error) {
	return decodeProviderOAuthResponse(body, func(r ProviderOAuthLoginResponse) error { return r.ValidateCode(request) })
}
func DecodeProviderOAuthLoginWaitResponse(body []byte, ref providerauth.OAuthLoginRef, after uint64) (ProviderOAuthLoginResponse, error) {
	return decodeProviderOAuthResponse(body, func(r ProviderOAuthLoginResponse) error { return r.ValidateWait(ref, after) })
}
func DecodeProviderOAuthLoginCancelResponse(body []byte, ref providerauth.OAuthLoginRef) (ProviderOAuthLoginResponse, error) {
	return decodeProviderOAuthResponse(body, func(r ProviderOAuthLoginResponse) error { return r.ValidateCancel(ref) })
}
func DecodeProviderOAuthLoginCompleteResponse(body []byte, ref providerauth.OAuthLoginRef) (ProviderAuthenticationMutationResponse, error) {
	var response ProviderAuthenticationMutationResponse
	if err := decodeProviderAuthJSON(body, MaxProviderAuthResponseBytes, &response); err != nil {
		return ProviderAuthenticationMutationResponse{}, errors.New("invalid OAuth login completion response")
	}
	if err := response.ValidateOAuthLogin(ref); err != nil {
		return ProviderAuthenticationMutationResponse{}, err
	}
	return response, nil
}
