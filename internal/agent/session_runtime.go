package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
)

type (
	sessionRuntimeKey          struct{}
	sessionInstalledRuntimeKey struct{}
)

func (a *sessionAgent) runtimeForContext(ctx context.Context) InstalledRuntime {
	if runtime, ok := ctx.Value(sessionInstalledRuntimeKey{}).(InstalledRuntime); ok {
		return runtime
	}
	return a.Runtime()
}

func (c *coordinator) modelSnapshotForContext(ctx context.Context) config.RuntimeSnapshot {
	if snapshot, ok := ctx.Value(sessionRuntimeKey{}).(config.RuntimeSnapshot); ok {
		return snapshot
	}
	return c.cfg.RuntimeSnapshot()
}

type sessionModelSelectionError struct {
	cause error
}

func (e *sessionModelSelectionError) Error() string {
	return fmt.Sprintf("session model selection is unavailable: %v", e.cause)
}

func (e *sessionModelSelectionError) Unwrap() error {
	return e.cause
}

func (e *sessionModelSelectionError) NonRetryable() bool {
	return true
}

func (c *coordinator) runtimeForSession(ctx context.Context, sessionID string) (InstalledRuntime, error) {
	current, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return InstalledRuntime{}, err
	}
	if len(current.Models) == 0 {
		return InstalledRuntime{}, &sessionModelSelectionError{cause: errors.New("select the intended models for this session")}
	}
	var models struct {
		config.AgentModelState
		LegacyPathMasking json.RawMessage `json:"path_masking"`
	}
	decoder := json.NewDecoder(bytes.NewReader(current.Models))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&models); err != nil {
		return InstalledRuntime{}, &sessionModelSelectionError{cause: err}
	}
	var runtime InstalledRuntime
	err = c.cfg.WithRuntimeSnapshot(func(snapshot config.RuntimeSnapshot) error {
		scoped, err := snapshot.WithAgentModels(models.AgentModelState)
		if err != nil {
			return &sessionModelSelectionError{cause: err}
		}
		if err := c.lockRuntimeGeneration(ctx); err != nil {
			return err
		}
		defer c.updateMu.Unlock()
		generation, err := c.buildRuntimeGeneration(ctx, scoped)
		if err != nil {
			return err
		}
		runtime = generation.installed
		runtime.SessionID, runtime.ModelRevision = current.ID, current.ModelRevision
		return nil
	})
	return runtime, err
}

func (c *coordinator) bindSessionRuntime(ctx context.Context, call *SessionAgentCall) (context.Context, error) {
	runtime, err := c.runtimeForSession(ctx, call.SessionID)
	if err != nil {
		return ctx, err
	}
	runtime, refreshErr := c.refreshAdmittedRuntime(ctx, runtime)
	if refreshErr != nil {
		if runtime.Snapshot.IsClientOwned() {
			return ctx, refreshErr
		}
		slog.Error("Failed to refresh OAuth2 token. Proceeding with existing token.", "error", refreshErr)
	}
	ctx = context.WithValue(ctx, sessionRuntimeKey{}, runtime.Snapshot)
	call.PrepareRuntime = func(stepContext context.Context, next InstalledRuntime) (fantasy.StepModelSettings, error) {
		model := next.LargeModel
		cfg := next.Snapshot.Config()
		providerCfg, ok := cfg.Providers.Get(model.ModelCfg.Provider)
		if !ok {
			return fantasy.StepModelSettings{}, errModelProviderNotConfigured
		}
		providerCfg.ID = model.ModelCfg.Provider
		registration, registered := next.Snapshot.ProviderBehaviorRegistration(model.ModelCfg.Provider, providerCfg)
		options, temp, topP, topK, freqPenalty, presPenalty, err := mergeCallOptions(model, providerCfg, registration)
		if err != nil {
			return fantasy.StepModelSettings{}, fmt.Errorf("provider %s options: %w", model.ModelCfg.Provider, err)
		}
		options = c.oauthReasoningOptionsForRegistration(model.ModelCfg.Provider, model.ModelCfg.Model, registration, registered, options)
		options = applyRegisteredRuntimeOptions(model.ModelCfg.Model, turnRuntimeOptions(modelAnalysisContext(ctx, model.ModelCfg), cfg.Options), registration, registered, options)
		options, err = applyTurnProviderOperation(ctx, model.ModelCfg.Provider, options)
		if err != nil {
			return fantasy.StepModelSettings{}, err
		}
		maxTokens := model.CatalogModel.DefaultMaxTokens
		if model.ModelCfg.MaxTokens != 0 {
			maxTokens = model.ModelCfg.MaxTokens
		}
		var outputLimit *int64
		if maxTokens > 0 {
			outputLimit = &maxTokens
		}
		runtime = next
		return fantasy.StepModelSettings{
			MaxOutputTokens: outputLimit, ProviderOptions: options,
			Temperature: temp, TopP: topP, TopK: topK,
			FrequencyPenalty: freqPenalty, PresencePenalty: presPenalty,
			MaxRetries:    modelMaxRetries(model),
			OnAuthRefresh: c.makeAuthRefreshCallback(runtime.Snapshot, providerCfg),
		}, nil
	}
	settings, err := call.PrepareRuntime(ctx, runtime)
	if err != nil {
		return ctx, err
	}
	call.applyModelSettings(settings)
	call.runtime = &runtime
	sessionID := call.SessionID
	call.RuntimeProvider = func(stepContext context.Context) (InstalledRuntime, error) {
		current, err := c.sessions.Get(stepContext, sessionID)
		if err != nil {
			return InstalledRuntime{}, err
		}
		if current.ModelRevision == runtime.ModelRevision {
			return runtime, nil
		}
		return c.runtimeForSession(stepContext, sessionID)
	}
	call.OnProviderWarning = func(w fantasy.CallWarning) {
		model := runtime.LargeModel
		providerCfg, ok := runtime.Snapshot.Config().Providers.Get(model.ModelCfg.Provider)
		if !ok {
			return
		}
		providerCfg.ID = model.ModelCfg.Provider
		registration, registered := runtime.Snapshot.ProviderBehaviorRegistration(model.ModelCfg.Provider, providerCfg)
		c.disableOAuthReasoningForRegistration(model.ModelCfg.Provider, model.ModelCfg.Model, registration, registered, w.Message)
	}
	return ctx, nil
}

func (call *SessionAgentCall) applyModelSettings(settings fantasy.StepModelSettings) {
	call.MaxOutputTokens = 0
	if settings.MaxOutputTokens != nil {
		call.MaxOutputTokens = *settings.MaxOutputTokens
	}
	call.ProviderOptions = settings.ProviderOptions
	call.Temperature = settings.Temperature
	call.TopP = settings.TopP
	call.TopK = settings.TopK
	call.FrequencyPenalty = settings.FrequencyPenalty
	call.PresencePenalty = settings.PresencePenalty
	call.OnAuthRefresh = settings.OnAuthRefresh
}
