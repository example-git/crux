package clientidentity

import (
	"context"
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestCapturedEnvironmentDoesNotSeedSharedVersion(t *testing.T) {
	identity := &manifest.ResolvedClientIdentity{Environment: "SYNTHETIC_IMAGE_VERSION", VersionPattern: `^\d+\.\d+$`, FallbackVersion: "1.0", UserAgentFormat: "synthetic/{version}"}
	t.Setenv(identity.Environment, "9.0")
	version, agent, err := ResolveWithEnvironment(t.Context(), identity, []string{identity.Environment + "=2.0"})
	require.NoError(t, err)
	require.Equal(t, "2.0", version)
	require.Equal(t, "synthetic/2.0", agent)
	version, _, err = ResolveWithEnvironment(t.Context(), identity, nil)
	require.NoError(t, err)
	require.Equal(t, "1.0", version)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = ResolveWithEnvironment(ctx, identity, []string{identity.Environment + "=2.0"})
	require.ErrorIs(t, err, context.Canceled)
}
