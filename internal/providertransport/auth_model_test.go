package providertransport

import (
	"context"
	"errors"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

type authStreamFixture struct {
	fantasy.LanguageModel
	text   fantasy.StreamResponse
	object fantasy.ObjectStreamResponse
	calls  int
}

func (m *authStreamFixture) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls++
	return m.text, nil
}

func (m *authStreamFixture) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	m.calls++
	return m.object, nil
}

func TestAuthenticationRefreshNeverReplaysEmittedOutput(t *testing.T) {
	for _, object := range []bool{false, true} {
		for _, replay := range []string{"before-first-event", "idempotent"} {
			t.Run(replay+map[bool]string{false: "/text", true: "/object"}[object], func(t *testing.T) {
				authErr := &fantasy.ProviderError{StatusCode: 401, AuthError: true}
				inner := &authStreamFixture{
					text: func(yield func(fantasy.StreamPart) bool) {
						if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "partial"}) {
							return
						}
						yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: authErr})
					},
					object: func(yield func(fantasy.ObjectStreamPart) bool) {
						if !yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeObject, Object: map[string]any{"partial": true}}) {
							return
						}
						yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeError, Error: authErr})
					},
				}
				refreshes := 0
				model := NewAuthRefreshModel(inner, manifest.RetryPolicy{MaxAttempts: 3, Authentication: "refresh-once", ReplayRequirement: replay}, nil, func(context.Context) (fantasy.LanguageModel, error) { refreshes++; return inner, nil })
				parts := 0
				var failure error
				if object {
					stream, err := model.StreamObject(t.Context(), fantasy.ObjectCall{})
					require.NoError(t, err)
					for part := range stream {
						parts++
						failure = part.Error
					}
				} else {
					stream, err := model.Stream(t.Context(), fantasy.Call{})
					require.NoError(t, err)
					for part := range stream {
						parts++
						failure = part.Error
					}
				}
				require.Equal(t, 2, parts)
				require.ErrorIs(t, failure, authErr)
				require.Zero(t, refreshes)
				require.Equal(t, 1, inner.calls)
			})
		}
	}
}

func TestAuthenticationRefreshCancellationBeforeRotation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	inner := &authStreamFixture{text: func(yield func(fantasy.StreamPart) bool) {
		cancel()
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: &fantasy.ProviderError{StatusCode: 401}})
	}}
	refreshes := 0
	model := NewAuthRefreshModel(inner, manifest.RetryPolicy{MaxAttempts: 2, Authentication: "refresh-once", ReplayRequirement: "before-first-event"}, nil, func(context.Context) (fantasy.LanguageModel, error) { refreshes++; return inner, nil })
	stream, err := model.Stream(ctx, fantasy.Call{})
	require.NoError(t, err)
	for part := range stream {
		require.ErrorIs(t, part.Error, context.Canceled)
	}
	require.Zero(t, refreshes)
	_, err = model.Stream(ctx, fantasy.Call{})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, inner.calls)
}

func TestAuthenticationRefreshUnwindsStreamBeforeRebuilding(t *testing.T) {
	active := false
	inner := &authStreamFixture{text: func(yield func(fantasy.StreamPart) bool) {
		active = true
		defer func() { active = false }()
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: &fantasy.ProviderError{StatusCode: 401}})
	}}
	refreshErr := errors.New("refresh failed")
	model := NewAuthRefreshModel(inner, manifest.RetryPolicy{MaxAttempts: 2, Authentication: "refresh-once", ReplayRequirement: "before-first-event"}, nil, func(context.Context) (fantasy.LanguageModel, error) { require.False(t, active); return nil, refreshErr })
	stream, err := model.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	for part := range stream {
		require.ErrorIs(t, part.Error, refreshErr)
	}
}
