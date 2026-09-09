package proto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
)

// AuthenticationProviderSurface preserves provider presentation while exposing
// only the generation-bound public owner. It cannot create execution authority.
// Keep its data fields in sync with providerregistry.Surface.
type AuthenticationProviderSurface struct {
	ID                string                                   `json:"id"`
	Name              string                                   `json:"name"`
	Owner             *providerauth.Owner                      `json:"owner,omitempty"`
	Available         bool                                     `json:"available"`
	Availability      string                                   `json:"availability"`
	Diagnostic        string                                   `json:"diagnostic,omitempty"`
	Description       string                                   `json:"description,omitempty"`
	Order             int                                      `json:"order"`
	FlatRate          bool                                     `json:"flat_rate,omitempty"`
	Brand             *providerregistry.Brand                  `json:"brand,omitempty"`
	DefaultLargeModel string                                   `json:"default_large_model,omitempty"`
	DefaultSmallModel string                                   `json:"default_small_model,omitempty"`
	Models            []catalog.Model                          `json:"models,omitempty"`
	Authentication    []providerregistry.Authentication        `json:"authentication,omitempty"`
	Configuration     map[string]any                           `json:"configuration_schema,omitempty"`
	ConfigurationUI   map[string]manifest.FieldDisplay         `json:"configuration_fields,omitempty"`
	Images            *manifest.ImagePolicy                    `json:"images,omitempty"`
	Instructions      *providerregistry.InstructionSurface     `json:"instructions,omitempty"`
	RuntimeControls   []providerregistry.RuntimeControlSurface `json:"runtime_controls,omitempty"`
	UsageAvailable    bool                                     `json:"usage_available,omitempty"`
}

// AuthenticationWorkspaceView is one exact authentication receipt's redacted
// configuration and public provider presentation. Existing nonsecret Config
// paths remain; connection state, account namespaces and raw accounts do not.
type AuthenticationWorkspaceView struct {
	ID               string                          `json:"id"`
	Config           *config.Config                  `json:"config"`
	ProviderSurfaces []AuthenticationProviderSurface `json:"provider_surfaces"`
}

func NewAuthenticationWorkspaceView(id string, snapshot config.RuntimeSnapshot) (*AuthenticationWorkspaceView, error) {
	if id == "" || snapshot.Config() == nil {
		return nil, errors.New("authentication workspace snapshot is unavailable")
	}
	result := &AuthenticationWorkspaceView{ID: id, Config: snapshot.Config().RedactedForTransport(), ProviderSurfaces: []AuthenticationProviderSurface{}}
	for _, s := range config.ProviderSurfaces(snapshot.Config()) {
		var owner *providerauth.Owner
		if s.Owner != nil {
			public := providerauth.PublicOwner(*s.Owner)
			owner = &public
		}
		result.ProviderSurfaces = append(result.ProviderSurfaces, AuthenticationProviderSurface{
			ID: s.ID, Name: s.Name, Owner: owner, Available: s.Available, Availability: s.Availability, Diagnostic: s.Diagnostic, Description: s.Description, Order: s.Order,
			FlatRate: s.FlatRate, Brand: s.Brand, DefaultLargeModel: s.DefaultLargeModel, DefaultSmallModel: s.DefaultSmallModel, Models: s.Models, Authentication: s.Authentication,
			Configuration: s.Configuration, ConfigurationUI: s.ConfigurationUI, Images: s.Images, Instructions: s.Instructions, RuntimeControls: s.RuntimeControls, UsageAvailable: s.UsageAvailable,
		})
	}
	return result, nil
}

// ProviderAuthenticationMutationResponse preserves progress even on errors.
// Workspace is present only for a current, complete, coherently verified local
// receipt; no ordinary workspace GET can substitute for that acknowledgement.
type ProviderAuthenticationMutationResponse struct {
	Outcome   providerauth.MutationOutcome `json:"outcome"`
	Workspace *AuthenticationWorkspaceView `json:"workspace,omitempty"`
	Error     *ProviderAuthenticationError `json:"error,omitempty"`
}

// ProviderAuthenticationError is a fixed safe wire error, never the private
// transaction cause. Unwrap preserves useful error classifications for SDK users.
type ProviderAuthenticationError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ProviderAuthenticationError) Error() string { return e.Message }
func (e *ProviderAuthenticationError) Unwrap() error { return providerAuthenticationErrorCause(e.Code) }

func providerAuthenticationErrorCause(code string) error {
	switch code {
	case "stale":
		return providerauth.ErrStale
	case "owner":
		return providerauth.ErrOwner
	case "account":
		return providerauth.ErrAccount
	case "operation_conflict":
		return providerauth.ErrOperationConflict
	case "canceled":
		return context.Canceled
	case "deadline":
		return context.DeadlineExceeded
	case "check_failed":
		return providerauth.ErrAPIKeyCheck
	case "check_unavailable":
		return providerauth.ErrAPIKeyCheckUnavailable
	case "mutation_failed":
		return providerauth.ErrMutation
	case "receipt_unverified":
		return providerauth.ErrReceiptUnverified
	case "client_runtime_managed":
		return config.ErrClientRuntimeManaged
	case "unavailable":
		return errors.New("provider authentication is unavailable")
	default:
		return nil
	}
}

func NewProviderAuthenticationError(err error) *ProviderAuthenticationError {
	// Prefer the public service classification over its private wrapped cause.
	for _, code := range []string{"receipt_unverified", "check_failed", "check_unavailable", "mutation_failed", "operation_conflict", "stale", "owner", "account", "client_runtime_managed", "deadline", "canceled"} {
		if errors.Is(err, providerAuthenticationErrorCause(code)) {
			return &ProviderAuthenticationError{Code: code, Message: providerAuthenticationErrorCause(code).Error()}
		}
	}
	return &ProviderAuthenticationError{Code: "unavailable", Message: providerAuthenticationErrorCause("unavailable").Error()}
}

func (e *ProviderAuthenticationError) Validate() error {
	cause := providerAuthenticationErrorCause(e.Code)
	if cause == nil || cause.Error() != e.Message {
		return errors.New("invalid provider authentication error")
	}
	return nil
}

func (r ProviderAuthenticationMutationResponse) ValidateSwitch(request providerauth.SwitchRequest) error {
	if err := r.Outcome.ValidateSwitch(request); err != nil {
		return err
	}
	return r.validate()
}
func (r ProviderAuthenticationMutationResponse) ValidateLogout(request providerauth.LogoutRequest) error {
	if err := r.Outcome.ValidateLogout(request); err != nil {
		return err
	}
	return r.validate()
}
func (r ProviderAuthenticationMutationResponse) validate() error {
	if r.Error != nil {
		if err := r.Error.Validate(); err != nil {
			return err
		}
		if r.Workspace != nil {
			return errors.New("failed authentication response cannot authorize a workspace view")
		}
		return nil
	}
	if r.Outcome.Change == nil {
		return errors.New("successful authentication response has no receipt")
	}
	if r.Outcome.Superseded {
		if r.Workspace != nil {
			return errors.New("historical authentication response cannot authorize a workspace view")
		}
		return nil
	}
	if r.Workspace == nil {
		return errors.New("current authentication response has no workspace view")
	}
	return r.Workspace.validate(r.Outcome.Change)
}

func (v *AuthenticationWorkspaceView) validate(change *providerauth.Change) error {
	if v.ID != change.Current.Target.WorkspaceID || v.Config == nil || v.ProviderSurfaces == nil {
		return errors.New("authentication workspace view is incomplete or changed workspace")
	}
	seen := map[string]AuthenticationProviderSurface{}
	for _, surface := range v.ProviderSurfaces {
		if surface.ID == "" {
			return errors.New("authentication surface has no provider")
		}
		if _, exists := seen[surface.ID]; exists {
			return errors.New("duplicate authentication surface")
		}
		if surface.Owner != nil {
			if err := surface.Owner.Validate(); err != nil {
				return err
			}
			if surface.Owner.ProviderID != surface.ID {
				return errors.New("authentication surface owner changed provider")
			}
		}
		seen[surface.ID] = surface
	}
	targetSurface, exists := seen[change.Current.Target.Owner.ProviderID]
	if !exists || targetSurface.Owner == nil || *targetSurface.Owner != change.Current.Target.Owner {
		return errors.New("authentication workspace changed target owner")
	}
	for _, slot := range []struct {
		kind     config.SelectedModelType
		selected *providerauth.OwnedModelState
	}{{config.SelectedModelTypeLarge, change.Models.Large}, {config.SelectedModelTypeSmall, change.Models.Small}} {
		model, exists := v.Config.Models[slot.kind]
		if exists != (slot.selected != nil) {
			return errors.New("authentication workspace changed model selection")
		}
		if slot.selected == nil {
			continue
		}
		if !authenticationJSONEqual(model, slot.selected.Model) {
			return errors.New("authentication workspace changed model state")
		}
		surface, present := seen[model.Provider]
		if slot.selected.Owner != nil && (!present || surface.Owner == nil || *surface.Owner != *slot.selected.Owner) {
			return errors.New("authentication workspace changed model owner")
		}
	}
	var configured, disabled bool
	if v.Config.Providers != nil {
		for id, provider := range v.Config.Providers.Seq2() {
			if provider.APIKey != "" || provider.APIKeyTemplate != "" || provider.OAuthToken != nil || len(provider.ExtraHeaders) != 0 {
				return errors.New("authentication workspace exposed provider credentials")
			}
			surface, exists := seen[id]
			if !exists || !authenticationJSONEqual(provider.Models, surface.Models) {
				return errors.New("authentication workspace provider catalog disagrees with presentation")
			}
			for key, field := range surface.ConfigurationUI {
				if field.Secret {
					if _, exists := provider.Configuration[key]; exists {
						return errors.New("authentication workspace exposed secret configuration")
					}
				}
			}
			if id == change.Current.Target.Owner.ProviderID {
				configured, disabled = true, provider.Disable
			}
		}
	}
	if configured != change.Current.Status.Configured || disabled != change.Current.Status.Disabled {
		return errors.New("authentication workspace changed configured provider membership")
	}
	if v.Config.Images != nil {
		for _, provider := range v.Config.Images.Providers {
			if len(provider.Configuration) != 0 || len(provider.Credentials) != 0 || len(provider.BrowserProfiles) != 0 {
				return errors.New("authentication workspace exposed private image configuration")
			}
		}
	}
	return nil
}

func authenticationJSONEqual(left, right any) bool {
	a, err := json.Marshal(left)
	if err != nil {
		return false
	}
	b, err := json.Marshal(right)
	if err != nil {
		return false
	}
	return config.RuntimeControlJSONEqual(a, b)
}

func DecodeProviderAuthSwitchRequest(body []byte) (providerauth.SwitchRequest, error) {
	var request providerauth.SwitchRequest
	if err := decodeProviderAuthJSON(body, MaxProviderAuthRequestBytes, &request); err != nil {
		return request, err
	}
	return request, request.Validate()
}
func DecodeProviderAuthLogoutRequest(body []byte) (providerauth.LogoutRequest, error) {
	var request providerauth.LogoutRequest
	if err := decodeProviderAuthJSON(body, MaxProviderAuthRequestBytes, &request); err != nil {
		return request, err
	}
	return request, request.Validate()
}
func DecodeProviderAuthSwitchResponse(body []byte, request providerauth.SwitchRequest) (ProviderAuthenticationMutationResponse, error) {
	var response ProviderAuthenticationMutationResponse
	if err := decodeProviderAuthJSON(body, MaxProviderAuthResponseBytes, &response); err != nil {
		return response, err
	}
	if err := response.ValidateSwitch(request); err != nil {
		return ProviderAuthenticationMutationResponse{}, fmt.Errorf("invalid authentication switch response: %w", err)
	}
	return response, nil
}
func DecodeProviderAuthLogoutResponse(body []byte, request providerauth.LogoutRequest) (ProviderAuthenticationMutationResponse, error) {
	var response ProviderAuthenticationMutationResponse
	if err := decodeProviderAuthJSON(body, MaxProviderAuthResponseBytes, &response); err != nil {
		return response, err
	}
	if err := response.ValidateLogout(request); err != nil {
		return ProviderAuthenticationMutationResponse{}, fmt.Errorf("invalid authentication logout response: %w", err)
	}
	return response, nil
}
