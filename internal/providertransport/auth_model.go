package providertransport

import (
	"context"
	"errors"
	"iter"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
)

type authRefreshModel struct {
	fantasy.LanguageModel
	policy  manifest.RetryPolicy
	expired func() bool
	refresh func(context.Context) (fantasy.LanguageModel, error)
}

// AuthenticationRefreshError preserves the failure cause while allowing
// callers to stop provider fallback after the credential authority failed.
type AuthenticationRefreshError struct{ Err error }

func (e *AuthenticationRefreshError) Error() string { return e.Err.Error() }
func (e *AuthenticationRefreshError) Unwrap() error { return e.Err }

// NewAuthRefreshModel supplies one credential authority for the complete text
// and object model contract. The refreshed model is captured for this operation.
func NewAuthRefreshModel(inner fantasy.LanguageModel, policy manifest.RetryPolicy, expired func() bool, refresh func(context.Context) (fantasy.LanguageModel, error)) fantasy.LanguageModel {
	return &authRefreshModel{LanguageModel: inner, policy: policy, expired: expired, refresh: refresh}
}

func (*authRefreshModel) HandlesAuthenticationRefresh() bool { return true }

type authModelCall struct {
	ctx       context.Context
	model     fantasy.LanguageModel
	budget    *AttemptBudget
	refreshed bool
	owner     *authRefreshModel
}

func (m *authRefreshModel) begin(ctx context.Context) (*authModelCall, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, budget := ContextWithAttemptBudget(ctx, m.policy.MaxAttempts)
	call := &authModelCall{ctx: ctx, model: m.LanguageModel, budget: budget, owner: m}
	if m.expired != nil && m.expired() {
		if err := call.rotate(); err != nil {
			return nil, err
		}
	}
	return call, nil
}

func (c *authModelCall) canRefresh(err error, emitted bool) bool {
	if emitted || c.refreshed || c.owner.policy.Authentication != "refresh-once" || c.owner.policy.ReplayRequirement == "never" || c.budget.Remaining() == 0 {
		return false
	}
	var providerErr *fantasy.ProviderError
	return errors.As(err, &providerErr) && (providerErr.StatusCode == 401 || providerErr.AuthError)
}

func (c *authModelCall) rotate() (err error) {
	defer func() {
		if err != nil {
			err = &AuthenticationRefreshError{Err: err}
		}
	}()
	if err := c.ctx.Err(); err != nil {
		return err
	}
	if c.owner.refresh == nil {
		return errors.New("provider credential refresh is unavailable")
	}
	model, err := c.owner.refresh(ContextWithoutAttemptBudget(c.ctx))
	if err != nil {
		return err
	}
	if model == nil {
		return errors.New("provider refresh returned no model")
	}
	// The returned model can carry the same policy for future independent calls.
	// This call keeps its original one-refresh and HTTP-attempt budgets.
	for {
		wrapped, ok := model.(*authRefreshModel)
		if !ok {
			break
		}
		model = wrapped.LanguageModel
	}
	c.model, c.refreshed = model, true
	return nil
}

func authGenerate[T any](ctx context.Context, m *authRefreshModel, invoke func(context.Context, fantasy.LanguageModel) (T, error)) (T, error) {
	var zero T
	call, err := m.begin(ctx)
	if err != nil {
		return zero, err
	}
	result, err := invoke(call.ctx, call.model)
	if !call.canRefresh(err, false) {
		return result, err
	}
	if err := call.rotate(); err != nil {
		return zero, err
	}
	return invoke(call.ctx, call.model)
}

func (m *authRefreshModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return authGenerate(ctx, m, func(ctx context.Context, model fantasy.LanguageModel) (*fantasy.Response, error) {
		return model.Generate(ctx, call)
	})
}

func (m *authRefreshModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return authGenerate(ctx, m, func(ctx context.Context, model fantasy.LanguageModel) (*fantasy.ObjectResponse, error) {
		return model.GenerateObject(ctx, call)
	})
}

func authStream[T any](ctx context.Context, m *authRefreshModel, invoke func(context.Context, fantasy.LanguageModel) (iter.Seq[T], error), partError func(T) error, errorPart func(error) T, localWarning func(T) bool) (iter.Seq[T], error) {
	call, err := m.begin(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := invoke(call.ctx, call.model)
	if call.canRefresh(err, false) {
		if err = call.rotate(); err == nil {
			stream, err = invoke(call.ctx, call.model)
		}
	}
	if err != nil {
		return nil, err
	}
	if stream == nil {
		return nil, errors.New("provider returned no stream")
	}
	return func(yield func(T) bool) {
		emitted := false
		for {
			var refreshErr error
			var warnings []T
			for part := range stream {
				if !emitted && localWarning(part) {
					warnings = append(warnings, part)
					continue
				}
				if err := partError(part); call.canRefresh(err, emitted) {
					refreshErr = err
					break
				}
				for _, warning := range warnings {
					if !yield(warning) {
						return
					}
				}
				warnings = nil
				emitted = true
				if !yield(part) {
					return
				}
			}
			if refreshErr == nil {
				for _, warning := range warnings {
					if !yield(warning) {
						return
					}
				}
				return
			}
			// The previous iterator has unwound its continuation locks before
			// refresh constructs another model from the accepted snapshot.
			if err := call.rotate(); err != nil {
				yield(errorPart(err))
				return
			}
			stream, err = invoke(call.ctx, call.model)
			if err != nil {
				yield(errorPart(err))
				return
			}
			if stream == nil {
				yield(errorPart(errors.New("provider returned no stream")))
				return
			}
		}
	}, nil
}

func (m *authRefreshModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	stream, err := authStream(ctx, m, func(ctx context.Context, model fantasy.LanguageModel) (iter.Seq[fantasy.StreamPart], error) {
		stream, err := model.Stream(ctx, call)
		return iter.Seq[fantasy.StreamPart](stream), err
	}, func(part fantasy.StreamPart) error {
		if part.Type == fantasy.StreamPartTypeError {
			return part.Error
		}
		return nil
	}, func(err error) fantasy.StreamPart {
		return fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err}
	}, func(part fantasy.StreamPart) bool {
		return part.Type == fantasy.StreamPartTypeWarnings
	})
	return fantasy.StreamResponse(stream), err
}

func (m *authRefreshModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	stream, err := authStream(ctx, m, func(ctx context.Context, model fantasy.LanguageModel) (iter.Seq[fantasy.ObjectStreamPart], error) {
		stream, err := model.StreamObject(ctx, call)
		return iter.Seq[fantasy.ObjectStreamPart](stream), err
	}, func(part fantasy.ObjectStreamPart) error {
		if part.Type == fantasy.ObjectStreamPartTypeError {
			return part.Error
		}
		return nil
	}, func(err error) fantasy.ObjectStreamPart {
		return fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeError, Error: err}
	}, func(part fantasy.ObjectStreamPart) bool {
		return part.Type == fantasy.ObjectStreamPartTypeObject && part.Object == nil && len(part.Warnings) > 0
	})
	return fantasy.ObjectStreamResponse(stream), err
}
