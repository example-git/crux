package oauth

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCapturedOAuthEnvironment(t *testing.T) {
	t.Setenv("OAUTH_ENV_TEST", "ambient-secret")
	t.Setenv("OAUTH_ENV_ABSENT", "ambient-fallback")
	entries := []string{"OAUTH_ENV_TEST=captured-secret"}
	ctx := ContextWithEnvironment(t.Context(), entries)
	entries[0] = "OAUTH_ENV_TEST=modified-input"
	t.Setenv("OAUTH_ENV_TEST", "later-ambient")
	value, ok := LookupEnvironment(ctx, "OAUTH_ENV_TEST")
	require.True(t, ok)
	require.Equal(t, "captured-secret", value)
	value, ok = LookupEnvironment(ctx, "OAUTH_ENV_ABSENT")
	require.False(t, ok)
	require.Empty(t, value)
	value, ok = LookupEnvironment(t.Context(), "OAUTH_ENV_TEST")
	require.True(t, ok)
	require.Equal(t, "later-ambient", value)
	value, ok = LookupEnvironment(ContextWithEnvironment(t.Context(), nil), "OAUTH_ENV_TEST")
	require.False(t, ok)
	require.Empty(t, value)
	for _, format := range []string{"%v", "%+v", "%#v"} {
		require.NotContains(t, fmt.Sprintf(format, ctx), "captured-secret")
	}
}
