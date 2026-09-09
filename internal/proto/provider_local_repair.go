package proto

import (
	"context"
	"errors"
	"fmt"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
)

type ProviderLocalRepairResponse struct {
	Request providerauth.LocalRepairRequest        `json:"request"`
	Result  config.LocalAuthenticationRepairResult `json:"result"`
	Error   *ProviderLocalRepairError              `json:"error,omitempty"`
}
type ProviderLocalRepairError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ProviderLocalRepairError) Error() string { return e.Message }
func (e *ProviderLocalRepairError) Unwrap() error {
	switch e.Code {
	case "canceled":
		return context.Canceled
	case "deadline":
		return context.DeadlineExceeded
	case "refresh_unresolved":
		return config.ErrLocalAuthenticationRefreshUnresolved
	case "client_runtime_managed":
		return config.ErrClientRuntimeManaged
	}
	return nil
}
func localRepairMessage(code string) string {
	switch code {
	case "canceled":
		return "Local authentication repair was canceled; inspect retained disk progress."
	case "deadline":
		return "Local authentication repair reached its deadline; inspect retained disk progress."
	case "refresh_unresolved":
		return "The original refresh may have consumed its token without a retained response. Sign in again; it will not be exchanged again."
	case "client_runtime_managed":
		return "This repair belongs to the owning client."
	case "failed":
		return "Local authentication repair could not complete. Review the same operation; changed inputs require explicit Reload and saved-state review."
	}
	return ""
}
func NewProviderLocalRepairError(err error) *ProviderLocalRepairError {
	code := "failed"
	switch {
	case errors.Is(err, config.ErrLocalAuthenticationRefreshUnresolved):
		code = "refresh_unresolved"
	case errors.Is(err, config.ErrClientRuntimeManaged):
		code = "client_runtime_managed"
	case errors.Is(err, context.Canceled):
		code = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		code = "deadline"
	}
	return &ProviderLocalRepairError{Code: code, Message: localRepairMessage(code)}
}
func (r ProviderLocalRepairResponse) Validate(request providerauth.LocalRepairRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if r.Request != request {
		return errors.New("local repair response identity changed")
	}
	s := r.Result.Summary
	if s.OperationID != "" {
		if err := s.Validate(); err != nil {
			return err
		}
		if s.WorkspaceID != request.OperationWorkspaceID || s.OperationID != request.OperationID || s.Revision == 0 || s.ProviderID == "" {
			return errors.New("local repair summary identity changed")
		}
		switch s.Action {
		case "switch", "logout", "remove", "oauth-login", "api-key":
		default:
			return errors.New("local repair action is unsupported")
		}
	} else if r.Error == nil {
		return errors.New("local repair summary is missing")
	}
	if !request.Apply && (r.Result.AccountsWritten || r.Result.ConfigWritten || r.Result.AccountsMatched || r.Result.ConfigMatched || r.Result.NeedsReload) {
		return errors.New("local repair review claims effects")
	}
	if r.Result.NeedsReload && (!request.Apply || !s.NeedsReload) {
		return errors.New("local repair completion is inconsistent")
	}
	if request.Apply && r.Error == nil && !r.Result.NeedsReload {
		return errors.New("local repair completion is missing")
	}
	if r.Error != nil && (localRepairMessage(r.Error.Code) == "" || r.Error.Message != localRepairMessage(r.Error.Code)) {
		return errors.New("local repair error is unsupported")
	}
	return nil
}
func DecodeProviderLocalRepairRequest(body []byte) (providerauth.LocalRepairRequest, error) {
	var request providerauth.LocalRepairRequest
	if err := decodeProviderAuthJSON(body, MaxProviderAuthRequestBytes, &request); err != nil {
		return request, err
	}
	return request, request.Validate()
}
func DecodeProviderLocalRepairResponse(body []byte, request providerauth.LocalRepairRequest) (ProviderLocalRepairResponse, error) {
	var response ProviderLocalRepairResponse
	if err := decodeProviderAuthJSON(body, MaxProviderAuthResponseBytes, &response); err != nil {
		return response, err
	}
	if err := response.Validate(request); err != nil {
		return ProviderLocalRepairResponse{}, fmt.Errorf("invalid local repair response: %w", err)
	}
	return response, nil
}
