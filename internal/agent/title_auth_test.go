package agent

import (
	"context"
	"errors"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/require"
)

type titleFallbackProbe struct {
	fastModel
	calls   int
	failure error
	length  bool
}

func (m *titleFallbackProbe) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls++
	if m.failure != nil {
		return nil, m.failure
	}
	if m.length {
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonLength})
		}, nil
	}
	return m.fastModel.Stream(ctx, call)
}

func TestTitleAuthenticationFailureDoesNotSelectAnotherProvider(t *testing.T) {
	for _, test := range []struct {
		name             string
		failure          error
		length, fallback bool
	}{
		{name: "unauthorized", failure: &fantasy.ProviderError{StatusCode: 401}},
		{name: "forbidden", failure: &fantasy.ProviderError{StatusCode: 403}},
		{name: "mapped-auth", failure: &fantasy.ProviderError{AuthError: true}},
		{name: "refresh-failed", failure: &providertransport.AuthenticationRefreshError{Err: errors.New("client unavailable")}},
		{name: "canceled", failure: context.Canceled},
		{name: "deadline", failure: context.DeadlineExceeded},
		{name: "ordinary-error", failure: errors.New("model unavailable"), fallback: true},
		{name: "length", length: true, fallback: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := testEnv(t)
			large, small := &titleFallbackProbe{}, &titleFallbackProbe{failure: test.failure, length: test.length}
			agent := testSessionAgent(env, large, small, "system").(*sessionAgent)
			runtime := agent.Runtime()
			policy := &manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "never"}
			runtime.LargeModel.Retry, runtime.SmallModel.Retry = policy, policy
			session, err := env.sessions.Create(t.Context(), "")
			require.NoError(t, err)
			agent.generateTitleWithRuntime(t.Context(), session.ID, "title content", runtime)
			require.Equal(t, 1, small.calls)
			stored, err := env.sessions.Get(t.Context(), session.ID)
			require.NoError(t, err)
			if test.fallback {
				require.Equal(t, 1, large.calls)
				require.Equal(t, "title", stored.Title)
			} else {
				require.Zero(t, large.calls)
				require.Equal(t, DefaultSessionName, stored.Title)
			}
		})
	}
}
