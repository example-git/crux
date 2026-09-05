package agent

import (
	"context"

	fantasy "github.com/example-git/crux/foundation"
	codexresponses "github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/example-git/crux/internal/providerregistry"
)

// languageModelWithErrorMappings applies declarative provider error policy at
// the model boundary, before Fantasy decides whether to retry or refresh
// credentials and before the session persists a final error.
type languageModelWithErrorMappings struct {
	fantasy.LanguageModel
	registration providerregistry.Registration
}

type remoteCompactorWithErrorMappings struct {
	RemoteCompactor
	registration providerregistry.Registration
}

// mapLanguageModelErrors keeps manifest-declared error semantics attached to
// the exact registered provider model. Bypassing this wrapper can turn
// retryable, authentication, or user-facing plugin errors into unrelated host
// defaults even when request transport remains functional.
func mapLanguageModelErrors(model fantasy.LanguageModel, registration providerregistry.Registration) fantasy.LanguageModel {
	if model == nil || len(registration.Errors) == 0 {
		return model
	}
	return languageModelWithErrorMappings{LanguageModel: model, registration: registration}
}

func mapRemoteCompactorErrors(compactor RemoteCompactor, registration providerregistry.Registration) RemoteCompactor {
	if compactor == nil || len(registration.Errors) == 0 {
		return compactor
	}
	return remoteCompactorWithErrorMappings{RemoteCompactor: compactor, registration: registration}
}

func (c remoteCompactorWithErrorMappings) Compact(ctx context.Context, call fantasy.Call) (*codexresponses.CompactionResult, error) {
	result, err := c.RemoteCompactor.Compact(ctx, call)
	return result, c.registration.MapError(err)
}

// Generate maps the final non-streaming error before higher layers classify it.
func (m languageModelWithErrorMappings) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	response, err := m.LanguageModel.Generate(ctx, call)
	return response, m.registration.MapError(err)
}

// Stream maps both stream creation failures and errors delivered inside stream
// parts; handling only one path loses plugin policy after streaming begins.
func (m languageModelWithErrorMappings) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	stream, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return nil, m.registration.MapError(err)
	}
	return func(yield func(fantasy.StreamPart) bool) {
		stream(func(part fantasy.StreamPart) bool {
			part.Error = m.registration.MapError(part.Error)
			return yield(part)
		})
	}, nil
}

// GenerateObject preserves the same plugin error contract for structured
// non-streaming output as ordinary Generate.
func (m languageModelWithErrorMappings) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	response, err := m.LanguageModel.GenerateObject(ctx, call)
	return response, m.registration.MapError(err)
}

// StreamObject maps both startup and in-stream structured-output errors so the
// transport mode cannot bypass manifest policy.
func (m languageModelWithErrorMappings) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	stream, err := m.LanguageModel.StreamObject(ctx, call)
	if err != nil {
		return nil, m.registration.MapError(err)
	}
	return func(yield func(fantasy.ObjectStreamPart) bool) {
		stream(func(part fantasy.ObjectStreamPart) bool {
			part.Error = m.registration.MapError(part.Error)
			return yield(part)
		})
	}, nil
}
