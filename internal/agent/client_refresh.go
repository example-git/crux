package agent

import (
	"context"
	"errors"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
)

// bindModelAuthentication owns refresh for all text/object methods, including
// auxiliary calls that do not pass through the main agent's refresh callback.
func (c *coordinator) bindModelAuthentication(snapshot config.RuntimeSnapshot, admitted Model, provider config.ProviderConfig) Model {
	if admitted.Retry == nil {
		return admitted
	}
	captured := admitted
	admitted.Model = providertransport.NewAuthRefreshModel(admitted.Model, *admitted.Retry, func() bool {
		return provider.OAuthToken != nil && provider.OAuthToken.IsExpired()
	}, func(ctx context.Context) (fantasy.LanguageModel, error) {
		if captured.OnAuthRefresh == nil {
			return nil, errors.New("provider credential refresh is unavailable")
		}
		target := &clientAuthRefreshTarget{admitted: captured}
		if err := captured.OnAuthRefresh(context.WithValue(ctx, clientAuthRefreshKey{}, target), nil); err != nil {
			return nil, err
		}
		if target.refreshed != nil {
			return target.refreshed.Model, nil
		}
		if snapshot.IsClientOwned() {
			return nil, errors.New("client refresh returned no captured model")
		}
		selected := captured.ModelCfg
		model, _, err := c.buildAgentModelsWithOptions(ctx, config.Agent{PrimaryModelOverride: &selected}, captured.isSubAgent, c.cfg.RuntimeSnapshot(), captured.runtimeOptions)
		return model.Model, err
	})
	return admitted
}

func (c *coordinator) refreshAdmittedClientModel(ctx context.Context, admitted config.RuntimeSnapshot, owner providerregistry.RegistrationOwner) error {
	target, ok := ctx.Value(clientAuthRefreshKey{}).(*clientAuthRefreshTarget)
	if !ok || target == nil {
		return errors.New("client authentication retry requires its admitted model")
	}
	snapshot, err := c.cfg.RequestClientRefresh(ctx, admitted, owner)
	if err != nil {
		return err
	}
	// Build from the acknowledged snapshot and the operation's selected model.
	// Reading the current agent here could adopt a later user account choice.
	selected := target.admitted.ModelCfg
	model, _, err := c.buildAgentModelsWithOptions(ctx, config.Agent{PrimaryModelOverride: &selected}, target.admitted.isSubAgent, snapshot, target.admitted.runtimeOptions)
	if err != nil {
		return err
	}
	target.refreshed = &model
	return nil
}

func (c *coordinator) refreshAdmittedRuntime(ctx context.Context, admitted InstalledRuntime) (InstalledRuntime, error) {
	// Native models refresh inside the captured call. Refreshing here as well
	// would replace its controls and give a rejected fresh token another exchange.
	if owner, ok := admitted.LargeModel.Model.(interface{ HandlesAuthenticationRefresh() bool }); ok && owner.HandlesAuthenticationRefresh() {
		return admitted, nil
	}
	provider, ok := admitted.Snapshot.Config().Providers.Get(admitted.LargeModel.ModelCfg.Provider)
	if !ok {
		return admitted, errModelProviderNotConfigured
	}
	provider.ID = admitted.LargeModel.ModelCfg.Provider
	if provider.OAuthToken == nil || !provider.OAuthToken.IsExpired() {
		return admitted, nil
	}
	if !admitted.Snapshot.IsClientOwned() {
		if err := c.refreshTokenIfExpired(ctx, admitted.Snapshot, provider); err != nil {
			return admitted, err
		}
		return c.currentAgent.Runtime(), nil
	}
	owner, ok := admitted.Snapshot.ProviderOwnerFor(provider.ID, provider)
	if !ok {
		return admitted, errors.New("admitted client provider owner is unavailable")
	}
	snapshot, err := c.cfg.RequestClientRefresh(ctx, admitted.Snapshot, owner)
	if err != nil {
		return admitted, err
	}
	c.updateMu.Lock()
	defer c.updateMu.Unlock()
	prepared, err := c.buildRuntimeGeneration(ctx, snapshot)
	if err != nil {
		return admitted, err
	}
	if prepared.installed.LargeModel.ModelCfg.Provider != admitted.LargeModel.ModelCfg.Provider || prepared.installed.LargeModel.ModelCfg.Model != admitted.LargeModel.ModelCfg.Model {
		return admitted, errors.New("selected model changed during client authentication refresh")
	}
	return prepared.installed, nil
}
