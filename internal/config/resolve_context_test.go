package config

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/example-git/crux/internal/env"
	"github.com/stretchr/testify/require"
)

func TestShellVariableResolverContextPreservesCapturedEnvironmentAndDeadline(t *testing.T) {
	t.Parallel()
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	calls := 0
	resolver := NewShellVariableResolver(env.NewFromMap(map[string]string{"FIXTURE": "captured"}), WithExpander(func(expansion context.Context, value string, environment []string) (string, error) {
		calls++
		actual, ok := expansion.Deadline()
		require.True(t, ok)
		require.Equal(t, deadline, actual)
		require.Equal(t, "$FIXTURE", value)
		require.Equal(t, []string{"FIXTURE=captured"}, environment)
		return "captured", nil
	})).(contextVariableResolver)
	for range 2 {
		value, err := resolver.ResolveValueContext(ctx, "$FIXTURE")
		require.NoError(t, err)
		require.Equal(t, "captured", value)
	}
	require.Equal(t, 2, calls, "normal resolution remains uncached")
	cancel()
	_, err := resolver.ResolveValueContext(ctx, "$FIXTURE")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 2, calls, "cancellation precedes expansion")
}

func TestShellVariableResolverContextRejectsSuccessAfterCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	resolver := NewShellVariableResolver(env.NewFromMap(nil), WithExpander(func(context.Context, string, []string) (string, error) {
		cancel()
		return "synthetic-resolved-secret", nil
	})).(contextVariableResolver)
	value, err := resolver.ResolveValueContext(ctx, "template")
	require.Empty(t, value)
	require.ErrorIs(t, err, context.Canceled)
	require.NotContains(t, err.Error(), "synthetic-resolved-secret")
}

func TestShellVariableResolverContextCancelsRealCommand(t *testing.T) {
	t.Parallel()
	resolver := NewShellVariableResolver(env.NewFromMap(map[string]string{"PATH": os.Getenv("PATH")})).(contextVariableResolver)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := resolver.ResolveValueContext(ctx, "$(sleep 30)")
	require.Error(t, err)
	require.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled))
	require.Less(t, time.Since(started), 3*time.Second)
}
