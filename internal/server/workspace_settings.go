package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
)

type WorkspaceSettings interface {
	RuntimeControlState(context.Context, string, config.Scope, config.RuntimeControlTarget) (config.RuntimeControlState, error)
	SetRuntimeControl(context.Context, string, config.Scope, config.RuntimeControlTarget, json.RawMessage) (config.RuntimeControlState, error)
	RemoveRuntimeControl(context.Context, string, config.Scope, config.RuntimeControlTarget) (config.RuntimeControlState, error)
	SetProviderToolingInstructions(context.Context, string, config.Scope, providerregistry.RegistrationOwner, string) (proto.ProviderToolingState, error)
	RemoveProviderToolingInstructions(context.Context, string, config.Scope, providerregistry.RegistrationOwner) (proto.ProviderToolingState, error)
	SetProviderDisabled(string, config.Scope, providerregistry.RegistrationOwner, bool) error
	SetConfigField(string, config.Scope, string, any) error
	RemoveConfigField(string, config.Scope, string) error
	UpdatePreferredModel(string, config.Scope, config.SelectedModelType, config.SelectedModel, providerregistry.RegistrationOwner) (config.AgentModelState, error)
	SetCompactMode(string, config.Scope, bool) error
	RemoveProviderCredentials(string, config.Scope, providerregistry.RegistrationOwner) error
	SetProviderAPIKey(string, config.Scope, string, any) error
	ImportCopilot(context.Context, string, providerregistry.RegistrationOwner) (bool, error)
	RefreshOAuthToken(context.Context, string, config.Scope, providerregistry.RegistrationOwner) error
}

func (c *controllerV1) workspaceSettings() WorkspaceSettings {
	if c.settings != nil {
		return c.settings
	}
	return c.backend
}

func (s *Server) WorkspaceSettingsHandler(settings WorkspaceSettings) http.Handler {
	if settings == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			jsonError(w, http.StatusServiceUnavailable, "workspace settings owner is unavailable")
		})
	}
	controller := &controllerV1{backend: s.backend, server: s, settings: settings}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/workspaces/{id}/config/set", controller.handlePostWorkspaceConfigSet)
	mux.HandleFunc("POST /v1/workspaces/{id}/config/remove", controller.handlePostWorkspaceConfigRemove)
	mux.HandleFunc("POST /v1/workspaces/{id}/config/model", controller.handlePostWorkspaceConfigModel)
	mux.HandleFunc("POST /v1/workspaces/{id}/config/compact", controller.handlePostWorkspaceConfigCompact)
	mux.HandleFunc("POST /v1/workspaces/{id}/config/provider-key", controller.handlePostWorkspaceConfigProviderKey)
	mux.HandleFunc("POST /v1/workspaces/{id}/config/import-copilot", controller.handlePostWorkspaceConfigImportCopilot)
	mux.HandleFunc("POST /v1/workspaces/{id}/config/refresh-oauth", controller.handlePostWorkspaceConfigRefreshOAuth)
	mux.HandleFunc("POST /v1/workspaces/{id}/config/runtime-control/resolve", controller.handleResolveWorkspaceRuntimeControl)
	mux.HandleFunc("PUT /v1/workspaces/{id}/config/runtime-control", controller.handlePutWorkspaceRuntimeControl)
	mux.HandleFunc("DELETE /v1/workspaces/{id}/config/runtime-control", controller.handleDeleteWorkspaceRuntimeControl)
	mux.HandleFunc("PUT /v1/workspaces/{id}/config/provider-tooling", controller.handlePutWorkspaceProviderTooling)
	mux.HandleFunc("DELETE /v1/workspaces/{id}/config/provider-tooling", controller.handleDeleteWorkspaceProviderTooling)
	return mux
}
