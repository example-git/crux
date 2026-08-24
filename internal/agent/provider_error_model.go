package agent

import (
	"context"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerregistry"
)

// languageModelWithErrorMappings applies declarative provider error policy at
// the model boundary, before Fantasy decides whether to retry or refresh
// credentials and before the session persists a final error.
type languageModelWithErrorMappings struct {
	fantasy.LanguageModel
	registration providerregistry.Registration
}

func mapLanguageModelErrors(model fantasy.LanguageModel, registration providerregistry.Registration) fantasy.LanguageModel {
	if model == nil || len(registration.Errors) == 0 {
		return model
	}
	return languageModelWithErrorMappings{LanguageModel: model, registration: registration}
}

func (m languageModelWithErrorMappings) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	response, err := m.LanguageModel.Generate(ctx, call)
	return response, m.registration.MapError(err)
}

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

func (m languageModelWithErrorMappings) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	response, err := m.LanguageModel.GenerateObject(ctx, call)
	return response, m.registration.MapError(err)
}

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
